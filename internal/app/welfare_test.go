package app

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 福利模块的离线测试：用假上游验证「幂等判定」与「凭证类型分流」。
// 这两条是线上唯一会真的改账号状态/攒积分的地方，必须能在没有真账号时回归。

type fixtureRequest struct {
	Method string
	Path   string
	Body   string
}

type fixtureReply struct {
	Status int
	Body   string
}

// welfareFixture 是一个按调用次序回放的假上游：同一路由的第 N 次调用取第 N 个回复，
// 超出后重复最后一个。这样「第一次读未点亮、发完打点复读已点亮」这类时序可以精确断言。
type welfareFixture struct {
	server    *httptest.Server
	mu        sync.Mutex
	requests  []fixtureRequest
	sequences map[string][]fixtureReply
}

func newWelfareFixture(t *testing.T, sequences map[string][]fixtureReply) *welfareFixture {
	t.Helper()
	fixture := &welfareFixture{sequences: sequences}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := ""
		if r.Body != nil {
			if raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)); err == nil {
				body = string(raw)
			}
		}
		fixture.mu.Lock()
		index := 0
		for _, item := range fixture.requests {
			if item.Method == r.Method && item.Path == r.URL.Path {
				index++
			}
		}
		fixture.requests = append(fixture.requests, fixtureRequest{Method: r.Method, Path: r.URL.Path, Body: body})
		candidates := fixture.sequences[r.Method+" "+r.URL.Path]
		fixture.mu.Unlock()

		if len(candidates) == 0 {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error_msg":"404 Route Not Found"}`))
			return
		}
		if index >= len(candidates) {
			index = len(candidates) - 1
		}
		reply := candidates[index]
		if reply.Status != 0 {
			w.WriteHeader(reply.Status)
		}
		_, _ = w.Write([]byte(reply.Body))
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

// count 返回某个 "METHOD path" 被调用的次数。
func (f *welfareFixture) count(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, item := range f.requests {
		if item.Method == method && item.Path == path {
			n++
		}
	}
	return n
}

// bodies 返回某个路由收到的全部报文。
func (f *welfareFixture) bodies(method, path string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, item := range f.requests {
		if item.Method == method && item.Path == path {
			out = append(out, item.Body)
		}
	}
	return out
}

func ok(body string) []fixtureReply { return []fixtureReply{{Body: body}} }

func checkinStatusBody(active, checkedIn bool, streak int64) string {
	return `{"code":0,"msg":"OK","data":{` +
		`"active":` + jsonBool(active) + `,` +
		`"today_checked_in":` + jsonBool(checkedIn) + `,` +
		`"streak_days":` + jsonNumber(streak) + `,` +
		`"daily_credit":100,"today_credit":100,"week_checkin_days":3,"total_credits":300,` +
		`"activity_name":"Buddy应用","theme_name":"Buddy加油站","end_time":"2026-12-31 23:59:59"}}`
}

func heatmapBody(today string, score int) string {
	return `{"code":0,"msg":"OK","data":{"cells":[` +
		`{"date":"2026-11-05","score":0},` +
		`{"date":"` + today + `","score":` + jsonNumber(int64(score)) + `}]}}`
}

