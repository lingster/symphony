// Package orchestrator implements the main polling loop and agent dispatching.
package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ling/symphony/internal/agent"
	"github.com/ling/symphony/internal/config"
	"github.com/ling/symphony/internal/envfile"
	"github.com/ling/symphony/internal/linear"
	"github.com/ling/symphony/internal/workspace"
)

// Orchestrator manages the polling loop and agent dispatch.
type Orchestrator struct {
	config    *config.Workflow
	tracker   *linear.Client
	workspace *workspace.Manager
	agents    *agent.Registry

	mu                sync.RWMutex
	running           map[string]*RunEntry
	claimed           map[string]struct{}
	retryAttempts     map[string]*RetryEntry
	inProgressLabelID string // cached "AGENT: In Progress" label ID

	showPrompt bool   // when true, print prompts before sending to agents
	botUserID  string // resolved bot user ID for assignee filtering

	wg     sync.WaitGroup // tracks running worker goroutines
	logger *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc
}

// RunEntry tracks a running agent session.
type RunEntry struct {
	IssueID     string
	Identifier  string
	Issue       linear.Issue
	Session     agent.Session
	Agent       agent.Agent
	StartedAt   time.Time
	LastEventAt time.Time
	TurnCount   int
}

// RetryEntry tracks a scheduled retry.
type RetryEntry struct {
	IssueID    string
	Identifier string
	Attempt    int
	DueAt      time.Time
	Error      string
}

// New creates a new orchestrator.
func New(cfg *config.Workflow, logger *slog.Logger) (*Orchestrator, error) {
	if err := cfg.Config.Validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	var tracker *linear.Client
	if cfg.Config.Tracker.AgentToken != "" {
		tracker = linear.NewClientWithToken(
			cfg.Config.Tracker.Endpoint,
			cfg.Config.Tracker.AgentToken,
			cfg.Config.Tracker.OAuthClientID,
			cfg.Config.Tracker.OAuthClientSecret,
			cfg.Config.Tracker.RefreshToken,
		)
		if cfg.EnvFilePath != "" {
			envPath := cfg.EnvFilePath
			tracker.SetOnTokenRefresh(func(accessToken, refreshToken string) {
				kvs := map[string]string{"LINEAR_AGENT_TOKEN": accessToken}
				if refreshToken != "" {
					kvs["LINEAR_REFRESH_TOKEN"] = refreshToken
				}
				if err := envfile.Update(envPath, kvs); err != nil {
					logger.Warn("failed to persist refreshed tokens to .env", "error", err)
				} else {
					logger.Info("persisted refreshed tokens to .env", "path", envPath)
				}
			})
		}
		if cfg.Config.Tracker.FallbackAgentToken != "" {
			tracker.SetFallbackToken(cfg.Config.Tracker.FallbackAgentToken)
		}
	} else {
		tracker = linear.NewClient(cfg.Config.Tracker.Endpoint, cfg.Config.Tracker.APIKey)
	}

	wsMgr := workspace.NewManager(
		cfg.Config.Workspace.Root,
		workspace.Hooks{
			AfterCreate:  cfg.Config.Hooks.AfterCreate,
			BeforeRun:    cfg.Config.Hooks.BeforeRun,
			AfterRun:     cfg.Config.Hooks.AfterRun,
			BeforeRemove: cfg.Config.Hooks.BeforeRemove,
		},
		cfg.Config.Hooks.TimeoutMS,
	)

	// Create agent registry
	agents := agent.NewRegistry()
	agents.Register(agent.NewCodexAgent(cfg.Config.Codex.Command))
	agents.Register(agent.NewClaudeAgent(""))
	agents.Register(agent.NewGeminiAgent(""))

	ctx, cancel := context.WithCancel(context.Background())

	orch := &Orchestrator{
		config:        cfg,
		tracker:       tracker,
		workspace:     wsMgr,
		agents:        agents,
		running:       make(map[string]*RunEntry),
		claimed:       make(map[string]struct{}),
		retryAttempts: make(map[string]*RetryEntry),
		logger:        logger,
		ctx:           ctx,
		cancel:        cancel,
	}

	// Resolve "AGENT: In Progress" label ID
	teamKey := cfg.Config.Tracker.TeamKey
	if teamKey != "" {
		labelName := cfg.Config.Agent.InProgressLabel
		labelID, err := tracker.FindOrCreateLabel(ctx, teamKey, labelName, "#4EA7FC")
		if err != nil {
			logger.Warn("failed to resolve in-progress label, label lifecycle disabled", "error", err)
		} else {
			orch.inProgressLabelID = labelID
			logger.Info("resolved in-progress label", "label", labelName, "id", labelID)
		}
	}

	// Resolve bot user ID for assignee filtering
	if cfg.Config.Tracker.FilterByAssignee {
		viewer, err := tracker.FetchViewer(ctx)
		if err != nil {
			logger.Warn("failed to resolve bot user for assignee filtering, filter disabled", "error", err)
		} else {
			orch.botUserID = viewer.ID
			logger.Info("assignee filtering enabled", "bot_user", viewer.Name, "bot_user_id", viewer.ID)
		}
	}

	return orch, nil
}

