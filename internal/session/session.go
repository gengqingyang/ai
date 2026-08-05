// Package session 管理多轮对话历史。
//
// eino 的 ReAct agent 本身不保存跨轮状态：每次 Generate/Stream 都要把完整的
// 消息列表传进去。这里负责累积和裁剪历史。
package session

import (
	"sync"

	"github.com/cloudwego/eino/schema"
)

// Session 保存一条会话的消息历史。并发安全。
type Session struct {
	mu sync.Mutex
	// history 只存 user 和 assistant 的最终消息，不含中间的 tool call 往返
	// （agent 内部自己管那部分），因此裁剪时不会切出孤立的 tool message。
	history []*schema.Message
	// maxTurns 是保留的轮数，一轮 = 一条 user + 一条 assistant。<=0 表示不限。
	maxTurns int
}

// New 创建一个会话，maxTurns 为保留轮数。
func New(maxTurns int) *Session {
	return &Session{maxTurns: maxTurns}
}

// AppendUser 追加一条用户消息。
func (s *Session) AppendUser(content string) {
	s.append(schema.UserMessage(content))
}

// AppendAssistant 追加一条助手消息。
func (s *Session) AppendAssistant(content string) {
	s.append(schema.AssistantMessage(content, nil))
}

func (s *Session) append(msg *schema.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = append(s.history, msg)
	s.trimLocked()
}

// Messages 返回历史消息的快照，可直接交给 agent。
func (s *Session) Messages() []*schema.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*schema.Message, len(s.history))
	copy(out, s.history)
	return out
}

// Len 返回当前历史消息条数。
func (s *Session) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.history)
}

// Reset 清空历史。
func (s *Session) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = nil
}

// trimLocked 裁掉最旧的消息，保证不超过 maxTurns 轮。调用前必须已持锁。
func (s *Session) trimLocked() {
	if s.maxTurns <= 0 {
		return
	}
	limit := s.maxTurns * 2
	if len(s.history) <= limit {
		return
	}
	// 从头丢弃，并保证剩下的第一条是 user 消息，避免历史以 assistant 开头。
	drop := len(s.history) - limit
	for drop < len(s.history) && s.history[drop].Role != schema.User {
		drop++
	}
	s.history = append([]*schema.Message(nil), s.history[drop:]...)
}
