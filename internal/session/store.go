package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"
)

const sessionStoreVersion = 1

// Info 是会话选择器需要的稳定元数据。
type Info struct {
	ID        string
	Name      string
	CreatedAt time.Time
	UpdatedAt time.Time
	Active    bool
}

// ShortID 返回适合终端展示和 /switch 输入的短 ID。
func (i Info) ShortID() string {
	const shortLength = 8
	if len(i.ID) <= shortLength {
		return i.ID
	}
	return i.ID[:shortLength]
}

type sessionRecord struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
}

type storeFile struct {
	Version  int             `json:"version"`
	ActiveID string          `json:"active_id"`
	Sessions []sessionRecord `json:"sessions"`
}

// Store 管理多个彼此隔离的对话 Session，并持久化当前选中的会话。
type Store struct {
	mu sync.Mutex

	maxTurns  int
	maxTokens int
	indexPath string
	dataDir   string

	activeID string
	records  []sessionRecord
	loaded   map[string]*Session
}

// OpenStore 打开多会话存储。旧版单会话缓存会在首次打开时自动迁移。
func OpenStore(maxTurns, maxTokens int, indexPath string) (*Store, error) {
	s := &Store{
		maxTurns:  maxTurns,
		maxTokens: maxTokens,
		indexPath: indexPath,
		dataDir:   sessionDataDir(indexPath),
		loaded:    make(map[string]*Session),
	}

	if indexPath == "" {
		if err := s.initializeEmpty(false); err != nil {
			return nil, err
		}
		return s, nil
	}

	if _, err := os.Stat(indexPath); errors.Is(err, os.ErrNotExist) {
		if err := s.initializeEmpty(true); err != nil {
			return nil, err
		}
		return s, nil
	} else if err != nil {
		return nil, fmt.Errorf("读取会话索引: %w", err)
	}

	index, indexErr := loadStoreFile(indexPath)
	if indexErr == nil {
		s.activeID = index.ActiveID
		s.records = index.Sessions
		active, err := s.openSession(s.activeID)
		if err != nil {
			return nil, fmt.Errorf("打开当前会话: %w", err)
		}
		s.loaded[s.activeID] = active
		return s, nil
	}

	legacy, legacyErr := newFileCache(indexPath).Load()
	if legacyErr != nil {
		return nil, fmt.Errorf("读取会话索引失败: %v；也不是有效的旧版历史缓存: %w", indexErr, legacyErr)
	}
	if err := s.migrateLegacy(legacy); err != nil {
		return nil, fmt.Errorf("迁移旧版对话历史: %w", err)
	}
	return s, nil
}

// Current 返回当前会话及其元数据。
func (s *Store) Current() (*Session, Info) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loaded[s.activeID], s.infoLocked(s.activeID)
}

// List 按当前会话优先、最近使用时间倒序返回会话列表。
func (s *Store) List() []Info {
	s.mu.Lock()
	defer s.mu.Unlock()

	items := make([]Info, 0, len(s.records))
	for _, record := range s.records {
		items = append(items, s.infoForRecordLocked(record))
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Active != items[j].Active {
			return items[i].Active
		}
		return items[i].UpdatedAt.After(items[j].UpdatedAt)
	})
	return items
}

// Create 创建一个空会话并立即切换过去。name 为空时使用时间生成名称。
func (s *Store) Create(name string) (*Session, Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	normalized, err := normalizeSessionName(name)
	if err != nil {
		return nil, Info{}, err
	}
	if normalized == "" {
		normalized = defaultSessionName(now)
	}

	id, err := s.newIDLocked()
	if err != nil {
		return nil, Info{}, fmt.Errorf("生成会话 ID: %w", err)
	}
	record := sessionRecord{
		ID:         id,
		Name:       normalized,
		CreatedAt:  now,
		LastUsedAt: now,
	}
	sessionCache := newFileCache(s.sessionPath(id))
	if s.indexPath != "" {
		if err := sessionCache.Save(nil); err != nil {
			return nil, Info{}, fmt.Errorf("创建会话缓存: %w", err)
		}
	}
	created := &Session{
		maxTurns:  s.maxTurns,
		maxTokens: s.maxTokens,
	}
	if s.indexPath != "" {
		created.cache = sessionCache
	}

	previousActive := s.activeID
	s.records = append(s.records, record)
	s.activeID = id
	s.loaded[id] = created
	if err := s.saveLocked(); err != nil {
		s.records = s.records[:len(s.records)-1]
		s.activeID = previousActive
		delete(s.loaded, id)
		if s.indexPath != "" {
			_ = os.Remove(s.sessionPath(id))
		}
		return nil, Info{}, fmt.Errorf("保存会话索引: %w", err)
	}
	return created, s.infoForRecordLocked(record), nil
}

