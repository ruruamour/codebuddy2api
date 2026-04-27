package app

import (
	"bufio"
	"fmt"
	"net"
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
}

func LoadConfig() Config {
	loadDotEnv(".env")
	models := splitCSV(envString("CODEBUDDY2API_MODELS", strings.Join(DefaultCodeBuddyModelIDs, ",")))
	if len(models) == 0 {
		models = append([]string{}, DefaultCodeBuddyModelIDs...)
	}
	dbPath := envString("CODEBUDDY2API_DB_PATH", "./data/codebuddy2api.sqlite3")
	_ = os.MkdirAll(filepath.Dir(expandHome(dbPath)), 0o755)
	return Config{
		Host:                   envString("CODEBUDDY2API_HOST", "127.0.0.1"),
		Port:                   envInt("CODEBUDDY2API_PORT", 18182),
		DBPath:                 dbPath,
		APIKey:                 strings.TrimSpace(os.Getenv("CODEBUDDY2API_API_KEY")),
		AdminKey:               strings.TrimSpace(os.Getenv("CODEBUDDY2API_ADMIN_KEY")),
		UpstreamURL:            envString("CODEBUDDY2API_UPSTREAM_URL", "https://copilot.tencent.com/v2/chat/completions"),
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
	}
}

func (c Config) ListenAddr() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
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
