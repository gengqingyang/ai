package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/cloudwego/eino/schema"
)

const cacheVersion = 1

// fileCache 用版本化 JSON 保存 user/assistant 最终消息。
type fileCache struct {
	path string
}

type cacheFile struct {
	Version  int            `json:"version"`
	Messages []cacheMessage `json:"messages"`
}

type cacheMessage struct {
	Role    string      `json:"role"`
	Content string      `json:"content"`
	Parts   []cachePart `json:"parts,omitempty"`
}

type cachePart struct {
	Type       string `json:"type"`
	Text       string `json:"text,omitempty"`
	URL        string `json:"url,omitempty"`
	Base64Data string `json:"base64_data,omitempty"`
	MIMEType   string `json:"mime_type,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

func newFileCache(path string) *fileCache {
	return &fileCache{path: path}
}

func (c *fileCache) Load() ([]*schema.Message, error) {
	if c.path == "" {
		return nil, nil
	}

	f, err := os.Open(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var data cacheFile
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&data); err != nil {
		return nil, fmt.Errorf("解析 %s: %w", c.path, err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("解析 %s: %w", c.path, err)
	}
	if data.Version != cacheVersion {
		return nil, fmt.Errorf("不支持的缓存版本 %d（当前支持 %d）", data.Version, cacheVersion)
	}

	messages := make([]*schema.Message, 0, len(data.Messages))
	for i, msg := range data.Messages {
		switch msg.Role {
		case "user":
			loaded, err := loadUserMessage(msg)
			if err != nil {
				return nil, fmt.Errorf("第 %d 条消息无效: %w", i+1, err)
			}
			messages = append(messages, loaded)
		case "assistant":
			messages = append(messages, schema.AssistantMessage(msg.Content, nil))
		default:
			return nil, fmt.Errorf("第 %d 条消息的角色 %q 无效", i+1, msg.Role)
		}
	}
	return messages, nil
}

func loadUserMessage(cached cacheMessage) (*schema.Message, error) {
	msg := schema.UserMessage(cached.Content)
	for i, part := range cached.Parts {
		switch part.Type {
		case "text":
			msg.UserInputMultiContent = append(msg.UserInputMultiContent, schema.MessageInputPart{
				Type: schema.ChatMessagePartTypeText,
				Text: part.Text,
			})
		case "image_url":
			if (part.URL == "") == (part.Base64Data == "") {
				return nil, fmt.Errorf("第 %d 个图片 part 必须且只能包含 URL 或 base64", i+1)
			}
			if part.Base64Data != "" && part.MIMEType == "" {
				return nil, fmt.Errorf("第 %d 个图片 part 缺少 MIME type", i+1)
			}
			image := &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{MIMEType: part.MIMEType},
				Detail:            schema.ImageURLDetail(part.Detail),
			}
			if part.URL != "" {
				image.URL = &part.URL
			} else {
				image.Base64Data = &part.Base64Data
			}
			msg.UserInputMultiContent = append(msg.UserInputMultiContent, schema.MessageInputPart{
				Type:  schema.ChatMessagePartTypeImageURL,
				Image: image,
			})
		default:
			return nil, fmt.Errorf("第 %d 个 part 类型 %q 不受支持", i+1, part.Type)
		}
	}
	return msg, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("文件包含多余的 JSON 数据")
}

func (c *fileCache) Save(messages []*schema.Message) error {
	if c.path == "" {
		return nil
	}

	data := cacheFile{
		Version:  cacheVersion,
		Messages: make([]cacheMessage, 0, len(messages)),
	}
	for _, msg := range messages {
		cached := cacheMessage{Content: msg.Content}
		switch msg.Role {
		case schema.User:
			cached.Role = "user"
			parts, err := saveUserParts(msg.UserInputMultiContent)
			if err != nil {
				return err
			}
			cached.Parts = parts
		case schema.Assistant:
			cached.Role = "assistant"
		default:
			return fmt.Errorf("不能缓存角色 %q", msg.Role)
		}
		data.Messages = append(data.Messages, cached)
	}

	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建缓存目录: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(c.path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	encoder := json.NewEncoder(tmp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, c.path); err != nil {
		return err
	}
	return nil
}

func saveUserParts(parts []schema.MessageInputPart) ([]cachePart, error) {
	cached := make([]cachePart, 0, len(parts))
	for i, part := range parts {
		switch part.Type {
		case schema.ChatMessagePartTypeText:
			cached = append(cached, cachePart{Type: "text", Text: part.Text})
		case schema.ChatMessagePartTypeImageURL:
			if part.Image == nil {
				return nil, fmt.Errorf("第 %d 个图片 part 为空", i+1)
			}
			item := cachePart{
				Type:     "image_url",
				MIMEType: part.Image.MIMEType,
				Detail:   string(part.Image.Detail),
			}
			if part.Image.URL != nil {
				item.URL = *part.Image.URL
			}
			if part.Image.Base64Data != nil {
				item.Base64Data = *part.Image.Base64Data
			}
			if (item.URL == "") == (item.Base64Data == "") {
				return nil, fmt.Errorf("第 %d 个图片 part 必须且只能包含 URL 或 base64", i+1)
			}
			cached = append(cached, item)
		default:
			return nil, fmt.Errorf("不能缓存第 %d 个用户消息 part 类型 %q", i+1, part.Type)
		}
	}
	return cached, nil
}
