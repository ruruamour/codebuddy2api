package app

import (
	"encoding/json"
	"fmt"
	"strings"
)

// 凭证的导出与导入。
//
// OAuth 凭证必须 access + refresh 成对才有意义：只导 access 的话，导进来的号全是
// 「365 天后静默失效」的残废号——正好是自助续期要解决的那个问题，会从导入口漏回来。
// 所以导出一定带 refresh token，导入也一定认 refresh token。
//
// 导出是明文：面板本身在 Cloudflare Access 后面，且导出的文件要能直接再导回去。

// PortableAccount 是一个可搬运的账号快照。字段名与导入时认的别名保持一致。
type PortableAccount struct {
	Name           string   `json:"name"`
	APIKey         string   `json:"api_key"`
	RefreshToken   string   `json:"refresh_token,omitempty"`
	TokenExpiresAt int64    `json:"token_expires_at,omitempty"`
	AuthMode       string   `json:"auth_mode,omitempty"`
	UpstreamURL    string   `json:"upstream_url,omitempty"`
	Domain         string   `json:"domain,omitempty"`
	RequestShape   string   `json:"request_shape,omitempty"`
	Models         []string `json:"models,omitempty"`
	TypeSlug       string   `json:"type_slug,omitempty"`
	ProxyURL       string   `json:"proxy_url,omitempty"`
	Notes          string   `json:"notes,omitempty"`
	Enabled        bool     `json:"enabled"`
	Priority       int      `json:"priority,omitempty"`
	Weight         int      `json:"weight,omitempty"`
	Concurrency    int      `json:"concurrency,omitempty"`
	QuotaLimit     float64  `json:"quota_limit,omitempty"`
}

// PortableBundle 是导出文件的整体结构，也能原样导回去。
type PortableBundle struct {
	Version    int               `json:"version"`
	ExportedAt int64             `json:"exported_at"`
	Accounts   []PortableAccount `json:"accounts"`
	// Types 是账号类型（渠道）定义。不带上它，备份恢复出来的号只剩一个 type_slug
	// 指向不存在的类型，于是集体回落到实例配置——上游可能整个变掉。
	Types []AccountType `json:"types,omitempty"`
	// Models 是公共模型表。同理：类型引用的模型如果不在新库里，那些模型就没法启用。
	Models []CatalogModel `json:"models,omitempty"`
}

// ImportDefaults 是导入时对没有自带配置的账号套用的默认值。
type ImportDefaults struct {
	AuthMode     string   `json:"auth_mode"`
	UpstreamURL  string   `json:"upstream_url"`
	Domain       string   `json:"domain"`
	RequestShape string   `json:"request_shape"`
	Models       []string `json:"models"`
	TypeSlug     string   `json:"type_slug"`
	ProxyURL     string   `json:"proxy_url"`
	Enabled      *bool    `json:"enabled"`
}

// ImportRequest 同时支持三种来源：导出文件、随手粘的 token.json、按行写的文本。
type ImportRequest struct {
	// Data 接任意形态的 JSON：我们自己的导出文件、单个 token.json、或它们的数组。
	Data json.RawMessage `json:"data"`
	// Text 是按行写的文本，每行 1~3 段：
	//   <凭证> | <名称>,<凭证> | <名称>,<access>,<refresh>
	Text      string         `json:"text"`
	Defaults  ImportDefaults `json:"defaults"`
	Overwrite bool           `json:"overwrite"` // 同 api_key 已存在时是否覆盖
}

// ImportResult 汇报导入结果。
type ImportResult struct {
	Imported int      `json:"imported"`
	Updated  int      `json:"updated"`
	Skipped  int      `json:"skipped"`
	Total    int      `json:"total"`
	Errors   []string `json:"errors,omitempty"`
}

