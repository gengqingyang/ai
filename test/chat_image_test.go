package test

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	. "diagnostic-system/internal/chat"
	"diagnostic-system/internal/ui"

	"github.com/cloudwego/eino/schema"
)

func TestParseImageCommand(t *testing.T) {
	tests := []struct {
		line       string
		wantSource string
		wantPrompt string
	}{
		{"/image a.png", "a.png", DefaultImagePrompt},
		{"/image a.png 看看报错", "a.png", "看看报错"},
		{`/image "/tmp/a b.png" 看看报错`, "/tmp/a b.png", "看看报错"},
		{"/image /tmp/a b.png -- 看看报错", "/tmp/a b.png", "看看报错"},
	}
	for _, tc := range tests {
		t.Run(tc.line, func(t *testing.T) {
			source, prompt, err := ParseImageCommand(tc.line)
			if err != nil {
				t.Fatal(err)
			}
			if source != tc.wantSource || prompt != tc.wantPrompt {
				t.Fatalf("got (%q, %q), want (%q, %q)", source, prompt, tc.wantSource, tc.wantPrompt)
			}
		})
	}
}

func TestBuildLocalImageMessage(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pixel.png")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	msg, meta, err := BuildImageMessage(path, "分析", 1024, "high")
	if err != nil {
		t.Fatal(err)
	}
	if msg.Role != schema.User || msg.Content != "分析" || len(msg.UserInputMultiContent) != 2 {
		t.Fatalf("多模态消息不完整: %#v", msg)
	}
	image := msg.UserInputMultiContent[1].Image
	if image == nil || image.Base64Data == nil || image.MIMEType != "image/png" || image.Detail != schema.ImageURLDetailHigh {
		t.Fatalf("图片 part 不正确: %#v", image)
	}
	if meta.Bytes != len(raw) || meta.MIMEType != "image/png" || meta.Remote {
		t.Fatalf("图片元数据不正确: %#v", meta)
	}
}

func TestBuildRemoteImageMessage(t *testing.T) {
	msg, meta, err := BuildImageMessage("https://example.com/a.png", "分析", 1, "auto")
	if err != nil {
		t.Fatal(err)
	}
	image := msg.UserInputMultiContent[1].Image
	if image == nil || image.URL == nil || *image.URL != "https://example.com/a.png" {
		t.Fatalf("远程图片 part 不正确: %#v", image)
	}
	if !meta.Remote {
		t.Fatal("远程图片元数据未标记 remote")
	}
}

func TestBuildImageRejectsUnsupportedAndOversizedFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-image.txt")
	if err := os.WriteFile(path, []byte("plain text"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := BuildImageMessage(path, "分析", 1024, "auto"); err == nil {
		t.Fatal("非图片应该被拒绝")
	}
	if _, _, err := BuildImageMessage(path, "分析", 2, "auto"); err == nil {
		t.Fatal("超限文件应该被拒绝")
	}
}

func TestInputMaxBytesSupportsMillionTokenInput(t *testing.T) {
	if got, want := ui.InputMaxBytes(1_000_000), 5_000_000; got != want {
		t.Fatalf("InputMaxBytes(1M) = %d, want %d", got, want)
	}
}
