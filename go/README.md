# Symphony Go Orchestrator

Symphony is a long-running automation service that continuously reads work from Linear, creates isolated workspaces for each issue, and runs coding agent sessions.

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                        cmd/symphony/                            │
│                    Main CLI entrypoint                          │
└──────────────────────────────┬──────────────────────────────────┘
                               │
                               ▼
┌─────────────────────────────────────────────────────────────────┐
│                   internal/orchestrator/                        │
│  • Polling loop (every N ms)                                   │
│  • Reconciliation (state refresh, stall detection)             │
│  • Dispatch logic (slots, priority, routing)                   │
│  • Retry queue with exponential backoff                        │
└───────────┬─────────────────────────────────┬───────────────────┘
            │                                 │
            ▼                                 ▼
┌─────────────────────────┐     ┌─────────────────────────────────┐
│    internal/linear/     │     │        internal/agent/          │
│  • GraphQL client       │     │  • Agent interface              │
│  • Issue normalization  │     │  • CodexAgent (app-server)      │
│  • Pagination           │     │  • ClaudeAgent (claude -p)      │
│  • Blocker parsing      │     │  • GeminiAgent (gemini -p)      │
└─────────────────────────┘     └─────────────────────────────────┘
                                              │
                                              ▼
                               ┌─────────────────────────────────┐
                               │      internal/workspace/        │
                               │  • Per-issue directories        │
                               │  • Lifecycle hooks              │
                               │  • Path sanitization            │
                               │  • Safety validation            │
                               └─────────────────────────────────┘
                                              │
                                              ▼
                               ┌─────────────────────────────────┐
                               │       internal/config/          │
                               │  • WORKFLOW.md parsing          │
                               │  • YAML front matter            │
                               │  • Environment resolution       │
                               │  • Validation                   │
                               └─────────────────────────────────┘
```

## Agent Routing

When a ticket is pulled from Linear, Symphony routes it to the appropriate agent based on the assignee:

| Assignee contains | Routes to    | Command                                    |
|-------------------|--------------|-------------------------------------------|
| "gemini"          | Gemini CLI   | `gemini -p <prompt> --output-format stream-json` |
| "claude"          | Claude Code  | `claude -p <prompt> --output-format stream-json` |
| "codex"           | Codex        | `codex app-server` (JSON-RPC over stdio)  |
| (default)         | Codex        | configurable via `agent.default`          |

## Agent Interface

All agents implement a common interface:

```go
type Agent interface {
    Name() string
    Start(ctx context.Context, workspace string, prompt string) (Session, error)
}

type Session interface {
    Events() <-chan Event  // Stream of events from agent
    Send(input string) error  // Send input (for interactive sessions)
    Stop() error              // Terminate session
    Wait() error              // Wait for completion
}

type Event struct {
    Type    EventType       // message, tool_use, tool_result, error, complete
    Content string
    Raw     json.RawMessage
}
```

### Agent Implementations

- **CodexAgent**: Uses `codex app-server` with JSON-RPC protocol over stdio. Supports interactive multi-turn sessions.
- **ClaudeAgent**: Uses `claude -p --output-format stream-json`. Single-prompt execution with streaming output.
- **GeminiAgent**: Uses `gemini -p --output-format stream-json`. Single-prompt execution with streaming output.

## Configuration (WORKFLOW.md)

Symphony is configured via a `WORKFLOW.md` file with YAML front matter:

```yaml
---
tracker:
  kind: linear
  project_slug: "my-project"
  api_key: $LINEAR_API_KEY
  active_states: ["Todo", "In Progress"]
  terminal_states: ["Done", "Closed", "Cancelled"]

polling:
  interval_ms: 30000

workspace:
  root: ~/workspaces

hooks:
  after_create: |
    git clone $REPO_URL .
  before_run: |
    git pull origin main
  after_run: |
    git status

agent:
  max_concurrent_agents: 4
  max_turns: 20
  default: codex

codex:
  command: codex app-server
  stall_timeout_ms: 300000
---

# Task Prompt

You are working on issue {{issue.identifier}}: {{issue.title}}

## Description
{{issue.description}}

## Instructions
1. Read the codebase
2. Implement the required changes
3. Run tests
4. Commit your work
```

## Usage

```bash
# Run with default WORKFLOW.md
symphony

# Run with specific workflow file
symphony path/to/WORKFLOW.md

# Or use flag
symphony --workflow path/to/WORKFLOW.md
```

## Environment Variables

- `LINEAR_API_KEY`: API token for Linear (required)

## Building

```bash
cd go/
go build -o symphony ./cmd/symphony
```

## Core Concepts

### Polling Loop
Every `polling.interval_ms`, the orchestrator:
1. Reconciles running sessions (stall detection, state refresh)
2. Processes retry queue
3. Fetches candidate issues from Linear
4. Dispatches eligible issues to available slots

### Dispatch Priority
Issues are sorted for dispatch:
1. Priority (1-4, lower is higher priority)
2. Created date (oldest first)
3. Identifier (lexicographic)

### Concurrency Control
- Global limit: `agent.max_concurrent_agents`
- Per-state limit: `agent.max_concurrent_agents_by_state`

### Retry Logic
- Normal completion: 1s continuation retry
- Failure: Exponential backoff (10s base, capped at `max_retry_backoff_ms`)

### Workspace Safety
- All workspace paths are sanitized (`[^A-Za-z0-9._-]` → `_`)
- Paths are validated to stay within workspace root
- Agent CWD is always the per-issue workspace

## Project Structure

```
go/
├── cmd/
│   └── symphony/
│       └── main.go           # CLI entrypoint
├── internal/
│   ├── agent/
│   │   ├── agent.go          # Interface + Registry
│   │   ├── codex.go          # Codex implementation
│   │   ├── claude.go         # Claude Code implementation
│   │   └── gemini.go         # Gemini CLI implementation
│   ├── config/
│   │   └── config.go         # WORKFLOW.md parsing
│   ├── linear/
│   │   └── client.go         # Linear GraphQL client
│   ├── orchestrator/
│   │   └── orchestrator.go   # Main polling loop
│   └── workspace/
│       └── workspace.go      # Workspace management
├── go.mod
└── README.md
```

## License

See repository root.
