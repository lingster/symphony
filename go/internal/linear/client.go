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

// Client is a Linear GraphQL API client.
type Client struct {
	endpoint   string
	apiKey     string
	httpClient *http.Client
}

// NewClient creates a new Linear client.
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

// FetchCandidateIssues fetches issues in active states for a project.
func (c *Client) FetchCandidateIssues(ctx context.Context, projectSlug string, activeStates []string) ([]Issue, error) {
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
							name
						}
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
				}
			}
		}
	`

	var allIssues []Issue
	var cursor *string

	for {
		variables := map[string]interface{}{
			"projectSlug": projectSlug,
			"first":       50,
		}
		if cursor != nil {
			variables["after"] = *cursor
		}

		resp, err := c.execute(ctx, query, variables)
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

			// Filter by active states
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
func (c *Client) FetchIssuesByStates(ctx context.Context, projectSlug string, states []string) ([]Issue, error) {
	if len(states) == 0 {
		return nil, nil
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
					id
					identifier
					state {
						name
					}
				}
			}
		}
	`

	var allIssues []Issue
	var cursor *string

	for {
		variables := map[string]interface{}{
			"projectSlug": projectSlug,
			"first":       50,
		}
		if cursor != nil {
			variables["after"] = *cursor
		}

		resp, err := c.execute(ctx, query, variables)
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

			// Filter by specified states
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
	reqBody := graphqlRequest{
		Query:     query,
		Variables: variables,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var gqlResp graphqlResponse
	if err := json.Unmarshal(respBody, &gqlResp); err != nil {
		return nil, fmt.Errorf("failed to parse GraphQL response: %w", err)
	}

	if len(gqlResp.Errors) > 0 {
		return nil, fmt.Errorf("GraphQL errors: %s", gqlResp.Errors[0].Message)
	}

	return gqlResp.Data, nil
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
				Name string `json:"name"`
			} `json:"nodes"`
		} `json:"labels"`
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

	// Normalize labels to lowercase
	for _, label := range node.Labels.Nodes {
		issue.Labels = append(issue.Labels, strings.ToLower(label.Name))
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
