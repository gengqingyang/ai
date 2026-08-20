package test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"diagnostic-system/internal/approval"
)

func TestCheckpointStoreRoundTrips(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "checkpoints")
	store, err := approval.OpenCheckpointStore(dir)
	if err != nil {
		t.Fatalf("OpenCheckpointStore() error = %v", err)
	}

	if _, ok, err := store.Get(ctx, "run-absent"); err != nil || ok {
		t.Fatalf("读取不存在的快照 = (%v, %v), want (false, nil)", ok, err)
	}

	want := []byte{0x0f, 0x00, 0xff, 'g', 'o', 'b'}
	if err := store.Set(ctx, "run-1", want); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	got, ok, err := store.Get(ctx, "run-1")
	if err != nil || !ok {
		t.Fatalf("Get() = (%v, %v), want (true, nil)", ok, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("取回的快照 = %v, want %v", got, want)
	}

	// 同一轮可能中断多次，后写的必须完整覆盖前一份。
	next := []byte("second")
	if err := store.Set(ctx, "run-1", next); err != nil {
		t.Fatalf("覆盖 Set() error = %v", err)
	}
	if got, _, _ := store.Get(ctx, "run-1"); string(got) != "second" {
		t.Errorf("覆盖后的快照 = %q, want %q", got, "second")
	}

	if err := store.Delete(ctx, "run-1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, ok, _ := store.Get(ctx, "run-1"); ok {
		t.Error("删除后仍能读到快照")
	}
	// 清理路径会被重复调用（正常结束 + 出错各走一次），不能因此报错。
	if err := store.Delete(ctx, "run-1"); err != nil {
		t.Errorf("重复 Delete() error = %v", err)
	}
}

// 快照里含有本轮完整的模型消息，权限必须和提案状态文件一样紧。
func TestCheckpointStoreKeepsPermissionsTight(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "checkpoints")
	store, err := approval.OpenCheckpointStore(dir)
	if err != nil {
		t.Fatalf("OpenCheckpointStore() error = %v", err)
	}
	if err := store.Set(context.Background(), "run-perm", []byte("x")); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat(dir) error = %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("快照目录权限 = %o, want 700", perm)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("快照目录里有 %d 个文件, want 1（临时文件应已被替换）", len(entries))
	}
	fileInfo, err := entries[0].Info()
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Errorf("快照文件权限 = %o, want 600", perm)
	}
}

// checkpoint ID 会直接变成文件名，越界的 ID 必须在落盘前被挡下。
func TestCheckpointStoreRejectsUnsafeIDs(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := approval.OpenCheckpointStore(filepath.Join(root, "checkpoints"))
	if err != nil {
		t.Fatalf("OpenCheckpointStore() error = %v", err)
	}

	cases := map[string]string{
		"空":      "",
		"跳出目录":   "../escaped",
		"含斜杠":    "nested/run",
		"绝对路径":   "/etc/passwd",
		"隐藏的父目录": "run/../../escaped",
		"含空格":    "run 1",
		"含点":     "run.1",
		"NUL 字符": "run\x00",
		"超长":     strings.Repeat("a", 129),
	}
	for name, id := range cases {
		t.Run(name, func(t *testing.T) {
			if err := store.Set(ctx, id, []byte("payload")); err == nil {
				t.Fatalf("Set(%q) 未报错，非法 ID 必须在落盘前被拒", id)
			}
			if _, _, err := store.Get(ctx, id); err == nil {
				t.Errorf("Get(%q) 未报错", id)
			}
			if err := store.Delete(ctx, id); err == nil {
				t.Errorf("Delete(%q) 未报错", id)
			}
		})
	}

	// 逐条断言之外再确认一次：临时目录里除了快照目录本身什么都没多出来。
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "checkpoints" {
		t.Fatalf("非法 ID 在快照目录之外写出了文件: %v", entries)
	}
}

// IDs 是重启后核对「提案记的那份快照还在不在」的唯一入口。
func TestCheckpointStoreListsIDs(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "checkpoints")
	store, err := approval.OpenCheckpointStore(dir)
	if err != nil {
		t.Fatalf("OpenCheckpointStore() error = %v", err)
	}
	for _, id := range []string{"run-c", "run-a", "run-b"} {
		if err := store.Set(ctx, id, []byte(id)); err != nil {
			t.Fatalf("Set(%q) error = %v", id, err)
		}
	}
	// 目录里的无关文件不能被当成快照。
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	ids, err := store.IDs()
	if err != nil {
		t.Fatalf("IDs() error = %v", err)
	}
	if want := []string{"run-a", "run-b", "run-c"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("IDs() = %v, want %v", ids, want)
	}
}

// 重开同一个目录必须看到上一进程留下的快照，这是跨重启恢复的前提。
func TestCheckpointStoreReopensExistingDir(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "checkpoints")

	first, err := approval.OpenCheckpointStore(dir)
	if err != nil {
		t.Fatalf("OpenCheckpointStore() error = %v", err)
	}
	if err := first.Set(ctx, "run-kept", []byte("payload")); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	second, err := approval.OpenCheckpointStore(dir)
	if err != nil {
		t.Fatalf("重开 OpenCheckpointStore() error = %v", err)
	}
	got, ok, err := second.Get(ctx, "run-kept")
	if err != nil || !ok {
		t.Fatalf("重开后 Get() = (%v, %v), want (true, nil)", ok, err)
	}
	if string(got) != "payload" {
		t.Errorf("重开后取回 = %q, want %q", got, "payload")
	}
	if second.Dir() != dir {
		t.Errorf("Dir() = %q, want %q", second.Dir(), dir)
	}
}

func TestOpenCheckpointStoreRejectsBadDir(t *testing.T) {
	if _, err := approval.OpenCheckpointStore("   "); err == nil {
		t.Error("空目录名未报错")
	}
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := approval.OpenCheckpointStore(file); err == nil {
		t.Error("把普通文件当快照目录未报错")
	}
}

// 目录权限过松时要收紧，而不是接受现状继续写入。
func TestOpenCheckpointStoreTightensLoosePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "checkpoints")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if _, err := approval.OpenCheckpointStore(dir); err != nil {
		t.Fatalf("OpenCheckpointStore() error = %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("目录权限 = %o, want 700（应被收紧）", perm)
	}
}
