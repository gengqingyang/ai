package repository

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/cloudwego/eino/adk/filesystem"
)

// LocalRepoBackend 实现 Eino filesystem.Backend，但只开放当前仓库的安全索引。
// Write 和 Edit 永远拒绝；该类型也没有实现 filesystem.Shell。
type LocalRepoBackend struct {
	manager *Manager
}

// NewLocalRepoBackend 创建 Eino filesystem 协议的只读仓库后端。
func NewLocalRepoBackend(manager *Manager) (*LocalRepoBackend, error) {
	if manager == nil {
		return nil, errors.New("仓库管理器不能为空")
	}
	return &LocalRepoBackend{manager: manager}, nil
}

func (b *LocalRepoBackend) LsInfo(ctx context.Context, req *filesystem.LsInfoRequest) ([]filesystem.FileInfo, error) {
	if req == nil {
		return nil, errors.New("列目录请求不能为空")
	}
	_, files, err := b.manager.ListFiles(ctx, req.Path, maxResultLimit)
	if err != nil {
		return nil, err
	}
	result := make([]filesystem.FileInfo, 0, len(files))
	for _, file := range files {
		result = append(result, filesystem.FileInfo{
			Path: file.Path, Size: file.Bytes, ModifiedAt: file.ModifiedAt.Format("2006-01-02T15:04:05.999999999Z07:00"),
		})
	}
	return result, nil
}

func (b *LocalRepoBackend) Read(ctx context.Context, req *filesystem.ReadRequest) (*filesystem.FileContent, error) {
	if req == nil {
		return nil, errors.New("读文件请求不能为空")
	}
	start := req.Offset
	if start <= 0 {
		start = 1
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 120
	}
	if limit > maxReadLines {
		limit = maxReadLines
	}
	_, lines, err := b.manager.ReadFile(ctx, req.FilePath, start, start+limit-1)
	if err != nil {
		return nil, err
	}
	text := make([]string, 0, len(lines))
	for _, line := range lines {
		text = append(text, line.Text)
	}
	return &filesystem.FileContent{Content: strings.Join(text, "\n")}, nil
}

func (b *LocalRepoBackend) GrepRaw(ctx context.Context, req *filesystem.GrepRequest) ([]filesystem.GrepMatch, error) {
	if req == nil {
		return nil, errors.New("搜索请求不能为空")
	}
	if req.EnableMultiline || req.BeforeLines > 0 || req.AfterLines > 0 || req.FileType != "" {
		return nil, errors.New("本地仓库后端只支持逐行正则搜索和可选 glob")
	}
	_, matches, err := b.manager.Grep(ctx, req.Pattern, req.Path, req.CaseInsensitive, maxResultLimit)
	if err != nil {
		return nil, err
	}
	result := make([]filesystem.GrepMatch, 0, len(matches))
	for _, match := range matches {
		if req.Glob != "" {
			matched, matchErr := path.Match(req.Glob, match.Path)
			if matchErr != nil {
				return nil, fmt.Errorf("非法 glob %q: %w", req.Glob, matchErr)
			}
			if !matched {
				continue
			}
		}
		result = append(result, filesystem.GrepMatch{Path: match.Path, Line: match.Line, Content: match.Text})
	}
	return result, nil
}

func (b *LocalRepoBackend) GlobInfo(ctx context.Context, req *filesystem.GlobInfoRequest) ([]filesystem.FileInfo, error) {
	if req == nil {
		return nil, errors.New("glob 请求不能为空")
	}
	files, err := b.LsInfo(ctx, &filesystem.LsInfoRequest{Path: req.Path})
	if err != nil {
		return nil, err
	}
	pattern := req.Pattern
	if pattern == "" {
		pattern = "*"
	}
	result := make([]filesystem.FileInfo, 0, len(files))
	for _, file := range files {
		matched, matchErr := path.Match(pattern, file.Path)
		if matchErr != nil {
			return nil, fmt.Errorf("非法 glob %q: %w", pattern, matchErr)
		}
		if matched {
			result = append(result, file)
		}
	}
	return result, nil
}

func (b *LocalRepoBackend) Write(context.Context, *filesystem.WriteRequest) error {
	return errors.New("本地代码仓库后端是只读的，禁止写文件")
}

func (b *LocalRepoBackend) Edit(context.Context, *filesystem.EditRequest) error {
	return errors.New("本地代码仓库后端是只读的，禁止编辑文件")
}

var _ filesystem.Backend = (*LocalRepoBackend)(nil)
