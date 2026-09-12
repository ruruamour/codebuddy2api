package app

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed web/admin.html
var adminHTML embed.FS

type Server struct {
	cfg      Config
	store    *Store
	pool     *Pool
	upstream *UpstreamClient
	types    *TypeRegistry
}

func NewServer(cfg Config, store *Store) *Server {
	types := NewTypeRegistry()
	// 类型表可能还没建好（老库首启、或测试里直接 NewServer）；灌不进去就先空着，
	// 空注册表等于「所有账号都未归类」，正好是加类型之前的行为。
	_ = store.LoadTypeRegistry(types)
	return &Server{
		cfg:      cfg,
		store:    store,
		types:    types,
		pool:     NewPool(store, types, cfg.Models, cfg.PoolStrategy),
		upstream: NewUpstreamClient(cfg, types),
	}
}

// reloadTypes 在类型表被改动后刷新进程内缓存。
// 所有写类型的入口都必须调它，否则面板上改完、转发那边还在用旧配置。
func (s *Server) reloadTypes() {
	if err := s.store.LoadTypeRegistry(s.types); err != nil {
		log.Printf("types: 刷新缓存失败: %v", err)
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/admin", s.handleAdminUI)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/v1/models", s.withClientAuth(s.handleModels))
	mux.HandleFunc("/v1/chat/completions", s.withClientAuth(s.handleChatCompletions))
	mux.HandleFunc("/admin/accounts", s.withAdminAuth(s.handleAdminAccounts))
	mux.HandleFunc("/admin/accounts/", s.withAdminAuth(s.handleAdminAccountByID))
	mux.HandleFunc("/admin/accounts/export", s.withAdminAuth(s.handleAccountsExport))
	mux.HandleFunc("/admin/accounts/import", s.withAdminAuth(s.handleAccountsImport))
	mux.HandleFunc("/admin/stats", s.withAdminAuth(s.handleAdminStats))
	mux.HandleFunc("/admin/credits/refresh", s.withAdminAuth(s.handleCreditsRefresh))
	mux.HandleFunc("/admin/settings", s.withAdminAuth(s.handleAdminSettings))
	mux.HandleFunc("/admin/types", s.withAdminAuth(s.handleAdminTypes))
	mux.HandleFunc("/admin/types/", s.withAdminAuth(s.handleAdminTypeBySlug))
	mux.HandleFunc("/admin/models", s.withAdminAuth(s.handleAdminModels))
	mux.HandleFunc("/admin/models/", s.withAdminAuth(s.handleAdminModelByID))
	return requestLogger(mux)
}

// StartCreditsLoop 定期从官方计费接口拉真实积分余额。
//
// 触发参数：查询本身不消耗积分，所以轮询很便宜；但接口只认浏览器 UA。
// 行为：余额 <= CreditsMinRemain 的账号会被暂停（status=cooldown，冷却到本周期结束），
// 下一周期重置后自动恢复；余额回到阈值以上则解除暂停。
// CREDITS_REFRESH_MIN=0 关闭轮询（此时不会暂停任何账号）。
func (s *Server) StartCreditsLoop(ctx context.Context) {
	interval := time.Duration(s.cfg.CreditsRefreshMinutes) * time.Minute
	if s.cfg.CreditsRefreshMinutes <= 0 {
		log.Printf("credits: 余额轮询已关闭（CODEBUDDY2API_CREDITS_REFRESH_MIN=0）")
		return
	}
	go func() {
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			s.RefreshCredits(ctx)
			timer.Reset(interval)
		}
	}()
}

