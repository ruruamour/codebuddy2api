package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// 从上游 /v3/config 同步模型表。
//
// 为什么要有这个：模型表是「我们对外提供什么」的唯一出处，但它的元信息
// （显示名、积分倍率、上下文长度、能不能看图）只有上游知道。上游每次换模型，
// 手抄一遍就会有漏项，而漏项的代价是面板上显示一个光秃秃的 ID 或者一个编出来的倍率。
//
// 两条硬约束：
//
//  1. **只更新元信息，不改 enabled。** 同步是「把事实抄下来」，上架/下架是运营决定。
//     把新模型顺手打开会让 /v1/models 在一次同步后凭空多出条目，下游按老列表做的
//     缓存与计费口径都会跟着漂。
//  2. **不删任何行。** 上游目录里消失的模型只做报告：它可能是临时故障，也可能真的
//     下架了（glm-4.6/glm-4.7 就是后者）。删行是不可逆的，报告给人看就够了。
//
// 认证：/v3/config 会校验 UA（普通 UA 得到 400 code 12403 "check ua, get coding
// copilot version error"）。IDE UA 与 CLI UA 返回的目录**不一样**（26 vs 37 个），
// 所以两个都拉、取并集——只拉一个会漏掉对方独有的一半。
const (
	configPath   = "/v3/config"
	ideUserAgent = "CodeBuddyIDE/5.9.3"
	cliConfigUA  = "CLI/2.149.0 CodeBuddy/2.149.0"
)

// upstreamModel 是 /v3/config 里 models[] 的一项（只取用得上的字段）。
type upstreamModel struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Credits          string   `json:"credits"`
	MaxInputTokens   int      `json:"maxInputTokens"`
	MaxOutputTokens  int      `json:"maxOutputTokens"`
	MaxAllowedSize   int      `json:"maxAllowedSize"`
	SupportsImages   bool     `json:"supportsImages"`
	SupportsToolCall *bool    `json:"supportsToolCall"`
	Tags             []string `json:"tags"`
	DescriptionZh    string   `json:"descriptionZh"`
}

// modelSyncReport 是一次同步的结果。面板直接展示它，所以字段要能自解释。
type modelSyncReport struct {
	OK        bool     `json:"ok"`
	Detail    string   `json:"detail,omitempty"`
	Fetched   int      `json:"fetched"`   // 上游目录里能用的 chat 模型数
	Created   []string `json:"created"`   // 新增进表（停用状态）
	Updated   []string `json:"updated"`   // 元信息有变化
	Unchanged int      `json:"unchanged"` // 无变化
	// Missing 是「表里有、上游目录里没有」的模型。不删，只报告：
	// 可能是上游临时故障，也可能真的下架了，判断该由人做。
	Missing []string `json:"missing"`
	Skipped []string `json:"skipped"` // 上游目录里的非 chat 模型（图像/视频/内部）

	// 以下仅重置模式（POST /admin/models/sync?reset=1）会填。
	Deleted     []string `json:"deleted,omitempty"`      // 从模型表删掉的行
	PrunedTypes []string `json:"pruned_types,omitempty"` // 被剪掉幽灵模型引用的类型
	Enabled     []string `json:"enabled,omitempty"`      // 重置后重新启用的模型
}

// SyncModelCatalog 从上游目录拉一次模型元信息并写回模型表。
//
// 用哪个账号去拉：任取一个启用的账号即可（目录是账号无关的，只受 UA 影响）。
// ck_ 号优先——它不是 JWT，不会因为 token 过期而拉不到。
func (s *Server) SyncModelCatalog(ctx context.Context) modelSyncReport {
	account, err := s.syncCredential()
	if err != nil {
		return modelSyncReport{OK: false, Detail: err.Error()}
	}

	models, err := s.upstream.FetchConfigModels(ctx, account)
	if err != nil {
		return modelSyncReport{OK: false, Detail: err.Error()}
	}

	live := make(map[string]upstreamModel, len(models))
	report := modelSyncReport{OK: true}
	for _, model := range models {
		if !isChatModel(model) {
			report.Skipped = append(report.Skipped, model.ID)
			continue
		}
		live[model.ID] = model
	}
	report.Fetched = len(live)
	sort.Strings(report.Skipped)

	existing, err := s.store.ListCatalog()
	if err != nil {
		return modelSyncReport{OK: false, Detail: err.Error()}
	}
	known := make(map[string]CatalogModel, len(existing))
	order := 0
	for _, item := range existing {
		known[item.ID] = item
		if item.SortOrder >= order {
			order = item.SortOrder + 1
		}
	}

	ids := make([]string, 0, len(live))
	for id := range live {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		model := live[id]
		previous, exists := known[id]
		// 新模型一律建为停用：上架要不要开是运营决定，同步只负责把事实抄下来。
		next := CatalogModel{
			ID:              id,
			Name:            firstNonEmpty(model.Name, id),
			Credits:         strings.TrimSpace(model.Credits),
			MaxInputTokens:  firstPositive(model.MaxInputTokens, model.MaxAllowedSize),
			MaxOutputTokens: model.MaxOutputTokens,
			SupportsImages:  model.SupportsImages,
			Enabled:         false,
			SortOrder:       order,
		}
		if exists {
			// 已有行：保号 enabled 与 sort_order，只覆盖上游给的元信息。
			next.Enabled = previous.Enabled
			next.SortOrder = previous.SortOrder
			if !catalogMetaChanged(previous, next) {
				report.Unchanged++
				continue
			}
			report.Updated = append(report.Updated, id)
		} else {
			order++
			report.Created = append(report.Created, id)
		}
		if err := s.store.UpsertCatalogModel(next); err != nil {
			return modelSyncReport{OK: false, Detail: "写入 " + id + ": " + err.Error()}
		}
	}

	for _, item := range existing {
		if _, ok := live[item.ID]; !ok {
			report.Missing = append(report.Missing, item.ID)
		}
	}
	sort.Strings(report.Missing)
	return report
}

