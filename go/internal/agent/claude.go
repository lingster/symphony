package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
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

func (a *ClaudeAgent) Start(ctx context.Context, workspace string, prompt string) (Session, error) {
	// Claude Code uses -p for prompt and --output-format stream-json for streaming
	cmd := exec.CommandContext(ctx, a.command, "-p", prompt, "--output-format", "stream-json")
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
	// Claude Code stream-json format
	msgType, _ := msg["type"].(string)

	switch msgType {
	case "assistant":
		content := ""
		if message, ok := msg["message"].(map[string]interface{}); ok {
			if contentBlocks, ok := message["content"].([]interface{}); ok {
				for _, block := range contentBlocks {
					if b, ok := block.(map[string]interface{}); ok {
						if text, ok := b["text"].(string); ok {
							content += text
						}
					}
				}
			}
		}
		return Event{Type: EventTypeMessage, Content: content, Raw: raw}

	case "tool_use":
		toolName := ""
		if name, ok := msg["name"].(string); ok {
			toolName = name
		}
		return Event{Type: EventTypeToolUse, Content: toolName, Raw: raw}

	case "tool_result":
		return Event{Type: EventTypeToolResult, Content: "tool_result", Raw: raw}

	case "error":
		errMsg := ""
		if e, ok := msg["error"].(string); ok {
			errMsg = e
		}
		return Event{Type: EventTypeError, Content: errMsg, Raw: raw}

	case "result":
		// Final result
		return Event{Type: EventTypeComplete, Content: "completed", Raw: raw}

	default:
		return Event{Type: EventTypeMessage, Content: msgType, Raw: raw}
	}
}

func (s *claudeSession) drainStderr() {
	io.Copy(io.Discard, s.stderr)
}
