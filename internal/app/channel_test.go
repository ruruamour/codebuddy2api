package app

import (
	"database/sql"
	"reflect"
	"testing"
)

// 迁移不能改变对外行为——这是整次改造的底线。
//
// 老库里「启用哪些模型」存在 settings 一行 JSON 里；改造后改由模型表的 enabled 列
// 决定。如果种子取的是代码里的默认值而不是库里当前的值，/v1/models 就会在一次
// 重启后悄悄多出或少掉模型，下游按老列表发的请求会突然 400。
func TestBootstrap_KeepsAdvertisedModelsIdentical(t *testing.T) {
	store := newTestStore(t)
	cfg := cnConfig()

	// 模拟老库：面板上曾经改过启用列表，落在 settings 那行里
	before, err := store.SaveModelSettings(ModelSettings{
		Models:       []string{"glm-5.1", "deepseek-v4.1-flash", "kimi-k2.6"},
		DefaultModel: "glm-5.1",
		PoolStrategy: PoolStrategyRoundRobin,
	}, cfg.Models, cfg.PoolStrategy)
	if err != nil {
		t.Fatalf("save legacy settings: %v", err)
	}

	if err := store.Bootstrap(cfg); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	after, err := store.ModelSettings(cfg.Models, cfg.PoolStrategy)
	if err != nil {
		t.Fatalf("model settings: %v", err)
	}
	if !reflect.DeepEqual(before.Models, after.Models) {
		t.Errorf("迁移改变了对外模型列表\n迁移前 %v\n迁移后 %v", before.Models, after.Models)
	}
	if after.DefaultModel != before.DefaultModel {
		t.Errorf("默认模型变了：%q -> %q", before.DefaultModel, after.DefaultModel)
	}

	// 启用列表之外的模型也要在表里留着（停用状态），否则面板上再想开就找不到了
	catalog, err := store.ListCatalog()
	if err != nil {
		t.Fatalf("list catalog: %v", err)
	}
	if len(catalog) <= len(before.Models) {
		t.Errorf("模型表只有 %d 行，应当把种子模型也收进来（停用状态）", len(catalog))
	}
}

// Bootstrap 要幂等：服务每次启动都会跑，不能每次都重新灌一遍把人改过的配置冲掉。
func TestBootstrap_Idempotent(t *testing.T) {
	store := newTestStore(t)
	cfg := cnConfig()
	if err := store.Bootstrap(cfg); err != nil {
		t.Fatalf("bootstrap 1: %v", err)
	}
	// 操作者改了内置类型的优先级
	typ, err := store.GetAccountType(TypeSlugIntl)
	if err != nil || typ == nil {
		t.Fatalf("get type: %v", err)
	}
	typ.Priority = 777
	if err := store.UpsertAccountType(*typ); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if err := store.Bootstrap(cfg); err != nil {
		t.Fatalf("bootstrap 2: %v", err)
	}
	again, err := store.GetAccountType(TypeSlugIntl)
	if err != nil || again == nil {
		t.Fatalf("get type again: %v", err)
	}
	if again.Priority != 777 {
		t.Errorf("第二次 Bootstrap 把手改的优先级冲掉了：%d", again.Priority)
	}
}

// 老账号（没归类、没覆盖）必须继续完全按实例配置走。
func TestServesModel_UnclassifiedAccountServesEverything(t *testing.T) {
	account := Account{ID: 1, Name: "老 ck 号"}
	if !servesModel(account, nil, "随便什么模型") {
		t.Error("未归类账号应当不限模型，否则老库一升级就全线 no_account_for_model")
	}
}

// 模型集合由类型给出：账号自己没声明时，类型说了算。
func TestServesModel_FromType(t *testing.T) {
	intl := &AccountType{Slug: TypeSlugIntl, Models: []string{"deepseek-v4.1-flash"}}
	account := Account{ID: 1}

	if !servesModel(account, intl, "deepseek-v4.1-flash") {
		t.Error("类型声明了这个模型，账号应当能接")
	}
	if servesModel(account, intl, "glm-5.1") {
		t.Error("类型没声明 glm-5.1，intl 凭证号不该接——它打不了")
	}
}

// 账号自己声明了就以账号为准，这是「个别号特殊」的逃生口。
func TestServesModel_AccountOverridesType(t *testing.T) {
	cn := &AccountType{Slug: TypeSlugCN, Models: []string{"glm-5.1", "kimi-k2.6"}}
	account := Account{ID: 1, Models: sql.NullString{String: `["glm-5.1"]`, Valid: true}}

	if servesModel(account, cn, "kimi-k2.6") {
		t.Error("账号只声明了 glm-5.1，不该被类型的模型集合放大")
	}
	if !servesModel(account, cn, "glm-5.1") {
		t.Error("账号声明的模型应当能接")
	}
}

