package app

import (
	"encoding/json"
	"testing"
)

// tool_choice 必须归一化成上游认的字符串形态。
//
// 上游把该字段声明成 string，OpenAI 允许的对象形态会让整条请求 400 code 11101
// （"cannot unmarshal object into Go struct field Request.tool_choice of type string"）。
// 改造前线上 13%–16% 的请求死在这条上。
func TestNormalizeToolChoice(t *testing.T) {
	cases := []struct {
		name     string
		in       any
		present  bool
		expected any
		// keepTools=false 表示 tools/functions 必须被删掉
		keepTools bool
	}{
		{
			name:      "对象形态的 function 转成函数名",
			in:        map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}},
			present:   true,
			expected:  "get_weather",
			keepTools: true,
		},
		{
			name:      "name 在外层也能认",
			in:        map[string]any{"type": "function", "name": "search"},
			present:   true,
			expected:  "search",
			keepTools: true,
		},
		{
			name:      "function 但没有名字退回 auto，绝不能 400",
			in:        map[string]any{"type": "function", "function": map[string]any{}},
			present:   true,
			expected:  "auto",
			keepTools: true,
		},
		{
			name:      "auto 对象转字符串",
			in:        map[string]any{"type": "auto"},
			present:   true,
			expected:  "auto",
			keepTools: true,
		},
		{
			name:      "required 对象转字符串",
			in:        map[string]any{"type": "required"},
			present:   true,
			expected:  "required",
			keepTools: true,
		},
		{
			name:      "字符串形态原样保留",
			in:        "required",
			present:   true,
			expected:  "required",
			keepTools: true,
		},
		{
			name:      "对象形态的 none 要连工具一起删",
			in:        map[string]any{"type": "none"},
			present:   false,
			expected:  nil,
			keepTools: false,
		},
		{
			name:      "字符串 none 同理",
			in:        "none",
			present:   false,
			expected:  nil,
			keepTools: false,
		},
		{
			name:      "未知对象直接丢掉字段",
			in:        map[string]any{"type": "weird"},
			present:   false,
			expected:  nil,
			keepTools: true,
		},
		{
			name:      "非标量（数组）也丢掉",
			in:        []any{"auto"},
			present:   false,
			expected:  nil,
			keepTools: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{
				"model":       "m",
				"messages":    []any{},
				"tool_choice": tc.in,
				"tools":       []any{map[string]any{"type": "function"}},
				"functions":   []any{map[string]any{"name": "f"}},
			}
			normalizeToolChoice(payload)

			actual, present := payload["tool_choice"]
			if present != tc.present {
				t.Fatalf("tool_choice 是否保留：期望 %v，实际 %v（值 %v）", tc.present, present, actual)
			}
			if tc.present && actual != tc.expected {
				t.Errorf("tool_choice = %v，期望 %v", actual, tc.expected)
			}
			_, hasTools := payload["tools"]
			_, hasFunctions := payload["functions"]
			if tc.keepTools && (!hasTools || !hasFunctions) {
				t.Errorf("不该删工具定义：tools=%v functions=%v", hasTools, hasFunctions)
			}
			if !tc.keepTools && (hasTools || hasFunctions) {
				t.Errorf("tool_choice=none 必须删掉工具定义，否则模型照样吐 tool_calls")
			}
			// 处理之后必须能序列化成功，且 tool_choice 是字符串（上游的 Go 字段类型）
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("序列化失败: %v", err)
			}
			var round map[string]any
			if err := json.Unmarshal(raw, &round); err != nil {
				t.Fatalf("反序列化失败: %v", err)
			}
			if value, ok := round["tool_choice"]; ok {
				if _, isString := value.(string); !isString {
					t.Errorf("归一化后仍不是字符串: %T %v", value, value)
				}
			}
		})
	}
}

// PreparePayload 是真正的入口：确认它在两条 shape 上都调到了归一化。
func TestPreparePayloadNormalizesToolChoice(t *testing.T) {
	client := NewUpstreamClient(Config{
		UpstreamURL:  "https://copilot.tencent.com/v2/chat/completions",
		Models:       []string{"glm-5.1"},
		AuthMode:     AuthModeAPIKey,
		RequestShape: ShapeCN,
		Domain:       "www.codebuddy.cn",
	}, nil)
	for _, shape := range []string{ShapeCN, ShapeIntl} {
		t.Run(shape, func(t *testing.T) {
			account := Account{ID: 1, APIKey: "ck_test"}
			profile := AccountProfile{
				AuthMode:     AuthModeAPIKey,
				UpstreamURL:  client.cfg.UpstreamURL,
				Domain:       "www.codebuddy.cn",
				RequestShape: shape,
				Models:       []string{"glm-5.1"},
			}
			out := client.PreparePayload(map[string]any{
				"model":    "glm-5.1",
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
				"tool_choice": map[string]any{
					"type":     "function",
					"function": map[string]any{"name": "lookup"},
				},
			}, profile)
			if got, _ := out["tool_choice"].(string); got != "lookup" {
				t.Fatalf("shape=%s 的 tool_choice = %v，期望 lookup", shape, out["tool_choice"])
			}
			_ = account
		})
	}
}
