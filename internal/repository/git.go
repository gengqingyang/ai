package repository

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

type gitInfo struct {
	Commit string
	Branch string
}

func readGitInfo(root string) gitInfo {
	gitDir := filepath.Join(root, ".git")
	if data, err := os.ReadFile(gitDir); err == nil {
		line := strings.TrimSpace(string(data))
		if strings.HasPrefix(line, "gitdir:") {
			candidate := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
			if !filepath.IsAbs(candidate) {
				candidate = filepath.Join(root, candidate)
			}
			gitDir = filepath.Clean(candidate)
		}
	}
	head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return gitInfo{}
	}
	value := strings.TrimSpace(string(head))
	if !strings.HasPrefix(value, "ref:") {
		return gitInfo{Commit: value}
	}
	ref := strings.TrimSpace(strings.TrimPrefix(value, "ref:"))
	info := gitInfo{Branch: strings.TrimPrefix(ref, "refs/heads/")}
	if data, err := os.ReadFile(filepath.Join(gitDir, filepath.FromSlash(ref))); err == nil {
		info.Commit = strings.TrimSpace(string(data))
		return info
	}
	packed, err := os.Open(filepath.Join(gitDir, "packed-refs"))
	if err != nil {
		return info
	}
	defer packed.Close()
	scanner := bufio.NewScanner(packed)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[1] == ref {
			info.Commit = fields[0]
			break
		}
	}
	return info
}
