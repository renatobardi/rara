package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// BuildWorkdir creates a task workdir under baseDir and populates it for Claude Code CLI:
//   - CLAUDE.md  — agent system prompt
//   - SKILL.md   — concatenation of all skill content (always written)
//   - skill files — written only for trusted skills (Trusted=true)
func BuildWorkdir(tc TaskCtx, baseDir string) (string, error) {
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
