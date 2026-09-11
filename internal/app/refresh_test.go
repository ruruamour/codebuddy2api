package app

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// makeJWT 造一个 exp 为 daysFromNow 天后的假 JWT（签名部分不参与解析）。
func makeJWT(t *testing.T, daysFromNow int) string {
	t.Helper()
	claims, err := json.Marshal(map[string]any{
		"exp": time.Now().AddDate(0, 0, daysFromNow).Unix(),
		"sub": "test-subject",
	})
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
}

// 并发续期必须只打一次上游。
//
// 余额轮询、续期巡检、面板手动刷新是三条独立路径，会同时命中同一个账号。
// refresh token 每次使用都会轮换，若并发刷新，后完成的那次会把已作废的凭证写回库里，
// 账号从此再也换不到新 token——正是这个功能要防的静默失效。
func TestEnsureFreshToken_ConcurrentRefreshHitsUpstreamOnce(t *testing.T) {
	var upstreamCalls int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, refreshPath) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		atomic.AddInt64(&upstreamCalls, 1)
		time.Sleep(40 * time.Millisecond) // 拉长窗口，让并发真的重叠
		fmt.Fprintf(w, `{"code":0,"data":{"accessToken":%q,"refreshToken":"rotated-token","expiresIn":31536000}}`,
			makeJWT(t, 365))
	}))
	defer server.Close()

	store := newTestStore(t)
	cfg := Config{
		AuthMode:              AuthModeBearer,
		UpstreamURL:           server.URL + "/v2/chat/completions",
		Domain:                "www.codebuddy.ai",
		RequestShape:          ShapeIntl,
		Models:                []string{"deepseek-v4.1-flash"},
		TokenRefreshAheadDays: 30,
		RequestTimeoutSeconds: 30,
		ConnectTimeoutSeconds: 10,
	}
	srv := NewServer(cfg, store)

	// 一个还剩 5 天的 token：落在 30 天续期窗口内
	id, err := store.AddAccount(AccountCreate{
		Name:         "并发续期测试号",
		APIKey:       makeJWT(t, 5),
		RefreshToken: strPtr("seed-refresh-token"),
	})
	if err != nil {
		t.Fatalf("add account: %v", err)
	}

	// 8 条路径同时冲同一个账号
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			srv.refreshAccountToken(id)
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&upstreamCalls); got != 1 {
		t.Fatalf("并发续期打了 %d 次上游，应该只有 1 次——重复刷新会作废 refresh token", got)
	}

	// 轮换后的 refresh token 必须落库，否则下次就续不了了
	stored, err := store.RefreshTokenFor(id)
	if err != nil {
		t.Fatalf("read refresh token: %v", err)
	}
	if stored != "rotated-token" {
		t.Fatalf("库里的 refresh token 是 %q，应为轮换后的 rotated-token", stored)
	}
}

// 寿命还很长的 token 不该被刷新——否则每轮巡检都会白白轮换一次凭证。
func TestEnsureFreshToken_SkipsHealthyToken(t *testing.T) {
	var called int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&called, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	store := newTestStore(t)
	srv := NewServer(Config{
		AuthMode:              AuthModeBearer,
		UpstreamURL:           server.URL + "/v2/chat/completions",
		TokenRefreshAheadDays: 30,
		RequestTimeoutSeconds: 30,
		ConnectTimeoutSeconds: 10,
	}, store)

	id, err := store.AddAccount(AccountCreate{
		Name:         "健康号",
		APIKey:       makeJWT(t, 365),
		RefreshToken: strPtr("seed"),
	})
	if err != nil {
		t.Fatalf("add account: %v", err)
	}
	if srv.refreshAccountToken(id) {
		t.Fatal("剩 365 天的 token 不该触发续期")
	}
	if atomic.LoadInt64(&called) != 0 {
		t.Fatal("不该打上游")
	}
}

// ck_ 账号走不到续期逻辑，哪怕实例是 bearer 模式。
func TestEnsureFreshToken_SkipsAPIKeyAccount(t *testing.T) {
	store := newTestStore(t)
	srv := NewServer(Config{
		AuthMode:              AuthModeBearer,
		UpstreamURL:           "https://example.invalid/v2/chat/completions",
		TokenRefreshAheadDays: 30,
	}, store)

	id, err := store.AddAccount(AccountCreate{Name: "ck 号", APIKey: "ck_" + strings.Repeat("a", 40)})
	if err != nil {
		t.Fatalf("add account: %v", err)
	}
	account, err := store.GetAccount(id)
	if err != nil || account == nil {
		t.Fatalf("get account: %v", err)
	}
	refreshed, err := srv.ensureFreshToken(*account)
	if err != nil {
		t.Fatalf("ck 号不该报错: %v", err)
	}
	if refreshed {
		t.Fatal("ck 号不该被续期")
	}
}

func strPtr(s string) *string { return &s }

// 已过期的 token 必须在请求路径上被兜底续期。
//
// 后台巡检正常时轮不到这里；它挡的是巡检空窗——服务刚启动或长期停机后重启，
// 此时 token 可能已经过期，没有兜底就会一直失败到下一轮巡检。
func TestEnsureUsableToken_RefreshesExpiredToken(t *testing.T) {
	fresh := makeJWT(t, 365)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, refreshPath) {
			fmt.Fprintf(w, `{"code":0,"data":{"accessToken":%q,"refreshToken":"rotated","expiresIn":31536000}}`, fresh)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	store := newTestStore(t)
	srv := NewServer(Config{
		AuthMode:              AuthModeBearer,
		UpstreamURL:           server.URL + "/v2/chat/completions",
		TokenRefreshAheadDays: 30,
		RequestTimeoutSeconds: 30,
		ConnectTimeoutSeconds: 10,
	}, store)

	id, err := store.AddAccount(AccountCreate{
		Name:         "过期号",
		APIKey:       makeJWT(t, -1), // 昨天就过期了
		RefreshToken: strPtr("seed"),
	})
	if err != nil {
		t.Fatalf("add account: %v", err)
	}
	account, err := store.GetAccount(id)
	if err != nil || account == nil {
		t.Fatalf("get account: %v", err)
	}

	got := srv.ensureUsableToken(*account)
	if got.APIKey != fresh {
		t.Fatal("过期 token 没有被兜底续期，请求会一直失败到下一轮巡检")
	}
}

// 没过期的 token 不该触发任何续期动作（这是热路径，不能每次都查库/打上游）。
func TestEnsureUsableToken_LeavesValidTokenAlone(t *testing.T) {
	var called int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&called, 1)
	}))
	defer server.Close()

	store := newTestStore(t)
	srv := NewServer(Config{
		AuthMode: AuthModeBearer, UpstreamURL: server.URL + "/v2/chat/completions",
		TokenRefreshAheadDays: 30, RequestTimeoutSeconds: 30, ConnectTimeoutSeconds: 10,
	}, store)

	token := makeJWT(t, 200)
	id, err := store.AddAccount(AccountCreate{Name: "正常号", APIKey: token, RefreshToken: strPtr("seed")})
	if err != nil {
		t.Fatalf("add account: %v", err)
	}
	account, _ := store.GetAccount(id)
	if got := srv.ensureUsableToken(*account); got.APIKey != token {
		t.Fatal("没过期的 token 被动了")
	}
	if atomic.LoadInt64(&called) != 0 {
		t.Fatal("没过期却打了上游")
	}
}
