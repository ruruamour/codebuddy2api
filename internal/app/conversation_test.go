package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func turn(model string, messages ...map[string]any) map[string]any {
	items := make([]any, 0, len(messages))
	for _, message := range messages {
		items = append(items, message)
	}
	return map[string]any{"model": model, "messages": items}
}

func msg(role string, content any) map[string]any {
	return map[string]any{"role": role, "content": content}
}

// 一轮对话越问越长，但会话 ID 必须自始至终是同一个——这正是这个模块存在的理由。
func TestConversationTracker_SameConversationKeepsOneID(t *testing.T) {
	tracker := newConversationTracker()
	first := tracker.IDFor(1, turn("m",
		msg("system", "你是助手"),
		msg("user", "3 加 4 等于几"),
	))
	second := tracker.IDFor(1, turn("m",
		msg("system", "你是助手"),
		msg("user", "3 加 4 等于几"),
		msg("assistant", "7"),
		msg("user", "再加 5 呢"),
	))
	third := tracker.IDFor(1, turn("m",
		msg("system", "你是助手"),
		msg("user", "3 加 4 等于几"),
		msg("assistant", "7"),
		msg("user", "再加 5 呢"),
		msg("assistant", "12"),
		msg("user", "再乘 2"),
	))
	if first == "" {
		t.Fatal("认不出会话")
	}
	if first != second || second != third {
		t.Errorf("同一轮对话应共用一个会话 ID，得到 %s / %s / %s", first, second, third)
	}
}

// 换个开头就是另一段对话，不能粘到一起。
func TestConversationTracker_DifferentOpeningIsNewConversation(t *testing.T) {
	tracker := newConversationTracker()
	a := tracker.IDFor(1, turn("m", msg("user", "写个快排")))
	b := tracker.IDFor(1, turn("m", msg("user", "今天天气")))
	if a == b {
		t.Errorf("不同对话不该共用会话 ID: %s", a)
	}
}

// 同一段开头换模型，在官方那边是两个会话。
func TestConversationTracker_ModelSeparatesConversations(t *testing.T) {
	tracker := newConversationTracker()
	a := tracker.IDFor(1, turn("glm-5.1", msg("user", "写个快排")))
	b := tracker.IDFor(1, turn("deepseek-v4.1-flash", msg("user", "写个快排")))
	if a == b {
		t.Errorf("不同模型不该共用会话 ID: %s", a)
	}
}

// 同一段话，一轮发字符串、一轮发 typed block（有的 SDK 会自己规范化），
// 必须算成同一个会话——所以键取的是文本，不是 content 的结构。
func TestConversationTracker_StringAndTypedBlockMatch(t *testing.T) {
	tracker := newConversationTracker()
	plain := tracker.IDFor(1, turn("m", msg("user", "只回复OK")))
	typed := tracker.IDFor(1, turn("m", msg("user", []any{
		map[string]any{"type": "text", "text": "只回复OK"},
	})))
	if plain != typed {
		t.Errorf("字符串与 typed block 应算同一个会话: %s vs %s", plain, typed)
	}
}

// 整形不能影响会话归属：键算在客户端原样发来的请求体上，
// 所以 intl 补的那条前导 system、user 转 typed block 都不该改变结果。
func TestConversationTracker_ShapingDoesNotSplitConversation(t *testing.T) {
	tracker := newConversationTracker()
	// 整形后的样子（有前导 system、content 是 typed block）不该参与算键：
	// StreamChat 传给 IDFor 的是 requestBody，这里就按那条路径给原始体
	raw := turn("m", msg("user", "第一问"))
	before := tracker.IDFor(1, raw)

	after := tracker.IDFor(1, turn("m",
		msg("user", "第一问"),
		msg("assistant", "答"),
		msg("user", "第二问"),
	))
	if before != after {
		t.Errorf("整形不该切断会话: %s vs %s", before, after)
	}
}

// 同一段对话若外溢到另一个号，必须换一个会话 ID：
// 同一个会话 ID 出现在两个不同凭证下，等于自己举报账号共享。
func TestConversationTracker_AccountSeparatesConversations(t *testing.T) {
	tracker := newConversationTracker()
	payload := turn("m", msg("user", "同一个问题"))
	onNine := tracker.IDFor(9, payload)
	onOne := tracker.IDFor(1, payload)
	if onNine == onOne {
		t.Errorf("不同账号不该共用会话 ID: %s", onNine)
	}
}

// 空闲超过 TTL 就是新对话了。
func TestConversationTracker_ExpiresAfterTTL(t *testing.T) {
	clock := time.Now()
	tracker := newConversationTracker()
	tracker.now = func() time.Time { return clock }

	payload := turn("m", msg("user", "问题"))
	first := tracker.IDFor(1, payload)

	clock = clock.Add(conversationTTL - time.Minute)
	if again := tracker.IDFor(1, payload); again != first {
		t.Errorf("还没到 TTL 就换 ID 了: %s vs %s", first, again)
	}
	// 上一次访问刷新了 lastSeen，所以这里要从那一刻再往后推
	clock = clock.Add(conversationTTL + time.Minute)
	if after := tracker.IDFor(1, payload); after == first {
		t.Error("超过 TTL 后应视为新对话")
	}
}

