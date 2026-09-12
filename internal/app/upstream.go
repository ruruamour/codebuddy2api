package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/proxy"
)

type StreamState struct {
	ResponseID     string
	Model          string
	ContentParts   []string
	ReasoningParts []string
	ToolCalls      []map[string]any
	FinishReason   string
	Usage          map[string]any
	SawDone        bool
	// SawContent / SawReasoningContent 记录整条响应里是否出现过非空文本，
	// 用来决定「上游把可见文本全放在 reasoning_content 里」时要不要回填，
	// 见 normalizeChunkForClient。按帧判断会误伤正常的「先思考后回答」流。
	SawContent          bool
	SawReasoningContent bool
}

type UpstreamStatusError struct {
	StatusCode int
	Body       string
}

func (e UpstreamStatusError) Error() string {
	return fmt.Sprintf("upstream returned %d: %s", e.StatusCode, truncate(e.Body, 300))
}

type UpstreamClient struct {
	cfg Config
	// clients 按代理配置缓存 http.Client。
	//
	// 以前每个请求都新建一个 Transport，实测 20 个请求就开 20 条 TCP 连接、零复用：
	// 每次都要重做一遍 TLS 握手，而且手工构造的 Transport 没设 IdleConnTimeout，
	// 空闲连接永不关闭，跑久了就是 socket 泄漏。
	// Transport 本身是并发安全的，按代理串缓存即可（其余参数都来自固定的 cfg）。
	clients sync.Map // proxyURL string -> *http.Client
	// types 是账号类型缓存，ProfileFor 每次都要查它，不能走库。
	types *TypeRegistry
	// conversations 把同一轮对话的多次请求认成一个会话，见 conversation.go。
	conversations *conversationTracker
}

func NewUpstreamClient(cfg Config, types *TypeRegistry) *UpstreamClient {
	if types == nil {
		types = NewTypeRegistry()
	}
	return &UpstreamClient{cfg: cfg, types: types, conversations: newConversationTracker()}
}

func (c *UpstreamClient) PreparePayload(body map[string]any, profile AccountProfile) map[string]any {
	payload := cloneMap(body)
	payload["stream"] = true // intl 也强制流式（非流式直接 11101）
	if _, ok := payload["model"]; !ok {
		payload["model"] = c.defaultModel(profile)
	}
	sanitizeReasoningEffort(payload, profile.RequestShape)
	if profile.RequestShape == ShapeIntl {
		payload = shapeIntlPayload(payload)
	}
	return payload
}

// supportedReasoningEfforts 是上游实际接受的思考档位。
//
// 逐值穷举实测（CN 与 intl 都测了）：这 6 个小写值是两个网关的交集，都接受。
// 其余大小写变体一律 400：
//
//	11150 invalid_reasoning_effort "not supported by the current model"
var supportedReasoningEfforts = map[string]struct{}{
	"minimal": {}, "low": {}, "medium": {},
	"high": {}, "xhigh": {}, "max": {},
}

// disableReasoningEfforts 是客户端用来表达「关思考」的写法。
//
// 两个上游对它的处理**不一致**：
//   - CN (copilot.tencent.com)：off 能静默关掉思考；不传反而会思考（默认开）
//   - intl (www.codebuddy.ai)：off/none 都是 400；关思考只能靠「不传」
//
// 所以不能统一成「一律丢弃」：那会让 CN 的关思考请求反而开始思考。
var disableReasoningEfforts = map[string]struct{}{
	"none": {}, "off": {}, "disabled": {}, "false": {},
}

// sanitizeReasoningEffort 归一化 reasoning_effort：
//   - 大小写变体折叠成小写（客户端可能送 "Max"）
//   - 未知值直接丢弃（宁可不思考，也不要整个请求 400）
//   - 非字符串丢弃
//   - 关思考档位按上游能力决定「原样透传」还是「丢弃」，见 disableReasoningEfforts
//
// 不能无条件 delete：上游靠这个字段决定思考档位，删了就等于强制关思考，
// 而且 shapeIntlPayload 还要读它来决定是否加 reasoning_summary。
func sanitizeReasoningEffort(payload map[string]any, shape string) {
	raw, ok := payload["reasoning_effort"]
	if !ok {
		return
	}
	text, isString := raw.(string)
	if !isString {
		delete(payload, "reasoning_effort")
		return
	}
	effort := strings.ToLower(strings.TrimSpace(text))
	if effort == "" {
		delete(payload, "reasoning_effort")
		return
	}
	if _, disable := disableReasoningEfforts[effort]; disable {
		if shape == ShapeIntl {
			// intl 会 400，只能靠不传来关思考
			delete(payload, "reasoning_effort")
			return
		}
		// CN 认 off，原样透传才能真的关掉
		payload["reasoning_effort"] = effort
		return
	}
	if _, supported := supportedReasoningEfforts[effort]; !supported {
		delete(payload, "reasoning_effort")
		return
	}
	payload["reasoning_effort"] = effort
}

