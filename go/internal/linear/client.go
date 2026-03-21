// Package linear provides a GraphQL client for Linear issue tracking.
package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Issue represents a normalized Linear issue.
type Issue struct {
	ID          string    `json:"id"`
	Identifier  string    `json:"identifier"`
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	Priority    *int      `json:"priority,omitempty"`
	State       string    `json:"state"`
	BranchName  string    `json:"branch_name,omitempty"`
	URL         string    `json:"url,omitempty"`
	Labels      []string  `json:"labels,omitempty"`
	LabelIDs    []string  `json:"label_ids,omitempty"`
	Project     string    `json:"project,omitempty"`
	BlockedBy   []Blocker `json:"blocked_by,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Assignee    *Assignee `json:"assignee,omitempty"`
}

// Assignee represents an issue assignee.
type Assignee struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Username string `json:"username,omitempty"`
	Email    string `json:"email,omitempty"`
}

// Blocker represents a blocking issue reference.
type Blocker struct {
	ID         string `json:"id,omitempty"`
	Identifier string `json:"identifier,omitempty"`
	State      string `json:"state,omitempty"`
}

// TokenRefreshCallback is called after a successful token refresh with the new
// access token and (optionally rotated) refresh token so callers can persist them.
type TokenRefreshCallback func(accessToken, refreshToken string)

// Client is a Linear GraphQL API client.
type Client struct {
	endpoint   string
	apiKey     string
	httpClient *http.Client

	// OAuth fields — set when using bot/agent identity instead of a personal API key.
	accessToken       string
	oauthClientID     string
	oauthClientSecret string
	refreshToken      string

	onTokenRefresh TokenRefreshCallback
	fallbackToken  string // Shell env token to try if primary auth + refresh both fail
}

// NewClient creates a new Linear client using a personal API key.
func NewClient(endpoint, apiKey string) *Client {
	if endpoint == "" {
		endpoint = "https://api.linear.app/graphql"
	}
	return &Client{
		endpoint: endpoint,
		apiKey:   apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// NewClientWithToken creates a Linear client that authenticates with an OAuth
// bearer token (agent/bot identity). When clientID, clientSecret, and
// refreshToken are supplied, the client will automatically refresh an expired
// access token and update the in-memory token for subsequent requests.
func NewClientWithToken(endpoint, accessToken, clientID, clientSecret, refreshToken string) *Client {
	if endpoint == "" {
		endpoint = "https://api.linear.app/graphql"
	}
	return &Client{
		endpoint:          endpoint,
		accessToken:       accessToken,
		oauthClientID:     clientID,
		oauthClientSecret: clientSecret,
		refreshToken:      refreshToken,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// SetOnTokenRefresh registers a callback that is invoked after a successful
// token refresh. This allows callers to persist the new tokens (e.g. to .env).
func (c *Client) SetOnTokenRefresh(cb TokenRefreshCallback) {
	c.onTokenRefresh = cb
}

// SetFallbackToken sets a fallback access token (e.g. from shell environment)
// that will be tried if the primary token and refresh both fail authentication.
func (c *Client) SetFallbackToken(token string) {
	c.fallbackToken = token
}

// graphqlRequest represents a GraphQL request payload.
type graphqlRequest struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables,omitempty"`
}

// graphqlResponse represents a GraphQL response.
type graphqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors,omitempty"`
}

// IssueFilter specifies how to scope issue queries. Set either ProjectSlug
// or TeamKey (TeamKey takes precedence if both are set).
type IssueFilter struct {
	ProjectSlug string
	TeamKey     string
}

// FetchCandidateIssues fetches issues in active states scoped by the filter.
func (c *Client) FetchCandidateIssues(ctx context.Context, filter IssueFilter, activeStates []string) ([]Issue, error) {
	query, variables := c.buildIssueQuery(filter)

	var allIssues []Issue
	var cursor *string

	for {
		vars := make(map[string]interface{})
		for k, v := range variables {
			vars[k] = v
		}
		vars["first"] = 50
		if cursor != nil {
			vars["after"] = *cursor
		}

		resp, err := c.execute(ctx, query, vars)
		if err != nil {
			return nil, err
		}

		var data struct {
			Issues struct {
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
				Nodes []json.RawMessage `json:"nodes"`
			} `json:"issues"`
		}
		if err := json.Unmarshal(resp, &data); err != nil {
			return nil, fmt.Errorf("failed to parse response: %w", err)
		}

		for _, node := range data.Issues.Nodes {
			issue, err := c.parseIssue(node)
			if err != nil {
				continue
			}

			stateLower := strings.ToLower(issue.State)
			isActive := false
			for _, s := range activeStates {
				if strings.ToLower(s) == stateLower {
					isActive = true
					break
				}
			}
			if isActive {
				allIssues = append(allIssues, issue)
			}
		}

		if !data.Issues.PageInfo.HasNextPage {
			break
		}
		cursor = &data.Issues.PageInfo.EndCursor
	}

	return allIssues, nil
}

