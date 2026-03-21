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

func (a *CodexAgent) Start(ctx context.Context, workspace string, prompt string, systemPrompt string) (Session, error) {
	// Codex doesn't support a separate system prompt flag; prepend it to the user prompt.
	if systemPrompt != "" {
		prompt = systemPrompt + "\n\n---\n\n" + prompt
	}
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

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 10*1024*1024), 10*1024*1024) // 10MB buffer

	session := &codexSession{
		cmd:       cmd,
		stdin:     stdin,
		stdout:    stdout,
		stderr:    stderr,
		scanner:   scanner,
		events:    make(chan Event, 100),
		done:      make(chan struct{}),
		workspace: workspace,
	}

	go session.drainStderr()

	// Send initialization sequence (reads responses synchronously to get thread ID)
	if err := session.initialize(prompt); err != nil {
		session.Stop()
		return nil, fmt.Errorf("failed to initialize session: %w", err)
	}

	// Now start async read loop for agent events
	go session.readLoop()

	return session, nil
}

type codexSession struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	stderr    io.ReadCloser
	scanner   *bufio.Scanner
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
			"clientInfo": map[string]string{
				"name":    "symphony-orchestrator",
				"title":   "Symphony Orchestrator",
				"version": "0.1.0",
			},
			"capabilities": map[string]interface{}{
				"experimentalApi": true,
			},
		},
	}
	if err := s.sendJSON(initReq); err != nil {
		return err
	}

	// Wait for initialize response
	if err := s.waitForResponse(); err != nil {
		return fmt.Errorf("waiting for initialize response: %w", err)
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
			"approvalPolicy": "never",
			"sandbox":        "workspace-write",
			"cwd":            s.workspace,
		},
	}
	if err := s.sendJSON(threadReq); err != nil {
		return err
	}

	// Wait for thread/start response and extract thread ID
	if err := s.waitForThreadID(); err != nil {
		return fmt.Errorf("waiting for thread ID: %w", err)
	}

	// Start turn with prompt — now we have the real thread ID
	turnReq := map[string]interface{}{
		"id":     s.nextRequestID(),
		"method": "turn/start",
		"params": map[string]interface{}{
			"threadId": s.threadID,
			"input": []map[string]string{
				{"type": "text", "text": prompt},
			},
			"cwd":            s.workspace,
			"approvalPolicy": "never",
			"sandboxPolicy": map[string]interface{}{
				"type":                "workspaceWrite",
				"writableRoots":      []string{s.workspace},
				"readOnlyAccess":     map[string]string{"type": "fullAccess"},
				"networkAccess":      false,
				"excludeTmpdirEnvVar": false,
				"excludeSlashTmp":    false,
			},
		},
	}
	return s.sendJSON(turnReq)
}

// waitForResponse reads lines from the scanner until a JSON-RPC response (has "id", no "method") is found.
// Notifications encountered along the way are logged but skipped.
func (s *codexSession) waitForResponse() error {
	for s.scanner.Scan() {
		line := s.scanner.Bytes()
		if len(line) == 0 || line[0] != '{' {
			fmt.Fprintf(os.Stderr, "[codex stdout skip] %s\n", string(line))
			continue
		}

		var msg map[string]interface{}
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}

		fmt.Fprintf(os.Stderr, "[codex raw] %s\n", string(line))

		_, hasID := msg["id"]
		_, hasMethod := msg["method"].(string)

		if hasID && !hasMethod {
			// Check for error
			if errObj, ok := msg["error"].(map[string]interface{}); ok {
				return fmt.Errorf("RPC error: %v", errObj["message"])
			}
			return nil
		}
		// It's a notification — log and continue waiting
	}
	return fmt.Errorf("scanner ended before response received")
}

