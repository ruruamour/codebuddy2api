package app

import (
	"strings"
	"sync"
)

// 账号类型（渠道）。
//
// 起因：能打哪些模型是「渠道」的属性，不是某个号的属性——所有 CN 订阅号能打的模型
// 完全一样，所有 intl 凭证号也一样。把模型集合写在账号上等于把同一份事实抄 N 遍：
// 上线一个新模型要改 N 个号，还不保证每个都抄对。CPA 的 openai-compatibility、
// sub2api 的渠道都是按「一类上游」组织的，这里也一样：
//
//	账号类型 = 上游地址 + 认证方式 + 请求整形 + 这一类能提供的模型
//	账号     = 一份凭证 + 属于哪个类型
//
// 账号上的 auth_mode/upstream_url/domain/request_shape/models 覆盖字段保留，但降级为
// 「个别号特殊」的逃生口，不再是常规配置手段。解析优先级：
//
//	账号覆盖 > 类型 > 实例 env
//
// 老库里的账号 type_slug 为空、覆盖字段也为空，于是整条链落到实例 env——行为和加
// 类型之前逐字一致，这是这次改造不能破坏的底线。

// AccountType 是一类账号共享的上游配置与模型集合。
type AccountType struct {
	Slug         string   `json:"slug"` // 稳定标识，导入导出跨实例靠它对齐
	Name         string   `json:"name"`
	AuthMode     string   `json:"auth_mode"`
	UpstreamURL  string   `json:"upstream_url"`
	Domain       string   `json:"domain"`
	RequestShape string   `json:"request_shape"`
	Models       []string `json:"models"`
	Priority     int      `json:"priority"` // 叠加到账号优先级上，让免费渠道整体排在付费渠道前面
	Enabled      bool     `json:"enabled"`
	Builtin      bool     `json:"builtin"` // 内置类型不可删，改还是能改
	Notes        string   `json:"notes"`
	CreatedAt    int64    `json:"created_at"`
	UpdatedAt    int64    `json:"updated_at"`

	Accounts int `json:"accounts"` // 面板展示用的账号数，不持久化
}

// 内置类型。装机即用，同时也是「官方三个域名各自怎么配」这份知识的唯一出处——
// 以前它散在 portable.go 的 domainPresets 和面板的下拉框里，两处各写一遍。
const (
	TypeSlugCN        = "cn"
	TypeSlugIntl      = "intl"
	TypeSlugWorkBuddy = "workbuddy"
)

// freeIntlModels 是 intl 侧账号默认提供的模型。
//
// 2026-09-12 实测（逐个打、看 usage.credit，要求 tokens>0 以避免空响应误判）：
//
//	deepseek-v4.1-flash  credit=0  促销，注册表标 promoFreeUntil 2026-09-24
//	hy3                  credit=0  192K 上下文
//	hy4-preview-f        credit=0  1M 上下文，连打 15 次未触发限流
//
// 注意 hy4-preview（不带 -f）是收费的（同请求扣 0.05），别写错条目。
// 其余模型（glm-5.x / kimi-k2.x / minimax-m3 / gemini-3.5-flash 等）在 intl 都扣积分，
// 想要更广的模型面就靠国内订阅类型去接。
var freeIntlModels = []string{
	"deepseek-v4.1-flash",
	"hy3",
	"hy4-preview-f",
}

func builtinAccountTypes() []AccountType {
	return []AccountType{
		{
			Slug:         TypeSlugCN,
			Name:         "CodeBuddy 国内订阅",
			AuthMode:     AuthModeAPIKey,
			UpstreamURL:  "https://copilot.tencent.com/v2/chat/completions",
			Domain:       "www.codebuddy.cn",
			RequestShape: ShapeCN,
			Models:       defaultCNTypeModels(),
			Priority:     0,
			Enabled:      true,
			Builtin:      true,
			Notes:        "ck_ 开头的订阅 key，按积分计费，余额耗尽会自动暂停",
		},
		{
			Slug:         TypeSlugIntl,
			Name:         "CodeBuddy 国际版",
			AuthMode:     AuthModeBearer,
			UpstreamURL:  "https://www.codebuddy.ai/v2/chat/completions",
			Domain:       "www.codebuddy.ai",
			RequestShape: ShapeIntl,
			Models:       freeIntlModels,
			Priority:     100,
			Enabled:      true,
			Builtin:      true,
			Notes:        "OAuth 凭证号，deepseek-v4.1-flash 限免至 2026-09-24；hy3 / hy4-preview-f 实测长期免费。优先级高于国内订阅，免费的先用完再落到付费号",
		},
		{
			Slug:         TypeSlugWorkBuddy,
			Name:         "WorkBuddy 国际版",
			AuthMode:     AuthModeBearer,
			UpstreamURL:  "https://www.workbuddy.ai/v2/chat/completions",
			Domain:       "www.workbuddy.ai",
			RequestShape: ShapeIntl,
			Models:       freeIntlModels,
			Priority:     100,
			Enabled:      false,
			Builtin:      true,
			Notes:        "与国际版同一套协议，换个域名",
		},
	}
}

