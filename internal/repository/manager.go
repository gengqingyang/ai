package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

const storeVersion = 1

type storeFile struct {
	Version      int    `json:"version"`
	Active       string `json:"active,omitempty"`
	Repositories []Info `json:"repositories"`
}

// Manager 管理多个命名仓库和一个当前仓库。索引只保存在内存，源码不会写入状态文件。
type Manager struct {
	mu        sync.RWMutex
	statePath string
	active    string
	records   map[string]Info
	indexes   map[string]*index
}

// Open 打开仓库目录。statePath 为空时仅使用内存状态。
func Open(statePath string) (*Manager, error) {
	m := &Manager{
		statePath: statePath,
		records:   make(map[string]Info),
		indexes:   make(map[string]*index),
	}
	if statePath == "" {
		return m, nil
	}
	f, err := os.Open(statePath)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, fmt.Errorf("打开仓库目录: %w", err)
	}
	defer f.Close()

	var data storeFile
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&data); err != nil {
		return nil, fmt.Errorf("解析仓库目录: %w", err)
	}
	if data.Version != storeVersion {
		return nil, fmt.Errorf("不支持的仓库目录版本 %d（当前支持 %d）", data.Version, storeVersion)
	}
	for _, item := range data.Repositories {
		name, err := normalizeName(item.Name)
		if err != nil || name != item.Name {
			return nil, fmt.Errorf("仓库名称 %q 无效", item.Name)
		}
		if !filepath.IsAbs(item.Root) {
			return nil, fmt.Errorf("仓库 %q 的根路径不是绝对路径", item.Name)
		}
		key := nameKey(name)
		if _, exists := m.records[key]; exists {
			return nil, fmt.Errorf("仓库名称 %q 重复", name)
		}
		item.Active = false
		m.records[key] = item
	}
	if data.Active != "" {
		key := nameKey(data.Active)
		if _, exists := m.records[key]; !exists {
			return nil, fmt.Errorf("当前仓库 %q 不在仓库目录中", data.Active)
		}
		m.active = key
	}
	return m, nil
}