// issueNodeFragment is the common set of fields fetched for issues.
const issueNodeFragment = `
	id
	identifier
	title
	description
	priority
	url
	branchName
	createdAt
	updatedAt
	state {
		name
	}
	labels {
		nodes {
			id
			name
		}
	}
	project {
		name
	}
	assignee {
		id
		name
		displayName
		email
	}
	relations(first: 50) {
		nodes {
			type
			relatedIssue {
				id
				identifier
				state {
					name
				}
			}
		}
	}
`

func (c *Client) buildIssueQuery(filter IssueFilter) (string, map[string]interface{}) {
	if filter.TeamKey != "" {
		query := `
			query($teamKey: String!, $first: Int!, $after: String) {
				issues(
					filter: {
						team: { key: { eq: $teamKey } }
					}
					first: $first
					after: $after
				) {
					pageInfo {
						hasNextPage
						endCursor
					}
					nodes {
						` + issueNodeFragment + `
					}
				}
			}
		`
		return query, map[string]interface{}{"teamKey": filter.TeamKey}
	}

	query := `
		query($projectSlug: String!, $first: Int!, $after: String) {
			issues(
				filter: {
					project: { slugId: { eq: $projectSlug } }
				}
				first: $first
				after: $after
			) {
				pageInfo {
					hasNextPage
					endCursor
				}
				nodes {
					` + issueNodeFragment + `
				}
			}
		}
	`
	return query, map[string]interface{}{"projectSlug": filter.ProjectSlug}
}

