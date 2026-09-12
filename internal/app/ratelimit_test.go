package app

import (
	"testing"
	"time"
)

// 真实的 429 响应体（intl 账号打满单模型频率限制时上游返回的原文）。
const sampleRateLimitBody = `{"code":6004,"msg":"usage exceeds frequency limit, but don't worry, your usage will reset at 2026-09-12 22:09:16 UTC+8, alternatively, you can switch to the other models to continue using it.","requestId":"e8ca4b81936cb0d4859d2e027102cc0f"}`

func TestParseRateLimitReset_RealMessage(t *testing.T) {
	got, ok := ParseRateLimitReset(sampleRateLimitBody)
	if !ok {
		t.Fatal("应能解析出重置时间")
	}
	want := time.Date(2026, 9, 12, 22, 9, 16, 0, time.FixedZone("", 8*3600)).Unix()
	if got != want {
		t.Errorf("重置时间 = %d (%s)，want %d (%s)", got, time.Unix(got, 0).UTC(),
			want, time.Unix(want, 0).UTC())
	}
}

func TestParseRateLimitReset_TimezonesAndFormats(t *testing.T) {
	cases := map[string]time.Time{
		"reset at 2026-09-12 22:09:16 UTC":       time.Date(2026, 9, 12, 22, 9, 16, 0, time.UTC),
		"reset at 2026-09-12 22:09:16 UTC+05:30": time.Date(2026, 9, 12, 22, 9, 16, 0, time.FixedZone("", 5*3600+30*60)),
		"reset at 2026-09-12 22:09:16 UTC-5":     time.Date(2026, 9, 12, 22, 9, 16, 0, time.FixedZone("", -5*3600)),
		"reset at 2026-09-12 22:09:16":           time.Date(2026, 9, 12, 22, 9, 16, 0, time.UTC),
		"reset at 2026-09-12T22:09:16Z":          time.Date(2026, 9, 12, 22, 9, 16, 0, time.UTC),
	}
	for message, want := range cases {
		got, ok := ParseRateLimitReset(message)
		if !ok {
			t.Errorf("%q 应能解析", message)
			continue
		}
		if got != want.Unix() {
			t.Errorf("%q → %s，want %s", message, time.Unix(got, 0).UTC(), want.UTC())
		}
	}
}

// 没有 "reset" 字样的错误里夹带日期时不能误判。
func TestParseRateLimitReset_RequiresResetKeyword(t *testing.T) {
	for _, message := range []string{
		`{"code":11150,"msg":"the reasoning effort value is not supported by the current model"}`,
		`{"code":9,"msg":"request created at 2026-09-12 22:09:16 failed"}`,
		`{"code":401,"msg":"unauthorized"}`,
	} {
		if _, ok := ParseRateLimitReset(message); ok {
			t.Errorf("%q 不应该被解析出重置时间", message)
		}
	}
}

// 限流要等 1h43m 时，冷却必须跟着拉长——否则账号每 5 分钟被拉起来撞一次墙，
// 面板一直显示「冷却中」但账号确实在被反复调用。
func TestCooldownSecondsForError_UsesUpstreamReset(t *testing.T) {
	// 假设"现在"是 2026-09-12 12:35:06 UTC，上游说 14:09:16 UTC 复位
	nowUnix := time.Date(2026, 9, 12, 12, 35, 6, 0, time.UTC).Unix()
	const fallback = 300

	got := CooldownSecondsForError(sampleRateLimitBody, fallback, nowUnix)
	want := int(time.Date(2026, 9, 12, 14, 9, 16, 0, time.UTC).Unix() - nowUnix)
	if got != want {
		t.Errorf("冷却 %ds，want %ds（≈%d 分钟）", got, want, want/60)
	}
	if got <= fallback {
		t.Errorf("冷却 %ds 不该比回退值 %ds 还短", got, fallback)
	}
}

// 解析不出来、或上游给的时刻已经过去/比固定值还早时，回落到配置值。
func TestCooldownSecondsForError_FallsBack(t *testing.T) {
	nowUnix := time.Date(2026, 9, 12, 12, 35, 6, 0, time.UTC).Unix()
	const fallback = 300

	if got := CooldownSecondsForError(`{"code":401,"msg":"unauthorized"}`, fallback, nowUnix); got != fallback {
		t.Errorf("无重置时间应回落到 %d，得到 %d", fallback, got)
	}
	if got := CooldownSecondsForError(`reset at 2020-01-01 00:00:00 UTC`, fallback, nowUnix); got != fallback {
		t.Errorf("重置时间已过去应回落到 %d，得到 %d", fallback, got)
	}
	if got := CooldownSecondsForError(`reset at 2026-09-12 12:36:00 UTC`, fallback, nowUnix); got != fallback {
		t.Errorf("重置时间(=60s后)比固定冷却短时应保持 %d，得到 %d", fallback, got)
	}
}

// 畸形日期不能把账号锁死很久。
func TestCooldownSecondsForError_Clamps(t *testing.T) {
	nowUnix := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC).Unix()
	got := CooldownSecondsForError(`reset at 2099-01-01 00:00:00 UTC`, 300, nowUnix)
	if got != maxCooldownSeconds {
		t.Errorf("超远重置时间应被夹到 %ds，得到 %ds", maxCooldownSeconds, got)
	}
}
