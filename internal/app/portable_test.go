package app

import (
	"encoding/json"
	"testing"
)

func mustRaw(t *testing.T, v string) json.RawMessage {
	t.Helper()
	if !json.Valid([]byte(v)) {
		t.Fatalf("测试数据不是合法 JSON: %s", v)
	}
	return json.RawMessage(v)
}

// 官方登录产出的 token.json 必须能直接导入，且 refresh token 不能丢。
//
// 只导 access 的话，导进来的号就是「到期即废」——正是自助续期要解决的问题
// 从导入口漏回来。domain 还要能反推出上游与整形，省得手填。
func TestImport_OfficialTokenJSON(t *testing.T) {
	store := newTestStore(t)
	raw := `{
      "accessToken": "eyJhbGciOiJSUzI1NiJ9.eyJleHAiOjE4MjA2NzEwMjB9.sig",
      "refreshToken": "eyJhbGciOiJIUzUxMiJ9.eyJ0eXAiOiJPZmZsaW5lIn0.sig",
      "expiresIn": 31535360,
      "tokenType": "Bearer",
      "domain": "www.codebuddy.ai"
    }`

	result, err := store.ImportAccounts(ImportRequest{Data: mustRaw(t, raw)})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Imported != 1 {
		t.Fatalf("应导入 1 个，得到 %+v", result)
	}

	accounts, err := store.ListAccounts()
	if err != nil || len(accounts) != 1 {
		t.Fatalf("list: %v (%d 个)", err, len(accounts))
	}
	a := accounts[0]
	if !a.HasRefreshToken {
		t.Error("refresh token 丢了——导进来的号到期就废")
	}
	if a.AuthMode != AuthModeBearer {
		t.Errorf("应按凭证识别为 bearer，得到 %q", a.AuthMode)
	}
	// domain 自带时上游与整形要能反推出来
	if a.UpstreamURL == nil || *a.UpstreamURL != "https://www.codebuddy.ai/v2/chat/completions" {
		t.Errorf("上游没按 domain 反推：%v", a.UpstreamURL)
	}
	if a.RequestShape != ShapeIntl {
		t.Errorf("整形应为 intl，得到 %q", a.RequestShape)
	}
	if a.TokenExpiresAt == nil || *a.TokenExpiresAt <= now() {
		t.Errorf("expiresIn 应换算成绝对到期时间，得到 %v", a.TokenExpiresAt)
	}
}

// 导出的备份必须能原样导回，且凭证完整。
func TestExportImport_RoundTrip(t *testing.T) {
	source := newTestStore(t)
	if _, err := source.AddAccount(AccountCreate{
		Name:         "intl 号",
		APIKey:       "eyJhbGciOiJSUzI1NiJ9.eyJleHAiOjE4MjA2NzEwMjB9.sig",
		RefreshToken: strPtr("refresh-secret"),
		AuthMode:     strPtr(AuthModeBearer),
		UpstreamURL:  strPtr("https://www.codebuddy.ai/v2/chat/completions"),
		RequestShape: strPtr(ShapeIntl),
		Models:       []string{"deepseek-v4.1-flash"},
	}); err != nil {
		t.Fatalf("add: %v", err)
	}

	bundle, err := source.ExportAccounts()
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(bundle.Accounts) != 1 || bundle.Accounts[0].RefreshToken != "refresh-secret" {
		t.Fatalf("导出没带上 refresh token: %+v", bundle.Accounts)
	}

	// 原样导进一个全新的库
	encoded, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	target := newTestStore(t)
	result, err := target.ImportAccounts(ImportRequest{Data: encoded})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Imported != 1 {
		t.Fatalf("回导失败: %+v", result)
	}

	restored, err := target.ExportAccounts()
	if err != nil {
		t.Fatalf("re-export: %v", err)
	}
	got, want := restored.Accounts[0], bundle.Accounts[0]
	if got.APIKey != want.APIKey || got.RefreshToken != want.RefreshToken ||
		got.AuthMode != want.AuthMode || got.UpstreamURL != want.UpstreamURL ||
		got.RequestShape != want.RequestShape || len(got.Models) != len(want.Models) {
		t.Errorf("往返后不一致\n导出 %+v\n回导 %+v", want, got)
	}
}

