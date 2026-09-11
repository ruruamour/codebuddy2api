package app

import (
	"database/sql"
	"errors"
	"sort"
	"strings"
)

// 公共模型表。
//
// 以前模型元信息（显示名、积分倍率、上下文长度）是 models.go 里一段硬编码的 Go 切片，
// 上游加一个模型就得改代码重新编译一次；「启用哪些」又单独存在 settings 的一行 JSON 里。
// 两处分离的结果是：库里启用了 deepseek-v4.1-flash，硬编码表里却查无此人，面板只能
// 显示一个光秃秃的 ID。
//
// 现在合成一张可编辑的表：一行一个模型，元信息和启用开关放一起，面板直接增删改。
// Go 里那份切片降级为「装机种子」，只在表是空的时候灌一次。

// CatalogModel 是公共模型表里的一行。
type CatalogModel struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Credits         string `json:"credits,omitempty"`
	MaxInputTokens  int    `json:"max_input_tokens,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
	SupportsImages  bool   `json:"supports_images,omitempty"`
	Enabled         bool   `json:"enabled"`
	SortOrder       int    `json:"sort_order"`

	// Types 是「哪些账号类型提供这个模型」，每次列表时算出来，不持久化。
	// 面板靠它提示「这个模型启用了但没有任何渠道能接」——那种配置只会在
	// 请求打进来时才暴露，放到面板上一眼就能看见。
	Types []string `json:"types,omitempty"`
}

const catalogDDL = `
CREATE TABLE IF NOT EXISTS model_catalog (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  credits TEXT,
  max_input_tokens INTEGER NOT NULL DEFAULT 0,
  max_output_tokens INTEGER NOT NULL DEFAULT 0,
  supports_images INTEGER NOT NULL DEFAULT 0,
  enabled INTEGER NOT NULL DEFAULT 1,
  sort_order INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);`

// seedCatalog 首次建表时把种子模型灌进去，并按「实例当前启用了哪些」设置开关。
//
// enabledSeed 来自老库 settings 里那行 model_settings，或者装机时的 env——
// 迁移后 /v1/models 对外给出的列表必须和迁移前逐字一致，不能因为换了存储就
// 悄悄多出或少掉模型。
func (s *Store) seedCatalog(enabledSeed []string) error {
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM model_catalog").Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	enabled := make(map[string]struct{}, len(enabledSeed))
	for _, id := range enabledSeed {
		enabled[strings.TrimSpace(id)] = struct{}{}
	}
	// 顺序要按启用列表原样排在最前面。
	//
	// 表里的排序不只是好看：列表第一个是「请求没带 model 时用哪个」的默认值，也是
	// 面板和 /v1/models 的展示顺序。按代码里那份种子的顺序重排，等于悄悄换掉了
	// 操作者排好的默认模型。
	meta := make(map[string]ModelInfo, len(CodeBuddyModelCatalog))
	for _, item := range CodeBuddyModelCatalog {
		meta[item.ID] = item
	}
	seeds := make([]ModelInfo, 0, len(CodeBuddyModelCatalog)+len(enabledSeed))
	known := make(map[string]struct{}, len(CodeBuddyModelCatalog)+len(enabledSeed))
	for _, id := range enabledSeed {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, seen := known[id]; seen {
			continue
		}
		known[id] = struct{}{}
		if item, ok := meta[id]; ok {
			seeds = append(seeds, item)
			continue
		}
		// 启用着、但种子表里没有的模型也要建行，否则迁移会把它们弄丢
		seeds = append(seeds, ModelInfo{ID: id, Name: id})
	}
	// 剩下的种子模型建成停用状态，操作者想开随时能开
	for _, item := range CodeBuddyModelCatalog {
		if _, seen := known[item.ID]; seen {
			continue
		}
		known[item.ID] = struct{}{}
		seeds = append(seeds, item)
	}

	timestamp := now()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for index, item := range seeds {
		_, isEnabled := enabled[item.ID]
		name := strings.TrimSpace(item.Name)
		if name == "" {
			name = item.ID
		}
		if _, err := tx.Exec(`
INSERT INTO model_catalog (id, name, credits, max_input_tokens, max_output_tokens,
                           supports_images, enabled, sort_order, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			item.ID, name, item.Credits, item.MaxInputTokens, item.MaxOutputTokens,
			boolToInt(item.SupportsImages), boolToInt(isEnabled), index, timestamp, timestamp); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListCatalog 返回整张模型表（含停用的），按 sort_order、id 排。
