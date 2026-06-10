// Command spawnery-ca generates CA material. The `dev` subcommand writes a complete LOCAL dev bundle —
// Root CA, self-hosted intermediate, a CP node-listener server cert, and a pre-provisioned node identity
// — so `just dev-enforced` can run the enforced (mTLS) node<->CP loop without an enrollment round-trip.
// NOT for production: production root/intermediate keys are generated in an offline ceremony.
package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

	"spawnery/internal/node/nodeid"
	"spawnery/internal/pki"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "dev" {
		log.Fatalf("usage: spawnery-ca dev [dir]   (default dir: .dev-ca)")
	}
	dir := ".dev-ca"
	if len(os.Args) >= 3 {
		dir = os.Args[2]
	}
	if err := genDev(dir); err != nil {
		log.Fatalf("spawnery-ca: %v", err)
	}
	log.Printf("spawnery-ca: dev CA written to %s (root.pem, self-hosted-intermediate.*, cp-server.*, node/)", dir)
}

func genDev(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	year := time.Now().Add(365 * 24 * time.Hour)

	root, err := pki.NewRootCA("Spawnery Dev Root")
	if err != nil {
		return fmt.Errorf("root: %w", err)
	}
	selfHosted, err := root.NewIntermediate(pki.ClassSelfHosted)
	if err != nil {
		return fmt.Errorf("intermediate: %w", err)
	}
	cpSrv, err := root.IssueServer("cp-node-listener", []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")}, year)
	if err != nil {
		return fmt.Errorf("cp server cert: %w", err)
	}
	// Pre-provisioned dev node identity: node-1 owned by "alice" (matches CP_DEV_TOKENS=dev-token=alice).
	node, err := selfHosted.IssueNode("node-1", "alice", pki.ClassSelfHosted, year)
	if err != nil {
		return fmt.Errorf("node cert: %w", err)
	}

	rootKey, err := pki.MarshalKeyPEM(root.Key)
	if err != nil {
		return err
	}
	shKey, err := pki.MarshalKeyPEM(selfHosted.Key)
	if err != nil {
		return err
	}
	cpKey, err := pki.MarshalKeyPEM(cpSrv.Key)
	if err != nil {
		return err
	}
	nodeKey, err := pki.MarshalKeyPEM(node.Key)
	if err != nil {
		return err
	}

	files := []struct {
		name string
		data []byte
	}{
		{"root.pem", pki.MarshalCertPEM(root.Cert)},
		{"root-key.pem", rootKey},
		{"self-hosted-intermediate.pem", pki.MarshalCertPEM(selfHosted.Cert)},
		{"self-hosted-intermediate-key.pem", shKey},
		{"cp-server.pem", pki.MarshalCertPEM(cpSrv.Cert)},
		{"cp-server-key.pem", cpKey},
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f.name), f.data, 0o600); err != nil {
			return err
		}
	}

	return nodeid.Save(filepath.Join(dir, "node"), nodeid.Identity{
		CertPEM:  pki.MarshalCertPEM(node.Cert),
		ChainPEM: pki.MarshalCertPEM(selfHosted.Cert),
		KeyPEM:   nodeKey,
		RootPEM:  pki.MarshalCertPEM(root.Cert),
	})
}
