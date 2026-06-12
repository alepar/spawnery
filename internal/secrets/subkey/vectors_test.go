package subkey_test

// Cross-language test vectors for subkey.VerifyNodeForSealing / SignedSubKey.
//
// TestGenerateSubKeyVectors validates the golden JSON in
// testdata/subkey/verify_node.json. Pass -update to regenerate.
//
// The vitest reader (web/src/keys/subkey-vectors.test.ts) reads the same file.

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"spawnery/internal/pki"
	"spawnery/internal/secrets/subkey"
)

var updateSubKey = flag.Bool("update-subkey", false, "regenerate testdata/subkey/verify_node.json")

// vectorTime is a fixed reference time used so all vector timestamps are stable.
var vectorTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// subKeyVector is the JSON shape written by this generator and read by the TS test.
type subKeyVector struct {
	// Root CA PEM (the client-pinned root, embedded in the web bundle).
	RootPEM string `json:"root_pem"`
	// Leaf cert PEM (what the CP relays as node_cert_chain — just the leaf for this vector).
	LeafPEM string `json:"leaf_pem"`
	// Intermediate cert PEM (the class intermediate chaining leaf to root).
	IntermediatePEM string `json:"intermediate_pem"`
	// Full chain PEM: leaf + intermediate (what GetSpawnNodeKeyResponse.NodeCertChain returns).
	ChainPEM string `json:"chain_pem"`
	// SignedSubKey as a JSON string (verbatim from the CP wire format).
	SubKeyJSON string `json:"subkey_json"`
	// Expected HPKE pubkey (raw 32 bytes, base64). TS must return this after verify.
	ExpectedHPKEPub string `json:"expected_hpke_pub"`
	// Expected verified identity.
	ExpectedNodeID    string `json:"expected_node_id"`
	ExpectedAccountID string `json:"expected_account_id"`
	ExpectedClass     string `json:"expected_class"`
	// Validity window for the sub-key (ISO 8601, UTC).
	NotBefore string `json:"not_before"`
	NotAfter  string `json:"not_after"`
	// Leaf cert notAfter (for cert-validity negative tests).
	LeafNotAfter string `json:"leaf_not_after"`
	// Negative test vectors for the TS x509 verifier.
	// ForgedCloudChainPEM: a cloud-SAN leaf signed by the self-hosted intermediate —
	// signatures are valid but name constraints are violated (Go rejects it; TS must too).
	ForgedCloudChainPEM string `json:"forged_cloud_chain_pem"`
	// NonCALeafChainPEM: a new leaf cert "signed" by the original leaf cert used as a CA —
	// the chain cert lacks basicConstraints CA:TRUE (both Go and TS must reject it).
	NonCALeafChainPEM string `json:"non_ca_leaf_chain_pem"`
}

const vectorsFile = "testdata/subkey/verify_node.json"

func TestGenerateSubKeyVectors(t *testing.T) {
	if *updateSubKey {
		generateSubKeyVectors(t)
	}
	verifySubKeyVectors(t)
}