// 按 domain 反推接入配置。token.json 里自带 domain，据此就能把上游、整形一并填好，
// 不用用户自己去记 www.codebuddy.ai 对应哪套参数。
var domainPresets = map[string]struct{ upstream, shape string }{
	"www.codebuddy.ai":    {"https://www.codebuddy.ai/v2/chat/completions", ShapeIntl},
	"www.workbuddy.ai":    {"https://www.workbuddy.ai/v2/chat/completions", ShapeIntl},
	"www.codebuddy.cn":    {"https://copilot.tencent.com/v2/chat/completions", ShapeCN},
	"copilot.tencent.com": {"https://copilot.tencent.com/v2/chat/completions", ShapeCN},
}

// ExportAccounts 导出全部账号（含完整凭证），产出的 bundle 能原样导回。
func (s *Store) ExportAccounts() (PortableBundle, error) {
	rows, err := s.db.Query(`
SELECT name, api_key, COALESCE(refresh_token, ''), COALESCE(token_expires_at, 0),
       COALESCE(auth_mode, ''), COALESCE(upstream_url, ''), COALESCE(domain, ''),
       COALESCE(request_shape, ''), COALESCE(models, ''), COALESCE(type_slug, ''),
       COALESCE(proxy_url, ''),
       COALESCE(notes, ''), enabled, priority, weight, concurrency, quota_limit
FROM accounts ORDER BY id ASC`)
	if err != nil {
		return PortableBundle{}, err
	}
	defer rows.Close()

	bundle := PortableBundle{Version: 1, ExportedAt: now(), Accounts: []PortableAccount{}}
	for rows.Next() {
		var a PortableAccount
		var models string
		var enabled int
		if err := rows.Scan(&a.Name, &a.APIKey, &a.RefreshToken, &a.TokenExpiresAt,
			&a.AuthMode, &a.UpstreamURL, &a.Domain, &a.RequestShape, &models, &a.TypeSlug,
			&a.ProxyURL, &a.Notes, &enabled, &a.Priority, &a.Weight, &a.Concurrency,
			&a.QuotaLimit); err != nil {
			return PortableBundle{}, err
		}
		a.Enabled = enabled == 1
		a.Models = decodeModelList(models)
		bundle.Accounts = append(bundle.Accounts, a)
	}
	if err := rows.Err(); err != nil {
		return PortableBundle{}, err
	}

	// 渠道定义和模型表一起带走：账号上只存一个 type_slug，没有这两张表，
	// 恢复出来的号会指向一个不存在的类型、集体回落到实例配置。
	types, err := s.ListAccountTypes()
	if err != nil {
		return PortableBundle{}, err
	}
	bundle.Types = types
	catalog, err := s.ListCatalog()
	if err != nil {
		return PortableBundle{}, err
	}
	bundle.Models = catalog
	return bundle, nil
}

// ImportAccounts 导入账号。同 api_key 的按 overwrite 决定覆盖还是跳过。
func (s *Store) ImportAccounts(req ImportRequest) (ImportResult, error) {
	items, err := collectImportItems(req)
	if err != nil {
		return ImportResult{}, err
	}
	// 先恢复模型表和类型，再导账号：账号的 type_slug 得有对应的类型才有意义。
	// 已存在的同名类型/模型不动——导入是补充，不是拿备份把现有配置整个盖掉。
	result := ImportResult{Total: len(items)}
	if err := s.importCatalogAndTypes(req, &result); err != nil {
		return result, err
	}
	for i, item := range items {
		applyImportDefaults(&item, req.Defaults)
		if strings.TrimSpace(item.Name) == "" {
			item.Name = fmt.Sprintf("导入账号 %d", i+1)
		}
		existing, err := s.accountIDByAPIKey(item.APIKey)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", item.Name, err))
			continue
		}
		if existing > 0 {
			if !req.Overwrite {
				result.Skipped++
				continue
			}
			if err := s.applyPortableToAccount(existing, item); err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", item.Name, err))
				continue
			}
			result.Updated++
			continue
		}
		if _, err := s.AddAccount(item.toCreate()); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", item.Name, err))
			continue
		}
		result.Imported++
	}
	return result, nil
}

