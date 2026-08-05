package main

import "testing"

func TestClassifyKey(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want menuKey
	}{
		{"上方向键", "\x1b[A", keyUp},
		{"左方向键", "\x1b[D", keyUp},
		{"下方向键", "\x1b[B", keyDown},
		{"右方向键", "\x1b[C", keyDown},
		{"vim k", "k", keyUp},
		{"vim j", "j", keyDown},
		{"回车", "\r", keyConfirm},
		{"换行", "\n", keyConfirm},
		// raw 模式下终端不再把 Ctrl-C 转成信号，得自己认这个字节。
		{"Ctrl-C", "\x03", keyInterrupt},
		{"Ctrl-D", "\x04", keyEOF},
		{"普通字符", "y", keyOther},
		{"孤立 ESC", "\x1b", keyOther},
		{"认不出的转义序列", "\x1b[5~", keyOther},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyKey([]byte(tc.in)); got != tc.want {
				t.Errorf("classifyKey(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// 光标默认停在「执行」——但这只是省一次按键，不是省一次确认：
// 不回车就什么都不会发生。
func TestApproveIsTheDefaultOption(t *testing.T) {
	if optRun != 0 {
		t.Fatalf("optRun = %d，光标默认位置必须是「执行」那一项", optRun)
	}
	if optDeny == optRun {
		t.Fatal("执行和拒绝指向了同一项")
	}
}
