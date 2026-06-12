// Command authsvc runs the Spawnery Auth Service: the identity root of trust, deployed in its own
// container apart from the CP. It loads the Root CA cert + the self-hosted intermediate (cert + key it
// alone holds) and serves the AS HTTP surface. Enrollment + session signing land on top (sp-0qc,
// sp-3ca). See docs/superpowers/specs/2026-06-05-node-auth-unified-identity-design.md.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"spawnery/internal/authsvc"
	"spawnery/internal/pki"
	"spawnery/internal/weborigin"
)

func main() {
	svc, err := buildService()
	if err != nil {
		log.Fatalf("authsvc: %v", err)
	}

	// Browser-origin allowlist, same mechanism as the CP's ([WL6]): every device-set RPC is a
	// browser->AS call. Empty = dev mode (localhost origins only).
	allow := weborigin.FromEnv(env("AS_ALLOWED_ORIGINS", ""))
	if allow.Dev() {
		log.Printf("authsvc: AS_ALLOWED_ORIGINS unset — dev mode, allowing localhost browser origins only")
	}

	addr := env("AS_LISTEN", "127.0.0.1:8090")
	srv := &http.Server{Addr: addr, Handler: allow.CORS(svc.Handler())}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sd, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sd)
	}()

	log.Printf("authsvc listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("authsvc: %v", err)
	}
}

// buildService loads the AS's CA material. AS_DEV=1 bootstraps an ephemeral in-memory CA (for `just
// dev` / local testing — NOT production); otherwise it loads the persisted Root + self-hosted
// intermediate (cert + key) from disk.
func buildService() (*authsvc.Service, error) {
	if os.Getenv("AS_DEV") == "1" {
		root, err := pki.NewRootCA("Spawnery Dev Root")
		if err != nil {
			return nil, err
		}
		inter, err := root.NewIntermediate(pki.ClassSelfHosted)
		if err != nil {
			return nil, err
		}
		log.Printf("authsvc: DEV MODE — ephemeral in-memory CA (do NOT use in production)")
		return authsvc.New(root.Cert, inter), nil
	}
	return authsvc.Load(
		mustRead("AS_ROOT_CA_PEM", "/etc/spawnery/as/root-ca.pem"),
		mustRead("AS_INTERMEDIATE_CERT_PEM", "/etc/spawnery/as/self-hosted-intermediate.pem"),
		mustRead("AS_INTERMEDIATE_KEY_PEM", "/etc/spawnery/as/self-hosted-intermediate-key.pem"),
	)
}

func mustRead(envKey, def string) []byte {
	path := env(envKey, def)
	b, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("authsvc: read %s (%s): %v", envKey, path, err)
	}
	return b
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
