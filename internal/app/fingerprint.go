package app

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"

	"github.com/google/uuid"
)

// 官方 CodeBuddy CLI 的请求指纹。
//
// 基线抓自官方 CLI 2.149.0（见 docs/official-request-baseline.md）：官方发 35 个
// 有效头，我们原来只发 10 个，其中 3 个值还是错的、2 个是官方根本不发的。
// 缺失最明显的是两组——OpenAI JS SDK 自带的 x-stainless-*，以及一整套分布式
// 追踪头（W3C traceparent + B3）。这两组基本等于"我是不是真客户端"的签名。

// 官方 CLI 的版本号与它内嵌的 OpenAI JS SDK 版本。
// 官方每次发版这两个值都会变，跟不上就等于自报家门说自己是山寨客户端。
const (
	officialCLIVersion = "2.149.0"
	officialSDKVersion = "6.25.0"
)

// TraceIDs 是一次请求用到的各级 ID。
//
// 官方的 ID 分三层，层与层之间有固定的派生关系（抓包实证）：
//
//	会话级  X-Conversation-ID                          整轮对话固定
//	请求级  X-Conversation-Request-ID == X-Root-Request-ID
//	消息级  X-Request-ID == X-Conversation-Message-ID
//
// 追踪头之间同样互相绑定：
//
//	X-B3-TraceId == X-Trace-ID == traceparent 的 trace 段
//	X-B3-SpanId  == traceparent 的 span 段
type TraceIDs struct {
	ConversationID string // UUIDv7
	RequestID      string // 32 hex
	RootRequestID  string // 32 hex
	TraceID        string // 32 hex
	SpanID         string // 16 hex
	ParentSpanID   string // 16 hex
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// 拿不到随机数时退回 uuid，总比发一个固定值强
		return strings.ReplaceAll(uuid.NewString(), "-", "")[:n*2]
	}
	return hex.EncodeToString(buf)
}

// NewTraceIDs 生成一组互相自洽的 ID。
//
// 会话 ID 这里是现生成的，只适合「一次性」的请求（查余额、诊断）。走对话的请求
// 应当由 conversationTracker 认出所属会话后覆盖它，见 conversation.go。
func NewTraceIDs() TraceIDs {
	return TraceIDs{
		ConversationID: newConversationID(),
		RequestID:      randomHex(16),
		RootRequestID:  randomHex(16),
		TraceID:        randomHex(16),
		SpanID:         randomHex(8),
		ParentSpanID:   randomHex(8),
	}
}

// stainlessOS 把 Go 的 GOOS 映射成 OpenAI JS SDK 上报的写法。
func stainlessOS() string {
	switch runtime.GOOS {
	case "linux":
		return "Linux"
	case "darwin":
		return "MacOS"
	case "windows":
		return "Windows"
	default:
		return strings.ToUpper(runtime.GOOS[:1]) + runtime.GOOS[1:]
	}
}

// stainlessArch 同上，映射 GOARCH。
func stainlessArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	case "386":
		return "x32"
	default:
		return runtime.GOARCH
	}
}

// JWTSubject 解出 JWT 的 sub claim —— 官方的 X-User-Id 就是这个值。
func JWTSubject(token string) string {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Sub
}

// BuildHeaders 按官方基线拼请求头。
//
// header_profile 里的字段可以逐项覆盖，extra_headers 可以追加任意头——
// 官方改版时不用改代码就能先顶上。
func (c *UpstreamClient) BuildHeaders(account Account) http.Header {
	return c.buildHeaders(account, c.ProfileFor(account), NewTraceIDs())
}

func (c *UpstreamClient) buildHeaders(account Account, profile AccountProfile, ids TraceIDs) http.Header {
	hp := account.HeaderProfile
	if hp == nil {
		hp = map[string]any{}
	}
	cliVersion := stringValue(hp["cli_version"], officialCLIVersion)
	sdkVersion := stringValue(hp["sdk_version"], officialSDKVersion)

	h := http.Header{}
	// 官方即使 stream:true 也发 application/json，不是 text/event-stream
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	h.Set("X-Requested-With", "XMLHttpRequest")

	// OpenAI JS SDK（stainless 生成）自带的一组指纹
	h.Set("x-stainless-arch", stringValue(hp["stainless_arch"], stainlessArch()))
	h.Set("x-stainless-lang", "js")
	h.Set("x-stainless-os", stringValue(hp["stainless_os"], stainlessOS()))
	h.Set("x-stainless-package-version", sdkVersion)
	h.Set("x-stainless-retry-count", "0")
	h.Set("x-stainless-runtime", "node")
	h.Set("x-stainless-runtime-version", stringValue(hp["node_version"], "v22.22.0"))

	// 会话 / agent 身份
	h.Set("X-Conversation-ID", ids.ConversationID)
	h.Set("X-Agent-Intent", stringValue(hp["agent_intent"], "craft"))
	h.Set("X-Agent-Purpose", stringValue(hp["agent_purpose"], "conversation"))
	h.Set("X-IDE-Type", stringValue(hp["ide_type"], "CLI"))
	h.Set("X-IDE-Name", stringValue(hp["ide_name"], "CLI"))
	h.Set("X-IDE-Version", cliVersion)
	h.Set("X-Private-Data", "false")
	h.Set("x-codebuddy-request", "1")

	// 三层请求 ID
	h.Set("X-Request-ID", ids.RequestID)
	h.Set("X-Conversation-Message-ID", ids.RequestID)
	h.Set("X-Conversation-Request-ID", ids.RootRequestID)
	h.Set("X-Root-Request-ID", ids.RootRequestID)
	h.Set("X-Agent-Type", stringValue(hp["agent_type"], "main"))

	// 分布式追踪：W3C traceparent + B3，两套值互相对应
	h.Set("traceparent", fmt.Sprintf("00-%s-%s-01", ids.TraceID, ids.SpanID))
	h.Set("b3", fmt.Sprintf("%s-%s-1-%s", ids.TraceID, ids.SpanID, ids.ParentSpanID))
	h.Set("X-B3-TraceId", ids.TraceID)
	h.Set("X-B3-ParentSpanId", ids.ParentSpanID)
	h.Set("X-B3-SpanId", ids.SpanID)
	h.Set("X-B3-Sampled", "1")
	h.Set("X-Trace-ID", ids.TraceID)

	// 认证 + 身份
	if profile.IsBearer() {
		h.Set("Authorization", "Bearer "+account.APIKey)
		// 官方的 X-User-Id 就是 access token 里的 sub
		if sub := stringValue(hp["user_id"], JWTSubject(account.APIKey)); sub != "" {
			h.Set("X-User-Id", sub)
		}
	} else {
		h.Set("X-Api-Key", account.APIKey)
		if sub := stringValue(hp["user_id"], ""); sub != "" {
			h.Set("X-User-Id", sub)
		}
	}

	h.Set("X-Domain", profile.Domain)
	h.Set("X-Product", stringValue(hp["product"], "SaaS"))
	h.Set("User-Agent", stringValue(hp["user_agent"],
		fmt.Sprintf("CLI/%s CodeBuddy/%s", cliVersion, cliVersion)))

	if extra, ok := hp["extra_headers"].(map[string]any); ok {
		for key, value := range extra {
			if key != "" && value != nil {
				h.Set(key, fmt.Sprint(value))
			}
		}
	}
	return h
}