// Start begins the orchestration loop.
func (o *Orchestrator) Start() error {
	// Ensure workspace root exists
	if err := o.workspace.EnsureRoot(); err != nil {
		return fmt.Errorf("failed to create workspace root: %w", err)
	}

	// Startup terminal workspace cleanup
	if err := o.startupCleanup(); err != nil {
		o.logger.Warn("startup cleanup failed", "error", err)
	}

	// Start polling loop
	go o.pollLoop()

	o.logger.Info("orchestrator started",
		"polling_interval_ms", o.config.Config.Polling.IntervalMS,
		"max_concurrent_agents", o.config.Config.Agent.MaxConcurrentAgents,
	)

	return nil
}

// GracefulStop stops accepting new work and waits for running agents to
// complete their current tasks. Returns true if all agents finished, false
// if the context was cancelled (e.g. a second signal arrived) before they
// completed.
func (o *Orchestrator) GracefulStop() bool {
	o.cancel() // stop polling loop and prevent new dispatches

	o.logger.Info("waiting for running agents to complete...")

	done := make(chan struct{})
	go func() {
		o.wg.Wait()
		close(done)
	}()

	o.mu.RLock()
	count := len(o.running)
	o.mu.RUnlock()

	if count > 0 {
		o.logger.Info("agents still running, waiting for completion", "count", count)
	}

	<-done
	return true
}

// ForceStop immediately kills all running agent sessions.
func (o *Orchestrator) ForceStop() {
	o.cancel()

	o.mu.Lock()
	for _, entry := range o.running {
		entry.Session.Stop()
	}
	o.mu.Unlock()
}

// RunningCount returns the number of currently running agents.
func (o *Orchestrator) RunningCount() int {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return len(o.running)
}

func (o *Orchestrator) pollLoop() {
	ticker := time.NewTicker(time.Duration(o.config.Config.Polling.IntervalMS) * time.Millisecond)
	defer ticker.Stop()

	// Immediate first tick
	o.tick()

	for {
		select {
		case <-o.ctx.Done():
			return
		case <-ticker.C:
			o.tick()
		}
	}
}

func (o *Orchestrator) tick() {
	// Reconcile running issues
	o.reconcile()

	// Process retry queue
	o.processRetries()

	// Fetch candidate issues
	issues, err := o.tracker.FetchCandidateIssues(
		o.ctx,
		o.issueFilter(),
		o.config.Config.Tracker.ActiveStates,
	)
	if err != nil {
		o.logger.Error("failed to fetch candidates", "error", err)
		return
	}

	// Sort issues for dispatch priority
	sortForDispatch(issues, o.config.Config.Agent.ProjectPriority)

	// Dispatch eligible issues
	for _, issue := range issues {
		if !o.hasAvailableSlots() {
			break
		}
		if o.shouldDispatch(issue) {
			o.dispatch(issue)
		}
	}
}

