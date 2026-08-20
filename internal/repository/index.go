package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxIndexedFileBytes = 2 * 1024 * 1024
	maxReadLines        = 200
	maxResultLimit      = 100
)

type indexedFile struct {
	info       FileInfo
	lines      []string
	modTimeNS  int64
	symbols    []Symbol
	references []Reference
}

type index struct {
	indexedAt    time.Time
	files        map[string]*indexedFile
	ignoredFiles map[string]struct{}
	paths        []string
	symbols      []Symbol
	references   []Reference
}

func buildIndex(ctx context.Context, root string, previous *index, ignoredFiles map[string]struct{}) (*index, error) {
	idx := &index{
		indexedAt: time.Now(), files: make(map[string]*indexedFile), ignoredFiles: ignoredFiles,
	}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if excludedDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if excludedFile(rel) || ignoredFile(rel, ignoredFiles) {
			return nil
		}
		stat, err := entry.Info()
		if err != nil {
			return err
		}
		if !stat.Mode().IsRegular() || stat.Size() > maxIndexedFileBytes {
			return nil
		}
		if previous != nil {
			if old := previous.files[rel]; old != nil && old.info.Bytes == stat.Size() && old.modTimeNS == stat.ModTime().UnixNano() {
				idx.files[rel] = old
				return nil
			}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !isTextFile(rel, data) {
			return nil
		}
		doc := newIndexedFile(rel, data, stat)
		idx.files[rel] = doc
		return nil
	})
	if err != nil {
		return nil, err
	}

	idx.paths = make([]string, 0, len(idx.files))
	for path, file := range idx.files {
		idx.paths = append(idx.paths, path)
		idx.symbols = append(idx.symbols, file.symbols...)
		idx.references = append(idx.references, file.references...)
	}
	sort.Strings(idx.paths)
	sort.Slice(idx.symbols, func(i, j int) bool {
		if idx.symbols[i].QualifiedName != idx.symbols[j].QualifiedName {
			return idx.symbols[i].QualifiedName < idx.symbols[j].QualifiedName
		}
		return locationLess(idx.symbols[i].Location, idx.symbols[j].Location)
	})
	sort.Slice(idx.references, func(i, j int) bool {
		if idx.references[i].Name != idx.references[j].Name {
			return idx.references[i].Name < idx.references[j].Name
		}
		return locationLess(idx.references[i].Location, idx.references[j].Location)
	})
	return idx, nil
}

func newIndexedFile(path string, data []byte, stat os.FileInfo) *indexedFile {
	sum := sha256.Sum256(data)
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	doc := &indexedFile{
		info: FileInfo{
			Path: path, Language: languageForPath(path), Bytes: stat.Size(), Lines: len(lines),
			Hash: hex.EncodeToString(sum[:]), ModifiedAt: stat.ModTime(),
		},
		lines:     lines,
		modTimeNS: stat.ModTime().UnixNano(),
	}
	if strings.EqualFold(filepath.Ext(path), ".go") {
		doc.symbols, doc.references = parseGoFile(path, data)
	}
	return doc
}

