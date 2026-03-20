// Package workspace manages per-issue workspace directories.
package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"
)

// Manager handles workspace lifecycle.
type Manager struct {
	root      string
	hooks     Hooks
	timeoutMS int
}

// Hooks contains workspace lifecycle hook scripts.
type Hooks struct {
	AfterCreate  string
	BeforeRun    string
	AfterRun     string
	BeforeRemove string
}

// Workspace represents a per-issue workspace.
type Workspace struct {
	Path       string
	Key        string
	CreatedNow bool
}

// NewManager creates a new workspace manager.
func NewManager(root string, hooks Hooks, timeoutMS int) *Manager {
	if timeoutMS <= 0 {
		timeoutMS = 60000
	}
	return &Manager{
		root:      root,
		hooks:     hooks,
		timeoutMS: timeoutMS,
	}
}

// sanitizeRegex matches characters that are not allowed in workspace keys.
var sanitizeRegex = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// SanitizeKey converts an issue identifier to a safe workspace key.
func SanitizeKey(identifier string) string {
	return sanitizeRegex.ReplaceAllString(identifier, "_")
}

// Create creates or reuses a workspace for an issue.
func (m *Manager) Create(ctx context.Context, identifier string) (*Workspace, error) {
	key := SanitizeKey(identifier)
	path := filepath.Join(m.root, key)

	// Validate path is within root
	absRoot, err := filepath.Abs(m.root)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve workspace root: %w", err)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve workspace path: %w", err)
	}
	if !filepath.HasPrefix(absPath, absRoot) {
		return nil, fmt.Errorf("workspace path %s escapes root %s", absPath, absRoot)
	}

	// Check if workspace exists
	info, err := os.Stat(path)
	createdNow := false

	if os.IsNotExist(err) {
		// Create workspace directory
		if err := os.MkdirAll(path, 0755); err != nil {
			return nil, fmt.Errorf("failed to create workspace: %w", err)
		}
		createdNow = true

		// Run after_create hook
		if m.hooks.AfterCreate != "" {
			if err := m.runHook(ctx, path, m.hooks.AfterCreate); err != nil {
				// Remove partially created workspace
				os.RemoveAll(path)
				return nil, fmt.Errorf("after_create hook failed: %w", err)
			}
		}
	} else if err != nil {
		return nil, fmt.Errorf("failed to check workspace: %w", err)
	} else if !info.IsDir() {
		// Path exists but is not a directory
		return nil, fmt.Errorf("workspace path exists but is not a directory: %s", path)
	}

	return &Workspace{
		Path:       path,
		Key:        key,
		CreatedNow: createdNow,
	}, nil
}

// Prepare runs before_run hook if configured.
func (m *Manager) Prepare(ctx context.Context, ws *Workspace) error {
	if m.hooks.BeforeRun != "" {
		if err := m.runHook(ctx, ws.Path, m.hooks.BeforeRun); err != nil {
			return fmt.Errorf("before_run hook failed: %w", err)
		}
	}
	return nil
}

// Finish runs after_run hook if configured.
func (m *Manager) Finish(ctx context.Context, ws *Workspace) {
	if m.hooks.AfterRun != "" {
		// Errors are logged but ignored
		_ = m.runHook(ctx, ws.Path, m.hooks.AfterRun)
	}
}

// Remove removes a workspace directory.
func (m *Manager) Remove(ctx context.Context, identifier string) error {
	key := SanitizeKey(identifier)
	path := filepath.Join(m.root, key)

	// Check if workspace exists
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}

	// Run before_remove hook (errors ignored)
	if m.hooks.BeforeRemove != "" {
		_ = m.runHook(ctx, path, m.hooks.BeforeRemove)
	}

	// Remove the workspace
	return os.RemoveAll(path)
}

// CleanTempFiles removes temporary artifacts from a workspace.
func (m *Manager) CleanTempFiles(ws *Workspace) error {
	tempDirs := []string{"tmp", ".elixir_ls", "__pycache__", "node_modules/.cache"}
	for _, dir := range tempDirs {
		path := filepath.Join(ws.Path, dir)
		if _, err := os.Stat(path); err == nil {
			os.RemoveAll(path)
		}
	}
	return nil
}

func (m *Manager) runHook(ctx context.Context, workdir, script string) error {
	timeout := time.Duration(m.timeoutMS) * time.Millisecond
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-lc", script)
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(), "WORKSPACE="+workdir)

	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("hook timed out after %v: %s", timeout, string(output))
	}
	if err != nil {
		return fmt.Errorf("hook failed: %w: %s", err, string(output))
	}
	return nil
}

// EnsureRoot ensures the workspace root directory exists.
func (m *Manager) EnsureRoot() error {
	return os.MkdirAll(m.root, 0755)
}

// Root returns the workspace root path.
func (m *Manager) Root() string {
	return m.root
}

// ListWorkspaces returns all workspace keys in the root.
func (m *Manager) ListWorkspaces() ([]string, error) {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var keys []string
	for _, entry := range entries {
		if entry.IsDir() {
			keys = append(keys, entry.Name())
		}
	}
	return keys, nil
}
