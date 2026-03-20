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

// GeminiAgent implements the Agent interface using Gemini CLI.
// It uses `gemini -p --output-format stream-json` for streaming JSON output.
type GeminiAgent struct {
	command string
}

// NewGeminiAgent creates a new Gemini CLI agent.
func NewGeminiAgent(command string) *GeminiAgent {
	if command == "" {
		command = "gemini"
	}
	return &GeminiAgent{command: command}
}

func (a *GeminiAgent) Name() string {
	return "gemini"
}

func (a *GeminiAgent) Start(ctx context.Context, workspace string, prompt string) (Session, error) {
	// Gemini CLI uses -p for prompt and --output-format stream-json for streaming
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
		return nil, fmt.Errorf("failed to start gemini: %w", err)
	}

	session := &geminiSession{
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

type geminiSession struct {
	cmd       *exec.Cmd
	stdout    io.ReadCloser
	stderr    io.ReadCloser
	events    chan Event
	done      chan struct{}
	workspace string
	mu        sync.Mutex
}

func (s *geminiSession) Events() <-chan Event {
	return s.events
}

func (s *geminiSession) Send(input string) error {
	// Gemini CLI in streaming mode doesn't support interactive input
	return fmt.Errorf("gemini session does not support interactive input")
}

func (s *geminiSession) Stop() error {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	return s.cmd.Process.Kill()
}

func (s *geminiSession) Wait() error {
	return s.cmd.Wait()
}

func (s *geminiSession) readLoop() {
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

func (s *geminiSession) parseMessage(msg map[string]interface{}, raw []byte) Event {
	// Gemini CLI stream-json format (similar to Claude)
	msgType, _ := msg["type"].(string)

	switch msgType {
	case "assistant", "model":
		content := ""
		if text, ok := msg["text"].(string); ok {
			content = text
		} else if parts, ok := msg["parts"].([]interface{}); ok {
			for _, part := range parts {
				if p, ok := part.(map[string]interface{}); ok {
					if text, ok := p["text"].(string); ok {
						content += text
					}
				}
			}
		}
		return Event{Type: EventTypeMessage, Content: content, Raw: raw}

	case "tool_call", "function_call":
		toolName := ""
		if name, ok := msg["name"].(string); ok {
			toolName = name
		}
		return Event{Type: EventTypeToolUse, Content: toolName, Raw: raw}

	case "tool_result", "function_response":
		return Event{Type: EventTypeToolResult, Content: "tool_result", Raw: raw}

	case "error":
		errMsg := ""
		if e, ok := msg["error"].(string); ok {
			errMsg = e
		} else if e, ok := msg["message"].(string); ok {
			errMsg = e
		}
		return Event{Type: EventTypeError, Content: errMsg, Raw: raw}

	case "result", "done":
		return Event{Type: EventTypeComplete, Content: "completed", Raw: raw}

	default:
		return Event{Type: EventTypeMessage, Content: msgType, Raw: raw}
	}
}

func (s *geminiSession) drainStderr() {
	io.Copy(io.Discard, s.stderr)
}
