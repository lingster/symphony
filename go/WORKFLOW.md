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
  root: ./workarea 

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
  skill_path: ../linear-claude-skill/SKILL.md
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
- **Issue UUID:** {{issue.id}}

## Description
{{issue.description}}

## Linear Issue Management

You MUST update the Linear issue status as you work. Use the following script to run GraphQL mutations against the Linear API:

**Script path:** `/home/ling/workarea/lingster/symphony/linear-claude-skill/scripts/query.sh`

### Linear State IDs (NUM team)
- **Backlog:** `56cf3167-792f-4f0a-a418-e562d7dc63f3`
- **Todo:** `a250ceb8-0fa4-4b17-b796-a905516eb58d`
- **In Progress:** `a66a7ef9-31fd-4aab-9b5b-a96fb383566a`
- **In Review:** `7b33dd1e-b642-44be-b9fa-e9e2484664ac`
- **Done:** `e19a65e1-753e-462d-8bc1-6fecea27dc7b`

### Update issue status
```bash
bash /home/ling/workarea/lingster/symphony/linear-claude-skill/scripts/query.sh 'mutation { issueUpdate(id: "{{issue.id}}", input: { stateId: "STATE_ID" }) { success } }'
```

### Add a comment to the issue
```bash
bash /home/ling/workarea/lingster/symphony/linear-claude-skill/scripts/query.sh 'mutation { commentCreate(input: { issueId: "{{issue.id}}", body: "YOUR_COMMENT" }) { success } }'
```

## Instructions

1. **Update Linear to "In Progress":**
   ```bash
   bash /home/ling/workarea/lingster/symphony/linear-claude-skill/scripts/query.sh 'mutation { issueUpdate(id: "{{issue.id}}", input: { stateId: "a66a7ef9-31fd-4aab-9b5b-a96fb383566a" }) { success } }'
   ```

1a. Your workarea will be a folder called workarea/{reponame} eg workarea/crowdcent-lstm. There are 2 folders: main and develop. The main contains check out of the main branch and develop current contains feature/frontend, which we consider as the develop branch and is the branch used to deploy to the develop environment. Before starting any task also update the feature/frontend to the latest version of code. 

2. **Create a feature branch** named `feature/{{issue.identifier}}` this should be in location workarea/crowdcent-lstm/{{issue.identifier}}

3. **Read the codebase** and understand the existing code structure

4. **Implement the required changes** described in the issue description above, however if anything is unclear please respond and add a label "Human Required" and consider this task complete. When addressing the task, if you encounter any unleated issues feel free to create a linear sub-task and label this "Human Required" and continue solving the original issue. Always use red/green TDD, SOLID, DRY coding principals. 

5. **Run any existing tests** to verify your changes

6. **Commit your work** with a clear, descriptive commit message referencing {{issue.identifier}}, after committing and pushing code, wait 20 mins and check github actions for any code review commits and ensure that you resolve each one, but addressing it and adding summary of what you did to resolve it. Repeat until all code review comments have been resolved. 

7. **Update Linear to "Review"** and add a summary comment and add label Human Required:
   ```bash
   bash /home/ling/workarea/lingster/symphony/linear-claude-skill/scripts/query.sh 'mutation { commentCreate(input: { issueId: "{{issue.id}}", body: "Completed: brief summary of changes made" }) { success } }'
   bash /home/ling/workarea/lingster/symphony/linear-claude-skill/scripts/query.sh 'mutation { issueUpdate(id: "{{issue.id}}", input: { stateId: "e19a65e1-753e-462d-8bc1-6fecea27dc7b" }) { success } }'
   ```
