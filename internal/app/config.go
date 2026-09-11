package app

import (
	"bufio"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const Version = "0.2.0-go"

type Config struct {
	Host                   string
	Port                   int
	DBPath                 string
	APIKey                 string
	AdminKey               string
	UpstreamURL            string
	Models                 []string
	PoolStrategy           string
	CooldownSeconds        int
	FailureThreshold       int
	DefaultConcurrency     int
	RequestTimeoutSeconds  int
	ConnectTimeoutSeconds  int
	LogLevel               string
	DebugRequests          bool
	AutoDisableStatusCodes map[int]struct{}
	AutoDisableQuotaErrors bool
	AdminTrustCFAccess     bool
	AdminAccessEmails      map[string]struct{}
	// CreditsRefreshMinutes 余额轮询间隔（分钟，0=关闭自动轮询）。
	// 查余额走官方计费接口，不消耗积分。
	CreditsRefreshMinutes int
	// CreditsMinRemain 低于该剩余积分即暂停账号（冷却到本周期结束）。
	CreditsMinRemain float64
	// AuthMode 上游认证方式：api_key（默认，X-Api-Key: ck_xxx）或 bearer（Authorization: Bearer <token>）。
	// intl 网关（www.codebuddy.ai / www.workbuddy.ai）只认 Bearer，且不接受 ck_ API key。
	AuthMode string
	// Domain 上游 X-Domain。默认从 UpstreamURL 主机推导（国内 = copilot.tencent.com）。
	// intl 必须与 chat endpoint 主机一致（官方 CLI 就是这么推导的）。
	Domain string
	// RequestShape 请求整形方式：cn（默认，透传）或 intl。
	// intl 网关比国内严：必须首条是 system，user 的字符串 content 要转成 typed block，
	// 否则 11128（first message is not system prompt）/ 11101。
	RequestShape string
	// TokenRefreshAheadDays Bearer 模式下提前多少天续期（默认 30）。
	TokenRefreshAheadDays int
	// TokenRefreshCheckHours 续期巡检间隔（小时，默认 6）。
	// 独立于余额轮询：续期是账号存活的底线，不能被 CREDITS_REFRESH_MIN=0 顺手关掉。
	TokenRefreshCheckHours int
}

// ShapeIntl / ShapeCN 是 RequestShape 的两个取值。
const (
	ShapeCN   = "cn"
	ShapeIntl = "intl"
)

// AuthModeAPIKey / AuthModeBearer 是 AuthMode 的两个取值。
// api_key 发 X-Api-Key: ck_xxx（CN）；bearer 发 Authorization: Bearer <token>（intl OAuth）。
const (
	AuthModeAPIKey = "api_key"
	AuthModeBearer = "bearer"
)

func LoadConfig() Config {
	loadDotEnv(".env")
	models := splitCSV(envString("CODEBUDDY2API_MODELS", strings.Join(DefaultCodeBuddyModelIDs, ",")))
	if len(models) == 0 {
		models = append([]string{}, DefaultCodeBuddyModelIDs...)
	}
	dbPath := envString("CODEBUDDY2API_DB_PATH", "./data/codebuddy2api.sqlite3")
	_ = os.MkdirAll(filepath.Dir(expandHome(dbPath)), 0o755)
	upstreamURL := envString("CODEBUDDY2API_UPSTREAM_URL", "https://copilot.tencent.com/v2/chat/completions")
	return Config{
		Host:                   envString("CODEBUDDY2API_HOST", "127.0.0.1"),
		Port:                   envInt("CODEBUDDY2API_PORT", 18182),
		DBPath:                 dbPath,
		APIKey:                 strings.TrimSpace(os.Getenv("CODEBUDDY2API_API_KEY")),
		AdminKey:               strings.TrimSpace(os.Getenv("CODEBUDDY2API_ADMIN_KEY")),
		UpstreamURL:            upstreamURL,
		AuthMode:               NormalizeAuthMode(envString("CODEBUDDY2API_AUTH_MODE", "api_key")),
		Domain:                 envString("CODEBUDDY2API_DOMAIN", HostOfURL(upstreamURL)),
		RequestShape:           NormalizeRequestShape(envString("CODEBUDDY2API_REQUEST_SHAPE", ShapeCN)),
		TokenRefreshAheadDays:  envInt("CODEBUDDY2API_TOKEN_REFRESH_AHEAD_DAYS", 30),
		TokenRefreshCheckHours: envInt("CODEBUDDY2API_TOKEN_REFRESH_CHECK_HOURS", 6),
		Models:                 models,
		PoolStrategy:           NormalizePoolStrategy(os.Getenv("CODEBUDDY2API_POOL_STRATEGY"), PoolStrategyRoundRobin),
		CooldownSeconds:        envInt("CODEBUDDY2API_COOLDOWN_SECONDS", 300),
		FailureThreshold:       envInt("CODEBUDDY2API_FAILURE_THRESHOLD", 3),
		DefaultConcurrency:     envInt("CODEBUDDY2API_DEFAULT_CONCURRENCY", 1),
		RequestTimeoutSeconds:  envInt("CODEBUDDY2API_REQUEST_TIMEOUT_SECONDS", 300),
		ConnectTimeoutSeconds:  envInt("CODEBUDDY2API_CONNECT_TIMEOUT_SECONDS", 10),
		LogLevel:               strings.ToUpper(envString("CODEBUDDY2API_LOG_LEVEL", "INFO")),
		DebugRequests:          envBool("CODEBUDDY2API_DEBUG_REQUESTS", false),
		AutoDisableStatusCodes: envIntSet("CODEBUDDY2API_AUTO_DISABLE_STATUS_CODES", "401,403"),
		AutoDisableQuotaErrors: envBool("CODEBUDDY2API_AUTO_DISABLE_QUOTA_ERRORS", true),
		AdminTrustCFAccess:     envBool("CODEBUDDY2API_ADMIN_TRUST_CF_ACCESS", true),
		AdminAccessEmails:      envStringSet("CODEBUDDY2API_ADMIN_ACCESS_EMAILS", ""),
		CreditsRefreshMinutes:  envInt("CODEBUDDY2API_CREDITS_REFRESH_MIN", 10),
		CreditsMinRemain:       envFloat("CODEBUDDY2API_CREDITS_MIN_REMAIN", 0.5),
	}
}

func (c Config) ListenAddr() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

// NormalizeAuthMode 把环境变量收敛到 api_key / bearer。
func NormalizeAuthMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "bearer", "token", "oauth", "jwt":
		return "bearer"
	default:
		return "api_key"
	}
}

