package app

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Account struct {
	ID                  int64          `json:"id"`
	Name                string         `json:"name"`
	APIKey              string         `json:"-"`
	Enabled             bool           `json:"enabled"`
	Status              string         `json:"status"`
	Priority            int            `json:"priority"`
	Weight              int            `json:"weight"`
	Concurrency         int            `json:"concurrency"`
	ProxyURL            sql.NullString `json:"-"`
	HeaderProfile       map[string]any `json:"header_profile"`
	Notes               sql.NullString `json:"-"`
	ConsecutiveFailures int            `json:"consecutive_failures"`
	CooldownUntil       sql.NullInt64  `json:"-"`
	// 账号级上游覆盖：空表示回落到实例级配置（老账号全空，行为不变）。
	// 有了这几列，一个实例里才能同时放 ck_ 号和 OAuth 凭证号。
	AuthMode     sql.NullString `json:"-"`
	UpstreamURL  sql.NullString `json:"-"`
	Domain       sql.NullString `json:"-"`
	RequestShape sql.NullString `json:"-"`
	Models       sql.NullString `json:"-"`
	// TypeSlug 是账号所属的类型（渠道）。常规配置走类型，上面那几个覆盖字段
	// 只在个别号要偏离本类型时才填。
	TypeSlug sql.NullString `json:"-"`
}

type PublicAccount struct {
	ID                  int64          `json:"id"`
	Name                string         `json:"name"`
	APIKeyPreview       string         `json:"api_key_preview"`
	Enabled             bool           `json:"enabled"`
	Status              string         `json:"status"`
	Priority            int            `json:"priority"`
	Weight              int            `json:"weight"`
	Concurrency         int            `json:"concurrency"`
	ProxyURL            *string        `json:"proxy_url"`
	HeaderProfile       map[string]any `json:"header_profile"`
	Notes               *string        `json:"notes"`
	ConsecutiveFailures int            `json:"consecutive_failures"`
	CooldownUntil       *int64         `json:"cooldown_until"`
	TotalRequests       int64          `json:"total_requests"`
	TotalSuccess        int64          `json:"total_success"`
	TotalFailures       int64          `json:"total_failures"`
	TotalCredit         float64        `json:"total_credit"`
	PromptTokens        int64          `json:"prompt_tokens"`
	CompletionTokens    int64          `json:"completion_tokens"`
	TotalTokens         int64          `json:"total_tokens"`
	LastSuccessAt       *int64         `json:"last_success_at"`
	LastFailureAt       *int64         `json:"last_failure_at"`
	LastError           *string        `json:"last_error"`
	LastErrorStatus     *int           `json:"last_error_status"`
	QuotaLimit          float64        `json:"quota_limit"`
	QuotaAutoDisable    bool           `json:"quota_auto_disable"`
	QuotaRemaining      *float64       `json:"quota_remaining"`
	QuotaExceeded       bool           `json:"quota_exceeded"`
	ExpiresAt           *int64         `json:"expires_at"`
	ExpireAutoDisable   bool           `json:"expire_auto_disable"`
	LastDisableReason   *string        `json:"last_disable_reason"`
	LastDisableAt       *int64         `json:"last_disable_at"`
	CreditsRemaining    *float64       `json:"credits_remaining"`
	CreditsTotal        *float64       `json:"credits_total"`
	CreditsCycleEnd     *int64         `json:"credits_cycle_end"`
	CreditsCheckedAt    *int64         `json:"credits_checked_at"`
	CreditsPackages     *string        `json:"credits_packages"`
	CreditsError        *string        `json:"credits_error"`
	CreditsPaused       bool           `json:"credits_paused"`
	// 账号级上游配置（面板要显示/编辑）
	AuthMode     string   `json:"auth_mode"`
	UpstreamURL  *string  `json:"upstream_url"`
	Domain       *string  `json:"domain"`
	RequestShape string   `json:"request_shape"`
	Models       []string `json:"models"`
	TypeSlug     string   `json:"type_slug"`
	// EffectiveModels 是这个号实际能接的模型（账号没声明就是类型的），面板直接显示，
	// 省得操作者自己在脑子里跑一遍回落规则
	EffectiveModels []string `json:"effective_models"`
	// 凭证只报状态，明文永不出库
	HasRefreshToken bool   `json:"has_refresh_token"`
	TokenExpiresAt  *int64 `json:"token_expires_at"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
	InFlight        int    `json:"in_flight,omitempty"`
}

type AccountCreate struct {
	Name              string         `json:"name"`
	APIKey            string         `json:"api_key"`
	Enabled           *bool          `json:"enabled"`
	Priority          *int           `json:"priority"`
	Weight            *int           `json:"weight"`
	Concurrency       *int           `json:"concurrency"`
	ProxyURL          *string        `json:"proxy_url"`
	HeaderProfile     map[string]any `json:"header_profile"`
	Notes             *string        `json:"notes"`
	QuotaLimit        *float64       `json:"quota_limit"`
	QuotaAutoDisable  *bool          `json:"quota_auto_disable"`
	ExpiresAt         *int64         `json:"expires_at"`
	ExpireAutoDisable *bool          `json:"expire_auto_disable"`
	// RefreshToken/TokenExpiresAt 是 Bearer 模式下自助续期的种子。
	// 建号时不传的话，账号活到 access token 过期那天就会因为
	// 「剩余寿命不足但没存 refresh token」被 ensureFreshToken 拒绝续期。
	RefreshToken   *string `json:"refresh_token"`
	TokenExpiresAt *int64  `json:"token_expires_at"`
	// 账号级上游覆盖，留空回落到实例级配置。
	AuthMode     *string  `json:"auth_mode"`
	UpstreamURL  *string  `json:"upstream_url"`
	Domain       *string  `json:"domain"`
	RequestShape *string  `json:"request_shape"`
	Models       []string `json:"models"`
	TypeSlug     *string  `json:"type_slug"`
}

type AccountPatch struct {
	Name              *string        `json:"name"`
	APIKey            *string        `json:"api_key"`
	Enabled           *bool          `json:"enabled"`
	Priority          *int           `json:"priority"`
	Weight            *int           `json:"weight"`
	Concurrency       *int           `json:"concurrency"`
	ProxyURL          *string        `json:"proxy_url"`
	HeaderProfile     map[string]any `json:"header_profile"`
	Notes             *string        `json:"notes"`
	ResetFailures     bool           `json:"reset_failures"`
	QuotaLimit        *float64       `json:"quota_limit"`
	QuotaAutoDisable  *bool          `json:"quota_auto_disable"`
	ExpiresAt         *int64         `json:"expires_at"`
	ExpireAutoDisable *bool          `json:"expire_auto_disable"`
	ResetUsage        bool           `json:"reset_usage"`
	// 账号级上游覆盖；传空字符串表示清掉覆盖、回落到实例级。
	AuthMode     *string  `json:"auth_mode"`
	UpstreamURL  *string  `json:"upstream_url"`
	Domain       *string  `json:"domain"`
	RequestShape *string  `json:"request_shape"`
	Models       []string `json:"models"`
	TypeSlug     *string  `json:"type_slug"`
	// RefreshToken 允许给已有账号补种子（老账号建号时没有这一列）。
	RefreshToken *string `json:"refresh_token"`
	// TokenExpiresAt 用于非 JWT 凭证手工维护到期时间；不传则从新 api_key 的 JWT 推导。
	TokenExpiresAt *int64 `json:"token_expires_at"`
}

type Stats struct {
	Accounts         int64   `json:"accounts"`
	EnabledAccounts  int64   `json:"enabled_accounts"`
	TotalRequests    int64   `json:"total_requests"`
	TotalSuccess     int64   `json:"total_success"`
	TotalFailures    int64   `json:"total_failures"`
	TotalCredit      float64 `json:"total_credit"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
}