func (a PortableAccount) toCreate() AccountCreate {
	create := AccountCreate{
		Name:    a.Name,
		APIKey:  a.APIKey,
		Enabled: &a.Enabled,
		Models:  a.Models,
	}
	if a.RefreshToken != "" {
		create.RefreshToken = &a.RefreshToken
	}
	if a.TokenExpiresAt > 0 {
		create.TokenExpiresAt = &a.TokenExpiresAt
	}
	for _, f := range []struct {
		value string
		dest  **string
	}{
		{a.AuthMode, &create.AuthMode},
		{a.UpstreamURL, &create.UpstreamURL},
		{a.Domain, &create.Domain},
		{a.RequestShape, &create.RequestShape},
		{a.TypeSlug, &create.TypeSlug},
		{a.ProxyURL, &create.ProxyURL},
		{a.Notes, &create.Notes},
	} {
		if f.value != "" {
			v := f.value
			*f.dest = &v
		}
	}
	if a.Priority > 0 {
		create.Priority = &a.Priority
	}
	if a.Weight > 0 {
		create.Weight = &a.Weight
	}
	if a.Concurrency > 0 {
		create.Concurrency = &a.Concurrency
	}
	if a.QuotaLimit > 0 {
		create.QuotaLimit = &a.QuotaLimit
	}
	return create
}

func (s *Store) accountIDByAPIKey(apiKey string) (int64, error) {
	var id int64
	err := s.db.QueryRow("SELECT id FROM accounts WHERE api_key = ? LIMIT 1", apiKey).Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return 0, nil
		}
		return 0, err
	}
	return id, nil
}

// applyPortableToAccount 用导入项覆盖已有账号（overwrite 模式）。
func (s *Store) applyPortableToAccount(id int64, a PortableAccount) error {
	patch := AccountPatch{Name: &a.Name, Models: a.Models, Enabled: &a.Enabled}
	for _, f := range []struct {
		value string
		dest  **string
	}{
		{a.RefreshToken, &patch.RefreshToken},
		{a.AuthMode, &patch.AuthMode},
		{a.UpstreamURL, &patch.UpstreamURL},
		{a.Domain, &patch.Domain},
		{a.RequestShape, &patch.RequestShape},
		{a.ProxyURL, &patch.ProxyURL},
		{a.Notes, &patch.Notes},
	} {
		if f.value != "" {
			v := f.value
			*f.dest = &v
		}
	}
	if a.TokenExpiresAt > 0 {
		patch.TokenExpiresAt = &a.TokenExpiresAt
	}
	_, err := s.PatchAccount(id, patch)
	return err
}

// applyImportDefaults 补齐导入项没自带的配置：先按 domain 反推，再落到用户给的默认值。
func applyImportDefaults(a *PortableAccount, d ImportDefaults) {
	if a.AuthMode == "" {
		a.AuthMode = DetectAuthMode(a.APIKey) // eyJ -> bearer，ck_ -> api_key
	}
	// token.json 自带 domain 时，上游与整形都能据此推出来
	if preset, ok := domainPresets[strings.ToLower(strings.TrimSpace(a.Domain))]; ok {
		if a.UpstreamURL == "" {
			a.UpstreamURL = preset.upstream
		}
		if a.RequestShape == "" {
			a.RequestShape = preset.shape
		}
	}
	if a.AuthMode == "" {
		a.AuthMode = d.AuthMode
	}
	if a.UpstreamURL == "" {
		a.UpstreamURL = d.UpstreamURL
	}
	if a.Domain == "" {
		a.Domain = d.Domain
	}
	if a.RequestShape == "" {
		a.RequestShape = d.RequestShape
	}
	if a.TypeSlug == "" {
		a.TypeSlug = d.TypeSlug
	}
	// 类型能推出来时，账号级覆盖就不必再写一遍——写了反而以后改类型时改不动它
	if a.TypeSlug == "" {
		a.TypeSlug = DetectTypeSlug(a.APIKey, a.Domain, a.UpstreamURL)
	}
	if a.ProxyURL == "" {
		a.ProxyURL = d.ProxyURL
	}
	if len(a.Models) == 0 {
		a.Models = d.Models
	}
	if d.Enabled != nil {
		a.Enabled = *d.Enabled
	}
}

