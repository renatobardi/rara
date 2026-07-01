package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// BuildWorkdir creates a task workdir under baseDir and populates it for Claude Code CLI:
//   - CLAUDE.md  — agent system prompt
//   - SKILL.md   — concatenation of all skill content (always written)
//   - skill files — written only for trusted skills (Trusted=true)
func BuildWorkdir(tc TaskCtx, baseDir string) (string, error) {
	if err := os.MkdirAll(baseDir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir work base %q: %w", baseDir, err)
	}
	dir, err := os.MkdirTemp(baseDir, fmt.Sprintf("task-%d-*", tc.TaskID))
	if err != nil {
		return "", fmt.Errorf("mktemp: %w", err)
	}

	claudeMD := fmt.Sprintf("# Agent: %s\n\n%s\n", tc.Agent.Name, tc.Agent.Instructions)
	if err := writeWorkFile(dir, "CLAUDE.md", claudeMD); err != nil {
		return dir, err
	}

	var skillContents strings.Builder
	for _, skill := range tc.Skills {
		fmt.Fprintf(&skillContents, "# %s\n\n%s\n\n", skill.Name, skill.Content)
		if skill.Trusted {
			for _, f := range skill.Files {
				if err := writeWorkFile(dir, f.Path, f.Content); err != nil {
					return dir, fmt.Errorf("skill %d file %q: %w", skill.ID, f.Path, err)
				}
			}
		}
	}
	if skillContents.Len() > 0 {
		if err := writeWorkFile(dir, "SKILL.md", skillContents.String()); err != nil {
			return dir, err
		}
	}

	// Write MCP config so Claude Code picks up the agent's MCP servers.
	if len(tc.Agent.MCPConfig) > 0 && string(tc.Agent.MCPConfig) != "{}" {
		if err := writeWorkFile(dir, ".claude/settings.json", string(tc.Agent.MCPConfig)); err != nil {
			return dir, fmt.Errorf("write mcp config: %w", err)
		}
	}

	return dir, nil
}

// writeContextRefs resolves context_refs (JSON int array) into context/<id>.md files so the CLI
// can read curated content before running. Invalid ids are skipped; unavailable distillations are
// logged and skipped so a single missing ref can't abort an otherwise valid task.
func writeContextRefs(ctx context.Context, refs []byte, dir string, store Store) error {
	if len(refs) == 0 {
		return nil
	}
	var elements []json.RawMessage
	if err := json.Unmarshal(refs, &elements); err != nil {
		return fmt.Errorf("parse context_refs: %w", err)
	}
	for _, elem := range elements {
		id, err := strconv.Atoi(strings.TrimSpace(string(elem)))
		if err != nil || id <= 0 {
			continue // not a positive integer — skip silently
		}
		content, err := store.FetchDistillation(ctx, id)
		if err != nil {
			log.Printf("context_ref %d: %v", id, err)
			continue
		}
		if content == "" {
			continue
		}
		if err := writeWorkFile(dir, fmt.Sprintf("context/%d.md", id), content); err != nil {
			return fmt.Errorf("write context/%d.md: %w", id, err)
		}
	}
	return nil
}

// writeWorkFile writes content to name inside dir.
// Rejects absolute paths and path traversal (skill_files content is not fully trusted).
func writeWorkFile(dir, name, content string) error {
	clean := filepath.Clean(name)
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		return fmt.Errorf("invalid file path %q", name)
	}
	full := filepath.Join(dir, clean)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return fmt.Errorf("mkdir %q: %w", filepath.Dir(full), err)
	}
	return os.WriteFile(full, []byte(content), 0o600)
}