// RefreshCredits 同步刷新所有账号余额，返回本次结果的摘要。
func (s *Server) RefreshCredits(ctx context.Context) map[string]any {
	accounts, err := s.store.ListAccounts()
	if err != nil {
		return map[string]any{"ok": false, "detail": err.Error()}
	}
	okCount, failCount, pausedCount, resumedCount := 0, 0, 0, 0
	var totalRemain float64
	for _, acc := range accounts {
		if ctx.Err() != nil {
			break
		}
		full, err := s.store.GetAccount(acc.ID)
		if err != nil || full == nil {
			continue
		}
		// Bearer 模式：查余额前顺手确保 token 没快过期（真正的巡检在
		// StartTokenRefreshLoop 里，这里只是避免拿一个将死的 token 去查余额）
		if s.refreshAccountToken(acc.ID) {
			if again, err := s.store.GetAccount(acc.ID); err == nil && again != nil {
				full = again
			}
		}
		info, err := s.upstream.FetchCredits(*full)
		if err != nil {
			failCount++
			// 查询失败不改状态：避免把能用的号误停。
			_ = s.store.SaveCredits(acc.ID, 0, 0, nil, truncate(err.Error(), 200))
			log.Printf("credits: #%d 查询失败: %v", acc.ID, err)
			continue
		}
		okCount++
		totalRemain += info.ActiveRemain
		// 面板展示用「生效中的包」口径：分母不含已用尽的包，重置时间也只取生效包的
		if err := s.store.SaveCredits(acc.ID, info.ActiveRemain, info.ActiveTotal, info.CycleEnd,
			"", creditPackagesJSON(info.Packages)); err != nil {
			log.Printf("credits: #%d 保存失败: %v", acc.ID, err)
		}
		// 暂停判断用全部包剩余：只要还有任何一个包有钱就不要停
		if info.Remain <= s.cfg.CreditsMinRemain {
			if err := s.store.PauseForCredits(acc.ID, info.CycleEnd, "credits_exhausted"); err == nil {
				pausedCount++
				log.Printf("credits: #%d 余额 %.2f <= %.2f，已暂停（冷却到本周期结束）",
					acc.ID, info.Remain, s.cfg.CreditsMinRemain)
			}
		} else if acc.CreditsPaused {
			if err := s.store.ResumeCreditsPause(acc.ID); err == nil {
				resumedCount++
				log.Printf("credits: #%d 余额恢复 %.2f，已解除暂停", acc.ID, info.Remain)
			}
		}
	}
	return map[string]any{
		"ok":            true,
		"checked":       okCount,
		"failed":        failCount,
		"paused":        pausedCount,
		"resumed":       resumedCount,
		"total_remain":  totalRemain,
		"min_remain":    s.cfg.CreditsMinRemain,
		"refresh_min":   s.cfg.CreditsRefreshMinutes,
		"checked_at":    now(),
		"accounts_seen": len(accounts),
	}
}

func (s *Server) handleCreditsRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"detail": "method not allowed"})
		return
	}
	summary := s.RefreshCredits(r.Context())
	accounts, err := s.store.ListAccounts()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       summary["ok"],
		"summary":  summary,
		"accounts": attachInFlight(accounts, s.pool.Snapshot()),
	})
}

func attachInFlight(accounts []PublicAccount, inFlight map[int64]int) []PublicAccount {
	for i := range accounts {
		accounts[i].InFlight = inFlight[accounts[i].ID]
	}
	return accounts
}

// creditPackagesJSON 把包明细序列化成紧凑列表，供面板 tooltip 展示。
func creditPackagesJSON(packages []CreditPackage) string {
	parts := make([]string, 0, len(packages))
	for _, p := range packages {
		label := fmt.Sprintf("%s %.2f/%.0f [%s", p.Name, p.Remain, p.Size, creditStatusText(p.Status))
		if p.CycleEnd != nil {
			label += "，至" + time.Unix(*p.CycleEnd, 0).Format("2006-01-02")
		}
		parts = append(parts, label+"]")
	}
	return strings.Join(parts, "；")
}

func creditStatusText(status int) string {
	switch status {
	case 0:
		return "生效"
	case 3:
		return "已用尽"
	default:
		return fmt.Sprintf("Status=%d", status)
	}
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeJSON(w, http.StatusNotFound, map[string]any{"detail": "not found"})
		return
	}
	http.Redirect(w, r, "/admin", http.StatusTemporaryRedirect)
}

