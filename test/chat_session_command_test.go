package test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	. "diagnostic-system/internal/chat"
	"diagnostic-system/internal/repository"
	"diagnostic-system/internal/session"
)

func TestSessionCommandsCreateAndSwitch(t *testing.T) {
	store, err := session.OpenStore(0, 1000, "")
	if err != nil {
		t.Fatal(err)
	}
	first, firstInfo := store.Current()
	a := NewApp(nil, store, first, 0, "")

	handled, err := a.RunCommand(context.Background(), "/new CDN 故障")
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("/new 没有被命令处理器消费")
	}
	created := a.CurrentSession()
	if created == nil || created == first {
		t.Fatal("/new 没有切换到新会话")
	}
	_, createdInfo := store.Current()
	if createdInfo.Name != "CDN 故障" {
		t.Fatalf("新会话名称 = %q", createdInfo.Name)
	}

	handled, err = a.RunCommand(context.Background(), "/switch "+firstInfo.ShortID())
	if err != nil {
		t.Fatal(err)
	}
	if !handled || a.CurrentSession() != first {
		t.Fatal("/switch 没有切回原会话")
	}
}

func TestRepositoryCommandsAddListUseAndReindex(t *testing.T) {
	store, err := session.OpenStore(0, 1000, "")
	if err != nil {
		t.Fatal(err)
	}
	current, _ := store.Current()
	manager, err := repository.Open(filepath.Join(t.TempDir(), "repositories.json"))
	if err != nil {
		t.Fatal(err)
	}
	a := NewApp(nil, store, current, 0, "", manager)
	root := makeCodeRepository(t)

	for _, command := range []string{
		"/repo add " + root + " installer",
		"/repos",
		"/repo use installer",
		"/repo reindex",
	} {
		handled, err := a.RunCommand(context.Background(), command)
		if err != nil || !handled {
			t.Fatalf("RunCommand(%q) handled=%v err=%v", command, handled, err)
		}
	}
	items := a.Repositories()
	if len(items) != 1 || !items[0].Active || items[0].Name != "installer" {
		t.Fatalf("repositories=%#v", items)
	}
}

func TestRepositoryAddCommandAcceptsQuotedPath(t *testing.T) {
	store, err := session.OpenStore(0, 1000, "")
	if err != nil {
		t.Fatal(err)
	}
	current, _ := store.Current()
	manager, err := repository.Open("")
	if err != nil {
		t.Fatal(err)
	}
	a := NewApp(nil, store, current, 0, "", manager)
	parent := t.TempDir()
	root := filepath.Join(parent, "repo with spaces")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	handled, err := a.RunCommand(context.Background(), `/repo add "`+root+`" "安装 源码"`)
	if err != nil || !handled {
		t.Fatalf("quoted /repo add handled=%v err=%v", handled, err)
	}
	wantRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	items := a.Repositories()
	if len(items) != 1 || items[0].Name != "安装 源码" || items[0].Root != wantRoot {
		t.Fatalf("repositories=%#v", items)
	}
}

func TestSwitchCommandRequiresTarget(t *testing.T) {
	store, err := session.OpenStore(0, 1000, "")
	if err != nil {
		t.Fatal(err)
	}
	current, _ := store.Current()
	a := NewApp(nil, store, current, 0, "")

	handled, err := a.RunCommand(context.Background(), "/switch")
	if !handled || err == nil {
		t.Fatalf("/switch 无参数: handled=%v err=%v", handled, err)
	}
}

func TestCommandArgsKeepsSessionNameSpaces(t *testing.T) {
	if got := CommandArgs("/new   CDN 华东 节点"); got != "CDN 华东 节点" {
		t.Fatalf("commandArgs() = %q", got)
	}
}
