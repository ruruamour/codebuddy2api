package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 账号自检：代理通不通、凭证还活着没有、生效配置到底是什么。
//
// 代理本来就支持（http/https/socks5，见 upstream.httpClient），但以前配完没有任何
// 反馈——填错了只能等真实请求失败才知道。这里给面板一个能直接点的自检。

// DiagnoseResult 是一次账号自检的结果。
type DiagnoseResult struct {
	AccountID int64  `json:"account_id"`
	OK        bool   `json:"ok"`
	Detail    string `json:"detail"`

	// 生效配置（账号级覆盖 + 实例级回落之后的最终值）
	AuthMode     string   `json:"auth_mode"`
	UpstreamURL  string   `json:"upstream_url"`
	Domain       string   `json:"domain"`
	RequestShape string   `json:"request_shape"`
	Models       []string `json:"models"`

	Proxy      *ProxyCheck `json:"proxy"`
	Credential *CredCheck  `json:"credential"`
	Upstream   *ReachCheck `json:"upstream"`
}

type ProxyCheck struct {
	Configured bool   `json:"configured"`
	URL        string `json:"url"`
	OK         bool   `json:"ok"`
	ExitIP     string `json:"exit_ip"`
	LatencyMS  int64  `json:"latency_ms"`
	Detail     string `json:"detail"`
}

type CredCheck struct {
	Kind            string `json:"kind"`
	HasRefreshToken bool   `json:"has_refresh_token"`
	ExpiresAt       int64  `json:"expires_at"`
	DaysLeft        int64  `json:"days_left"`
	Detail          string `json:"detail"`
}

type ReachCheck struct {
	OK        bool   `json:"ok"`
	Status    int    `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	Detail    string `json:"detail"`
}

// Diagnose 跑一遍账号自检。不消耗积分：只打计费接口和一个探活请求。
func (s *Server) Diagnose(ctx context.Context, account Account) DiagnoseResult {
	account = s.ensureUsableToken(account)
	profile := s.upstream.ProfileFor(account)
	out := DiagnoseResult{
		AccountID:    account.ID,
		AuthMode:     profile.AuthMode,
		UpstreamURL:  profile.UpstreamURL,
		Domain:       profile.Domain,
		RequestShape: profile.RequestShape,
		Models:       profile.Models,
	}

	out.Proxy = s.checkProxy(ctx, account)
	out.Credential = s.checkCredential(account, profile)
	out.Upstream = s.checkUpstream(ctx, account)

	switch {
	case out.Proxy.Configured && !out.Proxy.OK:
		out.Detail = "代理不通：" + out.Proxy.Detail
	case !out.Upstream.OK:
		out.Detail = "上游不通：" + out.Upstream.Detail
	default:
		out.OK = true
		out.Detail = "正常"
	}
	return out
}

// checkProxy 走账号自己的 http client 打一个回显服务，验证代理真的生效并报出口 IP。
func (s *Server) checkProxy(ctx context.Context, account Account) *ProxyCheck {
	check := &ProxyCheck{URL: account.ProxyString()}
	check.Configured = strings.TrimSpace(check.URL) != ""

	client, err := s.upstream.httpClient(account)
	if err != nil {
		check.Detail = err.Error()
		return check
	}
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "https://api.ipify.org?format=text", nil)
	if err != nil {
		check.Detail = err.Error()
		return check
	}
	started := time.Now()
	resp, err := client.Do(req)
	check.LatencyMS = time.Since(started).Milliseconds()
	if err != nil {
		check.Detail = err.Error()
		return check
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 128))
	check.ExitIP = strings.TrimSpace(string(raw))
	check.OK = resp.StatusCode == http.StatusOK && check.ExitIP != ""
	if !check.OK {
		check.Detail = fmt.Sprintf("http %d", resp.StatusCode)
	} else if !check.Configured {
		check.Detail = "未配置代理，直连出口"
	} else {
		check.Detail = "代理生效"
	}
	return check
}

// checkCredential 只看本地信息：凭证类型、有没有续期种子、还剩多少天。
func (s *Server) checkCredential(account Account, profile AccountProfile) *CredCheck {
	check := &CredCheck{Kind: "API key"}
	if !profile.IsBearer() {
		check.Detail = "ck_ 订阅 key，不涉及续期"
		return check
	}
	check.Kind = "OAuth 凭证"
	refreshToken, _ := s.store.RefreshTokenFor(account.ID)
	check.HasRefreshToken = strings.TrimSpace(refreshToken) != ""
	check.ExpiresAt = JWTExpiry(account.APIKey)
	if check.ExpiresAt > 0 {
		check.DaysLeft = int64(time.Until(time.Unix(check.ExpiresAt, 0)).Hours() / 24)
	}
	switch {
	case !check.HasRefreshToken:
		check.Detail = fmt.Sprintf("没存 refresh token，%d 天后过期就会静默失效", check.DaysLeft)
	case check.DaysLeft <= 0:
		check.Detail = "access token 已过期，等待下一轮续期巡检"
	default:
		check.Detail = fmt.Sprintf("可自助续期，剩余 %d 天", check.DaysLeft)
	}
	return check
}

// checkUpstream 用账号自己的配置探活。
func (s *Server) checkUpstream(ctx context.Context, account Account) *ReachCheck {
	check := &ReachCheck{}
	reqCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	started := time.Now()
	_, _, err := s.upstream.Probe(reqCtx, account)
	check.LatencyMS = time.Since(started).Milliseconds()
	if err != nil {
		var statusErr UpstreamStatusError
		if errors.As(err, &statusErr) {
			check.Status = statusErr.StatusCode
		}
		check.Detail = truncate(err.Error(), 300)
		return check
	}
	check.OK = true
	check.Status = http.StatusOK
	check.Detail = "探活成功"
	return check
}
