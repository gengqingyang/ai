package tools

import (
	"encoding/json"
	"fmt"
	"strings"
)

// RiskLevel 是对一次具体操作的风险评估结果。
//
// 注意这只是给审核人的提示，不是准入判断：任何等级都仍然要人点头才执行。
// 之所以不做「低风险自动放行」，是因为下面的判断是纯启发式的字符串匹配，
// 判错的代价是在客户设备上跑了不该跑的命令。
type RiskLevel int

const (
	// LevelReadOnly 命中只读命令白名单，基本不改变机器状态。
	LevelReadOnly RiskLevel = iota
	// LevelUnknown 认不出来。绝大多数命令会落在这里——认不出就是不安全。
	LevelUnknown
	// LevelDangerous 命中危险动作特征：删除、重启、改配置、装卸软件等。
	LevelDangerous
)

func (l RiskLevel) String() string {
	switch l {
	case LevelReadOnly:
		return "只读"
	case LevelUnknown:
		return "未知"
	case LevelDangerous:
		return "危险"
	default:
		return fmt.Sprintf("unknown(%d)", int(l))
	}
}

// Color 返回终端着色用的 ANSI 颜色码。
func (l RiskLevel) Color() string {
	switch l {
	case LevelReadOnly:
		return "32" // 绿
	case LevelUnknown:
		return "33" // 黄
	default:
		return "31" // 红
	}
}

// RiskAssessment 是一条操作的风险评估。
type RiskAssessment struct {
	Level RiskLevel
	// Reason 说明判成这个等级的依据，直接展示给审核人。
	Reason string
	// Target 是操作对象（节点 SN 等），可能为空。
	Target string
	// Command 是抽出来的命令原文，可能为空（非命令类工具）。
	Command string
	// Purpose 是模型自述的操作意图，用来让审核人一眼看懂这条命令是干嘛的。
	// 它没有经过任何校验——展示时必须和 Command 并排放，判断以命令原文为准。
	Purpose string
}

// readOnlyCommands 是明确不改变机器状态的命令。
// 只进不出地维护：拿不准的一律别往里加，落到「未知」等级没有任何损失。
var readOnlyCommands = map[string]bool{
	"date": true, "uptime": true, "whoami": true, "id": true, "hostname": true,
	"uname": true, "pwd": true, "arch": true, "nproc": true, "printenv": true,
	"env": true, "which": true, "whereis": true, "type": true, "echo": true,

	"ls": true, "cat": true, "head": true, "tail": true, "less": true, "more": true,
	"wc": true, "stat": true, "file": true, "readlink": true, "realpath": true,
	"md5sum": true, "sha1sum": true, "sha256sum": true, "basename": true, "dirname": true,

	"df": true, "du": true, "free": true, "lsblk": true, "blkid": true, "lscpu": true,
	"lspci": true, "lsusb": true, "lsmod": true, "lsof": true, "dmidecode": true,

	"ps": true, "pgrep": true, "vmstat": true, "iostat": true, "mpstat": true,
	"sar": true, "pidstat": true, "uptime2": true,

	"netstat": true, "ss": true, "ifconfig": true, "route": true, "arp": true,
	"ping": true, "ping6": true, "traceroute": true, "tracepath": true,
	"dig": true, "nslookup": true, "host": true, "getent": true,

	"dmesg": true, "journalctl": true, "last": true, "w": true, "who": true,

	"grep": true, "egrep": true, "fgrep": true, "zgrep": true, "sort": true,
	"uniq": true, "cut": true, "tr": true, "column": true, "jq": true, "xxd": true,
}

// dangerousCommands 是明确会改变机器状态的命令。
var dangerousCommands = map[string]bool{
	"rm": true, "rmdir": true, "shred": true, "truncate": true, "mv": true,
	"cp": true, "dd": true, "mkfs": true, "fdisk": true, "parted": true,
	"mount": true, "umount": true, "swapoff": true, "swapon": true,

	"reboot": true, "shutdown": true, "halt": true, "poweroff": true, "init": true,
	"kill": true, "pkill": true, "killall": true,

	"chmod": true, "chown": true, "chgrp": true, "chattr": true, "setfacl": true,
	"ln": true, "mkdir": true, "touch": true, "tee": true,

	"service": true, "supervisorctl": true, "crontab": true,
	"insmod": true, "rmmod": true, "modprobe": true, "depmod": true,
	"iptables": true, "ip6tables": true, "nft": true, "tc": true, "ipset": true,

	"yum": true, "dnf": true, "apt": true, "apt-get": true, "rpm": true, "dpkg": true,
	"pip": true, "pip3": true, "npm": true, "docker": true, "podman": true, "kubectl": true,

	"useradd": true, "userdel": true, "usermod": true, "groupadd": true,
	"passwd": true, "chpasswd": true, "visudo": true,

	"reset": true, "sysctl": true, "hwclock": true, "ntpdate": true, "timedatectl": true,
}

