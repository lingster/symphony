// Command oauth-setup performs the Linear OAuth authorization flow to obtain
// an agent/bot access token.
//
// Usage:
//
//	LINEAR_OAUTH_CLIENT_ID=xxx LINEAR_OAUTH_CLIENT_SECRET=xxx oauth-setup
//
// Required environment variables:
//
//	LINEAR_OAUTH_CLIENT_ID     - OAuth application client ID
//	LINEAR_OAUTH_CLIENT_SECRET - OAuth application client secret
//
// Optional:
//
//	LINEAR_AGENT_TOKEN - Existing agent token; if set it is revoked first so
//	                     Linear re-issues the consent screen.
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ling/symphony/internal/envfile"
	"github.com/ling/symphony/internal/linear"
)

const (
	redirectPort = 3456
	redirectPath = "/callback"
	flowTimeout  = 5 * time.Minute
)

func main() {
	// Load .env from the current directory so credentials don't need to be
	// passed as environment variables on every invocation.
	envPath := findEnvFile()
	if envPath != "" {
		loadDotEnv(envPath)
	}

	clientID := requireEnv("LINEAR_OAUTH_CLIENT_ID")
	clientSecret := requireEnv("LINEAR_OAUTH_CLIENT_SECRET")

	fmt.Println()
	fmt.Println("========================================")
	fmt.Println("  Linear Agent OAuth Setup")
	fmt.Println("========================================")
	fmt.Println()
	fmt.Println("This creates a dedicated agent/bot user in your Linear workspace.")
	fmt.Println("You need workspace admin permissions to approve the installation.")
	fmt.Println()

	// Revoke any existing agent token so Linear shows the consent screen again.
	if existing := os.Getenv("LINEAR_AGENT_TOKEN"); existing != "" {
		fmt.Print("Revoking existing token to allow fresh authorization... ")
		if err := linear.RevokeToken(existing); err != nil {
			fmt.Printf("could not revoke (%v) — proceeding anyway.\n", err)
			fmt.Println("  If Linear shows 'already authorized', revoke the app manually at")
			fmt.Println("  Linear → Settings → Security & access → Authorized applications.")
		} else {
			fmt.Println("done.")
		}
		fmt.Println()
	}

	redirectURI := fmt.Sprintf("http://localhost:%d%s", redirectPort, redirectPath)
	authURL := linear.BuildAuthURL(clientID, redirectURI)

	fmt.Println("Open this URL in your browser to authorize:")
	fmt.Println()
	fmt.Println(" ", authURL)
	fmt.Println()

	openBrowser(authURL)

	fmt.Println("After authorizing, either:")
	fmt.Println("  A) The browser redirects back automatically (if running locally)")
	fmt.Println("  B) Copy the code from the redirect URL and paste it below")
	fmt.Println()
	fmt.Printf("  The redirect URL looks like: http://localhost:%d%s?code=<THE_CODE>\n", redirectPort, redirectPath)
	fmt.Println()

	code, err := raceForCode(redirectURI)
	if err != nil {
		log.Fatalf("Failed to obtain authorization code: %v", err)
	}

	fmt.Println()
	fmt.Println("Authorization code received. Exchanging for access token...")
	fmt.Println()

	tokenResp, err := linear.ExchangeCode(clientID, clientSecret, code, redirectURI)
	if err != nil {
		log.Fatalf("Token exchange failed: %v", err)
	}

	// Verify the token works.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !linear.ValidateToken(ctx, tokenResp.AccessToken) {
		log.Fatal("Token obtained but failed API validation — the app may have been revoked.")
	}

	fmt.Println("========================================")
	fmt.Println("  Agent Token Obtained Successfully!")
	fmt.Println("========================================")
	fmt.Println()
	fmt.Printf("  Scopes: %s\n", tokenResp.Scope)
	fmt.Println()

	// Persist tokens to .env file.
	kvs := map[string]string{"LINEAR_AGENT_TOKEN": tokenResp.AccessToken}
	if tokenResp.RefreshToken != "" {
		kvs["LINEAR_REFRESH_TOKEN"] = tokenResp.RefreshToken
	}
	if envPath != "" {
		if err := envfile.Update(envPath, kvs); err != nil {
			fmt.Printf("  WARNING: could not update %s: %v\n", envPath, err)
			fmt.Println("  You will need to update your .env file manually.")
		} else {
			fmt.Printf("  Updated %s with new tokens.\n", envPath)
		}
	} else {
		fmt.Println("  No .env file found. Add these to your environment:")
		fmt.Println()
		fmt.Printf("  export LINEAR_AGENT_TOKEN=%s\n", tokenResp.AccessToken)
		if tokenResp.RefreshToken != "" {
			fmt.Printf("  export LINEAR_REFRESH_TOKEN=%s\n", tokenResp.RefreshToken)
		}
	}
	fmt.Println()

	if tokenResp.RefreshToken != "" {
		fmt.Println("  AUTO-REFRESH ENABLED — the orchestrator will renew the token")
		fmt.Println("  automatically and persist updates to .env.")
	} else {
		fmt.Println("  WARNING: No refresh token received. You will need to re-run this setup")
		fmt.Println("  when the access token expires.")
	}
	fmt.Println()
	fmt.Println("The Symphony orchestrator uses LINEAR_AGENT_TOKEN when set,")
	fmt.Println("falling back to LINEAR_API_KEY for personal key auth.")
}

