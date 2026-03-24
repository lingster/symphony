---
tracker:
  kind: linear
  team_key: "NUM"
  agent_token: $LINEAR_AGENT_TOKEN
  oauth_client_id: $LINEAR_OAUTH_CLIENT_ID
  oauth_client_secret: $LINEAR_OAUTH_CLIENT_SECRET
  refresh_token: $LINEAR_REFRESH_TOKEN
  api_key: $LINEAR_API_KEY
  filter_by_assignee: true
  active_states: ["Todo", "In Progress"]
  terminal_states: ["Done", "Closed", "Cancelled", "Canceled", "Duplicate"]

tmux:
  inject_delay_ms: 5000
  agents:
    claude: ["claude", "/model opus"]
    codex: ["codex"]
    gemini: ["gemini"]

polling:
  interval: 30m

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
  max_concurrent_agents: 1
  max_turns: 30
  max_retry_backoff_ms: 300000
  # claude | gemini | codex
  default: claude  
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

1a. Your workarea will be a folder called workarea/{reponame} eg workarea/crowdcent-lstm. There are 2 folders: main and develop. The main contains check out of the main branch and develop current contains feature/frontend, which we consider as the develop branch and is the branch used to deploy to the develop environment.

2. **Pull latest develop branch** before creating or resuming any feature branch:
   ```bash
   cd workarea/crowdcent-lstm/develop
   git fetch origin
   git pull origin feature/frontend
   ```
   This ensures your feature branch will be based on the most recent code.

3. **Create or resume the feature branch** named `feature/{{issue.identifier}}` at location workarea/crowdcent-lstm/{{issue.identifier}}:
   - **If the worktree/branch does not exist yet:** create it from the develop branch:
     ```bash
     cd workarea/crowdcent-lstm/develop
     git worktree add ../{{issue.identifier}} -b feature/{{issue.identifier}}
     ```
   - **If the worktree/branch already exists:** pull the latest code before continuing:
     ```bash
     cd workarea/crowdcent-lstm/{{issue.identifier}}
     git fetch origin
     git pull origin feature/{{issue.identifier}} || true
     git merge feature/frontend --no-edit || true
     ```

4. **Read the codebase** and understand the existing code structure

5. **Implement the required changes** described in the issue description above, however if anything is unclear please respond and add a label "Human Required" and consider this task complete. When addressing the task, if you encounter any unleated issues feel free to create a linear sub-task and label this "Human Required" and continue solving the original issue. Always use red/green TDD, SOLID, DRY coding principals.

6. **Run any existing tests** to verify your changes

7. **Commit your work** with a clear, descriptive commit message referencing {{issue.identifier}}, after committing and pushing code, wait 20 mins and check github actions for any code review commits and ensure that you resolve each one, but addressing it and adding summary of what you did to resolve it. Repeat until all code review comments have been resolved.

8. **Create an implementation summary** after pushing to remote. The summary MUST include all of the following sections:

   ```
   ## {{issue.identifier}} Implementation Summary

   ### Status
   <One-line status, e.g. "Bug fixed and PR created." or "Feature implemented and PR created.">

   ### Root Cause (if bug fix)
   <What caused the issue — be specific about the technical root cause>

   ### What was done
   <Bullet list of changes made, including any preventive/follow-up fixes>

   ### Evidence
   <Logs, test output, or other proof the fix/feature works>

   ### Links
   - **PR:** <GitHub PR URL — obtain via `gh pr view --json url -q .url` or from `gh pr create` output>
   - **Branch:** `feature/{{issue.identifier}}`
   - **Preview:** <If a PR preview deployment is available, include the URL. Check the repo's PR preview pattern, e.g. `https://preview-pr-<NUMBER>.example.com` or similar. If no preview deployment is configured, omit this line.>
   ```

9. **Post the summary as a Linear comment** and update status to "Done", and add label "Human Required":
   ```bash
   bash /home/ling/workarea/lingster/symphony/linear-claude-skill/scripts/query.sh 'mutation { commentCreate(input: { issueId: "{{issue.id}}", body: "YOUR_SUMMARY_HERE" }) { success } }'
   bash /home/ling/workarea/lingster/symphony/linear-claude-skill/scripts/query.sh 'mutation { issueUpdate(id: "{{issue.id}}", input: { stateId: "e19a65e1-753e-462d-8bc1-6fecea27dc7b" }) { success } }'
   ```

10. **Output the summary as your final response** so it is visible in stdout. This is your last action — after outputting the summary, the task is complete.