// 记录数不能无限涨。
func TestConversationTracker_EvictsWhenFull(t *testing.T) {
	tracker := newConversationTracker()
	tracker.max = 8
	for i := 0; i < 50; i++ {
		tracker.IDFor(1, turn("m", msg("user", string(rune('a'+i%26))+string(rune('0'+i/26)))))
	}
	tracker.mu.Lock()
	size := len(tracker.entries)
	tracker.mu.Unlock()
	if size > tracker.max {
		t.Errorf("记录数 %d 超过上限 %d", size, tracker.max)
	}
}

// 没有 user 消息就认不出会话，交回调用方现生成，不能返回一个空 ID 上线。
func TestConversationTracker_NoUserMessage(t *testing.T) {
	tracker := newConversationTracker()
	if id := tracker.IDFor(1, turn("m", msg("system", "只有 system"))); id != "" {
		t.Errorf("没有 user 消息时应返回空串，得到 %s", id)
	}
	if id := tracker.IDFor(1, map[string]any{"model": "m"}); id != "" {
		t.Errorf("没有 messages 时应返回空串，得到 %s", id)
	}
}

// 官方发的是 UUIDv7，我们原来发的是 v4——版本号本身就是个指纹。
func TestConversationID_IsUUIDv7(t *testing.T) {
	for _, id := range []string{newConversationID(), NewTraceIDs().ConversationID} {
		parsed, err := uuid.Parse(id)
		if err != nil {
			t.Fatalf("%s 不是合法 UUID: %v", id, err)
		}
		if parsed.Version() != 7 {
			t.Errorf("应为 UUIDv7，得到 v%d (%s)", parsed.Version(), id)
		}
	}
}

// 会话 ID 真的被写进请求头，而不是只在 tracker 里自娱自乐。
func TestBuildHeaders_UsesStickyConversationID(t *testing.T) {
	client := NewUpstreamClient(Config{}, nil)
	account := Account{APIKey: "ck_test"}
	payload := turn("m", msg("user", "问题"))

	seen := map[string]int{}
	for i := 0; i < 3; i++ {
		ids := NewTraceIDs()
		if id := client.conversations.IDFor(account.ID, payload); id != "" {
			ids.ConversationID = id
		}
		headers := client.buildHeaders(account, client.ProfileFor(account), ids)
		seen[headers.Get("X-Conversation-ID")]++
		// 消息级 ID 每次都要变
		if headers.Get("X-Request-ID") != headers.Get("X-Conversation-Message-ID") {
			t.Error("X-Request-ID 与 X-Conversation-Message-ID 应相等")
		}
	}
	if len(seen) != 1 {
		t.Errorf("三次请求应共用一个 X-Conversation-ID，实际出现 %d 个", len(seen))
	}
}

// 端到端：经 StreamChat 真的发出去，同一轮对话的两次请求头里带的是同一个会话 ID，
// 而消息级 ID 每次都换。只验到 buildHeaders 不够——真正上线的那条路径要自己走一遍。
func TestStreamChat_SendsStickyConversationHeader(t *testing.T) {
	var mu sync.Mutex
	var convIDs, requestIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		convIDs = append(convIDs, r.Header.Get("X-Conversation-ID"))
		requestIDs = append(requestIDs, r.Header.Get("X-Request-ID"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := NewUpstreamClient(Config{
		UpstreamURL: srv.URL, RequestTimeoutSeconds: 30, ConnectTimeoutSeconds: 10,
		Models: []string{"m"}, AuthMode: AuthModeAPIKey,
	}, nil)
	account := Account{ID: 1, APIKey: "ck_test"}
	noop := func([]byte, *StreamState) error { return nil }

	client.StreamChat(context.Background(), account,
		turn("m", msg("user", "第一问")), noop)
	client.StreamChat(context.Background(), account,
		turn("m", msg("user", "第一问"), msg("assistant", "答"), msg("user", "第二问")), noop)

	mu.Lock()
	defer mu.Unlock()
	if len(convIDs) != 2 {
		t.Fatalf("应收到 2 个请求，实际 %d 个", len(convIDs))
	}
	if convIDs[0] == "" {
		t.Fatal("没发 X-Conversation-ID")
	}
	if convIDs[0] != convIDs[1] {
		t.Errorf("同一轮对话应共用会话 ID: %s vs %s", convIDs[0], convIDs[1])
	}
	if requestIDs[0] == requestIDs[1] {
		t.Errorf("消息级 X-Request-ID 每次都该换，两次都是 %s", requestIDs[0])
	}
}