// requireEnv exits with an error when the named environment variable is empty.
func requireEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		fmt.Fprintf(os.Stderr, "\n[ERROR] %s environment variable is required\n\n", name)
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  LINEAR_OAUTH_CLIENT_ID=xxx LINEAR_OAUTH_CLIENT_SECRET=xxx oauth-setup\n\n")
		os.Exit(1)
	}
	return v
}

// openBrowser attempts to open the URL in the system browser.
func openBrowser(rawURL string) {
	var cmd string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd = "rundll32"
		rawURL = "url.dll,FileProtocolHandler " + rawURL
	default:
		cmd = "xdg-open"
	}
	if err := exec.Command(cmd, rawURL).Start(); err == nil {
		fmt.Println("  (browser opened automatically)")
		fmt.Println()
	}
}

// raceForCode races a local HTTP callback server against manual stdin paste.
// The first channel to deliver a non-empty code wins.
func raceForCode(redirectURI string) (string, error) {
	codeCh := make(chan string, 2)
	errCh := make(chan error, 2)

	go listenForCallback(redirectURI, codeCh, errCh)
	go readFromStdin(codeCh, errCh)

	timeout := time.NewTimer(flowTimeout)
	defer timeout.Stop()

	select {
	case code := <-codeCh:
		return code, nil
	case err := <-errCh:
		return "", err
	case <-timeout.C:
		return "", fmt.Errorf("timed out waiting for authorization (limit: %s)", flowTimeout)
	}
}

// listenForCallback starts a local HTTP server and waits for the OAuth redirect.
func listenForCallback(redirectURI string, codeCh chan<- string, errCh chan<- error) {
	u, _ := url.Parse(redirectURI)
	mux := http.NewServeMux()

	mux.HandleFunc(u.Path, func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		oauthErr := r.URL.Query().Get("error")

		if oauthErr != "" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, "<html><body><h2>Authorization failed</h2><p>%s</p></body></html>", oauthErr)
			errCh <- fmt.Errorf("OAuth error: %s", oauthErr)
			return
		}
		if code == "" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, "<html><body><h2>Missing authorization code</h2></body></html>")
			errCh <- fmt.Errorf("no authorization code in callback")
			return
		}

		fmt.Fprint(w, "<html><body><h2>Authorization successful! You can close this tab.</h2></body></html>")
		codeCh <- code
	})

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", redirectPort),
		Handler: mux,
	}

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		// Port unavailable — manual paste will handle it.
		errCh <- fmt.Errorf("could not start callback server: %w", err)
	}
}

// readFromStdin prompts the user to paste the authorization code or full
// redirect URL and extracts the code from their input.
func readFromStdin(codeCh chan<- string, errCh chan<- error) {
	fmt.Print("  Paste the code (or full redirect URL) here: ")

	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		errCh <- fmt.Errorf("failed to read from stdin")
		return
	}

	input := strings.TrimSpace(scanner.Text())
	if input == "" {
		errCh <- fmt.Errorf("empty input")
		return
	}

	// Accept a full redirect URL or a bare code.
	if strings.HasPrefix(input, "http") {
		parsed, err := url.Parse(input)
		if err == nil {
			if code := parsed.Query().Get("code"); code != "" {
				codeCh <- code
				return
			}
		}
	}

	// Treat as bare code (alphanumeric + hyphens).
	if isValidCode(input) {
		codeCh <- input
		return
	}

	errCh <- fmt.Errorf("could not extract authorization code from input: %q", input)
}

// findEnvFile looks for a .env file in the current directory, then walks up
// to the repository root looking for one.
func findEnvFile() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(dir, ".env")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// loadDotEnv reads a .env file and sets all keys as environment variables,
// overwriting any existing shell values so .env takes precedence.
func loadDotEnv(path string) {
	kvs, err := envfile.Load(path)
	if err != nil {
		return
	}
	for k, v := range kvs {
		os.Setenv(k, v)
	}
}

func isValidCode(s string) bool {
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_') {
			return false
		}
	}
	return len(s) > 0
}
