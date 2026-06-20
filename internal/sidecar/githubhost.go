package sidecar

import (
	"net"
	"strings"

	"golang.org/x/net/idna"
)

// ghAction is the per-host MITM policy for GitHub connections.
type ghAction int

const (
	// actionTunnel: plain CONNECT tunnel, no injection (presigned object stores, non-GitHub hosts).
	actionTunnel ghAction = iota
	// actionMitmBasic: MITM + inject Authorization: Basic base64("x-access-token:"+token).
	// Used for git smart-HTTP (github.com, codeload.github.com).
	actionMitmBasic
	// actionMitmBearer: MITM + inject Authorization: Bearer <token>.
	// Used for the GitHub API, raw content, uploads, etc.
	actionMitmBearer
)

// injectBasic maps hosts that receive Basic auth injection (git smart-HTTP / LFS batch).
// SECURITY: these are EXACT host names — never substring or suffix-match for inject hosts.
// A loose predicate would let an attacker-registered look-alike obtain the real token. (roast r4)
var injectBasic = map[string]bool{
	"github.com":          true,
	"codeload.github.com": true,
}

// injectBearer maps hosts that receive Bearer auth injection (REST/GraphQL/raw content).
// SECURITY: exact equality only — same rationale as injectBasic.
var injectBearer = map[string]bool{
	"api.github.com":          true,
	"uploads.github.com":      true,
	"gist.github.com":         true,
	"raw.githubusercontent.com": true,
}

// tunnelExact maps hosts that must be CONNECT-tunneled, never MITM'd (presigned object stores).
// Injecting auth onto these hosts causes S3 "Only one auth mechanism allowed".
var tunnelExact = map[string]bool{
	"objects.githubusercontent.com": true,
	"lfs.github.com":               true,
}

// classifyGitHubHost returns the MITM action for the given hostport. It is the security-critical
// host predicate (§2.2, roast r4 BLOCKER): every inject host is matched by EXACT equality
// (case-folded, IDNA-normalised, port-stripped) — never by substring or unanchored suffix.
//
// Match order:
//  1. Exact inject-basic table.
//  2. Exact inject-bearer table.
//  3. Exact tunnel table (object stores that must not be injected).
//  4. Dot-anchored suffix matches for object-store wildcard families — tunnel only, never inject.
//  5. Default: actionTunnel (plain CONNECT; non-GitHub hosts fall here too).
func classifyGitHubHost(hostport string) ghAction {
	h := normalizeHost(hostport)
	if h == "" {
		return actionTunnel
	}

	// 1. Exact inject-basic
	if injectBasic[h] {
		return actionMitmBasic
	}
	// 2. Exact inject-bearer
	if injectBearer[h] {
		return actionMitmBearer
	}
	// 3. Exact tunnel-only object stores
	if tunnelExact[h] {
		return actionTunnel
	}
	// 4. Dot/dash-anchored suffix matches for object-store families — tunnel only.
	//    "-cloud.githubusercontent.com" (e.g. "media-cloud.githubusercontent.com").
	//    ".s3.amazonaws.com" (e.g. "github-cloud.s3.amazonaws.com").
	//    SECURITY: suffix match is intentionally restricted to tunnel-only (never inject) object stores.
	if strings.HasSuffix(h, "-cloud.githubusercontent.com") {
		return actionTunnel
	}
	if strings.HasSuffix(h, ".s3.amazonaws.com") {
		return actionTunnel
	}

	// 5. Default: plain tunnel (non-GitHub hosts, anything not explicitly listed above).
	return actionTunnel
}

// normalizeHost strips the port from hostport, lower-cases the host, and applies IDNA
// ToASCII normalisation. On IDNA error (e.g. a unicode look-alike that cannot be normalised
// to a valid A-label) it returns "" so the caller defaults to actionTunnel (safe).
func normalizeHost(hostport string) string {
	h := hostport
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		h = host
	}
	h = strings.ToLower(h)
	if h == "" {
		return ""
	}
	// IDNA normalise so a unicode look-alike cannot pass as a real GitHub host.
	norm, err := idna.Lookup.ToASCII(h)
	if err != nil {
		// IDNA error — reject (safe default: tunnel).
		return ""
	}
	return norm
}