func (s *Server) handleAdminUI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"detail": "method not allowed"})
		return
	}
	data, err := adminHTML.ReadFile("web/admin.html")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 面板随二进制走，版本更新就必须立刻生效。
	// 以前不发缓存头，浏览器按启发式缓存 HTML，部署后仍显示旧面板——
	// 表现就是"新加的东西怎么没有"，很难往缓存上想。
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	_, _ = w.Write(data)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	stats, err := s.store.Stats()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"status": "error", "detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "ok",
		"service":          "codebuddy2api",
		"version":          Version,
		"accounts":         stats.Accounts,
		"enabled_accounts": stats.EnabledAccounts,
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"detail": "method not allowed"})
		return
	}
	settings, err := s.store.ModelSettings(s.cfg.Models, s.cfg.PoolStrategy)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	data := make([]map[string]any, 0, len(settings.Models))
	for _, model := range settings.Models {
		data = append(data, map[string]any{
			"id":       model,
			"object":   "model",
			"created":  0,
			"owned_by": "codebuddy",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"detail": "method not allowed"})
		return
	}
	body, err := readJSONMap(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "invalid request body: " + err.Error()})
		return
	}
	if _, ok := body["messages"].([]any); !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "invalid request body: messages is required"})
		return
	}
	if ok := s.applyModelSettings(w, body); !ok {
		return
	}
	if s.cfg.DebugRequests {
		log.Printf("chat.request model=%v stream=%v keys=%v", body["model"], body["stream"], mapKeys(body))
	}
	requestedModel := stringValue(body["model"], "")
	lease, err := s.pool.Acquire(requestedModel)
	if err != nil {
		status := http.StatusServiceUnavailable
		errorType := "no_account_available"
		switch {
		case errors.Is(err, ErrAllAccountsBusy):
			errorType = "account_concurrency_exhausted"
		case errors.Is(err, ErrNoAccountForModel):
			errorType = "no_account_for_model"
			err = fmt.Errorf("没有账号声明支持模型 %q", requestedModel)
		}
		writeOpenAIError(w, status, err.Error(), errorType)
		return
	}
	// 兜底：token 已过期就先续再用（正常由后台巡检提前换掉，这里只挡巡检空窗）
	lease.Account = s.ensureUsableToken(lease.Account)
	stream := boolValue(body["stream"])
	if stream {
		s.streamResponse(w, r.Context(), lease, body)
		return
	}
	defer s.pool.Release(lease)
	response, state, err := s.upstream.CompleteChat(r.Context(), lease.Account, body)
	if err != nil {
		s.recordFailure(lease.Account.ID, err)
		writeUpstreamError(w, err)
		return
	}
	if state != nil {
		s.store.RecordSuccess(lease.Account.ID, state.Usage)
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) streamResponse(w http.ResponseWriter, ctx context.Context, lease Lease, body map[string]any) {
	defer s.pool.Release(lease)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)
	var finalUsage map[string]any
	state, err := s.upstream.StreamChat(ctx, lease.Account, body, func(wire []byte, current *StreamState) error {
		if current != nil && current.Usage != nil {
			finalUsage = current.Usage
		}
		if _, err := w.Write(wire); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})
	if err != nil {
		s.recordFailure(lease.Account.ID, err)
		errorType := "proxy_error"
		message := err.Error()
		if upstreamErr, ok := err.(UpstreamStatusError); ok {
			errorType = "upstream_error"
			message = upstreamErr.Body
		}
		payload, _ := json.Marshal(map[string]any{"error": map[string]any{"message": truncate(message, 500), "type": errorType}})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	if state != nil && state.Usage != nil {
		finalUsage = state.Usage
	}
	s.store.RecordSuccess(lease.Account.ID, finalUsage)
}

func (s *Server) handleAdminAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		accounts, err := s.store.ListAccounts()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
			return
		}
		inFlight := s.pool.Snapshot()
		writeJSON(w, http.StatusOK, map[string]any{
			"accounts": s.attachEffectiveModels(attachInFlight(accounts, inFlight)),
		})
	case http.MethodPost:
		var payload AccountCreate
		if err := decodeJSON(r, &payload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
			return
		}
		id, err := s.store.AddAccount(payload)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"detail": "method not allowed"})
	}
}

