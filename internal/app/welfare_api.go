package app

import (
	"net/http"
)

// 福利任务的管理接口。
//
// 面板上是两个按钮（立即签到 / 点亮今日）加一列状态；状态单独走 /admin/welfare，
// 不塞进 /admin/accounts 的账号对象里——福利是「账号 × 任务」的旁路数据，
// 混进账号列表会让那个已经很宽的 JSON 再长一截，而且签到状态跟转发无关。

// handleAdminWelfare 返回所有账号的福利状态。
func (s *Server) handleAdminWelfare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"detail": "method not allowed"})
		return
	}
	states, err := s.store.ListWelfare()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"welfare": states,
		"config": map[string]any{
			"checkin_auto":      s.cfg.CheckinAuto,
			"checkin_hours":     s.cfg.CheckinHours,
			"daily_bonus_auto":  s.cfg.DailyBonusAuto,
			"daily_bonus_hours": s.cfg.DailyBonusHours,
		},
	})
}

// handleAdminWelfareRun 立刻跑一轮福利任务。
//
// 请求体可带 {"kind":"checkin"|"bonus"} 只跑一类；不带就按 realm 全跑（CN 号签到、
// intl 号点亮）。上游状态接口是权威口径，重复点按钮不会重复领取。
func (s *Server) handleAdminWelfareRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"detail": "method not allowed"})
		return
	}
	var payload struct {
		Kind      string `json:"kind"`
		AccountID int64  `json:"account_id"`
	}
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &payload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
			return
		}
	}
	var summary map[string]any
	if payload.AccountID > 0 {
		// 单账号：按它自己的 realm 选任务，忽略 payload.Kind（面板不总是知道该跑哪类）
		account, err := s.store.GetAccount(payload.AccountID)
		if err != nil || account == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"detail": "account not found"})
			return
		}
		summary = s.runWelfareOne(r.Context(), *account)
	} else {
		switch WelfareKind(payload.Kind) {
		case WelfareCheckin, WelfareBonus:
			summary = s.RunWelfareKind(r.Context(), WelfareKind(payload.Kind), 0)
		default:
			summary = s.RunWelfare(r.Context())
		}
	}
	states, err := s.store.ListWelfare()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	summary["welfare"] = states
	writeJSON(w, http.StatusOK, summary)
}