type Store struct {
	db *sql.DB
}

func NewStore(dbPath string) (*Store, error) {
	path := expandHome(dbPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) init() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS accounts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  api_key TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  status TEXT NOT NULL DEFAULT 'active',
  priority INTEGER NOT NULL DEFAULT 100,
  weight INTEGER NOT NULL DEFAULT 1,
  concurrency INTEGER NOT NULL DEFAULT 1,
  proxy_url TEXT,
  header_profile TEXT NOT NULL DEFAULT '{}',
  notes TEXT,
  consecutive_failures INTEGER NOT NULL DEFAULT 0,
  cooldown_until INTEGER,
  total_requests INTEGER NOT NULL DEFAULT 0,
  total_success INTEGER NOT NULL DEFAULT 0,
  total_failures INTEGER NOT NULL DEFAULT 0,
  total_credit REAL NOT NULL DEFAULT 0,
  prompt_tokens INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  last_success_at INTEGER,
  last_failure_at INTEGER,
  last_error TEXT,
  last_error_status INTEGER,
  quota_limit REAL NOT NULL DEFAULT 0,
  quota_auto_disable INTEGER NOT NULL DEFAULT 0,
  expires_at INTEGER,
  expire_auto_disable INTEGER NOT NULL DEFAULT 0,
  last_disable_reason TEXT,
  last_disable_at INTEGER,
  credits_remaining REAL,
  credits_total REAL,
  credits_cycle_end INTEGER,
  credits_checked_at INTEGER,
  credits_error TEXT,
  credits_packages TEXT,
  credits_paused INTEGER NOT NULL DEFAULT 0,
  refresh_token TEXT,
  token_expires_at INTEGER,
  auth_mode TEXT,
  upstream_url TEXT,
  domain TEXT,
  request_shape TEXT,
  models TEXT,
  type_slug TEXT,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_accounts_sched ON accounts(enabled, status, priority, id);
CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
` + accountTypesDDL + catalogDDL)
	if err != nil {
		return err
	}
	for _, migration := range []struct {
		name string
		ddl  string
	}{
		{"last_error_status", "ALTER TABLE accounts ADD COLUMN last_error_status INTEGER"},
		{"quota_limit", "ALTER TABLE accounts ADD COLUMN quota_limit REAL NOT NULL DEFAULT 0"},
		{"quota_auto_disable", "ALTER TABLE accounts ADD COLUMN quota_auto_disable INTEGER NOT NULL DEFAULT 0"},
		{"expires_at", "ALTER TABLE accounts ADD COLUMN expires_at INTEGER"},
		{"expire_auto_disable", "ALTER TABLE accounts ADD COLUMN expire_auto_disable INTEGER NOT NULL DEFAULT 0"},
		{"last_disable_reason", "ALTER TABLE accounts ADD COLUMN last_disable_reason TEXT"},
		{"last_disable_at", "ALTER TABLE accounts ADD COLUMN last_disable_at INTEGER"},
		{"credits_remaining", "ALTER TABLE accounts ADD COLUMN credits_remaining REAL"},
		{"credits_total", "ALTER TABLE accounts ADD COLUMN credits_total REAL"},
		{"credits_cycle_end", "ALTER TABLE accounts ADD COLUMN credits_cycle_end INTEGER"},
		{"credits_checked_at", "ALTER TABLE accounts ADD COLUMN credits_checked_at INTEGER"},
		{"credits_error", "ALTER TABLE accounts ADD COLUMN credits_error TEXT"},
		{"credits_packages", "ALTER TABLE accounts ADD COLUMN credits_packages TEXT"},
		{"credits_paused", "ALTER TABLE accounts ADD COLUMN credits_paused INTEGER NOT NULL DEFAULT 0"},
		{"refresh_token", "ALTER TABLE accounts ADD COLUMN refresh_token TEXT"},
		{"token_expires_at", "ALTER TABLE accounts ADD COLUMN token_expires_at INTEGER"},
		{"auth_mode", "ALTER TABLE accounts ADD COLUMN auth_mode TEXT"},
		{"upstream_url", "ALTER TABLE accounts ADD COLUMN upstream_url TEXT"},
		{"domain", "ALTER TABLE accounts ADD COLUMN domain TEXT"},
		{"request_shape", "ALTER TABLE accounts ADD COLUMN request_shape TEXT"},
		{"models", "ALTER TABLE accounts ADD COLUMN models TEXT"},
		{"type_slug", "ALTER TABLE accounts ADD COLUMN type_slug TEXT"},
	} {
		if err := s.ensureColumn("accounts", migration.name, migration.ddl); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ensureColumn(table string, column string, ddl string) error {
	rows, err := s.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.Exec(ddl)
	return err
}

// readModelSettingsRow 读 settings 里那行 JSON。
//
// 模型表落库之后，这一行只剩 default_model 和 pool_strategy 还在用；models 字段留着
// 是给 Bootstrap 读旧口径用的——迁移那一刻模型表还是空的，启用列表只有这里有。
func (s *Store) readModelSettingsRow() ModelSettings {
	var raw string
	if err := s.db.QueryRow("SELECT value FROM settings WHERE key = ?", "model_settings").Scan(&raw); err != nil {
		return ModelSettings{}
	}
	var settings ModelSettings
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return ModelSettings{}
	}
	return settings
}

func (s *Store) ModelSettings(fallback []string, fallbackPoolStrategy string) (ModelSettings, error) {
	stored := s.readModelSettingsRow()
	catalog, err := s.ListCatalog()
	if err != nil {
		return ModelSettings{}, err
	}
	if len(catalog) == 0 {
		// 模型表还没建立（Bootstrap 之前，或空库首启）：按老口径走。
		// 迁移时读到的就是这一支，新旧对外列表才能对得上。
		if len(stored.Models) == 0 {
			stored.Models = ModelSeed(fallback)
		}
		if stored.PoolStrategy == "" {
			stored.PoolStrategy = fallbackPoolStrategy
		}
		return NormalizeModelSettings(stored, fallback, fallbackPoolStrategy)
	}

	// 模型表就位后，「启用哪些模型」只认表里的 enabled 列。
	// 这里不再回落到 ModelSeed：操作者把模型全停了就该是全停，
	// 悄悄把默认模型塞回来只会让人以为面板没保存成功。
	models := make([]string, 0, len(catalog))
	info := make([]ModelInfo, 0, len(catalog))
	for _, item := range catalog {
		info = append(info, ModelInfo{
			ID:              item.ID,
			Name:            item.Name,
			Credits:         item.Credits,
			MaxInputTokens:  item.MaxInputTokens,
			MaxOutputTokens: item.MaxOutputTokens,
			SupportsImages:  item.SupportsImages,
		})
		if item.Enabled {
			models = append(models, item.ID)
		}
	}
	defaultModel := strings.TrimSpace(stored.DefaultModel)
	if defaultModel == "" || !containsString(models, defaultModel) {
		defaultModel = ""
		if len(models) > 0 {
			defaultModel = models[0]
		}
	}
	return ModelSettings{
		Models:         models,
		DefaultModel:   defaultModel,
		PoolStrategy:   NormalizePoolStrategy(stored.PoolStrategy, fallbackPoolStrategy),
		ModelCatalog:   info,
		PoolStrategies: CodeBuddyPoolStrategies,
	}, nil
}

func (s *Store) SaveModelSettings(payload ModelSettings, fallback []string, fallbackPoolStrategy string) (ModelSettings, error) {
	catalog, err := s.ListCatalog()
	if err != nil {
		return ModelSettings{}, err
	}
	if len(catalog) > 0 {
		// 勾选状态写回模型表；面板传来的 models 就是「勾了哪些」
		known := make(map[string]struct{}, len(catalog))
		for _, item := range catalog {
			known[item.ID] = struct{}{}
		}
		enabled := make([]string, 0, len(payload.Models))
		for _, id := range normalizeModelIDs(payload.Models) {
			if _, ok := known[id]; !ok {
				return ModelSettings{}, &ValidationError{Message: "模型表里没有这个模型，请先在模型表新增：" + id}
			}
			enabled = append(enabled, id)
		}
		if err := s.SetCatalogEnabled(enabled); err != nil {
			return ModelSettings{}, err
		}
	}
	raw, err := json.Marshal(ModelSettings{
		Models:       normalizeModelIDs(payload.Models),
		DefaultModel: strings.TrimSpace(payload.DefaultModel),
		PoolStrategy: NormalizePoolStrategy(payload.PoolStrategy, fallbackPoolStrategy),
	})
	if err != nil {
		return ModelSettings{}, err
	}
	if _, err := s.db.Exec(`
INSERT INTO settings (key, value, updated_at)
VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		"model_settings", string(raw), now()); err != nil {
		return ModelSettings{}, err
	}
	return s.ModelSettings(fallback, fallbackPoolStrategy)
}