func (s *Store) ListCatalog() ([]CatalogModel, error) {
	rows, err := s.db.Query(`
SELECT id, name, COALESCE(credits, ''), max_input_tokens, max_output_tokens,
       supports_images, enabled, sort_order
FROM model_catalog ORDER BY sort_order, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CatalogModel
	for rows.Next() {
		var item CatalogModel
		var supportsImages, enabled int
		if err := rows.Scan(&item.ID, &item.Name, &item.Credits, &item.MaxInputTokens,
			&item.MaxOutputTokens, &supportsImages, &enabled, &item.SortOrder); err != nil {
			return nil, err
		}
		item.SupportsImages = supportsImages != 0
		item.Enabled = enabled != 0
		out = append(out, item)
	}
	return out, rows.Err()
}

// EnabledModelIDs 是对外 /v1/models 和请求准入的唯一依据。
func (s *Store) EnabledModelIDs() ([]string, error) {
	rows, err := s.db.Query("SELECT id FROM model_catalog WHERE enabled = 1 ORDER BY sort_order, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// UpsertCatalogModel 新增或更新一个模型。
func (s *Store) UpsertCatalogModel(item CatalogModel) error {
	id := strings.TrimSpace(item.ID)
	if !validModelID(id) {
		return &ValidationError{Message: "invalid model id: " + id}
	}
	name := strings.TrimSpace(item.Name)
	if name == "" {
		name = id
	}
	timestamp := now()
	_, err := s.db.Exec(`
INSERT INTO model_catalog (id, name, credits, max_input_tokens, max_output_tokens,
                           supports_images, enabled, sort_order, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  name = excluded.name,
  credits = excluded.credits,
  max_input_tokens = excluded.max_input_tokens,
  max_output_tokens = excluded.max_output_tokens,
  supports_images = excluded.supports_images,
  enabled = excluded.enabled,
  sort_order = excluded.sort_order,
  updated_at = excluded.updated_at`,
		id, name, strings.TrimSpace(item.Credits), item.MaxInputTokens, item.MaxOutputTokens,
		boolToInt(item.SupportsImages), boolToInt(item.Enabled), item.SortOrder, timestamp, timestamp)
	return err
}

// SetCatalogEnabled 只改启用开关，面板上勾选/取消勾选走这里。
func (s *Store) SetCatalogEnabled(ids []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE model_catalog SET enabled = 0, updated_at = ?", now()); err != nil {
		return err
	}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, err := tx.Exec("UPDATE model_catalog SET enabled = 1, updated_at = ? WHERE id = ?", now(), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteCatalogModel 删掉一个模型；仍被某个类型引用时拒绝，避免删出一个路由黑洞。
func (s *Store) DeleteCatalogModel(id string) error {
	id = strings.TrimSpace(id)
	types, err := s.ListAccountTypes()
	if err != nil {
		return err
	}
	var used []string
	for _, typ := range types {
		for _, model := range typ.Models {
			if strings.EqualFold(strings.TrimSpace(model), id) {
				used = append(used, typ.Name)
				break
			}
		}
	}
	if len(used) > 0 {
		return &ValidationError{Message: "模型仍被账号类型引用，先从这些类型里移除：" + strings.Join(used, "、")}
	}
	_, err = s.db.Exec("DELETE FROM model_catalog WHERE id = ?", id)
	return err
}

// CatalogWithTypes 给面板用：每个模型附上「哪些启用中的类型提供它」。
func (s *Store) CatalogWithTypes() ([]CatalogModel, error) {
	catalog, err := s.ListCatalog()
	if err != nil {
		return nil, err
	}
	types, err := s.ListAccountTypes()
	if err != nil {
		return nil, err
	}
	providers := map[string][]string{}
	unlimited := make([]string, 0, len(types))
	for _, typ := range types {
		if !typ.Enabled {
			continue
		}
		if len(typ.Models) == 0 {
			// 没声明模型的类型接全部，对每个模型都算作提供方
			unlimited = append(unlimited, typ.Name)
			continue
		}
		for _, model := range typ.Models {
			key := strings.ToLower(strings.TrimSpace(model))
			providers[key] = append(providers[key], typ.Name)
		}
	}
	for i := range catalog {
		names := append([]string{}, providers[strings.ToLower(catalog[i].ID)]...)
		names = append(names, unlimited...)
		sort.Strings(names)
		catalog[i].Types = names
	}
	return catalog, nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// catalogRowExists 判断某个模型在表里有没有行（导入类型时用来提示缺模型）。
func (s *Store) catalogRowExists(id string) (bool, error) {
	var found string
	err := s.db.QueryRow("SELECT id FROM model_catalog WHERE id = ?", strings.TrimSpace(id)).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
