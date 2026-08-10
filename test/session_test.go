package test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "diagnostic-system/internal/session"

	"github.com/cloudwego/eino/schema"
)

func TestOpenRestoresPersistedHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	s, err := Open(10, 0, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendUser("第一问"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendAssistant("第一答"); err != nil {
		t.Fatal(err)
	}

	restored, err := Open(10, 0, path)
	if err != nil {
		t.Fatal(err)
	}
	assertMessages(t, restored.Messages(), []wantMessage{
		{schema.User, "第一问"},
		{schema.Assistant, "第一答"},
	})

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("缓存权限 = %o, want 600", got)
	}
}

func TestPersistentSessionTrimsOldTurns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	s, err := Open(0, 0, path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if err := s.AppendUser(fmt.Sprintf("问%d", i)); err != nil {
			t.Fatal(err)
		}
		if err := s.AppendAssistant(fmt.Sprintf("答%d", i)); err != nil {
			t.Fatal(err)
		}
	}

	// 模拟重启时把 AGENT_HISTORY_TURNS 从不限改成 2；Open 应立即裁剪并写回。
	restored, err := Open(2, 0, path)
	if err != nil {
		t.Fatal(err)
	}
	assertMessages(t, restored.Messages(), []wantMessage{
		{schema.User, "问2"},
		{schema.Assistant, "答2"},
		{schema.User, "问3"},
		{schema.Assistant, "答3"},
	})

	persisted, err := Open(0, 0, path)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Len() != 4 {
		t.Fatalf("启动裁剪没有写回缓存，重新加载得到 %d 条消息, want 4", persisted.Len())
	}
}

func TestResetClearsPersistedHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	s, err := Open(10, 0, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendUser("会被清空"); err != nil {
		t.Fatal(err)
	}
	if err := s.Reset(); err != nil {
		t.Fatal(err)
	}

	restored, err := Open(10, 0, path)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Len() != 0 {
		t.Fatalf("Reset 后恢复了 %d 条消息, want 0", restored.Len())
	}
}

func TestOpenRejectsInvalidCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"messages":[{"role":"tool","content":"x"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(10, 0, path); err == nil {
		t.Fatal("非法角色应该导致加载失败")
	}
}

func TestAppendRollsBackWhenCacheWriteFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.json")
	s, err := Open(10, 0, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := s.AppendUser("不能落盘"); err == nil {
		t.Fatal("缓存写入失败时 AppendUser 应该报错")
	}
	if s.Len() != 0 {
		t.Fatalf("缓存失败后内存里有 %d 条消息, want 0", s.Len())
	}
}

func TestPersistentSessionRestoresImageMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	s, err := Open(0, 900_000, path)
	if err != nil {
		t.Fatal(err)
	}
	imageData := "aGVsbG8="
	msg := &schema.Message{
		Role:    schema.User,
		Content: "分析图片",
		UserInputMultiContent: []schema.MessageInputPart{
			{Type: schema.ChatMessagePartTypeText, Text: "分析图片"},
			{
				Type: schema.ChatMessagePartTypeImageURL,
				Image: &schema.MessageInputImage{
					MessagePartCommon: schema.MessagePartCommon{Base64Data: &imageData, MIMEType: "image/png"},
					Detail:            schema.ImageURLDetailHigh,
				},
			},
		},
	}
	if err := s.AppendUserMessage(msg); err != nil {
		t.Fatal(err)
	}

	restored, err := Open(0, 900_000, path)
	if err != nil {
		t.Fatal(err)
	}
	got := restored.Messages()
	if len(got) != 1 || len(got[0].UserInputMultiContent) != 2 {
		t.Fatalf("恢复的多模态消息不完整: %#v", got)
	}
	image := got[0].UserInputMultiContent[1].Image
	if image == nil || image.Base64Data == nil || *image.Base64Data != imageData ||
		image.MIMEType != "image/png" || image.Detail != schema.ImageURLDetailHigh {
		t.Fatalf("恢复的图片 part 不正确: %#v", image)
	}
}

func TestPersistentSessionTrimsByTokenBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	s, err := Open(0, 100, path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		if err := s.AppendUser(fmt.Sprintf("问%d%s", i, strings.Repeat("中", 20))); err != nil {
			t.Fatal(err)
		}
		if err := s.AppendAssistant(fmt.Sprintf("答%d%s", i, strings.Repeat("中", 20))); err != nil {
			t.Fatal(err)
		}
	}

	got := s.Messages()
	if len(got) != 2 || !strings.HasPrefix(got[0].Content, "问2") {
		t.Fatalf("token 裁剪后历史不正确: %#v", got)
	}
	if s.EstimatedTokens() > 100 {
		t.Fatalf("token 估算 = %d, 超过预算 100", s.EstimatedTokens())
	}
}

func TestSessionRejectsSingleMessageOverTokenBudget(t *testing.T) {
	s, err := Open(0, 20, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendUser(strings.Repeat("中", 20)); err == nil {
		t.Fatal("单条消息超过预算时应该报错")
	}
	if s.Len() != 0 {
		t.Fatalf("超限消息不应写入历史，实际 %d 条", s.Len())
	}
}

type wantMessage struct {
	role    schema.RoleType
	content string
}

func assertMessages(t *testing.T, got []*schema.Message, want []wantMessage) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("消息数 = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Role != want[i].role || got[i].Content != want[i].content {
			t.Errorf("第 %d 条 = (%s, %q), want (%s, %q)", i+1,
				got[i].Role, got[i].Content, want[i].role, want[i].content)
		}
	}
}
