package tools

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cloudwego/eino/adk/filesystem"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"diagnostic-system/internal/repository"
)

const (
	ToolListFiles             = "list_files"
	ToolSearchCode            = "search_code"
	ToolReadFile              = "read_file"
	ToolFindSymbol            = "find_symbol"
	ToolFindReferences        = "find_references"
	ToolGetDefinition         = "get_definition"
	ToolGetRepositoryRevision = "get_repository_revision"
)

// CodeToolNames 返回代码问答 Agent 唯一允许使用的工具名。
func CodeToolNames() []string {
	return []string{
		ToolListFiles,
		ToolSearchCode,
		ToolReadFile,
		ToolFindSymbol,
		ToolFindReferences,
		ToolGetDefinition,
		ToolGetRepositoryRevision,
	}
}

type listFilesInput struct {
	Prefix string `json:"prefix,omitempty" jsonschema_description:"仓库内相对目录前缀；留空表示整个仓库"`
	Limit  int    `json:"limit,omitempty" jsonschema_description:"最大结果数，默认 20，最多 100"`
}

type searchCodeInput struct {
	Query         string `json:"query" jsonschema:"required" jsonschema_description:"要精确查找的错误原文、错误码、配置名或代码文本"`
	Prefix        string `json:"prefix,omitempty" jsonschema_description:"可选的仓库内相对路径前缀"`
	CaseSensitive bool   `json:"case_sensitive,omitempty" jsonschema_description:"是否区分大小写，默认 false"`
	Limit         int    `json:"limit,omitempty" jsonschema_description:"最大匹配数，默认 20，最多 100"`
}

type readFileInput struct {
	Path      string `json:"path" jsonschema:"required" jsonschema_description:"安全索引中的仓库相对文件路径"`
	StartLine int    `json:"start_line,omitempty" jsonschema_description:"起始行，从 1 开始，默认 1"`
	EndLine   int    `json:"end_line,omitempty" jsonschema_description:"结束行，默认读取 120 行，单次最多 200 行"`
}

type symbolInput struct {
	Name  string `json:"name" jsonschema:"required" jsonschema_description:"Go 符号名，例如 Run 或 Manager.Reindex"`
	Limit int    `json:"limit,omitempty" jsonschema_description:"最大结果数，默认 20，最多 100"`
}

type referencesInput struct {
	Name      string `json:"name" jsonschema:"required" jsonschema_description:"Go 符号名，例如 Run 或 Manager.Reindex"`
	CallsOnly bool   `json:"calls_only,omitempty" jsonschema_description:"只返回函数或方法调用位置"`
	Limit     int    `json:"limit,omitempty" jsonschema_description:"最大结果数，默认 20，最多 100"`
}

type definitionInput struct {
	Name         string `json:"name" jsonschema:"required" jsonschema_description:"Go 符号名，例如 Run 或 Manager.Reindex"`
	ContextLines int    `json:"context_lines,omitempty" jsonschema_description:"定义前后附带行数，范围 0 到 20"`
	Limit        int    `json:"limit,omitempty" jsonschema_description:"最大定义数，默认 20，最多 100"`
}

type revisionInput struct{}

type fileListOutput struct {
	DataSource string                `json:"data_source"`
	Snapshot   repository.Snapshot   `json:"snapshot"`
	Files      []filesystem.FileInfo `json:"files"`
}

type searchOutput struct {
	DataSource string                   `json:"data_source"`
	Snapshot   repository.Snapshot      `json:"snapshot"`
	Query      string                   `json:"query"`
	Matches    []repository.SearchMatch `json:"matches"`
}

type readOutput struct {
	DataSource    string                  `json:"data_source"`
	Snapshot      repository.Snapshot     `json:"snapshot"`
	Status        string                  `json:"status"`
	Message       string                  `json:"message,omitempty"`
	Path          string                  `json:"path"`
	StartLine     int                     `json:"start_line,omitempty"`
	EndLine       int                     `json:"end_line,omitempty"`
	TotalLines    int                     `json:"total_lines"`
	Truncated     bool                    `json:"truncated"`
	HasMore       bool                    `json:"has_more"`
	NextStartLine int                     `json:"next_start_line,omitempty"`
	Lines         []repository.SourceLine `json:"lines"`
}

type symbolOutput struct {
	DataSource string              `json:"data_source"`
	Snapshot   repository.Snapshot `json:"snapshot"`
	Symbols    []repository.Symbol `json:"symbols"`
}

type referencesOutput struct {
	DataSource string                 `json:"data_source"`
	Snapshot   repository.Snapshot    `json:"snapshot"`
	References []repository.Reference `json:"references"`
	Limitation string                 `json:"limitation"`
}