func (s *Store) AddAccount(payload AccountCreate) (int64, error) {
	name := strings.TrimSpace(payload.Name)
	if name == "" {
		name = "CodeBuddy account"
	}
	apiKey := strings.TrimSpace(payload.APIKey)
	if len(apiKey) < 8 {
		return 0, errors.New("api_key must contain at least 8 characters")
	}
	enabled := true
	if payload.Enabled != nil {
		enabled = *payload.Enabled
	}
	priority := intDefault(payload.Priority, 100)
	weight := clamp(intDefault(payload.Weight, 1), 1, 100)
	concurrency := clamp(intDefault(payload.Concurrency, 1), 1, 100)
	quotaLimit := floatDefault(payload.QuotaLimit, 0)
	quotaAutoDisable := boolDefault(payload.QuotaAutoDisable, false)
	expireAutoDisable := boolDefault(payload.ExpireAutoDisable, false)
	status := "active"
	if !enabled {
		status = "disabled"
	}
	profile, err := json.Marshal(defaultMap(payload.HeaderProfile))
	if err != nil {
		return 0, err
	}
	// token_expires_at 不必手填：Bearer 模式下 access token 就是 JWT，exp 自己解得出来。
	tokenExpiresAt := payload.TokenExpiresAt
	if tokenExpiresAt == nil {
		if exp := JWTExpiry(apiKey); exp > 0 {
			tokenExpiresAt = &exp
		}
	}
	// 没显式指定认证方式时按凭证形态判断：JWT 是 OAuth 凭证，ck_ 是 API key。
	// 这样同一个面板里粘哪种凭证都能直接用，不用先去想实例是什么模式。
	if payload.AuthMode == nil || strings.TrimSpace(*payload.AuthMode) == "" {
		if detected := DetectAuthMode(apiKey); detected != "" {
			payload.AuthMode = &detected
		}
	}
	// 同理，没指定类型就按凭证形态和上游归一个。归错比不归更糟，所以 DetectTypeSlug
	// 认不出来时返回空串，账号保持未归类、继续走实例级配置。
	if payload.TypeSlug == nil || strings.TrimSpace(*payload.TypeSlug) == "" {
		domain, upstream := "", ""
		if payload.Domain != nil {
			domain = *payload.Domain
		}
		if payload.UpstreamURL != nil {
			upstream = *payload.UpstreamURL
		}
		if slug := DetectTypeSlug(apiKey, domain, upstream); slug != "" {
			if exists, err := s.TypeSlugExists(slug); err == nil && exists {
				payload.TypeSlug = &slug
			}
		}
	}
	ts := now()
	result, err := s.db.Exec(`
INSERT INTO accounts (
  name, api_key, enabled, status, priority, weight, concurrency,
  proxy_url, header_profile, notes, quota_limit, quota_auto_disable,
  expires_at, expire_auto_disable, refresh_token, token_expires_at,
  auth_mode, upstream_url, domain, request_shape, models, type_slug,
  created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		name, apiKey, boolInt(enabled), status, priority, weight, concurrency,
		nullableString(payload.ProxyURL), string(profile), nullableString(payload.Notes),
		quotaLimit, boolInt(quotaAutoDisable), nullableInt64(payload.ExpiresAt), boolInt(expireAutoDisable),
		nullableString(payload.RefreshToken), nullableInt64(tokenExpiresAt),
		nullableString(payload.AuthMode), nullableString(payload.UpstreamURL),
		nullableString(payload.Domain), nullableString(payload.RequestShape),
		encodeModelList(payload.Models), nullableString(payload.TypeSlug), ts, ts)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *Store) PatchAccount(id int64, payload AccountPatch) (bool, error) {
	sets := make([]string, 0, 12)
	args := make([]any, 0, 12)
	if payload.Name != nil {
		name := strings.TrimSpace(*payload.Name)
		if name == "" {
			return false, errors.New("name cannot be empty")
		}
		sets = append(sets, "name = ?")
		args = append(args, name)
	}
	if payload.APIKey != nil {
		apiKey := strings.TrimSpace(*payload.APIKey)
		if len(apiKey) < 8 {
			return false, errors.New("api_key must contain at least 8 characters")
		}
		sets = append(sets, "api_key = ?")
		args = append(args, apiKey)
		// 换凭证时必须同步改到期时间，而且要无条件改：
		// 新凭证不是 JWT（换成 ck_ key，或非 JWT 的 bearer token）时若保留旧值，
		// 面板会显示上一份凭证的到期日，续期判断也会据此误判。解不出就置 NULL。
		sets = append(sets, "token_expires_at = ?")
		if exp := JWTExpiry(apiKey); exp > 0 {
			args = append(args, exp)
		} else {
			args = append(args, nil)
		}
	}
	if payload.TokenExpiresAt != nil {
		// 显式传了就以传入的为准，覆盖上面从 JWT 推导的结果
		sets = append(sets, "token_expires_at = ?")
		args = append(args, nullableInt64(payload.TokenExpiresAt))
	}
	if payload.RefreshToken != nil {
		sets = append(sets, "refresh_token = ?")
		args = append(args, nullableString(payload.RefreshToken))
	}
	// 账号级上游覆盖：传空字符串即清除覆盖、回落到实例级配置
	for _, override := range []struct {
		column string
		value  *string
	}{
		{"auth_mode", payload.AuthMode},
		{"upstream_url", payload.UpstreamURL},
		{"domain", payload.Domain},
		{"request_shape", payload.RequestShape},
		{"type_slug", payload.TypeSlug},
	} {
		if override.value == nil {
			continue
		}
		sets = append(sets, override.column+" = ?")
		args = append(args, nullableString(override.value))
	}
	if payload.Models != nil {
		sets = append(sets, "models = ?")
		args = append(args, encodeModelList(payload.Models))
	}
	if payload.Enabled != nil {
		sets = append(sets, "enabled = ?", "status = ?")
		args = append(args, boolInt(*payload.Enabled))
		if *payload.Enabled {
			args = append(args, "active")
		} else {
			args = append(args, "disabled")
		}
	}
	if payload.Priority != nil {
		sets = append(sets, "priority = ?")
		args = append(args, *payload.Priority)
	}
	if payload.Weight != nil {
		sets = append(sets, "weight = ?")
		args = append(args, clamp(*payload.Weight, 1, 100))
	}
	if payload.Concurrency != nil {
		sets = append(sets, "concurrency = ?")
		args = append(args, clamp(*payload.Concurrency, 1, 100))
	}
	if payload.ProxyURL != nil {
		sets = append(sets, "proxy_url = ?")
		args = append(args, nullableString(payload.ProxyURL))
	}
	if payload.HeaderProfile != nil {
		profile, err := json.Marshal(payload.HeaderProfile)
		if err != nil {
			return false, err
		}
		sets = append(sets, "header_profile = ?")
		args = append(args, string(profile))
	}
	if payload.Notes != nil {
		sets = append(sets, "notes = ?")
		args = append(args, nullableString(payload.Notes))
	}
	if payload.ResetFailures {
		sets = append(sets,
			"consecutive_failures = ?",
			"cooldown_until = ?",
			"last_error = ?",
			"last_error_status = ?",
			"last_disable_reason = ?",
			"last_disable_at = ?",
			"status = ?",
		)
		args = append(args, 0, nil, nil, nil, nil, nil, "active")
	}
	if payload.QuotaLimit != nil {
		sets = append(sets, "quota_limit = ?")
		args = append(args, *payload.QuotaLimit)
	}
	if payload.QuotaAutoDisable != nil {
		sets = append(sets, "quota_auto_disable = ?")
		args = append(args, boolInt(*payload.QuotaAutoDisable))
	}
	if payload.ExpiresAt != nil {
		sets = append(sets, "expires_at = ?")
		args = append(args, nullableInt64(payload.ExpiresAt))
	}
	if payload.ExpireAutoDisable != nil {
		sets = append(sets, "expire_auto_disable = ?")
		args = append(args, boolInt(*payload.ExpireAutoDisable))
	}
	if payload.ResetUsage {
		sets = append(sets,
			"total_credit = ?",
			"prompt_tokens = ?",
			"completion_tokens = ?",
			"total_tokens = ?",
		)
		args = append(args, 0, 0, 0, 0)
	}
	if len(sets) == 0 {
		account, err := s.GetAccount(id)
		return account != nil, err
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, now(), id)
	result, err := s.db.Exec("UPDATE accounts SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	if err != nil {
		return false, err
	}
	rows, _ := result.RowsAffected()
	return rows > 0, nil
}

func (s *Store) SetEnabled(id int64, enabled bool) (bool, error) {
	ts := now()
	result, err := s.db.Exec(`
UPDATE accounts
SET enabled = ?, status = ?, consecutive_failures = CASE WHEN ? THEN 0 ELSE consecutive_failures END,
    cooldown_until = CASE WHEN ? THEN NULL ELSE cooldown_until END,
    last_disable_reason = CASE WHEN ? THEN NULL ELSE last_disable_reason END,
    last_disable_at = CASE WHEN ? THEN NULL ELSE last_disable_at END,
    last_error_status = CASE WHEN ? THEN NULL ELSE last_error_status END,
    updated_at = ?
WHERE id = ?`,
		boolInt(enabled), enabledStatus(enabled), enabled, enabled, enabled, enabled, enabled, ts, id)
	if err != nil {
		return false, err
	}
	rows, _ := result.RowsAffected()
	return rows > 0, nil
}

func (s *Store) DeleteAccount(id int64) (bool, error) {
	result, err := s.db.Exec("DELETE FROM accounts WHERE id = ?", id)
	if err != nil {
		return false, err
	}
	rows, _ := result.RowsAffected()
	return rows > 0, nil
}

func (s *Store) GetAccount(id int64) (*Account, error) {
	row := s.db.QueryRow(`
SELECT id, name, api_key, enabled, status, priority, weight, concurrency, proxy_url,
       header_profile, notes, consecutive_failures, cooldown_until,
       auth_mode, upstream_url, domain, request_shape, models, type_slug
FROM accounts WHERE id = ?`, id)
	account, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return account, err
}

func (s *Store) ListAccounts() ([]PublicAccount, error) {
	rows, err := s.db.Query(`
SELECT id, name, api_key, enabled, status, priority, weight, concurrency,
       proxy_url, header_profile, notes, consecutive_failures, cooldown_until,
       total_requests, total_success, total_failures, total_credit,
       prompt_tokens, completion_tokens, total_tokens, last_success_at,
       last_failure_at, last_error, last_error_status, quota_limit,
       quota_auto_disable, expires_at, expire_auto_disable,
       last_disable_reason, last_disable_at, created_at, updated_at,
       credits_remaining, credits_total, credits_cycle_end, credits_checked_at,
       credits_error, credits_paused, credits_packages,
       auth_mode, upstream_url, domain, request_shape, models, type_slug,
       refresh_token IS NOT NULL AND refresh_token <> '', token_expires_at
FROM accounts ORDER BY priority DESC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var accounts []PublicAccount
	for rows.Next() {
		account, err := scanPublicAccount(rows)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

func (s *Store) SchedulableAccounts() ([]Account, error) {
	ts := now()
	s.autoPauseDueAccounts(ts)
	rows, err := s.db.Query(`
SELECT id, name, api_key, enabled, status, priority, weight, concurrency, proxy_url,
       header_profile, notes, consecutive_failures, cooldown_until,
       auth_mode, upstream_url, domain, request_shape, models, type_slug
FROM accounts
WHERE enabled = 1
  AND credits_paused = 0
  AND (status = 'active' OR (status = 'cooldown' AND (cooldown_until IS NULL OR cooldown_until <= ?)))
  AND NOT (quota_auto_disable = 1 AND quota_limit > 0 AND total_credit >= quota_limit)
  AND NOT (expire_auto_disable = 1 AND expires_at IS NOT NULL AND expires_at <= ?)
ORDER BY priority DESC, id ASC`, ts, ts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var accounts []Account
	for rows.Next() {
		account, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, *account)
	}
	return accounts, rows.Err()
}

func (s *Store) RecordSuccess(id int64, usage map[string]any) {
	credit := floatValue(usage["credit"])
	prompt := int64Value(usage["prompt_tokens"])
	completion := int64Value(usage["completion_tokens"])
	total := int64Value(usage["total_tokens"])
	if total == 0 {
		total = prompt + completion
	}
	ts := now()
	_, _ = s.db.Exec(`
UPDATE accounts
SET status = 'active',
    consecutive_failures = 0,
    cooldown_until = NULL,
    total_requests = total_requests + 1,
    total_success = total_success + 1,
    total_credit = total_credit + ?,
    prompt_tokens = prompt_tokens + ?,
    completion_tokens = completion_tokens + ?,
    total_tokens = total_tokens + ?,
    last_success_at = ?,
    last_error = NULL,
    updated_at = ?
WHERE id = ?`, credit, prompt, completion, total, ts, ts, id)
	s.autoPauseAccountIfNeeded(id, ts)
}

func (s *Store) RecordFailure(id int64, errorText string, statusCode int, cooldownSeconds int, failureThreshold int, autoDisable bool, disableReason string) {
	ts := now()
	if len(errorText) > 1000 {
		errorText = errorText[:1000]
	}
	var currentFailures int
	_ = s.db.QueryRow("SELECT consecutive_failures FROM accounts WHERE id = ?", id).Scan(&currentFailures)
	nextFailures := currentFailures + 1
	enabled := 1
	status := "active"
	var cooldownUntil any
	var lastDisableReason any
	var lastDisableAt any
	if autoDisable {
		enabled = 0
		status = "disabled"
		if disableReason == "" {
			disableReason = fmt.Sprintf("auto_disabled_status_%d", statusCode)
		}
		lastDisableReason = disableReason
		lastDisableAt = ts
	} else if statusCode == 429 || statusCode >= 500 || nextFailures >= failureThreshold {
		status = "cooldown"
		cooldownUntil = ts + int64(cooldownSeconds)
	}
	_, _ = s.db.Exec(`
UPDATE accounts
SET enabled = ?,
    status = ?,
    consecutive_failures = ?,
    cooldown_until = ?,
    total_requests = total_requests + 1,
    total_failures = total_failures + 1,
    last_failure_at = ?,
    last_error = ?,
    last_error_status = ?,
    last_disable_reason = CASE WHEN ? THEN ? ELSE last_disable_reason END,
    last_disable_at = CASE WHEN ? THEN ? ELSE last_disable_at END,
    updated_at = ?
WHERE id = ?`, enabled, status, nextFailures, cooldownUntil, ts, errorText, nullableStatus(statusCode), autoDisable, lastDisableReason, autoDisable, lastDisableAt, ts, id)
}

func (s *Store) autoPauseDueAccounts(ts int64) {
	_, _ = s.db.Exec(`
UPDATE accounts
SET enabled = 0, status = 'disabled', last_disable_reason = 'quota_exhausted',
    last_disable_at = ?, updated_at = ?
WHERE enabled = 1 AND quota_auto_disable = 1 AND quota_limit > 0 AND total_credit >= quota_limit`, ts, ts)
	_, _ = s.db.Exec(`
UPDATE accounts
SET enabled = 0, status = 'disabled', last_disable_reason = 'expired',
    last_disable_at = ?, updated_at = ?
WHERE enabled = 1 AND expire_auto_disable = 1 AND expires_at IS NOT NULL AND expires_at <= ?`, ts, ts, ts)
}

func (s *Store) autoPauseAccountIfNeeded(id int64, ts int64) {
	_, _ = s.db.Exec(`
UPDATE accounts
SET enabled = 0, status = 'disabled', last_disable_reason = 'quota_exhausted',
    last_disable_at = ?, updated_at = ?
WHERE id = ? AND enabled = 1 AND quota_auto_disable = 1 AND quota_limit > 0 AND total_credit >= quota_limit`, ts, ts, id)
	_, _ = s.db.Exec(`
UPDATE accounts
SET enabled = 0, status = 'disabled', last_disable_reason = 'expired',
    last_disable_at = ?, updated_at = ?
WHERE id = ? AND enabled = 1 AND expire_auto_disable = 1 AND expires_at IS NOT NULL AND expires_at <= ?`, ts, ts, id, ts)
}

func (s *Store) Stats() (Stats, error) {
	var stats Stats
	err := s.db.QueryRow(`
SELECT
  COALESCE(COUNT(*), 0),
  COALESCE(SUM(CASE WHEN enabled = 1 THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(total_requests), 0),
  COALESCE(SUM(total_success), 0),
  COALESCE(SUM(total_failures), 0),
  COALESCE(SUM(total_credit), 0),
  COALESCE(SUM(prompt_tokens), 0),
  COALESCE(SUM(completion_tokens), 0),
  COALESCE(SUM(total_tokens), 0)
FROM accounts`).Scan(
		&stats.Accounts,
		&stats.EnabledAccounts,
		&stats.TotalRequests,
		&stats.TotalSuccess,
		&stats.TotalFailures,
		&stats.TotalCredit,
		&stats.PromptTokens,
		&stats.CompletionTokens,
		&stats.TotalTokens,
	)
	return stats, err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanAccount(row rowScanner) (*Account, error) {
	var account Account
	var enabled int
	var profileRaw string
	if err := row.Scan(
		&account.ID,
		&account.Name,
		&account.APIKey,
		&enabled,
		&account.Status,
		&account.Priority,
		&account.Weight,
		&account.Concurrency,
		&account.ProxyURL,
		&profileRaw,
		&account.Notes,
		&account.ConsecutiveFailures,
		&account.CooldownUntil,
		&account.AuthMode,
		&account.UpstreamURL,
		&account.Domain,
		&account.RequestShape,
		&account.Models,
		&account.TypeSlug,
	); err != nil {
		return nil, err
	}
	account.Enabled = enabled == 1
	account.HeaderProfile = parseProfile(profileRaw)
	account.Weight = max(1, account.Weight)
	account.Concurrency = max(1, account.Concurrency)
	return &account, nil
}

func scanPublicAccount(rows *sql.Rows) (PublicAccount, error) {
	var authMode, upstreamURL, domain, requestShape, models, typeSlug sql.NullString
	var tokenExpiresAt sql.NullInt64
	var hasRefresh int
	var data PublicAccount
	var apiKey string
	var enabled int
	var proxy sql.NullString
	var profileRaw string
	var notes sql.NullString
	var cooldown sql.NullInt64
	var lastSuccess sql.NullInt64
	var lastFailure sql.NullInt64
	var lastError sql.NullString
	var lastErrorStatus sql.NullInt64
	var quotaAutoDisable int
	var expiresAt sql.NullInt64
	var expireAutoDisable int
	var lastDisableReason sql.NullString
	var lastDisableAt sql.NullInt64
	var creditsRemaining sql.NullFloat64
	var creditsTotal sql.NullFloat64
	var creditsCycleEnd sql.NullInt64
	var creditsCheckedAt sql.NullInt64
	var creditsError sql.NullString
	var creditsPaused int
	var creditsPackages sql.NullString
	if err := rows.Scan(
		&data.ID,
		&data.Name,
		&apiKey,
		&enabled,
		&data.Status,
		&data.Priority,
		&data.Weight,
		&data.Concurrency,
		&proxy,
		&profileRaw,
		&notes,
		&data.ConsecutiveFailures,
		&cooldown,
		&data.TotalRequests,
		&data.TotalSuccess,
		&data.TotalFailures,
		&data.TotalCredit,
		&data.PromptTokens,
		&data.CompletionTokens,
		&data.TotalTokens,
		&lastSuccess,
		&lastFailure,
		&lastError,
		&lastErrorStatus,
		&data.QuotaLimit,
		&quotaAutoDisable,
		&expiresAt,
		&expireAutoDisable,
		&lastDisableReason,
		&lastDisableAt,
		&data.CreatedAt,
		&data.UpdatedAt,
		&creditsRemaining,
		&creditsTotal,
		&creditsCycleEnd,
		&creditsCheckedAt,
		&creditsError,
		&creditsPaused,
		&creditsPackages,
		&authMode,
		&upstreamURL,
		&domain,
		&requestShape,
		&models,
		&typeSlug,
		&hasRefresh,
		&tokenExpiresAt,
	); err != nil {
		return data, err
	}
	data.APIKeyPreview = maskSecret(apiKey)
	data.Enabled = enabled == 1
	data.ProxyURL = nullStringPtr(proxy)
	data.HeaderProfile = parseProfile(profileRaw)
	data.Notes = nullStringPtr(notes)
	data.CooldownUntil = nullIntPtr(cooldown)
	data.LastSuccessAt = nullIntPtr(lastSuccess)
	data.LastFailureAt = nullIntPtr(lastFailure)
	data.LastError = nullStringPtr(lastError)
	data.LastErrorStatus = nullIntPtrAsInt(lastErrorStatus)
	data.QuotaAutoDisable = quotaAutoDisable == 1
	if data.QuotaLimit > 0 {
		remaining := data.QuotaLimit - data.TotalCredit
		if remaining < 0 {
			remaining = 0
		}
		data.QuotaRemaining = &remaining
		data.QuotaExceeded = data.TotalCredit >= data.QuotaLimit
	}
	data.ExpiresAt = nullIntPtr(expiresAt)
	data.ExpireAutoDisable = expireAutoDisable == 1
	data.LastDisableReason = nullStringPtr(lastDisableReason)
	data.LastDisableAt = nullIntPtr(lastDisableAt)
	data.CreditsRemaining = nullFloatPtr(creditsRemaining)
	data.CreditsTotal = nullFloatPtr(creditsTotal)
	data.CreditsCycleEnd = nullIntPtr(creditsCycleEnd)
	data.CreditsCheckedAt = nullIntPtr(creditsCheckedAt)
	data.CreditsError = nullStringPtr(creditsError)
	data.CreditsPackages = nullStringPtr(creditsPackages)
	data.CreditsPaused = creditsPaused == 1
	data.AuthMode = strings.TrimSpace(authMode.String)
	data.UpstreamURL = nullStringPtr(upstreamURL)
	data.Domain = nullStringPtr(domain)
	data.RequestShape = strings.TrimSpace(requestShape.String)
	data.Models = decodeModelList(models.String)
	data.TypeSlug = strings.TrimSpace(typeSlug.String)
	data.HasRefreshToken = hasRefresh == 1
	data.TokenExpiresAt = nullIntPtr(tokenExpiresAt)
	return data, nil
}

// RefreshTokenFor 读取账号的 refresh token（仅 Bearer 模式用）。
func (s *Store) RefreshTokenFor(id int64) (string, error) {
	var token sql.NullString
	err := s.db.QueryRow("SELECT refresh_token FROM accounts WHERE id = ?", id).Scan(&token)
	return token.String, err
}

// SaveAccessToken 保存刷新后的 access/refresh token。
func (s *Store) SaveAccessToken(id int64, access, refresh string, expiresAt int64) error {
	ts := now()
	_, err := s.db.Exec(`UPDATE accounts
SET api_key = ?,
    refresh_token = COALESCE(NULLIF(?, ''), refresh_token),
    token_expires_at = COALESCE(NULLIF(?, 0), token_expires_at),
    updated_at = ?
WHERE id = ?`, access, refresh, expiresAt, ts, id)
	return err
}

// SaveCredits 记录一次余额查询结果。errMsg 非空时只记错误，不动余额字段。
func (s *Store) SaveCredits(id int64, remain, total float64, cycleEnd *int64, errMsg string, packagesJSON ...string) error {
	ts := now()
	if errMsg != "" {
		_, err := s.db.Exec(
			"UPDATE accounts SET credits_error = ?, credits_checked_at = ?, updated_at = ? WHERE id = ?",
			errMsg, ts, ts, id)
		return err
	}
	var packages any
	if len(packagesJSON) > 0 && packagesJSON[0] != "" {
		packages = packagesJSON[0]
	}
	_, err := s.db.Exec(`UPDATE accounts
SET credits_remaining = ?, credits_total = ?, credits_cycle_end = ?, credits_checked_at = ?,
    credits_packages = COALESCE(?, credits_packages), credits_error = NULL, updated_at = ?
WHERE id = ?`, remain, total, cycleEnd, ts, packages, ts, id)
	return err
}

// PauseForCredits 余额耗尽：进入冷却而不是禁用——冷却到本周期结束自动恢复，
// 不需要人工介入（月度包每月 1 号归零）。
func (s *Store) PauseForCredits(id int64, cycleEnd *int64, reason string) error {
	ts := now()
	var until any
	if cycleEnd != nil && *cycleEnd > ts {
		until = *cycleEnd
	}
	_, err := s.db.Exec(`UPDATE accounts
SET credits_paused = 1,
    status = 'cooldown',
    consecutive_failures = 0,
    cooldown_until = COALESCE(?, cooldown_until),
    last_disable_reason = ?,
    updated_at = ?
WHERE id = ? AND credits_paused = 0`, until, reason, ts, id)
	return err
}

// ResumeCreditsPause 余额恢复（如跳月重置）：只解除由余额触发的暂停。
func (s *Store) ResumeCreditsPause(id int64) error {
	ts := now()
	_, err := s.db.Exec(`UPDATE accounts
SET credits_paused = 0,
    status = CASE WHEN last_disable_reason = 'credits_exhausted' AND status = 'cooldown' THEN 'active' ELSE status END,
    cooldown_until = CASE WHEN last_disable_reason = 'credits_exhausted' THEN NULL ELSE cooldown_until END,
    last_disable_reason = CASE WHEN last_disable_reason = 'credits_exhausted' THEN NULL ELSE last_disable_reason END,
    updated_at = ?
WHERE id = ? AND credits_paused = 1`, ts, id)
	return err
}

func parseProfile(raw string) map[string]any {
	var profile map[string]any
	if err := json.Unmarshal([]byte(raw), &profile); err != nil || profile == nil {
		return map[string]any{}
	}
	return profile
}

func maskSecret(value string) string {
	if value == "" {
		return ""
	}
	if len(value) <= 12 {
		return value[:min(3, len(value))] + "..."
	}
	return value[:6] + "..." + value[len(value)-4:]
}

func now() int64 {
	return time.Now().Unix()
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func enabledStatus(enabled bool) string {
	if enabled {
		return "active"
	}
	return "disabled"
}

func nullableString(value *string) any {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil
	}
	return *value
}

func nullableInt64(value *int64) any {
	if value == nil || *value <= 0 {
		return nil
	}
	return *value
}

func nullableStatus(value int) any {
	if value <= 0 {
		return nil
	}
	return value
}

func nullStringPtr(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func nullIntPtr(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	return &value.Int64
}

func nullFloatPtr(value sql.NullFloat64) *float64 {
	if !value.Valid {
		return nil
	}
	return &value.Float64
}

func nullIntPtrAsInt(value sql.NullInt64) *int {
	if !value.Valid {
		return nil
	}
	converted := int(value.Int64)
	return &converted
}

func defaultMap(value map[string]any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	return value
}

func intDefault(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

func floatDefault(value *float64, fallback float64) float64 {
	if value == nil {
		return fallback
	}
	return *value
}

func boolDefault(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func clamp(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func int64Value(value any) int64 {
	switch item := value.(type) {
	case int:
		return int64(item)
	case int64:
		return item
	case float64:
		return int64(item)
	case json.Number:
		parsed, _ := item.Int64()
		return parsed
	default:
		return 0
	}
}

func floatValue(value any) float64 {
	switch item := value.(type) {
	case float64:
		return item
	case float32:
		return float64(item)
	case int:
		return float64(item)
	case int64:
		return float64(item)
	case json.Number:
		parsed, _ := item.Float64()
		if math.IsNaN(parsed) {
			return 0
		}
		return parsed
	default:
		return 0
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (a Account) ProxyString() string {
	if a.ProxyURL.Valid {
		return a.ProxyURL.String
	}
	return ""
}

func (a Account) String() string {
	return fmt.Sprintf("account[%d:%s]", a.ID, a.Name)
}