// Select 按完整/短 ID 或唯一名称切换会话。
func (s *Store) Select(query string) (*Session, Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	index, err := s.resolveLocked(query)
	if err != nil {
		return nil, Info{}, err
	}
	record := s.records[index]
	selected, err := s.openSession(record.ID)
	if err != nil {
		return nil, Info{}, err
	}

	if record.ID == s.activeID {
		return selected, s.infoForRecordLocked(record), nil
	}

	previousActive := s.activeID
	previousUsedAt := record.LastUsedAt
	s.activeID = record.ID
	s.records[index].LastUsedAt = time.Now()
	if err := s.saveLocked(); err != nil {
		s.activeID = previousActive
		s.records[index].LastUsedAt = previousUsedAt
		return nil, Info{}, fmt.Errorf("保存当前会话: %w", err)
	}
	return selected, s.infoForRecordLocked(s.records[index]), nil
}

func (s *Store) initializeEmpty(persist bool) error {
	now := time.Now()
	id, err := s.newIDLocked()
	if err != nil {
		return fmt.Errorf("生成初始会话 ID: %w", err)
	}
	record := sessionRecord{
		ID:         id,
		Name:       defaultSessionName(now),
		CreatedAt:  now,
		LastUsedAt: now,
	}

	current := &Session{maxTurns: s.maxTurns, maxTokens: s.maxTokens}
	if persist {
		current.cache = newFileCache(s.sessionPath(id))
		if err := current.cache.Save(nil); err != nil {
			return fmt.Errorf("创建初始会话缓存: %w", err)
		}
	}
	s.activeID = id
	s.records = []sessionRecord{record}
	s.loaded[id] = current
	if err := s.saveLocked(); err != nil {
		if persist {
			_ = os.Remove(s.sessionPath(id))
		}
		return fmt.Errorf("创建会话索引: %w", err)
	}
	return nil
}

func (s *Store) migrateLegacy(history []*schema.Message) error {
	now := time.Now()
	if info, err := os.Stat(s.indexPath); err == nil {
		now = info.ModTime()
	}
	id, err := s.newIDLocked()
	if err != nil {
		return err
	}
	record := sessionRecord{
		ID:         id,
		Name:       legacySessionName(history),
		CreatedAt:  now,
		LastUsedAt: now,
	}
	current := &Session{
		history:   append([]*schema.Message(nil), history...),
		maxTurns:  s.maxTurns,
		maxTokens: s.maxTokens,
		cache:     newFileCache(s.sessionPath(id)),
	}
	current.trimLocked()
	if err := current.cache.Save(current.history); err != nil {
		return fmt.Errorf("写入迁移后的会话: %w", err)
	}

	s.activeID = id
	s.records = []sessionRecord{record}
	s.loaded[id] = current
	if err := s.saveLocked(); err != nil {
		_ = os.Remove(s.sessionPath(id))
		return err
	}
	return nil
}

func (s *Store) openSession(id string) (*Session, error) {
	if loaded := s.loaded[id]; loaded != nil {
		return loaded, nil
	}
	if s.indexPath == "" {
		return nil, fmt.Errorf("会话 %s 未加载", id)
	}
	path := s.sessionPath(id)
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("读取会话 %s: %w", id, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("会话缓存 %s 是目录", path)
	}
	opened, err := Open(s.maxTurns, s.maxTokens, path)
	if err != nil {
		return nil, fmt.Errorf("读取会话 %s: %w", id, err)
	}
	s.loaded[id] = opened
	return opened, nil
}

func (s *Store) resolveLocked(query string) (int, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return -1, errors.New("会话 ID 或名称不能为空")
	}

	for i, record := range s.records {
		if record.ID == query {
			return i, nil
		}
	}

	nameMatch := -1
	for i, record := range s.records {
		if strings.EqualFold(record.Name, query) {
			if nameMatch >= 0 {
				return -1, fmt.Errorf("会话名称 %q 不唯一，请改用 ID", query)
			}
			nameMatch = i
		}
	}
	if nameMatch >= 0 {
		return nameMatch, nil
	}

	if len(query) >= 4 {
		idMatch := -1
		for i, record := range s.records {
			if strings.HasPrefix(record.ID, query) {
				if idMatch >= 0 {
					return -1, fmt.Errorf("短 ID %q 匹配多个会话，请多输入几位", query)
				}
				idMatch = i
			}
		}
		if idMatch >= 0 {
			return idMatch, nil
		}
	}
	return -1, fmt.Errorf("找不到会话 %q", query)
}

