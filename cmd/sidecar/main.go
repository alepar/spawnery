package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"spawnery/internal/metrics"
	"spawnery/internal/safego"
	"spawnery/internal/sidecar"
)

// shutdownGrace is deliberately longer than authsvc's 10s because in-flight streaming inference
// requests (/v1/messages) can be long-lived. Promote to an env knob only if ops later need it.
const shutdownGrace = 30 * time.Second

func main() {
	upstream := getenv("SIDECAR_UPSTREAM", "https://openrouter.ai/api")
	key := os.Getenv("OPENROUTER_API_KEY")
	addr := getenv("SIDECAR_ADDR", "127.0.0.1:8080")
	controlToken := os.Getenv("SIDECAR_CONTROL_TOKEN")
	controlAddr := os.Getenv("SIDECAR_CONTROL_ADDR")
	if key == "" {
		log.Printf("sidecar starting without OPENROUTER_API_KEY; waiting for live credentials or forwarding with an empty bearer")
	}
	log.Printf("sidecar listening on %s -> %s", addr, upstream)

	// Live model override shared by both proxy paths; empty => passthrough.
	ov := &sidecar.Override{}
	inflight := sidecar.NewInflight()

	// /v1/messages is the Anthropic Messages API converter (Claude Code); everything else is
	// the transparent OpenAI passthrough (opencode/goose).
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	mux.Handle("/v1/messages", sidecar.NewMessagesHandler(upstream, key, ov, inflight))
	h, err := sidecar.NewHandler(upstream, key, ov, inflight)
	if err != nil {
		log.Fatalf("sidecar: invalid SIDECAR_UPSTREAM %q: %v", upstream, err)
	}
	mux.Handle("/", h)

	proxySrv := &http.Server{Addr: addr, Handler: mux}
	servers := []*http.Server{proxySrv}

	// Control server: a second listener on the pod IP (not loopback) so the node can set the
	// override. Started only when both a token and an address are configured.
	if controlToken != "" && controlAddr != "" {
		log.Printf("sidecar control listening on %s", controlAddr)
		controlSrv := &http.Server{Addr: controlAddr, Handler: sidecar.NewControlHandler(ov, controlToken, inflight)}
		servers = append(servers, controlSrv)
	} else {
		log.Printf("sidecar control endpoint disabled (set SIDECAR_CONTROL_TOKEN and SIDECAR_CONTROL_ADDR to enable)")
	}

	// GitHub MITM forward proxy (sp-n7iy.4): enabled when SIDECAR_GITHUB_PROXY_ADDR is set and a
	// control transport is configured. Disabled ⇒ log notice and skip (inference proxy unchanged).
	sidecar.StartGitHubProxy(os.Getenv)

	lns, err := bindAll(servers...)
	if err != nil {
		log.Fatalf("sidecar: bind failed: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := serveAll(ctx, shutdownGrace, servers, lns); err != nil {
		log.Fatalf("sidecar: %v", err)
	}
}

// bindAll opens a TCP listener for each server. On the first error it closes any already-bound
// listeners and returns the error (fail-fast, mirrors spawnlet's synchronous bind-then-fatal).
func bindAll(servers ...*http.Server) ([]net.Listener, error) {
	lns := make([]net.Listener, 0, len(servers))
	for _, s := range servers {
		ln, err := net.Listen("tcp", s.Addr)
		if err != nil {
			for _, open := range lns {
				open.Close()
			}
			return nil, err
		}
		lns = append(lns, ln)
	}
	return lns, nil
}

// serveAll launches each server on its pre-bound listener and blocks until ctx is cancelled or a
// server returns a non-ErrServerClosed error. On ctx cancellation it calls Shutdown on every
// server with a deadline of grace, then returns the first Shutdown error (nil on clean drain).
func serveAll(ctx context.Context, grace time.Duration, servers []*http.Server, lns []net.Listener) error {
	errc := make(chan error, len(servers))
	for i, s := range servers {
		safego.Go("sidecar.serve", func(srv *http.Server, ln net.Listener) func() {
			return func() {
				if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
					errc <- err
				}
			}
		}(s, lns[i]))
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	sd, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	var firstErr error
	for _, s := range servers {
		if err := s.Shutdown(sd); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