func (s *Server) handleAdminAccountByID(w http.ResponseWriter, r *http.Request) {
	id, action, ok := parseAccountPath(strings.TrimPrefix(r.URL.Path, "/admin/accounts/"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"detail": "account not found"})
		return
	}
	switch {
	case r.Method == http.MethodPatch && action == "":
		var payload AccountPatch
		if err := decodeJSON(r, &payload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
			return
		}
		ok, err := s.store.PatchAccount(id, payload)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
			return
		}
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"detail": "account not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case r.Method == http.MethodDelete && action == "":
		ok, err := s.store.DeleteAccount(id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
			return
		}
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"detail": "account not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case r.Method == http.MethodPost && action == "enable":
		s.setAccountEnabled(w, id, true)
	case r.Method == http.MethodPost && action == "disable":
		s.setAccountEnabled(w, id, false)
	case r.Method == http.MethodPost && action == "probe":
		s.probeAccount(w, r.Context(), id)
	case r.Method == http.MethodPost && action == "diagnose":
		account, err := s.store.GetAccount(id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
			return
		}
		if account == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"detail": "account not found"})
			return
		}
		writeJSON(w, http.StatusOK, s.Diagnose(r.Context(), *account))
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"detail": "method not allowed"})
	}
}

func (s *Server) setAccountEnabled(w http.ResponseWriter, id int64, enabled bool) {
	ok, err := s.store.SetEnabled(id, enabled)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"detail": "account not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) probeAccount(w http.ResponseWriter, ctx context.Context, id int64) {
	account, err := s.store.GetAccount(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	if account == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"detail": "account not found"})
		return
	}
	fresh := s.ensureUsableToken(*account)
	response, usage, err := s.upstream.Probe(ctx, fresh)
	if err != nil {
		s.recordFailure(id, err)
		status := http.StatusBadGateway
		if upstreamErr, ok := err.(UpstreamStatusError); ok {
			status = upstreamErr.StatusCode
			writeJSON(w, status, map[string]any{"detail": truncate(upstreamErr.Body, 500)})
			return
		}
		writeJSON(w, status, map[string]any{"detail": err.Error()})
		return
	}
	s.store.RecordSuccess(id, usage)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "usage": nonNilMap(usage), "response": response})
}

func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"detail": "method not allowed"})
		return
	}
	stats, err := s.store.Stats()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		settings, err := s.store.ModelSettings(s.cfg.Models, s.cfg.PoolStrategy)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, settings)
	case http.MethodPatch:
		var payload ModelSettings
		if err := decodeJSON(r, &payload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
			return
		}
		settings, err := s.store.SaveModelSettings(payload, s.cfg.Models, s.cfg.PoolStrategy)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, settings)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"detail": "method not allowed"})
	}
}

func (s *Server) applyModelSettings(w http.ResponseWriter, body map[string]any) bool {
	settings, err := s.store.ModelSettings(s.cfg.Models, s.cfg.PoolStrategy)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, err.Error(), "settings_error")
		return false
	}
	model := strings.TrimSpace(stringValue(body["model"], ""))
	if model == "" {
		body["model"] = settings.DefaultModel
		return true
	}
	if !containsString(settings.Models, model) {
		writeOpenAIError(w, http.StatusBadRequest, "model is not enabled: "+model, "invalid_model")
		return false
	}
	body["model"] = model
	return true
}

func (s *Server) withClientAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.APIKey == "" || bearerToken(r) == s.cfg.APIKey {
			next(w, r)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"detail": "invalid API key"})
	}
}

func (s *Server) withAdminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.trustedCloudflareAccessAdmin(r) {
			next(w, r)
			return
		}
		tokens := s.cfg.AdminTokens()
		if len(tokens) == 0 {
			next(w, r)
			return
		}
		got := bearerToken(r)
		for _, token := range tokens {
			if got == token {
				next(w, r)
				return
			}
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"detail": "invalid admin API key"})
	}
}

