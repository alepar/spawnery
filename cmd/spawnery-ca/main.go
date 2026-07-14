// Command spawnery-ca generates CA material. The `dev` subcommand writes a complete LOCAL dev bundle —
// Root CA, self-hosted intermediate, a CP node-listener server cert, and a pre-provisioned node identity
// — so `just dev-enforced` can run the enforced (mTLS) node<->CP loop without an enrollment round-trip.
// NOT for production: production root/intermediate keys are generated in an offline ceremony.
package main

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc/token"
	"spawnery/internal/node/nodeid"
	"spawnery/internal/pki"
)

func main() {
	switch {
	case len(os.Args) >= 2 && os.Args[1] == "dev":
		dir := ".dev-ca"
		if len(os.Args) >= 3 {
			dir = os.Args[2]
		}
		if err := genDev(dir); err != nil {
			log.Fatalf("spawnery-ca: %v", err)
		}
		log.Printf("spawnery-ca: dev CA written to %s (role intermediates, service SVIDs, current CRLs, certified auth signers, node identities)", dir)
	case len(os.Args) == 5 && os.Args[1] == "node":
		// Re-mint ONLY the node identity (dir/node) under a given owner, reusing the existing
		// intermediate — so a dev node can be re-owned (e.g. to a real AS accountID) without
		// rotating the CA/session key and breaking a running stack.
		dir, nodeID, owner := os.Args[2], os.Args[3], os.Args[4]
		if err := remintNode(dir, nodeID, owner); err != nil {
			log.Fatalf("spawnery-ca: %v", err)
		}
		log.Printf("spawnery-ca: re-minted node identity %s owned by %q in %s/node", nodeID, owner, dir)
	case len(os.Args) == 9 && os.Args[1] == "auth-token":
		spki, err := base64.StdEncoding.DecodeString(os.Args[8])
		if err != nil {
			log.Fatalf("spawnery-ca: decode session SPKI: %v", err)
		}
		wire, err := mintAuthToken(os.Args[2], os.Args[3], os.Args[4], os.Args[5], os.Args[6], os.Args[7], spki, time.Now())
		if err != nil {
			log.Fatalf("spawnery-ca: %v", err)
		}
		fmt.Println(wire)
	case len(os.Args) == 7 && os.Args[1] == "signer-revocation":
		generation, err := strconv.ParseUint(os.Args[6], 10, 64)
		if err != nil {
			log.Fatalf("spawnery-ca: parse signer-revocation generation: %v", err)
		}
		wire, err := signSignerRevocation(os.Args[2], os.Args[3], os.Args[4], os.Args[5], generation, time.Now())
		if err != nil {
			log.Fatalf("spawnery-ca: %v", err)
		}
		fmt.Println(wire)
	default:
		log.Fatalf("usage:\n  spawnery-ca dev [dir]\n  spawnery-ca node <dir> <node-id> <owner>\n  spawnery-ca auth-token <root> <key> <chain> <environment> <cp|node> <account-id> <spki-base64>\n  spawnery-ca signer-revocation <issuer-cert> <issuer-key> <revoked-leaf-chain> <environment> <generation>")
	}
}

func signSignerRevocation(intermediatePath, keyPath, revokedChainPath, environment string, generation uint64, now time.Time) (string, error) {
	intermediateBytes, err := os.ReadFile(intermediatePath)
	if err != nil {
		return "", fmt.Errorf("read auth-signing intermediate: %w", err)
	}
	intermediate, err := pki.ParseCertPEM(intermediateBytes)
	if err != nil {
		return "", fmt.Errorf("parse auth-signing intermediate: %w", err)
	}
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return "", fmt.Errorf("read auth-signing intermediate key: %w", err)
	}
	keyBlock, rest := pem.Decode(keyBytes)
	if keyBlock == nil || len(rest) != 0 {
		return "", fmt.Errorf("parse auth-signing intermediate key: expected one PEM block")
	}
	parsedKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse auth-signing intermediate key: %w", err)
	}
	key, ok := parsedKey.(*ecdsa.PrivateKey)
	if !ok {
		return "", fmt.Errorf("parse auth-signing intermediate key: key is %T, want ECDSA", parsedKey)
	}
	chainBytes, err := os.ReadFile(revokedChainPath)
	if err != nil {
		return "", fmt.Errorf("read revoked signer chain: %w", err)
	}
	leafBlock, _ := pem.Decode(chainBytes)
	if leafBlock == nil || leafBlock.Type != "CERTIFICATE" {
		return "", fmt.Errorf("parse revoked signer chain: missing leaf certificate")
	}
	leaf, err := x509.ParseCertificate(leafBlock.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse revoked signer leaf: %w", err)
	}
	spki, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil {
		return "", err
	}
	fingerprint := sha256.Sum256(spki)
	payload, err := proto.Marshal(&authv1.SignerRevocationStatement{
		Environment:       environment,
		Generation:        generation,
		IssuedAt:          now.Unix(),
		RevokedSerials:    [][]byte{leaf.SerialNumber.Bytes()},
		RevokedSpkiSha256: [][]byte{fingerprint[:]},
	})
	if err != nil {
		return "", err
	}
	return token.SignSignerRevocationStatement(intermediate, key, payload)
}

