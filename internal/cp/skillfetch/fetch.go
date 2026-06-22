package skillfetch

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// allowedHosts is the set of hostnames permitted for fetching GitHub tarballs.
// Each redirect hop must have its target host in this set (§4.9).
var allowedHosts = map[string]bool{
	"github.com":         true,
	"api.github.com":     true,
	"codeload.github.com": true,
}

const maxRedirects = 10

// ErrRateLimit is returned when GitHub responds with HTTP 429.
type ErrRateLimit struct {
	RetryAfter string
}

func (e *ErrRateLimit) Error() string {
	if e.RetryAfter != "" {
		return fmt.Sprintf("GitHub rate limit exceeded; retry after %s", e.RetryAfter)
	}
	return "GitHub rate limit exceeded"
}

// secureClient is an HTTP client with per-hop host allowlisting and IP-range blocking.
type secureClient struct {
	client *http.Client
}

// newSecureClient creates a secure HTTP client that:
//   - validates each redirect hop's host against the allowlist
//   - resolves the target host and rejects private/loopback/link-local/metadata IPs
func newSecureClient() *secureClient {
	transport := &http.Transport{
		DialContext: blockedDialContext(),
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConns:          10,
	}
	c := &http.Client{
		Transport: transport,
		Timeout:   HTTPTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("too many redirects (max %d)", maxRedirects)
			}
			host := req.URL.Hostname()
			if !allowedHosts[host] {
				return fmt.Errorf("redirect to disallowed host %q (must be one of: github.com, api.github.com, codeload.github.com)", host)
			}
			return nil
		},
	}
	return &secureClient{client: c}
}

// blockedDialContext returns a DialContext that resolves the target address and rejects
// private/loopback/link-local/CGNAT/metadata IPs (SSRF protection).
func blockedDialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("parse addr %q: %w", addr, err)
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve %q: %w", host, err)
		}
		for _, ip := range ips {
			if isBlockedIP(ip.IP) {
				return nil, fmt.Errorf("host %q resolves to blocked IP %s (SSRF protection: loopback/private/link-local/metadata ranges rejected)", host, ip.IP)
			}
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(host, port))
	}
}

// isBlockedIP returns true for IPs in private/loopback/link-local/CGNAT/metadata ranges.
func isBlockedIP(ip net.IP) bool {
	blocked := []string{
		"127.0.0.0/8",      // loopback
		"10.0.0.0/8",       // RFC1918
		"172.16.0.0/12",    // RFC1918
		"192.168.0.0/16",   // RFC1918
		"169.254.0.0/16",   // link-local + metadata (169.254.169.254)
		"100.64.0.0/10",    // CGNAT (RFC6598)
		"fc00::/7",         // IPv6 ULA
		"fe80::/10",        // IPv6 link-local
		"::1/128",          // IPv6 loopback
	}
	for _, cidr := range blocked {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// fetchAndUnpack downloads the tarball from rawURL, gunzips, strips the GitHub wrapper dir,
// descends into subdir if given, and returns the in-memory entries.
// It enforces streaming bounds before any buffering.
func (s *secureClient) fetchAndUnpack(ctx context.Context, rawURL, token, subdir string) ([]tarEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		// Wrap redirect errors for actionable messaging
		if ue, ok := err.(*url.Error); ok {
			return nil, fmt.Errorf("fetch %s: %w", redactURL(rawURL), ue.Err)
		}
		return nil, fmt.Errorf("fetch %s: %w", redactURL(rawURL), err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// continue
	case http.StatusTooManyRequests:
		retryAfter := resp.Header.Get("Retry-After")
		return nil, &ErrRateLimit{RetryAfter: retryAfter}
	default:
		return nil, fmt.Errorf("GitHub returned HTTP %d for %s", resp.StatusCode, redactURL(rawURL))
	}

	// Wire cap on compressed body
	wireReader := &io.LimitedReader{R: resp.Body, N: WireCapBytes + 1}

	// gzip decode
	gz, err := gzip.NewReader(wireReader)
	if err != nil {
		return nil, fmt.Errorf("gzip init: %w", err)
	}
	defer gz.Close()

	// Decompressed size cap (enforced streaming, before any parse)
	decompReader := &io.LimitedReader{R: gz, N: DecompressedCapBytes + 1}

	tr := tar.NewReader(decompReader)

	// Determine wrapper prefix (GitHub wraps in owner-repo-<sha>/)
	wrapperPrefix := ""
	fileCount := 0
	var entries []tarEntry

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar read: %w", err)
		}

		// Check decompressed cap
		if decompReader.N <= 0 {
			return nil, fmt.Errorf("decompressed tarball exceeds cap (%d bytes)", DecompressedCapBytes)
		}
		// Check wire cap
		if wireReader.N <= 0 {
			return nil, fmt.Errorf("compressed tarball exceeds wire cap (%d bytes)", WireCapBytes)
		}

		// Reject non-regular entries
		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeRegA, tar.TypeDir:
			// allowed
		case tar.TypeSymlink, tar.TypeLink:
			return nil, fmt.Errorf("symlink/hardlink entries are not allowed (entry %q)", hdr.Name)
		default:
			return nil, fmt.Errorf("non-regular tar entry type %d rejected (entry %q)", hdr.Typeflag, hdr.Name)
		}

		// Strip wrapper prefix (first entry gives us the wrapper dir)
		entryName := hdr.Name
		if wrapperPrefix == "" {
			// First entry should be the wrapper dir (e.g. "owner-repo-<sha>/")
			parts := strings.SplitN(entryName, "/", 2)
			if len(parts) >= 1 && parts[0] != "" {
				wrapperPrefix = parts[0] + "/"
			}
		}
		if wrapperPrefix != "" {
			entryName = strings.TrimPrefix(entryName, wrapperPrefix)
		}

		// Descend into subdir if given
		if subdir != "" {
			subdirPrefix := strings.TrimSuffix(subdir, "/") + "/"
			if entryName == strings.TrimSuffix(subdir, "/") || entryName == subdirPrefix {
				// Skip the subdir entry itself
				continue
			}
			if !strings.HasPrefix(entryName, subdirPrefix) {
				// Not in the requested subdir
				continue
			}
			entryName = strings.TrimPrefix(entryName, subdirPrefix)
		}

		if entryName == "" {
			continue
		}

		// Validate path safety
		cleaned, err := safeRelPath(entryName)
		if err != nil {
			return nil, err
		}
		entryName = cleaned

		fileCount++
		if fileCount > FileCountCap {
			return nil, fmt.Errorf("too many files in tarball (max %d)", FileCountCap)
		}

		if hdr.Typeflag == tar.TypeDir {
			entries = append(entries, tarEntry{
				path:  entryName,
				mode:  hdr.Mode,
				isDir: true,
			})
			continue
		}

		// Read file content with per-file cap (same decompressed budget)
		content, err := io.ReadAll(io.LimitReader(tr, DecompressedCapBytes))
		if err != nil {
			return nil, fmt.Errorf("read entry %q: %w", hdr.Name, err)
		}

		entries = append(entries, tarEntry{
			path:    entryName,
			mode:    hdr.Mode,
			isDir:   false,
			content: content,
		})
	}

	return entries, nil
}

// redactURL strips the path from a URL for safe logging (avoids leaking tokens in query strings).
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[url parse error]"
	}
	return u.Scheme + "://" + u.Host + u.Path
}
