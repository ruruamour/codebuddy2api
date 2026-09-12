package app

import (
	"regexp"
	"strconv"
	"time"
)

// 上游限流（HTTP 429，body code 6004）会给出权威的重置时间：
//
//	{"code":6004,"msg":"usage exceeds frequency limit, but don't worry, your usage
//	 will reset at 2026-09-12 22:09:16 UTC+8, alternatively, you can switch to the
//	 other models to continue using it."}
//
// 以前只按 COOLDOWN_SECONDS（默认 300s）冷却，于是当上游要等 1~2 小时才复位时，
// 账号每 5 分钟就被拉起来撞一次墙：面板一直显示「冷却中」，账号却确实在被反复调用，
// 用户只能手动停用。
//
// 这里把上游给的那一刻解析出来，冷却到那时候为止。
var (
	resetTimePattern = regexp.MustCompile(
		`(\d{4})-(\d{2})-(\d{2})[ T](\d{2}):(\d{2}):(\d{2})(?:\s*UTC\s*([+-]\d{1,2})(?::?(\d{2}))?)?`)
	resetKeywordPattern = regexp.MustCompile(`(?i)\breset\b`)
)

// maxCooldownSeconds 是解析出来的重置时间的上限。
// 防止畸形文本（比如误匹配到很远的日期）把账号锁死很久，超过就按上限处理。
const maxCooldownSeconds = 7 * 24 * 3600

// ParseRateLimitReset 从上游错误文本里解析限流重置时刻（unix 秒）。
//
// 必须先看到 "reset" 字样才认，否则一条普通错误里夹带的日期会被误当成重置时间。
// 没有时区后缀时按 UTC 解释。
func ParseRateLimitReset(message string) (int64, bool) {
	if !resetKeywordPattern.MatchString(message) {
		return 0, false
	}
	m := resetTimePattern.FindStringSubmatch(message)
	if m == nil {
		return 0, false
	}
	atoi := func(s string) int {
		v, _ := strconv.Atoi(s)
		return v
	}
	loc := time.UTC
	if m[7] != "" {
		hours := atoi(m[7])
		minutes := atoi(m[8])
		sign := 1
		if hours < 0 {
			sign = -1
			hours = -hours
		}
		loc = time.FixedZone("", sign*(hours*3600+minutes*60))
	}
	stamp := time.Date(atoi(m[1]), time.Month(atoi(m[2])), atoi(m[3]),
		atoi(m[4]), atoi(m[5]), atoi(m[6]), 0, loc)
	if stamp.IsZero() {
		return 0, false
	}
	return stamp.Unix(), true
}

// CooldownSecondsForError 决定这次失败要冷却多久。
//
// 上游限流给了重置时间就用它——固定值会让限流期间反复撞墙；
// 解析不出来才回落到配置值。
func CooldownSecondsForError(message string, fallback int, nowUnix int64) int {
	cooldown := fallback
	resetAt, ok := ParseRateLimitReset(message)
	if !ok {
		return cooldown
	}
	delta := resetAt - nowUnix
	if delta <= int64(cooldown) {
		// 上游给的时间比固定冷却还早（或已经过去），没必要缩短
		return cooldown
	}
	if delta > maxCooldownSeconds {
		delta = maxCooldownSeconds
	}
	return int(delta)
}
