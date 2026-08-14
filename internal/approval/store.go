package approval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const storeVersion = 1

type persistedStore struct {
	Version   int         `json:"version"`
	Sequence  int         `json:"sequence"`
	Proposals []*Proposal `json:"proposals"`
}

// Store 持久化提案和状态流转，并单独追加审计日志。并发安全。
// 每次变更先原子替换状态文件：替换前失败会回滚内存；替换后的目录同步失败
// 会保留更严格的新状态并报错，避免已经取得的执行权退回为可再次执行。
type Store struct {
	mu    sync.Mutex
	byID  map[string]*Proposal
	order []string
	seq   int

	statePath string
	auditPath string
	now       func() time.Time
}

// Option 是 Store 的可选配置。
type Option func(*Store)

// WithStateFile 指定提案状态文件。文件使用 0600 权限和原子替换写入。
func WithStateFile(path string) Option {
	return func(s *Store) { s.statePath = path }
}

// WithAuditLog 指定审计日志路径，每次状态流转追加一行 JSON。
func WithAuditLog(path string) Option {
	return func(s *Store) { s.auditPath = path }
}

// WithClock 注入时钟，仅用于测试。
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// NewStore 创建内存或全新文件 Store。生产启动应使用 OpenStore，以便报告加载错误。
func NewStore(opts ...Option) *Store {
	s, err := OpenStore(opts...)
	if err != nil {
		panic(err)
	}
	return s
}

