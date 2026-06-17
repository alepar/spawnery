package main

// gh.go — spawnctl owner GitHub-link driver.
//
//	spawnctl gh link    — link/relink a GitHub account (loopback default, --device/--no-browser device)
//	spawnctl gh status  — show the current GitHub link
//	spawnctl gh revoke  — revoke the GitHub link (grant-wide kill switch)
//
// The CLI never holds GitHub token material: redeem returns metadata only, and the loopback
// completer `rc` is a single-use channel nonce that is never logged or surfaced.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
)

const (
	ghLinkTimeout   = 5 * time.Minute
	ghDevicePollMax = 15 * time.Minute
	ghDefaultASURL  = "http://localhost:8090"

	ghConsentWarning = "SECURITY: Only continue if YOU started this link from THIS terminal. " +
		"Authorizing grants the Spawnery GitHub App access to your GitHub account on your behalf. " +
		"Do not authorize a link that someone else asked you to open."

	ghLoopbackPrecondition = "The default loopback flow assumes a single-user host: on a shared " +
		"multi-user machine a co-resident user could intercept the loopback callback (RFC 8252 §8.3). " +
		"Use --device on shared or headless hosts."
)

// ghDevicePollInterval is a var (not const) so tests can shrink it.
var ghDevicePollInterval = 3 * time.Second

// ghLink is the metadata-only projection returned by redeem and list. No token material.
type ghLink struct {
	SecretID     string `json:"secret_id"`
	Host         string `json:"host"`
	Login        string `json:"login"`
	GithubUserID string `json:"github_user_id"`
	Version      uint64 `json:"version"`
	UpdatedAt    int64  `json:"updated_at"`
	Status       string `json:"status"`
}

type redeemOutcome int

const (
	redeemDone redeemOutcome = iota
	redeemPending
	redeemUnauthorized
	redeemIdentityChange
	redeemTerminal
)

type redeemResult struct {
	outcome  redeemOutcome
	link     ghLink // populated on redeemDone
	oldLogin string // populated on redeemIdentityChange
	newLogin string
	errCode  string // populated on redeemTerminal
	errDesc  string
}

// bearerSource yields the AS Bearer token, with an explicit refresh hook for the device 401 path.
type bearerSource interface {
	token(ctx context.Context) (string, error)
	refresh(ctx context.Context) error
}

type cpBearerSource struct{ ts *cpTokenSource }

func (b cpBearerSource) token(ctx context.Context) (string, error)  { return b.ts.Token(ctx) }
func (b cpBearerSource) refresh(ctx context.Context) error          { return b.ts.OnUnauthenticated(ctx) }

func newBearerSource(dir, tokenFlag string, hc *http.Client) bearerSource {
	return cpBearerSource{ts: buildTokenSource(dir, tokenFlag, hc)}
}

// resolveASURL prefers the --as flag, then the logged-in session's AS URL, then the dev default.
func resolveASURL(c *cli.Command, dir string) string {
	if v := c.String("as"); v != "" {
		return strings.TrimRight(v, "/")
	}
	if s, err := loadState(dir); err == nil && s != nil && s.ASURL != "" {
		return strings.TrimRight(s.ASURL, "/")
	}
	return ghDefaultASURL
}

func ghCmd() *cli.Command {
	return &cli.Command{
		Name:     "gh",
		Usage:    "manage your GitHub account link",
		Commands: []*cli.Command{ghLinkCmd(), ghStatusCmd(), ghRevokeCmd()},
	}
}