func generateSubKeyVectors(t *testing.T) {
	t.Helper()

	// Issue a self-hosted node cert chain.
	r, err := pki.NewRootCA("vector-root")
	if err != nil {
		t.Fatalf("NewRootCA: %v", err)
	}
	inter, err := r.NewIntermediate(pki.ClassSelfHosted)
	if err != nil {
		t.Fatalf("NewIntermediate: %v", err)
	}
	n, err := inter.IssueNode("node1", "alice", pki.ClassSelfHosted, vectorTime.Add(365*24*time.Hour))
	if err != nil {
		t.Fatalf("IssueNode: %v", err)
	}

	// Mint a sub-key signed by the node's cert key. Use real time for the
	// validity window so the cert chain and sub-key validity windows align
	// (the cert's NotBefore is time.Now()-1m, sub-key starts at time.Now()).
	skNow := time.Now().UTC().Truncate(time.Second)
	sk, err := subkey.Sign(n.Key, "node1", make([]byte, 32), skNow, skNow.Add(72*time.Hour))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	skJSON, err := json.Marshal(sk)
	if err != nil {
		t.Fatalf("marshal sub-key: %v", err)
	}

	leafPEM  := pki.MarshalCertPEM(n.Cert)
	interPEM := pki.MarshalCertPEM(inter.Cert)
	rootPEM  := pki.MarshalCertPEM(r.Cert)
	chainPEM := string(leafPEM) + string(interPEM)

	// Forged-cloud negative vector: SH intermediate signs a cloud-class leaf.
	// The signature chain is cryptographically valid but violates name constraints.
	forgedCloudNode, err := inter.IssueNode("forge", "acc", pki.ClassCloud, vectorTime.Add(365*24*time.Hour))
	if err != nil {
		t.Fatalf("IssueNode (forged cloud): %v", err)
	}
	forgedCloudChainPEM := string(pki.MarshalCertPEM(forgedCloudNode.Cert)) + string(interPEM)

	// Non-CA-intermediate negative vector: use the leaf cert's key to sign another leaf.
	// The resulting chain has a non-CA cert at position [1]; both Go and TS must reject it.
	fakeCA := &pki.CA{Cert: n.Cert, Key: n.Key}
	nonCANode, err := fakeCA.IssueNode("n2", "alice", pki.ClassSelfHosted, vectorTime.Add(365*24*time.Hour))
	if err != nil {
		t.Fatalf("IssueNode (non-CA chain): %v", err)
	}
	nonCALeafChainPEM := string(pki.MarshalCertPEM(nonCANode.Cert)) + string(leafPEM)

	v := subKeyVector{
		RootPEM:             string(rootPEM),
		LeafPEM:             string(leafPEM),
		IntermediatePEM:     string(interPEM),
		ChainPEM:            chainPEM,
		SubKeyJSON:          string(skJSON),
		ExpectedHPKEPub:     base64.StdEncoding.EncodeToString(sk.HPKEPub),
		ExpectedNodeID:      "node1",
		ExpectedAccountID:   "alice",
		ExpectedClass:       pki.ClassSelfHosted,
		NotBefore:           sk.NotBefore.UTC().Format(time.RFC3339Nano),
		NotAfter:            sk.NotAfter.UTC().Format(time.RFC3339Nano),
		LeafNotAfter:        n.Cert.NotAfter.UTC().Format(time.RFC3339),
		ForgedCloudChainPEM: forgedCloudChainPEM,
		NonCALeafChainPEM:   nonCALeafChainPEM,
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal vector: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(vectorsFile), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(vectorsFile, append(b, '\n'), 0o600); err != nil {
		t.Fatalf("write vector file: %v", err)
	}
	t.Logf("wrote %s", vectorsFile)
}

func verifySubKeyVectors(t *testing.T) {
	t.Helper()
	data, err := os.ReadFile(vectorsFile)
	if err != nil {
		t.Skipf("vector file not found: %v (run -update-subkey to generate)", err)
	}
	var v subKeyVector
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("unmarshal vector: %v", err)
	}

	// Re-parse the sub-key from JSON.
	var sk subkey.SignedSubKey
	if err := json.Unmarshal([]byte(v.SubKeyJSON), &sk); err != nil {
		t.Fatalf("unmarshal sub-key: %v", err)
	}

	// Use a time 1 hour into the sub-key's validity window (past cert NotBefore too).
	notBefore, _ := time.Parse(time.RFC3339Nano, v.NotBefore)
	verifyAt := notBefore.Add(time.Hour)

	// Verify using VerifyNodeForSealing (leaf + chain + root + sk + expect + revoked + now).
	trustHPKE, id, err := subkey.VerifyNodeForSealing(
		[]byte(v.LeafPEM),
		[]byte(v.IntermediatePEM),
		[]byte(v.RootPEM),
		sk,
		subkey.Expectation{Tenancy: pki.ClassSelfHosted, AccountID: "alice"},
		nil, // AllowAll revocation
		verifyAt, // within sub-key validity window
	)
	if err != nil {
		t.Fatalf("VerifyNodeForSealing: %v", err)
	}
	if base64.StdEncoding.EncodeToString(trustHPKE) != v.ExpectedHPKEPub {
		t.Errorf("hpke_pub mismatch: got %s want %s", base64.StdEncoding.EncodeToString(trustHPKE), v.ExpectedHPKEPub)
	}
	if id.NodeID != v.ExpectedNodeID {
		t.Errorf("node_id: got %q want %q", id.NodeID, v.ExpectedNodeID)
	}
	if id.AccountID != v.ExpectedAccountID {
		t.Errorf("account_id: got %q want %q", id.AccountID, v.ExpectedAccountID)
	}
	if id.Class != v.ExpectedClass {
		t.Errorf("class: got %q want %q", id.Class, v.ExpectedClass)
	}
}
