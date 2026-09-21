package main

// agentcontext_test.go — invariants for the agent-context files (AGENTS.md and
// .claude/skills). Agents follow these files literally and nothing else reviews
// them, so the properties that make them trustworthy are asserted here rather
// than left to a reader: the root file stays inside its length budget, every
// repo path it cites resolves, each skill's frontmatter matches its directory,
// and any CLAUDE.md stays a pointer instead of a second source of truth.

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const agentContextLineCap = 200

// backtickedToken matches the inline-code spans agent-context files use to cite
// paths, commands and identifiers.
var backtickedToken = regexp.MustCompile("`([^`\n]+)`")

func TestAgentsFileStaysWithinLengthBudget(t *testing.T) {
	content, err := os.ReadFile("AGENTS.md")
	if err != nil {
		t.Fatalf("reading AGENTS.md: %v", err)
	}
	if lines := strings.Count(string(content), "\n"); lines > agentContextLineCap {
		t.Fatalf("AGENTS.md is %d lines, over the %d-line cap; push depth into .claude/skills/ or a nested AGENTS.md", lines, agentContextLineCap)
	}
}

// TestAgentContextFilesCiteResolvablePaths guards the failure the fleet has
// already shipped: a path that reads plausibly, names a real root directory, and
// resolves to nothing. Only tokens whose first segment is an existing root entry
// are checked, because that is exactly the class whose miss looks like a deleted
// file rather than a wrong citation; git refs, bare filenames and paths in other
// repos are deliberately out of scope.
func TestAgentContextFilesCiteResolvablePaths(t *testing.T) {
	roots, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading repository root: %v", err)
	}
	rootNames := map[string]bool{}
	for _, entry := range roots {
		rootNames[entry.Name()] = true
	}

	for _, file := range agentContextFiles(t) {
		content, errRead := os.ReadFile(file)
		if errRead != nil {
			t.Fatalf("reading %s: %v", file, errRead)
		}
		for _, match := range backtickedToken.FindAllStringSubmatch(string(content), -1) {
			token := match[1]
			if strings.ContainsAny(token, " \t") {
				continue
			}
			first, _, hasSeparator := strings.Cut(token, "/")
			if !hasSeparator || !rootNames[first] {
				continue
			}
			candidate := strings.TrimSuffix(token, "/")
			if _, errStat := os.Stat(candidate); errStat != nil {
				t.Errorf("%s cites %q, which does not resolve from the repository root", file, token)
			}
		}
	}
}

func TestSkillFrontmatterMatchesDirectory(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(".claude", "skills"))
	if err != nil {
		t.Fatalf("reading .claude/skills: %v", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(".claude", "skills", entry.Name(), "SKILL.md")
		name, description, errFrontmatter := skillFrontmatter(path)
		if errFrontmatter != nil {
			t.Errorf("%s: %v", path, errFrontmatter)
			continue
		}
		if name != entry.Name() {
			t.Errorf("%s declares name %q but lives in directory %q", path, name, entry.Name())
		}
		// The description is all an agent sees before deciding whether to load
		// the skill, so an empty or truncated one makes the skill unreachable.
		if description == "" {
			t.Errorf("%s has no description; it must say what the skill does and when to use it", path)
		}
		if len(description) > 1024 {
			t.Errorf("%s description is %d characters, over the 1024 limit", path, len(description))
		}
	}
}

// TestClaudeFileStaysAPointer keeps one canonical source. CLAUDE.md is absent
// today; if one is added it must point at AGENTS.md rather than duplicate it.
func TestClaudeFileStaysAPointer(t *testing.T) {
	content, err := os.ReadFile("CLAUDE.md")
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatalf("reading CLAUDE.md: %v", err)
	}
	if strings.TrimSpace(string(content)) != "@AGENTS.md" {
		t.Errorf("CLAUDE.md must be exactly the pointer @AGENTS.md, keeping AGENTS.md the one canonical source")
	}
}

func agentContextFiles(t *testing.T) []string {
	t.Helper()
	files := []string{"AGENTS.md"}
	err := filepath.WalkDir(filepath.Join(".claude", "skills"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(path, ".md") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking .claude/skills: %v", err)
	}
	return files
}

func skillFrontmatter(path string) (name string, description string, err error) {
	file, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "---" {
		return "", "", fmt.Errorf("must open with a --- frontmatter block")
	}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "---" {
			if name == "" {
				return "", "", fmt.Errorf("frontmatter declares no name")
			}
			return name, description, nil
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		switch strings.TrimSpace(key) {
		case "name":
			name = strings.TrimSpace(value)
		case "description":
			description = strings.TrimSpace(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", "", err
	}
	return "", "", fmt.Errorf("frontmatter block is never closed")
}