func parseGoFile(path string, data []byte) ([]Symbol, []Reference) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, data, parser.ParseComments|parser.AllErrors)
	if file == nil {
		return nil, nil
	}
	_ = err // 部分语法树仍可为错误截图对应的未完成代码提供定义和引用。
	packageName := file.Name.Name
	definitions := make(map[token.Pos]struct{})
	markDefinitions(file, definitions)

	symbols := []Symbol{{
		Name: packageName, QualifiedName: packageName, Kind: "package", Package: packageName,
		Documentation: commentText(file.Doc), Location: nodeLocation(fset, path, file.Name),
	}}
	for _, decl := range file.Decls {
		switch node := decl.(type) {
		case *ast.FuncDecl:
			receiver := receiverName(node.Recv)
			qualified := node.Name.Name
			kind := "function"
			if receiver != "" {
				qualified = receiver + "." + node.Name.Name
				kind = "method"
			}
			symbols = append(symbols, Symbol{
				Name: node.Name.Name, QualifiedName: qualified, Kind: kind, Package: packageName,
				Receiver: receiver, Signature: "func" + renderNode(fset, node.Type),
				Documentation: commentText(node.Doc), Location: nodeLocation(fset, path, node),
			})
		case *ast.GenDecl:
			kind := ""
			switch node.Tok {
			case token.TYPE:
				kind = "type"
			case token.CONST:
				kind = "const"
			case token.VAR:
				kind = "var"
			}
			if kind == "" {
				continue
			}
			for _, spec := range node.Specs {
				switch item := spec.(type) {
				case *ast.TypeSpec:
					documentation := commentText(item.Doc)
					if documentation == "" {
						documentation = commentText(node.Doc)
					}
					symbols = append(symbols, Symbol{
						Name: item.Name.Name, QualifiedName: item.Name.Name, Kind: kind, Package: packageName,
						Documentation: documentation, Location: nodeLocation(fset, path, item),
					})
				case *ast.ValueSpec:
					documentation := commentText(item.Doc)
					if documentation == "" {
						documentation = commentText(node.Doc)
					}
					for _, name := range item.Names {
						symbols = append(symbols, Symbol{
							Name: name.Name, QualifiedName: name.Name, Kind: kind, Package: packageName,
							Documentation: documentation, Location: nodeLocation(fset, path, item),
						})
					}
				}
			}
		}
	}

	callSites := make(map[token.Pos]string)
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if pos, expression, ok := callName(call.Fun, fset); ok {
			callSites[pos] = expression
		}
		return true
	})
	var references []Reference
	ast.Inspect(file, func(node ast.Node) bool {
		ident, ok := node.(*ast.Ident)
		if !ok || ident.Name == "_" {
			return true
		}
		if _, defined := definitions[ident.Pos()]; defined {
			return true
		}
		kind := "reference"
		expression := ""
		if call, ok := callSites[ident.Pos()]; ok {
			kind = "call"
			expression = call
		}
		references = append(references, Reference{
			Name: ident.Name, Expression: expression, Kind: kind,
			InSymbol: enclosingSymbol(symbols, path, fset.Position(ident.Pos()).Line),
			Location: Location{Path: path, Line: fset.Position(ident.Pos()).Line},
		})
		return true
	})
	return symbols, references
}

func markDefinitions(file *ast.File, definitions map[token.Pos]struct{}) {
	mark := func(id *ast.Ident) {
		if id != nil {
			definitions[id.Pos()] = struct{}{}
		}
	}
	mark(file.Name)
	ast.Inspect(file, func(node ast.Node) bool {
		switch item := node.(type) {
		case *ast.FuncDecl:
			mark(item.Name)
		case *ast.TypeSpec:
			mark(item.Name)
		case *ast.ValueSpec:
			for _, name := range item.Names {
				mark(name)
			}
		case *ast.Field:
			for _, name := range item.Names {
				mark(name)
			}
		case *ast.ImportSpec:
			mark(item.Name)
		case *ast.AssignStmt:
			if item.Tok == token.DEFINE {
				for _, expr := range item.Lhs {
					if id, ok := expr.(*ast.Ident); ok {
						mark(id)
					}
				}
			}
		case *ast.RangeStmt:
			if item.Tok == token.DEFINE {
				if id, ok := item.Key.(*ast.Ident); ok {
					mark(id)
				}
				if id, ok := item.Value.(*ast.Ident); ok {
					mark(id)
				}
			}
		case *ast.LabeledStmt:
			mark(item.Label)
		}
		return true
	})
}

func (m *Manager) ListFiles(ctx context.Context, prefix string, limit int) (Snapshot, []FileInfo, error) {
	info, idx, err := m.activeIndex(ctx)
	if err != nil {
		return Snapshot{}, nil, err
	}
	prefix, err = cleanRelativePrefix(prefix)
	if err != nil {
		return Snapshot{}, nil, err
	}
	limit = normalizedLimit(limit)
	files := make([]FileInfo, 0, min(limit, len(idx.paths)))
	for _, path := range idx.paths {
		if prefix != "" && path != prefix && !strings.HasPrefix(path, prefix+"/") {
			continue
		}
		files = append(files, idx.files[path].info)
		if len(files) == limit {
			break
		}
	}
	return m.snapshot(ctx, info, idx), files, nil
}