// doStart calls POST /github/link/start and returns (authorize_url, flow_id).
func doStart(ctx context.Context, hc *http.Client, asURL, bearer, clientKind string, port int, host string) (string, string, error) {
	reqBody, _ := json.Marshal(map[string]any{"client_kind": clientKind, "port": port, "host": host})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, asURL+"/github/link/start", bytes.NewReader(reqBody))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := hc.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("link start: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("link start: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		AuthorizeURL string `json:"authorize_url"`
		FlowID       string `json:"flow_id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", "", fmt.Errorf("link start: decode: %w", err)
	}
	if out.AuthorizeURL == "" || out.FlowID == "" {
		return "", "", errors.New("link start: empty authorize_url/flow_id")
	}
	return out.AuthorizeURL, out.FlowID, nil
}

// doRedeem calls POST /github/link/redeem and maps the HTTP status to a redeemOutcome.
// SECURITY: rc is sent in the body but NEVER echoed into any returned string or error.
func doRedeem(ctx context.Context, hc *http.Client, asURL, bearer, flowID, rc string, confirmSwitch bool) (redeemResult, error) {
	reqBody, _ := json.Marshal(map[string]any{"flow_id": flowID, "rc": rc, "confirm_switch": confirmSwitch})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, asURL+"/github/link/redeem", bytes.NewReader(reqBody))
	if err != nil {
		return redeemResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := hc.Do(req)
	if err != nil {
		return redeemResult{}, fmt.Errorf("link redeem: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	switch resp.StatusCode {
	case http.StatusOK:
		var l ghLink
		if err := json.Unmarshal(body, &l); err != nil {
			return redeemResult{}, fmt.Errorf("link redeem: decode: %w", err)
		}
		return redeemResult{outcome: redeemDone, link: l}, nil
	case http.StatusAccepted:
		return redeemResult{outcome: redeemPending}, nil
	case http.StatusUnauthorized:
		return redeemResult{outcome: redeemUnauthorized}, nil
	case http.StatusConflict:
		var c struct {
			Old string `json:"old"`
			New string `json:"new"`
		}
		_ = json.Unmarshal(body, &c)
		return redeemResult{outcome: redeemIdentityChange, oldLogin: c.Old, newLogin: c.New}, nil
	default:
		var e struct {
			Error string `json:"error"`
			Desc  string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &e)
		return redeemResult{outcome: redeemTerminal, errCode: e.Error, errDesc: e.Desc}, nil
	}
}

// doList calls GET /github/links and returns the account's link metadata (0/1 row for MVP).
func doList(ctx context.Context, hc *http.Client, asURL, bearer string) ([]ghLink, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asURL+"/github/links", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list links: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list links: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Links []ghLink `json:"links"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("list links: decode: %w", err)
	}
	return out.Links, nil
}

func printStatus(w io.Writer, l ghLink) {
	switch l.Status {
	case "linked":
		fmt.Fprintf(w, "Linked as @%s (v%d) on %s\n", l.Login, l.Version, l.Host)
	case "relink_required":
		fmt.Fprintf(w, "@%s on %s — relink required (run 'spawnctl gh link')\n", l.Login, l.Host)
	case "revoked":
		fmt.Fprintf(w, "@%s on %s — revoked\n", l.Login, l.Host)
	default:
		fmt.Fprintf(w, "@%s on %s — %s\n", l.Login, l.Host, l.Status)
	}
}

// printLink renders the metadata-only success line.
func printLink(w io.Writer, l ghLink) {
	fmt.Fprintf(w, "Linked GitHub account @%s (v%d) on %s\n", l.Login, l.Version, l.Host)
}

func ghStatusCmd() *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "show the current GitHub account link",
		Flags: []cli.Flag{
			configDirFlag(),
			&cli.StringFlag{Name: "as", Usage: "AS base URL (default: logged-in session)"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			dir, err := resolveDir(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			hc := &http.Client{Timeout: 30 * time.Second}
			asURL := resolveASURL(c, dir)
			bearer, err := newBearerSource(dir, "", hc).token(ctx)
			if err != nil {
				return cli.Exit("auth: "+err.Error(), 1)
			}
			links, err := doList(ctx, hc, asURL, bearer)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			if len(links) == 0 {
				fmt.Fprintln(c.Writer, "No GitHub link. Run 'spawnctl gh link'.")
				return nil
			}
			for _, l := range links {
				printStatus(c.Writer, l)
			}
			return nil
		},
	}
}

// errRevokeRemoteFailed signals AS returned 502: the link is locally revoked (kill switch took
// effect) but the GitHub grant teardown failed. Treated as a soft success with a warning.
var errRevokeRemoteFailed = errors.New("link revoked locally but GitHub grant teardown failed")