// OpenStore 打开持久化提案存储。遗留的 executing 表示进程可能在真实工具
// 下发后崩溃，会在返回前持久化为 unknown，禁止自动重试。
func OpenStore(opts ...Option) (*Store, error) {
	s := &Store{byID: make(map[string]*Proposal), now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	if s.now == nil {
		return nil, errors.New("提案存储时钟不能为空")
	}
	if s.statePath == "" {
		return s, nil
	}
	data, err := loadStateFile(s.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("加载提案状态: %w", err)
	}
	if err := s.installLoaded(data); err != nil {
		return nil, fmt.Errorf("校验提案状态: %w", err)
	}

	recovered := make([]*Proposal, 0)
	for _, id := range s.order {
		p := s.byID[id]
		if p.Status != StatusExecuting {
			continue
		}
		now := s.now()
		p.Status = StatusUnknown
		p.FinishedAt = &now
		p.Error = "服务在执行结果持久化前退出；命令可能已经生效，禁止自动重试"
		recovered = append(recovered, p.clone())
	}
	if len(recovered) > 0 {
		if err := s.saveLocked(); err != nil {
			return nil, fmt.Errorf("持久化未知执行结果: %w", err)
		}
		for _, p := range recovered {
			s.writeAudit("recovered_unknown", p)
		}
	}
	return s, nil
}

// Create 原子登记一条待审提案。状态文件写入失败时不会留下内存提案。
func (s *Store) Create(toolName, args string) (*Proposal, error) {
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return nil, errors.New("工具名不能为空")
	}
	s.mu.Lock()
	previousSeq := s.seq
	s.seq++
	createdAt := s.now()
	p := &Proposal{
		ID:        fmt.Sprintf("P%03d", s.seq),
		Tool:      toolName,
		Args:      args,
		Status:    StatusPending,
		CreatedAt: createdAt,
	}
	p.IdempotencyKey = proposalKey(p)
	s.byID[p.ID] = p
	s.order = append(s.order, p.ID)
	if err := s.saveLocked(); err != nil {
		committed := stateWasCommitted(err)
		if !committed {
			delete(s.byID, p.ID)
			s.order = s.order[:len(s.order)-1]
			s.seq = previousSeq
		}
		snapshot := p.clone()
		s.mu.Unlock()
		if committed {
			s.writeAudit("created", snapshot)
			return snapshot, fmt.Errorf("新提案状态已替换但持久性未确认: %w", err)
		}
		return nil, fmt.Errorf("持久化新提案: %w", err)
	}
	snapshot := p.clone()
	s.mu.Unlock()

	s.writeAudit("created", snapshot)
	return snapshot, nil
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

// Approve 批准提案。批准持久化后仍未取得真实工具执行权。
func (s *Store) Approve(id, decider string) (*Proposal, error) {
	decider = strings.TrimSpace(decider)
	if decider == "" {
		return nil, errors.New("批准人不能为空")
	}
	return s.transition(id, StatusPending, StatusApproved, "approved", func(p *Proposal) {
		p.Decider = decider
		t := s.now()
		p.DecidedAt = &t
	})
}

// Reject 驳回提案。只有 pending 可驳回。
func (s *Store) Reject(id, decider, reason string) (*Proposal, error) {
	decider = strings.TrimSpace(decider)
	if decider == "" {
		return nil, errors.New("驳回人不能为空")
	}
	return s.transition(id, StatusPending, StatusRejected, "rejected", func(p *Proposal) {
		p.Decider = decider
		p.RejectReason = reason
		t := s.now()
		p.DecidedAt = &t
	})
}

// ClaimExecution 从 approved 原子抢占真实工具执行权。同一提案最多一个调用成功。
func (s *Store) ClaimExecution(id string) (*Proposal, error) {
	return s.transition(id, StatusApproved, StatusExecuting, "executing", func(p *Proposal) {
		t := s.now()
		p.ExecutingAt = &t
	})
}

// MarkExecuted 完成一次已抢占的执行。execErr 非空表示确定失败。
func (s *Store) MarkExecuted(id, result string, execErr error) (*Proposal, error) {
	status := StatusExecuted
	if execErr != nil {
		status = StatusFailed
	}
	return s.finish(id, status, result, execErr)
}

// MarkUnknown 记录结果未知。该状态是终态，禁止自动重试。
func (s *Store) MarkUnknown(id, result string, execErr error) (*Proposal, error) {
	if execErr == nil {
		execErr = errors.New("执行结果未知")
	}
	return s.finish(id, StatusUnknown, result, execErr)
}

func (s *Store) finish(id string, status Status, result string, execErr error) (*Proposal, error) {
	return s.transition(id, StatusExecuting, status, string(status), func(p *Proposal) {
		p.Result = result
		p.Error = ""
		if execErr != nil {
			p.Error = execErr.Error()
		}
		t := s.now()
		p.FinishedAt = &t
	})
}

// BindInterrupt atomically associates a pending proposal with a workflow checkpoint.
func (s *Store) BindInterrupt(id, checkpointID, interruptID string) (*Proposal, error) {
	checkpointID = strings.TrimSpace(checkpointID)
	interruptID = strings.TrimSpace(interruptID)
	if checkpointID == "" || interruptID == "" {
		return nil, errors.New("checkpoint_id 和 interrupt_id 不能为空")
	}
	s.mu.Lock()
	p, ok := s.byID[id]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("提案 %s 不存在", id)
	}
	if p.Status != StatusPending {
		s.mu.Unlock()
		return nil, fmt.Errorf("提案 %s 当前状态为 %s，无法绑定中断", id, p.Status)
	}
	if p.CheckpointID != "" || p.InterruptID != "" {
		if p.CheckpointID == checkpointID && p.InterruptID == interruptID {
			snapshot := p.clone()
			s.mu.Unlock()
			return snapshot, nil
		}
		s.mu.Unlock()
		return nil, fmt.Errorf("提案 %s 已绑定 checkpoint=%s interrupt=%s",
			id, p.CheckpointID, p.InterruptID)
	}
	before := p.clone()
	p.CheckpointID = checkpointID
	p.InterruptID = interruptID
	if err := s.saveLocked(); err != nil {
		committed := stateWasCommitted(err)
		if !committed {
			*p = *before
		}
		snapshot := p.clone()
		s.mu.Unlock()
		if committed {
			s.writeAudit("interrupt_bound", snapshot)
			return snapshot, fmt.Errorf("提案 %s 中断关联已替换但持久性未确认: %w", id, err)
		}
		return nil, fmt.Errorf("持久化提案 %s 中断关联: %w", id, err)
	}
	snapshot := p.clone()
	s.mu.Unlock()
	s.writeAudit("interrupt_bound", snapshot)
	return snapshot, nil
}

