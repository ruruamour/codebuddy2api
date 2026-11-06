package app

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// 福利状态持久化。
//
// 单独一张表而不是往 accounts 上加列：福利状态是「每个账号 × 每个任务」的两条记录，
// 塞进 accounts 要加十几列、还要改 ListAccounts 的列清单与 scan，收益为零。
// 这张表只被福利调度与面板读，删掉它不影响转发链路。
const welfareDDL = `
CREATE TABLE IF NOT EXISTS welfare_state (
  account_id INTEGER NOT NULL,
  kind TEXT NOT NULL,
  day TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT '',
  detail TEXT,
  score INTEGER NOT NULL DEFAULT 0,
  state TEXT,
  checked_at INTEGER NOT NULL,
  PRIMARY KEY (account_id, kind)
);`

// WelfareRecord 是一行福利状态（未反序列化的形态）。
type WelfareRecord struct {
	AccountID int64
	Kind      WelfareKind
	Day       string
	Status    string
	Detail    string
	Score     int
	State     string
	CheckedAt int64
}

// SaveWelfareCheckin 保存一次 CN 签到结果。
func (s *Store) SaveWelfareCheckin(accountID int64, day, status, detail string, state *CheckinState) error {
	return s.saveWelfare(accountID, WelfareCheckin, day, status, detail, 0, state)
}

// SaveWelfareBonus 保存一次 intl 每日奖励结果。
func (s *Store) SaveWelfareBonus(accountID int64, day string, score int, status, detail string, state *BonusState) error {
	return s.saveWelfare(accountID, WelfareBonus, day, status, detail, score, state)
}

func (s *Store) saveWelfare(accountID int64, kind WelfareKind, day, status, detail string,
	score int, state any) error {
	var blob any
	if state != nil {
		if raw, err := json.Marshal(state); err == nil {
			blob = string(raw)
		}
	}
	_, err := s.db.Exec(`
INSERT INTO welfare_state (account_id, kind, day, status, detail, score, state, checked_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(account_id, kind) DO UPDATE SET
  day = excluded.day,
  status = excluded.status,
  detail = excluded.detail,
  score = excluded.score,
  state = COALESCE(excluded.state, welfare_state.state),
  checked_at = excluded.checked_at`,
		accountID, string(kind), day, status, detail, score, blob, now())
	return err
}

// Welfare 读一个账号的两类福利状态；都没有记录时返回 nil。
func (s *Store) Welfare(accountID int64) *WelfareState {
	rows, err := s.db.Query(
		"SELECT account_id, kind, day, status, detail, score, state, checked_at FROM welfare_state WHERE account_id = ?", accountID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return collectWelfare(rows, accountID)
}

// ListWelfare 一次性读出全部账号的福利状态，面板与统计用。
func (s *Store) ListWelfare() (map[int64]*WelfareState, error) {
	rows, err := s.db.Query(`
SELECT account_id, kind, day, status, detail, score, state, checked_at
FROM welfare_state ORDER BY account_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]*WelfareState{}
	for rows.Next() {
		var accountID int64
		var record WelfareRecord
		var state sql.NullString
		if err := rows.Scan(&accountID, &record.Kind, &record.Day, &record.Status,
			&record.Detail, &record.Score, &state, &record.CheckedAt); err != nil {
			return nil, err
		}
		record.AccountID = accountID
		record.State = state.String
		applyWelfareRecord(out, record)
	}
	return out, rows.Err()
}

// collectWelfare 把「每行一个 kind」的结果拼成 WelfareState。
// 单账号版只返回该账号的那条（以前写成取 map[0]，对小 id 之外的账号永远是 nil）。
func collectWelfare(rows *sql.Rows, accountID int64) *WelfareState {
	byAccount := map[int64]*WelfareState{}
	for rows.Next() {
		var rowAccountID int64
		var record WelfareRecord
		var state sql.NullString
		if err := rows.Scan(&rowAccountID, &record.Kind, &record.Day, &record.Status,
			&record.Detail, &record.Score, &state, &record.CheckedAt); err != nil {
			return nil
		}
		record.AccountID = rowAccountID
		record.State = state.String
		applyWelfareRecord(byAccount, record)
	}
	return byAccount[accountID]
}

// applyWelfareRecord 把一行记录写进账号的 WelfareState。
//
// 优先用落库时的完整 JSON（面板要展示连登天数、额度、活动名这些明细）；
// 反序列化失败就退化成只有 day/status/detail 的骨架，面板至少不会显示成「没有状态」。
func applyWelfareRecord(target map[int64]*WelfareState, record WelfareRecord) {
	entry := target[record.AccountID]
	if entry == nil {
		entry = &WelfareState{}
		target[record.AccountID] = entry
	}
	checkedAt := record.CheckedAt
	switch record.Kind {
	case WelfareCheckin:
		state := &CheckinState{}
		if record.State != "" {
			_ = json.Unmarshal([]byte(record.State), state)
		}
		if state.Day == "" {
			state.Day = record.Day
		}
		if state.Status == "" {
			state.Status = record.Status
		}
		if state.Detail == "" {
			state.Detail = record.Detail
		}
		if state.CheckedAt == nil {
			state.CheckedAt = &checkedAt
		}
		entry.Checkin = state
	case WelfareBonus:
		state := &BonusState{}
		if record.State != "" {
			_ = json.Unmarshal([]byte(record.State), state)
		}
		if state.Day == "" {
			state.Day = record.Day
		}
		if state.Status == "" {
			state.Status = record.Status
		}
		if state.Detail == "" {
			state.Detail = record.Detail
		}
		if state.Score == 0 {
			state.Score = record.Score
		}
		if state.CheckedAt == nil {
			state.CheckedAt = &checkedAt
		}
		entry.Bonus = state
	}
}

// WelfareAge 返回某个账号某类福利距上次执行过去了多久；没记录返回 -1。
func WelfareAge(state *WelfareState, kind WelfareKind) time.Duration {
	if state == nil {
		return -1
	}
	var checkedAt *int64
	switch kind {
	case WelfareCheckin:
		if state.Checkin != nil {
			checkedAt = state.Checkin.CheckedAt
		}
	case WelfareBonus:
		if state.Bonus != nil {
			checkedAt = state.Bonus.CheckedAt
		}
	}
	if checkedAt == nil {
		return -1
	}
	return time.Since(time.Unix(*checkedAt, 0))
}

// ensureWelfareTable 在打开库时建表（幂等）。
func (s *Store) ensureWelfareTable() error {
	if _, err := s.db.Exec(welfareDDL); err != nil {
		return err
	}
	return nil
}

var errWelfareNotFound = errors.New("welfare state not found")
