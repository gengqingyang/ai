package tools

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	evidenceOK      = "ok"
	evidenceInfo    = "info"
	evidenceWarning = "warning"
)

func parseInstallationEvidence(output string, report *EvidenceReport) error {
	for _, fields := range evidenceRecords(output) {
		switch fields[0] {
		case "kv":
			if len(fields) < 3 {
				continue
			}
			name, value := fields[1], fields[2]
			if name == "collected_at" {
				report.CollectedAt = value
				continue
			}
			status, unit := evidenceInfo, ""
			switch name {
			case "system_state":
				status = statusWhen(value == "running", evidenceOK)
			case "failed_services":
				status = statusWhen(value == "0", evidenceOK)
			case "memory_available_mb":
				unit = "MiB"
				available, _ := strconv.ParseInt(value, 10, 64)
				status = statusWhen(available >= 512, evidenceOK)
			case "uptime_seconds":
				unit = "s"
			}
			report.Items = append(report.Items, EvidenceItem{Name: name, Status: status, Value: value, Unit: unit})
		case "mount":
			if len(fields) < 3 {
				continue
			}
			used, _ := strconv.Atoi(fields[2])
			report.Items = append(report.Items, EvidenceItem{
				Name: "filesystem_usage", Status: statusWhen(used < 85, evidenceOK),
				Value: fields[2], Unit: "%", Detail: fields[1],
			})
		case "process":
			if len(fields) < 3 {
				continue
			}
			report.Items = append(report.Items, EvidenceItem{
				Name: fields[1], Status: statusWhen(fields[2] == "running", evidenceOK), Value: fields[2],
			})
		}
	}
	return requireEvidence(report)
}

func parseTrafficEvidence(output string, report *EvidenceReport) error {
	before := make(map[string]uint64)
	after := make(map[string]uint64)
	iface := ""
	sampleSeconds := uint64(1)
	for _, fields := range evidenceRecords(output) {
		if len(fields) < 3 {
			continue
		}
		switch fields[0] {
		case "kv":
			if fields[1] == "collected_at" {
				report.CollectedAt = fields[2]
			} else if fields[1] == "interface" {
				iface = fields[2]
			} else if fields[1] == "sample_seconds" {
				parsed, err := parseEvidenceCounter(fields[2])
				if err != nil || parsed == 0 {
					return fmt.Errorf("非法采样秒数 %q", fields[2])
				}
				sampleSeconds = parsed
			}
		case "counter0":
			value, err := parseEvidenceCounter(fields[2])
			if err != nil {
				return fmt.Errorf("采样前计数器 %s 不可用: %w", fields[1], err)
			}
			before[fields[1]] = value
		case "counter1":
			value, err := parseEvidenceCounter(fields[2])
			if err != nil {
				return fmt.Errorf("采样后计数器 %s 不可用: %w", fields[1], err)
			}
			after[fields[1]] = value
		}
	}
	if iface == "" {
		return errors.New("节点没有默认路由接口")
	}
	report.Items = append(report.Items, EvidenceItem{Name: "interface", Status: evidenceInfo, Value: iface})
	rxRate := counterDelta(before["rx_bytes"], after["rx_bytes"]) / sampleSeconds
	txRate := counterDelta(before["tx_bytes"], after["tx_bytes"]) / sampleSeconds
	report.Items = append(report.Items, EvidenceItem{
		Name: "traffic_activity", Status: statusWhen(rxRate+txRate > 0, evidenceOK),
		Value: strconv.FormatUint(rxRate+txRate, 10), Unit: "B/s",
	})
	metrics := []struct {
		name string
		unit string
	}{
		{"rx_bytes", "B/s"}, {"tx_bytes", "B/s"}, {"rx_packets", "packet/s"}, {"tx_packets", "packet/s"},
		{"rx_errors", "count"}, {"tx_errors", "count"}, {"rx_dropped", "count"}, {"tx_dropped", "count"},
	}
	for _, metric := range metrics {
		if _, ok := before[metric.name]; !ok {
			return fmt.Errorf("采样前缺少计数器 %s", metric.name)
		}
		if _, ok := after[metric.name]; !ok {
			return fmt.Errorf("采样后缺少计数器 %s", metric.name)
		}
		delta := counterDelta(before[metric.name], after[metric.name])
		value := delta
		name := metric.name + "_delta"
		status := evidenceInfo
		if strings.Contains(metric.name, "errors") || strings.Contains(metric.name, "dropped") {
			status = statusWhen(delta == 0, evidenceOK)
		} else {
			value = delta / sampleSeconds
			name = metric.name + "_per_second"
		}
		report.Items = append(report.Items, EvidenceItem{
			Name: name, Status: status, Value: strconv.FormatUint(value, 10), Unit: metric.unit,
		})
	}
	return requireEvidence(report)
}

