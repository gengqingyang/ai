package test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk/filesystem"

	"diagnostic-system/internal/repository"
)

func TestRepositoryIndexesCodeAndKeepsCitations(t *testing.T) {
	root := makeCodeRepository(t)
	manager, err := repository.Open(filepath.Join(t.TempDir(), "repositories.json"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := manager.Add(context.Background(), root, "installer")
	if err != nil {
		t.Fatal(err)
	}
	if info.FileCount != 3 || info.SymbolCount == 0 || !info.Active {
		t.Fatalf("repository info = %#v", info)
	}

	snapshot, matches, err := manager.Search(context.Background(), "activation failed", "", true, 20)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Repository != "installer" || snapshot.Stale || len(matches) != 1 ||
		matches[0].Path != "internal/install.go" || matches[0].Line != 16 {
		t.Fatalf("snapshot=%#v matches=%#v", snapshot, matches)
	}

	_, symbols, err := manager.FindSymbols(context.Background(), "Installer.Run", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(symbols) != 1 || symbols[0].Kind != "method" || symbols[0].Receiver != "Installer" ||
		symbols[0].Location.Path != "internal/install.go" || symbols[0].Location.Line != 11 ||
		!strings.Contains(symbols[0].Documentation, "validates the activation") {
		t.Fatalf("symbols = %#v", symbols)
	}

	_, refs, err := manager.FindReferences(context.Background(), "validateConfig", true, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Kind != "call" || refs[0].InSymbol != "Installer.Run" || refs[0].Location.Line != 12 {
		t.Fatalf("references = %#v", refs)
	}

	_, definitions, err := manager.Definitions(context.Background(), "validateConfig", 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 1 || definitions[0].Symbol.Location.Line != 15 || len(definitions[0].Lines) == 0 {
		t.Fatalf("definitions = %#v", definitions)
	}
}

func TestLocalRepoBackendImplementsReadOnlyEinoFilesystem(t *testing.T) {
	manager, err := repository.Open("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Add(context.Background(), makeCodeRepository(t), "backend"); err != nil {
		t.Fatal(err)
	}
	backend, err := repository.NewLocalRepoBackend(manager)
	if err != nil {
		t.Fatal(err)
	}
	files, err := backend.LsInfo(context.Background(), &filesystem.LsInfoRequest{Path: "internal"})
	if err != nil || len(files) != 1 || files[0].Path != "internal/install.go" {
		t.Fatalf("LsInfo()=%#v err=%v", files, err)
	}
	content, err := backend.Read(context.Background(), &filesystem.ReadRequest{
		FilePath: "internal/install.go", Offset: 10, Limit: 500,
	})
	if err != nil || !strings.Contains(content.Content, "Installer) Run") {
		t.Fatalf("Read()=%#v err=%v", content, err)
	}
	matches, err := backend.GrepRaw(context.Background(), &filesystem.GrepRequest{
		Pattern: "activation\\s+failed", Path: "internal",
	})
	if err != nil || len(matches) != 1 || matches[0].Line != 16 {
		t.Fatalf("GrepRaw()=%#v err=%v", matches, err)
	}
	if err := backend.Write(context.Background(), &filesystem.WriteRequest{FilePath: "new.go"}); err == nil {
		t.Fatal("只读 backend 允许 Write")
	}
	if err := backend.Edit(context.Background(), &filesystem.EditRequest{FilePath: "internal/install.go"}); err == nil {
		t.Fatal("只读 backend 允许 Edit")
	}
}

func TestRepositoryExcludesSecretsBinaryLogsGeneratedAndSymlinks(t *testing.T) {
	root := makeCodeRepository(t)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	writeTestFile(t, outside, "do not read\n")
	if err := os.Symlink(outside, filepath.Join(root, "outside-link.txt")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, ".env"), "TOKEN=secret\n")
	writeTestFile(t, filepath.Join(root, "diagnostic.log"), "secret log\n")
	writeTestFile(t, filepath.Join(root, ".repositories.json"), "repository state\n")
	writeTestFile(t, filepath.Join(root, ".approvals.json"), "approval state\n")
	writeTestFile(t, filepath.Join(root, ".chat_history_sessions", "private.json"), "private chat\n")
	writeTestFile(t, filepath.Join(root, "private.txt"), "-----BEGIN PRIVATE KEY-----\nsecret\n")
	writeTestFile(t, filepath.Join(root, "blob.bin"), string([]byte{0, 1, 2, 3}))
	writeTestFile(t, filepath.Join(root, "node_modules", "generated.js"), "generated\n")

	manager, err := repository.Open("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Add(context.Background(), root, "safe"); err != nil {
		t.Fatal(err)
	}
	_, files, err := manager.ListFiles(context.Background(), "", 100)
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	joined := strings.Join(paths, "\n")
	for _, excluded := range []string{
		".env", "diagnostic.log", ".repositories.json", ".approvals.json", "private.json",
		"private.txt", "blob.bin", "generated.js", "outside-link.txt",
	} {
		if strings.Contains(joined, excluded) {
			t.Errorf("安全索引包含了 %q: %v", excluded, paths)
		}
		if _, _, err := manager.ReadFile(context.Background(), excluded, 1, 10); err == nil {
			t.Errorf("被排除文件 %q 仍可读取", excluded)
		}
	}
	for _, unsafe := range []string{"../outside.txt", outside, "/etc/passwd"} {
		if _, _, err := manager.ReadFile(context.Background(), unsafe, 1, 10); err == nil {
			t.Errorf("越界路径 %q 未被拒绝", unsafe)
		}
	}
}

func TestRepositoryPersistsCatalogReindexesAndDetectsRevisionChange(t *testing.T) {
	root := makeCodeRepository(t)
	statePath := filepath.Join(t.TempDir(), "repositories.json")
	manager, err := repository.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Add(context.Background(), root, "source"); err != nil {
		t.Fatal(err)
	}

	reopened, err := repository.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	items := reopened.List()
	if len(items) != 1 || !items[0].Active || items[0].Name != "source" {
		t.Fatalf("reopened repositories = %#v", items)
	}
	if _, symbols, err := reopened.FindSymbols(context.Background(), "Installer.Run", 20); err != nil || len(symbols) != 1 {
		t.Fatalf("lazy index symbols=%#v err=%v", symbols, err)
	}

	installPath := filepath.Join(root, "internal", "install.go")
	f, err := os.OpenFile(installPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\nfunc RetryInstall() {}\n"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	changedSnapshot, err := reopened.Revision(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !changedSnapshot.Stale {
		t.Fatal("未提交源码发生变化后 snapshot.stale=false")
	}
	if changedSnapshot.CurrentGitCommit != changedSnapshot.GitCommit ||
		!reflect.DeepEqual(changedSnapshot.StaleReasons, []repository.StaleReason{repository.StaleSourceFileChanged}) ||
		!reflect.DeepEqual(changedSnapshot.StalePaths, []string{"internal/install.go"}) {
		t.Fatalf("源码变化原因不准确: %#v", changedSnapshot)
	}
	if _, err := reopened.Reindex(context.Background()); err != nil {
		t.Fatal(err)
	}
	freshSnapshot, err := reopened.Revision(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if freshSnapshot.Stale || len(freshSnapshot.StaleReasons) != 0 {
		t.Fatalf("重索引后仍过期: %#v", freshSnapshot)
	}
	if _, symbols, err := reopened.FindSymbols(context.Background(), "RetryInstall", 20); err != nil || len(symbols) != 1 {
		t.Fatalf("reindexed symbols=%#v err=%v", symbols, err)
	}

	writeTestFile(t, filepath.Join(root, ".git", "refs", "heads", "main"), strings.Repeat("b", 40)+"\n")
	snapshot, err := reopened.Revision(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Stale || snapshot.GitCommit != strings.Repeat("a", 40) ||
		snapshot.CurrentGitCommit != strings.Repeat("b", 40) ||
		!reflect.DeepEqual(snapshot.StaleReasons, []repository.StaleReason{repository.StaleGitCommitChanged}) {
		t.Fatalf("revision snapshot = %#v", snapshot)
	}
}

func TestRepositoryCatalogInsideRootDoesNotInvalidateOwnIndex(t *testing.T) {
	root := makeCodeRepository(t)
	statePath := filepath.Join(root, "catalog-state.json")
	manager, err := repository.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Add(context.Background(), root, "source"); err != nil {
		t.Fatal(err)
	}

	assertFresh := func(stage string) {
		t.Helper()
		snapshot, err := manager.Revision(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Stale || len(snapshot.StaleReasons) != 0 {
			t.Fatalf("%s 后索引过期: %#v", stage, snapshot)
		}
	}
	assertFresh("添加仓库并保存目录")

	tempState := filepath.Join(root, ".catalog-state.json.tmp-interrupted")
	writeTestFile(t, tempState, "temporary state\n")
	assertFresh("出现状态临时文件")
	if err := os.Remove(tempState); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, ".repositories.json"), "repository state changed\n")
	writeTestFile(t, filepath.Join(root, ".approvals.json"), "approval state changed\n")
	writeTestFile(t, filepath.Join(root, ".chat_history_sessions", "session.json"), "chat state changed\n")
	assertFresh("本地状态文件变化")

	if _, err := manager.Reindex(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertFresh("重新索引并保存目录")

	_, files, err := manager.ListFiles(context.Background(), "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file.Path == "catalog-state.json" || strings.Contains(file.Path, "catalog-state.json.tmp-") {
			t.Fatalf("仓库状态文件进入安全索引: %#v", file)
		}
	}
}

func TestRepositoryReportsAddedAndRemovedSourceFiles(t *testing.T) {
	root := makeCodeRepository(t)
	manager, err := repository.Open("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Add(context.Background(), root, "source"); err != nil {
		t.Fatal(err)
	}

	addedPath := filepath.Join(root, "new.go")
	writeTestFile(t, addedPath, "package source\n")
	added, err := manager.Revision(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(added.StaleReasons, []repository.StaleReason{repository.StaleSourceFileAdded}) ||
		!reflect.DeepEqual(added.StalePaths, []string{"new.go"}) {
		t.Fatalf("新增源码原因=%#v", added)
	}
	if _, err := manager.Reindex(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(addedPath); err != nil {
		t.Fatal(err)
	}
	removed, err := manager.Revision(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(removed.StaleReasons, []repository.StaleReason{repository.StaleSourceFileRemoved}) ||
		!reflect.DeepEqual(removed.StalePaths, []string{"new.go"}) {
		t.Fatalf("删除源码原因=%#v", removed)
	}
}

func makeCodeRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "go.mod"), "module example.com/installer\n\ngo 1.24\n")
	writeTestFile(t, filepath.Join(root, "config.yaml"), "retry: 3\n")
	writeTestFile(t, filepath.Join(root, "internal", "install.go"), `// Package install handles node installation.
package install

import "errors"

type Installer struct{}

const RetryLimit = 3

// Run validates the activation configuration.
func (i *Installer) Run() error {
	return validateConfig()
}

func validateConfig() error {
	return errors.New("activation failed")
}
`)
	writeTestFile(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeTestFile(t, filepath.Join(root, ".git", "refs", "heads", "main"), strings.Repeat("a", 40)+"\n")
	return root
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
