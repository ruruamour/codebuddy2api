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
}

func NewUpstreamClient(cfg Config, types *TypeRegistry) *UpstreamClient {
	if types == nil {
		types = NewTypeRegistry()
	}
	return &UpstreamClient{cfg: cfg, types: types}
}

func (c *UpstreamClient) PreparePayload(body map[string]any, profile AccountProfile) map[string]any {
	payload := cloneMap(body)
	payload["stream"] = true // intl 也强制流式（非流式直接 11101）
	if _, ok := payload["model"]; !ok {
		payload["model"] = c.defaultModel(profile)
	}
	delete(payload, "reasoning_effort")
	if profile.RequestShape == ShapeIntl {
		payload = shapeIntlPayload(payload)
	}
	return payload
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
	// 带 reasoning 参数时官方 CLI 会开 reasoning_summary
	if effort, ok := payload["reasoning_effort"]; ok && effort != nil {
		if text, _ := effort.(string); text != "" && text != "none" && text != "off" {
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
	req.Header = c.BuildHeaders(account)

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
		content, contentOK := delta["content"].(string)
		if contentOK {
			state.ContentParts = append(state.ContentParts, content)
		}
		reasoning, reasoningOK := delta["reasoning_content"].(string)
		if reasoningOK && reasoning != "" {
			state.ReasoningParts = append(state.ReasoningParts, reasoning)
			if !contentOK || content == "" {
				delta["content"] = reasoning
			}
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
	if content == "" {
		content = strings.Join(state.ReasoningParts, "")
	}
	message := map[string]any{
		"role":    "assistant",
		"content": content,
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