type definitionOutput struct {
	DataSource  string                  `json:"data_source"`
	Snapshot    repository.Snapshot     `json:"snapshot"`
	Definitions []repository.Definition `json:"definitions"`
}

type revisionOutput struct {
	DataSource string              `json:"data_source"`
	Snapshot   repository.Snapshot `json:"snapshot"`
}

// NewCodeTools 创建只访问当前安全索引的本地代码工具。
func NewCodeTools(manager *repository.Manager) ([]tool.BaseTool, error) {
	if manager == nil {
		return nil, errors.New("仓库管理器不能为空")
	}
	backend, err := repository.NewLocalRepoBackend(manager)
	if err != nil {
		return nil, err
	}
	listFiles, err := utils.InferTool(
		ToolListFiles,
		"列出当前代码仓库安全索引中的文件；只接受仓库相对路径，不返回被排除的凭证、日志、二进制或外链文件。",
		func(ctx context.Context, in listFilesInput) (fileListOutput, error) {
			files, err := backend.LsInfo(ctx, &filesystem.LsInfoRequest{Path: in.Prefix})
			if err != nil {
				return fileListOutput{}, fmt.Errorf("列出仓库文件失败: %w", err)
			}
			files = limitSlice(files, in.Limit)
			snapshot, err := manager.Revision(ctx)
			if err != nil {
				return fileListOutput{}, fmt.Errorf("读取仓库版本失败: %w", err)
			}
			return fileListOutput{DataSource: "local_repository", Snapshot: snapshot, Files: files}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("构造 list_files 工具失败: %w", err)
	}

	searchCode, err := utils.InferTool(
		ToolSearchCode,
		"在当前代码仓库中按错误原文、错误码、配置名或代码片段做逐行精确文本检索，返回可引用的 path:line。",
		func(ctx context.Context, in searchCodeInput) (searchOutput, error) {
			matches, err := backend.GrepRaw(ctx, &filesystem.GrepRequest{
				Pattern: regexp.QuoteMeta(in.Query), Path: in.Prefix, CaseInsensitive: !in.CaseSensitive,
			})
			if err != nil {
				return searchOutput{}, fmt.Errorf("搜索仓库代码失败: %w", err)
			}
			matches = limitSlice(matches, in.Limit)
			snapshot, err := manager.Revision(ctx)
			if err != nil {
				return searchOutput{}, fmt.Errorf("读取仓库版本失败: %w", err)
			}
			converted := make([]repository.SearchMatch, 0, len(matches))
			for _, match := range matches {
				converted = append(converted, repository.SearchMatch{Path: match.Path, Line: match.Line, Text: match.Content})
			}
			return searchOutput{DataSource: "local_repository", Snapshot: snapshot, Query: in.Query, Matches: converted}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("构造 search_code 工具失败: %w", err)
	}

	readFile, err := utils.InferTool(
		ToolReadFile,
		"按行读取当前代码仓库安全索引中的文本文件；返回行号并自动将过大区间截成最多 200 行的一页，通过 next_start_line 继续读取。禁止绝对路径、路径穿越和被排除文件。",
		func(ctx context.Context, in readFileInput) (readOutput, error) {
			snapshot, files, err := manager.ListFiles(ctx, in.Path, 1)
			if err != nil {
				return readOutput{}, fmt.Errorf("读取仓库文件信息失败: %w", err)
			}
			if len(files) == 0 {
				return readOutput{}, fmt.Errorf("文件 %q 不在安全索引中，可能不存在或已被排除", in.Path)
			}
			file := files[0]
			requestedPath := filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(in.Path))))
			if file.Path != requestedPath {
				return readOutput{}, fmt.Errorf("路径 %q 不是安全索引中的文件", in.Path)
			}
			start := in.StartLine
			if start <= 0 {
				start = 1
			}
			if start > file.Lines {
				return readOutput{
					DataSource: "local_repository", Snapshot: snapshot, Status: "eof",
					Message: fmt.Sprintf("start_line=%d 超过文件总行数 %d", start, file.Lines),
					Path:    file.Path, TotalLines: file.Lines, Lines: []repository.SourceLine{},
				}, nil
			}
			requestedEnd := in.EndLine
			if requestedEnd <= 0 {
				requestedEnd = start + 119
			}
			if requestedEnd < start {
				return readOutput{
					DataSource: "local_repository", Snapshot: snapshot, Status: "invalid_range",
					Message: "end_line 不能小于 start_line", Path: file.Path, TotalLines: file.Lines,
					Lines: []repository.SourceLine{},
				}, nil
			}
			pageEnd := min(requestedEnd, start+199)
			actualEnd := min(pageEnd, file.Lines)
			content, err := backend.Read(ctx, &filesystem.ReadRequest{
				FilePath: file.Path, Offset: start, Limit: actualEnd - start + 1,
			})
			if err != nil {
				return readOutput{}, fmt.Errorf("读取仓库文件失败: %w", err)
			}
			textLines := strings.Split(content.Content, "\n")
			lines := make([]repository.SourceLine, 0, len(textLines))
			for offset, text := range textLines {
				lines = append(lines, repository.SourceLine{Line: start + offset, Text: text})
			}
			hasMore := actualEnd < file.Lines
			nextStart := 0
			if hasMore {
				nextStart = actualEnd + 1
			}
			return readOutput{
				DataSource: "local_repository", Snapshot: snapshot, Status: "ok", Path: file.Path,
				StartLine: start, EndLine: actualEnd, TotalLines: file.Lines,
				Truncated: requestedEnd > pageEnd && file.Lines > pageEnd,
				HasMore:   hasMore, NextStartLine: nextStart, Lines: lines,
			}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("构造 read_file 工具失败: %w", err)
	}

	findSymbol, err := utils.InferTool(
		ToolFindSymbol,
		"使用 Go AST 在当前代码仓库中查找函数、方法、类型、常量和变量定义，返回准确的 path:line。",
		func(ctx context.Context, in symbolInput) (symbolOutput, error) {
			snapshot, symbols, err := manager.FindSymbols(ctx, in.Name, in.Limit)
			if err != nil {
				return symbolOutput{}, fmt.Errorf("查找 Go 符号失败: %w", err)
			}
			return symbolOutput{DataSource: "local_repository", Snapshot: snapshot, Symbols: symbols}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("构造 find_symbol 工具失败: %w", err)
	}

	findReferences, err := utils.InferTool(
		ToolFindReferences,
		"使用 Go AST 查找标识符引用或调用位置，返回调用所在函数和 path:line；首版为语法级索引，同名符号需结合定义核对。",
		func(ctx context.Context, in referencesInput) (referencesOutput, error) {
			snapshot, references, err := manager.FindReferences(ctx, in.Name, in.CallsOnly, in.Limit)
			if err != nil {
				return referencesOutput{}, fmt.Errorf("查找 Go 引用失败: %w", err)
			}
			return referencesOutput{
				DataSource: "local_repository", Snapshot: snapshot, References: references,
				Limitation: "首版是 AST 语法级引用索引；不同包或接收者的同名符号需要结合 definition 和 expression 核对。",
			}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("构造 find_references 工具失败: %w", err)
	}

	getDefinition, err := utils.InferTool(
		ToolGetDefinition,
		"获取 Go 符号定义及带行号的源码片段，用于形成可验证的 path:line 引用。",
		func(ctx context.Context, in definitionInput) (definitionOutput, error) {
			contextLines := in.ContextLines
			if contextLines < 0 {
				contextLines = 0
			} else if contextLines > 20 {
				contextLines = 20
			}
			snapshot, definitions, err := manager.Definitions(ctx, in.Name, contextLines, in.Limit)
			if err != nil {
				return definitionOutput{}, fmt.Errorf("读取 Go 符号定义失败: %w", err)
			}
			return definitionOutput{DataSource: "local_repository", Snapshot: snapshot, Definitions: definitions}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("构造 get_definition 工具失败: %w", err)
	}

	getRevision, err := utils.InferTool(
		ToolGetRepositoryRevision,
		"返回当前代码仓库的索引 commit、当前 commit、分支、索引版本、明确的过期原因和相关文件路径；不执行 Git 命令。只有 stale_reasons 包含 git_commit_changed 才表示提交变化，source_file_* 表示 stale_paths 中的工作区文件在索引后发生变化。",
		func(ctx context.Context, _ revisionInput) (revisionOutput, error) {
			snapshot, err := manager.Revision(ctx)
			if err != nil {
				return revisionOutput{}, fmt.Errorf("读取仓库版本失败: %w", err)
			}
			return revisionOutput{DataSource: "local_repository", Snapshot: snapshot}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("构造 get_repository_revision 工具失败: %w", err)
	}

	return []tool.BaseTool{listFiles, searchCode, readFile, findSymbol, findReferences, getDefinition, getRevision}, nil
}

func limitSlice[T any](items []T, limit int) []T {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	if len(items) <= limit {
		return items
	}
	return items[:limit]
}