func parsePluginEvidence(output string, report *EvidenceReport) error {
	for _, fields := range evidenceRecords(output) {
		if fields[0] == "kv" && len(fields) >= 3 && fields[1] == "collected_at" {
			report.CollectedAt = fields[2]
			continue
		}
		if fields[0] != "component" || len(fields) < 7 {
			continue
		}
		healthy := fields[2] == "active" && fields[3] == "running" && fields[4] == "0" && fields[6] != "not-running"
		report.Items = append(report.Items, EvidenceItem{
			Name: fields[1], Status: statusWhen(healthy, evidenceOK), Value: fields[2] + "/" + fields[3],
			Detail: fmt.Sprintf("exit=%s unit=%s executable=%s", fields[4], fields[5], fields[6]),
		})
	}
	return requireEvidence(report)
}

func parseKernelEvidence(output string, report *EvidenceReport) error {
	current, defaultKernel, currentInstalled := "", "", ""
	var installed []string
	for _, fields := range evidenceRecords(output) {
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "kv":
			if len(fields) < 3 {
				continue
			}
			switch fields[1] {
			case "collected_at":
				report.CollectedAt = fields[2]
			case "current":
				current = fields[2]
			case "default":
				defaultKernel = filepath.Base(fields[2])
			case "current_installed":
				currentInstalled = fields[2]
			}
		case "installed":
			installed = append(installed, fields[1])
		}
	}
	if current == "" {
		return errors.New("未获取到当前内核")
	}
	report.Items = append(report.Items,
		EvidenceItem{Name: "current_kernel", Status: evidenceInfo, Value: current},
		EvidenceItem{Name: "default_kernel", Status: statusWhen(defaultKernel == current, evidenceOK), Value: defaultKernel},
		EvidenceItem{Name: "current_kernel_installed", Status: statusWhen(currentInstalled == "yes", evidenceOK), Value: currentInstalled},
		EvidenceItem{Name: "installed_kernels", Status: evidenceInfo, Value: strings.Join(installed, ",")},
	)
	return nil
}

func parseNetworkEvidence(output string, report *EvidenceReport) error {
	defaultInterface := ""
	interfaceStates := make(map[string]string)
	dnsCount := 0
	for _, fields := range evidenceRecords(output) {
		switch fields[0] {
		case "kv":
			if len(fields) >= 3 && fields[1] == "collected_at" {
				report.CollectedAt = fields[2]
			}
		case "interface":
			if len(fields) >= 3 {
				interfaceStates[fields[1]] = fields[2]
				report.Items = append(report.Items, EvidenceItem{
					Name: "interface", Status: evidenceInfo,
					Value: fields[2], Detail: fields[1],
				})
			}
		case "address":
			if len(fields) >= 3 {
				report.Items = append(report.Items, EvidenceItem{Name: "ipv4_address", Status: evidenceInfo, Value: fields[2], Detail: fields[1]})
			}
		case "default_route":
			if len(fields) >= 3 {
				defaultInterface = fields[1]
				report.Items = append(report.Items, EvidenceItem{Name: "default_route", Status: evidenceOK, Value: fields[2], Detail: fields[1]})
			}
		case "dns":
			if len(fields) >= 2 {
				dnsCount++
				report.Items = append(report.Items, EvidenceItem{Name: "dns", Status: evidenceOK, Value: fields[1]})
			}
		case "counter":
			if len(fields) >= 3 {
				value, err := parseEvidenceCounter(fields[2])
				status, rendered := evidenceInfo, fields[2]
				if err != nil {
					status = evidenceWarning
					rendered = "unavailable"
				} else {
					rendered = strconv.FormatUint(value, 10)
				}
				report.Items = append(report.Items, EvidenceItem{
					Name: fields[1], Status: status, Value: rendered, Unit: "count",
					Detail: defaultInterface,
				})
			}
		}
	}
	if defaultInterface == "" {
		report.Items = append(report.Items, EvidenceItem{Name: "default_route", Status: evidenceWarning, Value: "missing"})
	} else {
		report.Items = append(report.Items, EvidenceItem{
			Name: "default_interface", Status: statusWhen(interfaceStates[defaultInterface] == "up", evidenceOK),
			Value: interfaceStates[defaultInterface], Detail: defaultInterface,
		})
	}
	if dnsCount == 0 {
		report.Items = append(report.Items, EvidenceItem{Name: "dns", Status: evidenceWarning, Value: "missing"})
	}
	return requireEvidence(report)
}

func evidenceRecords(output string) [][]string {
	lines := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")
	records := make([][]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		records = append(records, strings.Split(line, "\t"))
	}
	return records
}

func requireEvidence(report *EvidenceReport) error {
	if len(report.Items) == 0 {
		return errors.New("节点返回中没有可识别的证据")
	}
	return nil
}

func finalizeEvidence(report *EvidenceReport) {
	warnings := 0
	for _, item := range report.Items {
		if item.Status == evidenceWarning {
			warnings++
		}
	}
	report.Status = "healthy"
	if warnings > 0 {
		report.Status = "warning"
	}
	report.Summary = fmt.Sprintf("采集 %d 项证据，%d 项需要关注", len(report.Items), warnings)
}

func statusWhen(ok bool, success string) string {
	if ok {
		return success
	}
	return evidenceWarning
}

func parseEvidenceCounter(value string) (uint64, error) {
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("计数器值 %q 无效", value)
	}
	return parsed, nil
}

func counterDelta(before, after uint64) uint64 {
	if after < before {
		return 0
	}
	return after - before
}