func (o *Orchestrator) reconcile() {
	o.mu.RLock()
	runningIDs := make([]string, 0, len(o.running))
	for id := range o.running {
		runningIDs = append(runningIDs, id)
	}
	o.mu.RUnlock()

	if len(runningIDs) == 0 {
		return
	}

	// Stall detection
	o.checkStalls()

	// Fetch current states
	issues, err := o.tracker.FetchIssuesByIDs(o.ctx, runningIDs)
	if err != nil {
		o.logger.Debug("state refresh failed, keeping workers", "error", err)
		return
	}

	issueMap := make(map[string]linear.Issue)
	for _, issue := range issues {
		issueMap[issue.ID] = issue
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	for id, entry := range o.running {
		issue, ok := issueMap[id]
		if !ok {
			// Issue not found, keep running
			continue
		}

		stateLower := strings.ToLower(issue.State)

		// Check if terminal
		isTerminal := false
		for _, s := range o.config.Config.Tracker.TerminalStates {
			if strings.ToLower(s) == stateLower {
				isTerminal = true
				break
			}
		}

		if isTerminal {
			o.logger.Info("stopping run (terminal state)",
				"issue_id", id,
				"issue_identifier", entry.Identifier,
				"state", issue.State,
			)
			entry.Session.Stop()
			delete(o.running, id)
			delete(o.claimed, id)

			// Clean workspace
			go o.workspace.Remove(o.ctx, entry.Identifier)
			continue
		}

		// Check if still active
		isActive := false
		for _, s := range o.config.Config.Tracker.ActiveStates {
			if strings.ToLower(s) == stateLower {
				isActive = true
				break
			}
		}

		if !isActive {
			o.logger.Info("stopping run (non-active state)",
				"issue_id", id,
				"issue_identifier", entry.Identifier,
				"state", issue.State,
			)
			entry.Session.Stop()
			delete(o.running, id)
			delete(o.claimed, id)
			continue
		}

		// Update issue snapshot
		entry.Issue = issue
	}
}

func (o *Orchestrator) checkStalls() {
	stallTimeout := time.Duration(o.config.Config.Codex.StallTimeoutMS) * time.Millisecond
	if stallTimeout <= 0 {
		return
	}

	now := time.Now()
	o.mu.Lock()
	defer o.mu.Unlock()

	for id, entry := range o.running {
		lastEvent := entry.LastEventAt
		if lastEvent.IsZero() {
			lastEvent = entry.StartedAt
		}

		if now.Sub(lastEvent) > stallTimeout {
			o.logger.Warn("killing stalled session",
				"issue_id", id,
				"issue_identifier", entry.Identifier,
				"elapsed", now.Sub(lastEvent),
			)
			entry.Session.Stop()
			delete(o.running, id)

			// Schedule retry
			o.scheduleRetry(id, entry.Identifier, 1, "session stalled")
		}
	}
}

func (o *Orchestrator) processRetries() {
	now := time.Now()
	o.mu.Lock()
	defer o.mu.Unlock()

	for id, entry := range o.retryAttempts {
		if now.After(entry.DueAt) {
			delete(o.retryAttempts, id)

			// Re-check if we can dispatch
			// This will be picked up in the next tick
			o.logger.Debug("retry due",
				"issue_id", id,
				"issue_identifier", entry.Identifier,
				"attempt", entry.Attempt,
			)
		}
	}
}

func (o *Orchestrator) hasAvailableSlots() bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return len(o.running) < o.config.Config.Agent.MaxConcurrentAgents
}

func (o *Orchestrator) shouldDispatch(issue linear.Issue) bool {
	o.mu.RLock()
	defer o.mu.RUnlock()

	// Check required fields
	if issue.ID == "" || issue.Identifier == "" || issue.Title == "" || issue.State == "" {
		return false
	}

	// Check if already running or claimed
	if _, ok := o.running[issue.ID]; ok {
		return false
	}
	if _, ok := o.claimed[issue.ID]; ok {
		return false
	}

	// Skip issues that already have the "AGENT: In Progress" label
	inProgressLabel := strings.ToLower(o.config.Config.Agent.InProgressLabel)
	for _, label := range issue.Labels {
		if label == inProgressLabel {
			return false
		}
	}

	// Check blocker rule for Todo state
	stateLower := strings.ToLower(issue.State)
	if stateLower == "todo" {
		for _, blocker := range issue.BlockedBy {
			isTerminal := false
			for _, s := range o.config.Config.Tracker.TerminalStates {
				if strings.ToLower(s) == strings.ToLower(blocker.State) {
					isTerminal = true
					break
				}
			}
			if !isTerminal {
				return false // Has non-terminal blocker
			}
		}
	}

	// Check per-state concurrency limit
	if limit, ok := o.config.Config.Agent.MaxConcurrentAgentsByState[stateLower]; ok {
		count := 0
		for _, entry := range o.running {
			if strings.ToLower(entry.Issue.State) == stateLower {
				count++
			}
		}
		if count >= limit {
			return false
		}
	}

	return true
}

