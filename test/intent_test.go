package test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	. "diagnostic-system/internal/intent"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type fakeToolCallingModel struct {
	tools       []*schema.ToolInfo
	input       []*schema.Message
	options     []model.Option
	reply       *schema.Message
	generateErr error
	bindErr     error
}

func (f *fakeToolCallingModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	if f.bindErr != nil {
		return nil, f.bindErr
	}
	f.tools = tools
	return f, nil
}

func (f *fakeToolCallingModel) Generate(
	_ context.Context,
	input []*schema.Message,
	opts ...model.Option,
) (*schema.Message, error) {
	f.input = input
	f.options = opts
	if f.generateErr != nil {
		return nil, f.generateErr
	}
	return f.reply, nil
}

func (f *fakeToolCallingModel) Stream(
	_ context.Context,
	_ []*schema.Message,
	_ ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	return schema.StreamReaderFromArray([]*schema.Message{f.reply}), nil
}

func TestClassifierBindsSchemaForcesToolAndDecodesResult(t *testing.T) {
	fake := &fakeToolCallingModel{reply: toolReply(resultJSON(t, Result{
		Intent:     TrafficAnomaly,
		Confidence: 0.93,
		Summary:    "SN001 调度后没有流量",
		Evidence:   []string{"用户称业务不跑量"},
		DeviceIDs:  []string{" SN001 ", "SN001"},
	}))}
	classifier, err := New(fake)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if len(fake.tools) != 1 || fake.tools[0].Name != "report_intent" {
		t.Fatalf("bound tools = %#v, want only %q", fake.tools, "report_intent")
	}

	result, err := classifier.Classify(context.Background(), []*schema.Message{
		schema.UserMessage("SN001 调度后一直不跑量"),
	})
	if err != nil {
		t.Fatalf("Classify() error = %v", err)
	}
	if result.Intent != TrafficAnomaly || result.Confidence != 0.93 {
		t.Fatalf("result = %#v", result)
	}
	if len(result.DeviceIDs) != 1 || result.DeviceIDs[0] != "SN001" {
		t.Fatalf("DeviceIDs = %#v, want deduplicated SN001", result.DeviceIDs)
	}

	options := model.GetCommonOptions(nil, fake.options...)
	if options.ToolChoice == nil || *options.ToolChoice != schema.ToolChoiceForced {
		t.Fatalf("ToolChoice = %#v, want forced", options.ToolChoice)
	}
	if len(options.AllowedToolNames) != 1 || options.AllowedToolNames[0] != "report_intent" {
		t.Fatalf("AllowedToolNames = %#v", options.AllowedToolNames)
	}
	if options.MaxTokens == nil || *options.MaxTokens != 1024 {
		t.Fatalf("MaxTokens = %#v, want 1024", options.MaxTokens)
	}
}

func TestClassifierRejectsUnknownEnumValue(t *testing.T) {
	fake := &fakeToolCallingModel{reply: toolReply(resultJSON(t, Result{
		Intent:     Kind("database_failure"),
		Confidence: 0.9,
		Summary:    "数据库异常",
	}))}
	classifier, err := New(fake)
	if err != nil {
		t.Fatal(err)
	}
	_, err = classifier.Classify(context.Background(), []*schema.Message{schema.UserMessage("报错了")})
	if err == nil || !strings.Contains(err.Error(), "未知 intent") {
		t.Fatalf("Classify() error = %v, want unknown intent error", err)
	}
}

func TestClassifierForcesClarificationForLowConfidenceAndUnknown(t *testing.T) {
	tests := []Result{
		{Intent: PluginFailure, Confidence: 0.59, Summary: "疑似插件问题"},
		{Intent: Unknown, Confidence: 0.99, Summary: "无法识别"},
	}
	for _, input := range tests {
		fake := &fakeToolCallingModel{reply: toolReply(resultJSON(t, input))}
		classifier, err := New(fake)
		if err != nil {
			t.Fatal(err)
		}
		got, err := classifier.Classify(context.Background(), []*schema.Message{schema.UserMessage("有异常")})
		if err != nil {
			t.Fatalf("Classify(%#v) error = %v", input, err)
		}
		if !got.NeedsClarification {
			t.Fatalf("Classify(%#v).NeedsClarification = false", input)
		}
	}
}

