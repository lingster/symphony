package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// ClaudeAgent implements the Agent interface using Claude Code CLI.
// It uses `claude -p --output-format stream-json` for streaming JSON output.
type ClaudeAgent struct {
	command string
}

// NewClaudeAgent creates a new Claude Code agent.
func NewClaudeAgent(command string) *ClaudeAgent {
	if command == "" {
		command = "claude"
	}
	return &ClaudeAgent{command: command}
}

func (a *ClaudeAgent) Name() string {
	return "claude"
}

func (a *ClaudeAgent) Start(ctx context.Context, workspace string, prompt string, systemPrompt string) (Session, error) {
	// Claude Code uses -p for prompt and --output-format stream-json for streaming.
	// --dangerously-skip-permissions is required for non-interactive execution.
	args := []string{"-p", prompt, "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions"}
	if systemPrompt != "" {
		args = append(args, "--append-system-prompt", systemPrompt)
	}
	cmd := exec.CommandContext(ctx, a.command, args...)
	cmd.Dir = workspace
	cmd.Env = os.Environ()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start claude: %w", err)
	}

	session := &claudeSession{
		cmd:       cmd,
		stdout:    stdout,
		stderr:    stderr,
		events:    make(chan Event, 100),
		done:      make(chan struct{}),
		workspace: workspace,
	}

	go session.readLoop()
	go session.drainStderr()

	return session, nil
}

type claudeSession struct {
	cmd       *exec.Cmd
	stdout    io.ReadCloser
	stderr    io.ReadCloser
	events    chan Event
	done      chan struct{}
	workspace string
	mu        sync.Mutex
}

func (s *claudeSession) Events() <-chan Event {
	return s.events
}

func (s *claudeSession) Send(input string) error {
	// Claude Code in streaming mode doesn't support interactive input
	// Each invocation is a single prompt
	return fmt.Errorf("claude session does not support interactive input")
}

func (s *claudeSession) Stop() error {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	return s.cmd.Process.Kill()
}

func (s *claudeSession) Wait() error {
	return s.cmd.Wait()
}

func (s *claudeSession) readLoop() {
	defer close(s.events)
	scanner := bufio.NewScanner(s.stdout)
	scanner.Buffer(make([]byte, 10*1024*1024), 10*1024*1024) // 10MB buffer

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var msg map[string]interface{}
		if err := json.Unmarshal(line, &msg); err != nil {
			// Non-JSON output, treat as message
			s.events <- Event{
				Type:    EventTypeMessage,
				Content: string(line),
			}
			continue
		}

		event := s.parseMessage(msg, line)
		select {
		case s.events <- event:
		case <-s.done:
			return
		}
	}

	// Process completed
	s.events <- Event{Type: EventTypeComplete, Content: "completed"}
}

func (s *claudeSession) parseMessage(msg map[string]interface{}, raw []byte) Event {
	// Claude Code stream-json format: tool use and tool results are nested
	// inside "assistant" and "user" messages as content blocks.
	//   assistant + content[].type=="tool_use"   → EventTypeToolUse
	//   assistant + content[].type=="text"        → EventTypeMessage
	//   user      + content[].type=="tool_result" → EventTypeToolResult
	//   result                                    → EventTypeComplete
	msgType, _ := msg["type"].(string)

	switch msgType {
	case "assistant":
		return s.parseAssistantMessage(msg, raw)

	case "user":
		return s.parseUserMessage(msg, raw)

	case "error":
		errMsg := ""
		if e, ok := msg["error"].(string); ok {
			errMsg = e
		}
		return Event{Type: EventTypeError, Content: errMsg, Raw: raw}

	case "result":
		return s.parseResultMessage(msg, raw)

	default:
		// system, rate_limit_event, etc. — pass through as messages
		return Event{Type: EventTypeMessage, Content: msgType, Raw: raw}
	}
}

// parseAssistantMessage inspects content blocks to distinguish text from tool_use.
func (s *claudeSession) parseAssistantMessage(msg map[string]interface{}, raw []byte) Event {
	message, ok := msg["message"].(map[string]interface{})
	if !ok {
		return Event{Type: EventTypeMessage, Raw: raw}
	}

	contentBlocks, ok := message["content"].([]interface{})
	if !ok {
		return Event{Type: EventTypeMessage, Raw: raw}
	}

	// Check what types of content blocks are present
	var textParts []string
	var toolDescriptions []string

	for _, block := range contentBlocks {
		b, ok := block.(map[string]interface{})
		if !ok {
			continue
		}
		blockType, _ := b["type"].(string)
		switch blockType {
		case "tool_use":
			name, _ := b["name"].(string)
			if name == "" {
				name = "tool_call"
			}
			desc := formatToolUse(name, b["input"])
			toolDescriptions = append(toolDescriptions, desc)
		case "text":
			if text, ok := b["text"].(string); ok && text != "" {
				textParts = append(textParts, text)
			}
		}
	}

	// Emit tool_use events if present (they take priority)
	if len(toolDescriptions) > 0 {
		return Event{Type: EventTypeToolUse, Content: strings.Join(toolDescriptions, "\n"), Raw: raw}
	}

	return Event{Type: EventTypeMessage, Content: strings.Join(textParts, ""), Raw: raw}
}

