# Symphony Setup Guide

This guide walks through the prerequisites and configuration needed to run Symphony.

## Prerequisites

- Go 1.21+
- A Linear workspace with admin access
- One of: Linear personal API key or OAuth application credentials
- A coding agent CLI installed: `claude`, `gemini`, or `codex`

## 1. Build Symphony

```bash
cd go/
go build -o symphony ./cmd/symphony
```

## 2. Configure Linear Credentials

Copy the example env file and fill in your credentials:

```bash
cp .env.example .env
```

See the [Authentication section in README.md](../README.md#authentication) for details on personal API key vs OAuth agent token setup.

## 3. Create Required Linear Labels

Symphony uses labels to track agent status and outcomes on issues. These labels **must be created manually** in Linear because the OAuth bot token does not have permission to create labels.

### How to create labels

1. Open Linear and go to **Settings** (gear icon)
2. Navigate to **Workspace > Labels**
3. Create a label group called **AGENT** (optional, for organisation)
4. Create the following labels (inside the AGENT group if you made one):

| Label | Color | Purpose |
|---|---|---|
| `AGENT: In Progress` | Blue (`#4EA7FC`) | Applied when an agent starts working on an issue |
| `AGENT: Success` | Green (`#00aa00`) | Applied when the agent completes successfully |
| `AGENT: Failure` | Red (`#cc0000`) | Applied when the agent fails |
| `AGENT: Cancelled` | Yellow (`#ccaa00`) | Applied when the agent run is cancelled |

Additionally, create this label if your workflow uses it:

| Label | Color | Purpose |
|---|---|---|
| `Human Required` | Blue (`#4EA7FC`) | Applied when the agent needs human intervention |

### Why manual creation is required

Linear's OAuth `actor=app` tokens cannot create labels — label management is considered an admin action. The `issueLabelCreate` mutation returns `"not allowed to take action"` for bot tokens. Pre-creating the labels allows the bot to find and apply them without needing create permissions.

### Verify labels are visible to the bot

After creating the labels, verify the bot can see them:

```bash
bash /path/to/linear-claude-skill/scripts/query.sh 'query { issueLabels(filter: { name: { containsIgnoreCase: "agent" } }) { nodes { id name color } } }'
```

You should see all four AGENT labels in the response.

## 4. Configure WORKFLOW.md

Create or edit your `WORKFLOW.md` file with YAML front matter. See [WORKFLOW.md](../WORKFLOW.md) for a complete example.

Key configuration sections:

### Tracker

```yaml
tracker:
  kind: linear
  team_key: "YOUR_TEAM_KEY"
  api_key: $LINEAR_API_KEY
  active_states: ["Todo", "In Progress"]
  terminal_states: ["Done", "Closed", "Cancelled", "Canceled", "Duplicate"]
```

### Agent

```yaml
agent:
  max_concurrent_agents: 1
  max_turns: 30
  default: claude
  in_progress_label: "AGENT: In Progress"
```

The `in_progress_label` value **must match exactly** the label name you created in step 3.

### Workspace

```yaml
workspace:
  root: ./workarea
```

This is where Symphony creates per-issue working directories.

## 5. Start Symphony

```bash
./symphony start
```

Or with a specific workflow file:

```bash
./symphony start path/to/WORKFLOW.md
```

## Troubleshooting

### "failed to find/create outcome label"

The AGENT labels haven't been created in Linear. Follow step 3 above.

### "not allowed to take action"

The bot token lacks permission for the operation. Common causes:
- **Label creation**: Create labels manually (step 3)
- **Issue updates**: Ensure the bot has member-level access to the team
- **Token expired**: Check if the OAuth token needs refreshing

### Labels not visible to the bot

If you created labels but the bot can't find them:
- Ensure they are **workspace-level labels**, not team-specific labels that the bot can't access
- Check that the label names match exactly (including spaces and capitalisation)
- Verify with the query in step 3