func (o *Orchestrator) dispatch(issue linear.Issue) {
	o.mu.Lock()
	o.claimed[issue.ID] = struct{}{}
	o.mu.Unlock()

	// Add "AGENT: In Progress" label
	o.addInProgressLabel(issue)

	// Select agent based on labels, then assignee
	selectedAgent := o.selectAgent(issue)

	o.wg.Add(1)
	go o.runWorker(issue, selectedAgent)
}

func (o *Orchestrator) addInProgressLabel(issue linear.Issue) {
	if o.inProgressLabelID == "" {
		return
	}

	// Check if already has the label
	for _, id := range issue.LabelIDs {
		if id == o.inProgressLabelID {
			return
		}
	}

	labelIDs := append(issue.LabelIDs, o.inProgressLabelID)
	if err := o.tracker.UpdateIssueLabels(o.ctx, issue.ID, labelIDs); err != nil {
		o.logger.Warn("failed to add in-progress label", "issue", issue.Identifier, "error", err)
	} else {
		o.logger.Debug("added in-progress label", "issue", issue.Identifier)
	}
}

func (o *Orchestrator) removeInProgressLabel(issue linear.Issue) {
	if o.inProgressLabelID == "" {
		return
	}

	var labelIDs []string
	for _, id := range issue.LabelIDs {
		if id != o.inProgressLabelID {
			labelIDs = append(labelIDs, id)
		}
	}

	// Only update if we actually removed something
	if len(labelIDs) == len(issue.LabelIDs) {
		return
	}

	if err := o.tracker.UpdateIssueLabels(o.ctx, issue.ID, labelIDs); err != nil {
		o.logger.Warn("failed to remove in-progress label", "issue", issue.Identifier, "error", err)
	} else {
		o.logger.Debug("removed in-progress label", "issue", issue.Identifier)
	}
}

func (o *Orchestrator) selectAgent(issue linear.Issue) agent.Agent {
	defaultAgent := o.config.Config.Agent.Default
	if defaultAgent == "" {
		defaultAgent = "codex"
	}

	// Check labels first (exact or prefixed: "claude", "agent:claude", "agent: claude")
	agentNames := []string{"codex", "claude", "gemini"}
	for _, label := range issue.Labels {
		for _, agentName := range agentNames {
			if matchesAgentLabel(label, agentName) {
				if a, ok := o.agents.Get(agentName); ok {
					return a
				}
			}
		}
	}

	// Fall back to assignee-based routing
	if issue.Assignee != nil {
		name := strings.ToLower(issue.Assignee.Name)
		username := strings.ToLower(issue.Assignee.Username)

		for _, agentName := range agentNames {
			if strings.Contains(name, agentName) || strings.Contains(username, agentName) {
				if a, ok := o.agents.Get(agentName); ok {
					return a
				}
			}
		}
	}

	// Use default
	if a, ok := o.agents.Get(defaultAgent); ok {
		return a
	}

	// Fallback to codex
	a, _ := o.agents.Get("codex")
	return a
}

// matchesAgentLabel checks if a label matches an agent name.
// Supports: "claude", "agent:claude", "agent: claude" (case-insensitive, labels already lowered).
func matchesAgentLabel(label, agentName string) bool {
	if label == agentName {
		return true
	}
	// Check prefix patterns: "agent:X", "agent: X"
	prefixes := []string{"agent:", "agent: "}
	for _, prefix := range prefixes {
		if strings.HasPrefix(label, prefix) {
			if strings.TrimSpace(label[len(prefix):]) == agentName {
				return true
			}
		}
	}
	return false
}

