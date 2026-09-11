package app

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
)

// 账号类型与公共模型表的管理接口。
//
// 这两张表是「可运营」的落点：以前上游加一个模型要改 Go 代码重编译，新开一类渠道
// 要新起一个实例。现在都在面板上完成。

func (s *Server) handleAdminTypes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := s.store.ListAccountTypes()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
			return
		}
		if list == nil {
			list = []AccountType{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"types": list})
	case http.MethodPost:
		var payload AccountType
		if err := decodeJSON(r, &payload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
			return
		}
		// builtin 只能由代码种下，不接受外部指定——否则谁都能造一个删不掉的类型
		existing, err := s.store.GetAccountType(NormalizeTypeSlug(payload.Slug))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
			return
		}
		payload.Builtin = existing != nil && existing.Builtin
		if err := s.store.UpsertAccountType(payload); err != nil {
			writeValidationError(w, err)
			return
		}
		s.reloadTypes()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "slug": NormalizeTypeSlug(payload.Slug)})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"detail": "method not allowed"})
	}
}

func (s *Server) handleAdminTypeBySlug(w http.ResponseWriter, r *http.Request) {
	slug := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/types/"), "/")
	if slug == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"detail": "type not found"})
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if err := s.store.DeleteAccountType(slug); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeJSON(w, http.StatusNotFound, map[string]any{"detail": "type not found"})
				return
			}
			writeValidationError(w, err)
			return
		}
		s.reloadTypes()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"detail": "method not allowed"})
	}
}

func (s *Server) handleAdminModels(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		catalog, err := s.store.CatalogWithTypes()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
			return
		}
		if catalog == nil {
			catalog = []CatalogModel{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"models": catalog})
	case http.MethodPost:
		var payload CatalogModel
		if err := decodeJSON(r, &payload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
			return
		}
		if err := s.store.UpsertCatalogModel(payload); err != nil {
			writeValidationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": strings.TrimSpace(payload.ID)})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"detail": "method not allowed"})
	}
}

func (s *Server) handleAdminModelByID(w http.ResponseWriter, r *http.Request) {
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/models/"), "/")
	if id == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"detail": "model not found"})
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if err := s.store.DeleteCatalogModel(id); err != nil {
			writeValidationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"detail": "method not allowed"})
	}
}

// writeValidationError 把校验类错误报成 400，其余报 500。
// 分开是为了让面板能直接把 detail 显示给操作者——校验错误是给人看的。
func writeValidationError(w http.ResponseWriter, err error) {
	var validation *ValidationError
	if errors.As(err, &validation) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": validation.Message})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
}

// attachEffectiveModels 给面板补上每个号实际能接的模型。
//
// 回落链有三层，操作者不该自己在脑子里跑一遍才知道某个号到底接不接某个模型——
// 那正是这次改造之前最难解释的部分。
func (s *Server) attachEffectiveModels(accounts []PublicAccount) []PublicAccount {
	for i := range accounts {
		if len(accounts[i].Models) > 0 {
			accounts[i].EffectiveModels = accounts[i].Models
			continue
		}
		if typ, ok := s.types.Get(accounts[i].TypeSlug); ok && len(typ.Models) > 0 {
			accounts[i].EffectiveModels = typ.Models
			continue
		}
		accounts[i].EffectiveModels = nil // 不限
	}
	return accounts
}
