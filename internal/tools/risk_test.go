package tools

import "testing"

func TestAssessCommand(t *testing.T) {
	tests := []struct {
		cmd  string
		want RiskLevel
		why  string
	}{
		// 只读：查时间、看状态、翻日志
		{"date", LevelReadOnly, "查时间"},
		{"  date  ", LevelReadOnly, "前后空格不影响"},
		{"uptime", LevelReadOnly, ""},
		{"df -h", LevelReadOnly, ""},
		{"cat /etc/hostname", LevelReadOnly, ""},
		{"ps aux", LevelReadOnly, ""},
		{"/usr/bin/date", LevelReadOnly, "全路径按 basename 判断"},
		{"sudo date", LevelReadOnly, "sudo 前缀剥掉后仍是只读"},
		{"LANG=C date", LevelReadOnly, "环境变量前缀剥掉后仍是只读"},
		{"systemctl status nginx", LevelReadOnly, "systemctl 只读子命令"},
		{"systemctl is-active nginx", LevelReadOnly, ""},
		{"ip addr", LevelReadOnly, "ip 只读子命令"},
		{"journalctl -u nginx -n 100", LevelReadOnly, ""},
		{"ps aux | grep nginx", LevelReadOnly, "管道两端都是只读"},
		{"df -h && free -m", LevelReadOnly, "串联的两条都是只读"},
		{"sed -n '1,10p' /var/log/messages", LevelReadOnly, "sed 不带 -i 是只读"},
		{"find /data -name '*.log'", LevelReadOnly, "find 不带 -delete/-exec"},
		{"top -bn1", LevelReadOnly, "top 批处理模式"},

		// 危险：删、改、重启、装卸
		{"rm -rf /data/cache", LevelDangerous, "删除"},
		{"reboot", LevelDangerous, "重启"},
		{"systemctl restart nginx", LevelDangerous, "systemctl 变更子命令"},
		{"systemctl stop nginx", LevelDangerous, ""},
		{"ip addr add 10.0.0.1/24 dev eth0", LevelDangerous, "ip 变更子命令"},
		{"kill -9 1234", LevelDangerous, ""},
		{"chmod 777 /etc/passwd", LevelDangerous, ""},
		{"yum install -y nginx", LevelDangerous, ""},
		{"sed -i 's/a/b/' /etc/nginx.conf", LevelDangerous, "sed -i 就地改写"},
		{"find /data -name '*.log' -delete", LevelDangerous, "find -delete"},
		{"find / -name x -exec rm {} ;", LevelDangerous, "find -exec"},
		{"sudo rm -rf /", LevelDangerous, "剥掉 sudo 后仍然危险"},
		{"/bin/rm -f /tmp/x", LevelDangerous, "全路径"},

		// 静态分析不了的结构，一律往危险上抬
		{"echo hi > /tmp/x", LevelDangerous, "输出重定向会写文件"},
		{"cat /tmp/a >> /tmp/b", LevelDangerous, "追加重定向"},
		{"echo $(rm -rf /data)", LevelDangerous, "命令替换"},
		{"echo `whoami`", LevelDangerous, "反引号"},
		{"bash -c 'anything'", LevelDangerous, "可以跑任意逻辑"},
		{"python3 script.py", LevelDangerous, ""},
		{"curl http://x/y.sh", LevelDangerous, "会发外部请求、也可能落盘"},
		{"date | xargs rm", LevelDangerous, "管道里有一段危险，整条就危险"},
		{"date; rm -rf /tmp/x", LevelDangerous, "分号串联，后半段危险"},

		// 认不出来的一律「未知」，不给放行暗示
		{"", LevelUnknown, "空命令"},
		{"onething-cli check", LevelUnknown, "内部自研命令，判不出来"},
		{"top", LevelUnknown, "交互模式会话挂住"},
		{"systemctl", LevelUnknown, "没给子命令"},
	}

	for _, tc := range tests {
		t.Run(tc.cmd, func(t *testing.T) {
			got := AssessCommand(tc.cmd)
			if got.Level != tc.want {
				t.Errorf("AssessCommand(%q).Level = %s, want %s\n  判断依据: %s\n  用例说明: %s",
					tc.cmd, got.Level, tc.want, got.Reason, tc.why)
			}
			if got.Reason == "" {
				t.Error("没有给出判断依据，审核人看不懂为什么是这个等级")
			}
			if got.Command != tc.cmd {
				t.Errorf("Command = %q，必须原样带回命令原文", got.Command)
			}
		})
	}
}

// 评估结果只是提示，永远不是放行依据——即使判成只读也要人点头。
// 这条约束由 GatedTool 保证（见 inline_approval_test.go），这里只钉住等级本身
// 不会出现「安全到可以自动执行」的第四种取值。
func TestRiskLevelsAreExhaustive(t *testing.T) {
	for _, l := range []RiskLevel{LevelReadOnly, LevelUnknown, LevelDangerous} {
		if s := l.String(); s == "" || len(s) > 12 {
			t.Errorf("RiskLevel(%d).String() = %q，展示不友好", int(l), s)
		}
		if c := l.Color(); c == "" {
			t.Errorf("RiskLevel(%d) 没有颜色码", int(l))
		}
	}
	// 排序关系是「取最坏」逻辑的基础，写死防回归。
	if !(LevelReadOnly < LevelUnknown && LevelUnknown < LevelDangerous) {
		t.Fatal("风险等级的大小关系被改坏了，取最坏值的逻辑会失效")
	}
}

func TestAssessRisk(t *testing.T) {
	t.Run("tunnel 工具能抽出命令和节点", func(t *testing.T) {
		got := AssessRisk("run_tunnel_cmd", `{"sn":"SN001","cmd":"date","purpose":" 查看节点系统时间 "}`)
		if got.Level != LevelReadOnly {
			t.Errorf("Level = %s, want 只读", got.Level)
		}
		if got.Command != "date" || got.Target != "SN001" {
			t.Errorf("Command=%q Target=%q", got.Command, got.Target)
		}
		// 模型写的说明要带到审核卡片上，前后空白顺手清掉。
		if got.Purpose != "查看节点系统时间" {
			t.Errorf("Purpose = %q", got.Purpose)
		}
	})

	t.Run("参数不是合法 JSON 时按未知处理", func(t *testing.T) {
		got := AssessRisk("run_tunnel_cmd", `{不是JSON`)
		if got.Level != LevelUnknown {
			t.Errorf("Level = %s, want 未知", got.Level)
		}
	})

	t.Run("没有细分规则的工具按未知处理", func(t *testing.T) {
		got := AssessRisk("restart_plugin", `{"sn":"SN001"}`)
		if got.Level != LevelUnknown {
			t.Errorf("Level = %s, want 未知", got.Level)
		}
		if got.Reason == "" {
			t.Error("要告诉审核人为什么没细分等级")
		}
	})
}