func (c *UpstreamClient) defaultModel(profile AccountProfile) string {
	if len(profile.Models) > 0 && profile.Models[0] != "" {
		return profile.Models[0]
	}
	return "glm-5.1"
}

// toAnySlice 把任意切片规整成 []any。
//
// 从 HTTP 进来的请求经过 JSON 解码，messages 一定是 []any；但服务内部自己构造的
// 请求（Probe、测试）写的是 []map[string]any，直接断言 []any 会失败并静默当成空数组——
// 结果就是整条用户消息被丢掉，只发一条前导 system。
func toAnySlice(value any) []any {
	if value == nil {
		return nil
	}
	if items, ok := value.([]any); ok {
		return items
	}
	rv := reflect.ValueOf(value)
	if rv.Kind() != reflect.Slice {
		return nil
	}
	items := make([]any, rv.Len())
	for i := range items {
		items[i] = rv.Index(i).Interface()
	}
	return items
}

// shapeIntlPayload 把客户端发来的普通 OpenAI 请求改成 CodeBuddy 国际网关能吃的形状。
//
// 实测不整形的话：
//   - 首条不是 system   → 11128 first message is not system prompt
//   - user content 是字符串 → 同样会在解析阶段被拒
//
// 官方 CLI 的请求永远带一条前导 system，且 content 是 typed block 数组。
func shapeIntlPayload(payload map[string]any) map[string]any {
	messages := toAnySlice(payload["messages"])
	shaped := make([]any, 0, len(messages)+1)
	seenSystem := false
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := message["role"].(string)
		switch role {
		case "system", "developer":
			// 只保留第一条 system（网关只认前导 system），后面的丢弃
			if seenSystem {
				continue
			}
			role = "system"
			seenSystem = true
			message = cloneMap(message)
			message["role"] = role
		case "user":
			// 字符串 content 要转成 typed block，否则国际网关解析失败
			if text, ok := message["content"].(string); ok {
				message = cloneMap(message)
				message["content"] = []any{map[string]any{"type": "text", "text": text}}
			}
		}
		shaped = append(shaped, message)
	}
	if !seenSystem {
		shaped = append([]any{map[string]any{"role": "system", "content": codeBuddySystemPrompt}}, shaped...)
	}
	payload["messages"] = shaped
	// 带 reasoning 参数时官方 CLI 会同时开 reasoning_summary，
	// 否则即便 effort 生效，上游也不会把思考过程写进 reasoning_content。
	// 走到这里 reasoning_effort 已经过校验：intl 下的关思考档位已被删掉，
	// 所以非空即代表真的要思考；支持档位都不在 disableReasoningEfforts 里。
	if effort, ok := payload["reasoning_effort"].(string); ok && effort != "" {
		if _, disable := disableReasoningEfforts[effort]; !disable {
			payload["reasoning_summary"] = "auto"
		}
	}
	return payload
}

// codeBuddySystemPrompt 与官方 CLI 的前导 system 一致。
const codeBuddySystemPrompt = "You are CodeBuddy Code."

type StreamCallback func(wire []byte, state *StreamState) error

