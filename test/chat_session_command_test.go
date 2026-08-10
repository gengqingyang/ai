package test

import (
	"context"
	"testing"

	. "diagnostic-system/internal/chat"
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
