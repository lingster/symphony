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

## Building

```bash
cd go/
go build -o symphony ./cmd/symphony

# With version info embedded at build time
go build -ldflags "-X main.version=1.0.0" -o symphony ./cmd/symphony
```

## Quick Start

```bash
# 1. Build
cd go/
go build -o symphony ./cmd/symphony

# 2. Configure Linear credentials (see Authentication section below)
cp .env.example .env
# Edit .env with your credentials

# 3. Create a WORKFLOW.md (see Configuration section above)

# 4. Start the orchestration service
./symphony start
```

## Authentication

Symphony requires Linear credentials. You have two options:

### Option A: Personal API Key (simplest)

1. Go to [Linear Settings > API > Personal API Keys](https://linear.app/settings/api)
2. Create a new key
3. Add to your `.env`:
   ```
   LINEAR_API_KEY=lin_api_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
   ```

This is the quickest setup. Actions will appear as your personal user.

### Option B: OAuth Agent Token (bot identity)

This creates a dedicated bot user in your Linear workspace. You need workspace admin permissions.

#### Prerequisites

1. Create an OAuth application at [Linear Settings > API > OAuth Applications](https://linear.app/settings/api/applications)
2. Set the callback URL to `http://localhost:3456/callback`
3. Note your **Client ID** and **Client Secret**

#### With browser access

```bash
LINEAR_OAUTH_CLIENT_ID=xxx LINEAR_OAUTH_CLIENT_SECRET=xxx go run ./cmd/oauth-setup/
```

This opens your browser to authorize, exchanges the code, and prints the tokens to add to your `.env`.

#### On a remote/headless server (no browser)

1. Run the setup on the remote server:
   ```bash
   LINEAR_OAUTH_CLIENT_ID=xxx LINEAR_OAUTH_CLIENT_SECRET=xxx go run ./cmd/oauth-setup/
   ```
2. Copy the printed authorization URL and open it in your **local** browser
3. After authorizing, Linear redirects to `http://localhost:3456/callback?code=SOME_CODE` — this will fail in your browser (that's expected)
4. Copy the `code` parameter from the browser's address bar and paste it into the stdin prompt on the remote server

Alternatively, use **SSH port forwarding** so the redirect works automatically:

```bash
ssh -L 3456:localhost:3456 your-remote-server
# Then run the oauth-setup command as normal
```

#### Result

After completing the OAuth flow, add the output to your `.env`:

```
LINEAR_AGENT_TOKEN=<access_token>
LINEAR_REFRESH_TOKEN=<refresh_token>
LINEAR_OAUTH_CLIENT_ID=<client_id>
LINEAR_OAUTH_CLIENT_SECRET=<client_secret>
```

When all four values are set, Symphony automatically refreshes the token when it expires.

## Usage

Symphony uses subcommands. Run `symphony --help` for a full overview.

### Global Flags

| Flag | Description | Default |
|---|---|---|
| `--workflow <path>` | Path to `WORKFLOW.md` | `./WORKFLOW.md` |
| `-h`, `--help` | Show help for any command | — |

### Commands

#### `symphony start [workflow-path]`

Start the orchestration service. Polls Linear for issues, creates workspaces, and dispatches agents. Runs until interrupted (SIGINT/SIGTERM).

```bash
# Start with default ./WORKFLOW.md
symphony start

# Start with a specific workflow file
symphony start path/to/WORKFLOW.md

# Or use the global --workflow flag
symphony --workflow path/to/WORKFLOW.md start
```

#### `symphony linear list`

Fetch and display pending issues from the configured Linear project (issues in active states).

```bash
symphony linear list
symphony --workflow path/to/WORKFLOW.md linear list
```

Output includes identifier, state, priority, assignee, and title in a tabular format. Blocked issues are listed separately.

#### `symphony agent run --agent <name> <prompt>`

Launch a single coding agent with a prompt in the current directory. Useful for testing agent dispatch outside the orchestration loop.

```bash
symphony agent run --agent claude "Explain the main function"
symphony agent run --agent codex "Fix the failing tests"
symphony agent run --agent gemini --verbose "List all TODO comments"
```

| Flag | Description | Default |
|---|---|---|
| `--agent <name>` | Agent to use: `codex`, `claude`, `gemini` | `codex` |
| `--verbose` | Stream all agent events to stderr | `false` |

Without `--verbose`, only errors are shown during execution and a summary is printed when the agent finishes. With `--verbose`, all events (messages, tool calls, results) are streamed to stderr in real-time.

#### `symphony agent list`

List all available agents and their descriptions.

```bash
symphony agent list
```

#### `symphony version`

Print the Symphony version.

```bash
symphony version
```

#### `symphony completion`

Generate shell autocompletion scripts (provided by Cobra).

```bash
# Bash
symphony completion bash > /etc/bash_completion.d/symphony

# Zsh
symphony completion zsh > "${fpath[1]}/_symphony"
```

### Environment Variables

| Variable | Description | Required |
|---|---|---|
| `LINEAR_API_KEY` | Linear personal API token | Yes (unless using OAuth) |
| `LINEAR_AGENT_TOKEN` | Linear OAuth agent token (takes priority over `LINEAR_API_KEY`) | No |
| `LINEAR_OAUTH_CLIENT_ID` | OAuth client ID for automatic token refresh | No |
| `LINEAR_OAUTH_CLIENT_SECRET` | OAuth client secret for token refresh | No |
| `LINEAR_REFRESH_TOKEN` | OAuth refresh token | No |

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
│       ├── main.go           # CLI entrypoint
│       ├── root.go           # Root Cobra command + global flags
│       ├── start.go          # start command (orchestration service)
│       ├── linear.go         # linear list command
│       ├── agent.go          # agent run/list commands
│       └── version.go        # version command
├── internal/
│   ├── agent/
│   │   ├── agent.go          # Interface + Registry
│   │   ├── codex.go          # Codex implementation
│   │   ├── claude.go         # Claude Code implementation
│   │   ├── gemini.go         # Gemini CLI implementation
│   │   └── router.go         # Assignee-based agent routing
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
