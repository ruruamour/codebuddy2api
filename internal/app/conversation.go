package app

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// 会话标识的粘性。
//
// 官方 CLI 一轮对话从头到尾复用同一个 X-Conversation-ID：用户问十轮，十次请求
// 带的是同一个 ID。我们原来每次请求现生成一个，于是上游看到的是「每条消息都开一个
// 新会话、每个会话只有一轮」——这个形状任何按会话聚合的统计都能一眼看出来，
// 而且是纯行为特征，比 TLS 指纹便宜得多也常用得多。
//
// 难点在于 OpenAI 协议本身是无状态的：客户端每轮把完整历史重发一遍，不给会话 ID。
// 所以这里反过来从历史里认会话——同一轮对话的每次请求，开头那段是不变的：
//
//	第 1 轮  [sys, u1]
//	第 2 轮  [sys, u1, a1, u2]
//	第 3 轮  [sys, u1, a1, u2, a2, u3]
//
// 取「前导 system + 第一条 user」做键，三次算出来是同一个键，于是复用同一个 ID。
// 之所以不把整个历史都算进去，正是因为历史每轮都在变，那样等于没做。
//
// 键会碰撞：两段开头一模一样的对话会共用一个会话 ID。这不是缺陷——真实 CLI 里
// 同一个项目反复问同类问题本来就该落在一个会话里，碰撞的结果恰好更像真的。

const (
	// 空闲多久算这轮对话结束。官方 CLI 一个会话能开一整天，但我们只能靠空闲判断；
	// 放太长会把明显无关的两段对话粘成一个，2 小时是个能覆盖正常编码节奏的值。
	conversationTTL = 2 * time.Hour
	// 记录上限。一条记录几十字节，1024 条够高并发用，超了按最久未用淘汰。
	conversationMax = 1024
)

type conversationEntry struct {
	id       string
	lastSeen time.Time
}

type conversationTracker struct {
	mu      sync.Mutex
	entries map[string]conversationEntry
	ttl     time.Duration
	max     int
	now     func() time.Time // 测试注入
}

func newConversationTracker() *conversationTracker {
	return &conversationTracker{
		entries: map[string]conversationEntry{},
		ttl:     conversationTTL,
		max:     conversationMax,
		now:     time.Now,
	}
}

// IDFor 返回这次请求该用的会话 ID：同一轮对话复用，新对话新开。
// 认不出会话形状（没有任何 user 消息）时返回空串，由调用方现生成一个。
//
// payload 要传客户端原样发来的请求体，不是整形后的：整形只对 intl 生效，拿整形后的
// 算键会让同一段对话在不同渠道上算出不同的键。
func (t *conversationTracker) IDFor(accountID int64, payload map[string]any) string {
	if t == nil {
		return ""
	}
	key := conversationKey(accountID, payload)
	if key == "" {
		return ""
	}
	now := t.now()

	t.mu.Lock()
	defer t.mu.Unlock()
	if entry, ok := t.entries[key]; ok && now.Sub(entry.lastSeen) < t.ttl {
		entry.lastSeen = now
		t.entries[key] = entry
		return entry.id
	}
	t.evictLocked(now)
	id := newConversationID()
	t.entries[key] = conversationEntry{id: id, lastSeen: now}
	return id
}

// evictLocked 先清过期的，还是满就淘汰最久未用的那条。
func (t *conversationTracker) evictLocked(now time.Time) {
	if len(t.entries) < t.max {
		return
	}
	for key, entry := range t.entries {
		if now.Sub(entry.lastSeen) >= t.ttl {
			delete(t.entries, key)
		}
	}
	for len(t.entries) >= t.max {
		var oldestKey string
		var oldest time.Time
		for key, entry := range t.entries {
			if oldestKey == "" || entry.lastSeen.Before(oldest) {
				oldestKey, oldest = key, entry.lastSeen
			}
		}
		if oldestKey == "" {
			return
		}
		delete(t.entries, oldestKey)
	}
}

// newConversationID 生成 UUIDv7——官方发的就是 v7，不是 v4。
// v7 前 48 位是毫秒时间戳，和 v4 一眼就能分辨。
func newConversationID() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}

// conversationKey 从请求体里取「前导 system + 第一条 user」作为会话指纹。
//
// 账号也算进去，而且是必须的：同一段对话可能因为并发打满外溢到另一个号，若两边共用
// 一个会话 ID，上游看到的就是「同一个会话挂在两个不同凭证下」——那比每次换新 ID
// 可疑得多，等于自己举报账号共享。
//
// 模型同样算进去：同一段开头换个模型问，在官方那边本来就是两个会话。
func conversationKey(accountID int64, payload map[string]any) string {
	messages := toAnySlice(payload["messages"])
	if len(messages) == 0 {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(strconv.FormatInt(accountID, 10)))
	h.Write([]byte{0})
	h.Write([]byte(stringValue(payload["model"], "")))
	h.Write([]byte{0})

	var haveSystem, haveUser bool
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch role, _ := message["role"].(string); role {
		case "system", "developer":
			// 只认前导 system；夹在中间的那条不算会话身份的一部分
			if haveSystem || haveUser {
				continue
			}
			haveSystem = true
			h.Write([]byte("system\x00"))
			h.Write([]byte(messageText(message["content"])))
			h.Write([]byte{0})
		case "user":
			haveUser = true
			h.Write([]byte("user\x00"))
			h.Write([]byte(messageText(message["content"])))
			h.Write([]byte{0})
		}
		if haveUser {
			break
		}
	}
	if !haveUser {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// messageText 把 content 抽成纯文本。
//
// 必须同时吃字符串和 typed block 数组，而且两者要得到同样的结果：客户端第一轮发的是
// 字符串，经 shapeIntlPayload 整形后变成 typed block，若两种形态算出不同的键，
// 整个粘性就白做了。
func messageText(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case nil:
		return ""
	}
	blocks := toAnySlice(content)
	if blocks == nil {
		return ""
	}
	var b strings.Builder
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := block["text"].(string); ok {
			b.WriteString(text)
		}
	}
	return b.String()
}