// 按行文本的三种写法都要认，尤其 名称,access,refresh 这种带续期种子的。
func TestParseImportLines(t *testing.T) {
	items := parseImportLines(`
# 注释行会被跳过
ck_onlycredential
号2,ck_withname
intl号,eyJaccess.aa.bb,eyJrefresh.cc.dd
eyJbare.ee.ff,eyJpair.gg.hh
`)
	if len(items) != 4 {
		t.Fatalf("应解析出 4 条，得到 %d: %+v", len(items), items)
	}
	if items[0].APIKey != "ck_onlycredential" || items[0].Name != "" {
		t.Errorf("单段应当成纯凭证: %+v", items[0])
	}
	if items[1].Name != "号2" || items[1].APIKey != "ck_withname" {
		t.Errorf("两段（名称+凭证）解析错: %+v", items[1])
	}
	if items[2].Name != "intl号" || items[2].RefreshToken != "eyJrefresh.cc.dd" {
		t.Errorf("三段应带上 refresh token: %+v", items[2])
	}
	// 两段且两段都像凭证时，应理解成 access,refresh 而不是 名称,凭证
	if items[3].APIKey != "eyJbare.ee.ff" || items[3].RefreshToken != "eyJpair.gg.hh" {
		t.Errorf("两段皆为凭证时应当成 access,refresh: %+v", items[3])
	}
}

// 同一个凭证重复导入默认跳过，overwrite 时才更新。
func TestImport_DuplicateHandling(t *testing.T) {
	store := newTestStore(t)
	raw := mustRaw(t, `{"name":"原名","api_key":"ck_duplicate_key_1234","refresh_token":"rt1"}`)

	if r, err := store.ImportAccounts(ImportRequest{Data: raw}); err != nil || r.Imported != 1 {
		t.Fatalf("首次导入失败: %+v %v", r, err)
	}
	r, err := store.ImportAccounts(ImportRequest{Data: raw})
	if err != nil {
		t.Fatalf("重复导入: %v", err)
	}
	if r.Skipped != 1 || r.Imported != 0 {
		t.Errorf("重复凭证默认应跳过，得到 %+v", r)
	}

	updated := mustRaw(t, `{"name":"改名","api_key":"ck_duplicate_key_1234","refresh_token":"rt2"}`)
	r, err = store.ImportAccounts(ImportRequest{Data: updated, Overwrite: true})
	if err != nil {
		t.Fatalf("覆盖导入: %v", err)
	}
	if r.Updated != 1 {
		t.Errorf("overwrite 应更新，得到 %+v", r)
	}
	accounts, _ := store.ListAccounts()
	if len(accounts) != 1 {
		t.Errorf("覆盖不该产生重复账号，现有 %d 个", len(accounts))
	}
	if accounts[0].Name != "改名" {
		t.Errorf("覆盖后名称应更新，得到 %q", accounts[0].Name)
	}
}

// 导入源里没带的字段才用默认值，自带的不能被覆盖。
func TestImport_DefaultsDoNotOverrideSource(t *testing.T) {
	store := newTestStore(t)
	raw := mustRaw(t, `{"api_key":"ck_has_own_config_1234","request_shape":"cn"}`)
	_, err := store.ImportAccounts(ImportRequest{
		Data:     raw,
		Defaults: ImportDefaults{RequestShape: ShapeIntl, Models: []string{"from-default"}},
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	accounts, _ := store.ListAccounts()
	if accounts[0].RequestShape != ShapeCN {
		t.Errorf("源里自带 cn，不该被默认值覆盖成 %q", accounts[0].RequestShape)
	}
	if len(accounts[0].Models) != 1 || accounts[0].Models[0] != "from-default" {
		t.Errorf("源里没带模型，应落到默认值，得到 %v", accounts[0].Models)
	}
}