func (m *Manager) Search(ctx context.Context, query, prefix string, caseSensitive bool, limit int) (Snapshot, []SearchMatch, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return Snapshot{}, nil, errors.New("搜索内容不能为空")
	}
	info, idx, err := m.activeIndex(ctx)
	if err != nil {
		return Snapshot{}, nil, err
	}
	prefix, err = cleanRelativePrefix(prefix)
	if err != nil {
		return Snapshot{}, nil, err
	}
	limit = normalizedLimit(limit)
	needle := query
	if !caseSensitive {
		needle = strings.ToLower(needle)
	}
	result := make([]SearchMatch, 0, limit)
	for _, path := range idx.paths {
		if prefix != "" && path != prefix && !strings.HasPrefix(path, prefix+"/") {
			continue
		}
		for lineNo, line := range idx.files[path].lines {
			haystack := line
			if !caseSensitive {
				haystack = strings.ToLower(haystack)
			}
			if strings.Contains(haystack, needle) {
				result = append(result, SearchMatch{Path: path, Line: lineNo + 1, Text: truncateLine(line, 800)})
				if len(result) == limit {
					return m.snapshot(ctx, info, idx), result, nil
				}
			}
		}
	}
	return m.snapshot(ctx, info, idx), result, nil
}

// Grep 使用 Go 正则表达式在安全索引内逐行搜索，供 Eino filesystem.Backend 使用。
func (m *Manager) Grep(ctx context.Context, pattern, prefix string, caseInsensitive bool, limit int) (Snapshot, []SearchMatch, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return Snapshot{}, nil, errors.New("搜索表达式不能为空")
	}
	if caseInsensitive {
		pattern = "(?i)" + pattern
	}
	expression, err := regexp.Compile(pattern)
	if err != nil {
		return Snapshot{}, nil, fmt.Errorf("非法搜索表达式: %w", err)
	}
	info, idx, err := m.activeIndex(ctx)
	if err != nil {
		return Snapshot{}, nil, err
	}
	prefix, err = cleanRelativePrefix(prefix)
	if err != nil {
		return Snapshot{}, nil, err
	}
	limit = normalizedLimit(limit)
	result := make([]SearchMatch, 0, limit)
	for _, filePath := range idx.paths {
		if prefix != "" && filePath != prefix && !strings.HasPrefix(filePath, prefix+"/") {
			continue
		}
		for lineNo, line := range idx.files[filePath].lines {
			if expression.MatchString(line) {
				result = append(result, SearchMatch{Path: filePath, Line: lineNo + 1, Text: truncateLine(line, 800)})
				if len(result) == limit {
					return m.snapshot(ctx, info, idx), result, nil
				}
			}
		}
	}
	return m.snapshot(ctx, info, idx), result, nil
}

func (m *Manager) ReadFile(ctx context.Context, path string, startLine, endLine int) (Snapshot, []SourceLine, error) {
	info, idx, err := m.activeIndex(ctx)
	if err != nil {
		return Snapshot{}, nil, err
	}
	path, err = cleanRelativeFile(path)
	if err != nil {
		return Snapshot{}, nil, err
	}
	doc := idx.files[path]
	if doc == nil {
		return Snapshot{}, nil, fmt.Errorf("文件 %q 不在安全索引中，可能不存在或已被排除", path)
	}
	startLine, endLine, err = lineRange(startLine, endLine, len(doc.lines))
	if err != nil {
		return Snapshot{}, nil, err
	}
	lines := make([]SourceLine, 0, endLine-startLine+1)
	for line := startLine; line <= endLine; line++ {
		lines = append(lines, SourceLine{Line: line, Text: truncateLine(doc.lines[line-1], 4000)})
	}
	return m.snapshot(ctx, info, idx), lines, nil
}