// parseUserMessage inspects content blocks for tool_result entries.
func (s *claudeSession) parseUserMessage(msg map[string]interface{}, raw []byte) Event {
	message, ok := msg["message"].(map[string]interface{})
	if !ok {
		return Event{Type: EventTypeMessage, Raw: raw}
	}

	contentBlocks, ok := message["content"].([]interface{})
	if !ok {
		return Event{Type: EventTypeMessage, Raw: raw}
	}

	// Also check top-level tool_use_result for richer output
	toolResult, _ := msg["tool_use_result"].(map[string]interface{})

	for _, block := range contentBlocks {
		b, ok := block.(map[string]interface{})
		if !ok {
			continue
		}
		if blockType, _ := b["type"].(string); blockType == "tool_result" {
			// Prefer stdout from top-level tool_use_result if available
			var output string
			if toolResult != nil {
				if stdout, ok := toolResult["stdout"].(string); ok && stdout != "" {
					output = stdout
				}
				if stderr, ok := toolResult["stderr"].(string); ok && stderr != "" {
					if output != "" {
						output += "\n" + stderr
					} else {
						output = stderr
					}
				}
			}
			if output == "" {
				if content, ok := b["content"].(string); ok {
					output = content
				}
			}

			summary := truncateStr(output, 500)
			return Event{Type: EventTypeToolResult, Content: summary, Raw: raw}
		}
	}

	return Event{Type: EventTypeMessage, Raw: raw}
}

// parseResultMessage extracts completion details from a Claude CLI result message.
func (s *claudeSession) parseResultMessage(msg map[string]interface{}, raw []byte) Event {
	subtype, _ := msg["subtype"].(string)
	isError, _ := msg["is_error"].(bool)
	resultText, _ := msg["result"].(string)
	durationMS, _ := msg["duration_ms"].(float64)
	costUSD, _ := msg["total_cost_usd"].(float64)
	numTurns, _ := msg["num_turns"].(float64)

	status := "success"
	if isError || subtype == "error" {
		status = "error"
	}

	summary := fmt.Sprintf("status=%s duration=%.0fs cost=$%.4f turns=%.0f",
		status, durationMS/1000, costUSD, numTurns)
	if resultText != "" {
		truncated := resultText
		if len(truncated) > 200 {
			truncated = truncated[:200] + "..."
		}
		summary += " result=" + truncated
	}

	return Event{Type: EventTypeComplete, Content: summary, ResultText: resultText, Raw: raw}
}

// formatToolUse returns a human-readable summary of a tool invocation.
func formatToolUse(name string, input interface{}) string {
	inputMap, ok := input.(map[string]interface{})
	if !ok {
		return name
	}

	switch name {
	case "Bash":
		if cmd, ok := inputMap["command"].(string); ok {
			desc, _ := inputMap["description"].(string)
			if desc != "" {
				return fmt.Sprintf("Bash: %s  (%s)", desc, truncateStr(cmd, 120))
			}
			return fmt.Sprintf("Bash: %s", truncateStr(cmd, 150))
		}
	case "Read":
		if fp, ok := inputMap["file_path"].(string); ok {
			return fmt.Sprintf("Read: %s", fp)
		}
	case "Write":
		if fp, ok := inputMap["file_path"].(string); ok {
			return fmt.Sprintf("Write: %s", fp)
		}
	case "Edit":
		if fp, ok := inputMap["file_path"].(string); ok {
			return fmt.Sprintf("Edit: %s", fp)
		}
	case "Glob":
		if pattern, ok := inputMap["pattern"].(string); ok {
			return fmt.Sprintf("Glob: %s", pattern)
		}
	case "Grep":
		if pattern, ok := inputMap["pattern"].(string); ok {
			return fmt.Sprintf("Grep: %s", pattern)
		}
	case "Agent", "Task":
		if desc, ok := inputMap["description"].(string); ok {
			return fmt.Sprintf("%s: %s", name, desc)
		}
		if prompt, ok := inputMap["prompt"].(string); ok {
			return fmt.Sprintf("%s: %s", name, truncateStr(prompt, 100))
		}
	}

	return name
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func (s *claudeSession) drainStderr() {
	scanner := bufio.NewScanner(s.stderr)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line != "" {
			fmt.Fprintf(os.Stderr, "[claude stderr] %s\n", line)
		}
	}
}
