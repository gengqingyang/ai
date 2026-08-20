package approval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// checkpointSuffix 是快照文件后缀。带后缀才能把快照和目录里的临时文件区分开。
const checkpointSuffix = ".ckpt"

// maxCheckpointIDLen 限制 checkpoint ID 长度，避免超出文件名上限。
const maxCheckpointIDLen = 128

// CheckpointStore 把 Eino 中断快照按 checkpoint ID 落盘，实现 adk.CheckPointStore。
//
// 快照里含有本轮完整的模型消息（可能包括截图 base64），因此和提案状态文件一样
// 使用 0700 目录、0600 文件和原子替换：中断期间进程退出后，重启仍能取回同一轮
// 的执行上下文，把人批过的那条命令接着跑完，而不是让模型从头再想一遍。
//
// 快照只负责「流程停在哪」。批准与否、审核人和原始参数一律以 Store 里的提案
// 为准——快照是模型侧状态，不能用来决定要不要在客户设备上动手。
type CheckpointStore struct {
	dir string

	// mu 串行化写入。Eino 只在中断点写快照，竞争极少，用互斥换取
	// 「同一 ID 不会被两个 goroutine 交错替换」的确定性。
	mu sync.Mutex
}

// 编译期确认满足 Eino 的 CheckPointStore 协议（Get/Set），
// 具体接口在 adk 侧断言，这里只固定方法签名。
var _ interface {
	Get(context.Context, string) ([]byte, bool, error)
	Set(context.Context, string, []byte) error
	Delete(context.Context, string) error
} = (*CheckpointStore)(nil)

// OpenCheckpointStore 打开（必要时创建）快照目录。
func OpenCheckpointStore(dir string) (*CheckpointStore, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("快照目录不能为空")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建快照目录 %s: %w", dir, err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("读取快照目录属性: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("快照路径不是目录: %s", dir)
	}
	if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, fmt.Errorf("收紧快照目录权限为 0700: %w", err)
		}
	}
	return &CheckpointStore{dir: dir}, nil
}

// Dir 返回快照目录，供启动信息展示。
func (s *CheckpointStore) Dir() string { return s.dir }

// Get 读取快照。不存在时返回 ok=false 而不是错误——Eino 用它判断能否恢复。
func (s *CheckpointStore) Get(_ context.Context, checkpointID string) ([]byte, bool, error) {
	path, err := s.pathFor(checkpointID)
	if err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("读取快照 %s: %w", checkpointID, err)
	}
	return data, true, nil
}

// Set 原子写入快照。
func (s *CheckpointStore) Set(_ context.Context, checkpointID string, checkpoint []byte) error {
	path, err := s.pathFor(checkpointID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := atomicWriteFile(path, checkpoint); err != nil {
		return fmt.Errorf("持久化快照 %s: %w", checkpointID, err)
	}
	return nil
}

// Delete 删除快照。已经不存在时视为成功，便于清理路径重复调用。
func (s *CheckpointStore) Delete(_ context.Context, checkpointID string) error {
	path, err := s.pathFor(checkpointID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("删除快照 %s: %w", checkpointID, err)
	}
	return nil
}

// IDs 列出当前已落盘的 checkpoint ID，按字典序返回。
// 启动恢复用它核对提案里记录的 checkpoint 是否还在。
func (s *CheckpointStore) IDs() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("列出快照目录: %w", err)
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, checkpointSuffix) {
			continue
		}
		ids = append(ids, strings.TrimSuffix(name, checkpointSuffix))
	}
	sort.Strings(ids)
	return ids, nil
}

// pathFor 把 checkpoint ID 映射成目录内的文件路径。
//
// ID 只允许字母、数字、连字符和下划线。这个限制是 fail-closed 的：ID 最终会
// 成为文件名，放行 `/` 或 `..` 就等于把写入点交给了调用方。
func (s *CheckpointStore) pathFor(checkpointID string) (string, error) {
	if err := validateCheckpointID(checkpointID); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, checkpointID+checkpointSuffix), nil
}

func validateCheckpointID(id string) error {
	if id == "" {
		return errors.New("checkpoint_id 不能为空")
	}
	if len(id) > maxCheckpointIDLen {
		return fmt.Errorf("checkpoint_id 长度 %d 超过上限 %d", len(id), maxCheckpointIDLen)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return fmt.Errorf("checkpoint_id %q 含有非法字符 %q，只允许字母、数字、- 和 _", id, r)
		}
	}
	return nil
}
