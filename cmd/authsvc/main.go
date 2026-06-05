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
)

func main() {
	root := mustRead("AS_ROOT_CA_PEM", "/etc/spawnery/as/root-ca.pem")
	interCert := mustRead("AS_INTERMEDIATE_CERT_PEM", "/etc/spawnery/as/self-hosted-intermediate.pem")
	interKey := mustRead("AS_INTERMEDIATE_KEY_PEM", "/etc/spawnery/as/self-hosted-intermediate-key.pem")

	svc, err := authsvc.Load(root, interCert, interKey)
	if err != nil {
		log.Fatalf("authsvc: load CA material: %v", err)
	}

	addr := env("AS_LISTEN", "127.0.0.1:8090")
	srv := &http.Server{Addr: addr, Handler: svc.Handler()}

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