func (m *Manager) FindSymbols(ctx context.Context, name string, limit int) (Snapshot, []Symbol, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Snapshot{}, nil, errors.New("符号名称不能为空")
	}
	info, idx, err := m.activeIndex(ctx)
	if err != nil {
		return Snapshot{}, nil, err
	}
	limit = normalizedLimit(limit)
	result := make([]Symbol, 0, limit)
	for _, symbol := range idx.symbols {
		if symbol.Name == name || symbol.QualifiedName == name {
			result = append(result, symbol)
			if len(result) == limit {
				break
			}
		}
	}
	return m.snapshot(ctx, info, idx), result, nil
}

func (m *Manager) FindReferences(ctx context.Context, name string, callsOnly bool, limit int) (Snapshot, []Reference, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Snapshot{}, nil, errors.New("符号名称不能为空")
	}
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		name = name[dot+1:]
	}
	info, idx, err := m.activeIndex(ctx)
	if err != nil {
		return Snapshot{}, nil, err
	}
	limit = normalizedLimit(limit)
	result := make([]Reference, 0, limit)
	for _, reference := range idx.references {
		if reference.Name != name || callsOnly && reference.Kind != "call" {
			continue
		}
		result = append(result, reference)
		if len(result) == limit {
			break
		}
	}
	return m.snapshot(ctx, info, idx), result, nil
}

func (m *Manager) Definitions(ctx context.Context, name string, contextLines, limit int) (Snapshot, []Definition, error) {
	snapshot, symbols, err := m.FindSymbols(ctx, name, limit)
	if err != nil {
		return Snapshot{}, nil, err
	}
	if contextLines < 0 || contextLines > 20 {
		return Snapshot{}, nil, errors.New("context_lines 必须在 0 到 20 之间")
	}
	_, idx, err := m.activeIndex(ctx)
	if err != nil {
		return Snapshot{}, nil, err
	}
	definitions := make([]Definition, 0, len(symbols))
	for _, symbol := range symbols {
		doc := idx.files[symbol.Location.Path]
		if doc == nil {
			continue
		}
		start := max(1, symbol.Location.Line-contextLines)
		end := min(len(doc.lines), symbol.Location.EndLine+contextLines)
		if end-start+1 > maxReadLines {
			end = start + maxReadLines - 1
		}
		lines := make([]SourceLine, 0, end-start+1)
		for line := start; line <= end; line++ {
			lines = append(lines, SourceLine{Line: line, Text: truncateLine(doc.lines[line-1], 4000)})
		}
		definitions = append(definitions, Definition{Symbol: symbol, Lines: lines})
	}
	return snapshot, definitions, nil
}

func (m *Manager) Revision(ctx context.Context) (Snapshot, error) {
	info, idx, err := m.activeIndex(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	return m.snapshot(ctx, info, idx), nil
}

func indexFileChanges(ctx context.Context, root string, idx *index) ([]StaleReason, []string) {
	reasons := make(map[StaleReason]struct{})
	paths := make(map[string]struct{})
	seen := make(map[string]struct{}, len(idx.files))
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if excludedDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if excludedFile(rel) || ignoredFile(rel, idx.ignoredFiles) {
			return nil
		}
		stat, err := entry.Info()
		if err != nil {
			return err
		}
		if !stat.Mode().IsRegular() || stat.Size() > maxIndexedFileBytes {
			return nil
		}
		if old := idx.files[rel]; old != nil {
			seen[rel] = struct{}{}
			if stat.Size() != old.info.Bytes || stat.ModTime().UnixNano() != old.modTimeNS {
				reasons[StaleSourceFileChanged] = struct{}{}
				paths[rel] = struct{}{}
			}
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if isTextFile(rel, data) {
			reasons[StaleSourceFileAdded] = struct{}{}
			paths[rel] = struct{}{}
		}
		return nil
	})
	if err != nil {
		reasons[StaleRepositoryScanFailed] = struct{}{}
	} else {
		for path := range idx.files {
			if _, ok := seen[path]; !ok {
				reasons[StaleSourceFileRemoved] = struct{}{}
				paths[path] = struct{}{}
			}
		}
	}
	changedPaths := make([]string, 0, len(paths))
	for path := range paths {
		changedPaths = append(changedPaths, path)
	}
	sort.Strings(changedPaths)
	return orderedStaleReasons(reasons), changedPaths
}

