package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 每日福利：CN 签到 + intl 每日活跃奖励。
//
// 两个上游的「每日进项」是两条完全不同的路：
//
//	CN    Buddy 加油站签到，100 积分/天
//	      POST /v2/billing/meter/checkin-activity-status   只读，查今天签没签
//	      POST /v2/billing/meter/daily-checkin             领取
//
//	intl  成长热力墙，点亮一天 → 次日到账 30 积分的 Bonus Pack
//	      GET  /activity/growth/heatmap                     只读，今天热力值
//	      POST /v2/report                                   打一个 chat_request_send 事件把今天点亮
//
// 三条线上实测结论（2026-11-06，nuoapi-hk 直连上游）：
//
//  1. intl 的 /v2/report **不是**按天幂等的：每发一次热力值 +2（实测 0→2→4→6）。
//     所以必须先用 heatmap 读「今天是否已点亮」，score>0 就一个字节都不发。
//     事件里的 userId 必须是 access token 的 sub，留空虽然也回 200，但热力墙不动。
//  2. CN 的 daily-checkin 对 ck_ 订阅 key 一律 403
//     {"message":"API key not allowed for this path or method"}：这个写路径只对桌面端
//     OAuth 会话开放。ck_ 号只能读状态，领不了。所以 ck_ 号记 unsupported，不再每天重试。
//  3. CN 的 /activity/growth/heatmap 对 ck_ 号 401（APISIX），cn 侧只有签到这一条路。
//
// 幂等策略：写请求一次都不重试（CN 领取上游本身幂等，重复只会多一次风控面；
// intl 打点是计数器，重试就是刷分）。读请求允许一次 5xx 重试。

// WelfareKind 是福利任务种类。
type WelfareKind string

const (
	// WelfareCheckin CN 每日签到。
	WelfareCheckin WelfareKind = "checkin"
	// WelfareBonus intl 每日活跃奖励（成长热力墙点亮）。
	WelfareBonus WelfareKind = "bonus"
)

// 福利任务的结果状态。
const (
	// WelfareOK 本次真的完成了动作（签到了 / 点亮了）。
	WelfareOK = "ok"
	// WelfareAlready 上游说今天已经完成过，属于成功。
	WelfareAlready = "already"
	// WelfareUnsupported 该凭证类型上游不允许做这件事（ck_ 号签不了到）。
	WelfareUnsupported = "unsupported"
	// WelfareSkipped 今天该做的已经做过，或账号不适用。
	WelfareSkipped = "skipped"
	// WelfareFailed 真失败，下次 tick 还会再试。
	WelfareFailed = "failed"
)

const (
	checkinActivityPath = "/v2/billing/meter/checkin-activity-status"
	dailyCheckinPath    = "/v2/billing/meter/daily-checkin"
	growthHeatmapPath   = "/activity/growth/heatmap"
	growthReportPath    = "/v2/report"
)

// cstLoc 是上游判定「今天」用的时区。
//
// 两个上游都按中国时间切天：intl 的 Bonus Pack 在次日凌晨到账、CN 签到在 00:00 重置。
// 本地时区（服务器跑在 UTC）拿来做日界判断会错 8 小时，所以日键一律用它。
var cstLoc = time.FixedZone("CST", 8*3600)

// CheckinState 是 CN 签到在面板/接口上的完整状态。
type CheckinState struct {
	// Supported=false 表示这个凭证类型上游不给签（ck_ 订阅 key）。
	Supported      bool    `json:"supported"`
	Active         bool    `json:"active"`           // 活动是否生效
	TodayCheckedIn bool    `json:"today_checked_in"` // 上游权威口径
	StreakDays     int64   `json:"streak_days"`
	DailyCredit    float64 `json:"daily_credit"`
	TodayCredit    float64 `json:"today_credit"`
	WeekChecked    int64   `json:"week_checkin_days"`
	TotalCredits   float64 `json:"total_credits"`
	Activity       string  `json:"activity_name"`
	Theme          string  `json:"theme_name"`
	EndTime        string  `json:"end_time"`
	Day            string  `json:"day"`
	Status         string  `json:"status"`
	Detail         string  `json:"detail,omitempty"`
	CheckedAt      *int64  `json:"checked_at,omitempty"`
}