// defaultCNTypeModels 取模型表里的全部 CN 模型作为国内订阅类型的默认集合。
func defaultCNTypeModels() []string {
	out := make([]string, 0, len(CodeBuddyModelCatalog))
	for _, item := range CodeBuddyModelCatalog {
		out = append(out, item.ID)
	}
	return out
}

// TypeRegistry 是账号类型的进程内缓存。
//
// ProfileFor 在每次转发、每次查余额、每次续期时都会走一遍，不能每次都查库；
// 类型只在面板改动时才变，所以整张表缓存住，写入后由 Store 重新灌一次。
type TypeRegistry struct {
	mu    sync.RWMutex
	types map[string]AccountType
	order []string
}

func NewTypeRegistry() *TypeRegistry {
	return &TypeRegistry{types: map[string]AccountType{}}
}

// Replace 整表替换（类型很少，几十条顶天，不值得做增量）。
func (r *TypeRegistry) Replace(list []AccountType) {
	if r == nil {
		return
	}
	types := make(map[string]AccountType, len(list))
	order := make([]string, 0, len(list))
	for _, item := range list {
		types[item.Slug] = item
		order = append(order, item.Slug)
	}
	r.mu.Lock()
	r.types, r.order = types, order
	r.mu.Unlock()
}

func (r *TypeRegistry) Get(slug string) (AccountType, bool) {
	if r == nil {
		return AccountType{}, false
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return AccountType{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	item, ok := r.types[slug]
	return item, ok
}

func (r *TypeRegistry) List() []AccountType {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]AccountType, 0, len(r.order))
	for _, slug := range r.order {
		out = append(out, r.types[slug])
	}
	return out
}

// TypeOf 取账号所属类型；没归类或类型已被删掉时返回 nil（回落实例配置）。
//
// 停用的类型也照样返回：停用管的是「还调不调度它的号」，不是「它的号怎么配」。
// 面板上那些停用类型的号仍要显示正确的上游、诊断也要能跑。
func (r *TypeRegistry) TypeOf(account Account) *AccountType {
	item, ok := r.Get(account.TypeSlug.String)
	if !ok {
		return nil
	}
	return &item
}

// TypeModels 返回类型声明的模型集合；没声明返回 nil（表示不限）。
func (t *AccountType) TypeModels() []string {
	if t == nil {
		return nil
	}
	return t.Models
}

// servesModel 判断「账号 + 它的类型」能不能承接某个模型。
//
// 依据顺序和配置解析一致：账号自己声明了就以账号为准（逃生口），否则看类型；
// 两者都没声明才是「不限」——老库里的号正是这种，行为和加类型之前一样。
func servesModel(account Account, typ *AccountType, model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return true
	}
	list := account.ModelList()
	if len(list) == 0 {
		list = typ.TypeModels()
	}
	if len(list) == 0 {
		return true
	}
	for _, item := range list {
		if strings.EqualFold(strings.TrimSpace(item), model) {
			return true
		}
	}
	return false
}

// effectivePriority 是账号在池子里的实际优先级：类型优先级叠加到账号优先级上。
//
// 相加而不是二选一，是为了两级都有意义：类型级决定「免费渠道整体排在付费渠道前面」，
// 账号级决定「同一个渠道内部谁先上」。若是二选一，改了类型优先级就会把渠道内部
// 精心排好的顺序一把抹平。
func effectivePriority(account Account, typ *AccountType) int {
	if typ == nil {
		return account.Priority
	}
	return account.Priority + typ.Priority
}

// NormalizeTypeSlug 收敛 slug 写法：小写、空格转连字符、去掉路径分隔符和控制字符。
//
// 保留中日韩等非 ASCII 字母：这个字段会被当成类型的名字直接敲进来，只留 [a-z0-9]
// 的话「自定义渠道」会被整段吃掉、变成一句莫名其妙的「类型标识不能为空」。
// slug 只出现在 URL 路径里，面板负责 encodeURIComponent，Go 拿到的是解码后的路径。
func NormalizeTypeSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	for _, r := range value {
		switch {
		case r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ' || r == '.' || r == '/' || r == '\\':
			b.WriteRune('-')
		case r < 0x20 || r == 0x7f: // 控制字符直接丢
		case r < 0x80:
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
				b.WriteRune(r)
			}
		default:
			b.WriteRune(r)
		}
	}
	return strings.Trim(b.String(), "-")
}

// DetectTypeSlug 按凭证形态和上游猜账号该归哪个类型，用于老库迁移和导入。
// 猜不出来返回空串——宁可不归类（回落实例配置，行为不变），也不要归错。
func DetectTypeSlug(credential string, domain string, upstreamURL string) string {
	host := strings.ToLower(strings.TrimSpace(domain))
	if host == "" {
		host = strings.ToLower(HostOfURL(upstreamURL))
	}
	switch {
	case strings.Contains(host, "workbuddy.ai"):
		return TypeSlugWorkBuddy
	case strings.Contains(host, "codebuddy.ai"):
		return TypeSlugIntl
	case strings.Contains(host, "codebuddy.cn"), strings.Contains(host, "copilot.tencent.com"):
		return TypeSlugCN
	}
	switch DetectAuthMode(credential) {
	case AuthModeAPIKey:
		return TypeSlugCN
	case AuthModeBearer:
		return TypeSlugIntl
	}
	return ""
}