// NormalizeRequestShape 把环境变量收敛到 cn / intl。
func NormalizeRequestShape(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "intl", "international", "global", "oversea", "ai":
		return ShapeIntl
	default:
		return ShapeCN
	}
}

// HostOfURL 取出 URL 的主机名（失败时返回空串）。
func HostOfURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return parsed.Host
}

func (c Config) AdminTokens() []string {
	var tokens []string
	if c.AdminKey != "" {
		tokens = append(tokens, c.AdminKey)
	}
	if c.APIKey != "" {
		tokens = append(tokens, c.APIKey)
	}
	return tokens
}

func envString(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envFloat(name string, fallback float64) float64 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func envBool(name string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	if value == "" {
		return fallback
	}
	switch value {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func envIntSet(name string, fallback string) map[int]struct{} {
	result := map[int]struct{}{}
	for _, item := range splitCSV(envString(name, fallback)) {
		parsed, err := strconv.Atoi(item)
		if err == nil {
			result[parsed] = struct{}{}
		}
	}
	return result
}

func envStringSet(name string, fallback string) map[string]struct{} {
	result := map[string]struct{}{}
	for _, item := range splitCSV(envString(name, fallback)) {
		result[strings.ToLower(item)] = struct{}{}
	}
	return result
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if item := strings.TrimSpace(part); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func loadDotEnv(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		key := strings.TrimSpace(parts[0])
		if key == "" || os.Getenv(key) != "" {
			continue
		}
		value := strings.TrimSpace(parts[1])
		value = strings.Trim(value, `"'`)
		_ = os.Setenv(key, value)
	}
}

func expandHome(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return fmt.Sprint(path)
}