// BonusState 是 intl 每日奖励的状态。
type BonusState struct {
	Day        string `json:"day"`   // 上游热力墙的「今天」（中国时间）
	Score      int    `json:"score"` // 今天的热力值，>0 = 已点亮
	Lit        bool   `json:"lit"`
	StatusText string `json:"status_text,omitempty"`
	Status     string `json:"status"`
	Detail     string `json:"detail,omitempty"`
	CheckedAt  *int64 `json:"checked_at,omitempty"`
}

// WelfareState 是挂在账号上的一对福利状态。
type WelfareState struct {
	Checkin *CheckinState `json:"checkin,omitempty"`
	Bonus   *BonusState   `json:"bonus,omitempty"`
}

// checkinActivityStatus 是 /v2/billing/meter/checkin-activity-status 的响应。
type checkinActivityStatus struct {
	Active          bool    `json:"active"`
	TodayCheckedIn  bool    `json:"today_checked_in"`
	StreakDays      int64   `json:"streak_days"`
	DailyCredit     float64 `json:"daily_credit"`
	TodayCredit     float64 `json:"today_credit"`
	WeekCheckinDays int64   `json:"week_checkin_days"`
	TotalCredits    float64 `json:"total_credits"`
	ActivityName    string  `json:"activity_name"`
	ThemeName       string  `json:"theme_name"`
	EndTime         string  `json:"end_time"`
}

// growthHeatmap 是 /activity/growth/heatmap 的 data 段。
type growthHeatmap struct {
	Cells []struct {
		Date  string `json:"date"`
		Score int    `json:"score"`
	} `json:"cells"`
}

// welfareResponse 是上游统一信封（code/msg/data），data 原样留着按需解析。
type welfareResponse struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// welfareHTTPError 保留上游的 HTTP 状态与响应片段，调用方要按 403/404 分流。
type welfareHTTPError struct {
	StatusCode int
	Body       string
}

func (e *welfareHTTPError) Error() string {
	return fmt.Sprintf("welfare http %d: %s", e.StatusCode, truncate(e.Body, 200))
}

// apiKeyNotAllowedMarker 是上游「这种 key 不许走这个路径」的固定文案。
//
// CK 账号的每日签到写路径就是被它挡住的；识别出来才能把「不适用」和「真失败」分开，
// 否则每天两次无意义的重试，日志里永远是一条假的红色错误。
const apiKeyNotAllowedMarker = "API key not allowed for this path or method"

// welfareBase 返回某账号的计费/活动域基址（scheme://host），取自它生效的上游地址。
//
// CN 与 intl 的 chat 端点与 billing 端点同域（实测 copilot.tencent.com、
// www.codebuddy.ai、www.workbuddy.ai 都是同域不同路径），所以直接从 UpstreamURL 推导。
func (c *UpstreamClient) welfareBase(account Account) (string, error) {
	parsed, err := url.Parse(c.ProfileFor(account).UpstreamURL)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("上游地址不可解析: %q", c.ProfileFor(account).UpstreamURL)
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

// welfareCall 发一次福利接口请求。
//
// 认证与积分查询同源（BuildHeaders），因此 ck_ 号发 X-Api-Key、OAuth 号发
// Authorization: Bearer，不需要在这里分支。计费族要绕过 WAF 的非浏览器 UA 拦截，
// 并去掉对话专有的 X-Agent-Intent。
func (c *UpstreamClient) welfareCall(account Account, method, path string, body any) (*welfareResponse, error) {
	base, err := c.welfareBase(account)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	} else if method == http.MethodPost {
		// 上游计费族无参 POST 的 body 固定是 `{}`（真机 Content-Length: 2）
		reader = strings.NewReader("{}")
	}
	req, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		return nil, err
	}
	headers := c.BuildHeaders(account)
	headers.Set("User-Agent", billingUA)
	headers.Set("Accept", "application/json")
	headers.Del("X-Agent-Intent")
	req.Header = headers

	client, err := c.httpClient(account)
	if err != nil {
		return nil, err
	}
	transportClient := *client
	transportClient.Timeout = 30 * time.Second
	resp, err := transportClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode != http.StatusOK {
		return nil, &welfareHTTPError{StatusCode: resp.StatusCode, Body: string(raw)}
	}
	var parsed welfareResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("welfare decode %s: %w", path, err)
	}
	if parsed.Code != 0 {
		return nil, fmt.Errorf("welfare %s code %d: %s", path, parsed.Code, truncate(parsed.Msg, 160))
	}
	return &parsed, nil
}

