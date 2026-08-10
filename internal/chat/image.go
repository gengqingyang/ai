package chat

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cloudwego/eino/schema"
)

const DefaultImagePrompt = "请分析这张图片，并结合当前对话给出结论。"

type ImageMeta struct {
	Source   string
	MIMEType string
	Bytes    int
	Remote   bool
}

func ParseImageCommand(line string) (source, prompt string, err error) {
	args := strings.TrimSpace(strings.TrimPrefix(line, "/image"))
	if args == "" {
		return "", "", errors.New("用法: /image <图片路径或 URL> [问题]")
	}

	if i := strings.Index(args, " -- "); i >= 0 {
		source, err = unquoteImageSource(strings.TrimSpace(args[:i]))
		prompt = strings.TrimSpace(args[i+4:])
	} else {
		source, prompt, err = splitImageArgs(args)
	}
	if err != nil {
		return "", "", err
	}
	if source == "" {
		return "", "", errors.New("图片路径或 URL 不能为空")
	}
	if prompt == "" {
		prompt = DefaultImagePrompt
	}
	return source, prompt, nil
}

func splitImageArgs(args string) (source, prompt string, err error) {
	switch args[0] {
	case '"':
		end := -1
		escaped := false
		for i := 1; i < len(args); i++ {
			if escaped {
				escaped = false
				continue
			}
			if args[i] == '\\' {
				escaped = true
				continue
			}
			if args[i] == '"' {
				end = i
				break
			}
		}
		if end < 0 {
			return "", "", errors.New("图片路径的双引号没有闭合")
		}
		source, err = strconv.Unquote(args[:end+1])
		if err != nil {
			return "", "", fmt.Errorf("解析图片路径: %w", err)
		}
		return source, strings.TrimSpace(args[end+1:]), nil
	case '\'':
		end := strings.IndexByte(args[1:], '\'')
		if end < 0 {
			return "", "", errors.New("图片路径的单引号没有闭合")
		}
		end++
		return args[1:end], strings.TrimSpace(args[end+1:]), nil
	default:
		if i := strings.IndexAny(args, " \t"); i >= 0 {
			return args[:i], strings.TrimSpace(args[i:]), nil
		}
		return args, "", nil
	}
}

func unquoteImageSource(source string) (string, error) {
	if len(source) >= 2 && source[0] == '\'' && source[len(source)-1] == '\'' {
		return source[1 : len(source)-1], nil
	}
	if len(source) >= 2 && source[0] == '"' && source[len(source)-1] == '"' {
		unquoted, err := strconv.Unquote(source)
		if err != nil {
			return "", fmt.Errorf("解析图片路径: %w", err)
		}
		return unquoted, nil
	}
	return source, nil
}

func BuildImageMessage(source, prompt string, maxBytes int, detail string) (*schema.Message, ImageMeta, error) {
	if imageURL, ok, err := parseImageURL(source); ok || err != nil {
		if err != nil {
			return nil, ImageMeta{}, err
		}
		msg := multimodalUserMessage(prompt, &schema.MessageInputImage{
			MessagePartCommon: schema.MessagePartCommon{URL: &imageURL},
			Detail:            schema.ImageURLDetail(detail),
		})
		return msg, ImageMeta{Source: imageURL, Remote: true}, nil
	}

	path, err := expandImagePath(source)
	if err != nil {
		return nil, ImageMeta{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, ImageMeta{}, fmt.Errorf("打开图片 %s: %w", path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, ImageMeta{}, fmt.Errorf("读取图片信息: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, ImageMeta{}, fmt.Errorf("图片路径不是普通文件: %s", path)
	}
	if info.Size() > int64(maxBytes) {
		return nil, ImageMeta{}, fmt.Errorf("图片大小 %.1fMB 超过限制 %.1fMB", float64(info.Size())/(1024*1024), float64(maxBytes)/(1024*1024))
	}

	data, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil {
		return nil, ImageMeta{}, fmt.Errorf("读取图片: %w", err)
	}
	if len(data) > maxBytes {
		return nil, ImageMeta{}, fmt.Errorf("图片读取后超过 %d bytes 限制", maxBytes)
	}
	mimeType := detectImageMIME(data)
	if !supportedImageMIME(mimeType) {
		return nil, ImageMeta{}, fmt.Errorf("不支持的图片格式 %q，仅支持 PNG、JPEG、GIF、WebP", mimeType)
	}

	encoded := base64.StdEncoding.EncodeToString(data)
	msg := multimodalUserMessage(prompt, &schema.MessageInputImage{
		MessagePartCommon: schema.MessagePartCommon{
			Base64Data: &encoded,
			MIMEType:   mimeType,
		},
		Detail: schema.ImageURLDetail(detail),
	})
	return msg, ImageMeta{Source: path, MIMEType: mimeType, Bytes: len(data)}, nil
}

func parseImageURL(source string) (string, bool, error) {
	parsed, err := url.Parse(source)
	if err != nil {
		return "", false, nil
	}
	if parsed.Scheme == "" {
		return "", false, nil
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", true, fmt.Errorf("图片 URL 仅支持 HTTP/HTTPS，当前为 %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", true, errors.New("图片 URL 缺少主机名")
	}
	return parsed.String(), true, nil
}

func expandImagePath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("展开图片路径: %w", err)
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("解析图片路径: %w", err)
	}
	return abs, nil
}

func detectImageMIME(data []byte) string {
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return "image/webp"
	}
	return http.DetectContentType(data)
}

func supportedImageMIME(mimeType string) bool {
	switch mimeType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

func multimodalUserMessage(prompt string, image *schema.MessageInputImage) *schema.Message {
	return &schema.Message{
		Role:    schema.User,
		Content: prompt,
		UserInputMultiContent: []schema.MessageInputPart{
			{Type: schema.ChatMessagePartTypeText, Text: prompt},
			{Type: schema.ChatMessagePartTypeImageURL, Image: image},
		},
	}
}
