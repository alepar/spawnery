package main

// login.go — spawnctl login/logout commands.
//
//  spawnctl login              — loopback PKCE (opens browser)
//  spawnctl login --device     — RFC 8628 device grant (prints user_code)
//  spawnctl login --no-browser — loopback PKCE, print URL instead of opening browser
//  spawnctl logout             — revoke refresh family + wipe local state

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
)

const (
	loginTimeout  = 5 * time.Minute
	devicePollMax = 15 * time.Minute
)

// loginCmd returns the 'spawnctl login' command.
func loginCmd() *cli.Command {
	return &cli.Command{
		Name:  "login",
		Usage: "authenticate with the Spawnery AS (loopback PKCE or device grant)",
		Flags: []cli.Flag{
			configDirFlag(),
			&cli.StringFlag{
				Name:  "as",
				Usage: "AS base URL (e.g. https://as.example.com)",
				Value: "http://localhost:8090",
			},
			&cli.BoolFlag{
				Name:  "device",
				Usage: "use RFC 8628 device grant instead of loopback PKCE",
			},
			&cli.BoolFlag{
				Name:  "no-browser",
				Usage: "print the authorize URL instead of opening a browser",
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			dir, err := resolveDir(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			asURL := strings.TrimRight(c.String("as"), "/")

			if c.Bool("device") {
				if err := loginDevice(ctx, dir, asURL, c.Writer); err != nil {
					return cli.Exit(err.Error(), 1)
				}
				return nil
			}
			if err := loginLoopback(ctx, dir, asURL, c.Bool("no-browser"), c.Writer); err != nil {
				return cli.Exit(err.Error(), 1)
			}
			return nil
		},
	}
}

// logoutCmd returns the 'spawnctl logout' command.
func logoutCmd() *cli.Command {
	return &cli.Command{
		Name:  "logout",
		Usage: "revoke the current session and remove local credentials",
		Flags: []cli.Flag{configDirFlag()},
		Action: func(ctx context.Context, c *cli.Command) error {
			dir, err := resolveDir(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			if err := doLogout(ctx, dir); err != nil {
				return cli.Exit(err.Error(), 1)
			}
			fmt.Fprintln(c.Writer, "logged out")
			return nil
		},
	}
}

// --- loopback PKCE ---

// loginLoopback drives the OAuth 2.0 auth-code + PKCE flow using a loopback listener.
// It opens (or prints) the authorize URL and waits for the browser to land on the /cb handler.
func loginLoopback(ctx context.Context, dir, asURL string, noBrowser bool, w io.Writer) error {
	// 1. Generate session key.
	sessKey, err := generateSessionKey()
	if err != nil {
		return fmt.Errorf("generate session key: %w", err)
	}
	pubB64, err := sessionPubkeySPKIB64(sessKey)
	if err != nil {
		return fmt.Errorf("marshal session pubkey: %w", err)
	}

	// 2. Bind loopback listener on a random port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/cb", port)

	// 3. Generate PKCE verifier/challenge and CSRF state.
	verifier, err := pkceVerifier()
	if err != nil {
		return fmt.Errorf("pkce: %w", err)
	}
	challenge := pkceChallenge(verifier)
	csrfState, err := randOpaqueHex()
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}

	// 4. Build authorize URL.
	authURL := asURL + "/oauth/authorize?" + url.Values{
		"redirect_uri":          {redirectURI},
		"state":                 {csrfState},
		"session_pubkey":        {pubB64},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}.Encode()

	// 5. Open browser or print URL.
	if noBrowser {
		fmt.Fprintf(w, "Open this URL in your browser to log in:\n\n  %s\n\n", authURL)
	} else {
		fmt.Fprintf(w, "Opening browser for login...\n")
		if err := openBrowser(authURL); err != nil {
			fmt.Fprintf(w, "Could not open browser automatically. Open this URL manually:\n\n  %s\n\n", authURL)
		}
	}

	// 6. Serve the callback; the handler fills in the result or error.
	type cbResult struct {
		accessToken  string
		refreshToken string
		err          error
	}
	done := make(chan cbResult, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/cb", func(rw http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if errCode := q.Get("error"); errCode != "" {
			msg := errCode
			if d := q.Get("error_description"); d != "" {
				msg += ": " + d
			}
			rw.Header().Set("Content-Type", "text/html")
			rw.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(rw, "<html><body><h2>Login failed: %s</h2><p>You may close this tab.</p></body></html>", msg)
			done <- cbResult{err: fmt.Errorf("AS error: %s", msg)}
			return
		}
		if q.Get("state") != csrfState {
			rw.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(rw, "<html><body><p>State mismatch. Please try again.</p></body></html>")
			done <- cbResult{err: errors.New("state mismatch — possible CSRF")}
			return
		}
		at := q.Get("access_token")
		rt := q.Get("refresh_token")
		if at == "" {
			rw.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(rw, "<html><body><p>No access token in callback. Please try again.</p></body></html>")
			done <- cbResult{err: errors.New("no access_token in callback")}
			return
		}
		rw.Header().Set("Content-Type", "text/html")
		rw.WriteHeader(http.StatusOK)
		fmt.Fprintln(rw, "<html><body><h2>Login successful!</h2><p>You may close this tab and return to the terminal.</p></body></html>")
		done <- cbResult{accessToken: at, refreshToken: rt}
	})

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	// 7. Wait for callback or timeout.
	tctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()

	var result cbResult
	select {
	case result = <-done:
	case <-tctx.Done():
		return fmt.Errorf("login timed out after %s", loginTimeout)
	}
	if result.err != nil {
		return result.err
	}

	// 8. Persist state.
	keyPEM, err := marshalSessionKey(sessKey)
	if err != nil {
		return fmt.Errorf("marshal session key: %w", err)
	}
	s := &authState{
		ASURL:              asURL,
		AccessToken:        result.accessToken,
		AccessExpiresAt:    time.Now().Add(accessTokenTTLClient).Unix(),
		RefreshToken:       result.refreshToken,
		SessionKeyPKCS8PEM: keyPEM,
	}
	if err := saveState(dir, s); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	fmt.Fprintln(w, "Login successful. Credentials saved.")
	return nil
}