func (s *Store) newIDLocked() (string, error) {
	for attempt := 0; attempt < 4; attempt++ {
		var raw [8]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", err
		}
		id := hex.EncodeToString(raw[:])
		found := false
		for _, record := range s.records {
			if record.ID == id {
				found = true
				break
			}
		}
		if !found {
			return id, nil
		}
	}
	return "", errors.New("连续生成了重复 ID")
}

func (s *Store) saveLocked() error {
	if s.indexPath == "" {
		return nil
	}
	return saveStoreFile(s.indexPath, storeFile{
		Version:  sessionStoreVersion,
		ActiveID: s.activeID,
		Sessions: append([]sessionRecord(nil), s.records...),
	})
}

func (s *Store) infoLocked(id string) Info {
	for _, record := range s.records {
		if record.ID == id {
			return s.infoForRecordLocked(record)
		}
	}
	return Info{}
}

func (s *Store) infoForRecordLocked(record sessionRecord) Info {
	updated := record.LastUsedAt
	if s.indexPath != "" {
		if stat, err := os.Stat(s.sessionPath(record.ID)); err == nil && stat.ModTime().After(updated) {
			updated = stat.ModTime()
		}
	}
	return Info{
		ID:        record.ID,
		Name:      record.Name,
		CreatedAt: record.CreatedAt,
		UpdatedAt: updated,
		Active:    record.ID == s.activeID,
	}
}

func (s *Store) sessionPath(id string) string {
	return filepath.Join(s.dataDir, id+".json")
}

func sessionDataDir(indexPath string) string {
	if indexPath == "" {
		return ""
	}
	dir := filepath.Dir(indexPath)
	base := filepath.Base(indexPath)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if stem == "" {
		stem = base
	}
	return filepath.Join(dir, stem+"_sessions")
}

func loadStoreFile(path string) (storeFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return storeFile{}, err
	}
	defer f.Close()

	var data storeFile
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&data); err != nil {
		return storeFile{}, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return storeFile{}, err
	}
	if data.Version != sessionStoreVersion {
		return storeFile{}, fmt.Errorf("不支持的会话索引版本 %d（当前支持 %d）", data.Version, sessionStoreVersion)
	}
	if len(data.Sessions) == 0 {
		return storeFile{}, errors.New("会话索引不能为空")
	}

	seen := make(map[string]struct{}, len(data.Sessions))
	activeFound := false
	for i, record := range data.Sessions {
		if len(record.ID) != 16 {
			return storeFile{}, fmt.Errorf("第 %d 个会话 ID 无效", i+1)
		}
		if _, err := hex.DecodeString(record.ID); err != nil {
			return storeFile{}, fmt.Errorf("第 %d 个会话 ID 无效: %w", i+1, err)
		}
		if record.Name == "" {
			return storeFile{}, fmt.Errorf("第 %d 个会话名称为空", i+1)
		}
		if _, exists := seen[record.ID]; exists {
			return storeFile{}, fmt.Errorf("会话 ID %s 重复", record.ID)
		}
		seen[record.ID] = struct{}{}
		if record.ID == data.ActiveID {
			activeFound = true
		}
	}
	if !activeFound {
		return storeFile{}, fmt.Errorf("当前会话 %q 不在会话列表中", data.ActiveID)
	}
	return data, nil
}

func saveStoreFile(path string, data storeFile) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建会话索引目录: %w", err)
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
	return nil
}

func normalizeSessionName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if utf8.RuneCountInString(name) > 60 {
		return "", errors.New("会话名称不能超过 60 个字符")
	}
	return name, nil
}

func defaultSessionName(now time.Time) string {
	return "会话 " + now.Local().Format("2006-01-02 15:04")
}

func legacySessionName(history []*schema.Message) string {
	for _, msg := range history {
		if msg == nil || msg.Role != schema.User {
			continue
		}
		name := strings.Join(strings.Fields(msg.Content), " ")
		if name == "" {
			continue
		}
		runes := []rune(name)
		if len(runes) > 32 {
			name = string(runes[:32]) + "..."
		}
		return name
	}
	return "历史会话"
}
