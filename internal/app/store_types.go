package app

import (
	"database/sql"
	"errors"
	"log"
	"strings"
)

const accountTypesDDL = `
CREATE TABLE IF NOT EXISTS account_types (
  slug TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  auth_mode TEXT,
  upstream_url TEXT,
  domain TEXT,
  request_shape TEXT,
  models TEXT,
  priority INTEGER NOT NULL DEFAULT 0,
  enabled INTEGER NOT NULL DEFAULT 1,
  builtin INTEGER NOT NULL DEFAULT 0,
  notes TEXT,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);`

// Bootstrap 建立类型表与模型表的初始数据，并把老账号归类。
//
// 它和 init() 分开，是因为这一步需要实例配置（env）才能判断「这个实例原本是哪一类」，
// 而 NewStore 只拿得到库路径。分开也让迁移这件事显式可测。
//
// 迁移的硬要求：跑完之后 /v1/models 对外给出的列表、以及每个模型能被哪些号接，
// 都必须和跑之前逐字一致。所以两张表的种子都取自「这个库当前实际生效的值」，
// 而不是代码里的默认值。
func (s *Store) Bootstrap(cfg Config) error {
	// 先读旧口径：这一步必须在灌模型表之前，否则读到的就是新表的空结果
	previous, err := s.ModelSettings(cfg.Models, cfg.PoolStrategy)
	if err != nil {
		return err
	}
	if err := s.seedCatalog(previous.Models); err != nil {
		return err
	}
	if err := s.seedAccountTypes(cfg, previous.Models); err != nil {
		return err
	}
	if err := s.ensureTypeModelsInCatalog(); err != nil {
		return err
	}
	return s.assignMissingTypeSlugs()
}

// seedAccountTypes 首次灌入内置类型。
//
// 与实例 env 同类的那个内置类型，模型集合取实例当前启用的列表而不是代码默认值：
// 老库里这些号本来就在接这批模型，换成默认值会让某些模型突然没有渠道能接。
func (s *Store) seedAccountTypes(cfg Config, enabledModels []string) error {
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM account_types").Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	instanceSlug := DetectTypeSlug("", cfg.Domain, cfg.UpstreamURL)
	if instanceSlug == "" {
		instanceSlug = TypeSlugCN
	}
	for _, typ := range builtinAccountTypes() {
		if typ.Slug == instanceSlug {
			if len(enabledModels) > 0 {
				typ.Models = enabledModels
			}
			// 实例原本就是这一类，先前所有账号都归它，不能是停用的
			typ.Enabled = true
			if v := strings.TrimSpace(cfg.UpstreamURL); v != "" {
				typ.UpstreamURL = v
			}
			if v := strings.TrimSpace(cfg.Domain); v != "" {
				typ.Domain = v
			}
			if v := strings.TrimSpace(cfg.AuthMode); v != "" {
				typ.AuthMode = NormalizeAuthMode(v)
			}
			if v := strings.TrimSpace(cfg.RequestShape); v != "" {
				typ.RequestShape = NormalizeRequestShape(v)
			}
		}
		if err := s.UpsertAccountType(typ); err != nil {
			return err
		}
	}
	log.Printf("types: 已初始化内置账号类型（实例原本属于 %s）", instanceSlug)
	return nil
}

