package approval

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Store 保存提案并记录审计日志。并发安全。
//
// 当前是内存实现：进程重启后待审提案会丢。实时对话场景下这可以接受
// （审核就在当前会话里当场完成）。要支持异步/跨会话审核时，把这里换成
// 数据库实现即可，上层接口不用变。
type Store struct {
	mu    sync.Mutex
	byID  map[string]*Proposal
	order []string
	seq   int

	// auditPath 为空表示不落审计文件（仅内存）。
	auditPath string
	// now 可注入，便于测试。
	now func() time.Time
}

// Option 是 Store 的可选配置。
type Option func(*Store)

// WithAuditLog 指定审计日志路径，每次状态流转追加一行 JSON。
func WithAuditLog(path string) Option {
	return func(s *Store) { s.auditPath = path }
}

// WithClock 注入时钟，仅用于测试。
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// NewStore 创建提案存储。
func NewStore(opts ...Option) *Store {
	s := &Store{
		byID: make(map[string]*Proposal),
		now:  time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Create 登记一条待审提案。
func (s *Store) Create(toolName, args string) *Proposal {
	s.mu.Lock()
	s.seq++
	p := &Proposal{
		ID:        fmt.Sprintf("P%03d", s.seq),
		Tool:      toolName,
		Args:      args,
		Status:    StatusPending,
		CreatedAt: s.now(),
	}
	s.byID[p.ID] = p
	s.order = append(s.order, p.ID)
	snapshot := p.clone()
	s.mu.Unlock()

	s.writeAudit("created", snapshot)
	return snapshot
}

// Get 按 ID 取提案副本。
func (s *Store) Get(id string) (*Proposal, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byID[id]
	if !ok {
		return nil, false
	}
	return p.clone(), true
}

// Pending 返回全部待审提案，按创建顺序。
func (s *Store) Pending() []*Proposal {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Proposal
	for _, id := range s.order {
		if p := s.byID[id]; p.Pending() {
			out = append(out, p.clone())
		}
	}
	return out
}

// All 返回全部提案，按创建顺序。
func (s *Store) All() []*Proposal {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Proposal, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.byID[id].clone())
	}
	return out
}

// Approve 批准提案，记录批准人。只有待审状态可批准。
// 批准只是拿到执行许可，真正执行由 tools.Gate 触发。
func (s *Store) Approve(id, decider string) (*Proposal, error) {
	snapshot, err := s.transition(id, StatusPending, func(p *Proposal) {
		p.Status = StatusApproved
		p.Decider = decider
		s.stampDecision(p)
	})
	if err != nil {
		return nil, err
	}
	s.writeAudit("approved", snapshot)
	return snapshot, nil
}

// Reject 驳回提案。只有待审状态可驳回。
func (s *Store) Reject(id, decider, reason string) (*Proposal, error) {
	snapshot, err := s.transition(id, StatusPending, func(p *Proposal) {
		p.Status = StatusRejected
		p.Decider = decider
		p.RejectReason = reason
		s.stampDecision(p)
	})
	if err != nil {
		return nil, err
	}
	s.writeAudit("rejected", snapshot)
	return snapshot, nil
}

// MarkExecuted 记录执行结果。只有已批准状态可流转到这里。
// execErr 非空表示执行失败。
func (s *Store) MarkExecuted(id, result string, execErr error) (*Proposal, error) {
	snapshot, err := s.transition(id, StatusApproved, func(p *Proposal) {
		p.Result = result
		if execErr != nil {
			p.Status = StatusFailed
			p.Error = execErr.Error()
		} else {
			p.Status = StatusExecuted
		}
	})
	if err != nil {
		return nil, err
	}
	s.writeAudit(string(snapshot.Status), snapshot)
	return snapshot, nil
}

// transition 校验前置状态后改动提案，返回改完后的副本。
// 前置状态校验是这里的重点：它保证一条提案不会被重复批准、也不会绕过
// 批准直接进入执行态。
func (s *Store) transition(id string, want Status, mutate func(*Proposal)) (*Proposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, ok := s.byID[id]
	if !ok {
		return nil, fmt.Errorf("提案 %s 不存在", id)
	}
	if p.Status != want {
		return nil, fmt.Errorf("提案 %s 当前状态为 %s，无法执行需要 %s 状态的操作", id, p.Status, want)
	}
	mutate(p)
	return p.clone(), nil
}

// stampDecision 打上决策时间。调用前必须已持锁。
func (s *Store) stampDecision(p *Proposal) {
	t := s.now()
	p.DecidedAt = &t
}

// auditEntry 是审计日志的一行。Proposal 内嵌且不带 json tag，字段会被展平。
type auditEntry struct {
	Timestamp time.Time `json:"ts"`
	Event     string    `json:"event"`
	*Proposal
}

func (s *Store) writeAudit(event string, p *Proposal) {
	if s.auditPath == "" {
		return
	}

	line, err := json.Marshal(auditEntry{Timestamp: s.now(), Event: event, Proposal: p})
	if err != nil {
		fmt.Fprintf(os.Stderr, "审计日志序列化失败: %v\n", err)
		return
	}

	f, err := os.OpenFile(s.auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		// 审计写失败不中断主流程，但必须让人看见。
		fmt.Fprintf(os.Stderr, "审计日志打开失败 (%s): %v\n", s.auditPath, err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "审计日志写入失败: %v\n", err)
	}
}