// doRevoke calls POST /github/link/revoke (form-encoded secret_id). 204 → nil; 502 →
// errRevokeRemoteFailed (locally dead); anything else → error.
func doRevoke(ctx context.Context, hc *http.Client, asURL, bearer, secretID string) error {
	form := url.Values{"secret_id": {secretID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, asURL+"/github/link/revoke", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("revoke: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusBadGateway:
		return errRevokeRemoteFailed
	default:
		return fmt.Errorf("revoke: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

// resolveRevokeSecretID returns the explicit flag value, or — for the MVP 0/1-row model — the
// single existing link's secret_id. Zero links → error; multiple → require --secret-id.
func resolveRevokeSecretID(ctx context.Context, hc *http.Client, asURL, bearer, flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	links, err := doList(ctx, hc, asURL, bearer)
	if err != nil {
		return "", err
	}
	switch len(links) {
	case 0:
		return "", errors.New("no GitHub link to revoke")
	case 1:
		return links[0].SecretID, nil
	default:
		return "", errors.New("multiple links found; pass --secret-id to choose one")
	}
}

func ghRevokeCmd() *cli.Command {
	return &cli.Command{
		Name:  "revoke",
		Usage: "revoke the GitHub link (grant-wide kill switch)",
		Flags: []cli.Flag{
			configDirFlag(),
			&cli.StringFlag{Name: "as", Usage: "AS base URL (default: logged-in session)"},
			&cli.StringFlag{Name: "secret-id", Usage: "secret id to revoke (default: the single existing link)"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			dir, err := resolveDir(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			hc := &http.Client{Timeout: 30 * time.Second}
			asURL := resolveASURL(c, dir)
			bearer, err := newBearerSource(dir, "", hc).token(ctx)
			if err != nil {
				return cli.Exit("auth: "+err.Error(), 1)
			}
			secretID, err := resolveRevokeSecretID(ctx, hc, asURL, bearer, c.String("secret-id"))
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			switch err := doRevoke(ctx, hc, asURL, bearer, secretID); {
			case err == nil:
				fmt.Fprintf(c.Writer, "Revoked GitHub link %s\n", secretID)
				return nil
			case errors.Is(err, errRevokeRemoteFailed):
				fmt.Fprintf(c.Writer, "WARNING: %s; the link is locally revoked (access tokens lapse within ~8h).\n", err)
				return nil
			default:
				return cli.Exit(err.Error(), 1)
			}
		},
	}
}

// doneHTML is the loopback completion page. It is fully self-contained (no external subresources),
// declares a no-referrer policy, and strips the rc from the address bar via history.replaceState.
const doneHTML = `<!doctype html>
<meta charset="utf-8">
<meta name="referrer" content="no-referrer">
<title>Spawnery — GitHub linked</title>
<body style="font-family: sans-serif; max-width: 40em; margin: 4em auto;">
<h2>GitHub authorization received</h2>
<p>You may close this tab and return to your terminal.</p>
<script>history.replaceState(null, "", location.pathname);</script>
</body>
`

// renderDonePage writes the self-contained completion page. It NEVER reflects the rc.
func renderDonePage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, doneHTML)
}

// finishRedeem turns a single-shot redeem result (loopback) into output or an error.
func finishRedeem(w io.Writer, res redeemResult) error {
	switch res.outcome {
	case redeemDone:
		printLink(w, res.link)
		return nil
	case redeemIdentityChange:
		return fmt.Errorf("GitHub identity is changing: @%s → @%s; re-run with --confirm-switch to proceed", res.oldLogin, res.newLogin)
	case redeemPending:
		return errors.New("authorization not complete; please retry")
	case redeemUnauthorized:
		return errors.New("unauthorized; run 'spawnctl login' and retry")
	default:
		return fmt.Errorf("link failed: %s (%s)", res.errCode, res.errDesc)
	}
}

// linkLoopback runs the default loopback flow: bind 127.0.0.1:0, start the flow, open the browser,
// serve /done, read the rc, and redeem (Bearer+flow_id+rc). `open` is injectable for tests.
func linkLoopback(ctx context.Context, w io.Writer, src bearerSource, hc *http.Client, asURL, host string, confirmSwitch bool, open func(string) error) error {
	bearer, err := src.token(ctx)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	ln, err := bindLoopbackEphemeral()
	if err != nil {
		return err
	}
	port := ln.Addr().(*net.TCPAddr).Port

	authURL, flowID, err := doStart(ctx, hc, asURL, bearer, "loopback", port, host)
	if err != nil {
		_ = ln.Close()
		return err
	}

	rcCh := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/done", func(rw http.ResponseWriter, r *http.Request) {
		renderDonePage(rw)
		select {
		case rcCh <- r.URL.Query().Get("rc"):
		default:
		}
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	fmt.Fprintln(w, "Opening a browser to authorize GitHub...")
	if err := open(authURL); err != nil {
		fmt.Fprintf(w, "Could not open a browser. Open this URL manually:\n\n  %s\n\n", authURL)
	}

	tctx, cancel := context.WithTimeout(ctx, ghLinkTimeout)
	defer cancel()
	var rc string
	select {
	case rc = <-rcCh:
	case <-tctx.Done():
		return fmt.Errorf("gh link timed out after %s", ghLinkTimeout)
	}
	if rc == "" {
		return errors.New("loopback callback carried no completion token")
	}

	res, err := doRedeem(ctx, hc, asURL, bearer, flowID, rc, confirmSwitch)
	if err != nil {
		return err
	}
	return finishRedeem(w, res)
}

// linkDevice runs the device flow: start, print the authorize URL + consent warning, then poll
// redeem (Bearer+flow_id). 202 → keep polling; 401 → refresh Bearer + retry; 409 → surface the
// identity change (gated behind --confirm-switch); terminal 4xx → fail fast; 200 → done.
//
// NOTE: §6.3's terse "409 retry" is realized as a fail-with-instruction: a blind re-redeem without
// confirm_switch would livelock (the AS keeps returning 409), so the identity-continuity gate (§6.5)
// requires an explicit confirmed re-run (--confirm-switch). This matches the web modal's
// confirm_switch=true re-redeem.
func linkDevice(ctx context.Context, w io.Writer, src bearerSource, hc *http.Client, asURL, host string, confirmSwitch bool) error {
	bearer, err := src.token(ctx)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	authURL, flowID, err := doStart(ctx, hc, asURL, bearer, "device", 0, host)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "\n%s\n\nTo authorize, open this URL in a browser (on any device):\n\n  %s\n\n", ghConsentWarning, authURL)

	deadline := time.Now().Add(ghDevicePollMax)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return errors.New("gh link device flow timed out")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(ghDevicePollInterval):
		}

		res, err := doRedeem(ctx, hc, asURL, bearer, flowID, "", confirmSwitch)
		if err != nil {
			continue // transient network error; keep polling
		}
		switch res.outcome {
		case redeemPending:
			continue
		case redeemUnauthorized:
			if rerr := src.refresh(ctx); rerr != nil {
				return fmt.Errorf("token refresh: %w", rerr)
			}
			if bearer, err = src.token(ctx); err != nil {
				return fmt.Errorf("auth: %w", err)
			}
			continue
		case redeemDone:
			printLink(w, res.link)
			return nil
		case redeemIdentityChange:
			return fmt.Errorf("GitHub identity is changing: @%s → @%s; re-run with --device --confirm-switch to proceed", res.oldLogin, res.newLogin)
		default:
			return fmt.Errorf("link failed: %s (%s)", res.errCode, res.errDesc)
		}
	}
}

// selectDeviceFlow picks the device flow when explicitly requested (--device / --no-browser) or
// when no browser is reachable (headless). Otherwise the loopback flow is used.
func selectDeviceFlow(device, noBrowser, browserReachableFlag bool) bool {
	return device || noBrowser || !browserReachableFlag
}

// bindLoopbackEphemeral binds a loopback listener whose port satisfies the AS's ephemeral-port
// constraint [49152,65535] (the AS rejects ports outside this range for loopback flows). The OS
// sometimes assigns ports below that range (Linux ip_local_port_range defaults to 32768–60999),
// so we retry or probe explicitly when needed.
func bindLoopbackEphemeral() (net.Listener, error) {
	const (
		ephMin = 49152
		ephMax = 65535
	)
	// Try OS-assigned first (commonly in range).
	for range 5 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("listen: %w", err)
		}
		if p := ln.Addr().(*net.TCPAddr).Port; p >= ephMin && p <= ephMax {
			return ln, nil
		}
		_ = ln.Close()
	}
	// Explicit probe of the range.
	for p := ephMin; p <= ephMax; p++ {
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p)); err == nil {
			return ln, nil
		}
	}
	return nil, errors.New("could not bind a loopback port in the ephemeral range [49152,65535]")
}