// 解析链：账号覆盖 > 类型 > 实例。
func TestResolveProfile_TypeSitsBetweenAccountAndInstance(t *testing.T) {
	cfg := cnConfig() // 实例是 CN：api_key + copilot.tencent.com
	intl := &AccountType{
		Slug:         TypeSlugIntl,
		AuthMode:     AuthModeBearer,
		UpstreamURL:  "https://www.codebuddy.ai/v2/chat/completions",
		RequestShape: ShapeIntl,
		Models:       []string{"deepseek-v4.1-flash"},
	}

	// 只归类、不覆盖：整条链应当落到类型上
	got := resolveProfile(Account{ID: 1}, intl, cfg)
	if !got.IsBearer() {
		t.Errorf("类型是 bearer，实例是 api_key，应当以类型为准，得到 %q", got.AuthMode)
	}
	if got.UpstreamURL != intl.UpstreamURL {
		t.Errorf("上游应当来自类型，得到 %q", got.UpstreamURL)
	}
	if got.Domain != "www.codebuddy.ai" {
		t.Errorf("类型没写 domain 时应从上游主机推导，得到 %q", got.Domain)
	}
	if got.RequestShape != ShapeIntl {
		t.Errorf("整形应当来自类型，得到 %q", got.RequestShape)
	}

	// 账号覆盖压过类型
	account := Account{
		ID:          2,
		UpstreamURL: sql.NullString{String: "https://www.workbuddy.ai/v2/chat/completions", Valid: true},
	}
	got = resolveProfile(account, intl, cfg)
	if got.UpstreamURL != "https://www.workbuddy.ai/v2/chat/completions" {
		t.Errorf("账号覆盖没有压过类型，得到 %q", got.UpstreamURL)
	}
	if got.Domain != "www.workbuddy.ai" {
		t.Errorf("账号换了上游、没写 domain，应当跟着推导，得到 %q", got.Domain)
	}
	if !got.IsBearer() {
		t.Error("账号只覆盖了上游，认证方式应当继续用类型的 bearer")
	}
}

// 类型优先级叠加在账号优先级上：改渠道优先级不能抹平渠道内部的排序。
func TestEffectivePriority_TypeStacksOnAccount(t *testing.T) {
	free := &AccountType{Slug: TypeSlugIntl, Priority: 100}
	paid := &AccountType{Slug: TypeSlugCN, Priority: 0}

	freeAccount := Account{ID: 1, Priority: 10}
	paidFirst := Account{ID: 2, Priority: 90}
	paidSecond := Account{ID: 3, Priority: 50}

	if effectivePriority(freeAccount, free) <= effectivePriority(paidFirst, paid) {
		t.Error("免费渠道整体应当排在付费渠道前面")
	}
	if effectivePriority(paidFirst, paid) <= effectivePriority(paidSecond, paid) {
		t.Error("同一渠道内部仍应按账号优先级排序")
	}
	if effectivePriority(freeAccount, nil) != 10 {
		t.Error("未归类账号的优先级就是它自己的")
	}
}

// 免费渠道优先：deepseek 请求先给 intl 凭证号，付费的 ck_ 号只当溢出容量。
//
// 这是合并两个实例之后最容易出错的地方——两类号都能打这个模型，轮询会让付费号
// 白白烧积分，而免费额度在旁边闲着。
func TestPool_FreeTypeWinsForSharedModel(t *testing.T) {
	store := newTestStore(t)
	registry := NewTypeRegistry()
	registry.Replace([]AccountType{
		{Slug: TypeSlugCN, Name: "国内订阅", Models: []string{"glm-5.1", "deepseek-v4.1-flash"},
			Priority: 0, Enabled: true},
		{Slug: TypeSlugIntl, Name: "国际版", Models: []string{"deepseek-v4.1-flash"},
			Priority: 100, Enabled: true},
	})

	cnID, err := store.AddAccount(AccountCreate{
		Name: "ck 号", APIKey: "ck_paid_account_credential", TypeSlug: strPtr(TypeSlugCN),
	})
	if err != nil {
		t.Fatalf("add cn: %v", err)
	}
	intlID, err := store.AddAccount(AccountCreate{
		Name: "凭证号", APIKey: makeJWT(t, 365), TypeSlug: strPtr(TypeSlugIntl),
	})
	if err != nil {
		t.Fatalf("add intl: %v", err)
	}

	pool := NewPool(store, registry, []string{"glm-5.1"}, PoolStrategyRoundRobin)

	// 两类都能接 deepseek，但免费的优先级更高
	lease, err := pool.Acquire("deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("acquire deepseek: %v", err)
	}
	if lease.Account.ID != intlID {
		t.Errorf("deepseek 应当先走免费的 intl 号 #%d，实际给了 #%d", intlID, lease.Account.ID)
	}
	pool.Release(lease)

	// glm-5.1 只有 CN 能接，不该被路由到凭证号上
	lease, err = pool.Acquire("glm-5.1")
	if err != nil {
		t.Fatalf("acquire glm: %v", err)
	}
	if lease.Account.ID != cnID {
		t.Errorf("glm-5.1 只有 CN 号能打，实际给了 #%d", lease.Account.ID)
	}
	pool.Release(lease)
}

