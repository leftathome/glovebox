package routing

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func ValidateDestination(agent string, agentsDir string, allowlist []string) (string, error) {
	if !slices.Contains(allowlist, agent) {
		return "", fmt.Errorf("unknown_destination: %q not in allowlist", agent)
	}

	destPath := filepath.Join(agentsDir, agent, "workspace", "inbox")
	resolved, err := filepath.Abs(destPath)
	if err != nil {
		return "", fmt.Errorf("path_traversal: cannot resolve %q: %w", destPath, err)
	}

	// EvalSymlinks to prevent symlink-based escape.
	// Only eval if the path exists -- during validation the inbox may not exist yet.
	if _, statErr := os.Stat(resolved); statErr == nil {
		resolved, err = filepath.EvalSymlinks(resolved)
		if err != nil {
			return "", fmt.Errorf("path_traversal: cannot eval symlinks %q: %w", resolved, err)
		}
	}

	absAgentsDir, err := filepath.Abs(agentsDir)
	if err != nil {
		return "", fmt.Errorf("path_traversal: cannot resolve agents dir: %w", err)
	}
	if _, statErr := os.Stat(absAgentsDir); statErr == nil {
		absAgentsDir, err = filepath.EvalSymlinks(absAgentsDir)
		if err != nil {
			return "", fmt.Errorf("path_traversal: cannot eval symlinks on agents dir: %w", err)
		}
	}

	if !strings.HasPrefix(resolved, absAgentsDir+string(filepath.Separator)) && resolved != absAgentsDir {
		return "", fmt.Errorf("path_traversal: resolved path %q escapes agents dir %q", resolved, absAgentsDir)
	}

	return resolved, nil
}
