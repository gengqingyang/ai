package test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"

	projecttools "diagnostic-system/internal/tools"
)

type evidenceCall struct {
	node    string
	command string
}

type fakeEvidenceRunner struct {
	err            error
	stdoutOverride string
	calls          []evidenceCall
}

func (f *fakeEvidenceRunner) RunReadOnly(
	_ context.Context,
	node string,
	command string,
) (projecttools.ReadOnlyCommandOutput, error) {
	f.calls = append(f.calls, evidenceCall{node: node, command: command})
	if f.err != nil {
		return projecttools.ReadOnlyCommandOutput{}, f.err
	}
	if f.stdoutOverride != "" {
		return projecttools.ReadOnlyCommandOutput{Stdout: f.stdoutOverride}, nil
	}
	var stdout string
	switch {
	case strings.Contains(command, "evidence:installation"):
		stdout = strings.Join([]string{
			"kv\tcollected_at\t2026-08-11T12:00:00+08:00",
			"kv\tos\tCentOS Linux 7 (Core)",
			"kv\tsystem_state\trunning",
			"kv\tfailed_services\t0",
			"kv\tuptime_seconds\t120000",
			"kv\tmemory_available_mb\t1024",
			"mount\t/\t20",
			"mount\t/boot\t80",
			"mount\t/xyapp\t10",
			"process\tactivation\trunning",
			"process\tagent\trunning",
			"process\tinstallIPK\trunning",
		}, "\n")
	case strings.Contains(command, "evidence:traffic"):
		stdout = strings.Join([]string{
			"kv\tcollected_at\t2026-08-11T12:00:00+08:00",
			"kv\tinterface\teth0",
			"kv\tsample_seconds\t2",
			"counter0\trx_bytes\t1000",
			"counter0\ttx_bytes\t2000",
			"counter0\trx_packets\t100",
			"counter0\ttx_packets\t200",
			"counter0\trx_errors\t0",
			"counter0\ttx_errors\t0",
			"counter0\trx_dropped\t1",
			"counter0\ttx_dropped\t0",
			"counter1\trx_bytes\t5000",
			"counter1\ttx_bytes\t8000",
			"counter1\trx_packets\t300",
			"counter1\ttx_packets\t600",
			"counter1\trx_errors\t0",
			"counter1\ttx_errors\t0",
			"counter1\trx_dropped\t1",
			"counter1\ttx_dropped\t0",
		}, "\n")
	case strings.Contains(command, "evidence:plugin"):
		stdout = strings.Join([]string{
			"kv\tcollected_at\t2026-08-11T12:00:00+08:00",
			"component\tnextipk.service\tactive\trunning\t0\t/etc/systemd/system/nextipk.service\t/xyapp/system/miner.plugin-nextipk.ipk/bin/nextipk",
			"component\tvirtualrouter.service\tactive\trunning\t0\t/etc/systemd/system/virtualrouter.service\t/xyapp/system/miner.plugin-virtualrouter.ipk/bin/virtualrouter",
		}, "\n")
	case strings.Contains(command, "evidence:kernel"):
		stdout = strings.Join([]string{
			"kv\tcollected_at\t2026-08-11T12:00:00+08:00",
			"kv\tcurrent\t5.10.25-1.8.23",
			"kv\tdefault\t5.10.25-1.8.23",
			"kv\tcurrent_installed\tyes",
			"installed\t5.10.25-1.8.23",
		}, "\n")
	case strings.Contains(command, "evidence:network"):
		stdout = strings.Join([]string{
			"kv\tcollected_at\t2026-08-11T12:00:00+08:00",
			"interface\tlo\tup",
			"interface\teth0\tup",
			"address\teth0\t192.168.83.180/24",
			"default_route\teth0\t192.168.83.2",
			"dns\t192.168.83.2",
			"counter\trx_errors\t0",
			"counter\trx_dropped\t0",
			"counter\ttx_errors\t0",
			"counter\ttx_dropped\t0",
		}, "\n")
	default:
		return projecttools.ReadOnlyCommandOutput{}, errors.New("unexpected evidence command")
	}
	return projecttools.ReadOnlyCommandOutput{Stdout: stdout}, nil
}