func (s *Store) transition(id string, want, next Status, event string, mutate func(*Proposal)) (*Proposal, error) {
	s.mu.Lock()
	p, ok := s.byID[id]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("提案 %s 不存在", id)
	}
	if p.Status != want {
		s.mu.Unlock()
		return nil, fmt.Errorf("提案 %s 当前状态为 %s，无法执行需要 %s 状态的操作", id, p.Status, want)
	}
	before := p.clone()
	mutate(p)
	p.Status = next
	if err := s.saveLocked(); err != nil {
		committed := stateWasCommitted(err)
		if !committed {
			*p = *before
		}
		snapshot := p.clone()
		s.mu.Unlock()
		if committed {
			s.writeAudit(event, snapshot)
			return snapshot, fmt.Errorf("提案 %s 状态 %s 已替换但持久性未确认: %w", id, next, err)
		}
		return nil, fmt.Errorf("持久化提案 %s 状态 %s: %w", id, next, err)
	}
	snapshot := p.clone()
	s.mu.Unlock()
	s.writeAudit(event, snapshot)
	return snapshot, nil
}

func proposalKey(p *Proposal) string {
	h := sha256.New()
	_, _ = io.WriteString(h, p.ID)
	_, _ = io.WriteString(h, "\x00"+p.Tool+"\x00"+p.Args+"\x00"+p.CreatedAt.Format(time.RFC3339Nano))
	return hex.EncodeToString(h.Sum(nil))
}

func loadStateFile(path string) (persistedStore, error) {
	f, err := os.Open(path)
	if err != nil {
		return persistedStore{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return persistedStore{}, fmt.Errorf("读取状态文件属性: %w", err)
	}
	if !info.Mode().IsRegular() {
		return persistedStore{}, fmt.Errorf("状态路径不是普通文件: %s", info.Mode().Type())
	}
	if info.Mode().Perm() != 0o600 {
		if err := f.Chmod(0o600); err != nil {
			return persistedStore{}, fmt.Errorf("收紧状态文件权限为 0600: %w", err)
		}
	}
	var data persistedStore
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&data); err != nil {
		return persistedStore{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return persistedStore{}, errors.New("文件包含多余的 JSON 数据")
		}
		return persistedStore{}, err
	}
	if data.Version != storeVersion {
		return persistedStore{}, fmt.Errorf("不支持的提案状态版本 %d（当前支持 %d）", data.Version, storeVersion)
	}
	return data, nil
}

func (s *Store) installLoaded(data persistedStore) error {
	if data.Sequence < 0 {
		return errors.New("sequence 不能为负数")
	}
	maxSeq := 0
	for index, p := range data.Proposals {
		if p == nil {
			return fmt.Errorf("第 %d 条提案为空", index+1)
		}
		if err := validateLoadedProposal(p); err != nil {
			return fmt.Errorf("第 %d 条提案无效: %w", index+1, err)
		}
		if _, exists := s.byID[p.ID]; exists {
			return fmt.Errorf("提案 ID %s 重复", p.ID)
		}
		seq, _ := strconv.Atoi(strings.TrimPrefix(p.ID, "P"))
		if seq > maxSeq {
			maxSeq = seq
		}
		s.byID[p.ID] = p.clone()
		s.order = append(s.order, p.ID)
	}
	if data.Sequence < maxSeq {
		return fmt.Errorf("sequence %d 小于最大提案序号 %d", data.Sequence, maxSeq)
	}
	s.seq = data.Sequence
	return nil
}