func (o *Orchestrator) runWorker(issue linear.Issue, selectedAgent agent.Agent) {
	defer o.wg.Done()
	ctx := o.ctx
	logger := o.logger.With(
		"issue_id", issue.ID,
		"issue_identifier", issue.Identifier,
		"agent", selectedAgent.Name(),
	)

	logger.Info("starting worker")

	// Create/reuse workspace
	ws, err := o.workspace.Create(ctx, issue.Identifier)
	if err != nil {
		logger.Error("workspace creation failed", "error", err)
		o.handleWorkerFailure(issue, 1, err.Error())
		return
	}

	// Prepare workspace
	if err := o.workspace.Prepare(ctx, ws); err != nil {
		logger.Error("workspace preparation failed", "error", err)
		o.handleWorkerFailure(issue, 1, err.Error())
		return
	}

	defer o.workspace.Finish(ctx, ws)

	// Build prompt
	prompt, err := o.buildPrompt(issue, nil)
	if err != nil {
		logger.Error("prompt building failed", "error", err)
		o.handleWorkerFailure(issue, 1, err.Error())
		return
	}

	if o.showPrompt {
		fmt.Fprintf(os.Stdout, "\n--- Prompt for %s ---\n\n%s\n\n--- End Prompt ---\n\n", issue.Identifier, prompt)
	}

	// Start agent session
	session, err := selectedAgent.Start(ctx, ws.Path, prompt, o.config.SkillContent)
	if err != nil {
		logger.Error("agent start failed", "error", err)
		o.handleWorkerFailure(issue, 1, err.Error())
		return
	}

	entry := &RunEntry{
		IssueID:    issue.ID,
		Identifier: issue.Identifier,
		Issue:      issue,
		Session:    session,
		Agent:      selectedAgent,
		StartedAt:  time.Now(),
		TurnCount:  1,
	}

	o.mu.Lock()
	o.running[issue.ID] = entry
	o.mu.Unlock()

	// Process events
	completedSuccessfully := false
	for event := range session.Events() {
		entry.LastEventAt = time.Now()

		switch event.Type {
		case agent.EventTypeMessage:
			logger.Debug("agent message", "content", truncate(event.Content, 100))
			if event.Content != "" {
				fmt.Fprintf(os.Stdout, "[%s] %s\n", issue.Identifier, event.Content)
			}
		case agent.EventTypeToolUse:
			logger.Debug("agent tool use", "tool", event.Content)
			fmt.Fprintf(os.Stdout, "[%s] > %s\n", issue.Identifier, event.Content)
		case agent.EventTypeToolResult:
			logger.Debug("agent tool result", "content", truncate(event.Content, 200))
			if event.Content != "" {
				fmt.Fprintf(os.Stdout, "[%s]   %s\n", issue.Identifier, truncate(event.Content, 300))
			}
		case agent.EventTypeError:
			logger.Warn("agent error", "error", event.Content)
			fmt.Fprintf(os.Stdout, "[%s] ERROR: %s\n", issue.Identifier, event.Content)
		case agent.EventTypeComplete:
			logger.Info("agent completed", "details", event.Content)
			fmt.Fprintf(os.Stdout, "[%s] Completed: %s\n", issue.Identifier, event.Content)
			if event.ResultText != "" {
				fmt.Fprintf(os.Stdout, "\n=== Agent Result [%s] ===\n%s\n=== End Result ===\n\n",
					issue.Identifier, event.ResultText)
			}
			if strings.Contains(event.Content, "status=success") {
				completedSuccessfully = true
			}
		}
	}

	// Wait for session to fully complete
	if err := session.Wait(); err != nil {
		logger.Warn("session wait error", "error", err)
	}

	// Remove from running
	o.mu.Lock()
	delete(o.running, issue.ID)
	o.mu.Unlock()

	// Remove "AGENT: In Progress" label
	o.removeInProgressLabel(issue)

	if completedSuccessfully {
		logger.Info("task completed successfully, no retry needed",
			"issue_identifier", issue.Identifier,
		)
	} else {
		// Schedule continuation retry only if the agent did not complete successfully
		o.scheduleRetry(issue.ID, issue.Identifier, 1, "")
	}

	logger.Info("worker completed")
}

func (o *Orchestrator) handleWorkerFailure(issue linear.Issue, attempt int, errMsg string) {
	o.mu.Lock()
	delete(o.running, issue.ID)
	o.mu.Unlock()

	// Remove "AGENT: In Progress" label on failure
	o.removeInProgressLabel(issue)

	o.scheduleRetry(issue.ID, issue.Identifier, attempt, errMsg)
}