// Add 添加、索引并选中一个本地仓库。
func (m *Manager) Add(ctx context.Context, path, name string) (Info, error) {
	root, err := canonicalRoot(path)
	if err != nil {
		return Info{}, err
	}
	if strings.TrimSpace(name) == "" {
		name = filepath.Base(root)
	}
	name, err = normalizeName(name)
	if err != nil {
		return Info{}, err
	}
	key := nameKey(name)

	m.mu.RLock()
	_, exists := m.records[key]
	m.mu.RUnlock()
	if exists {
		return Info{}, fmt.Errorf("仓库名称 %q 已存在", name)
	}

	idx, err := buildIndex(ctx, root, nil, m.indexIgnoredFiles(root))
	if err != nil {
		return Info{}, fmt.Errorf("索引仓库 %q: %w", name, err)
	}
	git := readGitInfo(root)
	info := Info{
		Name:         name,
		Root:         root,
		GitCommit:    git.Commit,
		GitBranch:    git.Branch,
		IndexVersion: IndexVersion,
		IndexedAt:    idx.indexedAt,
		FileCount:    len(idx.paths),
		SymbolCount:  len(idx.symbols),
		Active:       true,
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.records[key]; exists {
		return Info{}, fmt.Errorf("仓库名称 %q 已存在", name)
	}
	previousActive := m.active
	m.records[key] = info
	m.indexes[key] = idx
	m.active = key
	if err := m.saveLocked(); err != nil {
		delete(m.records, key)
		delete(m.indexes, key)
		m.active = previousActive
		return Info{}, fmt.Errorf("保存仓库目录: %w", err)
	}
	return info, nil
}

// List 返回全部仓库，当前仓库优先，其余按名称排序。
func (m *Manager) List() []Info {
	m.mu.RLock()
	defer m.mu.RUnlock()
	items := make([]Info, 0, len(m.records))
	for key, item := range m.records {
		item.Active = key == m.active
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Active != items[j].Active {
			return items[i].Active
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
	return items
}

// Active 返回当前仓库元数据。
func (m *Manager) Active() (Info, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	item, ok := m.records[m.active]
	item.Active = ok
	return item, ok
}

// Use 切换当前仓库，并在首次使用时建立内存索引。
func (m *Manager) Use(ctx context.Context, name string) (Info, error) {
	key, item, err := m.resolve(name)
	if err != nil {
		return Info{}, err
	}
	if _, err := m.ensureIndex(ctx, key, item); err != nil {
		return Info{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	previous := m.active
	m.active = key
	if err := m.saveLocked(); err != nil {
		m.active = previous
		return Info{}, fmt.Errorf("保存当前仓库: %w", err)
	}
	item = m.records[key]
	item.Active = true
	return item, nil
}

// Reindex 增量重建当前仓库索引。
func (m *Manager) Reindex(ctx context.Context) (Info, error) {
	m.mu.RLock()
	key := m.active
	item, ok := m.records[key]
	previous := m.indexes[key]
	m.mu.RUnlock()
	if !ok {
		return Info{}, errors.New("尚未选择代码仓库，请先使用 /repo add 或 /repo use")
	}

	idx, err := buildIndex(ctx, item.Root, previous, m.indexIgnoredFiles(item.Root))
	if err != nil {
		return Info{}, fmt.Errorf("重新索引仓库 %q: %w", item.Name, err)
	}
	git := readGitInfo(item.Root)
	item.GitCommit = git.Commit
	item.GitBranch = git.Branch
	item.IndexVersion = IndexVersion
	item.IndexedAt = idx.indexedAt
	item.FileCount = len(idx.paths)
	item.SymbolCount = len(idx.symbols)
	item.Active = true

	m.mu.Lock()
	defer m.mu.Unlock()
	oldInfo := m.records[key]
	oldIndex := m.indexes[key]
	m.records[key] = item
	m.indexes[key] = idx
	if err := m.saveLocked(); err != nil {
		m.records[key] = oldInfo
		m.indexes[key] = oldIndex
		return Info{}, fmt.Errorf("保存仓库索引信息: %w", err)
	}
	return item, nil
}

func (m *Manager) resolve(name string) (string, Info, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", Info{}, errors.New("仓库名称不能为空")
	}
	key := nameKey(name)
	m.mu.RLock()
	defer m.mu.RUnlock()
	item, ok := m.records[key]
	if !ok {
		return "", Info{}, fmt.Errorf("找不到仓库 %q", name)
	}
	return key, item, nil
}

func (m *Manager) activeIndex(ctx context.Context) (Info, *index, error) {
	m.mu.RLock()
	key := m.active
	item, ok := m.records[key]
	m.mu.RUnlock()
	if !ok {
		return Info{}, nil, errors.New("尚未选择代码仓库，请先使用 /repo add <path> [name]")
	}
	idx, err := m.ensureIndex(ctx, key, item)
	if err != nil {
		return Info{}, nil, err
	}
	m.mu.RLock()
	item = m.records[key]
	m.mu.RUnlock()
	return item, idx, nil
}

func (m *Manager) ensureIndex(ctx context.Context, key string, item Info) (*index, error) {
	m.mu.RLock()
	idx := m.indexes[key]
	m.mu.RUnlock()
	if idx != nil {
		return idx, nil
	}
	idx, err := buildIndex(ctx, item.Root, nil, m.indexIgnoredFiles(item.Root))
	if err != nil {
		return nil, fmt.Errorf("加载仓库 %q 的索引: %w", item.Name, err)
	}
	m.mu.Lock()
	if existing := m.indexes[key]; existing != nil {
		idx = existing
	} else {
		m.indexes[key] = idx
		item.IndexVersion = IndexVersion
		item.IndexedAt = idx.indexedAt
		item.FileCount = len(idx.paths)
		item.SymbolCount = len(idx.symbols)
		m.records[key] = item
	}
	m.mu.Unlock()
	return idx, nil
}

func (m *Manager) snapshot(ctx context.Context, info Info, idx *index) Snapshot {
	current := readGitInfo(info.Root)
	reasons := make(map[StaleReason]struct{})
	if current.Commit != info.GitCommit {
		reasons[StaleGitCommitChanged] = struct{}{}
	}
	fileReasons, stalePaths := indexFileChanges(ctx, info.Root, idx)
	for _, reason := range fileReasons {
		reasons[reason] = struct{}{}
	}
	ordered := orderedStaleReasons(reasons)
	return Snapshot{
		Repository:       info.Name,
		GitCommit:        info.GitCommit,
		CurrentGitCommit: current.Commit,
		GitBranch:        info.GitBranch,
		IndexVersion:     info.IndexVersion,
		IndexedAt:        info.IndexedAt,
		Stale:            len(ordered) > 0,
		StaleReasons:     ordered,
		StalePaths:       stalePaths,
	}
}

func (m *Manager) indexIgnoredFiles(root string) map[string]struct{} {
	ignored := make(map[string]struct{})
	if m.statePath == "" {
		return ignored
	}
	statePath, err := filepath.Abs(m.statePath)
	if err != nil {
		return ignored
	}
	stateDir, err := filepath.EvalSymlinks(filepath.Dir(statePath))
	if err != nil {
		return ignored
	}
	statePath = filepath.Join(stateDir, filepath.Base(statePath))
	rel, err := filepath.Rel(root, filepath.Clean(statePath))
	if err != nil {
		return ignored
	}
	rel = filepath.ToSlash(rel)
	if rel != "." && rel != ".." && !strings.HasPrefix(rel, "../") {
		ignored[rel] = struct{}{}
	}
	return ignored
}

func (m *Manager) saveLocked() error {
	if m.statePath == "" {
		return nil
	}
	items := make([]Info, 0, len(m.records))
	for _, item := range m.records {
		item.Active = false
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name) })
	active := ""
	if item, ok := m.records[m.active]; ok {
		active = item.Name
	}
	return saveStore(m.statePath, storeFile{Version: storeVersion, Active: active, Repositories: items})
}

func saveStore(path string, data storeFile) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
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
	return os.Rename(tmpPath, path)
}

func canonicalRoot(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("仓库路径不能为空")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("解析仓库路径: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("访问仓库路径 %q: %w", path, err)
	}
	stat, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("访问仓库路径 %q: %w", path, err)
	}
	if !stat.IsDir() {
		return "", fmt.Errorf("仓库路径 %q 不是目录", path)
	}
	return filepath.Clean(real), nil
}

func normalizeName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "", errors.New("仓库名称不能为空")
	}
	if utf8.RuneCountInString(name) > 64 {
		return "", errors.New("仓库名称不能超过 64 个字符")
	}
	for _, r := range name {
		if unicode.IsControl(r) || r == '/' || r == '\\' {
			return "", fmt.Errorf("仓库名称包含非法字符 %q", r)
		}
	}
	return name, nil
}

func nameKey(name string) string { return strings.ToLower(strings.TrimSpace(name)) }
