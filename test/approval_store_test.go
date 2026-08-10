package test

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	. "diagnostic-system/internal/approval"
)

func fixedClock() func() time.Time {
	t := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

func TestCreateStartsPending(t *testing.T) {
	s := NewStore(WithClock(fixedClock()))

	p := s.Create("run_tunnel_cmd", `{"sn":"SN001","cmd":"uptime"}`)
	if p.Status != StatusPending {
		t.Fatalf("新建提案状态 = %s, want %s", p.Status, StatusPending)
	}
	if !p.Pending() {
		t.Error("Pending() = false, want true")
	}
	if p.ID == "" {
		t.Error("提案 ID 为空")
	}
	if got := len(s.Pending()); got != 1 {
		t.Errorf("待审数量 = %d, want 1", got)
	}
}

// 返回的必须是副本：调用方改到手里的东西，不能影响 store 里的真实状态。
func TestReturnedProposalIsCopy(t *testing.T) {
	s := NewStore(WithClock(fixedClock()))
	p := s.Create("t", "{}")

	p.Status = StatusExecuted
	p.Args = "篡改过的参数"

	got, ok := s.Get(p.ID)
	if !ok {
		t.Fatal("提案不存在")
	}
	if got.Status != StatusPending {
		t.Errorf("store 内状态被外部修改: %s", got.Status)
	}
	if got.Args != "{}" {
		t.Errorf("store 内参数被外部修改: %s", got.Args)
	}
}

// 这个用例是整套审核机制的核心不变量：没批准就不能进入执行态。
func TestExecuteRequiresApproval(t *testing.T) {
	s := NewStore(WithClock(fixedClock()))
	p := s.Create("run_tunnel_cmd", "{}")

	if _, err := s.MarkExecuted(p.ID, "output", nil); err == nil {
		t.Fatal("未经批准就流转到执行态，审核闸门被绕过")
	}

	got, _ := s.Get(p.ID)
	if got.Status != StatusPending {
		t.Errorf("失败的流转不该改变状态，当前 = %s", got.Status)
	}
}

func TestApproveThenExecute(t *testing.T) {
	s := NewStore(WithClock(fixedClock()))
	p := s.Create("run_tunnel_cmd", "{}")

	approved, err := s.Approve(p.ID, "alice")
	if err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	if approved.Status != StatusApproved {
		t.Errorf("批准后状态 = %s, want %s", approved.Status, StatusApproved)
	}
	if approved.Decider != "alice" {
		t.Errorf("批准人 = %q, want alice", approved.Decider)
	}
	if approved.DecidedAt == nil {
		t.Error("批准时间未记录")
	}

	done, err := s.MarkExecuted(p.ID, "load average: 0.1", nil)
	if err != nil {
		t.Fatalf("MarkExecuted() error = %v", err)
	}
	if done.Status != StatusExecuted {
		t.Errorf("执行后状态 = %s, want %s", done.Status, StatusExecuted)
	}
	if done.Result != "load average: 0.1" {
		t.Errorf("执行输出 = %q", done.Result)
	}
	if len(s.Pending()) != 0 {
		t.Error("已执行的提案仍出现在待审列表里")
	}
}

func TestExecuteFailureRecorded(t *testing.T) {
	s := NewStore(WithClock(fixedClock()))
	p := s.Create("run_tunnel_cmd", "{}")
	if _, err := s.Approve(p.ID, "alice"); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}

	done, err := s.MarkExecuted(p.ID, "", errors.New("连接超时"))
	if err != nil {
		t.Fatalf("MarkExecuted() error = %v", err)
	}
	if done.Status != StatusFailed {
		t.Errorf("状态 = %s, want %s", done.Status, StatusFailed)
	}
	if done.Error != "连接超时" {
		t.Errorf("错误信息 = %q", done.Error)
	}
}