// assignMissingTypeSlugs 给还没归类的账号按凭证形态和上游猜一个类型。
//
// 猜不出来就留空——留空的账号继续走实例 env，行为不变；归错类反而会把它的上游
// 或认证方式改掉，那才是真的会出事。
func (s *Store) assignMissingTypeSlugs() error {
	rows, err := s.db.Query(`
SELECT id, api_key, COALESCE(domain, ''), COALESCE(upstream_url, '')
FROM accounts WHERE type_slug IS NULL OR type_slug = ''`)
	if err != nil {
		return err
	}
	type pending struct {
		id   int64
		slug string
	}
	var todo []pending
	for rows.Next() {
		var id int64
		var apiKey, domain, upstream string
		if err := rows.Scan(&id, &apiKey, &domain, &upstream); err != nil {
			rows.Close()
			return err
		}
		if slug := DetectTypeSlug(apiKey, domain, upstream); slug != "" {
			todo = append(todo, pending{id: id, slug: slug})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range todo {
		if _, err := s.db.Exec("UPDATE accounts SET type_slug = ?, updated_at = ? WHERE id = ?",
			item.slug, now(), item.id); err != nil {
			return err
		}
	}
	if len(todo) > 0 {
		log.Printf("types: 已为 %d 个账号自动归类", len(todo))
	}
	return nil
}

// ListAccountTypes 返回全部类型，附带各自的账号数。
func (s *Store) ListAccountTypes() ([]AccountType, error) {
	rows, err := s.db.Query(`
SELECT t.slug, t.name, COALESCE(t.auth_mode, ''), COALESCE(t.upstream_url, ''),
       COALESCE(t.domain, ''), COALESCE(t.request_shape, ''), COALESCE(t.models, ''),
       t.priority, t.enabled, t.builtin, COALESCE(t.notes, ''), t.created_at, t.updated_at,
       (SELECT COUNT(*) FROM accounts a WHERE a.type_slug = t.slug)
FROM account_types t ORDER BY t.priority DESC, t.slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountType
	for rows.Next() {
		var item AccountType
		var models string
		var enabled, builtin int
		if err := rows.Scan(&item.Slug, &item.Name, &item.AuthMode, &item.UpstreamURL,
			&item.Domain, &item.RequestShape, &models, &item.Priority, &enabled, &builtin,
			&item.Notes, &item.CreatedAt, &item.UpdatedAt, &item.Accounts); err != nil {
			return nil, err
		}
		item.Models = decodeModelList(models)
		item.Enabled = enabled != 0
		item.Builtin = builtin != 0
		out = append(out, item)
	}
	return out, rows.Err()
}

// GetAccountType 取单个类型；不存在返回 nil。
func (s *Store) GetAccountType(slug string) (*AccountType, error) {
	list, err := s.ListAccountTypes()
	if err != nil {
		return nil, err
	}
	slug = strings.TrimSpace(slug)
	for i := range list {
		if list[i].Slug == slug {
			return &list[i], nil
		}
	}
	return nil, nil
}

// UpsertAccountType 新增或整体更新一个类型。
func (s *Store) UpsertAccountType(payload AccountType) error {
	slug := NormalizeTypeSlug(payload.Slug)
	if slug == "" {
		return &ValidationError{Message: "类型标识不能为空"}
	}
	name := strings.TrimSpace(payload.Name)
	if name == "" {
		name = slug
	}
	if v := strings.TrimSpace(payload.UpstreamURL); v != "" && HostOfURL(v) == "" {
		return &ValidationError{Message: "上游地址不是合法 URL: " + v}
	}
	timestamp := now()
	_, err := s.db.Exec(`
INSERT INTO account_types (slug, name, auth_mode, upstream_url, domain, request_shape,
                           models, priority, enabled, builtin, notes, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(slug) DO UPDATE SET
  name = excluded.name,
  auth_mode = excluded.auth_mode,
  upstream_url = excluded.upstream_url,
  domain = excluded.domain,
  request_shape = excluded.request_shape,
  models = excluded.models,
  priority = excluded.priority,
  enabled = excluded.enabled,
  notes = excluded.notes,
  updated_at = excluded.updated_at`,
		slug, name, NormalizeAuthMode(payload.AuthMode), strings.TrimSpace(payload.UpstreamURL),
		strings.TrimSpace(payload.Domain), NormalizeRequestShape(payload.RequestShape),
		encodeModelList(payload.Models), payload.Priority, boolToInt(payload.Enabled),
		boolToInt(payload.Builtin), strings.TrimSpace(payload.Notes), timestamp, timestamp)
	return err
}

// DeleteAccountType 删掉一个类型。内置类型不能删；还挂着账号的也不能删——
// 删了那些号会静默回落到实例 env，上游可能整个变掉，属于最难查的一类故障。
func (s *Store) DeleteAccountType(slug string) error {
	slug = strings.TrimSpace(slug)
	typ, err := s.GetAccountType(slug)
	if err != nil {
		return err
	}
	if typ == nil {
		return sql.ErrNoRows
	}
	if typ.Builtin {
		return &ValidationError{Message: "内置类型不能删除，可以停用"}
	}
	if typ.Accounts > 0 {
		return &ValidationError{Message: "该类型下还有账号，请先把它们改到别的类型"}
	}
	_, err = s.db.Exec("DELETE FROM account_types WHERE slug = ?", slug)
	return err
}

// LoadTypeRegistry 把类型表灌进进程内缓存。
func (s *Store) LoadTypeRegistry(registry *TypeRegistry) error {
	list, err := s.ListAccountTypes()
	if err != nil {
		return err
	}
	registry.Replace(list)
	return nil
}

// TypeSlugExists 判断类型是否存在，账号表单校验用。
func (s *Store) TypeSlugExists(slug string) (bool, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return true, nil // 不归类是允许的
	}
	var found string
	err := s.db.QueryRow("SELECT slug FROM account_types WHERE slug = ?", slug).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ensureTypeModelsInCatalog 保证每个类型引用的模型在模型表里都有行。
//
// 少了行不会让路由出错（路由只比字符串），但面板的模型勾选框是按模型表渲染的：
// 表里没有的模型在类型编辑页看不见，一保存就被静默清掉。
func (s *Store) ensureTypeModelsInCatalog() error {
	types, err := s.ListAccountTypes()
	if err != nil {
		return err
	}
	order := 1000
	for _, typ := range types {
		for _, model := range typ.Models {
			model = strings.TrimSpace(model)
			if model == "" {
				continue
			}
			exists, err := s.catalogRowExists(model)
			if err != nil {
				return err
			}
			if exists {
				continue
			}
			// 补的行默认停用：它出现在某个类型里，不代表这个实例要对外提供它
			if err := s.UpsertCatalogModel(CatalogModel{
				ID: model, Name: model, Enabled: false, SortOrder: order,
			}); err != nil {
				return err
			}
			order++
			log.Printf("types: 模型表补入 %s（被类型 %s 引用）", model, typ.Slug)
		}
	}
	return nil
}