// waitForThreadID reads lines until it finds the thread/start response containing the thread ID.
// Notifications encountered along the way are forwarded to the events channel.
func (s *codexSession) waitForThreadID() error {
	for s.scanner.Scan() {
		line := s.scanner.Bytes()
		if len(line) == 0 || line[0] != '{' {
			fmt.Fprintf(os.Stderr, "[codex stdout skip] %s\n", string(line))
			continue
		}

		var msg map[string]interface{}
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}

		fmt.Fprintf(os.Stderr, "[codex raw] %s\n", string(line))

		_, hasID := msg["id"]
		method, hasMethod := msg["method"].(string)

		if hasID && !hasMethod {
			// This is the response to thread/start
			if errObj, ok := msg["error"].(map[string]interface{}); ok {
				return fmt.Errorf("RPC error: %v", errObj["message"])
			}
			if result, ok := msg["result"].(map[string]interface{}); ok {
				if thread, ok := result["thread"].(map[string]interface{}); ok {
					if id, ok := thread["id"].(string); ok {
						s.threadID = id
						return nil
					}
				}
			}
			return fmt.Errorf("thread/start response did not contain thread ID")
		}

		// It's a notification — emit as event if meaningful
		if hasMethod && method != "" {
			event := s.parseMessage(msg, line)
			select {
			case s.events <- event:
			default:
			}
		}
	}
	return fmt.Errorf("scanner ended before thread ID received")
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

	for s.scanner.Scan() {
		line := s.scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// Skip non-JSON lines (e.g. nvm output from login shell)
		if line[0] != '{' {
			fmt.Fprintf(os.Stderr, "[codex stdout skip] %s\n", string(line))
			continue
		}

		var msg map[string]interface{}
		if err := json.Unmarshal(line, &msg); err != nil {
			s.events <- Event{
				Type:    EventTypeError,
				Content: fmt.Sprintf("malformed JSON: %v\nraw line: %s", err, string(line)),
				Raw:     line,
			}
			continue
		}

		// Debug: log raw JSON-RPC messages
		fmt.Fprintf(os.Stderr, "[codex raw] %s\n", string(line))

		// Distinguish JSON-RPC responses (have "id", no "method") from notifications (have "method")
		method, hasMethod := msg["method"].(string)
		_, hasID := msg["id"]

		if hasID && !hasMethod {
			// Responses are not agent events; skip emitting them
			continue
		}

		event := s.parseMessage(msg, line)
		select {
		case s.events <- event:
		case <-s.done:
			return
		}

		// Check for turn completion
		if method == "turn/completed" || method == "turn/failed" || method == "turn/cancelled" {
			s.events <- Event{Type: EventTypeComplete, Content: method}
			return
		}
	}
}

func (s *codexSession) parseMessage(msg map[string]interface{}, raw []byte) Event {
	method, _ := msg["method"].(string)

	switch method {
	case "item/message", "item/agentMessage/delta":
		content := extractTextContent(msg)
		return Event{Type: EventTypeMessage, Content: content, Raw: raw}

	case "item/tool/call":
		toolName := ""
		if params, ok := msg["params"].(map[string]interface{}); ok {
			toolName, _ = params["name"].(string)
		}
		if toolName == "" {
			toolName = "tool_call"
		}
		return Event{Type: EventTypeToolUse, Content: toolName, Raw: raw}

	case "item/tool/result":
		return Event{Type: EventTypeToolResult, Content: "tool_result", Raw: raw}

	case "turn/completed", "turn/failed", "turn/cancelled":
		return Event{Type: EventTypeComplete, Content: method, Raw: raw}

	default:
		return Event{Type: EventTypeMessage, Content: method, Raw: raw}
	}
}

// extractTextContent tries multiple paths to find text content in a codex message,
// mirroring the Elixir implementation's flexible path-based extraction.
func extractTextContent(msg map[string]interface{}) string {
	params, ok := msg["params"].(map[string]interface{})
	if !ok {
		return ""
	}

	// Try direct paths under params
	roots := []map[string]interface{}{params}

	// Also try params.msg and params.msg.payload as roots
	if msgObj, ok := params["msg"].(map[string]interface{}); ok {
		roots = append(roots, msgObj)
		if payload, ok := msgObj["payload"].(map[string]interface{}); ok {
			roots = append(roots, payload)
		}
	}

	// Text field names in priority order (matching Elixir's delta_paths)
	fields := []string{"delta", "textDelta", "outputDelta", "text", "summaryText", "content"}

	for _, root := range roots {
		for _, field := range fields {
			if val, ok := root[field].(string); ok && val != "" {
				return val
			}
		}
	}

	return ""
}

func (s *codexSession) drainStderr() {
	scanner := bufio.NewScanner(s.stderr)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line != "" {
			fmt.Fprintf(os.Stderr, "[codex stderr] %s\n", line)
		}
	}
}
