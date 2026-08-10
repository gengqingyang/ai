package test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "diagnostic-system/internal/session"

	"github.com/cloudwego/eino/schema"
)

func TestStoreCreatesSwitchesAndRestoresSessions(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), "history.json")
	store, err := OpenStore(0, 900_000, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	first, firstInfo := store.Current()
	if first == nil || firstInfo.ID == "" || !firstInfo.Active {
		t.Fatalf("初始会话无效: %#v", firstInfo)
	}
	if err := first.AppendUser("第一个会话"); err != nil {
		t.Fatal(err)
	}

	second, secondInfo, err := store.Create("CDN 排查")
	if err != nil {
		t.Fatal(err)
	}
	if !secondInfo.Active || secondInfo.Name != "CDN 排查" {
		t.Fatalf("新会话信息不正确: %#v", secondInfo)
	}
	if err := second.AppendUser("第二个会话"); err != nil {
		t.Fatal(err)
	}

	selected, selectedInfo, err := store.Select(firstInfo.ShortID())
	if err != nil {
		t.Fatal(err)
	}
	if selectedInfo.ID != firstInfo.ID || !selectedInfo.Active {
		t.Fatalf("短 ID 切换结果不正确: %#v", selectedInfo)
	}
	assertMessages(t, selected.Messages(), []wantMessage{{schema.User, "第一个会话"}})

	restored, err := OpenStore(0, 900_000, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	current, currentInfo := restored.Current()
	if currentInfo.ID != firstInfo.ID {
		t.Fatalf("重启后当前会话 = %s, want %s", currentInfo.ID, firstInfo.ID)
	}
	assertMessages(t, current.Messages(), []wantMessage{{schema.User, "第一个会话"}})

	secondAgain, _, err := restored.Select(secondInfo.Name)
	if err != nil {
		t.Fatal(err)
	}
	assertMessages(t, secondAgain.Messages(), []wantMessage{{schema.User, "第二个会话"}})

	if got := len(restored.List()); got != 2 {
		t.Fatalf("会话数 = %d, want 2", got)
	}
}

func TestStoreMigratesLegacyCache(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), ".chat_history.json")
	legacy, err := Open(0, 900_000, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.AppendUser("迁移前的问题"); err != nil {
		t.Fatal(err)
	}
	if err := legacy.AppendAssistant("迁移前的回答"); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(0, 900_000, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	current, info := store.Current()
	if info.Name != "迁移前的问题" {
		t.Fatalf("迁移会话名称 = %q", info.Name)
	}
	assertMessages(t, current.Messages(), []wantMessage{
		{schema.User, "迁移前的问题"},
		{schema.Assistant, "迁移前的回答"},
	})

	var index struct {
		ActiveID string `json:"active_id"`
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	data, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	if index.ActiveID != info.ID || len(index.Sessions) != 1 {
		t.Fatalf("迁移后的索引不正确: %#v", index)
	}
	if _, err := os.Stat(filepath.Join(testSessionDataDir(indexPath), info.ID+".json")); err != nil {
		t.Fatalf("迁移后的会话文件不存在: %v", err)
	}

	reopened, err := OpenStore(0, 900_000, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, _ := reopened.Current()
	assertMessages(t, reloaded.Messages(), []wantMessage{
		{schema.User, "迁移前的问题"},
		{schema.Assistant, "迁移前的回答"},
	})
}

func TestStoreKeepsDuplicateNamesButRejectsAmbiguousSelection(t *testing.T) {
	store, err := OpenStore(0, 1000, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Create("同名"); err != nil {
		t.Fatal(err)
	}
	second, secondInfo, err := store.Create("同名")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Select("同名"); err == nil {
		t.Fatal("同名会话应该要求使用 ID")
	}
	selected, info, err := store.Select(secondInfo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if selected != second || info.ID != secondInfo.ID {
		t.Fatalf("按完整 ID 选择了错误会话: %#v", info)
	}
}

func TestStoreRejectsLongSessionName(t *testing.T) {
	store, err := OpenStore(0, 1000, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Create("这是一个超过六十个字符的会话名称这是一个超过六十个字符的会话名称这是一个超过六十个字符的会话名称这是一个超过六十个字符的会话名称"); err == nil {
		t.Fatal("过长会话名应该报错")
	}
}

func TestStoreFilesArePrivate(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), "history.json")
	store, err := OpenStore(0, 1000, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	_, info := store.Current()
	for _, path := range []string{indexPath, filepath.Join(testSessionDataDir(indexPath), info.ID+".json")} {
		stat, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := stat.Mode().Perm(); got != 0o600 {
			t.Errorf("%s 权限 = %o, want 600", path, got)
		}
	}
}

func testSessionDataDir(indexPath string) string {
	base := filepath.Base(indexPath)
	ext := filepath.Ext(base)
	return filepath.Join(filepath.Dir(indexPath), strings.TrimSuffix(base, ext)+"_sessions")
}