// collectImportItems 把请求里的各种来源统一成账号列表。
func collectImportItems(req ImportRequest) ([]PortableAccount, error) {
	var items []PortableAccount
	if len(req.Data) > 0 && strings.TrimSpace(string(req.Data)) != "null" {
		var blob any
		if err := json.Unmarshal(req.Data, &blob); err != nil {
			return nil, fmt.Errorf("JSON 解析失败: %w", err)
		}
		items = append(items, extractPortableAccounts(blob)...)
	}
	items = append(items, parseImportLines(req.Text)...)
	return dedupePortable(items), nil
}

// parseImportLines 解析按行写的文本。
//
//	<凭证>
//	<名称>,<凭证>
//	<名称>,<access>,<refresh>
//
// 两段时靠「第二段像不像凭证」来区分是 名称,凭证 还是 access,refresh。
func parseImportLines(text string) []PortableAccount {
	var items []PortableAccount
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		var item PortableAccount
		switch len(parts) {
		case 1:
			item.APIKey = parts[0]
		case 2:
			if looksLikeCredential(parts[0]) && looksLikeCredential(parts[1]) {
				item.APIKey, item.RefreshToken = parts[0], parts[1]
			} else {
				item.Name, item.APIKey = parts[0], parts[1]
			}
		default:
			item.Name, item.APIKey, item.RefreshToken = parts[0], parts[1], parts[2]
		}
		if item.APIKey != "" {
			item.Enabled = true
			items = append(items, item)
		}
	}
	return items
}

func looksLikeCredential(v string) bool {
	return strings.HasPrefix(v, "ck_") || strings.HasPrefix(v, "eyJ")
}

// extractPortableAccounts 从任意形态的 JSON 里把账号挖出来。
//
// 认这些形态：我们自己的导出文件、官方登录产出的 token.json、
// 以及它们套在 accounts/data/items 下的数组。
func extractPortableAccounts(value any) []PortableAccount {
	var out []PortableAccount
	switch node := value.(type) {
	case []any:
		for _, child := range node {
			out = append(out, extractPortableAccounts(child)...)
		}
	case map[string]any:
		// 先钻进已知的容器字段
		for _, key := range []string{"accounts", "data", "items", "list"} {
			if child, ok := node[key]; ok {
				if _, isString := child.(string); !isString {
					out = append(out, extractPortableAccounts(child)...)
				}
			}
		}
		if item := extractOnePortable(node); item != nil {
			out = append(out, *item)
		}
	}
	return out
}

