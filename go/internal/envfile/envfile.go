// Package envfile provides helpers to read and update .env files while
// preserving comments, blank lines, and ordering.
package envfile

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Update sets one or more key=value pairs in the given .env file. Existing
// keys are updated in place; new keys are appended. Comments and blank lines
// are preserved.
func Update(path string, kvs map[string]string) error {
	lines, err := readLines(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}

	remaining := make(map[string]string, len(kvs))
	for k, v := range kvs {
		remaining[k] = v
	}

	// Update existing lines in place.
	for i, line := range lines {
		key := lineKey(line)
		if key == "" {
			continue
		}
		if val, ok := remaining[key]; ok {
			lines[i] = fmt.Sprintf("%s=%s", key, val)
			delete(remaining, key)
		}
	}

	// Append any keys that were not already present.
	for k, v := range remaining {
		lines = append(lines, fmt.Sprintf("%s=%s", k, v))
	}

	return writeLines(path, lines)
}

// Load reads a .env file and returns a map of key=value pairs. It does NOT
// set environment variables — it only returns the parsed values.
func Load(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	result := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		key, val := parseLine(scanner.Text())
		if key != "" {
			result[key] = val
		}
	}
	return result, scanner.Err()
}

// lineKey returns the key from a KEY=VALUE line, or "" for comments/blanks.
func lineKey(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return ""
	}
	parts := strings.SplitN(trimmed, "=", 2)
	if len(parts) < 2 {
		return ""
	}
	return strings.TrimSpace(parts[0])
}

// parseLine extracts a key and unquoted value from a line.
func parseLine(line string) (string, string) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", ""
	}
	parts := strings.SplitN(trimmed, "=", 2)
	if len(parts) < 2 {
		return "", ""
	}
	key := strings.TrimSpace(parts[0])
	val := strings.TrimSpace(parts[1])
	// Strip surrounding quotes.
	val = strings.Trim(val, `"'`)
	return key, val
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}

func writeLines(path string, lines []string) error {
	content := strings.Join(lines, "\n") + "\n"
	return os.WriteFile(path, []byte(content), 0600)
}