func validateLoadedProposal(p *Proposal) error {
	if len(p.ID) < 2 || p.ID[0] != 'P' {
		return fmt.Errorf("提案 ID %q 无效", p.ID)
	}
	sequence, err := strconv.Atoi(p.ID[1:])
	if err != nil || sequence <= 0 || fmt.Sprintf("P%03d", sequence) != p.ID {
		return fmt.Errorf("提案 ID %q 无效", p.ID)
	}
	if strings.TrimSpace(p.Tool) == "" || p.IdempotencyKey == "" || p.CreatedAt.IsZero() {
		return errors.New("tool、idempotency_key 和 created_at 不能为空")
	}
	if p.Tool != strings.TrimSpace(p.Tool) {
		return errors.New("tool 不能包含首尾空白")
	}
	switch p.Status {
	case StatusPending, StatusApproved, StatusRejected, StatusExecuting,
		StatusExecuted, StatusFailed, StatusUnknown:
	default:
		return fmt.Errorf("状态 %q 无效", p.Status)
	}
	if p.CheckpointID == "" != (p.InterruptID == "") {
		return errors.New("checkpoint_id 和 interrupt_id 必须同时存在")
	}
	if p.CheckpointID != strings.TrimSpace(p.CheckpointID) ||
		p.InterruptID != strings.TrimSpace(p.InterruptID) {
		return errors.New("checkpoint_id 和 interrupt_id 不能包含首尾空白")
	}
	if p.IdempotencyKey != proposalKey(p) {
		return errors.New("idempotency_key 与提案原始内容不一致")
	}
	if p.Decider != strings.TrimSpace(p.Decider) {
		return errors.New("decider 不能包含首尾空白")
	}
	hasDecider := p.Decider != ""
	hasDecisionTime := p.DecidedAt != nil
	if hasDecider != hasDecisionTime {
		return errors.New("decider 和 decided_at 必须同时存在")
	}
	decided := hasDecider
	claimed := p.ExecutingAt != nil
	finished := p.FinishedAt != nil
	if decided && p.DecidedAt.Before(p.CreatedAt) {
		return errors.New("decided_at 不能早于 created_at")
	}
	if claimed && (!decided || p.ExecutingAt.Before(*p.DecidedAt)) {
		return errors.New("executing_at 不能早于 decided_at")
	}
	if finished && (!claimed || p.FinishedAt.Before(*p.ExecutingAt)) {
		return errors.New("finished_at 不能早于 executing_at")
	}
	switch p.Status {
	case StatusPending:
		if decided || claimed || finished || p.RejectReason != "" || p.Result != "" || p.Error != "" {
			return errors.New("pending 提案不能包含决定、执行或结果字段")
		}
	case StatusApproved:
		if !decided || claimed || finished || p.RejectReason != "" || p.Result != "" || p.Error != "" {
			return errors.New("approved 提案的状态字段不一致")
		}
	case StatusRejected:
		if !decided || claimed || finished || p.Result != "" || p.Error != "" {
			return errors.New("rejected 提案的状态字段不一致")
		}
	case StatusExecuting:
		if !decided || !claimed || finished || p.RejectReason != "" || p.Result != "" || p.Error != "" {
			return errors.New("executing 提案的状态字段不一致")
		}
	case StatusExecuted:
		if !decided || !claimed || !finished || p.RejectReason != "" || p.Error != "" {
			return errors.New("executed 提案的状态或错误字段不一致")
		}
	case StatusFailed, StatusUnknown:
		if !decided || !claimed || !finished || p.RejectReason != "" || p.Error == "" {
			return fmt.Errorf("%s 提案的状态或错误字段不一致", p.Status)
		}
	}
	return nil
}

func (s *Store) saveLocked() error {
	if s.statePath == "" {
		return nil
	}
	proposals := make([]*Proposal, 0, len(s.order))
	for _, id := range s.order {
		proposals = append(proposals, s.byID[id].clone())
	}
	return saveStateFile(s.statePath, persistedStore{
		Version: storeVersion, Sequence: s.seq, Proposals: proposals,
	})
}

func saveStateFile(path string, data persistedStore) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建提案状态目录: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	encoder := json.NewEncoder(tmp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	dirHandle, err := os.Open(dir)
	if err != nil {
		return &committedStateError{err: fmt.Errorf("打开状态目录以同步: %w", err)}
	}
	syncErr := dirHandle.Sync()
	closeErr := dirHandle.Close()
	if syncErr != nil {
		return &committedStateError{err: fmt.Errorf("同步状态目录: %w", syncErr)}
	}
	if closeErr != nil {
		return &committedStateError{err: fmt.Errorf("关闭状态目录: %w", closeErr)}
	}
	return nil
}

// committedStateError 表示 rename 已成功、但其后的目录同步没有得到确认。
// 调用方必须保留新状态并停止后续动作，不能按普通写盘失败回滚。
type committedStateError struct {
	err error
}

func (e *committedStateError) Error() string { return e.err.Error() }
func (e *committedStateError) Unwrap() error { return e.err }

func stateWasCommitted(err error) bool {
	var committed *committedStateError
	return errors.As(err, &committed)
}

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
		slog.Error("审计日志序列化失败", "err", err)
		return
	}
	f, err := os.OpenFile(s.auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		slog.Error("审计日志打开失败", "path", s.auditPath, "err", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		slog.Error("审计日志写入失败", "path", s.auditPath, "err", err)
	}
}
