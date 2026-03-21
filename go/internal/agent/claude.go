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
		return Event{Type: EventTypeComplete, Content: "completed", Raw: raw}

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
	var toolNames []string

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
			toolNames = append(toolNames, name)
		case "text":
			if text, ok := b["text"].(string); ok && text != "" {
				textParts = append(textParts, text)
			}
		}
	}

	// Emit tool_use events if present (they take priority)
	if len(toolNames) > 0 {
		return Event{Type: EventTypeToolUse, Content: strings.Join(toolNames, ", "), Raw: raw}
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

	for _, block := range contentBlocks {
		b, ok := block.(map[string]interface{})
		if !ok {
			continue
		}
		if blockType, _ := b["type"].(string); blockType == "tool_result" {
			return Event{Type: EventTypeToolResult, Content: "tool_result", Raw: raw}
		}
	}

	return Event{Type: EventTypeMessage, Raw: raw}
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