func (c *UpstreamClient) StreamChat(ctx context.Context, account Account, requestBody map[string]any, callback StreamCallback) (*StreamState, error) {
	profile := c.ProfileFor(account)
	payload := c.PreparePayload(requestBody, profile)
	model := stringValue(requestBody["model"], stringValue(payload["model"], "glm-5.1"))
	state := &StreamState{
		ResponseID: "chatcmpl-" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		Model:      model,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return state, err
	}

	reqCtx, cancel := context.WithTimeout(ctx, time.Duration(c.cfg.RequestTimeoutSeconds)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, profile.UpstreamURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return state, err
	}
	// 对话请求要认会话：同一轮对话的每次请求共用一个 X-Conversation-ID。
	// 用 requestBody 而不是整形后的 payload，理由见 conversationKey。
	ids := NewTraceIDs()
	if id := c.conversations.IDFor(account.ID, requestBody); id != "" {
		ids.ConversationID = id
	}
	req.Header = c.buildHeaders(account, profile, ids)

	client, err := c.httpClient(account)
	if err != nil {
		return state, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return state, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return state, UpstreamStatusError{StatusCode: resp.StatusCode, Body: string(body)}
	}

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			data := parseSSEDataLine(line)
			if data != "" {
				if data == "[DONE]" {
					state.SawDone = true
					if err := callback([]byte("data: [DONE]\n\n"), state); err != nil {
						return state, err
					}
				} else {
					var chunk map[string]any
					if json.Unmarshal([]byte(data), &chunk) == nil {
						normalizeChunkForClient(chunk, state)
						wire, _ := json.Marshal(chunk)
						wire = append([]byte("data: "), wire...)
						wire = append(wire, '\n', '\n')
						if err := callback(wire, state); err != nil {
							return state, err
						}
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return state, err
		}
	}
	return state, nil
}

func (c *UpstreamClient) CompleteChat(ctx context.Context, account Account, requestBody map[string]any) (map[string]any, *StreamState, error) {
	state, err := c.StreamChat(ctx, account, requestBody, func(_ []byte, _ *StreamState) error { return nil })
	if state == nil {
		state = &StreamState{
			ResponseID: "chatcmpl-" + strings.ReplaceAll(uuid.NewString(), "-", ""),
			Model:      stringValue(requestBody["model"], "glm-5.1"),
		}
	}
	return buildNonStreamResponse(state), state, err
}

func (c *UpstreamClient) Probe(ctx context.Context, account Account) (map[string]any, map[string]any, error) {
	body := map[string]any{
		"model":      c.defaultModel(c.ProfileFor(account)),
		"messages":   []map[string]any{{"role": "user", "content": "只回复OK"}},
		"stream":     true, // 必须流式，intl 对非流式直接 11101
		"max_tokens": 8,
	}
	response, state, err := c.CompleteChat(ctx, account, body)
	if state == nil {
		return response, nil, err
	}
	return response, state.Usage, err
}

func (c *UpstreamClient) httpClient(account Account) (*http.Client, error) {
	key := account.ProxyString()
	if cached, ok := c.clients.Load(key); ok {
		return cached.(*http.Client), nil
	}
	client, err := c.newHTTPClient(account)
	if err != nil {
		return nil, err
	}
	// 并发建了同一把 key 时留先到的那个，避免同一代理存在两份连接池
	actual, _ := c.clients.LoadOrStore(key, client)
	return actual.(*http.Client), nil
}

func (c *UpstreamClient) newHTTPClient(account Account) (*http.Client, error) {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: time.Duration(c.cfg.ConnectTimeoutSeconds) * time.Second}).DialContext,
		TLSHandshakeTimeout:   time.Duration(c.cfg.ConnectTimeoutSeconds) * time.Second,
		ResponseHeaderTimeout: time.Duration(c.cfg.RequestTimeoutSeconds) * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// 手工构造的 Transport 这两项零值意味着「不限制」，必须显式设，
		// 否则空闲连接永不回收
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		ForceAttemptHTTP2:   true,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
	}
	if account.ProxyString() != "" {
		proxyURL, err := url.Parse(account.ProxyString())
		if err != nil {
			return nil, err
		}
		switch proxyURL.Scheme {
		case "http", "https":
			transport.Proxy = http.ProxyURL(proxyURL)
		case "socks5", "socks5h":
			dialer, err := proxy.FromURL(proxyURL, proxy.Direct)
			if err != nil {
				return nil, err
			}
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				type contextDialer interface {
					DialContext(context.Context, string, string) (net.Conn, error)
				}
				if cd, ok := dialer.(contextDialer); ok {
					return cd.DialContext(ctx, network, address)
				}
				return dialer.Dial(network, address)
			}
			transport.Proxy = nil
		default:
			return nil, fmt.Errorf("unsupported proxy scheme: %s", proxyURL.Scheme)
		}
	}
	return &http.Client{Transport: transport}, nil
}

func parseSSEDataLine(line string) string {
	stripped := strings.TrimSpace(line)
	if stripped == "" || !strings.HasPrefix(stripped, "data:") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(stripped, "data:"))
}

