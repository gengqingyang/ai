package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

var (
	// errMenuInterrupted 是菜单里按下 Ctrl-C。raw 模式下终端不再发 SIGINT，
	// 得自己认这个字节。
	errMenuInterrupted = errors.New("确认被中断（Ctrl-C）")
	// errMenuEOF 是输入流断了。
	errMenuEOF = errors.New("确认输入已结束")
)

// menuKey 是菜单认识的按键动作。
type menuKey int

const (
	keyOther menuKey = iota
	keyUp
	keyDown
	keyConfirm
	keyInterrupt
	keyEOF
)

// classifyKey 把读到的原始字节翻译成动作。
//
// 方向键是 ESC [ A 这样的多字节序列。raw 模式下 Ctrl-C / Ctrl-D 不再由终端转成
// 信号，而是原样送到这里，必须自己认。
func classifyKey(b []byte) menuKey {
	switch string(b) {
	case "\x1b[A", "\x1b[D", "k", "K": // 上 / 左 / vim k
		return keyUp
	case "\x1b[B", "\x1b[C", "j", "J": // 下 / 右 / vim j
		return keyDown
	case "\r", "\n":
		return keyConfirm
	case "\x03": // Ctrl-C
		return keyInterrupt
	case "\x04": // Ctrl-D
		return keyEOF
	default:
		return keyOther
	}
}

// selectOption 在终端里画一个上下键菜单，回车确认，返回选中项下标。
//
// 直接读 tty 的原始字节，不走 REPL 那个 bufio.Scanner：一来 raw 模式下要按字节
// 认方向键的转义序列，二来 scanner 里可能攒着用户提前敲进去的内容——那些内容
// 绝不该拿来替人回答「要不要在客户设备上执行」。
//
// shortcuts 是直接定音的快捷键（比如 y/n），按下即选中并返回。
func selectOption(tty *os.File, out io.Writer, options []string, def int, shortcuts map[byte]int) (int, error) {
	if len(options) == 0 {
		return 0, errors.New("没有可选项")
	}

	fd := int(tty.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return 0, fmt.Errorf("终端无法进入 raw 模式: %w", err)
	}
	defer term.Restore(fd, state)

	cur := def
	if cur < 0 || cur >= len(options) {
		cur = 0
	}

	// raw 模式下 \n 只换行不回到行首，所以每行都要自己带 \r。
	draw := func(redraw bool) {
		if redraw {
			fmt.Fprintf(out, "\033[%dA", len(options)) // 回到菜单第一行
		}
		for i, opt := range options {
			if i == cur {
				fmt.Fprintf(out, "\r\033[K  \033[1;36m❯ %s\033[0m\r\n", opt)
			} else {
				fmt.Fprintf(out, "\r\033[K    \033[2m%s\033[0m\r\n", opt)
			}
		}
	}
	draw(false)

	buf := make([]byte, 16)
	for {
		n, err := tty.Read(buf)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0, errMenuEOF
			}
			return 0, err
		}
		if n == 0 {
			continue
		}

		switch classifyKey(buf[:n]) {
		case keyUp:
			cur = (cur - 1 + len(options)) % len(options)
			draw(true)
			continue
		case keyDown:
			cur = (cur + 1) % len(options)
			draw(true)
			continue
		case keyConfirm:
			return cur, nil
		case keyInterrupt:
			return 0, errMenuInterrupted
		case keyEOF:
			return 0, errMenuEOF
		}

		if n == 1 {
			if i, ok := shortcuts[buf[0]]; ok {
				cur = i
				draw(true)
				return cur, nil
			}
		}
	}
}
