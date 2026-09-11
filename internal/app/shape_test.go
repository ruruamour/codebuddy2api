package app

import (
	"encoding/json"
	"testing"
)

// intl 整形不能丢消息。
//
// 从 HTTP 来的请求 messages 是 []any，但服务内部构造的（Probe）是 []map[string]any。
// 以前只断言 []any，内部请求会被当成空数组——整条用户消息被丢掉，只剩前导 system，
// 探活/自检发出去的都是退化请求。
func TestShapeIntlPayload_KeepsGoTypedMessages(t *testing.T) {
	for name, messages := range map[string]any{
		"JSON 解码的 []any": []any{
			map[string]any{"role": "user", "content": "只回复OK"},
		},
		"Go 内部构造的 []map[string]any": []map[string]any{
			{"role": "user", "content": "只回复OK"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			out := shapeIntlPayload(map[string]any{"model": "m", "messages": messages})
			shaped, _ := out["messages"].([]any)
			raw, _ := json.Marshal(shaped)

			if len(shaped) != 2 {
				t.Fatalf("应为 system + user 两条，得到 %d 条: %s", len(shaped), raw)
			}
			first, _ := shaped[0].(map[string]any)
			if first["role"] != "system" {
				t.Errorf("首条必须是 system，得到 %v", first["role"])
			}
			user, _ := shaped[1].(map[string]any)
			if user["role"] != "user" {
				t.Fatalf("用户消息丢了: %s", raw)
			}
			// 字符串 content 必须转成 typed block，否则国际网关拒收
			blocks, ok := user["content"].([]any)
			if !ok || len(blocks) != 1 {
				t.Fatalf("user content 没转成 typed block: %s", raw)
			}
			block, _ := blocks[0].(map[string]any)
			if block["type"] != "text" || block["text"] != "只回复OK" {
				t.Errorf("typed block 内容不对: %s", raw)
			}
		})
	}
}
