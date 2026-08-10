package test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"

	projectskills "diagnostic-system/internal/skills"
)

func TestSkillMiddlewareLoadsInlineSkill(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "evidence")
	writeSkill(t, skillDir, `---
name: evidence
description: Collect diagnostic evidence.
---

Always collect read-only evidence first.
`)

	handler, matters, err := projectskills.Load(context.Background(), dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(matters) != 1 || matters[0].Name != "evidence" {
		t.Fatalf("matters = %#v", matters)
	}

	runCtx := &adk.ChatModelAgentContext{Instruction: "base"}
	_, got, err := handler.BeforeAgent(context.Background(), runCtx)
	if err != nil {
		t.Fatalf("BeforeAgent() error = %v", err)
	}
	if !strings.Contains(got.Instruction, "Skill 系统") {
		t.Fatalf("instruction 没有 Skill middleware 提示: %q", got.Instruction)
	}
	if len(got.Tools) != 1 {
		t.Fatalf("len(tools) = %d, want 1", len(got.Tools))
	}

	info, err := got.Tools[0].Info(context.Background())
	if err != nil {
		t.Fatalf("skill tool Info() error = %v", err)
	}
	if info.Name != "skill" || !strings.Contains(info.Desc, "evidence") {
		t.Fatalf("skill tool info = %#v", info)
	}
	invokable, ok := got.Tools[0].(tool.InvokableTool)
	if !ok {
		t.Fatalf("skill tool type = %T, want tool.InvokableTool", got.Tools[0])
	}
	result, err := invokable.InvokableRun(context.Background(), `{"skill":"evidence"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if !strings.Contains(result, "Always collect read-only evidence first.") ||
		!strings.Contains(result, skillDir) {
		t.Fatalf("skill result = %q", result)
	}
}

func TestSkillMiddlewareRejectsForkMode(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, filepath.Join(dir, "forked"), `---
name: forked
description: Forked test skill.
context: fork
---

Do work in a child agent.
`)

	_, _, err := projectskills.Load(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "当前仅支持 inline Skill") {
		t.Fatalf("Load() error = %v, want inline-only error", err)
	}
}

func TestSkillMiddlewareRejectsMissingDescription(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, filepath.Join(dir, "invalid"), `---
name: invalid
---

Missing description.
`)

	_, _, err := projectskills.Load(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "description 不能为空") {
		t.Fatalf("Load() error = %v, want description error", err)
	}
}

func writeSkill(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