// isAPIKeyNotAllowed 报告 err 是否为「这类 key 不许走这个路径」。
func isAPIKeyNotAllowed(err error) bool {
	var httpErr *welfareHTTPError
	if !asError(err, &httpErr) {
		return false
	}
	return httpErr.StatusCode == http.StatusForbidden &&
		strings.Contains(httpErr.Body, apiKeyNotAllowedMarker)
}

// asError 是 errors.As 的薄封装，避免 welfare.go 依赖 errors 包名与外部变量冲突。
func asError(err error, target **welfareHTTPError) bool {
	for err != nil {
		if typed, ok := err.(*welfareHTTPError); ok {
			*target = typed
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

// retryWelfareRead 对只读调用做一次 5xx 重试。
//
// 只给读路径用：写路径（签到领取、打点）一次都不重试，理由见文件头。
func retryWelfareRead(fn func() (*welfareResponse, error)) (*welfareResponse, error) {
	response, err := fn()
	if err == nil {
		return response, nil
	}
	var httpErr *welfareHTTPError
	if !asError(err, &httpErr) || httpErr.StatusCode < 500 {
		return nil, err
	}
	time.Sleep(2 * time.Second)
	return fn()
}

// FetchCheckinActivity 读 CN 签到状态（只读，不消耗、不改变任何东西）。
func (c *UpstreamClient) FetchCheckinActivity(account Account) (checkinActivityStatus, error) {
	var status checkinActivityStatus
	response, err := retryWelfareRead(func() (*welfareResponse, error) {
		return c.welfareCall(account, http.MethodPost, checkinActivityPath, nil)
	})
	if err != nil {
		return status, err
	}
	if err := json.Unmarshal(response.Data, &status); err != nil {
		return status, fmt.Errorf("checkin status decode: %w", err)
	}
	return status, nil
}

// ClaimDailyCheckin 领取 CN 每日签到。
//
// 不重试：上游本身对重复领取返回幂等错误，但多发一次就多一次风控面，而且
// 网络层失败时上游可能已经发放成功，重放等于拿不确定的结果去赌。
func (c *UpstreamClient) ClaimDailyCheckin(account Account) error {
	_, err := c.welfareCall(account, http.MethodPost, dailyCheckinPath, nil)
	return err
}

// FetchGrowthHeatmap 读 intl 成长热力墙（只读）。
func (c *UpstreamClient) FetchGrowthHeatmap(account Account) (growthHeatmap, error) {
	var heatmap growthHeatmap
	response, err := retryWelfareRead(func() (*welfareResponse, error) {
		return c.welfareCall(account, http.MethodGet, growthHeatmapPath, nil)
	})
	if err != nil {
		return heatmap, err
	}
	if err := json.Unmarshal(response.Data, &heatmap); err != nil {
		return heatmap, fmt.Errorf("heatmap decode: %w", err)
	}
	if len(heatmap.Cells) == 0 {
		return heatmap, fmt.Errorf("heatmap 为空")
	}
	return heatmap, nil
}

// SendGrowthActivity 打一个 chat_request_send 事件，把今天点亮。
//
// 字段与官方客户端一致（camelCase，且空数组也要在），userId 必须是 access token 的
// sub —— 实测留空也回 200，但热力墙的 score 不动，等于白打。
func (c *UpstreamClient) SendGrowthActivity(account Account) error {
	conversation := "wb-" + randomHex(16)
	nowMillis := time.Now().UnixMilli()
	event := map[string]any{
		"eventCode":            "chat_request_send",
		"timestamp":            nowMillis,
		"mode":                 "craft",
		"conversationId":       conversation,
		"requestId":            conversation,
		"inputLength":          12,
		"requestModelId":       "deepseek-v4-flash",
		"requestModelName":     "DeepSeek V4 Flash",
		"mentionContexts":      []any{},
		"knowledgeId":          []any{},
		"knowledgeName":        []any{},
		"presentAt":            nowMillis,
		"rootRequestId":        conversation,
		"parentConversationId": conversation,
		"agentName":            "default",
		"agentType":            "conversation",
		"userId":               JWTSubject(account.APIKey),
	}
	_, err := c.welfareCall(account, http.MethodPost, growthReportPath, []any{event})
	return err
}

// -----------------------------------------------------------------------------
// 调度与执行
// -----------------------------------------------------------------------------

// welfareRun 是一次任务在单个账号上的结果，只用于日志与接口返回。
type welfareRun struct {
	AccountID int64         `json:"account_id"`
	Name      string        `json:"name"`
	Kind      WelfareKind   `json:"kind"`
	Status    string        `json:"status"`
	Detail    string        `json:"detail,omitempty"`
	State     *WelfareState `json:"state,omitempty"`
}

// realmOf 判断账号属于哪一侧：intl（含 workbuddy 类型）走成长热力墙，其余走 CN 签到。
func realmOf(profile AccountProfile) string {
	if profile.RequestShape == ShapeIntl {
		return ShapeIntl
	}
	return ShapeCN
}

// todayCST 返回中国时区下的今天（YYYY-MM-DD）。
func todayCST() string {
	return time.Now().In(cstLoc).Format("2006-01-02")
}

// RunWelfare 跑一轮福利任务，返回每个账号的结果。
//
// 幂等边界：
//   - CN：上游 today_checked_in 是权威口径，已签就直接记 already；
//     ck_ 号的 403 记 unsupported（凭证类型限制，不是故障），不再每天重试。
//   - intl：先读热力墙，score>0 就当今天已完成；只有 score==0 才发一次打点，
//     发完复读一次确认真的点亮（上游对「接受了但没生效」不会报错）。
func (s *Server) RunWelfare(ctx context.Context) map[string]any {
	accounts, err := s.store.ListAccounts()
	if err != nil {
		return map[string]any{"ok": false, "detail": err.Error()}
	}
	runs := make([]welfareRun, 0, len(accounts))
	counts := map[string]int{}
	for _, account := range accounts {
		if ctx.Err() != nil {
			break
		}
		if !account.Enabled {
			continue
		}
		full, err := s.store.GetAccount(account.ID)
		if err != nil || full == nil {
			continue
		}
		run := s.runWelfareFor(*full)
		if run == nil {
			continue
		}
		counts[string(run.Kind)+":"+run.Status]++
		runs = append(runs, *run)
	}
	summary := map[string]any{"ok": true, "runs": runs, "counts": counts}
	log.Printf("welfare: 本轮完成 %d 个账号 %v", len(runs), counts)
	return summary
}

// runWelfareFor 按账号所属 realm 跑对应任务；返回 nil 表示这个号没有适用任务。
func (s *Server) runWelfareFor(account Account) *welfareRun {
	profile := s.upstream.ProfileFor(account)
	kind := WelfareBonus
	if realmOf(profile) == ShapeCN {
		kind = WelfareCheckin
	}
	switch kind {
	case WelfareCheckin:
		if !s.cfg.CheckinAuto {
			return nil
		}
		return s.runCheckin(account)
	default:
		if !s.cfg.DailyBonusAuto {
			return nil
		}
		return s.runDailyBonus(account)
	}
}

// runCheckin 处理一个账号的 CN 签到。
func (s *Server) runCheckin(account Account) *welfareRun {
	run := &welfareRun{AccountID: account.ID, Name: account.Name, Kind: WelfareCheckin}
	day := todayCST()

	stored := s.store.Welfare(account.ID)
	state := &WelfareState{Checkin: &CheckinState{Supported: true, Day: day}}

	status, err := s.upstream.FetchCheckinActivity(account)
	if err != nil {
		// 读失败不改状态：活动关没关、网络通不通，明天还有机会，不能据此把号判死。
		// 也不能存一个 Supported=false 的空壳——那会让面板把「查不到」说成「凭证不支持」。
		detail := truncate(err.Error(), 200)
		state.Checkin.Supported = true
		state.Checkin.Status = WelfareFailed
		state.Checkin.Detail = detail
		s.store.SaveWelfareCheckin(account.ID, day, WelfareFailed, detail, state.Checkin)
		run.Status = WelfareFailed
		run.Detail = detail
		run.State = state
		log.Printf("welfare: #%d 签到状态查询失败: %v", account.ID, err)
		return run
	}
	state.Checkin.Active = status.Active
	state.Checkin.TodayCheckedIn = status.TodayCheckedIn
	state.Checkin.StreakDays = status.StreakDays
	state.Checkin.DailyCredit = status.DailyCredit
	state.Checkin.TodayCredit = status.TodayCredit
	state.Checkin.WeekChecked = status.WeekCheckinDays
	state.Checkin.TotalCredits = status.TotalCredits
	state.Checkin.Activity = status.ActivityName
	state.Checkin.Theme = status.ThemeName
	state.Checkin.EndTime = status.EndTime
	switch {
	case !status.Active:
		// 活动没开（国际号常态、CN 停摆期）——不是错误，但要说清楚
		state.Checkin.Status = WelfareSkipped
		state.Checkin.Detail = "签到活动未开启"
		run.Status = WelfareSkipped
		run.Detail = "签到活动未开启"
	case status.TodayCheckedIn:
		state.Checkin.Status = WelfareAlready
		state.Checkin.Detail = fmt.Sprintf("今天已签到（连登 %d 天）", status.StreakDays)
		run.Status = WelfareAlready
		run.Detail = state.Checkin.Detail
	case stored != nil && stored.Checkin != nil && !stored.Checkin.Supported &&
		stored.Checkin.Day == day:
		// 今天已经确认过这个凭证签不了，别一天问两次
		*state.Checkin = *stored.Checkin
		state.Checkin.Status = WelfareUnsupported
		run.Status = WelfareUnsupported
		run.Detail = state.Checkin.Detail
	default:
		if err := s.upstream.ClaimDailyCheckin(account); err != nil {
			if isAPIKeyNotAllowed(err) {
				// ck_ 订阅 key 的写路径被上游按策略挡住：这不是故障，是这类凭证没有权限。
				// 记录一次并停止每天重试，否则日志里天天是一条假红。
				detail := "ck_ 订阅 key 不支持签到（上游 403: 该路径只对桌面端 OAuth 会话开放）"
				state.Checkin.Supported = false
				state.Checkin.Status = WelfareUnsupported
				state.Checkin.Detail = detail
				run.Status = WelfareUnsupported
				run.Detail = detail
				log.Printf("welfare: #%d 签到不适用: %s", account.ID, detail)
				break
			}
			detail := truncate(err.Error(), 200)
			state.Checkin.Status = WelfareFailed
			state.Checkin.Detail = detail
			run.Status = WelfareFailed
			run.Detail = detail
			log.Printf("welfare: #%d 签到失败: %v", account.ID, err)
			break
		}
		// 领完复读一次：上游今天到底是领到了还是本来就签过，以状态接口为准
		if refreshed, err := s.upstream.FetchCheckinActivity(account); err == nil {
			state.Checkin.TodayCheckedIn = refreshed.TodayCheckedIn
			state.Checkin.StreakDays = refreshed.StreakDays
			state.Checkin.TodayCredit = refreshed.TodayCredit
		} else {
			state.Checkin.TodayCheckedIn = true
		}
		state.Checkin.Status = WelfareOK
		state.Checkin.Detail = fmt.Sprintf("签到成功，+%g 积分", status.DailyCredit)
		run.Status = WelfareOK
		run.Detail = state.Checkin.Detail
		log.Printf("welfare: #%d 签到成功（每日 %g 积分）", account.ID, status.DailyCredit)
	}

	// 注意：这里不能用 status.StreakDays 覆盖——领取前读到的是领取前的值，
	// 复读已经更新了它；再盖一遍会把「连登 1 天」重新写成 0。
	s.store.SaveWelfareCheckin(account.ID, state.Checkin.Day, state.Checkin.Status,
		state.Checkin.Detail, state.Checkin)
	run.State = state
	return run
}

// runDailyBonus 处理一个账号的 intl 每日奖励。
func (s *Server) runDailyBonus(account Account) *welfareRun {
	run := &welfareRun{AccountID: account.ID, Name: account.Name, Kind: WelfareBonus}
	state := &WelfareState{Bonus: &BonusState{}}

	heatmap, err := s.upstream.FetchGrowthHeatmap(account)
	if err != nil {
		detail := truncate(err.Error(), 200)
		state.Bonus.Status = WelfareFailed
		state.Bonus.Detail = detail
		s.store.SaveWelfareBonus(account.ID, "", 0, WelfareFailed, detail, state.Bonus)
		run.Status = WelfareFailed
		run.Detail = detail
		log.Printf("welfare: #%d 热力墙查询失败: %v", account.ID, err)
		return run
	}
	today := heatmap.Cells[len(heatmap.Cells)-1]
	state.Bonus.Day = today.Date
	state.Bonus.Score = today.Score
	state.Bonus.Lit = today.Score > 0

	// 已点亮：打点是加分计数器，再发一次等于刷分（实测每发一次 +2）
	if today.Score > 0 {
		state.Bonus.Status = WelfareAlready
		state.Bonus.Detail = fmt.Sprintf("%s 已点亮（热力值 %d）", today.Date, today.Score)
		run.Status = WelfareAlready
		run.Detail = state.Bonus.Detail
		s.store.SaveWelfareBonus(account.ID, state.Bonus.Day, state.Bonus.Score, run.Status, run.Detail, state.Bonus)
		run.State = state
		return run
	}
	// userId 必须是 token 的 sub：留空时上游仍回 200，但热力墙的 score 不动。
	// 与其发一个必然无效的请求（还会占一个热力值计数），不如直接报清楚原因。
	if JWTSubject(account.APIKey) == "" {
		detail := "凭证不是 JWT（取不到 sub），每日奖励无法打点"
		state.Bonus.Status = WelfareUnsupported
		state.Bonus.Detail = detail
		run.Status = WelfareUnsupported
		run.Detail = detail
		s.store.SaveWelfareBonus(account.ID, state.Bonus.Day, state.Bonus.Score, run.Status, run.Detail, state.Bonus)
		run.State = state
		log.Printf("welfare: #%d %s", account.ID, detail)
		return run
	}
	if err := s.upstream.SendGrowthActivity(account); err != nil {
		detail := truncate(err.Error(), 200)
		state.Bonus.Status = WelfareFailed
		state.Bonus.Detail = detail
		run.Status = WelfareFailed
		run.Detail = detail
		log.Printf("welfare: #%d 每日奖励打点失败: %v", account.ID, err)
	} else {
		// 上游收下了不代表生效，复读一次热力墙确认真的点亮
		if verify, verifyErr := s.upstream.FetchGrowthHeatmap(account); verifyErr == nil {
			cell := verify.Cells[len(verify.Cells)-1]
			state.Bonus.Day = cell.Date
			state.Bonus.Score = cell.Score
			state.Bonus.Lit = cell.Score > 0
		} else {
			state.Bonus.Lit = true
		}
		if state.Bonus.Lit {
			state.Bonus.Status = WelfareOK
			state.Bonus.Detail = fmt.Sprintf("%s 已点亮，次日到账 30 积分", state.Bonus.Day)
			run.Status = WelfareOK
			run.Detail = state.Bonus.Detail
			log.Printf("welfare: #%d 每日奖励打点成功（%s）", account.ID, state.Bonus.Day)
		} else {
			state.Bonus.Status = WelfareFailed
			state.Bonus.Detail = "上游接受了打点但热力墙未点亮"
			run.Status = WelfareFailed
			run.Detail = state.Bonus.Detail
			log.Printf("welfare: #%d 打点后热力墙仍未点亮", account.ID)
		}
	}

	s.store.SaveWelfareBonus(account.ID, state.Bonus.Day, state.Bonus.Score,
		state.Bonus.Status, state.Bonus.Detail, state.Bonus)
	run.State = state
	return run
}

// StartWelfareLoop 启动福利调度。
//
// 每 5 分钟醒一次，只在配置的整点时段内真正执行；同一「任务+小时」每天只跑一次
// （进程内记录，重启后由上游状态接口兜底重放，不会重复领取）。
func (s *Server) StartWelfareLoop(ctx context.Context) {
	if !s.cfg.CheckinAuto && !s.cfg.DailyBonusAuto {
		log.Printf("welfare: 签到与每日奖励都已关闭，调度不启动")
		return
	}
	log.Printf("welfare: 调度已启动（CN 签到 %v，intl 每日奖励 %v，中国时区）",
		s.cfg.CheckinHours, s.cfg.DailyBonusHours)
	go func() {
		// 最后执行过的时段键：kind -> 2026-11-06T09
		lastSlot := map[WelfareKind]string{}
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		// 启动后先跑一次：服务重启（部署、OOM）之后不该等到下一个整点才补今天的活
		s.runWelfareSlots(ctx, lastSlot)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			s.runWelfareSlots(ctx, lastSlot)
		}
	}()
}

// runWelfareSlots 判断当前时段该跑哪些任务。
func (s *Server) runWelfareSlots(ctx context.Context, lastSlot map[WelfareKind]string) {
	now := time.Now().In(cstLoc)
	for kind, hours := range map[WelfareKind][]int{
		WelfareCheckin: s.cfg.CheckinHours,
		WelfareBonus:   s.cfg.DailyBonusHours,
	} {
		if kind == WelfareCheckin && !s.cfg.CheckinAuto {
			continue
		}
		if kind == WelfareBonus && !s.cfg.DailyBonusAuto {
			continue
		}
		if !containsInt(hours, now.Hour()) {
			continue
		}
		slot := fmt.Sprintf("%sT%02d", now.Format("2006-01-02"), now.Hour())
		if lastSlot[kind] == slot {
			continue
		}
		lastSlot[kind] = slot
		s.RunWelfareKind(ctx, kind, 0)
	}
}

// RunWelfareKind 只跑某一类福利任务（手动触发与定时共用）。
// accountID > 0 时只跑那一个账号（面板上的行内按钮）。
func (s *Server) RunWelfareKind(ctx context.Context, kind WelfareKind, accountID int64) map[string]any {
	accounts, err := s.store.ListAccounts()
	if err != nil {
		return map[string]any{"ok": false, "detail": err.Error()}
	}
	runs := make([]welfareRun, 0, len(accounts))
	counts := map[string]int{}
	for _, account := range accounts {
		if ctx.Err() != nil {
			break
		}
		if accountID > 0 && account.ID != accountID {
			continue
		}
		if !account.Enabled {
			continue
		}
		full, err := s.store.GetAccount(account.ID)
		if err != nil || full == nil {
			continue
		}
		if realmKind(s.upstream.ProfileFor(*full)) != kind {
			continue
		}
		run := s.runWelfareFor(*full)
		if run == nil {
			continue
		}
		counts[run.Status]++
		runs = append(runs, *run)
	}
	log.Printf("welfare: %s 手动/定时执行完成 %v", kind, counts)
	return map[string]any{"ok": true, "kind": kind, "runs": runs, "counts": counts}
}

// realmKind 把 realm 映射到福利任务种类。
func realmKind(profile AccountProfile) WelfareKind {
	if realmOf(profile) == ShapeIntl {
		return WelfareBonus
	}
	return WelfareCheckin
}

// containsInt 判断整型切片里有没有目标值（配置的时段列表）。
func containsInt(values []int, target int) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// runWelfareOne 只跑一个账号的福利任务，返回与批量接口同形状的摘要。
func (s *Server) runWelfareOne(ctx context.Context, account Account) map[string]any {
	_ = ctx
	run := s.runWelfareFor(account)
	if run == nil {
		return map[string]any{"ok": false, "detail": "该账号没有启用的福利任务"}
	}
	return map[string]any{"ok": true, "runs": []welfareRun{*run}, "counts": map[string]int{run.Status: 1}}
}