// systemctlReadOnly 是 systemctl 的只读子命令。
var systemctlReadOnly = map[string]bool{
	"status": true, "show": true, "cat": true, "is-active": true, "is-enabled": true,
	"is-failed": true, "list-units": true, "list-unit-files": true,
	"list-timers": true, "list-sockets": true, "list-dependencies": true,
	"get-default": true, "show-environment": true,
}

// ipReadOnly 是 ip 命令的只读子命令。
var ipReadOnly = map[string]bool{
	"addr": true, "a": true, "link": true, "l": true, "route": true, "r": true,
	"neigh": true, "n": true, "rule": true, "netns": true, "maddr": true,
}

// AssessCommand 对一条 shell 命令做启发式风险评估。
//
// 判断刻意保守：只有整条命令的每一段都命中只读白名单，才会给出「只读」；
// 出现重定向、命令替换、后台执行等无法静态分析的结构，一律往上抬。
func AssessCommand(cmd string) RiskAssessment {
	trimmed := strings.TrimSpace(cmd)
	if trimmed == "" {
		return RiskAssessment{Level: LevelUnknown, Reason: "命令为空", Command: cmd}
	}

	// 命令替换的内容无法静态判断，直接按危险处理。
	if strings.Contains(trimmed, "$(") || strings.Contains(trimmed, "`") {
		return RiskAssessment{
			Level:   LevelDangerous,
			Reason:  "含命令替换（$( ) 或反引号），实际执行什么无法事先判断",
			Command: cmd,
		}
	}
	// 输出重定向会写文件。
	if strings.Contains(trimmed, ">") {
		return RiskAssessment{
			Level:   LevelDangerous,
			Reason:  "含输出重定向（>），会写入文件",
			Command: cmd,
		}
	}

	segments := splitSegments(trimmed)
	worst := RiskAssessment{Level: LevelReadOnly, Reason: "全部是只读查询命令", Command: cmd}
	for _, seg := range segments {
		a := assessSegment(seg)
		if a.Level > worst.Level {
			worst = RiskAssessment{Level: a.Level, Reason: a.Reason, Command: cmd}
		}
	}
	return worst
}