func TestTerminalStatesAreFinal(t *testing.T) {
	tests := []struct {
		name string
		// settle 把提案推到某个终态
		settle func(t *testing.T, s *Store, id string)
	}{
		{
			name: "已驳回不可再批准",
			settle: func(t *testing.T, s *Store, id string) {
				if _, err := s.Reject(id, "alice", "风险太大"); err != nil {
					t.Fatalf("Reject() error = %v", err)
				}
			},
		},
		{
			name: "已执行不可再批准",
			settle: func(t *testing.T, s *Store, id string) {
				if _, err := s.Approve(id, "alice"); err != nil {
					t.Fatalf("Approve() error = %v", err)
				}
				if _, err := s.MarkExecuted(id, "ok", nil); err != nil {
					t.Fatalf("MarkExecuted() error = %v", err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore(WithClock(fixedClock()))
			p := s.Create("run_tunnel_cmd", "{}")
			tc.settle(t, s, p.ID)

			if _, err := s.Approve(p.ID, "bob"); err == nil {
				t.Error("终态提案被重新批准")
			}
			if _, err := s.Reject(p.ID, "bob", "再驳一次"); err == nil {
				t.Error("终态提案被重新驳回")
			}
		})
	}
}

// 重复批准会导致同一条命令被执行两次，必须挡住。
func TestApproveIsIdempotentlyRejected(t *testing.T) {
	s := NewStore(WithClock(fixedClock()))
	p := s.Create("run_tunnel_cmd", "{}")

	if _, err := s.Approve(p.ID, "alice"); err != nil {
		t.Fatalf("首次 Approve() error = %v", err)
	}
	if _, err := s.Approve(p.ID, "alice"); err == nil {
		t.Fatal("同一条提案被批准了两次")
	}
}

func TestUnknownIDErrors(t *testing.T) {
	s := NewStore(WithClock(fixedClock()))

	if _, ok := s.Get("P999"); ok {
		t.Error("Get() 返回了不存在的提案")
	}
	if _, err := s.Approve("P999", "alice"); err == nil {
		t.Error("Approve() 未知 ID 应报错")
	}
	if _, err := s.Reject("P999", "alice", "x"); err == nil {
		t.Error("Reject() 未知 ID 应报错")
	}
}

func TestIDsAreUniqueAndOrdered(t *testing.T) {
	s := NewStore(WithClock(fixedClock()))
	seen := make(map[string]bool)
	var created []string
	for range 5 {
		p := s.Create("t", "{}")
		if seen[p.ID] {
			t.Fatalf("提案 ID 重复: %s", p.ID)
		}
		seen[p.ID] = true
		created = append(created, p.ID)
	}

	all := s.All()
	if len(all) != len(created) {
		t.Fatalf("All() 返回 %d 条, want %d", len(all), len(created))
	}
	for i, p := range all {
		if p.ID != created[i] {
			t.Errorf("All()[%d].ID = %s, want %s（未按创建顺序返回）", i, p.ID, created[i])
		}
	}
}

func TestAuditLogRecordsEveryTransition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	s := NewStore(WithClock(fixedClock()), WithAuditLog(path))

	p := s.Create("run_tunnel_cmd", `{"sn":"SN001","cmd":"systemctl restart foo"}`)
	if _, err := s.Approve(p.ID, "alice"); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	if _, err := s.MarkExecuted(p.ID, "done", nil); err != nil {
		t.Fatalf("MarkExecuted() error = %v", err)
	}

	events, lines := readAudit(t, path)
	want := []string{"created", "approved", string(StatusExecuted)}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Errorf("审计事件 = %v, want %v", events, want)
	}

	// 审计行必须能定位到「谁、对哪台机器、批了什么命令」，缺一条都没法追责。
	last := lines[len(lines)-1]
	for _, must := range []string{p.ID, "alice", "SN001", "systemctl restart foo"} {
		if !strings.Contains(last, must) {
			t.Errorf("审计行缺少 %q: %s", must, last)
		}
	}
}

func TestAuditLogRecordsRejection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	s := NewStore(WithClock(fixedClock()), WithAuditLog(path))

	p := s.Create("run_tunnel_cmd", "{}")
	if _, err := s.Reject(p.ID, "bob", "影响面太大"); err != nil {
		t.Fatalf("Reject() error = %v", err)
	}

	events, lines := readAudit(t, path)
	if strings.Join(events, ",") != "created,rejected" {
		t.Errorf("审计事件 = %v", events)
	}
	if !strings.Contains(lines[1], "影响面太大") {
		t.Errorf("驳回理由未落审计: %s", lines[1])
	}
}

// 审计文件是追加的：同一路径的多个 Store 实例不会互相截断。
func TestAuditLogAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")

	for range 2 {
		s := NewStore(WithClock(fixedClock()), WithAuditLog(path))
		s.Create("t", "{}")
	}
	events, _ := readAudit(t, path)
	if len(events) != 2 {
		t.Errorf("审计行数 = %d, want 2（第二个 Store 截断了文件）", len(events))
	}
}

func TestConcurrentCreateAndTransition(t *testing.T) {
	s := NewStore(WithClock(fixedClock()))

	const n = 50
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i] = s.Create("t", "{}").ID
		}(i)
	}
	wg.Wait()

	if len(s.All()) != n {
		t.Fatalf("并发创建后总数 = %d, want %d", len(s.All()), n)
	}

	// 每条提案让两个 goroutine 抢批准，只能有一个成功。
	var okCount int64
	var mu sync.Mutex
	for _, id := range ids {
		wg.Add(2)
		for range 2 {
			go func(id string) {
				defer wg.Done()
				if _, err := s.Approve(id, "racer"); err == nil {
					mu.Lock()
					okCount++
					mu.Unlock()
				}
			}(id)
		}
	}
	wg.Wait()

	if okCount != n {
		t.Errorf("成功批准次数 = %d, want %d（存在重复批准）", okCount, n)
	}
}

// readAudit 读回审计文件，返回事件序列和原始行。
func readAudit(t *testing.T, path string) ([]string, []string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开审计日志失败: %v", err)
	}
	defer f.Close()

	var events, lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		var entry struct {
			Event string `json:"event"`
			ID    string `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("审计行不是合法 JSON: %v\n%s", err, line)
		}
		if entry.Event == "" {
			t.Errorf("审计行缺少 event 字段: %s", line)
		}
		events = append(events, entry.Event)
		lines = append(lines, line)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("读审计日志失败: %v", err)
	}
	return events, lines
}
