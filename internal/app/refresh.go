package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// OAuth access token 自助续期。
//
// intl 网关（www.codebuddy.ai / www.workbuddy.ai）的 access token 是标准 Keycloak JWT，
// 默认 365 天；refresh token 同样是 365 天且每次刷新都会轮换（返回新的 refresh token）。
//
// 刷新姿势（逆向自官方 CLI / buddy-proxy 实测）：
//
//	POST /v2/plugin/auth/token/refresh   body {}
//	关键头：X-Refresh-Token: <refresh_token>、X-Auth-Refresh-Source: plugin
//	注意 refresh token 不在 body 里，放 body 会得到 10001 refreshToken is empty
const (
	refreshPath          = "/v2/plugin/auth/token/refresh"
	refreshAuthSourceKey = "X-Auth-Refresh-Source"
)

// RefreshAccessToken 用 refresh token 换一对新的 access/refresh token。
func (c *UpstreamClient) RefreshAccessToken(account Account, refreshToken string) (string, string, int64, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return "", "", 0, fmt.Errorf("refresh token is empty")
	}
	profile := c.ProfileFor(account)
	domain := profile.Domain
	if domain == "" {
		domain = "www.codebuddy.ai"
	}
	base, err := url.Parse(profile.UpstreamURL)
	if err != nil {
		return "", "", 0, err
	}
	endpoint := fmt.Sprintf("%s://%s%s", base.Scheme, base.Host, refreshPath)

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", "", 0, err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-Request-Id", strings.ReplaceAll(uuid.NewString(), "-", ""))
	req.Header.Set("X-Domain", domain)
	req.Header.Set("X-Refresh-Token", refreshToken)
	req.Header.Set(refreshAuthSourceKey, "plugin")
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("User-Agent", fmt.Sprintf("CLI/%s CodeBuddy/%s", officialCLIVersion, officialCLIVersion))
	if token := strings.TrimSpace(account.APIKey); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client, err := c.httpClient(account)
	if err != nil {
		return "", "", 0, err
	}
	transportClient := *client
	transportClient.Timeout = 30 * time.Second
	resp, err := transportClient.Do(req)
	if err != nil {
		return "", "", 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode != http.StatusOK {
		return "", "", 0, fmt.Errorf("refresh http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var parsed struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AccessToken      string `json:"accessToken"`
			RefreshToken     string `json:"refreshToken"`
			ExpiresIn        int64  `json:"expiresIn"`
			RefreshExpiresIn int64  `json:"refreshExpiresIn"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", "", 0, fmt.Errorf("refresh decode: %w", err)
	}
	if parsed.Code != 0 || parsed.Data.AccessToken == "" {
		return "", "", 0, fmt.Errorf("refresh code %d: %s", parsed.Code, parsed.Msg)
	}
	var expiresAt int64
	if parsed.Data.ExpiresIn > 0 {
		expiresAt = time.Now().Unix() + parsed.Data.ExpiresIn
	} else if exp := JWTExpiry(parsed.Data.AccessToken); exp > 0 {
		expiresAt = exp
	}
	if parsed.Data.RefreshToken == "" {
		parsed.Data.RefreshToken = refreshToken // 没轮换就沿用旧的
	}
	return parsed.Data.AccessToken, parsed.Data.RefreshToken, expiresAt, nil
}

// JWTExpiry 解出 JWT 的 exp（unix 秒）；不是 JWT 或解不出来返回 0。
func JWTExpiry(token string) int64 {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return 0
	}
	return claims.Exp
}

// refreshMu 串行化续期。
//
// 有三条路径会并发走到这里：余额轮询、续期巡检、面板上的手动刷新。而 refresh token
// 每次使用都会轮换——两次并发刷新里，后完成的那次会把一个已经被作废的 refresh token
// 写回库里，账号从此再也换不到新 token。那正是这个功能本身要防的「静默失效」。
//
// 续期一年才发生一次，直接全局串行即可；真正挡住重复刷新的是拿到锁之后的重新读取。
var refreshMu sync.Mutex

// ensureFreshToken 在 Bearer 模式下检查 access token 剩余寿命，快过期就用 refresh token 换新的。
// 返回是否真的换过。
func (s *Server) ensureFreshToken(account Account) (bool, error) {
	// 按账号判断，而不是实例级——同一个实例里可能既有 ck_ 号又有凭证号
	if !s.upstream.ProfileFor(account).IsBearer() {
		return false, nil
	}
	if !s.needsRefresh(account.APIKey) {
		return false, nil
	}

	refreshMu.Lock()
	defer refreshMu.Unlock()

	// 拿到锁后重新读一次：如果刚才另一条路径已经续过期了，这里看到的就是新 token，
	// 判定不再需要续期，直接退出——重复刷新就是这样被挡掉的。
	latest, err := s.store.GetAccount(account.ID)
	if err != nil {
		return false, err
	}
	if latest == nil {
		return false, nil
	}
	account = *latest
	if !s.needsRefresh(account.APIKey) {
		return false, nil
	}

	refreshToken, err := s.store.RefreshTokenFor(account.ID)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(refreshToken) == "" {
		return false, fmt.Errorf("账号 #%d 剩余寿命不足但没存 refresh token", account.ID)
	}
	access, refresh, expiresAt, err := s.upstream.RefreshAccessToken(account, refreshToken)
	if err != nil {
		return false, err
	}
	if err := s.store.SaveAccessToken(account.ID, access, refresh, expiresAt); err != nil {
		return false, err
	}
	return true, nil
}

// ensureUsableToken 在真正发请求前兜底：token 已经过期就立刻续一次，并用续期后的账号。
//
// 正常情况下后台巡检会提前 30 天换掉，轮不到这里。它挡的是巡检来不及跑的窗口——
// 服务刚启动（首轮巡检在 10s 后）、或长时间停机后重启，此时 token 可能已经过期，
// 没有这个兜底就会一直失败到下一轮巡检。
//
// 正常路径只解一次 JWT、不查库，代价可以忽略。
func (s *Server) ensureUsableToken(account Account) Account {
	expiry := JWTExpiry(account.APIKey)
	if expiry == 0 || time.Now().Unix() < expiry {
		return account // 不是 JWT，或还没过期
	}
	if !s.refreshAccountToken(account.ID) {
		return account
	}
	if latest, err := s.store.GetAccount(account.ID); err == nil && latest != nil {
		return *latest
	}
	return account
}

// needsRefresh 判断 access token 是否进入了提前续期窗口。
func (s *Server) needsRefresh(token string) bool {
	expiry := JWTExpiry(token)
	if expiry == 0 {
		return false
	}
	ahead := time.Duration(s.cfg.TokenRefreshAheadDays) * 24 * time.Hour
	if ahead <= 0 {
		ahead = 30 * 24 * time.Hour
	}
	return time.Until(time.Unix(expiry, 0)) <= ahead
}

// refreshAccountToken 对单个账号跑一次续期检查，统一日志口径。
// 返回是否真的换过 token。
func (s *Server) refreshAccountToken(id int64) bool {
	full, err := s.store.GetAccount(id)
	if err != nil || full == nil {
		return false
	}
	refreshed, err := s.ensureFreshToken(*full)
	if err != nil {
		log.Printf("token: #%d 续期失败: %v", id, err)
		return false
	}
	if refreshed {
		log.Printf("token: #%d access token 已自动续期", id)
	}
	return refreshed
}

// RefreshTokens 扫一遍所有账号做续期检查，返回换过 token 的数量。
func (s *Server) RefreshTokens(ctx context.Context) int {
	accounts, err := s.store.ListAccounts()
	if err != nil {
		log.Printf("token: 列账号失败: %v", err)
		return 0
	}
	refreshed := 0
	for _, acc := range accounts {
		if ctx.Err() != nil {
			break
		}
		if s.refreshAccountToken(acc.ID) {
			refreshed++
		}
	}
	return refreshed
}

// StartTokenRefreshLoop 独立于余额轮询的 token 续期巡检。
//
// 续期是账号存活的底线——access token 过期后服务会静默死掉——所以它不能挂在
// 余额轮询上：CREDITS_REFRESH_MIN=0 关掉的应该只是余额查询。intl 实例跑的是
// 免费模型（余额恒定不动），关余额轮询是完全合理的操作，绝不该顺手把续期也关了。
func (s *Server) StartTokenRefreshLoop(ctx context.Context) {
	// 不按实例模式开关：合并面板后，一个实例里可能同时有 ck_ 号和凭证号，
	// 到底要不要续期由 ensureFreshToken 逐账号判断。
	hours := s.cfg.TokenRefreshCheckHours
	if hours <= 0 {
		hours = 6
	}
	interval := time.Duration(hours) * time.Hour
	log.Printf("token: 续期巡检已启动（每 %dh 检查，提前 %d 天续期）",
		hours, s.cfg.TokenRefreshAheadDays)
	go func() {
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			s.RefreshTokens(ctx)
			timer.Reset(interval)
		}
	}()
}
