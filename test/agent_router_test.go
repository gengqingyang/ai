package test

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/schema"

	projectagent "diagnostic-system/internal/agent"
	"diagnostic-system/internal/intent"
)

type stubIntentClassifier struct {
	result intent.Result
	err    error
	input  []*schema.Message
}

func (s *stubIntentClassifier) Classify(
	_ context.Context,
	messages []*schema.Message,
) (intent.Result, error) {
	s.input = messages
	return s.result, s.err
}

func TestDiagnosticRouterSelectsFaultFlow(t *testing.T) {
	tests := []struct {
		name   string
		result intent.Result
		want   projectagent.Flow
	}{
		{"installation", intent.Result{Intent: intent.InstallationFailure}, projectagent.FlowInstallation},
		{"traffic", intent.Result{Intent: intent.TrafficAnomaly}, projectagent.FlowTraffic},
		{"plugin", intent.Result{Intent: intent.PluginFailure}, projectagent.FlowPlugin},
		{"kernel", intent.Result{Intent: intent.KernelUpgradeFailure}, projectagent.FlowKernel},
		{"network", intent.Result{Intent: intent.NetworkConfigurationFailure}, projectagent.FlowNetwork},
		{"code", intent.Result{Intent: intent.CodeRepositoryQuestion}, projectagent.FlowCode},
		{"other", intent.Result{Intent: intent.Other}, projectagent.FlowOther},
		{"unknown", intent.Result{Intent: intent.Unknown}, projectagent.FlowClarification},
		{"explicit clarification", intent.Result{
			Intent: intent.InstallationFailure, NeedsClarification: true,
		}, projectagent.FlowClarification},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			classifier := &stubIntentClassifier{result: tt.result}
			router, err := projectagent.NewRouter(context.Background(), classifier)
			if err != nil {
				t.Fatal(err)
			}
			input := []*schema.Message{schema.UserMessage("诊断一下")}
			got, err := router.Select(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			if got.Flow != tt.want || got.Classification.Intent != tt.result.Intent {
				t.Fatalf("route=%#v, want flow=%q intent=%q", got, tt.want, tt.result.Intent)
			}
			if len(classifier.input) != 1 || classifier.input[0] != input[0] {
				t.Fatalf("classifier input=%#v", classifier.input)
			}
		})
	}
}

func TestDiagnosticRouterPropagatesClassifierError(t *testing.T) {
	want := errors.New("classifier unavailable")
	router, err := projectagent.NewRouter(context.Background(), &stubIntentClassifier{err: want})
	if err != nil {
		t.Fatal(err)
	}
	_, err = router.Select(context.Background(), []*schema.Message{schema.UserMessage("查一下")})
	if !errors.Is(err, want) {
		t.Fatalf("Select() error=%v, want wrapped %v", err, want)
	}
}
