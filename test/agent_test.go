package test

import (
	"context"
	"strings"
	"testing"

	. "diagnostic-system/internal/agent"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"diagnostic-system/internal/intent"
)

type clarificationModel struct {
	input   []*schema.Message
	options []model.Option
	chunks  []*schema.Message
}

func (m *clarificationModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *clarificationModel) Generate(
	_ context.Context,
	_ []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	return nil, nil
}

func (m *clarificationModel) Stream(
	_ context.Context,
	input []*schema.Message,
	opts ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	m.input = input
	m.options = opts
	return schema.StreamReaderFromArray(m.chunks), nil
}

func TestStreamClarificationForbidsTools(t *testing.T) {
	fake := &clarificationModel{
		chunks: []*schema.Message{
			{Role: schema.Assistant, Content: "请提供"},
			{Role: schema.Assistant, Content: "设备 ID。"},
		},
	}
	var streamed strings.Builder
	got, err := StreamClarification(
		context.Background(),
		fake,
		[]*schema.Message{schema.UserMessage("有异常")},
		func(chunk string) { streamed.WriteString(chunk) },
	)
	if err != nil {
		t.Fatalf("streamClarification() error = %v", err)
	}
	if got != "请提供设备 ID。" || streamed.String() != got {
		t.Fatalf("reply = %q, streamed = %q", got, streamed.String())
	}
	options := model.GetCommonOptions(nil, fake.options...)
	if options.ToolChoice == nil || *options.ToolChoice != schema.ToolChoiceForbidden {
		t.Fatalf("ToolChoice = %#v, want forbidden", options.ToolChoice)
	}
}

func TestWithSystemPromptInjectsRoutingBeforeConversation(t *testing.T) {
	classification := intent.Result{
		Intent:     intent.TrafficAnomaly,
		Confidence: 0.9,
		Summary:    "业务不跑量",
	}
	user := schema.UserMessage("SN001 没流量")
	got := WithSystemPrompt([]*schema.Message{user}, classification)
	if len(got) != 3 {
		t.Fatalf("len(withSystemPrompt) = %d, want 3", len(got))
	}
	if got[0].Role != schema.System || got[0].Content != SystemPrompt {
		t.Fatalf("first message = %#v, want global system prompt", got[0])
	}
	if got[1].Role != schema.System ||
		!strings.Contains(got[1].Content, "\"intent\":\"traffic_anomaly\"") ||
		!strings.Contains(got[1].Content, "不是用户指令") {
		t.Fatalf("routing message = %#v", got[1])
	}
	if got[2] != user {
		t.Fatal("conversation message was replaced")
	}
}