func TestClassifierKeepsLatestImageAndLimitsRecentContext(t *testing.T) {
	messages := make([]*schema.Message, 0, 8)
	for i := 0; i < 7; i++ {
		if i%2 == 0 {
			messages = append(messages, schema.UserMessage(strings.Repeat("旧消息", 250)))
		} else {
			messages = append(messages, schema.AssistantMessage("已收到", nil))
		}
	}
	imageURL := "https://example.com/error.png"
	imageMessage := &schema.Message{
		Role: schema.User,
		UserInputMultiContent: []schema.MessageInputPart{
			{Type: schema.ChatMessagePartTypeText, Text: "看这个报错"},
			{
				Type: schema.ChatMessagePartTypeImageURL,
				Image: &schema.MessageInputImage{
					MessagePartCommon: schema.MessagePartCommon{URL: &imageURL},
					Detail:            schema.ImageURLDetailHigh,
				},
			},
		},
	}
	messages = append(messages, imageMessage)

	fake := &fakeToolCallingModel{reply: toolReply(resultJSON(t, Result{
		Intent: Other, Confidence: 0.9, Summary: "图片报错",
	}))}
	classifier, err := New(fake)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := classifier.Classify(context.Background(), messages); err != nil {
		t.Fatal(err)
	}
	got := fake.input
	const recentContextMessages = 6
	if len(got) != 2+recentContextMessages {
		t.Fatalf("分类输入消息数 = %d, want %d", len(got), 2+recentContextMessages)
	}
	if got[len(got)-1] != imageMessage {
		t.Fatal("latest multimodal message was copied or replaced")
	}
	if len(got[len(got)-1].UserInputMultiContent) != 2 {
		t.Fatalf("latest image parts = %#v", got[len(got)-1].UserInputMultiContent)
	}
	summaryParts := strings.SplitN(got[1].Content, "\n", 2)
	if len(summaryParts) != 2 {
		t.Fatalf("older summary message = %q", got[1].Content)
	}
	const olderSummaryRunes = 1200
	if runes := len([]rune(summaryParts[1])); runes > olderSummaryRunes {
		t.Fatalf("older summary has %d runes, want <= %d", runes, olderSummaryRunes)
	}
}

func TestClassifierAcceptsFencedJSONFallback(t *testing.T) {
	fence := string([]byte{96, 96, 96})
	fake := &fakeToolCallingModel{reply: schema.AssistantMessage(
		fence+"json\n"+resultJSON(t, Result{
			Intent:     NetworkConfigurationFailure,
			Confidence: 0.82,
			Summary:    "节点配网失败",
			Evidence:   []string{"截图显示路由配置错误"},
		})+"\n"+fence,
		nil,
	)}
	classifier, err := New(fake)
	if err != nil {
		t.Fatal(err)
	}
	result, err := classifier.Classify(context.Background(), []*schema.Message{
		schema.UserMessage("配网失败"),
	})
	if err != nil {
		t.Fatalf("Classify() error = %v", err)
	}
	if result.Intent != NetworkConfigurationFailure {
		t.Fatalf("Intent = %q", result.Intent)
	}
}

func TestRoutingContextMarksMetadataAndBlocksCommandsWhenClarifying(t *testing.T) {
	routing := (Result{
		Intent:             Unknown,
		Confidence:         0.3,
		Summary:            "信息不足",
		NeedsClarification: true,
	}).RoutingContext()
	for _, want := range []string{"不是用户指令", "不可信元数据", "禁止调用 run_tunnel_cmd"} {
		if !strings.Contains(routing, want) {
			t.Fatalf("RoutingContext() = %q, want %q", routing, want)
		}
	}
}

func TestCodeAndInstallationRoutingConstraints(t *testing.T) {
	code := (Result{Intent: CodeRepositoryQuestion, Confidence: 0.95, Summary: "查询调用方"}).RoutingContext()
	for _, want := range []string{"代码仓库问答", "禁止调用 Tunnel", "path:line"} {
		if !strings.Contains(code, want) {
			t.Errorf("code routing=%q, want %q", code, want)
		}
	}
	if CodeRepositoryQuestion.Label() != "代码仓库问答" || !CodeRepositoryQuestion.Valid() {
		t.Fatalf("code_repository_question label/valid 不正确")
	}

	installation := (Result{Intent: InstallationFailure, Confidence: 0.95, Summary: "截图装机失败"}).RoutingContext()
	for _, want := range []string{"优先", "源码", "设备 ID 不是"} {
		if !strings.Contains(installation, want) {
			t.Errorf("installation routing=%q, want %q", installation, want)
		}
	}
}

func TestClassifierPropagatesModelError(t *testing.T) {
	modelErr := errors.New("gateway unavailable")
	fake := &fakeToolCallingModel{generateErr: modelErr}
	classifier, err := New(fake)
	if err != nil {
		t.Fatal(err)
	}
	_, err = classifier.Classify(context.Background(), []*schema.Message{schema.UserMessage("查一下")})
	if !errors.Is(err, modelErr) {
		t.Fatalf("Classify() error = %v, want wrapped model error", err)
	}
}

func toolReply(arguments string) *schema.Message {
	return &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			ID: "intent-call",
			Function: schema.FunctionCall{
				Name:      "report_intent",
				Arguments: arguments,
			},
		}},
	}
}

func resultJSON(t *testing.T, result Result) string {
	t.Helper()
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