func (o *Orchestrator) scheduleRetry(issueID, identifier string, attempt int, errMsg string) {
	// Calculate backoff
	var delay time.Duration
	if errMsg == "" {
		// Continuation retry
		delay = time.Second
	} else {
		// Failure retry with exponential backoff
		baseDelay := 10 * time.Second
		maxBackoff := time.Duration(o.config.Config.Agent.MaxRetryBackoffMS) * time.Millisecond

		delay = baseDelay * (1 << (attempt - 1))
		if delay > maxBackoff {
			delay = maxBackoff
		}
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	o.retryAttempts[issueID] = &RetryEntry{
		IssueID:    issueID,
		Identifier: identifier,
		Attempt:    attempt,
		DueAt:      time.Now().Add(delay),
		Error:      errMsg,
	}

	o.logger.Debug("scheduled retry",
		"issue_id", issueID,
		"issue_identifier", identifier,
		"attempt", attempt,
		"delay", delay,
	)
}

func (o *Orchestrator) buildPrompt(issue linear.Issue, attempt *int) (string, error) {
	template := o.config.PromptTemplate
	if template == "" {
		template = "You are working on an issue from Linear."
	}

	// Simple template substitution
	prompt := template
	prompt = strings.ReplaceAll(prompt, "{{issue.id}}", issue.ID)
	prompt = strings.ReplaceAll(prompt, "{{issue.identifier}}", issue.Identifier)
	prompt = strings.ReplaceAll(prompt, "{{issue.title}}", issue.Title)
	prompt = strings.ReplaceAll(prompt, "{{issue.description}}", issue.Description)
	prompt = strings.ReplaceAll(prompt, "{{issue.state}}", issue.State)
	prompt = strings.ReplaceAll(prompt, "{{issue.url}}", issue.URL)
	prompt = strings.ReplaceAll(prompt, "{{issue.branch_name}}", issue.BranchName)
	prompt = strings.ReplaceAll(prompt, "{{issue.project}}", issue.Project)
	prompt = strings.ReplaceAll(prompt, "{{issue.labels}}", strings.Join(issue.Labels, ", "))

	if attempt != nil {
		prompt = strings.ReplaceAll(prompt, "{{attempt}}", fmt.Sprintf("%d", *attempt))
	} else {
		prompt = strings.ReplaceAll(prompt, "{{attempt}}", "")
	}

	return prompt, nil
}

func (o *Orchestrator) startupCleanup() error {
	// Fetch terminal issues
	issues, err := o.tracker.FetchIssuesByStates(
		o.ctx,
		o.issueFilter(),
		o.config.Config.Tracker.TerminalStates,
	)
	if err != nil {
		return fmt.Errorf("failed to fetch terminal issues: %w", err)
	}

	// Remove workspaces for terminal issues
	for _, issue := range issues {
		if err := o.workspace.Remove(o.ctx, issue.Identifier); err != nil {
			o.logger.Warn("failed to remove terminal workspace",
				"issue_identifier", issue.Identifier,
				"error", err,
			)
		}
	}

	return nil
}

// State returns the current orchestrator state for monitoring.
func (o *Orchestrator) State() OrchestratorState {
	o.mu.RLock()
	defer o.mu.RUnlock()

	state := OrchestratorState{
		RunningCount:  len(o.running),
		RetryingCount: len(o.retryAttempts),
		Running:       make([]RunningInfo, 0, len(o.running)),
		Retrying:      make([]RetryInfo, 0, len(o.retryAttempts)),
	}

	for _, entry := range o.running {
		state.Running = append(state.Running, RunningInfo{
			IssueID:     entry.IssueID,
			Identifier:  entry.Identifier,
			State:       entry.Issue.State,
			Agent:       entry.Agent.Name(),
			StartedAt:   entry.StartedAt,
			LastEventAt: entry.LastEventAt,
			TurnCount:   entry.TurnCount,
		})
	}

	for _, entry := range o.retryAttempts {
		state.Retrying = append(state.Retrying, RetryInfo{
			IssueID:    entry.IssueID,
			Identifier: entry.Identifier,
			Attempt:    entry.Attempt,
			DueAt:      entry.DueAt,
			Error:      entry.Error,
		})
	}

	return state
}

// OrchestratorState represents the runtime state snapshot.
type OrchestratorState struct {
	RunningCount  int           `json:"running_count"`
	RetryingCount int           `json:"retrying_count"`
	Running       []RunningInfo `json:"running"`
	Retrying      []RetryInfo   `json:"retrying"`
}

// RunningInfo contains info about a running session.
type RunningInfo struct {
	IssueID     string    `json:"issue_id"`
	Identifier  string    `json:"issue_identifier"`
	State       string    `json:"state"`
	Agent       string    `json:"agent"`
	StartedAt   time.Time `json:"started_at"`
	LastEventAt time.Time `json:"last_event_at"`
	TurnCount   int       `json:"turn_count"`
}

// RetryInfo contains info about a pending retry.
type RetryInfo struct {
	IssueID    string    `json:"issue_id"`
	Identifier string    `json:"issue_identifier"`
	Attempt    int       `json:"attempt"`
	DueAt      time.Time `json:"due_at"`
	Error      string    `json:"error,omitempty"`
}

// sortForDispatch sorts issues for dispatch priority using project ranking,
// state preference, Linear priority, and staleness.
func sortForDispatch(issues []linear.Issue, projectPriority []string) {
	// Build project rank lookup (lower rank = higher priority)
	projectRank := make(map[string]int)
	for i, name := range projectPriority {
		projectRank[strings.ToLower(name)] = i
	}

	// State rank: "In Progress" first (resume), then "Todo", then everything else
	stateRank := func(state string) int {
		switch strings.ToLower(state) {
		case "in progress":
			return 0
		case "todo":
			return 1
		case "backlog":
			return 2
		default:
			return 3
		}
	}

	sort.SliceStable(issues, func(i, j int) bool {
		// 1. Project priority (if configured)
		if len(projectPriority) > 0 {
			pi := len(projectPriority) // unlisted projects sort last
			pj := len(projectPriority)
			if rank, ok := projectRank[strings.ToLower(issues[i].Project)]; ok {
				pi = rank
			}
			if rank, ok := projectRank[strings.ToLower(issues[j].Project)]; ok {
				pj = rank
			}
			if pi != pj {
				return pi < pj
			}
		}

		// 2. State: In Progress > Todo > Backlog > other
		si, sj := stateRank(issues[i].State), stateRank(issues[j].State)
		if si != sj {
			return si < sj
		}

		// 3. Linear priority ascending (1=Urgent, 2=High, etc.; 0/nil = no priority = lowest)
		pi, pj := 999, 999
		if issues[i].Priority != nil && *issues[i].Priority > 0 {
			pi = *issues[i].Priority
		}
		if issues[j].Priority != nil && *issues[j].Priority > 0 {
			pj = *issues[j].Priority
		}
		if pi != pj {
			return pi < pj
		}

		// 4. Staleness: least recently updated first (waiting longest)
		if !issues[i].UpdatedAt.Equal(issues[j].UpdatedAt) {
			return issues[i].UpdatedAt.Before(issues[j].UpdatedAt)
		}

		// 5. Identifier (stable tiebreaker)
		return issues[i].Identifier < issues[j].Identifier
	})
}

// Tracker returns the underlying Linear client.
func (o *Orchestrator) Tracker() *linear.Client {
	return o.tracker
}

// Agents returns the agent registry.
func (o *Orchestrator) Agents() *agent.Registry {
	return o.agents
}

// BuildPromptForIssue builds a prompt for a given issue (public wrapper).
func (o *Orchestrator) BuildPromptForIssue(issue linear.Issue) (string, error) {
	return o.buildPrompt(issue, nil)
}

// SkillContent returns the loaded skill content from the workflow configuration.
func (o *Orchestrator) SkillContent() string {
	return o.config.SkillContent
}

// SetShowPrompt enables printing prompts before sending to agents.
func (o *Orchestrator) SetShowPrompt(show bool) {
	o.showPrompt = show
}

// SelectAgentForIssue selects an agent for a given issue (public wrapper).
func (o *Orchestrator) SelectAgentForIssue(issue linear.Issue) agent.Agent {
	return o.selectAgent(issue)
}

// FetchDispatchQueue fetches candidate issues, sorts them by dispatch priority,
// and filters to only those that would be dispatched. Used by --dry-run.
func (o *Orchestrator) FetchDispatchQueue(ctx context.Context) ([]linear.Issue, error) {
	issues, err := o.tracker.FetchCandidateIssues(
		ctx,
		o.issueFilter(),
		o.config.Config.Tracker.ActiveStates,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch candidates: %w", err)
	}

	sortForDispatch(issues, o.config.Config.Agent.ProjectPriority)

	var queue []linear.Issue
	for _, issue := range issues {
		if o.shouldDispatch(issue) {
			queue = append(queue, issue)
		}
	}

	return queue, nil
}

// IssueFilter returns the configured issue filter.
func (o *Orchestrator) IssueFilter() linear.IssueFilter {
	return o.issueFilter()
}

func (o *Orchestrator) issueFilter() linear.IssueFilter {
	return linear.IssueFilter{
		ProjectSlug: o.config.Config.Tracker.ProjectSlug,
		TeamKey:     o.config.Config.Tracker.TeamKey,
		AssigneeID:  o.botUserID,
	}
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
