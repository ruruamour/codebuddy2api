package app

import (
	"encoding/json"
	"strings"
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

// reasoning_effort 不能被无条件删掉。
//
// 曾经的实现是 PreparePayload 里直接 delete(reasoning_effort)，等于强制关思考：
// 上游收到的请求没有档位，reasoning_tokens 恒为 0 且不返回 reasoning_content，
// 客户端 UI 上「thinking: max」只是本地状态，实际什么都没发生。
// 顺带连累了 shapeIntlPayload 里的 reasoning_summary —— 它读的是同一个键，
// 删在前面就永远加不上。
func TestPreparePayload_KeepsReasoningEffort(t *testing.T) {
	client := NewUpstreamClient(Config{}, nil)
	profile := AccountProfile{RequestShape: ShapeIntl}

	for _, level := range []string{"minimal", "low", "medium", "high", "xhigh", "max"} {
		out := client.PreparePayload(map[string]any{
			"model":            "m",
			"reasoning_effort": level,
			"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
		}, profile)
		if got := out["reasoning_effort"]; got != level {
			t.Errorf("档位 %q 被改成了 %v", level, got)
		}
		if out["reasoning_summary"] != "auto" {
			t.Errorf("档位 %q 下 reasoning_summary 应为 auto，得到 %v", level, out["reasoning_summary"])
		}
	}
}

// 上游只认 6 个小写值，其余一律 400 invalid_reasoning_effort (11150)。
// 客户端送来别的写法时应降级为「不思考」，而不是原样透传把整个请求打挂；
// 大小写变体折叠成小写即可。
func TestSanitizeReasoningEffort_MapsAndDropsUnknown(t *testing.T) {
	cases := map[string]string{
		"Max":     "max",  // 大小写归一化
		"  HIGH ": "high", // 去空白
		"bogus":   "",     // 未知档位不许透传（会 11150）
		"max":     "max",
	}
	for input, want := range cases {
		payload := map[string]any{"reasoning_effort": input}
		sanitizeReasoningEffort(payload, ShapeCN)
		got, exists := payload["reasoning_effort"]
		if want == "" {
			if exists {
				t.Errorf("输入 %q 应被丢弃，却得到 %v", input, got)
			}
			continue
		}
		if got != want {
			t.Errorf("输入 %q 应归一为 %q，得到 %v", input, want, got)
		}
	}

	// 非字符串不能原样透传
	payload := map[string]any{"reasoning_effort": 123}
	sanitizeReasoningEffort(payload, ShapeCN)
	if _, exists := payload["reasoning_effort"]; exists {
		t.Error("非字符串档位应被丢弃")
	}
}

// 「关思考」在两个上游的语义不同，不能统一处理。
//
//   - CN：off 能静默关掉思考；不传反而会思考（默认开）——丢掉 off 等于反效果
//   - intl：off/none 都是 400；关思考只能靠「不传」
func TestSanitizeReasoningEffort_DisableSemanticsPerShape(t *testing.T) {
	for _, level := range []string{"none", "off"} {
		cn := map[string]any{"reasoning_effort": level}
		sanitizeReasoningEffort(cn, ShapeCN)
		if cn["reasoning_effort"] != level {
			t.Errorf("CN 下 %q 应原样透传，得到 %v", level, cn["reasoning_effort"])
		}

		intl := map[string]any{"reasoning_effort": level}
		sanitizeReasoningEffort(intl, ShapeIntl)
		if _, exists := intl["reasoning_effort"]; exists {
			t.Errorf("intl 下 %q 应被丢弃（否则 400），得到 %v", level, intl["reasoning_effort"])
		}
	}
}

// 关思考时不应附加 reasoning_summary（那是「请回传思考过程」的开关）。
func TestShapeIntlPayload_NoReasoningSummaryWhenDisabled(t *testing.T) {
	out := shapeIntlPayload(map[string]any{
		"model":    "m",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if out["reasoning_summary"] != nil {
		t.Errorf("无档位时不应有 reasoning_summary，得到 %v", out["reasoning_summary"])
	}
}

// 非流式聚合要带上 reasoning_content。
//
// 流式路径是原样透传 delta.reasoning_content 的，非流式却只在 content 为空时
// 把思考回填进 content、且从不设置 reasoning_content —— 同一个模型换个客户端形态
// 就看不到思考了。
func TestBuildNonStreamResponse_IncludesReasoningContent(t *testing.T) {
	state := &StreamState{
		ResponseID:     "chatcmpl-test",
		Model:          "deepseek-v4.1-flash",
		ContentParts:   []string{"答案"},
		ReasoningParts: []string{"先看题，", "再推理。"},
	}
	message := buildNonStreamResponse(state)["choices"].([]map[string]any)[0]["message"].(map[string]any)

	if message["reasoning_content"] != "先看题，再推理。" {
		t.Errorf("reasoning_content 缺失或不对: %v", message["reasoning_content"])
	}
	if message["content"] != "答案" {
		t.Errorf("content 被污染: %v", message["content"])
	}

	// 没有思考时不该凭空造一个空字段
	plain := buildNonStreamResponse(&StreamState{ContentParts: []string{"x"}})
	plainMsg := plain["choices"].([]map[string]any)[0]["message"].(map[string]any)
	if _, exists := plainMsg["reasoning_content"]; exists {
		t.Error("无思考时不应输出空 reasoning_content")
	}
}

// 流式不能把思考当正文重复发出去。
//
// 上游每帧都同时带 content 与 reasoning_content 两个键，只是其中一个为空：
//
//	思考阶段：content=""、reasoning 有值
//	回答阶段：content 有值、reasoning=""
//
// 早期实现按帧判断（content 为空就镜像 reasoning），于是思考阶段每一帧的
// reasoning 都被复制进 content，下游拿到 reasoning_content 与 content
// 长度完全相等的结果——同一段文字渲染两遍。
func TestNormalizeChunkForClient_DoesNotDuplicateReasoning(t *testing.T) {
	client := &UpstreamClient{}
	_ = client
	state := &StreamState{ResponseID: "x", Model: "m"}

	chunk := func(delta map[string]any) map[string]any {
		return map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": delta}},
		}
	}

	// 思考阶段的帧：content 是空字符串
	normalizeChunkForClient(chunk(map[string]any{"content": "", "reasoning_content": "先想"}), state)
	normalizeChunkForClient(chunk(map[string]any{"content": "", "reasoning_content": "再想"}), state)

	// 回答阶段的帧：content 有值、reasoning 空
	normalizeChunkForClient(chunk(map[string]any{"content": "答案", "reasoning_content": ""}), state)
	normalizeChunkForClient(chunk(map[string]any{"content": "是9", "reasoning_content": ""}), state)

	if got := strings.Join(state.ContentParts, ""); got != "答案是9" {
		t.Errorf("正文被思考污染: %q（思考内容混进来了）", got)
	}
	if got := strings.Join(state.ReasoningParts, ""); got != "先想再想" {
		t.Errorf("思考丢失: %q", got)
	}
}

// 上游把可见文本全放 reasoning_content 时仍要兜底：
// 整条流没有任何非空 content，应当回填，避免标准客户端收到空白。
func TestNormalizeChunkForClient_MirrorsWhenNoContentAtAll(t *testing.T) {
	state := &StreamState{ResponseID: "x", Model: "m"}
	chunk := map[string]any{
		"choices": []any{map[string]any{"index": 0,
			"delta": map[string]any{"content": "", "reasoning_content": "只有这里才有字"}}},
	}
	normalizeChunkForClient(chunk, state)

	delta := chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if delta["content"] != "只有这里才有字" {
		t.Errorf("全文在 reasoning_content 里时应回填 content，得到 %v", delta["content"])
	}
}
