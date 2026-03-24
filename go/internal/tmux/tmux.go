// Package tmux provides tmux session and pane management for dispatching agents.
package tmux

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// IsInsideSession returns true if the current process is running inside a tmux session.
func IsInsideSession() bool {
	return os.Getenv("TMUX") != ""
}

// PaneConfig describes how to create and populate a tmux pane for an issue.
type PaneConfig struct {
	// IssueName is the pane title (e.g. "NUM-10").
	IssueName string
	// InjectSequence is the ordered list of strings to send into the pane
	// (e.g. ["claude", "/model opus"]). Each is injected with a delay between.
	InjectSequence []string
	// Prompt is the main agent prompt injected after the sequence.
	Prompt string
	// InjectDelayMS is the delay in milliseconds between each injection.
	InjectDelayMS int
	// StatusFilePath is the path to the STATUS.md file to watch for completion.
	StatusFilePath string
	// Logger for status output.
	Logger *slog.Logger
}

// CreatePane creates a new tmux pane, names it, injects the startup sequence,
// and then injects the main prompt. It returns the pane ID.
func CreatePane(cfg PaneConfig) (string, error) {
	// Create a new window (pane) named after the issue
	out, err := exec.Command("tmux", "new-window", "-n", cfg.IssueName, "-P", "-F", "#{pane_id}").Output()
	if err != nil {
		return "", fmt.Errorf("failed to create tmux window %q: %w", cfg.IssueName, err)
	}
	paneID := strings.TrimSpace(string(out))

	delay := time.Duration(cfg.InjectDelayMS) * time.Millisecond
	if delay <= 0 {
		delay = 5 * time.Second
	}

	// Inject the startup command sequence
	for _, text := range cfg.InjectSequence {
		if err := sendKeys(paneID, text); err != nil {
			return paneID, fmt.Errorf("failed to inject %q into pane %s: %w", text, paneID, err)
		}
		time.Sleep(delay)
	}

	// Wait before injecting the main prompt
	time.Sleep(delay)

	// Inject the main agent prompt
	if err := sendKeys(paneID, cfg.Prompt); err != nil {
		return paneID, fmt.Errorf("failed to inject prompt into pane %s: %w", paneID, err)
	}

	return paneID, nil
}

// sendKeys sends text to a tmux pane followed by Enter.
func sendKeys(paneID, text string) error {
	return exec.Command("tmux", "send-keys", "-t", paneID, text, "Enter").Run()
}

// WatchStatusFile watches the given STATUS.md file for a line containing
// "success" or "failed". It blocks until one is found or the context is cancelled.
// Returns the matched status line.
func WatchStatusFile(ctx context.Context, path string, pollInterval time.Duration, logger *slog.Logger) (string, error) {
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}

	logger.Info("watching STATUS.md for completion", "path", path)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			line, found := scanForStatus(path)
			if found {
				logger.Info("found status line in STATUS.md", "status", line)
				fmt.Printf("STATUS.md [%s]: %s\n", filepath.Base(filepath.Dir(path)), line)
				return line, nil
			}
		}
	}
}

// scanForStatus reads the file and looks for the most recent line containing
// "success" or "failed" (case-insensitive).
func scanForStatus(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()

	var lastMatch string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		lower := strings.ToLower(strings.TrimSpace(line))
		if strings.Contains(lower, "success") || strings.Contains(lower, "failed") {
			lastMatch = line
		}
	}

	if lastMatch != "" {
		return lastMatch, true
	}
	return "", false
}

// ExtractModelFromLabels looks for a label with the "MODEL:" prefix and returns
// the model name (e.g. "claude" from "MODEL:claude"). Returns empty string if
// no MODEL: label is found.
func ExtractModelFromLabels(labels []string) string {
	for _, label := range labels {
		upper := strings.ToUpper(strings.TrimSpace(label))
		if strings.HasPrefix(upper, "MODEL:") {
			return strings.TrimSpace(label[len("MODEL:"):])
		}
	}
	return ""
}