func normalizeChunkForClient(chunk map[string]any, state *StreamState) {
	if _, ok := chunk["id"]; !ok || chunk["id"] == "" {
		chunk["id"] = state.ResponseID
	}
	chunk["model"] = state.Model
	if _, ok := chunk["object"]; !ok {
		chunk["object"] = "chat.completion.chunk"
	}
	if _, ok := chunk["created"]; !ok {
		chunk["created"] = time.Now().Unix()
	}
	if usage, ok := chunk["usage"].(map[string]any); ok {
		state.Usage = usage
	}
	choices, ok := chunk["choices"].([]any)
	if !ok {
		return
	}
	for _, item := range choices {
		choice, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if finish := stringValue(choice["finish_reason"], ""); finish != "" {
			state.FinishReason = finish
		}
		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			continue
		}
		if content, ok := delta["content"].(string); ok && content != "" {
			state.ContentParts = append(state.ContentParts, content)
			state.SawContent = true
		}
		reasoning, reasoningOK := delta["reasoning_content"].(string)
		if reasoningOK && reasoning != "" {
			state.ReasoningParts = append(state.ReasoningParts, reasoning)
			state.SawReasoningContent = true
		}
		// 镜像判断不能只看当前帧。
		//
		// 上游（CN 与 intl 都一样）每帧都同时带 content 与 reasoning_content 两个键，
		// 只是其中一个为空：思考阶段 content=""、reasoning 有值；回答阶段反过来。
		// 按帧镜像会把思考阶段每一帧的 reasoning 都复制进 content，
		// 于是下游把同一段文字当正文再渲染一遂（实测 reasoning_content 与 content
		// 长度完全相等的情况）。
		//
		// 这里只在「整条响应至今没出现过任何 content」时才回填，
		// 对齐 Python 原版的意图：兼容那些把可见文本全放在 reasoning_content
		// 里的上游，让标准 OpenAI 客户端不至于只收到空白。
		if !state.SawContent && reasoningOK && reasoning != "" {
			delta["content"] = reasoning
		}
		mergeToolCalls(&state.ToolCalls, delta["tool_calls"])
	}
}

func mergeToolCalls(target *[]map[string]any, incoming any) {
	items, ok := incoming.([]any)
	if !ok {
		return
	}
	for _, item := range items {
		call, ok := item.(map[string]any)
		if !ok {
			continue
		}
		index := int(int64Value(call["index"]))
		for len(*target) <= index {
			*target = append(*target, map[string]any{"id": "", "type": "function", "function": map[string]any{"name": "", "arguments": ""}})
		}
		current := (*target)[index]
		if id := stringValue(call["id"], ""); id != "" {
			current["id"] = id
		}
		if callType := stringValue(call["type"], ""); callType != "" {
			current["type"] = callType
		}
		function, ok := call["function"].(map[string]any)
		if !ok {
			continue
		}
		currentFunction, ok := current["function"].(map[string]any)
		if !ok {
			currentFunction = map[string]any{"name": "", "arguments": ""}
			current["function"] = currentFunction
		}
		if name := stringValue(function["name"], ""); name != "" {
			currentFunction["name"] = stringValue(currentFunction["name"], "") + name
		}
		if args := stringValue(function["arguments"], ""); args != "" {
			currentFunction["arguments"] = stringValue(currentFunction["arguments"], "") + args
		}
	}
}

func buildNonStreamResponse(state *StreamState) map[string]any {
	content := strings.Join(state.ContentParts, "")
	reasoning := strings.Join(state.ReasoningParts, "")
	if content == "" {
		content = reasoning
	}
	message := map[string]any{
		"role":    "assistant",
		"content": content,
	}
	// 流式路径是原样透传 delta.reasoning_content 的，非流式聚合以前却没带上这个字段，
	// 于是同一个模型用非流式客户端就永远看不到思考过程。这里补齐，与流式对齐。
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if len(state.ToolCalls) > 0 {
		message["tool_calls"] = state.ToolCalls
		if content == "" {
			message["content"] = nil
		}
	}
	usage := state.Usage
	if usage == nil {
		usage = map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	}
	finishReason := state.FinishReason
	if finishReason == "" {
		finishReason = "stop"
	}
	return map[string]any{
		"id":      state.ResponseID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   state.Model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		"usage": usage,
	}
}

func cloneMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func stringValue(value any, fallback string) string {
	if value == nil {
		return fallback
	}
	if text, ok := value.(string); ok && text != "" {
		return text
	}
	return fmt.Sprint(value)
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
