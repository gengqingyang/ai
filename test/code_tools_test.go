package test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"

	"diagnostic-system/internal/repository"
	projecttools "diagnostic-system/internal/tools"
)

func TestCodeToolsAreStructuredReadOnlyAndUseSafeIndex(t *testing.T) {
	manager, err := repository.Open(filepath.Join(t.TempDir(), "repositories.json"))
	if err != nil {
		t.Fatal(err)
	}
	root := makeCodeRepository(t)
	writeTestFile(t, filepath.Join(root, "long.md"), strings.Repeat("line\n", 300))
	if _, err := manager.Add(context.Background(), root, "installer"); err != nil {
		t.Fatal(err)
	}
	codeTools, err := projecttools.NewCodeTools(manager)
	if err != nil {
		t.Fatal(err)
	}
	if len(codeTools) != 7 {
		t.Fatalf("len(codeTools)=%d, want 7", len(codeTools))
	}

	registry := projecttools.NewRegistry()
	names := make(map[string]tool.InvokableTool)
	for _, codeTool := range codeTools {
		if err := registry.Register(context.Background(), codeTool, projecttools.RiskReadOnly); err != nil {
			t.Fatal(err)
		}
		info, err := codeTool.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		schema, err := info.ToJSONSchema()
		if err != nil || schema == nil {
			t.Fatalf("%s schema=%#v err=%v", info.Name, schema, err)
		}
		invokable, ok := codeTool.(tool.InvokableTool)
		if !ok {
			t.Fatalf("%s type=%T, want InvokableTool", info.Name, codeTool)
		}
		names[info.Name] = invokable
	}
	if len(registry.ReadOnly()) != 7 || len(registry.Mutating()) != 0 {
		t.Fatalf("read-only=%d mutating=%d", len(registry.ReadOnly()), len(registry.Mutating()))
	}
	selected, err := registry.Named(projecttools.CodeToolNames()...)
	if err != nil || len(selected) != 7 {
		t.Fatalf("Named() len=%d err=%v", len(selected), err)
	}

	calls := map[string]string{
		projecttools.ToolListFiles:             `{"prefix":"internal"}`,
		projecttools.ToolSearchCode:            `{"query":"activation failed","case_sensitive":true}`,
		projecttools.ToolReadFile:              `{"path":"internal/install.go","start_line":9,"end_line":15}`,
		projecttools.ToolFindSymbol:            `{"name":"Installer.Run"}`,
		projecttools.ToolFindReferences:        `{"name":"validateConfig","calls_only":true}`,
		projecttools.ToolGetDefinition:         `{"name":"validateConfig","context_lines":1}`,
		projecttools.ToolGetRepositoryRevision: `{}`,
	}
	for name, input := range calls {
		output, err := names[name].InvokableRun(context.Background(), input)
		if err != nil {
			t.Fatalf("%s error=%v", name, err)
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(output), &decoded); err != nil {
			t.Fatalf("%s output is not JSON: %v\n%s", name, err, output)
		}
		if decoded["data_source"] != "local_repository" || decoded["snapshot"] == nil {
			t.Errorf("%s output=%s", name, output)
		}
		if name == projecttools.ToolGetRepositoryRevision {
			snapshot := decoded["snapshot"].(map[string]any)
			if snapshot["git_commit"] == "" || snapshot["current_git_commit"] != snapshot["git_commit"] ||
				snapshot["stale"] != false {
				t.Fatalf("revision snapshot=%#v", snapshot)
			}
			if _, exists := snapshot["stale_reasons"]; exists {
				t.Fatalf("fresh revision should omit stale_reasons: %#v", snapshot)
			}
			if _, exists := snapshot["stale_paths"]; exists {
				t.Fatalf("fresh revision should omit stale_paths: %#v", snapshot)
			}
		}
	}

	if _, err := names[projecttools.ToolReadFile].InvokableRun(
		context.Background(), `{"path":"../.env"}`); err == nil || !strings.Contains(err.Error(), "仓库根目录") {
		t.Fatalf("unsafe read error=%v", err)
	}

	pageJSON, err := names[projecttools.ToolReadFile].InvokableRun(
		context.Background(), `{"path":"long.md","start_line":1,"end_line":9999}`)
	if err != nil {
		t.Fatalf("oversized read should paginate instead of failing: %v", err)
	}
	var page struct {
		Status        string                  `json:"status"`
		EndLine       int                     `json:"end_line"`
		TotalLines    int                     `json:"total_lines"`
		Truncated     bool                    `json:"truncated"`
		HasMore       bool                    `json:"has_more"`
		NextStartLine int                     `json:"next_start_line"`
		Lines         []repository.SourceLine `json:"lines"`
	}
	if err := json.Unmarshal([]byte(pageJSON), &page); err != nil {
		t.Fatal(err)
	}
	if page.Status != "ok" || page.EndLine != 200 || page.TotalLines != 300 || !page.Truncated ||
		!page.HasMore || page.NextStartLine != 201 || len(page.Lines) != 200 {
		t.Fatalf("oversized read page=%#v", page)
	}
	if _, err := names[projecttools.ToolGetDefinition].InvokableRun(
		context.Background(), `{"name":"Installer.Run","context_lines":999}`); err != nil {
		t.Fatalf("oversized definition context should be clamped: %v", err)
	}
}

func TestCodeToolsRequireActiveRepository(t *testing.T) {
	manager, err := repository.Open("")
	if err != nil {
		t.Fatal(err)
	}
	codeTools, err := projecttools.NewCodeTools(manager)
	if err != nil {
		t.Fatal(err)
	}
	invokable := codeTools[0].(tool.InvokableTool)
	if _, err := invokable.InvokableRun(context.Background(), `{}`); err == nil || !strings.Contains(err.Error(), "/repo add") {
		t.Fatalf("InvokableRun() error=%v", err)
	}
}
