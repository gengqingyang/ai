package test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	projectagent "diagnostic-system/internal/agent"
	"diagnostic-system/internal/approval"
	"diagnostic-system/internal/intent"
	projecttools "diagnostic-system/internal/tools"
)

type evidenceToolStub struct {
	name   string
	domain string
	source string
	run    func(context.Context, string) (string, error)
	calls  atomic.Int32
}

func (s *evidenceToolStub) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: s.name, Desc: "测试采证工具"}, nil
}

func (s *evidenceToolStub) InvokableRun(
	ctx context.Context,
	arguments string,
	_ ...tool.Option,
) (string, error) {
	s.calls.Add(1)
	if s.run != nil {
		return s.run(ctx, arguments)
	}
	return evidenceReportJSON(testedNode(arguments), s.domain, s.source), nil
}

func TestEvidencePipelineCollectsDevicesConcurrentlyInStableOrder(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	traffic := &evidenceToolStub{
		name:   projecttools.ToolTrafficEvidence,
		domain: string(projecttools.DomainTraffic),
		source: projecttools.TrafficEvidenceSource,
		run: func(ctx context.Context, arguments string) (string, error) {
			node := testedNode(arguments)
			started <- node
			select {
			case <-release:
				return evidenceReportJSON(node, string(projecttools.DomainTraffic), projecttools.TrafficEvidenceSource), nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		},
	}
	pipeline := newTestEvidencePipeline(t, traffic)

	type result struct {
		bundle projectagent.EvidenceBundle
		err    error
	}
	done := make(chan result, 1)
	go func() {
		bundle, err := pipeline.Run(context.Background(), projectagent.EvidenceRequest{
			Flow: projectagent.FlowTraffic,
			Classification: intent.Result{
				Intent: intent.TrafficAnomaly, DeviceIDs: []string{"SN002", "SN001"},
			},
		})
		done <- result{bundle: bundle, err: err}
	}()

	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("第二台设备未在第一台释放前开始采证，采证没有并发执行")
		}
	}
	close(release)
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	if len(got.bundle.Gaps) != 0 || len(got.bundle.Reports) != 2 {
		t.Fatalf("bundle=%#v", got.bundle)
	}
	if got.bundle.Reports[0].Node != "SN002" || got.bundle.Reports[1].Node != "SN001" {
		t.Fatalf("报告顺序=%q, %q，未保持分类器设备顺序",
			got.bundle.Reports[0].Node, got.bundle.Reports[1].Node)
	}
}