// FetchIssuesByIDs fetches issues by their IDs for reconciliation.
func (c *Client) FetchIssuesByIDs(ctx context.Context, ids []string) ([]Issue, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	query := `
		query($ids: [ID!]!) {
			issues(filter: { id: { in: $ids } }) {
				nodes {
					id
					identifier
					title
					state {
						name
					}
				}
			}
		}
	`

	variables := map[string]interface{}{
		"ids": ids,
	}

	resp, err := c.execute(ctx, query, variables)
	if err != nil {
		return nil, err
	}

	var data struct {
		Issues struct {
			Nodes []json.RawMessage `json:"nodes"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(resp, &data); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	issues := make([]Issue, 0, len(data.Issues.Nodes))
	for _, node := range data.Issues.Nodes {
		issue, err := c.parseIssue(node)
		if err != nil {
			continue
		}
		issues = append(issues, issue)
	}

	return issues, nil
}

// FetchIssuesByStates fetches issues in specific states (for terminal cleanup).
func (c *Client) FetchIssuesByStates(ctx context.Context, filter IssueFilter, states []string) ([]Issue, error) {
	if len(states) == 0 {
		return nil, nil
	}

	// Reuse the same filter-based query builder but with minimal fields
	query, baseVars := c.buildIssueQuery(filter)

	var allIssues []Issue
	var cursor *string

	for {
		vars := make(map[string]interface{})
		for k, v := range baseVars {
			vars[k] = v
		}
		vars["first"] = 50
		if cursor != nil {
			vars["after"] = *cursor
		}

		resp, err := c.execute(ctx, query, vars)
		if err != nil {
			return nil, err
		}

		var data struct {
			Issues struct {
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
				Nodes []json.RawMessage `json:"nodes"`
			} `json:"issues"`
		}
		if err := json.Unmarshal(resp, &data); err != nil {
			return nil, fmt.Errorf("failed to parse response: %w", err)
		}

		for _, node := range data.Issues.Nodes {
			issue, err := c.parseIssue(node)
			if err != nil {
				continue
			}

			stateLower := strings.ToLower(issue.State)
			for _, s := range states {
				if strings.ToLower(s) == stateLower {
					allIssues = append(allIssues, issue)
					break
				}
			}
		}

		if !data.Issues.PageInfo.HasNextPage {
			break
		}
		cursor = &data.Issues.PageInfo.EndCursor
	}

	return allIssues, nil
}

func (c *Client) execute(ctx context.Context, query string, variables map[string]interface{}) (json.RawMessage, error) {
	data, statusCode, err := c.doRequest(ctx, query, variables)
	if err == nil {
		return data, nil
	}

	// On 401, attempt token refresh if OAuth credentials are available.
	if statusCode == http.StatusUnauthorized && c.canRefresh() {
		if refreshErr := c.tryRefreshToken(); refreshErr == nil {
			// Retry with the new token.
			data, _, retryErr := c.doRequest(ctx, query, variables)
			if retryErr == nil {
				return data, nil
			}
		}
	}

	// If still failing and we have a fallback token, try it.
	if statusCode == http.StatusUnauthorized && c.fallbackToken != "" && c.fallbackToken != c.accessToken {
		c.accessToken = c.fallbackToken
		c.fallbackToken = "" // Don't retry fallback again
		data, _, fallbackErr := c.doRequest(ctx, query, variables)
		if fallbackErr == nil {
			return data, nil
		}
		// Return original error since fallback also failed
	}

	return data, err
}

// canRefresh returns true when the client has the OAuth credentials needed to
// obtain a new access token.
func (c *Client) canRefresh() bool {
	return c.oauthClientID != "" && c.oauthClientSecret != "" && c.refreshToken != ""
}

// tryRefreshToken exchanges the refresh token for a new access token and
// updates the client's in-memory token. If an OnTokenRefresh callback is
// registered, it is called so the caller can persist the new credentials.
func (c *Client) tryRefreshToken() error {
	resp, err := RefreshAccessToken(c.oauthClientID, c.oauthClientSecret, c.refreshToken)
	if err != nil {
		return err
	}
	c.accessToken = resp.AccessToken
	if resp.RefreshToken != "" {
		c.refreshToken = resp.RefreshToken
	}
	if c.onTokenRefresh != nil {
		c.onTokenRefresh(c.accessToken, c.refreshToken)
	}
	return nil
}

func (c *Client) doRequest(ctx context.Context, query string, variables map[string]interface{}) (json.RawMessage, int, error) {
	reqBody := graphqlRequest{
		Query:     query,
		Variables: variables,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if c.accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.accessToken)
	} else {
		req.Header.Set("Authorization", c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var gqlResp graphqlResponse
	if err := json.Unmarshal(respBody, &gqlResp); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("failed to parse GraphQL response: %w", err)
	}

	if len(gqlResp.Errors) > 0 {
		return nil, resp.StatusCode, fmt.Errorf("GraphQL errors: %s", gqlResp.Errors[0].Message)
	}

	return gqlResp.Data, resp.StatusCode, nil
}

func (c *Client) parseIssue(raw json.RawMessage) (Issue, error) {
	var node struct {
		ID          string  `json:"id"`
		Identifier  string  `json:"identifier"`
		Title       string  `json:"title"`
		Description *string `json:"description"`
		Priority    *int    `json:"priority"`
		URL         string  `json:"url"`
		BranchName  *string `json:"branchName"`
		CreatedAt   string  `json:"createdAt"`
		UpdatedAt   string  `json:"updatedAt"`
		State       struct {
			Name string `json:"name"`
		} `json:"state"`
		Labels struct {
			Nodes []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"nodes"`
		} `json:"labels"`
		Project *struct {
			Name string `json:"name"`
		} `json:"project"`
		Assignee *struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
			Email       string `json:"email"`
		} `json:"assignee"`
		Relations struct {
			Nodes []struct {
				Type         string `json:"type"`
				RelatedIssue struct {
					ID         string `json:"id"`
					Identifier string `json:"identifier"`
					State      struct {
						Name string `json:"name"`
					} `json:"state"`
				} `json:"relatedIssue"`
			} `json:"nodes"`
		} `json:"relations"`
	}

	if err := json.Unmarshal(raw, &node); err != nil {
		return Issue{}, err
	}

	issue := Issue{
		ID:         node.ID,
		Identifier: node.Identifier,
		Title:      node.Title,
		Priority:   node.Priority,
		State:      node.State.Name,
		URL:        node.URL,
	}

	if node.Description != nil {
		issue.Description = *node.Description
	}
	if node.BranchName != nil {
		issue.BranchName = *node.BranchName
	}

	// Parse timestamps
	if t, err := time.Parse(time.RFC3339, node.CreatedAt); err == nil {
		issue.CreatedAt = t
	}
	if t, err := time.Parse(time.RFC3339, node.UpdatedAt); err == nil {
		issue.UpdatedAt = t
	}

	// Normalize labels to lowercase and capture IDs
	for _, label := range node.Labels.Nodes {
		issue.Labels = append(issue.Labels, strings.ToLower(label.Name))
		issue.LabelIDs = append(issue.LabelIDs, label.ID)
	}

	// Parse project
	if node.Project != nil {
		issue.Project = node.Project.Name
	}

	// Parse assignee
	if node.Assignee != nil {
		issue.Assignee = &Assignee{
			ID:       node.Assignee.ID,
			Name:     node.Assignee.Name,
			Username: node.Assignee.DisplayName,
			Email:    node.Assignee.Email,
		}
	}

	// Parse blockers (inverse relations where type is "blocks")
	for _, rel := range node.Relations.Nodes {
		if strings.ToLower(rel.Type) == "blocks" {
			issue.BlockedBy = append(issue.BlockedBy, Blocker{
				ID:         rel.RelatedIssue.ID,
				Identifier: rel.RelatedIssue.Identifier,
				State:      rel.RelatedIssue.State.Name,
			})
		}
	}

	return issue, nil
}