// splitSegments 按 shell 的命令分隔符切开，逐段判断。
// 用 |、&&、||、;、& 分隔——只要有一段危险，整条就危险。
func splitSegments(cmd string) []string {
	replacer := strings.NewReplacer("&&", "\x00", "||", "\x00", "|", "\x00", ";", "\x00", "&", "\x00", "\n", "\x00")
	parts := strings.Split(replacer.Replace(cmd), "\x00")

	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func assessSegment(seg string) RiskAssessment {
	fields := strings.Fields(seg)
	if len(fields) == 0 {
		return RiskAssessment{Level: LevelUnknown, Reason: "无法解析的命令片段"}
	}

	// 剥掉 sudo / 环境变量前缀，看真正执行的是什么。
	for len(fields) > 0 {
		head := fields[0]
		if head == "sudo" || head == "nohup" || head == "time" || head == "nice" {
			fields = fields[1:]
			continue
		}
		// FOO=bar cmd 形式的环境变量前缀
		if strings.Contains(head, "=") && !strings.HasPrefix(head, "-") {
			fields = fields[1:]
			continue
		}
		break
	}
	if len(fields) == 0 {
		return RiskAssessment{Level: LevelUnknown, Reason: "只有前缀、没有实际命令"}
	}

	// 只取 basename，/usr/bin/rm 和 rm 是一回事。
	name := fields[0]
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	args := fields[1:]

	// 带子命令的几个特例，先于总表判断。
	switch name {
	case "systemctl":
		return assessSubcommand(name, args, systemctlReadOnly,
			"systemctl 的变更子命令（start/stop/restart/enable 等）会改变服务状态")
	case "ip":
		return assessIP(args)
	case "sed":
		if containsFlag(args, "-i") || containsFlag(args, "--in-place") {
			return RiskAssessment{Level: LevelDangerous, Reason: "sed -i 会就地改写文件"}
		}
		return RiskAssessment{Level: LevelReadOnly, Reason: "sed 只读流处理"}
	case "find":
		for _, a := range args {
			if a == "-delete" || a == "-exec" || a == "-execdir" || a == "-ok" {
				return RiskAssessment{Level: LevelDangerous, Reason: "find " + a + " 会执行删除或任意命令"}
			}
		}
		return RiskAssessment{Level: LevelReadOnly, Reason: "find 只做检索"}
	case "awk", "perl", "python", "python3", "sh", "bash", "zsh", "xargs", "eval":
		return RiskAssessment{Level: LevelDangerous, Reason: name + " 可以执行任意逻辑，无法静态判断实际行为"}
	case "curl", "wget":
		return RiskAssessment{Level: LevelDangerous, Reason: name + " 会发起外部请求、也可能落盘文件"}
	case "top":
		if containsFlag(args, "-b") {
			return RiskAssessment{Level: LevelReadOnly, Reason: "top 批处理模式，只读"}
		}
		return RiskAssessment{Level: LevelUnknown, Reason: "top 交互模式会话挂住，建议加 -bn1"}
	}

	if dangerousCommands[name] {
		return RiskAssessment{Level: LevelDangerous, Reason: name + " 会改变机器状态"}
	}
	if readOnlyCommands[name] {
		return RiskAssessment{Level: LevelReadOnly, Reason: name + " 是只读查询命令"}
	}
	return RiskAssessment{Level: LevelUnknown, Reason: "不认识 " + name + " 这条命令，无法判断它会做什么"}
}

// assessSubcommand 处理「主命令 + 子命令」形式：子命令在白名单里才算只读。
func assessSubcommand(name string, args []string, readOnly map[string]bool, dangerReason string) RiskAssessment {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue // 跳过选项，找第一个子命令
		}
		if readOnly[a] {
			return RiskAssessment{Level: LevelReadOnly, Reason: name + " " + a + " 是只读查询"}
		}
		return RiskAssessment{Level: LevelDangerous, Reason: dangerReason}
	}
	return RiskAssessment{Level: LevelUnknown, Reason: name + " 未指定子命令"}
}

// assessIP 判断 ip 命令。
//
// 它是「ip <对象> <动作>」两级结构：ip addr 是查，ip addr add 是改。
// 只看第一级会把 `ip addr add ...` 误判成只读，所以动作那一级必须一起看。
func assessIP(args []string) RiskAssessment {
	var words []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			words = append(words, a)
		}
	}
	if len(words) == 0 {
		return RiskAssessment{Level: LevelUnknown, Reason: "ip 未指定操作对象"}
	}
	if !ipReadOnly[words[0]] {
		return RiskAssessment{Level: LevelDangerous, Reason: "ip " + words[0] + " 不在只读对象白名单里"}
	}
	// 只有 show/list/get 这几个动作是查询；不写动作时默认就是 show。
	if len(words) == 1 {
		return RiskAssessment{Level: LevelReadOnly, Reason: "ip " + words[0] + " 默认是查询"}
	}
	switch words[1] {
	case "show", "list", "lst", "get":
		return RiskAssessment{Level: LevelReadOnly, Reason: "ip " + words[0] + " " + words[1] + " 是只读查询"}
	default:
		return RiskAssessment{
			Level:  LevelDangerous,
			Reason: "ip " + words[0] + " " + words[1] + " 会改动网络配置",
		}
	}
}

func containsFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") &&
			len(flag) == 2 && strings.Contains(a, flag[1:])) {
			return true
		}
	}
	return false
}

// AssessRisk 根据工具名和调用参数评估风险。
//
// 目前只有 run_tunnel_cmd 能细分等级：它的参数里带着要执行的 shell 命令。
// 其他变更类工具认不出细节，统一按「未知」处理，让审核人自己看参数。
func AssessRisk(toolName, argsJSON string) RiskAssessment {
	if toolName != tunnelToolName {
		return RiskAssessment{
			Level:  LevelUnknown,
			Reason: "该工具没有细分风险规则，请直接核对下面的参数",
		}
	}

	var in tunnelInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return RiskAssessment{
			Level:  LevelUnknown,
			Reason: "参数不是合法 JSON，无法解析出命令: " + err.Error(),
		}
	}

	a := AssessCommand(in.Cmd)
	a.Target = in.Sn
	a.Purpose = strings.TrimSpace(in.Purpose)
	return a
}