func TestEvidenceToolsProduceStructuredReports(t *testing.T) {
	runner := &fakeEvidenceRunner{}
	tools, err := projecttools.NewEvidenceTools(runner)
	if err != nil {
		t.Fatalf("NewEvidenceTools() error = %v", err)
	}
	if len(tools) != 5 {
		t.Fatalf("len(tools) = %d, want 5", len(tools))
	}

	wantDomains := map[string]string{
		projecttools.ToolInstallationEvidence: "installation",
		projecttools.ToolTrafficEvidence:      "traffic",
		projecttools.ToolPluginEvidence:       "plugin",
		projecttools.ToolKernelEvidence:       "kernel",
		projecttools.ToolNetworkEvidence:      "network",
	}
	for _, evidenceTool := range tools {
		info, infoErr := evidenceTool.Info(context.Background())
		if infoErr != nil {
			t.Fatalf("Info() error = %v", infoErr)
		}
		invokable, ok := evidenceTool.(tool.InvokableTool)
		if !ok {
			t.Fatalf("tool %s type = %T, want InvokableTool", info.Name, evidenceTool)
		}
		input := `{"node":"XRVD73841ED3160A"}`
		if info.Name == projecttools.ToolTrafficEvidence {
			input = `{"node":"XRVD73841ED3160A","sample_seconds":2}`
		}
		output, runErr := invokable.InvokableRun(context.Background(), input)
		if runErr != nil {
			t.Fatalf("%s InvokableRun() error = %v", info.Name, runErr)
		}
		var report projecttools.EvidenceReport
		if err := json.Unmarshal([]byte(output), &report); err != nil {
			t.Fatalf("%s output is not EvidenceReport: %v\n%s", info.Name, err, output)
		}
		if report.Domain != wantDomains[info.Name] || report.Node != "XRVD73841ED3160A" ||
			report.DataSource == "" || report.CollectedAt == "" || len(report.Items) == 0 {
			t.Errorf("%s report = %#v", info.Name, report)
		}
	}

	if len(runner.calls) != 5 {
		t.Fatalf("runner calls = %d, want 5", len(runner.calls))
	}
	for _, call := range runner.calls {
		if call.node != "XRVD73841ED3160A" || strings.Contains(call.command, call.node) {
			t.Errorf("runner call 没有隔离 node 和固定命令: %#v", call)
		}
	}
}

func TestRegisterBuiltinReadOnlyEvidenceTools(t *testing.T) {
	runner := &fakeEvidenceRunner{}
	evidenceTools, err := projecttools.NewEvidenceTools(runner)
	if err != nil {
		t.Fatal(err)
	}
	registry := projecttools.NewRegistry()
	for _, evidenceTool := range evidenceTools {
		if err := registry.Register(context.Background(), evidenceTool, projecttools.RiskReadOnly); err != nil {
			t.Fatalf("Register() error = %v", err)
		}
		info, err := evidenceTool.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		schema, err := info.ToJSONSchema()
		if err != nil {
			t.Fatalf("%s schema error = %v", info.Name, err)
		}
		if schema == nil {
			t.Errorf("%s 没有参数 schema", info.Name)
			continue
		}
		if !containsString(schema.Required, "node") {
			t.Errorf("%s required = %v, want node", info.Name, schema.Required)
		}
		schemaJSON, err := json.Marshal(schema)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(schemaJSON), `"node"`) {
			t.Errorf("%s schema 缺少 node: %s", info.Name, schemaJSON)
		}
	}
	if len(registry.ReadOnly()) != 5 || len(registry.Mutating()) != 0 {
		t.Fatalf("registry read-only=%d mutating=%d", len(registry.ReadOnly()), len(registry.Mutating()))
	}
	names, err := evidenceToolNames(evidenceTools)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		projecttools.ToolInstallationEvidence,
		projecttools.ToolTrafficEvidence,
		projecttools.ToolPluginEvidence,
		projecttools.ToolKernelEvidence,
		projecttools.ToolNetworkEvidence,
	} {
		if !containsString(names, want) {
			t.Errorf("registered names = %v, missing %s", names, want)
		}
	}
}

func TestTrafficEvidenceCalculatesPerSecondRate(t *testing.T) {
	runner := &fakeEvidenceRunner{}
	traffic := evidenceToolByName(t, runner, projecttools.ToolTrafficEvidence)
	output, err := traffic.InvokableRun(context.Background(), `{"node":"XRVD73841ED3160A","sample_seconds":2}`)
	if err != nil {
		t.Fatal(err)
	}
	var report projecttools.EvidenceReport
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatal(err)
	}
	item, ok := evidenceItem(report, "rx_bytes_per_second")
	if !ok || item.Value != "2000" || item.Unit != "B/s" {
		t.Fatalf("rx rate = %#v, found=%v", item, ok)
	}
	if len(runner.calls) != 1 || !strings.Contains(runner.calls[0].command, "seconds=2") {
		t.Fatalf("traffic command = %#v", runner.calls)
	}
}

func TestEvidenceToolsRejectUnsafeInputsBeforeRunner(t *testing.T) {
	runner := &fakeEvidenceRunner{}
	installation := evidenceToolByName(t, runner, projecttools.ToolInstallationEvidence)
	if _, err := installation.InvokableRun(context.Background(), `{"node":"SN001; reboot"}`); err == nil {
		t.Fatal("非法 node 未被拒绝")
	}
	traffic := evidenceToolByName(t, runner, projecttools.ToolTrafficEvidence)
	if _, err := traffic.InvokableRun(context.Background(), `{"node":"SN001","sample_seconds":6}`); err == nil {
		t.Fatal("非法 sample_seconds 未被拒绝")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("非法输入仍调用了 runner: %#v", runner.calls)
	}
}

