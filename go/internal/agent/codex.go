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

// CodexAgent implements the Agent interface using codex app-server.
// It communicates via JSON-RPC over stdio.
type CodexAgent struct {
	command string
}

// NewCodexAgent creates a new Codex agent.
func NewCodexAgent(command string) *CodexAgent {
	if command == "" {
		command = "codex app-server"
	}
	return &CodexAgent{command: command}
}

func (a *CodexAgent) Name() string {
	return "codex"
}

func (a *CodexAgent) Start(ctx context.Context, workspace string, prompt string) (Session, error) {
	cmd := exec.CommandContext(ctx, "bash", "-lc", a.command)
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), "CODEX_WORKSPACE="+workspace)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start codex: %w", err)
	}

	session := &codexSession{
		cmd:       cmd,
		stdin:     stdin,
		stdout:    stdout,
		stderr:    stderr,
		events:    make(chan Event, 100),
		done:      make(chan struct{}),
		workspace: workspace,
	}

	go session.readLoop()
	go session.drainStderr()

	// Send initialization sequence
	if err := session.initialize(prompt); err != nil {
		session.Stop()
		return nil, fmt.Errorf("failed to initialize session: %w", err)
	}

	return session, nil
}

type codexSession struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	stderr    io.ReadCloser
	events    chan Event
	done      chan struct{}
	workspace string
	mu        sync.Mutex
	threadID  string
	requestID int
}

func (s *codexSession) Events() <-chan Event {
	return s.events
}

func (s *codexSession) Send(input string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	msg := map[string]interface{}{
		"id":     s.nextRequestID(),
		"method": "turn/start",
		"params": map[string]interface{}{
			"threadId": s.threadID,
			"input": []map[string]string{
				{"type": "text", "text": input},
			},
			"cwd": s.workspace,
		},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	_, err = s.stdin.Write(append(data, '\n'))
	return err
}

func (s *codexSession) Stop() error {
	s.stdin.Close()
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	return s.cmd.Process.Kill()
}

func (s *codexSession) Wait() error {
	return s.cmd.Wait()
}

func (s *codexSession) nextRequestID() int {
	s.requestID++
	return s.requestID
}

func (s *codexSession) initialize(prompt string) error {
	// Send initialize request
	initReq := map[string]interface{}{
		"id":     s.nextRequestID(),
		"method": "initialize",
		"params": map[string]interface{}{
			"clientInfo":   map[string]string{"name": "symphony", "version": "1.0"},
			"capabilities": map[string]interface{}{},
		},
	}
	if err := s.sendJSON(initReq); err != nil {
		return err
	}

	// Send initialized notification
	initNotif := map[string]interface{}{
		"method": "initialized",
		"params": map[string]interface{}{},
	}
	if err := s.sendJSON(initNotif); err != nil {
		return err
	}

	// Start thread
	threadReq := map[string]interface{}{
		"id":     s.nextRequestID(),
		"method": "thread/start",
		"params": map[string]interface{}{
			"approvalPolicy": "auto-edit",
			"sandbox":        "none",
			"cwd":            s.workspace,
		},
	}
	if err := s.sendJSON(threadReq); err != nil {
		return err
	}

	// Start turn with prompt
	turnReq := map[string]interface{}{
		"id":     s.nextRequestID(),
		"method": "turn/start",
		"params": map[string]interface{}{
			"threadId": "", // Will be filled by app-server response
			"input": []map[string]string{
				{"type": "text", "text": prompt},
			},
			"cwd":            s.workspace,
			"approvalPolicy": "auto-edit",
			"sandboxPolicy":  map[string]string{"type": "none"},
		},
	}
	return s.sendJSON(turnReq)
}

func (s *codexSession) sendJSON(v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.stdin.Write(append(data, '\n'))
	return err
}

func (s *codexSession) readLoop() {
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
			s.events <- Event{
				Type:    EventTypeError,
				Content: fmt.Sprintf("malformed JSON: %v", err),
				Raw:     line,
			}
			continue
		}

		event := s.parseMessage(msg, line)
		select {
		case s.events <- event:
		case <-s.done:
			return
		}

		// Check for turn completion
		if method, ok := msg["method"].(string); ok {
			if method == "turn/completed" || method == "turn/failed" || method == "turn/cancelled" {
				s.events <- Event{Type: EventTypeComplete, Content: method}
				return
			}
		}
	}
}

func (s *codexSession) parseMessage(msg map[string]interface{}, raw []byte) Event {
	method, _ := msg["method"].(string)

	switch method {
	case "thread/start":
		if result, ok := msg["result"].(map[string]interface{}); ok {
			if thread, ok := result["thread"].(map[string]interface{}); ok {
				s.threadID, _ = thread["id"].(string)
			}
		}
		return Event{Type: EventTypeMessage, Content: "session_started", Raw: raw}

	case "item/message":
		content := ""
		if params, ok := msg["params"].(map[string]interface{}); ok {
			if text, ok := params["text"].(string); ok {
				content = text
			}
		}
		return Event{Type: EventTypeMessage, Content: content, Raw: raw}

	case "item/tool/call":
		return Event{Type: EventTypeToolUse, Content: "tool_call", Raw: raw}

	case "item/tool/result":
		return Event{Type: EventTypeToolResult, Content: "tool_result", Raw: raw}

	default:
		return Event{Type: EventTypeMessage, Content: method, Raw: raw}
	}
}

func (s *codexSession) drainStderr() {
	io.Copy(io.Discard, s.stderr)
}