func ignoredFile(path string, ignored map[string]struct{}) bool {
	if _, ok := ignored[path]; ok {
		return true
	}
	dir := filepath.ToSlash(filepath.Dir(filepath.FromSlash(path)))
	base := filepath.Base(filepath.FromSlash(path))
	for ignoredPath := range ignored {
		ignoredDir := filepath.ToSlash(filepath.Dir(filepath.FromSlash(ignoredPath)))
		ignoredBase := filepath.Base(filepath.FromSlash(ignoredPath))
		if dir == ignoredDir && strings.HasPrefix(base, "."+ignoredBase+".tmp-") {
			return true
		}
	}
	return false
}

func orderedStaleReasons(found map[StaleReason]struct{}) []StaleReason {
	order := []StaleReason{
		StaleGitCommitChanged,
		StaleSourceFileAdded,
		StaleSourceFileChanged,
		StaleSourceFileRemoved,
		StaleRepositoryScanFailed,
	}
	result := make([]StaleReason, 0, len(found))
	for _, reason := range order {
		if _, ok := found[reason]; ok {
			result = append(result, reason)
		}
	}
	return result
}

func excludedDir(name string) bool {
	switch strings.ToLower(name) {
	// .checkpoints 里是中断快照，含本轮完整模型消息（可能包括截图 base64）。
	// 它和 .chat_history_sessions 同性质：属于本工具的运行状态，不是被诊断仓库的代码。
	case ".git", ".hg", ".svn", ".chat_history_sessions", ".checkpoints", "node_modules", "vendor", "bin", "dist", "build", "target", "coverage", ".cache", "__pycache__":
		return true
	default:
		return false
	}
}

func excludedFile(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	if base == ".env" || strings.HasPrefix(base, ".env.") || strings.HasSuffix(base, ".log") ||
		base == ".netrc" || base == ".npmrc" || base == ".pypirc" || base == "credentials" ||
		base == "credentials.json" || strings.HasPrefix(base, ".chat_history") ||
		base == ".repositories.json" || base == ".approvals.json" ||
		strings.HasPrefix(base, "..repositories.json.tmp-") ||
		strings.HasPrefix(base, "..approvals.json.tmp-") ||
		base == "audit.log" || base == "diagnostic.log" || base == "id_rsa" || base == "id_ed25519" {
		return true
	}
	switch strings.ToLower(filepath.Ext(base)) {
	// .ckpt 是中断快照。目录名可由 AGENT_CHECKPOINT_DIR 改掉，所以除了排除
	// 默认目录，后缀本身也要挡一道，快照落到哪儿都不会被当成源码索引。
	case ".pem", ".key", ".p12", ".pfx", ".der", ".crt", ".ckpt":
		return true
	default:
		return false
	}
}

func isTextFile(path string, data []byte) bool {
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return false
	}
	head := data
	if len(head) > 4096 {
		head = head[:4096]
	}
	upper := bytes.ToUpper(head)
	if bytes.Contains(upper, []byte("-----BEGIN PRIVATE KEY-----")) ||
		bytes.Contains(upper, []byte("-----BEGIN RSA PRIVATE KEY-----")) ||
		bytes.Contains(upper, []byte("-----BEGIN OPENSSH PRIVATE KEY-----")) {
		return false
	}
	return true
}

func languageForPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".sh", ".bash", ".zsh":
		return "shell"
	case ".yaml", ".yml":
		return "yaml"
	case ".json":
		return "json"
	case ".md", ".markdown":
		return "markdown"
	case ".toml":
		return "toml"
	case ".xml":
		return "xml"
	case ".sql":
		return "sql"
	case ".proto":
		return "protobuf"
	default:
		return "text"
	}
}