func (s *Server) trustedCloudflareAccessAdmin(r *http.Request) bool {
	if !s.cfg.AdminTrustCFAccess {
		return false
	}
	email := strings.ToLower(strings.TrimSpace(r.Header.Get("Cf-Access-Authenticated-User-Email")))
	assertion := strings.TrimSpace(r.Header.Get("Cf-Access-Jwt-Assertion"))
	if email == "" && assertion == "" {
		return false
	}
	if len(s.cfg.AdminAccessEmails) == 0 {
		return true
	}
	_, ok := s.cfg.AdminAccessEmails[email]
	return ok
}

func (s *Server) recordFailure(id int64, err error) {
	statusCode := 0
	message := err.Error()
	if upstreamErr, ok := err.(UpstreamStatusError); ok {
		statusCode = upstreamErr.StatusCode
		message = upstreamErr.Body
	}
	autoDisable := false
	reason := ""
	if _, ok := s.cfg.AutoDisableStatusCodes[statusCode]; ok {
		autoDisable = true
		reason = fmt.Sprintf("status_%d", statusCode)
	}
	if s.cfg.AutoDisableQuotaErrors && isQuotaExhaustedError(statusCode, message) {
		autoDisable = true
		reason = "quota_or_credits_exhausted"
	}
	// 冷却时长优先用上游给的重置时间（限流可能要等几十分钟到几小时，
	// 固定值会导致账号在限流期间被反复拉起来撞墙）。
	cooldownSeconds := CooldownSecondsForError(message, s.cfg.CooldownSeconds, now())
	s.store.RecordFailure(id, message, statusCode, cooldownSeconds, s.cfg.FailureThreshold, autoDisable, reason)
}

func writeUpstreamError(w http.ResponseWriter, err error) {
	if upstreamErr, ok := err.(UpstreamStatusError); ok {
		writeOpenAIError(w, upstreamErr.StatusCode, upstreamErr.Body, "upstream_error")
		return
	}
	writeOpenAIError(w, http.StatusBadGateway, err.Error(), "proxy_error")
}

func writeOpenAIError(w http.ResponseWriter, status int, message string, errorType string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": truncate(message, 1000), "type": errorType}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func readJSONMap(r *http.Request) (map[string]any, error) {
	var value map[string]any
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if value == nil {
		value = map[string]any{}
	}
	return value, nil
}

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	return decoder.Decode(target)
}

func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
}

func parseAccountPath(path string) (int64, string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return 0, "", false
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, "", false
	}
	if len(parts) == 1 {
		return id, "", true
	}
	if len(parts) == 2 {
		return id, parts[1], true
	}
	return 0, "", false
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}

func boolValue(value any) bool {
	switch item := value.(type) {
	case bool:
		return item
	case string:
		return strings.EqualFold(item, "true")
	default:
		return false
	}
}

func nonNilMap(value map[string]any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	return value
}

func mapKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	return keys
}

func isQuotaExhaustedError(statusCode int, message string) bool {
	text := strings.ToLower(message)
	if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden {
		return false
	}
	needles := []string{
		"usage_limit",
		"usage limit",
		"insufficient_quota",
		"quota exceeded",
		"quota_exceeded",
		"credit exhausted",
		"credits exhausted",
		"balance insufficient",
		"insufficient balance",
		"no credits",
		"额度不足",
		"额度已用完",
		"余额不足",
	}
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

// handleAccountsExport 导出全部账号（含完整凭证）。
//
// 明文导出：面板在 Cloudflare Access 后面，且导出文件必须能原样导回，
// 脱敏过的凭证导回去就是一堆废号。
func (s *Server) handleAccountsExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"detail": "method not allowed"})
		return
	}
	bundle, err := s.store.ExportAccounts()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition", fmt.Sprintf(
			`attachment; filename="codebuddy2api-accounts-%s.json"`,
			time.Now().Format("20060102-150405")))
	}
	writeJSON(w, http.StatusOK, bundle)
}

// handleAccountsImport 导入账号：认导出文件、token.json、按行文本三种来源。
func (s *Server) handleAccountsImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"detail": "method not allowed"})
		return
	}
	var payload ImportRequest
	if err := decodeJSON(r, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	result, err := s.store.ImportAccounts(payload)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}