func mintAuthToken(rootPath, keyPath, chainPath, environment, audience, accountID string, spki []byte, now time.Time) (string, error) {
	if audience != "cp" && audience != "node" {
		return "", fmt.Errorf("auth token audience %q must be cp or node", audience)
	}
	if accountID == "" || len(spki) == 0 {
		return "", fmt.Errorf("auth token account and session SPKI are required")
	}
	rootBytes, err := os.ReadFile(rootPath)
	if err != nil {
		return "", fmt.Errorf("read auth root: %w", err)
	}
	root, err := pki.ParseCertPEM(rootBytes)
	if err != nil {
		return "", fmt.Errorf("parse auth root: %w", err)
	}
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return "", fmt.Errorf("read auth signer key: %w", err)
	}
	keyBlock, rest := pem.Decode(keyBytes)
	if keyBlock == nil || len(rest) != 0 {
		return "", fmt.Errorf("parse auth signer key: expected one PEM block")
	}
	parsedKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse auth signer key: %w", err)
	}
	privateKey, ok := parsedKey.(ed25519.PrivateKey)
	if !ok {
		return "", fmt.Errorf("parse auth signer key: key is %T, want Ed25519", parsedKey)
	}
	chainBytes, err := os.ReadFile(chainPath)
	if err != nil {
		return "", fmt.Errorf("read auth signer chain: %w", err)
	}
	var chain []*x509.Certificate
	for len(chainBytes) > 0 {
		block, remaining := pem.Decode(chainBytes)
		if block == nil || block.Type != "CERTIFICATE" {
			return "", fmt.Errorf("parse auth signer chain: expected certificate PEM")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return "", fmt.Errorf("parse auth signer chain: %w", err)
		}
		chain = append(chain, cert)
		chainBytes = remaining
	}
	credential, err := token.NewSigningCredential(privateKey, chain, root, environment, now)
	if err != nil {
		return "", fmt.Errorf("validate auth signer credential: %w", err)
	}
	var tokenID [16]byte
	if _, err := rand.Read(tokenID[:]); err != nil {
		return "", fmt.Errorf("generate auth token ID: %w", err)
	}
	body := &authv1.SessionTokenBody{
		AccountId:      accountID,
		TokenId:        hex.EncodeToString(tokenID[:]),
		Audience:       audience,
		IssuedAt:       now.Unix(),
		ExpiresAt:      now.Add(15 * time.Minute).Unix(),
		SessionKeyHash: token.SessionKeyHash(spki),
		KeyId:          hex.EncodeToString(credential.KeyID[:]),
	}
	payload, err := proto.Marshal(body)
	if err != nil {
		return "", err
	}
	return credential.Sign(token.ArtifactTypeSession, payload)
}

// remintNode loads the existing self-hosted intermediate from <dir> and issues a fresh node
// identity (<dir>/node) bound to nodeID + owner, leaving all other CA material untouched.
func remintNode(dir, nodeID, owner string) error {
	interCertPEM, err := os.ReadFile(filepath.Join(dir, "self-hosted-intermediate.pem"))
	if err != nil {
		return fmt.Errorf("read intermediate cert: %w", err)
	}
	interKeyPEM, err := os.ReadFile(filepath.Join(dir, "self-hosted-intermediate-key.pem"))
	if err != nil {
		return fmt.Errorf("read intermediate key: %w", err)
	}
	rootCertPEM, err := os.ReadFile(filepath.Join(dir, "root.pem"))
	if err != nil {
		return fmt.Errorf("read root cert: %w", err)
	}
	interCert, err := pki.ParseCertPEM(interCertPEM)
	if err != nil {
		return fmt.Errorf("parse intermediate cert: %w", err)
	}
	interKey, err := pki.ParseKeyPEM(interKeyPEM)
	if err != nil {
		return fmt.Errorf("parse intermediate key: %w", err)
	}
	inter := &pki.CA{Cert: interCert, Key: interKey}

	node, err := inter.IssueNode(nodeID, owner, pki.ClassSelfHosted, time.Now().Add(365*24*time.Hour))
	if err != nil {
		return fmt.Errorf("node cert: %w", err)
	}
	nodeKey, err := pki.MarshalKeyPEM(node.Key)
	if err != nil {
		return err
	}
	return nodeid.Save(filepath.Join(dir, "node"), nodeid.Identity{
		CertPEM:  pki.MarshalCertPEM(node.Cert),
		ChainPEM: interCertPEM,
		KeyPEM:   nodeKey,
		RootPEM:  rootCertPEM,
	})
}