// syncCredential 挑一个账号去拉上游目录。ck_ 号优先（不受 token 过期影响）。
func (s *Server) syncCredential() (Account, error) {
	accounts, err := s.store.ListAccounts()
	if err != nil {
		return Account{}, err
	}
	var fallback *Account
	for i := range accounts {
		if !accounts[i].Enabled {
			continue
		}
		full, err := s.store.GetAccount(accounts[i].ID)
		if err != nil || full == nil {
			continue
		}
		if !s.upstream.ProfileFor(*full).IsBearer() {
			return *full, nil
		}
		if fallback == nil {
			fallback = full
		}
	}
	if fallback != nil {
		return *fallback, nil
	}
	return Account{}, fmt.Errorf("没有启用的账号可用于拉取上游模型目录")
}

// FetchConfigModels 用两种客户端 UA 各拉一次 /v3/config，返回并集。
func (c *UpstreamClient) FetchConfigModels(ctx context.Context, account Account) ([]upstreamModel, error) {
	byID := map[string]upstreamModel{}
	var lastErr error
	for _, userAgent := range []string{ideUserAgent, cliConfigUA} {
		models, err := c.fetchConfigOnce(ctx, account, userAgent)
		if err != nil {
			lastErr = err
			continue
		}
		for _, model := range models {
			if _, seen := byID[model.ID]; !seen {
				byID[model.ID] = model
			}
		}
	}
	if len(byID) == 0 {
		if lastErr == nil {
			lastErr = fmt.Errorf("/v3/config 没有返回任何模型")
		}
		return nil, lastErr
	}
	out := make([]upstreamModel, 0, len(byID))
	for _, model := range byID {
		out = append(out, model)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (c *UpstreamClient) fetchConfigOnce(ctx context.Context, account Account, userAgent string) ([]upstreamModel, error) {
	headers := c.BuildHeaders(account)
	// 目录接口不认对话族的 agent 头，去掉以免被当成一次对话请求
	headers.Del("X-Agent-Intent")
	headers.Del("X-Agent-Purpose")
	headers.Set("User-Agent", userAgent)
	headers.Set("Accept", "application/json")
	headers.Set("x-codebuddy-request", "1")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.configURL(account), nil)
	if err != nil {
		return nil, err
	}
	req.Header = headers

	client, err := c.httpClient(account)
	if err != nil {
		return nil, err
	}
	transportClient := *client
	transportClient.Timeout = 30 * time.Second
	resp, err := transportClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("/v3/config http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var parsed struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Models []upstreamModel `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("/v3/config decode: %w", err)
	}
	if parsed.Code != 0 {
		return nil, fmt.Errorf("/v3/config code %d: %s", parsed.Code, truncate(parsed.Msg, 160))
	}
	return parsed.Data.Models, nil
}

// configURL 把账号的 chat 端点换成同源 /v3/config。
func (c *UpstreamClient) configURL(account Account) string {
	base, err := c.welfareBase(account)
	if err != nil {
		return ""
	}
	return base + configPath
}

// isChatModel 判断一条目录项是不是我们该收进模型表的对话模型。
//
// 排除两类：
//   - 图像/视频生成模型（tags 里带 text-to-image / text-to-video / image-to-video），
//     它们走另一套接口，塞进 chat 模型表只会让请求必然失败；
//   - 内部补全/搜索模型（codewise-*、nes-gf），它们不是给用户点的。
func isChatModel(model upstreamModel) bool {
	id := strings.TrimSpace(model.ID)
	if id == "" {
		return false
	}
	if strings.HasPrefix(id, "codewise-") || id == "nes-gf" {
		return false
	}
	for _, tag := range model.Tags {
		switch strings.ToLower(strings.TrimSpace(tag)) {
		case "text-to-image", "text-to-video", "image-to-video", "image-edit":
			return false
		}
	}
	// 没有上下文长度又没有工具调用能力的，基本就是生成类模型（目录里它们的
	// maxInputTokens 是 null）。正常对话模型两者至少有一个。
	if model.MaxInputTokens == 0 && model.MaxOutputTokens == 0 &&
		(model.SupportsToolCall == nil || !*model.SupportsToolCall) {
		return false
	}
	return true
}

// catalogMetaChanged 只比元信息，忽略 enabled/sort_order（那两项同步不动）。
func catalogMetaChanged(previous CatalogModel, next CatalogModel) bool {
	return previous.Name != next.Name ||
		strings.TrimSpace(previous.Credits) != strings.TrimSpace(next.Credits) ||
		previous.MaxInputTokens != next.MaxInputTokens ||
		previous.MaxOutputTokens != next.MaxOutputTokens ||
		previous.SupportsImages != next.SupportsImages
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

// ResetModelCatalog 从上游重拉模型表，并删掉目录里没有的行（「清空重拉」）。
//
// 和 SyncModelCatalog 的区别：同步只补元信息、一个行都不删；重置是要把历史累积的
// 幽灵模型（glm-4.6 / glm-4.7 / hunyuan-2.0-thinking 这类实测 11102 的）真的清掉。
//
// 顺序不能变，两条防线都是为了「宁可什么都不做，也不能把服务弄哑」：
//
//  1. **先拉、后清。** 拉取失败时模型表原封不动。反过来写的话，一旦上游抽风，
//     模型表被清空而重拉失败，/v1/models 立刻变空，所有请求 400。
//  2. **先剪类型引用、再删行。** ensureTypeModelsInCatalog 会在每次 Bootstrap 时
//     把「类型引用但表里没有」的模型补回去，所以只删行不剪引用 = 重启后全部复活。
//     反过来先删行也不行：DeleteCatalogModel 会以「仍被类型引用」拒绝删除。
//
// 关于「目录里没有但实际还能调用」的模型（实测 glm-5.1 / kimi-k2.6 / hy4-preview-f
// 都还能跑）：重置就是明确「以官方目录为准」的选择，它们会从对外列表消失。
// 想继续用它们就手动加回模型表，或改回普通同步。
func (s *Server) ResetModelCatalog(ctx context.Context) modelSyncReport {
	account, err := s.syncCredential()
	if err != nil {
		return modelSyncReport{OK: false, Detail: err.Error()}
	}
	models, err := s.upstream.FetchConfigModels(ctx, account)
	if err != nil {
		return modelSyncReport{OK: false, Detail: "拉取失败，模型表未改动: " + err.Error()}
	}

	live := make(map[string]upstreamModel, len(models))
	report := modelSyncReport{OK: true}
	for _, model := range models {
		if !isChatModel(model) {
			report.Skipped = append(report.Skipped, model.ID)
			continue
		}
		live[model.ID] = model
	}
	if len(live) == 0 {
		return modelSyncReport{OK: false, Detail: "上游目录没有可用的对话模型，已中止（模型表未改动）"}
	}
	report.Fetched = len(live)
	sort.Strings(report.Skipped)

	// 重置前的启用列表要留着：重置不该顺手把线上模型全下架。
	// 只要模型在上游目录里还活着，就保持它原来的开关状态。
	previousEnabled := map[string]struct{}{}
	before, err := s.store.ListCatalog()
	if err != nil {
		return modelSyncReport{OK: false, Detail: err.Error()}
	}
	for _, item := range before {
		if item.Enabled {
			previousEnabled[item.ID] = struct{}{}
		}
	}

	// 第一步：剪掉模型类型里指向「上游已无」的引用（否则删不掉，且重启会复活）。
	pruned, err := s.store.PruneTypeModels(live)
	if err != nil {
		return modelSyncReport{OK: false, Detail: "剪类型引用失败: " + err.Error()}
	}
	report.PrunedTypes = pruned

	// 第二步：删掉模型表里上游已经没有的行。
	deleted, err := s.store.DeleteCatalogWhere(func(id string) bool {
		_, alive := live[id]
		return !alive
	})
	if err != nil {
		return modelSyncReport{OK: false, Detail: "删除旧模型失败: " + err.Error()}
	}
	report.Deleted = deleted

	// 第三步：按上游目录整表重建，排序按目录顺序。
	ids := make([]string, 0, len(live))
	for id := range live {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for order, id := range ids {
		model := live[id]
		_, wasEnabled := previousEnabled[id]
		if wasEnabled {
			report.Enabled = append(report.Enabled, id)
		}
		if err := s.store.UpsertCatalogModel(CatalogModel{
			ID:              id,
			Name:            firstNonEmpty(model.Name, id),
			Credits:         strings.TrimSpace(model.Credits),
			MaxInputTokens:  firstPositive(model.MaxInputTokens, model.MaxAllowedSize),
			MaxOutputTokens: model.MaxOutputTokens,
			SupportsImages:  model.SupportsImages,
			Enabled:         wasEnabled,
			SortOrder:       order,
		}); err != nil {
			return modelSyncReport{OK: false, Detail: "写入 " + id + ": " + err.Error()}
		}
		report.Created = append(report.Created, id)
	}
	return report
}