func TestEvidencePipelineIsolatesToolAndValidationFailures(t *testing.T) {
	traffic := &evidenceToolStub{
		name:   projecttools.ToolTrafficEvidence,
		domain: string(projecttools.DomainTraffic),
		source: projecttools.TrafficEvidenceSource,
		run: func(_ context.Context, arguments string) (string, error) {
			node := testedNode(arguments)
			switch node {
			case "SN-ERROR":
				return "", errors.New("数据源超时")
			case "SN-WRONG-DOMAIN":
				return evidenceReportJSON(node, "network", projecttools.TrafficEvidenceSource), nil
			case "SN-NO-TIME":
				var report projecttools.EvidenceReport
				if err := json.Unmarshal([]byte(evidenceReportJSON(node, "traffic", projecttools.TrafficEvidenceSource)), &report); err != nil {
					return "", err
				}
				report.CollectedAt = "not-a-time"
				encoded, err := json.Marshal(report)
				return string(encoded), err
			default:
				return evidenceReportJSON(node, "traffic", projecttools.TrafficEvidenceSource), nil
			}
		},
	}
	pipeline := newTestEvidencePipeline(t, traffic)
	bundle, err := pipeline.Run(context.Background(), projectagent.EvidenceRequest{
		Flow: projectagent.FlowTraffic,
		Classification: intent.Result{DeviceIDs: []string{
			"SN-OK", "SN-ERROR", "SN-WRONG-DOMAIN", "SN-NO-TIME", "SN-LIMIT",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Reports) != 1 || bundle.Reports[0].Node != "SN-OK" {
		t.Fatalf("有效 reports=%#v, want only SN-OK", bundle.Reports)
	}
	if len(bundle.Gaps) != 4 {
		t.Fatalf("gaps=%#v, want tool/domain/time/limit four gaps", bundle.Gaps)
	}
	wants := []string{"数据源超时", "证据域", "不是合法 RFC3339", "本轮最多采集 4 台设备"}
	for _, want := range wants {
		found := false
		for _, gap := range bundle.Gaps {
			if contains(gap.Reason, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("gaps=%#v, missing %q", bundle.Gaps, want)
		}
	}
}

func TestEvidencePipelineRejectsUnexpectedDataSource(t *testing.T) {
	traffic := &evidenceToolStub{
		name: projecttools.ToolTrafficEvidence, domain: "traffic",
		source: projecttools.TrafficEvidenceSource,
		run: func(_ context.Context, arguments string) (string, error) {
			return evidenceReportJSON(testedNode(arguments), "traffic", "untrusted:source"), nil
		},
	}
	pipeline := newTestEvidencePipeline(t, traffic)
	bundle, err := pipeline.Run(context.Background(), projectagent.EvidenceRequest{
		Flow:           projectagent.FlowTraffic,
		Classification: intent.Result{DeviceIDs: []string{"SN001"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Reports) != 0 || len(bundle.Gaps) != 1 ||
		!strings.Contains(bundle.Gaps[0].Reason, "证据来源") {
		t.Fatalf("bundle=%#v", bundle)
	}
}

func TestEvidencePipelineSkipsUnsupportedFlowAndRecordsMissingDevice(t *testing.T) {
	traffic := &evidenceToolStub{
		name: projecttools.ToolTrafficEvidence, domain: "traffic", source: projecttools.TrafficEvidenceSource,
	}
	pipeline := newTestEvidencePipeline(t, traffic)

	codeBundle, err := pipeline.Run(context.Background(), projectagent.EvidenceRequest{
		Flow: projectagent.FlowCode,
	})
	if err != nil || len(codeBundle.Reports) != 0 || len(codeBundle.Gaps) != 0 {
		t.Fatalf("code bundle=%#v err=%v", codeBundle, err)
	}
	missing, err := pipeline.Run(context.Background(), projectagent.EvidenceRequest{
		Flow: projectagent.FlowTraffic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if traffic.calls.Load() != 0 || len(missing.Gaps) != 1 {
		t.Fatalf("calls=%d bundle=%#v", traffic.calls.Load(), missing)
	}
}

func TestEvidencePipelineRejectsWrongRiskOrDomainAtStartup(t *testing.T) {
	tests := []struct {
		name   string
		risk   projecttools.Risk
		domain projecttools.Domain
		want   string
	}{
		{"mutating evidence", projecttools.RiskMutating, projecttools.DomainTraffic, "必须注册为只读"},
		{"wrong domain", projecttools.RiskReadOnly, projecttools.DomainNetwork, "未注册到业务域"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := projecttools.NewRegistry()
			for _, spec := range evidenceToolSpecs() {
				risk, domain := projecttools.RiskReadOnly, spec.domain
				if spec.name == projecttools.ToolTrafficEvidence {
					risk, domain = tt.risk, tt.domain
				}
				stub := &evidenceToolStub{
					name: spec.name, domain: string(spec.domain), source: spec.source,
				}
				var candidate tool.BaseTool = stub
				if risk == projecttools.RiskMutating {
					gate := projecttools.NewGate(approval.NewStore())
					gated, err := gate.Wrap(context.Background(), stub)
					if err != nil {
						t.Fatal(err)
					}
					candidate = gated
				}
				if err := registry.RegisterInDomains(
					context.Background(), candidate, risk, domain,
				); err != nil {
					t.Fatal(err)
				}
			}
			_, err := projectagent.NewEvidencePipeline(context.Background(), registry)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("NewEvidencePipeline() error=%v, want %q", err, tt.want)
			}
		})
	}
}

func newTestEvidencePipeline(t *testing.T, replacement *evidenceToolStub) *projectagent.EvidencePipeline {
	t.Helper()
	registry := projecttools.NewRegistry()
	for _, spec := range evidenceToolSpecs() {
		stub := &evidenceToolStub{name: spec.name, domain: string(spec.domain), source: spec.source}
		if replacement != nil && replacement.name == spec.name {
			stub = replacement
		}
		if err := registry.RegisterInDomains(
			context.Background(), stub, projecttools.RiskReadOnly, spec.domain,
		); err != nil {
			t.Fatal(err)
		}
	}
	pipeline, err := projectagent.NewEvidencePipeline(context.Background(), registry)
	if err != nil {
		t.Fatal(err)
	}
	return pipeline
}

func evidenceToolSpecs() []struct {
	name   string
	domain projecttools.Domain
	source string
} {
	return []struct {
		name   string
		domain projecttools.Domain
		source string
	}{
		{projecttools.ToolInstallationEvidence, projecttools.DomainInstallation, projecttools.InstallationEvidenceSource},
		{projecttools.ToolTrafficEvidence, projecttools.DomainTraffic, projecttools.TrafficEvidenceSource},
		{projecttools.ToolPluginEvidence, projecttools.DomainPlugin, projecttools.PluginEvidenceSource},
		{projecttools.ToolKernelEvidence, projecttools.DomainKernel, projecttools.KernelEvidenceSource},
		{projecttools.ToolNetworkEvidence, projecttools.DomainNetwork, projecttools.NetworkEvidenceSource},
	}
}

func testedNode(arguments string) string {
	var input struct {
		Node string `json:"node"`
	}
	_ = json.Unmarshal([]byte(arguments), &input)
	return input.Node
}

func evidenceReportJSON(node, domain, source string) string {
	report := projecttools.EvidenceReport{
		Domain: domain, Node: node, DataSource: source,
		CollectedAt: "2026-08-12T12:00:00+08:00", Status: "healthy",
		Summary: "采集 1 项证据，0 项需要关注",
		Items:   []projecttools.EvidenceItem{{Name: "state", Status: "ok", Value: "ready"}},
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		panic(fmt.Sprintf("marshal evidence fixture: %v", err))
	}
	return string(encoded)
}

func contains(text, fragment string) bool { return strings.Contains(text, fragment) }