// --- device grant ---

// loginDevice implements RFC 8628 device grant: POST /device/authorize → print user_code →
// poll /device/token → persist tokens.
func loginDevice(ctx context.Context, dir, asURL string, w io.Writer) error {
	sessKey, err := generateSessionKey()
	if err != nil {
		return fmt.Errorf("generate session key: %w", err)
	}
	pubB64, err := sessionPubkeySPKIB64(sessKey)
	if err != nil {
		return fmt.Errorf("marshal session pubkey: %w", err)
	}

	// POST /device/authorize.
	form := url.Values{
		"session_pubkey": {pubB64},
		"client_kind":    {"cli"},
	}
	resp, err := http.PostForm(asURL+"/device/authorize", form)
	if err != nil {
		return fmt.Errorf("device/authorize: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("device/authorize: status %d: %s", resp.StatusCode, body)
	}

	var authOut struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&authOut); err != nil {
		return fmt.Errorf("device/authorize: decode: %w", err)
	}

	pollInterval := time.Duration(authOut.Interval) * time.Second
	if pollInterval < time.Second {
		pollInterval = 5 * time.Second
	}

	fmt.Fprintf(w, "\nTo authorize this device, visit:\n\n  %s\n\nAnd enter code: %s\n\n",
		authOut.VerificationURI, authOut.UserCode)

	// Poll /device/token.
	deadline := time.Now().Add(devicePollMax)
	httpClient := &http.Client{Timeout: 10 * time.Second}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("device grant timed out")
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}

		tokenResp, err := httpClient.PostForm(asURL+"/device/token",
			url.Values{"device_code": {authOut.DeviceCode}})
		if err != nil {
			continue // transient network error
		}

		var tokenOut struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			Error        string `json:"error"`
		}
		body, _ := io.ReadAll(tokenResp.Body)
		tokenResp.Body.Close()
		_ = json.Unmarshal(body, &tokenOut)

		switch tokenOut.Error {
		case "":
			// Success.
			if tokenOut.AccessToken == "" {
				return fmt.Errorf("device/token: no access_token in response: %s", body)
			}
			keyPEM, err := marshalSessionKey(sessKey)
			if err != nil {
				return fmt.Errorf("marshal session key: %w", err)
			}
			s := &authState{
				ASURL:              asURL,
				AccessToken:        tokenOut.AccessToken,
				AccessExpiresAt:    time.Now().Add(accessTokenTTLClient).Unix(),
				RefreshToken:       tokenOut.RefreshToken,
				SessionKeyPKCS8PEM: keyPEM,
			}
			if err := saveState(dir, s); err != nil {
				return fmt.Errorf("save state: %w", err)
			}
			fmt.Fprintln(w, "Device authorized. Credentials saved.")
			return nil
		case "authorization_pending":
			// Continue polling.
		case "slow_down":
			pollInterval += 5 * time.Second
		case "access_denied":
			return errors.New("device grant denied by user")
		case "expired_token":
			return errors.New("device code expired — run 'spawnctl login --device' again")
		default:
			return fmt.Errorf("device/token error: %s", tokenOut.Error)
		}
	}
}

// --- logout ---

func doLogout(ctx context.Context, dir string) error {
	s, err := loadState(dir)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	if s == nil {
		return nil // already logged out
	}

	// Best-effort AS call: revoke the family.
	// /logout reads the logout_session mirror cookie (Path=/logout); the refresh_token cookie
	// lives at Path=/refresh and would not be sent by a browser. The CLI bypasses the browser
	// cookie jar so we set logout_session directly, matching what the AS handler reads.
	if s.ASURL != "" && s.RefreshToken != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.ASURL+"/logout", nil)
		if err == nil {
			req.AddCookie(&http.Cookie{Name: "logout_session", Value: s.RefreshToken})
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				resp.Body.Close()
			}
		}
	}

	// Wipe local state regardless of AS response.
	_ = os.Remove(authStatePath(dir))
	_ = os.Remove(authLockPath(dir))
	return nil
}

// --- PKCE helpers ---

func pkceVerifier() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func randOpaqueHex() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// openBrowser opens url in the user's default browser.
func openBrowser(rawURL string) error {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "linux":
		cmd = "xdg-open"
		args = []string{rawURL}
	case "darwin":
		cmd = "open"
		args = []string{rawURL}
	case "windows":
		cmd = "cmd"
		args = []string{"/c", "start", rawURL}
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
	return exec.Command(cmd, args...).Start()
}
