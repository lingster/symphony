---
tracker:
  kind: linear
  team_key: "NUM"
  agent_token: $LINEAR_AGENT_TOKEN
  oauth_client_id: $LINEAR_OAUTH_CLIENT_ID
  oauth_client_secret: $LINEAR_OAUTH_CLIENT_SECRET
  refresh_token: $LINEAR_REFRESH_TOKEN
  api_key: $LINEAR_API_KEY
  active_states: ["Todo", "In Progress"]
  terminal_states: ["Done", "Closed", "Cancelled", "Canceled", "Duplicate"]

polling:
  interval_ms: 30000

workspace:
  root: ~/workspaces/symphony

hooks:
  # after_create: |
  #   git clone $REPO_URL .
  # before_run: |
  #   git pull origin main
  # after_run: |
  #   git add -A && git commit -m "symphony: {{issue.identifier}} automated changes" || true
  # before_remove: |
  #   echo "Cleaning up workspace for {{issue.identifier}}"
  timeout_ms: 60000

agent:
  max_concurrent_agents: 4
  max_turns: 20
  max_retry_backoff_ms: 300000
  default: codex
  in_progress_label: "AGENT: In Progress"
  # skill_path: ~/path/to/SKILL.md
  # project_priority:
  #   - "Project Alpha"
  #   - "Project Beta"

codex:
  command: codex app-server
  stall_timeout_ms: 300000
---

# Task Prompt

You are working on issue {{issue.identifier}}: {{issue.title}}

- **URL:** {{issue.url}}
- **State:** {{issue.state}}
- **Labels:** {{issue.labels}}
- **Branch:** {{issue.branch_name}}

## Description
{{issue.description}}

## Instructions
1. Create a worktree or working branch named `feature/{{issue.identifier}}`
2. Read the codebase and understand the existing code structure
3. Implement the required changes described above
4. Run any existing tests to verify your changes
5. Commit your work with a clear, descriptive commit message referencing {{issue.identifier}}