func TestEvidenceRunnerErrorIsReturnedWithContext(t *testing.T) {
	runner := &fakeEvidenceRunner{err: errors.New("tunnel unavailable")}
	installation := evidenceToolByName(t, runner, projecttools.ToolInstallationEvidence)
	_, err := installation.InvokableRun(context.Background(), `{"node":"SN001"}`)
	if err == nil || !strings.Contains(err.Error(), "SN001") || !strings.Contains(err.Error(), "tunnel unavailable") {
		t.Fatalf("InvokableRun() error = %v", err)
	}
}

func TestEvidenceToolsNormalizeGNUDateInsTimestamp(t *testing.T) {
	runner := &fakeEvidenceRunner{stdoutOverride: strings.Join([]string{
		"kv\tcollected_at\t2026-08-12T20:53:42,416118102+0800",
		"kv\tsystem_state\trunning",
	}, "\n")}
	installation := evidenceToolByName(t, runner, projecttools.ToolInstallationEvidence)
	output, err := installation.InvokableRun(context.Background(), `{"node":"SN001"}`)
	if err != nil {
		t.Fatal(err)
	}
	var report projecttools.EvidenceReport
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatal(err)
	}
	const want = "2026-08-12T20:53:42.416118102+08:00"
	if report.CollectedAt != want {
		t.Fatalf("CollectedAt=%q, want %q", report.CollectedAt, want)
	}
}

func TestEvidenceToolsRejectInvalidCollectedAt(t *testing.T) {
	runner := &fakeEvidenceRunner{stdoutOverride: strings.Join([]string{
		"kv\tcollected_at\tnot-a-time",
		"kv\tsystem_state\trunning",
	}, "\n")}
	installation := evidenceToolByName(t, runner, projecttools.ToolInstallationEvidence)
	_, err := installation.InvokableRun(context.Background(), `{"node":"SN001"}`)
	if err == nil || !strings.Contains(err.Error(), "SN001") ||
		!strings.Contains(err.Error(), "采集时间") || !strings.Contains(err.Error(), "not-a-time") {
		t.Fatalf("InvokableRun() error=%v", err)
	}
}

func TestTrafficEvidenceDoesNotTreatMissingCounterAsZero(t *testing.T) {
	runner := &fakeEvidenceRunner{stdoutOverride: strings.Join([]string{
		"kv\tcollected_at\t2026-08-11T12:00:00+08:00",
		"kv\tinterface\teth0",
		"kv\tsample_seconds\t1",
		"counter0\trx_bytes\tmissing",
	}, "\n")}
	traffic := evidenceToolByName(t, runner, projecttools.ToolTrafficEvidence)
	_, err := traffic.InvokableRun(context.Background(), `{"node":"SN001"}`)
	if err == nil || !strings.Contains(err.Error(), "计数器") || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("InvokableRun() error = %v", err)
	}
}

func TestNetworkEvidenceMarksMissingCounterUnavailable(t *testing.T) {
	runner := &fakeEvidenceRunner{stdoutOverride: strings.Join([]string{
		"kv\tcollected_at\t2026-08-11T12:00:00+08:00",
		"interface\teth0\tup",
		"address\teth0\t192.0.2.10/24",
		"default_route\teth0\t192.0.2.1",
		"dns\t192.0.2.53",
		"counter\trx_errors\tmissing",
	}, "\n")}
	network := evidenceToolByName(t, runner, projecttools.ToolNetworkEvidence)
	output, err := network.InvokableRun(context.Background(), `{"node":"SN001"}`)
	if err != nil {
		t.Fatal(err)
	}
	var report projecttools.EvidenceReport
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatal(err)
	}
	item, ok := evidenceItem(report, "rx_errors")
	if report.Status != "warning" || !ok || item.Status != "warning" || item.Value != "unavailable" {
		t.Fatalf("report=%#v item=%#v found=%v", report, item, ok)
	}
}

func evidenceToolByName(
	t *testing.T,
	runner projecttools.ReadOnlyCommandRunner,
	name string,
) tool.InvokableTool {
	t.Helper()
	tools, err := projecttools.NewEvidenceTools(runner)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range tools {
		info, infoErr := candidate.Info(context.Background())
		if infoErr != nil {
			t.Fatal(infoErr)
		}
		if info.Name == name {
			invokable, ok := candidate.(tool.InvokableTool)
			if !ok {
				t.Fatalf("tool %s is %T", name, candidate)
			}
			return invokable
		}
	}
	t.Fatalf("tool %s not found", name)
	return nil
}

func evidenceItem(report projecttools.EvidenceReport, name string) (projecttools.EvidenceItem, bool) {
	for _, item := range report.Items {
		if item.Name == name {
			return item, true
		}
	}
	return projecttools.EvidenceItem{}, false
}

func evidenceToolNames(tools []tool.BaseTool) ([]string, error) {
	names := make([]string, 0, len(tools))
	for _, evidenceTool := range tools {
		info, err := evidenceTool.Info(context.Background())
		if err != nil {
			return nil, fmt.Errorf("tool info: %w", err)
		}
		names = append(names, info.Name)
	}
	return names, nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
