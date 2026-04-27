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
	"strings"
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
}

func NewUpstreamClient(cfg Config) *UpstreamClient {
	return &UpstreamClient{cfg: cfg}
}

func (c *UpstreamClient) PreparePayload(body map[string]any) map[string]any {
	payload := cloneMap(body)
	payload["stream"] = true
	if _, ok := payload["model"]; !ok {
		payload["model"] = "glm-5.1"
	}
	delete(payload, "reasoning_effort")
	return payload
}

func (c *UpstreamClient) BuildHeaders(account Account) http.Header {
	profile := account.HeaderProfile
	if profile == nil {
		profile = map[string]any{}
	}
	headers := http.Header{}
	headers.Set("Accept", "text/event-stream")
	headers.Set("Content-Type", "application/json")
	headers.Set("X-Requested-With", "XMLHttpRequest")
	headers.Set("X-Domain", "copilot.tencent.com")
	headers.Set("X-Product", "SaaS")
	headers.Set("X-Agent-Intent", stringValue(profile["agent_intent"], "CodeCompletion"))
	headers.Set("X-Env-ID", stringValue(profile["env_id"], "production"))
	headers.Set("X-Request-ID", stringValue(profile["request_id"], strings.ReplaceAll(uuid.NewString(), "-", "")))
	headers.Set("X-Machine-Id", stringValue(profile["machine_id"], uuid.NewString()))
	headers.Set("User-Agent", stringValue(profile["user_agent"], "CLI/1.0.8 CodeBuddy/1.0.8"))
	headers.Set("X-Api-Key", account.APIKey)
	if extra, ok := profile["extra_headers"].(map[string]any); ok {
		for key, value := range extra {
			if key != "" && value != nil {
				headers.Set(key, fmt.Sprint(value))
			}
		}
	}
	return headers
}

type StreamCallback func(wire []byte, state *StreamState) error

func (c *UpstreamClient) StreamChat(ctx context.Context, account Account, requestBody map[string]any, callback StreamCallback) (*StreamState, error) {
	payload := c.PreparePayload(requestBody)
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
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.cfg.UpstreamURL, bytes.NewReader(payloadBytes))
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
		"model":      "glm-5.1",
		"messages":   []map[string]any{{"role": "user", "content": "只回复OK"}},
		"stream":     false,
		"max_tokens": 8,
	}
	response, state, err := c.CompleteChat(ctx, account, body)
	if state == nil {
		return response, nil, err
	}
	return response, state.Usage, err
}

func (c *UpstreamClient) httpClient(account Account) (*http.Client, error) {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: time.Duration(c.cfg.ConnectTimeoutSeconds) * time.Second}).DialContext,
		TLSHandshakeTimeout:   time.Duration(c.cfg.ConnectTimeoutSeconds) * time.Second,
		ResponseHeaderTimeout: time.Duration(c.cfg.RequestTimeoutSeconds) * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
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