func genDev(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	year := time.Now().Add(365 * 24 * time.Hour)
	trustDomain := os.Getenv("SPAWNERY_TRUST_DOMAIN")
	if trustDomain == "" {
		trustDomain = pki.DefaultTrustDomain
	}
	environment := strings.TrimSuffix(trustDomain, ".spawnery.internal")

	root, err := pki.NewRootCA("Spawnery Dev Root")
	if err != nil {
		return fmt.Errorf("root: %w", err)
	}
	selfHosted, err := root.NewIntermediate(pki.ClassSelfHosted, trustDomain)
	if err != nil {
		return fmt.Errorf("intermediate: %w", err)
	}
	// Cloud intermediate + cloud node identity. The cloud node is multi-tenant (no owner
	// match at placement); it chains to the same root so the CP's internal trust accepts it.
	cloud, err := root.NewIntermediate(pki.ClassCloud, trustDomain)
	if err != nil {
		return fmt.Errorf("cloud intermediate: %w", err)
	}
	service, err := root.NewIntermediate(pki.IssuerService, trustDomain)
	if err != nil {
		return fmt.Errorf("service intermediate: %w", err)
	}
	authSigning, err := root.NewAuthSigningIntermediate(environment)
	if err != nil {
		return fmt.Errorf("auth-signing intermediate: %w", err)
	}
	authSignerCurrent, err := authSigning.IssueAuthArtifactSigner(environment, "current", time.Now().Add(90*24*time.Hour))
	if err != nil {
		return fmt.Errorf("current auth signer: %w", err)
	}
	authSignerNext, err := authSigning.IssueAuthArtifactSigner(environment, "next", time.Now().Add(90*24*time.Hour))
	if err != nil {
		return fmt.Errorf("next auth signer: %w", err)
	}
	cpSrv, err := service.IssueService(pki.RoleCP, "cp-1", trustDomain, []string{"cp.internal"}, []net.IP{net.ParseIP("127.0.0.1")}, year)
	if err != nil {
		return fmt.Errorf("cp service cert: %w", err)
	}
	authsvcSrv, err := service.IssueService(pki.RoleAuthService, "authsvc-1", trustDomain, []string{"authsvc.internal"}, []net.IP{net.ParseIP("127.0.0.1")}, year)
	if err != nil {
		return fmt.Errorf("authsvc service cert: %w", err)
	}
	// Pre-provisioned dev node identity: node-1 owned by "alice" (matches CP_DEV_TOKENS=dev-token=alice).
	// Used by node-enforced / dev-enforced (self-hosted lane).
	node, err := selfHosted.IssueNode("node-1", "alice", pki.ClassSelfHosted, trustDomain, year)
	if err != nil {
		return fmt.Errorf("node cert: %w", err)
	}
	// Cloud dev node identity: node-1 owned by "spawnery-system" (multi-tenant — no owner
	// check at placement; any logged-in user's spawns land here). Used by node-github / just dev.
	cloudNode, err := cloud.IssueNode("node-1", "spawnery-system", pki.ClassCloud, trustDomain, year)
	if err != nil {
		return fmt.Errorf("cloud node cert: %w", err)
	}

	rootKey, err := pki.MarshalKeyPEM(root.Key)
	if err != nil {
		return err
	}
	shKey, err := pki.MarshalKeyPEM(selfHosted.Key)
	if err != nil {
		return err
	}
	cloudKey, err := pki.MarshalKeyPEM(cloud.Key)
	if err != nil {
		return err
	}
	serviceKey, err := pki.MarshalKeyPEM(service.Key)
	if err != nil {
		return err
	}
	authSigningKey, err := pki.MarshalPKCS8KeyPEM(authSigning.Key)
	if err != nil {
		return err
	}
	authSignerCurrentKey, err := pki.MarshalPKCS8KeyPEM(authSignerCurrent.Key)
	if err != nil {
		return err
	}
	authSignerNextKey, err := pki.MarshalPKCS8KeyPEM(authSignerNext.Key)
	if err != nil {
		return err
	}
	cpKey, err := pki.MarshalKeyPEM(cpSrv.Key)
	if err != nil {
		return err
	}
	authsvcKey, err := pki.MarshalKeyPEM(authsvcSrv.Key)
	if err != nil {
		return err
	}
	crlNow := time.Now().UTC().Truncate(time.Second)
	crlNext := crlNow.Add(30 * 24 * time.Hour)
	serviceCRL, err := service.CreateCRL(big.NewInt(1), nil, crlNow, crlNext)
	if err != nil {
		return fmt.Errorf("service CRL: %w", err)
	}
	cloudCRL, err := cloud.CreateCRL(big.NewInt(1), nil, crlNow, crlNext)
	if err != nil {
		return fmt.Errorf("cloud CRL: %w", err)
	}
	selfHostedCRL, err := selfHosted.CreateCRL(big.NewInt(1), nil, crlNow, crlNext)
	if err != nil {
		return fmt.Errorf("self-hosted CRL: %w", err)
	}
	nodeKey, err := pki.MarshalKeyPEM(node.Key)
	if err != nil {
		return err
	}
	cloudNodeKey, err := pki.MarshalKeyPEM(cloudNode.Key)
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
		{"cloud-intermediate.pem", pki.MarshalCertPEM(cloud.Cert)},
		{"cloud-intermediate-key.pem", cloudKey},
		{"service-intermediate.pem", pki.MarshalCertPEM(service.Cert)},
		{"service-intermediate-key.pem", serviceKey},
		{"auth-signing-intermediate.pem", pki.MarshalCertPEM(authSigning.Cert)},
		{"auth-signing-intermediate-key.pem", authSigningKey},
		{"auth-signer-current-key.pem", authSignerCurrentKey},
		{"auth-signer-current-chain.pem", pki.MarshalCertChainPEM([]*x509.Certificate{authSignerCurrent.Cert, authSigning.Cert})},
		{"auth-signer-next-key.pem", authSignerNextKey},
		{"auth-signer-next-chain.pem", pki.MarshalCertChainPEM([]*x509.Certificate{authSignerNext.Cert, authSigning.Cert})},
		{"cp-server.pem", pki.MarshalCertPEM(cpSrv.Cert)},
		{"cp-server-key.pem", cpKey},
		{"cp-service.pem", pki.MarshalCertPEM(cpSrv.Cert)},
		{"cp-service-key.pem", cpKey},
		{"cp-service-chain.pem", pki.MarshalCertPEM(service.Cert)},
		{"authsvc-service.pem", pki.MarshalCertPEM(authsvcSrv.Cert)},
		{"authsvc-service-key.pem", authsvcKey},
		{"authsvc-service-chain.pem", pki.MarshalCertPEM(service.Cert)},
		{"service.crl.pem", pki.MarshalCRLPEM(serviceCRL)},
		{"cloud-node.crl.pem", pki.MarshalCRLPEM(cloudCRL)},
		{"self-hosted-node.crl.pem", pki.MarshalCRLPEM(selfHostedCRL)},
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f.name), f.data, 0o600); err != nil {
			return err
		}
	}

	// Self-hosted node identity (node-enforced / dev-enforced lane).
	if err := nodeid.Save(filepath.Join(dir, "node"), nodeid.Identity{
		CertPEM:  pki.MarshalCertPEM(node.Cert),
		ChainPEM: pki.MarshalCertPEM(selfHosted.Cert),
		KeyPEM:   nodeKey,
		RootPEM:  pki.MarshalCertPEM(root.Cert),
	}); err != nil {
		return err
	}
	// Cloud node identity (node-github / just dev lane).
	if err := nodeid.Save(filepath.Join(dir, "node-cloud"), nodeid.Identity{
		CertPEM:  pki.MarshalCertPEM(cloudNode.Cert),
		ChainPEM: pki.MarshalCertPEM(cloud.Cert),
		KeyPEM:   cloudNodeKey,
		RootPEM:  pki.MarshalCertPEM(root.Cert),
	}); err != nil {
		return err
	}

	return nil
}