func cleanRelativePrefix(path string) (string, error) {
	path = strings.TrimSpace(filepath.ToSlash(path))
	if path == "" || path == "." {
		return "", nil
	}
	if strings.HasPrefix(path, "/") {
		return "", errors.New("路径必须是仓库内的相对路径")
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("路径不能离开仓库根目录")
	}
	return strings.TrimSuffix(clean, "/"), nil
}

func cleanRelativeFile(path string) (string, error) {
	clean, err := cleanRelativePrefix(path)
	if err != nil {
		return "", err
	}
	if clean == "" {
		return "", errors.New("文件路径不能为空")
	}
	return clean, nil
}

func normalizedLimit(limit int) int {
	if limit <= 0 {
		return 20
	}
	return min(limit, maxResultLimit)
}

func lineRange(start, end, total int) (int, int, error) {
	if total == 0 {
		return 0, 0, errors.New("文件为空")
	}
	if start <= 0 {
		start = 1
	}
	if end <= 0 {
		end = min(total, start+119)
	}
	if start > total || end < start {
		return 0, 0, fmt.Errorf("非法行范围 %d-%d，文件共 %d 行", start, end, total)
	}
	end = min(end, total)
	if end-start+1 > maxReadLines {
		return 0, 0, fmt.Errorf("单次最多读取 %d 行", maxReadLines)
	}
	return start, end, nil
}

func nodeLocation(fset *token.FileSet, path string, node ast.Node) Location {
	return Location{Path: path, Line: fset.Position(node.Pos()).Line, EndLine: fset.Position(node.End()).Line}
}

func receiverName(fields *ast.FieldList) string {
	if fields == nil || len(fields.List) == 0 {
		return ""
	}
	switch expr := fields.List[0].Type.(type) {
	case *ast.Ident:
		return expr.Name
	case *ast.StarExpr:
		if id, ok := expr.X.(*ast.Ident); ok {
			return id.Name
		}
	case *ast.IndexExpr:
		if id, ok := expr.X.(*ast.Ident); ok {
			return id.Name
		}
	case *ast.IndexListExpr:
		if id, ok := expr.X.(*ast.Ident); ok {
			return id.Name
		}
	}
	return ""
}

func renderNode(fset *token.FileSet, node any) string {
	var out bytes.Buffer
	if err := format.Node(&out, fset, node); err != nil {
		return ""
	}
	return strings.Join(strings.Fields(out.String()), " ")
}

func commentText(group *ast.CommentGroup) string {
	if group == nil {
		return ""
	}
	return truncateLine(strings.TrimSpace(group.Text()), 2000)
}

func callName(expr ast.Expr, fset *token.FileSet) (token.Pos, string, bool) {
	switch node := expr.(type) {
	case *ast.Ident:
		return node.Pos(), node.Name, true
	case *ast.SelectorExpr:
		return node.Sel.Pos(), renderNode(fset, node), true
	case *ast.IndexExpr:
		return callName(node.X, fset)
	case *ast.IndexListExpr:
		return callName(node.X, fset)
	default:
		return token.NoPos, "", false
	}
}

func enclosingSymbol(symbols []Symbol, path string, line int) string {
	best := ""
	bestSpan := int(^uint(0) >> 1)
	for _, symbol := range symbols {
		if symbol.Location.Path != path || symbol.Kind != "function" && symbol.Kind != "method" ||
			line < symbol.Location.Line || line > symbol.Location.EndLine {
			continue
		}
		span := symbol.Location.EndLine - symbol.Location.Line
		if span < bestSpan {
			best = symbol.QualifiedName
			bestSpan = span
		}
	}
	return best
}

func locationLess(left, right Location) bool {
	if left.Path != right.Path {
		return left.Path < right.Path
	}
	return left.Line < right.Line
}

func truncateLine(line string, maxRunes int) string {
	runes := []rune(line)
	if len(runes) <= maxRunes {
		return line
	}
	return string(runes[:maxRunes]) + "..."
}