// WorkflowState represents a Linear workflow state.
type WorkflowState struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// FetchWorkflowStates retrieves all workflow states for a project's team.
func (c *Client) FetchWorkflowStates(ctx context.Context, projectSlug string) ([]WorkflowState, error) {
	query := `
		query($projectSlug: String!) {
			projects(filter: { slugId: { eq: $projectSlug } }) {
				nodes {
					teams {
						nodes {
							states {
								nodes {
									id
									name
									type
								}
							}
						}
					}
				}
			}
		}
	`

	variables := map[string]interface{}{
		"projectSlug": projectSlug,
	}

	resp, err := c.execute(ctx, query, variables)
	if err != nil {
		return nil, err
	}

	var data struct {
		Projects struct {
			Nodes []struct {
				Teams struct {
					Nodes []struct {
						States struct {
							Nodes []WorkflowState `json:"nodes"`
						} `json:"states"`
					} `json:"nodes"`
				} `json:"teams"`
			} `json:"nodes"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(resp, &data); err != nil {
		return nil, fmt.Errorf("failed to parse workflow states: %w", err)
	}

	var states []WorkflowState
	seen := make(map[string]bool)
	for _, project := range data.Projects.Nodes {
		for _, team := range project.Teams.Nodes {
			for _, state := range team.States.Nodes {
				if !seen[state.ID] {
					states = append(states, state)
					seen[state.ID] = true
				}
			}
		}
	}

	return states, nil
}

// UpdateIssueState updates an issue's workflow state by issue ID and state ID.
func (c *Client) UpdateIssueState(ctx context.Context, issueID, stateID string) error {
	query := `
		mutation($id: String!, $stateId: String!) {
			issueUpdate(id: $id, input: { stateId: $stateId }) {
				success
			}
		}
	`

	variables := map[string]interface{}{
		"id":      issueID,
		"stateId": stateID,
	}

	resp, err := c.execute(ctx, query, variables)
	if err != nil {
		return err
	}

	var data struct {
		IssueUpdate struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}
	if err := json.Unmarshal(resp, &data); err != nil {
		return fmt.Errorf("failed to parse update response: %w", err)
	}
	if !data.IssueUpdate.Success {
		return fmt.Errorf("issue update was not successful")
	}

	return nil
}

// AddComment adds a comment to an issue by issue ID.
func (c *Client) AddComment(ctx context.Context, issueID, body string) error {
	query := `
		mutation($issueId: String!, $body: String!) {
			commentCreate(input: { issueId: $issueId, body: $body }) {
				success
			}
		}
	`

	variables := map[string]interface{}{
		"issueId": issueID,
		"body":    body,
	}

	resp, err := c.execute(ctx, query, variables)
	if err != nil {
		return err
	}

	var data struct {
		CommentCreate struct {
			Success bool `json:"success"`
		} `json:"commentCreate"`
	}
	if err := json.Unmarshal(resp, &data); err != nil {
		return fmt.Errorf("failed to parse comment response: %w", err)
	}
	if !data.CommentCreate.Success {
		return fmt.Errorf("comment creation was not successful")
	}

	return nil
}

// FindOrCreateLabel finds a label by name in the team, or creates it if not found.
func (c *Client) FindOrCreateLabel(ctx context.Context, teamKey, labelName, color string) (string, error) {
	// Search for existing label
	query := `
		query($name: String!) {
			issueLabels(filter: { name: { eq: $name } }) {
				nodes {
					id
					name
				}
			}
		}
	`
	resp, err := c.execute(ctx, query, map[string]interface{}{"name": labelName})
	if err != nil {
		return "", err
	}

	var data struct {
		IssueLabels struct {
			Nodes []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"nodes"`
		} `json:"issueLabels"`
	}
	if err := json.Unmarshal(resp, &data); err != nil {
		return "", fmt.Errorf("failed to parse labels response: %w", err)
	}

	if len(data.IssueLabels.Nodes) > 0 {
		return data.IssueLabels.Nodes[0].ID, nil
	}

	// Need team ID to create a label — look it up from key
	teamQuery := `
		query($key: String!) {
			teams(filter: { key: { eq: $key } }) {
				nodes { id }
			}
		}
	`
	teamResp, err := c.execute(ctx, teamQuery, map[string]interface{}{"key": teamKey})
	if err != nil {
		return "", fmt.Errorf("failed to look up team: %w", err)
	}

	var teamData struct {
		Teams struct {
			Nodes []struct {
				ID string `json:"id"`
			} `json:"nodes"`
		} `json:"teams"`
	}
	if err := json.Unmarshal(teamResp, &teamData); err != nil {
		return "", fmt.Errorf("failed to parse team response: %w", err)
	}
	if len(teamData.Teams.Nodes) == 0 {
		return "", fmt.Errorf("team %q not found", teamKey)
	}

	// Create the label
	createQuery := `
		mutation($name: String!, $color: String!, $teamId: String!) {
			issueLabelCreate(input: { name: $name, color: $color, teamId: $teamId }) {
				success
				issueLabel {
					id
				}
			}
		}
	`
	createResp, err := c.execute(ctx, createQuery, map[string]interface{}{
		"name":   labelName,
		"color":  color,
		"teamId": teamData.Teams.Nodes[0].ID,
	})
	if err != nil {
		return "", fmt.Errorf("failed to create label: %w", err)
	}

	var createData struct {
		IssueLabelCreate struct {
			Success    bool `json:"success"`
			IssueLabel struct {
				ID string `json:"id"`
			} `json:"issueLabel"`
		} `json:"issueLabelCreate"`
	}
	if err := json.Unmarshal(createResp, &createData); err != nil {
		return "", fmt.Errorf("failed to parse create label response: %w", err)
	}
	if !createData.IssueLabelCreate.Success {
		return "", fmt.Errorf("label creation was not successful")
	}

	return createData.IssueLabelCreate.IssueLabel.ID, nil
}

