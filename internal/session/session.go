// Package session 管理多轮对话历史。
//
// eino 的 ReAct agent 本身不保存跨轮状态：每次 Generate/Stream 都要把完整的
// 消息列表传进去。这里负责累积和裁剪历史。
package session

import (
	"errors"
	"fmt"
	"sync"
	"unicode/utf8"

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
	// maxTokens 是历史消息的估算 token 预算。<=0 表示不限。
	maxTokens int
	// cache 不为空时，每次变更都会先原子写入本地缓存，再提交到内存。
	cache *fileCache
}

// New 创建一个会话，maxTurns 为保留轮数。
func New(maxTurns int) *Session {
	return &Session{maxTurns: maxTurns}
}

// Open 创建一个带本地缓存的会话，并在返回前恢复已有历史。
func Open(maxTurns, maxTokens int, cachePath string) (*Session, error) {
	cache := newFileCache(cachePath)
	history, err := cache.Load()
	if err != nil {
		return nil, fmt.Errorf("读取对话缓存: %w", err)
	}

	s := &Session{
		history:   history,
		maxTurns:  maxTurns,
		maxTokens: maxTokens,
		cache:     cache,
	}
	before := len(s.history)
	s.trimLocked()
	if len(s.history) != before {
		if err := s.cache.Save(s.history); err != nil {
			return nil, fmt.Errorf("保存裁剪后的对话缓存: %w", err)
		}
	}
	return s, nil
}

// AppendUser 追加一条用户消息。
func (s *Session) AppendUser(content string) error {
	return s.AppendUserMessage(schema.UserMessage(content))
}

// AppendUserMessage 追加一条可能包含图片等多模态内容的用户消息。
func (s *Session) AppendUserMessage(msg *schema.Message) error {
	if msg == nil || msg.Role != schema.User {
		return errors.New("只能追加 user 消息")
	}
	estimated := estimateMessageTokens(msg)
	if s.maxTokens > 0 && estimated > s.maxTokens {
		return fmt.Errorf("当前消息估算为 %d tokens，超过历史预算 %d", estimated, s.maxTokens)
	}
	return s.append(msg)
}

// AppendAssistant 追加一条助手消息。
func (s *Session) AppendAssistant(content string) error {
	return s.append(schema.AssistantMessage(content, nil))
}

func (s *Session) append(msg *schema.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	previous := s.history
	s.history = append(append([]*schema.Message(nil), previous...), msg)
	s.trimLocked()
	if err := s.persistLocked(); err != nil {
		s.history = previous
		return err
	}
	return nil
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

// EstimatedTokens 返回当前历史占用的估算 token 数。
func (s *Session) EstimatedTokens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return estimateHistoryTokens(s.history)
}

// Reset 清空历史。
func (s *Session) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache != nil {
		if err := s.cache.Save(nil); err != nil {
			return err
		}
	}
	s.history = nil
	return nil
}

// persistLocked 在持锁状态下保存当前快照。
func (s *Session) persistLocked() error {
	if s.cache == nil {
		return nil
	}
	if err := s.cache.Save(s.history); err != nil {
		return fmt.Errorf("写入对话缓存: %w", err)
	}
	return nil
}

// trimLocked 裁掉最旧的完整轮次，保证不超过轮数和 token 预算。调用前必须已持锁。
func (s *Session) trimLocked() {
	if s.maxTurns > 0 {
		for countUserMessages(s.history) > s.maxTurns && s.dropOldestTurnLocked() {
		}
	}
	if s.maxTokens > 0 {
		for estimateHistoryTokens(s.history) > s.maxTokens && s.dropOldestTurnLocked() {
		}
	}
}

// dropOldestTurnLocked 丢弃最旧一轮，但始终保留最近一轮，即使它单独超预算。
func (s *Session) dropOldestTurnLocked() bool {
	for i := 1; i < len(s.history); i++ {
		if s.history[i].Role == schema.User {
			s.history = append([]*schema.Message(nil), s.history[i:]...)
			return true
		}
	}
	return false
}

func countUserMessages(messages []*schema.Message) int {
	count := 0
	for _, msg := range messages {
		if msg.Role == schema.User {
			count++
		}
	}
	return count
}

func estimateHistoryTokens(messages []*schema.Message) int {
	total := 0
	for _, msg := range messages {
		total += estimateMessageTokens(msg)
	}
	return total
}

func estimateMessageTokens(msg *schema.Message) int {
	const (
		messageOverhead    = 8
		imageTokenEstimate = 4096
	)
	if msg == nil {
		return 0
	}

	total := messageOverhead
	if len(msg.UserInputMultiContent) == 0 {
		total += estimateTextTokens(msg.Content)
		return total
	}
	for _, part := range msg.UserInputMultiContent {
		switch part.Type {
		case schema.ChatMessagePartTypeText:
			total += estimateTextTokens(part.Text)
		case schema.ChatMessagePartTypeImageURL:
			total += imageTokenEstimate
		}
	}
	return total
}

// estimateTextTokens 对英文按约 3 bytes/token、对中文按约 1 rune/token 保守估算。
func estimateTextTokens(text string) int {
	byBytes := (len(text) + 2) / 3
	byRunes := utf8.RuneCountInString(text)
	if byRunes > byBytes {
		return byRunes
	}
	return byBytes
}