// 免费号占满并发后要落到付费号，而不是直接报错——优先级是排序，不是排他。
func TestPool_FallsBackToPaidWhenFreeIsBusy(t *testing.T) {
	store := newTestStore(t)
	registry := NewTypeRegistry()
	registry.Replace([]AccountType{
		{Slug: TypeSlugCN, Models: []string{"deepseek-v4.1-flash"}, Priority: 0, Enabled: true},
		{Slug: TypeSlugIntl, Models: []string{"deepseek-v4.1-flash"}, Priority: 100, Enabled: true},
	})
	cnID, _ := store.AddAccount(AccountCreate{
		Name: "ck 号", APIKey: "ck_paid_account_credential", TypeSlug: strPtr(TypeSlugCN),
	})
	if _, err := store.AddAccount(AccountCreate{
		Name: "凭证号", APIKey: makeJWT(t, 365), TypeSlug: strPtr(TypeSlugIntl),
	}); err != nil {
		t.Fatalf("add intl: %v", err)
	}

	pool := NewPool(store, registry, nil, PoolStrategyRoundRobin)
	first, err := pool.Acquire("deepseek-v4.1-flash") // 免费号，并发 1，占住不放
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	second, err := pool.Acquire("deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("免费号占满后应当落到付费号，却报错: %v", err)
	}
	if second.Account.ID != cnID {
		t.Errorf("应当落到 CN 号 #%d，实际 #%d", cnID, second.Account.ID)
	}
	pool.Release(first)
	pool.Release(second)
}

// 停用类型等于把它名下的号整体下线，不用逐个关号。
func TestPool_DisabledTypeTakesItsAccountsOffline(t *testing.T) {
	store := newTestStore(t)
	registry := NewTypeRegistry()
	registry.Replace([]AccountType{
		{Slug: TypeSlugIntl, Models: []string{"deepseek-v4.1-flash"}, Enabled: false},
	})
	if _, err := store.AddAccount(AccountCreate{
		Name: "凭证号", APIKey: makeJWT(t, 365), TypeSlug: strPtr(TypeSlugIntl),
	}); err != nil {
		t.Fatalf("add: %v", err)
	}

	pool := NewPool(store, registry, nil, PoolStrategyRoundRobin)
	if _, err := pool.Acquire("deepseek-v4.1-flash"); err == nil {
		t.Error("类型已停用，它名下的账号不该被调度")
	}
}

// 归类要按凭证形态和上游来，猜不出来宁可不归。
func TestDetectTypeSlug(t *testing.T) {
	cases := []struct {
		credential, domain, upstream, want string
	}{
		{"ck_abcdefghijklmnop", "", "", TypeSlugCN},
		{"eyJhbGciOiJSUzI1NiJ9.eyJhIjoxfQ.sig", "", "", TypeSlugIntl},
		{"", "www.codebuddy.ai", "", TypeSlugIntl},
		{"", "www.workbuddy.ai", "", TypeSlugWorkBuddy},
		{"", "", "https://copilot.tencent.com/v2/chat/completions", TypeSlugCN},
		// 上游比凭证形态更可信：workbuddy 也是 JWT，但不是 codebuddy.ai
		{"eyJhbGciOiJSUzI1NiJ9.eyJhIjoxfQ.sig", "www.workbuddy.ai", "", TypeSlugWorkBuddy},
		{"某种没见过的凭证", "", "", ""},
	}
	for _, c := range cases {
		if got := DetectTypeSlug(c.credential, c.domain, c.upstream); got != c.want {
			t.Errorf("DetectTypeSlug(%q,%q,%q) = %q，应为 %q",
				c.credential, c.domain, c.upstream, got, c.want)
		}
	}
}

// 内置类型不能删；还挂着账号的类型也不能删——删了那些号会静默改走实例配置。
func TestDeleteAccountType_Guards(t *testing.T) {
	store := newTestStore(t)
	if err := store.Bootstrap(cnConfig()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := store.DeleteAccountType(TypeSlugCN); err == nil {
		t.Error("内置类型不该能删")
	}

	custom := AccountType{Slug: "自定义 渠道", Name: "自定义", Enabled: true}
	if err := store.UpsertAccountType(custom); err != nil {
		t.Fatalf("upsert custom: %v", err)
	}
	slug := NormalizeTypeSlug(custom.Slug)
	if _, err := store.AddAccount(AccountCreate{
		Name: "挂着的号", APIKey: "ck_attached_to_custom_type", TypeSlug: &slug,
	}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := store.DeleteAccountType(slug); err == nil {
		t.Error("类型下还有账号时不该能删")
	}
}

// 模型仍被某个类型引用时不能删，否则那个类型会指向一个不存在的模型。
func TestDeleteCatalogModel_RejectsReferenced(t *testing.T) {
	store := newTestStore(t)
	if err := store.Bootstrap(cnConfig()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := store.DeleteCatalogModel("deepseek-v4.1-flash"); err == nil {
		t.Error("intl 类型还在用这个模型，不该能删")
	}
}