// UpdateIssueLabels updates an issue's labels by setting the full list of label IDs.
func (c *Client) UpdateIssueLabels(ctx context.Context, issueID string, labelIDs []string) error {
	query := `
		mutation($id: String!, $labelIds: [String!]!) {
			issueUpdate(id: $id, input: { labelIds: $labelIds }) {
				success
			}
		}
	`

	resp, err := c.execute(ctx, query, map[string]interface{}{
		"id":       issueID,
		"labelIds": labelIDs,
	})
	if err != nil {
		return err
	}

	var data struct {
		IssueUpdate struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}
	if err := json.Unmarshal(resp, &data); err != nil {
		return fmt.Errorf("failed to parse update response: %w", err)
	}
	if !data.IssueUpdate.Success {
		return fmt.Errorf("label update was not successful")
	}

	return nil
}

// FetchIssueByIdentifier fetches a single issue by its human-readable identifier (e.g. NUM-1).
func (c *Client) FetchIssueByIdentifier(ctx context.Context, identifier string) (*Issue, error) {
	query := `
		query($identifier: String!) {
			issue(id: $identifier) {
				` + issueNodeFragment + `
			}
		}
	`

	resp, err := c.execute(ctx, query, map[string]interface{}{"identifier": identifier})
	if err != nil {
		return nil, err
	}

	var data struct {
		Issue json.RawMessage `json:"issue"`
	}
	if err := json.Unmarshal(resp, &data); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	if data.Issue == nil || string(data.Issue) == "null" {
		return nil, fmt.Errorf("issue %q not found", identifier)
	}

	issue, err := c.parseIssue(data.Issue)
	if err != nil {
		return nil, err
	}

	return &issue, nil
}
