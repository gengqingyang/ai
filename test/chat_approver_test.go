package test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"diagnostic-system/internal/approval"
	. "diagnostic-system/internal/chat"
	"diagnostic-system/internal/tools"
)

func testProposal() (*approval.Proposal, tools.RiskAssessment) {
	p := approval.NewStore().Create("run_tunnel_cmd", `{"sn":"SN001","cmd":"date","purpose":"查看节点系统时间"}`)
	return p, tools.AssessRisk(p.Tool, p.Args)
}

func updateRoot(t *testing.T, model Model, msg tea.Msg) Model {
	t.Helper()
	updated, _ := model.Update(msg)
	root, ok := updated.(Model)
	if !ok {
		t.Fatalf("Update 返回了 %T, want Model", updated)
	}
	return root
}

func TestUIApproverApproveThroughRootModel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan tea.Msg, 4)
	approver := NewUIApprover(ctx, events, "tester")
	p, risk := testProposal()

	type outcome struct {
		decision tools.Decision
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		decision, err := approver.Review(ctx, p, risk)
		done <- outcome{decision: decision, err: err}
	}()

	model := NewModel(ctx, nil, events, "")
	model = updateRoot(t, model, <-events)
	if model.Mode() != "approval" {
		t.Fatalf("mode = %q, want approval", model.Mode())
	}
	for _, want := range []string{"查看节点系统时间", "$ date", "只读", "SN001", p.ID} {
		if !strings.Contains(model.View(), want) {
			t.Errorf("审批界面没有 %q:\n%s", want, model.View())
		}
	}

	model = updateRoot(t, model, tea.KeyMsg{Type: tea.KeyEnter})
	result := <-done
	if result.err != nil || !result.decision.Approved || result.decision.Decider != "tester" {
		t.Fatalf("Review() = %#v, err=%v", result.decision, result.err)
	}
	if model.Mode() != "busy" {
		t.Fatalf("批准后 mode = %q, want busy", model.Mode())
	}
}

func TestUIApproverRejectReasonThroughRootModel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan tea.Msg, 4)
	approver := NewUIApprover(ctx, events, "tester")
	p, risk := testProposal()

	done := make(chan tools.Decision, 1)
	go func() {
		decision, _ := approver.Review(ctx, p, risk)
		done <- decision
	}()

	model := NewModel(ctx, nil, events, "")
	model = updateRoot(t, model, <-events)
	model = updateRoot(t, model, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if model.Mode() != "rejection_reason" {
		t.Fatalf("mode = %q, want rejection_reason", model.Mode())
	}
	model = updateRoot(t, model, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("线上高峰期")})
	model = updateRoot(t, model, tea.KeyMsg{Type: tea.KeyEnter})

	decision := <-done
	if decision.Approved || decision.Decider != "tester" || decision.Reason != "线上高峰期" {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestUIApproverClosedUIFailsClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan tea.Msg)
	approver := NewUIApprover(ctx, events, "tester")
	cancel()
	p, risk := testProposal()

	decision, err := approver.Review(context.Background(), p, risk)
	if !errors.Is(err, ErrNoApproval) || decision.Approved {
		t.Fatalf("Review() = %#v, err=%v; want fail-closed", decision, err)
	}
}

func TestUIApproverHonorsRequestCancellation(t *testing.T) {
	events := make(chan tea.Msg)
	approver := NewUIApprover(context.Background(), events, "tester")
	p, risk := testProposal()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	decision, err := approver.Review(ctx, p, risk)
	if !errors.Is(err, context.Canceled) || decision.Approved {
		t.Fatalf("Review() = %#v, err=%v", decision, err)
	}
}

func TestUIApproverNoticeUsesBubbleTeaEvent(t *testing.T) {
	events := make(chan tea.Msg, 1)
	approver := NewUIApprover(context.Background(), events, "tester")
	p, _ := testProposal()
	p.Status = approval.StatusFailed
	p.Error = "调用超时"
	approver.Notice(p)

	model := NewModel(context.Background(), nil, events, "")
	model = updateRoot(t, model, <-events)
	if !strings.Contains(model.TranscriptText(), "执行失败：调用超时") {
		t.Fatalf("notice 未进入 UI transcript: %s", model.TranscriptText())
	}
}

func TestApprovalCardDoesNotTruncateCommand(t *testing.T) {
	long := "journalctl -u onething-agent --since '2026-07-30 00:00:00' --no-pager | grep -i error"
	p := approval.NewStore().Create("run_tunnel_cmd", `{"sn":"SN001","cmd":"`+long+`"}`)
	card := ApprovalCard(p, tools.AssessRisk(p.Tool, p.Args))
	if !strings.Contains(card, long) || !strings.Contains(card, "run_tunnel_cmd") {
		t.Fatalf("审批卡片信息不完整:\n%s", card)
	}
}

func TestUIApproverReviewWaitsForExplicitDecision(t *testing.T) {
	events := make(chan tea.Msg, 1)
	ctx, cancel := context.WithCancel(context.Background())
	approver := NewUIApprover(ctx, events, "tester")
	p, risk := testProposal()
	done := make(chan struct{})
	go func() {
		_, _ = approver.Review(context.Background(), p, risk)
		close(done)
	}()
	<-events
	select {
	case <-done:
		t.Fatal("没有明确决策时 Review 不应返回")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	<-done
}