func extractOnePortable(node map[string]any) *PortableAccount {
	auth, _ := node["auth"].(map[string]any)
	apiKey := pickStr(node["api_key"], node["apiKey"], node["accessToken"], node["access_token"],
		node["jwt"], node["token"], mapStr(auth, "accessToken"), mapStr(auth, "access_token"))
	if apiKey == "" {
		return nil
	}
	item := &PortableAccount{
		Name: pickStr(node["name"], node["nickname"], node["label"], node["remark"]),
		RefreshToken: pickStr(node["refresh_token"], node["refreshToken"],
			mapStr(auth, "refreshToken"), mapStr(auth, "refresh_token")),
		AuthMode:     pickStr(node["auth_mode"], node["authMode"]),
		UpstreamURL:  pickStr(node["upstream_url"], node["upstreamUrl"], node["base_url"], node["baseUrl"]),
		Domain:       pickStr(node["domain"], mapStr(auth, "domain")),
		RequestShape: pickStr(node["request_shape"], node["requestShape"]),
		ProxyURL:     pickStr(node["proxy_url"], node["proxyUrl"], node["proxy"]),
		Notes:        pickStr(node["notes"], node["note"]),
		APIKey:       apiKey,
		Enabled:      true,
	}
	if enabled, ok := node["enabled"].(bool); ok {
		item.Enabled = enabled
	}
	item.TokenExpiresAt = pickInt(node["token_expires_at"], node["tokenExpiresAt"])
	// token.json 给的是 expiresIn（相对秒数），换算成绝对时间戳
	if item.TokenExpiresAt == 0 {
		if in := pickInt(node["expiresIn"], node["expires_in"]); in > 0 {
			item.TokenExpiresAt = now() + in
		}
	}
	item.Models = pickStrList(node["models"], node["model"])
	item.Priority = int(pickInt(node["priority"]))
	item.Weight = int(pickInt(node["weight"]))
	item.Concurrency = int(pickInt(node["concurrency"]))
	if q, ok := node["quota_limit"].(float64); ok {
		item.QuotaLimit = q
	}
	return item
}

func pickStr(values ...any) string {
	for _, v := range values {
		if s, ok := v.(string); ok {
			if s = strings.TrimSpace(s); s != "" {
				return s
			}
		}
	}
	return ""
}

func pickInt(values ...any) int64 {
	for _, v := range values {
		switch n := v.(type) {
		case float64:
			if n > 0 {
				return int64(n)
			}
		case int64:
			if n > 0 {
				return n
			}
		}
	}
	return 0
}

func pickStrList(values ...any) []string {
	for _, v := range values {
		switch node := v.(type) {
		case []any:
			var out []string
			for _, item := range node {
				if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, strings.TrimSpace(s))
				}
			}
			if len(out) > 0 {
				return out
			}
		case string:
			if list := splitCommaList(node); len(list) > 0 {
				return list
			}
		}
	}
	return nil
}

func splitCommaList(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func mapStr(m map[string]any, key string) any {
	if m == nil {
		return nil
	}
	return m[key]
}

// dedupePortable 按 api_key 去重，后出现的补齐前面缺的字段。
func dedupePortable(items []PortableAccount) []PortableAccount {
	index := map[string]int{}
	out := make([]PortableAccount, 0, len(items))
	for _, item := range items {
		if item.APIKey == "" {
			continue
		}
		if at, ok := index[item.APIKey]; ok {
			if out[at].RefreshToken == "" {
				out[at].RefreshToken = item.RefreshToken
			}
			if out[at].Name == "" {
				out[at].Name = item.Name
			}
			continue
		}
		index[item.APIKey] = len(out)
		out = append(out, item)
	}
	return out
}

// importCatalogAndTypes 从导入数据里恢复模型表和账号类型。
//
// 只补不覆盖：导入常见的用法是「把另一台机器的号搬过来」，那台机器的渠道配置不该
// 把本机已经调好的覆盖掉。真要改配置有面板，不该藏在导入里发生。
func (s *Store) importCatalogAndTypes(req ImportRequest, result *ImportResult) error {
	if len(req.Data) == 0 {
		return nil
	}
	var bundle PortableBundle
	if err := json.Unmarshal(req.Data, &bundle); err != nil {
		return nil // 不是我们的导出格式（token.json、数组等），没有这两张表可恢复
	}
	for _, model := range bundle.Models {
		exists, err := s.catalogRowExists(model.ID)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if err := s.UpsertCatalogModel(model); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("模型 %s: %v", model.ID, err))
		}
	}
	for _, typ := range bundle.Types {
		existing, err := s.GetAccountType(NormalizeTypeSlug(typ.Slug))
		if err != nil {
			return err
		}
		if existing != nil {
			continue
		}
		typ.Builtin = false // 别人的内置标记在本机不算数
		if err := s.UpsertAccountType(typ); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("类型 %s: %v", typ.Slug, err))
		}
	}
	return nil
}