// browserReachable is a best-effort GUI check used to auto-select the device flow on headless hosts.
func browserReachable() bool {
	if runtime.GOOS == "linux" {
		return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
	}
	return true
}

func ghLinkCmd() *cli.Command {
	return &cli.Command{
		Name:  "link",
		Usage: "link or relink a GitHub account (loopback by default; device on shared/headless hosts)",
		Description: "Links your GitHub account to Spawnery via the Spawnery GitHub App.\n\n" +
			ghLoopbackPrecondition,
		Flags: []cli.Flag{
			configDirFlag(),
			&cli.StringFlag{Name: "as", Usage: "AS base URL (default: logged-in session)"},
			&cli.BoolFlag{Name: "device", Usage: "use the device flow (print URL + poll) instead of loopback"},
			&cli.BoolFlag{Name: "no-browser", Usage: "do not open a browser; use the device flow"},
			&cli.StringFlag{Name: "host", Usage: "GitHub host (default: AS default, e.g. github.com)"},
			&cli.BoolFlag{Name: "confirm-switch", Usage: "confirm changing the linked GitHub identity"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			dir, err := resolveDir(c)
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			hc := &http.Client{Timeout: 30 * time.Second}
			asURL := resolveASURL(c, dir)
			src := newBearerSource(dir, "", hc)
			host := c.String("host")
			confirm := c.Bool("confirm-switch")

			if selectDeviceFlow(c.Bool("device"), c.Bool("no-browser"), browserReachable()) {
				if err := linkDevice(ctx, c.Writer, src, hc, asURL, host, confirm); err != nil {
					return cli.Exit(err.Error(), 1)
				}
				return nil
			}
			if err := linkLoopback(ctx, c.Writer, src, hc, asURL, host, confirm, openBrowser); err != nil {
				return cli.Exit(err.Error(), 1)
			}
			return nil
		},
	}
}
