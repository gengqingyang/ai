// Package skills wires local SKILL.md files into Eino's Skill middleware.
package skills

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/filesystem"
	einoskill "github.com/cloudwego/eino/adk/middlewares/skill"
)

// Load validates the local skill catalog and builds the ADK middleware.
//
// Skills are intentionally limited to inline mode. Forked skills require an
// AgentHub and a separate decision about which tools and approval policy the
// child agent inherits; accepting them without that boundary would fail late
// or silently weaken the main agent's safety model.
func Load(ctx context.Context, dir string) (adk.ChatModelAgentMiddleware, []einoskill.FrontMatter, error) {
	if err := adk.SetLanguage(adk.LanguageChinese); err != nil {
		return nil, nil, fmt.Errorf("设置 Eino ADK 语言失败: %w", err)
	}
	fs, err := newReadOnlyFilesystem(dir)
	if err != nil {
		return nil, nil, err
	}

	backend, err := einoskill.NewBackendFromFilesystem(ctx, &einoskill.BackendFromFilesystemConfig{
		Backend: fs,
		BaseDir: fs.root,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("创建 Skill 文件后端失败: %w", err)
	}

	matters, err := backend.List(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("加载 Skill 目录 %q 失败: %w", dir, err)
	}
	if err := validateFrontMatters(matters); err != nil {
		return nil, nil, fmt.Errorf("校验 Skill 目录 %q 失败: %w", dir, err)
	}

	handler, err := einoskill.NewMiddleware(ctx, &einoskill.Config{Backend: backend})
	if err != nil {
		return nil, nil, fmt.Errorf("创建 Skill middleware 失败: %w", err)
	}
	return handler, matters, nil
}

func validateFrontMatters(matters []einoskill.FrontMatter) error {
	seen := make(map[string]struct{}, len(matters))
	for _, matter := range matters {
		name := strings.TrimSpace(matter.Name)
		if name == "" {
			return errors.New("SKILL.md 的 name 不能为空")
		}
		if strings.TrimSpace(matter.Description) == "" {
			return fmt.Errorf("Skill %q 的 description 不能为空", name)
		}
		if _, ok := seen[name]; ok {
			return fmt.Errorf("Skill 名称 %q 重复", name)
		}
		seen[name] = struct{}{}

		switch matter.Context {
		case "":
		case einoskill.ContextModeFork, einoskill.ContextModeForkWithContext:
			return fmt.Errorf("Skill %q 使用 context: %s；当前仅支持 inline Skill", name, matter.Context)
		default:
			return fmt.Errorf("Skill %q 的 context %q 无效", name, matter.Context)
		}
		if matter.Agent != "" {
			return fmt.Errorf("Skill %q 配置了 agent；inline Skill 不支持子 Agent", name)
		}
		if matter.Model != "" {
			return fmt.Errorf("Skill %q 配置了 model；当前未配置 Skill ModelHub", name)
		}
	}
	return nil
}

// readOnlyFilesystem implements only the two filesystem operations used by
// Eino's Skill backend. All paths are confined to root, including symlinks.
type readOnlyFilesystem struct {
	root string
}

func newReadOnlyFilesystem(dir string) (*readOnlyFilesystem, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("AGENT_SKILLS_DIR 不能为空")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("解析 Skill 目录失败: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("访问 Skill 目录 %q 失败: %w", dir, err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return nil, fmt.Errorf("访问 Skill 目录 %q 失败: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("Skill 路径 %q 不是目录", dir)
	}
	return &readOnlyFilesystem{root: filepath.Clean(real)}, nil
}

func (f *readOnlyFilesystem) Read(ctx context.Context, req *filesystem.ReadRequest) (*filesystem.FileContent, error) {
	if req == nil {
		return nil, errors.New("读取请求不能为空")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := f.resolve(req.FilePath)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	content := string(data)
	if req.Offset <= 1 && req.Limit <= 0 {
		return &filesystem.FileContent{Content: content}, nil
	}
	return &filesystem.FileContent{Content: sliceLines(content, req.Offset, req.Limit)}, nil
}

func (f *readOnlyFilesystem) GlobInfo(ctx context.Context, req *filesystem.GlobInfoRequest) ([]filesystem.FileInfo, error) {
	if req == nil {
		return nil, errors.New("glob 请求不能为空")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	base, err := f.resolve(req.Path)
	if err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(base, filepath.FromSlash(req.Pattern)))
	if err != nil {
		return nil, err
	}
	result := make([]filesystem.FileInfo, 0, len(matches))
	for _, match := range matches {
		path, resolveErr := f.resolve(match)
		if resolveErr != nil {
			return nil, resolveErr
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			return nil, statErr
		}
		result = append(result, filesystem.FileInfo{
			Path:       path,
			IsDir:      info.IsDir(),
			Size:       info.Size(),
			ModifiedAt: info.ModTime().Format("2006-01-02T15:04:05.999999999Z07:00"),
		})
	}
	return result, nil
}

func (f *readOnlyFilesystem) resolve(path string) (string, error) {
	if path == "" {
		path = f.root
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(f.root, path)
	}
	path = filepath.Clean(path)
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(f.root, real)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("路径 %q 超出 Skill 目录", path)
	}
	return real, nil
}

func sliceLines(content string, offset, limit int) string {
	lines := strings.Split(content, "\n")
	start := offset - 1
	if start < 0 {
		start = 0
	}
	if start >= len(lines) {
		return ""
	}
	end := len(lines)
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	return strings.Join(lines[start:end], "\n")
}

var errReadOnly = errors.New("Skill 文件后端只允许读取")

func (f *readOnlyFilesystem) LsInfo(context.Context, *filesystem.LsInfoRequest) ([]filesystem.FileInfo, error) {
	return nil, errReadOnly
}

func (f *readOnlyFilesystem) GrepRaw(context.Context, *filesystem.GrepRequest) ([]filesystem.GrepMatch, error) {
	return nil, errReadOnly
}

func (f *readOnlyFilesystem) Write(context.Context, *filesystem.WriteRequest) error {
	return errReadOnly
}

func (f *readOnlyFilesystem) Edit(context.Context, *filesystem.EditRequest) error {
	return errReadOnly
}
