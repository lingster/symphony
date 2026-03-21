// Package linear provides OAuth token management for the Linear API.
package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	linearTokenURL = "https://api.linear.app/oauth/token"
	linearRevokeURL = "https://api.linear.app/oauth/revoke"
)

// OAuthScopes are the scopes requested during the OAuth flow.
var OAuthScopes = []string{
	"read",
	"write",
	"app:assignable",
	"app:mentionable",
	"initiative:read",
	"initiative:write",
}

// TokenResponse holds the result of a token exchange or refresh.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

// BuildAuthURL returns the Linear OAuth authorization URL with actor=app.
func BuildAuthURL(clientID, redirectURI string) string {
	params := url.Values{}
	params.Set("client_id", clientID)
	params.Set("redirect_uri", redirectURI)
	params.Set("response_type", "code")
	params.Set("scope", strings.Join(OAuthScopes, ","))
	params.Set("actor", "app")
	params.Set("prompt", "consent")
	return "https://linear.app/oauth/authorize?" + params.Encode()
}

// ExchangeCode exchanges an authorization code for an access token.
func ExchangeCode(clientID, clientSecret, code, redirectURI string) (*TokenResponse, error) {
	return postTokenRequest(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	})
}

// RefreshAccessToken exchanges a refresh token for a new access token.
func RefreshAccessToken(clientID, clientSecret, refreshToken string) (*TokenResponse, error) {
	return postTokenRequest(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	})
}

// RevokeToken revokes an OAuth access token.
func RevokeToken(token string) error {
	resp, err := http.PostForm(linearRevokeURL, url.Values{"token": {token}})
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("revoke returned status %d", resp.StatusCode)
	}
	return nil
}

// ValidateToken returns true if the token successfully authenticates against the Linear API.
func ValidateToken(ctx context.Context, token string) bool {
	c := NewClientWithToken("", token, "", "", "")
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := c.execute(ctx, `query { viewer { id } }`, nil)
	return err == nil
}

func postTokenRequest(params url.Values) (*TokenResponse, error) {
	resp, err := http.PostForm(linearTokenURL, params)
	if err != nil {
		return nil, fmt.Errorf("token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token request returned status %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp TokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to parse token response: %w", err)
	}

	return &tokenResp, nil
}