func jsonBool(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func jsonNumber(value int64) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func cnAccount(id int64, key string) Account {
	return Account{ID: id, Name: "cn", APIKey: key, TypeSlug: sql.NullString{String: TypeSlugCN, Valid: true}}
}

// isAPIKeyNotAllowed 必须把「这类 key 没权限」与真故障分开。
//
// CN 的 daily-checkin 写路径对 ck_ 订阅 key 一律 403；认不出来就会每天两次
// 拿一条假红色错误刷日志，并且永远重试。
func TestIsAPIKeyNotAllowed(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "上游 403 + 固定文案",
			err: &welfareHTTPError{
				StatusCode: http.StatusForbidden,
				Body:       `{"message":"API key not allowed for this path or method"}`,
			},
			want: true,
		},
		{
			name: "403 但文案不同（例如风控）不算",
			err:  &welfareHTTPError{StatusCode: http.StatusForbidden, Body: `{"message":"forbidden by waf"}`},
			want: false,
		},
		{
			name: "401 带同样文案也不算（那是凭证失效，该报错）",
			err: &welfareHTTPError{
				StatusCode: http.StatusUnauthorized,
				Body:       `{"message":"API key not allowed for this path or method"}`,
			},
			want: false,
		},
		{name: "普通错误", err: sql.ErrNoRows, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAPIKeyNotAllowed(tc.err); got != tc.want {
				t.Errorf("isAPIKeyNotAllowed = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// intl 打点必须「已点亮就不发」。
//
// /v2/report 不是按天幂等的计数器：实测每发一次热力值 +2（0→2→4→6）。
// 这里锁住的行为是：热力值 > 0 时一个写请求都不发。
func TestRunDailyBonusSkipsWhenLit(t *testing.T) {
	fixture := newWelfareFixture(t, map[string][]fixtureReply{
		"GET /activity/growth/heatmap": ok(heatmapBody("2026-11-06", 2)),
		"POST /v2/report":              ok(`{"code":0,"msg":"OK"}`),
	})
	server := welfareTestServer(t, fixture)

	run := server.runDailyBonus(Account{ID: 9, Name: "intl", APIKey: fakeJWTSubject("16539ee0-de0c-4b7d-9a55-000000000000")})
	if run == nil || run.Status != WelfareAlready {
		t.Fatalf("状态 = %v，期望 already", run)
	}
	if n := fixture.count("POST", "/v2/report"); n != 0 {
		t.Fatalf("已点亮还打了 %d 次点，会刷分", n)
	}
}

// 未点亮时打一次点，发完复读确认真的点亮。
func TestRunDailyBonusReportsWhenUnlit(t *testing.T) {
	fixture := newWelfareFixture(t, map[string][]fixtureReply{
		"GET /activity/growth/heatmap": {
			{Body: heatmapBody("2026-11-06", 0)},
			{Body: heatmapBody("2026-11-06", 2)},
		},
		"POST /v2/report": ok(`{"code":0,"msg":"OK"}`),
	})
	server := welfareTestServer(t, fixture)

	run := server.runDailyBonus(Account{ID: 9, Name: "intl", APIKey: fakeJWTSubject("16539ee0-de0c-4b7d-9a55-000000000000")})
	if run == nil || run.Status != WelfareOK {
		t.Fatalf("状态 = %v，期望 ok（detail=%v）", run, run.Detail)
	}
	if n := fixture.count("POST", "/v2/report"); n != 1 {
		t.Fatalf("打点次数 = %d，期望恰好 1 次", n)
	}
	if n := fixture.count("GET", "/activity/growth/heatmap"); n != 2 {
		t.Fatalf("热力墙读取次数 = %d，期望 2（判定 + 复读）", n)
	}
	// userId 必须是 token 的 sub：留空时上游也回 200，但热力墙不动
	bodies := fixture.bodies("POST", "/v2/report")
	if !strings.Contains(bodies[0], `"userId"`) {
		t.Errorf("打点报文缺 userId: %s", bodies[0])
	}
	if strings.Contains(bodies[0], `"userId":""`) {
		t.Errorf("userId 为空，上游会接受但热力墙不点亮: %s", bodies[0])
	}
}

// 上游收下打点但没点亮时不许报成功——「200 不等于生效」。
func TestRunDailyBonusReportsFailureWhenNotLit(t *testing.T) {
	fixture := newWelfareFixture(t, map[string][]fixtureReply{
		"GET /activity/growth/heatmap": ok(heatmapBody("2026-11-06", 0)),
		"POST /v2/report":              ok(`{"code":0,"msg":"OK"}`),
	})
	server := welfareTestServer(t, fixture)

	run := server.runDailyBonus(Account{ID: 9, Name: "intl", APIKey: fakeJWTSubject("16539ee0-de0c-4b7d-9a55-000000000000")})
	if run == nil || run.Status != WelfareFailed {
		t.Fatalf("状态 = %v，期望 failed", run)
	}
}

// CN 已签到就不再发领取请求。
func TestRunCheckinSkipsWhenAlreadyCheckedIn(t *testing.T) {
	fixture := newWelfareFixture(t, map[string][]fixtureReply{
		"POST /v2/billing/meter/checkin-activity-status": ok(checkinStatusBody(true, true, 7)),
		"POST /v2/billing/meter/daily-checkin":           ok(`{"code":0,"msg":"OK"}`),
	})
	server := welfareTestServer(t, fixture)

	run := server.runCheckin(cnAccount(1, "ck_test"))
	if run == nil || run.Status != WelfareAlready {
		t.Fatalf("状态 = %v，期望 already", run)
	}
	if n := fixture.count("POST", "/v2/billing/meter/daily-checkin"); n != 0 {
		t.Fatalf("已签到还发了 %d 次领取", n)
	}
}

// ck_ 号的 403 记 unsupported（凭证类型限制），不是 failed；同一天不再重试。
func TestRunCheckinMarksAPIKeyUnsupported(t *testing.T) {
	fixture := newWelfareFixture(t, map[string][]fixtureReply{
		"POST /v2/billing/meter/checkin-activity-status": ok(checkinStatusBody(true, false, 0)),
		"POST /v2/billing/meter/daily-checkin": {
			{Status: http.StatusForbidden, Body: `{"message":"API key not allowed for this path or method"}`},
		},
	})
	server := welfareTestServer(t, fixture)

	account := cnAccount(1, "ck_test")
	run := server.runCheckin(account)
	if run == nil || run.Status != WelfareUnsupported {
		t.Fatalf("状态 = %v，期望 unsupported（不是 failed）", run)
	}
	if run.State == nil || run.State.Checkin == nil || run.State.Checkin.Supported {
		t.Fatalf("应标记 Supported=false")
	}

	// 同一天第二轮的 tick 不应再打一次写请求
	before := fixture.count("POST", "/v2/billing/meter/daily-checkin")
	second := server.runCheckin(account)
	if second == nil || second.Status != WelfareUnsupported {
		t.Fatalf("第二轮状态 = %v，期望 unsupported", second)
	}
	if after := fixture.count("POST", "/v2/billing/meter/daily-checkin"); after != before {
		t.Fatalf("同一天重试了领取请求：%d -> %d", before, after)
	}
}

// 签到成功会写状态并复读一次活动状态。
func TestRunCheckinClaimsAndVerifies(t *testing.T) {
	fixture := newWelfareFixture(t, map[string][]fixtureReply{
		"POST /v2/billing/meter/checkin-activity-status": {
			{Body: checkinStatusBody(true, false, 0)},
			{Body: checkinStatusBody(true, true, 1)},
		},
		"POST /v2/billing/meter/daily-checkin": ok(`{"code":0,"msg":"OK","data":{}}`),
	})
	server := welfareTestServer(t, fixture)

	run := server.runCheckin(cnAccount(1, "eyJfake.oauth.sig"))
	if run == nil || run.Status != WelfareOK {
		t.Fatalf("状态 = %v，期望 ok", run)
	}
	if n := fixture.count("POST", "/v2/billing/meter/daily-checkin"); n != 1 {
		t.Fatalf("领取次数 = %d，期望 1", n)
	}
	if n := fixture.count("POST", "/v2/billing/meter/checkin-activity-status"); n != 2 {
		t.Fatalf("领取后应复读状态，实际读了 %d 次", n)
	}
	// 复读结果要落进状态（连登天数取上游口径）
	if run.State == nil || run.State.Checkin == nil || run.State.Checkin.StreakDays != 1 {
		t.Fatalf("复读后的连登天数没落库: %+v", run.State)
	}
}

// 活动没开（国际号常态 / CN 停摆）要记 skipped 而不是 failed。
func TestRunCheckinInactiveActivityIsSkipped(t *testing.T) {
	fixture := newWelfareFixture(t, map[string][]fixtureReply{
		"POST /v2/billing/meter/checkin-activity-status": ok(checkinStatusBody(false, false, 0)),
		"POST /v2/billing/meter/daily-checkin":           ok(`{"code":0,"msg":"OK"}`),
	})
	server := welfareTestServer(t, fixture)

	run := server.runCheckin(cnAccount(1, "ck_test"))
	if run == nil || run.Status != WelfareSkipped {
		t.Fatalf("状态 = %v，期望 skipped", run)
	}
	if n := fixture.count("POST", "/v2/billing/meter/daily-checkin"); n != 0 {
		t.Fatalf("活动没开还发了领取请求")
	}
}

// 读状态失败不能把账号判死，也不能发写请求。
func TestRunCheckinReadFailureDoesNotWrite(t *testing.T) {
	fixture := newWelfareFixture(t, map[string][]fixtureReply{
		"POST /v2/billing/meter/checkin-activity-status": {{Status: http.StatusBadGateway, Body: `{"code":10000}`}},
		"POST /v2/billing/meter/daily-checkin":           ok(`{"code":0,"msg":"OK"}`),
	})
	server := welfareTestServer(t, fixture)

	run := server.runCheckin(cnAccount(1, "ck_test"))
	if run == nil || run.Status != WelfareFailed {
		t.Fatalf("状态 = %v，期望 failed", run)
	}
	if n := fixture.count("POST", "/v2/billing/meter/daily-checkin"); n != 0 {
		t.Fatalf("读失败时不应发写请求")
	}
}

// realm 分流：intl 走打点，cn 走签到。
func TestRealmKindRouting(t *testing.T) {
	if got := realmKind(AccountProfile{RequestShape: ShapeIntl}); got != WelfareBonus {
		t.Errorf("intl 应该走 bonus，得到 %s", got)
	}
	if got := realmKind(AccountProfile{RequestShape: ShapeCN}); got != WelfareCheckin {
		t.Errorf("cn 应该走 checkin，得到 %s", got)
	}
}

// 时段解析只丢掉坏项，不整条回退。
func TestEnvIntListDropsInvalidItems(t *testing.T) {
	t.Setenv("WB_TEST_HOURS", "9,21,oops,24,-1")
	if got := envIntList("WB_TEST_HOURS", "8"); len(got) != 2 || got[0] != 9 || got[1] != 21 {
		t.Fatalf("得到 %v，期望 [9 21]", got)
	}
	t.Setenv("WB_TEST_HOURS", "oops")
	if fallback := envIntList("WB_TEST_HOURS", "8,12"); len(fallback) != 2 || fallback[0] != 8 || fallback[1] != 12 {
		t.Fatalf("全非法时应回落到默认 [8 12]，得到 %v", fallback)
	}
}

// 调度时段判断：命中的整点跑一次，同一小时不重复跑。
func TestWelfareSlotsRunOncePerHour(t *testing.T) {
	fixture := newWelfareFixture(t, map[string][]fixtureReply{
		"POST /v2/billing/meter/checkin-activity-status": ok(checkinStatusBody(true, true, 3)),
	})
	server := welfareTestServer(t, fixture)
	account := cnAccount(1, "ck_test")
	if _, err := server.store.AddAccount(AccountCreate{Name: "cn", APIKey: "ck_test_0123456789"}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	_ = account
	lastSlot := map[WelfareKind]string{}
	// 直接按整点判定逻辑测试：把当前小时塞进配置里
	hour := nowCSTHour()
	server.cfg.CheckinHours = []int{hour}
	server.cfg.DailyBonusHours = []int{}
	server.runWelfareSlots(t.Context(), lastSlot)
	first := fixture.count("POST", "/v2/billing/meter/checkin-activity-status")
	server.runWelfareSlots(t.Context(), lastSlot)
	second := fixture.count("POST", "/v2/billing/meter/checkin-activity-status")
	if first == 0 {
		t.Fatalf("当前小时在时段里却没执行")
	}
	if second != first {
		t.Fatalf("同一个小时重复执行了：%d -> %d", first, second)
	}
}

func nowCSTHour() int { return time.Now().In(cstLoc).Hour() }

func welfareTestServer(t *testing.T, fixture *welfareFixture) *Server {
	t.Helper()
	dbPath := t.TempDir() + "/welfare.sqlite3"
	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	host := strings.TrimPrefix(fixture.server.URL, "http://")
	cfg := Config{
		DBPath:          dbPath,
		UpstreamURL:     fixture.server.URL + "/v2/chat/completions",
		Domain:          host,
		AuthMode:        AuthModeAPIKey,
		RequestShape:    ShapeCN,
		Models:          []string{"glm-5.1"},
		CheckinAuto:     true,
		CheckinHours:    []int{9, 21},
		DailyBonusAuto:  true,
		DailyBonusHours: []int{8, 12, 16, 20},
	}
	return NewServer(cfg, store)
}

// fakeJWTSubject 造一个结构合法的 JWT（无有效签名），只为让 JWTSubject 取得到 sub。
func fakeJWTSubject(sub string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"` + sub + `","exp":2000000000}`))
	return header + "." + payload + ".not-a-real-signature"
}
