package app

import (
	"database/sql"
	"testing"
)

func cnConfig() Config {
	return Config{
		AuthMode:     AuthModeAPIKey,
		UpstreamURL:  "https://copilot.tencent.com/v2/chat/completions",
		Domain:       "copilot.tencent.com",
		RequestShape: ShapeCN,
		Models:       []string{"glm-5.1"},
	}
}

// 老账号（几列覆盖全是 NULL）必须完全按实例级配置走。
// 这是 CN 生产不回归的保证：库里已有的行不会因为这次改造改变任何行为。
func TestResolveProfile_LegacyAccountUnchanged(t *testing.T) {
	cfg := cnConfig()
	got := resolveProfile(Account{ID: 1, Name: "老 ck 号"}, nil, cfg)

	if got.AuthMode != AuthModeAPIKey {
		t.Errorf("认证方式应回落实例级 api_key，得到 %q", got.AuthMode)
	}
	if got.UpstreamURL != cfg.UpstreamURL {
		t.Errorf("上游应回落 %q，得到 %q", cfg.UpstreamURL, got.UpstreamURL)
	}
	if got.Domain != cfg.Domain {
		t.Errorf("domain 应回落 %q，得到 %q", cfg.Domain, got.Domain)
	}
	if got.RequestShape != ShapeCN {
		t.Errorf("整形应回落 cn，得到 %q", got.RequestShape)
	}
	if len(got.Models) != 1 || got.Models[0] != "glm-5.1" {
		t.Errorf("模型应回落实例级，得到 %v", got.Models)
	}
}

// 同一个 CN 实例里放一个 intl 凭证号，必须整套走自己的配置。
func TestResolveProfile_IntlAccountInCNInstance(t *testing.T) {
	account := Account{
		ID:           2,
		AuthMode:     sql.NullString{String: "bearer", Valid: true},
		UpstreamURL:  sql.NullString{String: "https://www.codebuddy.ai/v2/chat/completions", Valid: true},
		RequestShape: sql.NullString{String: "intl", Valid: true},
		Models:       sql.NullString{String: `["deepseek-v4.1-flash"]`, Valid: true},
	}
	got := resolveProfile(account, nil, cnConfig())

	if !got.IsBearer() {
		t.Errorf("应为 bearer，得到 %q", got.AuthMode)
	}
	if got.UpstreamURL != "https://www.codebuddy.ai/v2/chat/completions" {
		t.Errorf("上游没走账号级覆盖：%q", got.UpstreamURL)
	}
	// 没显式写 domain，应按上游主机推导，而不是继承 CN 的 copilot.tencent.com
	if got.Domain != "www.codebuddy.ai" {
		t.Errorf("domain 应由上游推导为 www.codebuddy.ai，得到 %q", got.Domain)
	}
	if got.RequestShape != ShapeIntl {
		t.Errorf("整形应为 intl，得到 %q", got.RequestShape)
	}
}

func TestServesModel(t *testing.T) {
	intl := Account{Models: sql.NullString{String: `["deepseek-v4.1-flash"]`, Valid: true}}
	legacy := Account{} // 没声明模型 = 接全部，老账号行为

	if !intl.ServesModel("deepseek-v4.1-flash") {
		t.Error("intl 号应接自己声明的模型")
	}
	if intl.ServesModel("glm-5.1") {
		t.Error("intl 号不该接 glm-5.1——免费凭证号接到付费模型请求会直接失败")
	}
	if !legacy.ServesModel("glm-5.1") || !legacy.ServesModel("任意模型") {
		t.Error("没声明模型的账号应接全部")
	}
	if !intl.ServesModel("") {
		t.Error("没指定模型的请求不应被过滤掉")
	}
}

func TestDetectAuthMode(t *testing.T) {
	cases := map[string]string{
		"ck_abcdefghijklmn":       AuthModeAPIKey,
		"eyJhbGciOi.eyJzdWIi.sig": AuthModeBearer,
		"随便一个串":                   "",
		"":                        "",
	}
	for input, want := range cases {
		if got := DetectAuthMode(input); got != want {
			t.Errorf("DetectAuthMode(%q) = %q，期望 %q", input, got, want)
		}
	}
}

func TestModelListAcceptsCommaSeparated(t *testing.T) {
	account := Account{Models: sql.NullString{String: "glm-5.1, deepseek-v4.1-flash", Valid: true}}
	got := account.ModelList()
	if len(got) != 2 || got[0] != "glm-5.1" || got[1] != "deepseek-v4.1-flash" {
		t.Errorf("逗号分隔也该解析，得到 %v", got)
	}
}
