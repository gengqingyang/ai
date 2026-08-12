// Package repository 提供本地代码仓库的安全只读索引和查询能力。
package repository

import "time"

const IndexVersion = 1

// Info 是仓库目录中持久化的稳定元数据。
type Info struct {
	Name         string    `json:"name"`
	Root         string    `json:"root"`
	GitCommit    string    `json:"git_commit,omitempty"`
	GitBranch    string    `json:"git_branch,omitempty"`
	IndexVersion int       `json:"index_version"`
	IndexedAt    time.Time `json:"indexed_at"`
	FileCount    int       `json:"file_count"`
	SymbolCount  int       `json:"symbol_count"`
	Active       bool      `json:"active,omitempty"`
}

// Snapshot 标识一次查询使用的仓库和索引版本。
type Snapshot struct {
	Repository   string    `json:"repository"`
	GitCommit    string    `json:"git_commit,omitempty"`
	GitBranch    string    `json:"git_branch,omitempty"`
	IndexVersion int       `json:"index_version"`
	IndexedAt    time.Time `json:"indexed_at"`
	Stale        bool      `json:"stale"`
}

// FileInfo 是已经通过安全扫描的文件元数据。
type FileInfo struct {
	Path       string    `json:"path"`
	Language   string    `json:"language"`
	Bytes      int64     `json:"bytes"`
	Lines      int       `json:"lines"`
	Hash       string    `json:"hash"`
	ModifiedAt time.Time `json:"modified_at"`
}

// Location 是可直接引用的仓库相对位置。
type Location struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	EndLine int    `json:"end_line,omitempty"`
}

// Symbol 描述 Go AST 中的一项定义。
type Symbol struct {
	Name          string   `json:"name"`
	QualifiedName string   `json:"qualified_name"`
	Kind          string   `json:"kind"`
	Package       string   `json:"package"`
	Receiver      string   `json:"receiver,omitempty"`
	Signature     string   `json:"signature,omitempty"`
	Documentation string   `json:"documentation,omitempty"`
	Location      Location `json:"location"`
}

// Reference 描述标识符引用或函数调用位置。
type Reference struct {
	Name       string   `json:"name"`
	Expression string   `json:"expression,omitempty"`
	Kind       string   `json:"kind"`
	InSymbol   string   `json:"in_symbol,omitempty"`
	Location   Location `json:"location"`
}

// SearchMatch 是逐行精确文本检索结果。
type SearchMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// SourceLine 保留源码行号，供模型生成 path:line 引用。
type SourceLine struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

// Definition 是符号定义及其源码片段。
type Definition struct {
	Symbol Symbol       `json:"symbol"`
	Lines  []SourceLine `json:"lines"`
}
