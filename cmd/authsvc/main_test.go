package main

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"spawnery/internal/authsvc/token"
	"spawnery/internal/pki"
)

func TestASValidateSigning(t *testing.T) {
	base := func() AS {
		var cfg AS
		cfg.FakeGithub = true
		cfg.GitHub.TokenEncKey = "configured"
		cfg.Signing.Environment = "prod"
		cfg.Signing.RootPEM = "root.pem"
		cfg.Signing.CurrentKeyPEM = "current-key.pem"
		cfg.Signing.CurrentChainPEM = "current-chain.pem"
		return cfg
	}

	for _, tc := range []struct {
		name string
		edit func(*AS)
		want string
	}{
		{"missing environment", func(c *AS) { c.Signing.Environment = "" }, "signing.environment"},
		{"missing root", func(c *AS) { c.Signing.RootPEM = "" }, "signing.root_pem"},
		{"missing current key", func(c *AS) { c.Signing.CurrentKeyPEM = "" }, "signing.current_key_pem"},
		{"missing current chain", func(c *AS) { c.Signing.CurrentChainPEM = "" }, "signing.current_chain_pem"},
		{"next key without chain", func(c *AS) { c.Signing.NextKeyPEM = "next-key.pem" }, "signing.next_key_pem and signing.next_chain_pem"},
		{"next chain without key", func(c *AS) { c.Signing.NextChainPEM = "next-chain.pem" }, "signing.next_key_pem and signing.next_chain_pem"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.edit(&cfg)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tc.want)
			}
		})
	}

	cfg := base()
	cfg.Signing.NextKeyPEM = "next-key.pem"
	cfg.Signing.NextChainPEM = "next-chain.pem"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("paired next credential rejected: %v", err)
	}
}

func TestASConfigSigningAliasesAndLegacyIgnored(t *testing.T) {
	cfg, err := loadASTest(t, "dev", map[string]string{
		"AS_DEV":                            "1",
		"AS_AUTH_SIGNING_ENVIRONMENT":       "dev",
		"AS_AUTH_SIGNING_ROOT_PEM":          "/root.pem",
		"AS_AUTH_SIGNING_CURRENT_KEY_PEM":   "/current.key",
		"AS_AUTH_SIGNING_CURRENT_CHAIN_PEM": "/current.chain",
		"AS_AUTH_SIGNING_NEXT_KEY_PEM":      "/next.key",
		"AS_AUTH_SIGNING_NEXT_CHAIN_PEM":    "/next.chain",
		"AS_SESSION_KEY_PEM":                "/legacy.key",
		"AS_SESSION_KEY_NEXT_PEM":           "/legacy-next.pub",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Signing.Environment != "dev" || cfg.Signing.RootPEM != "/root.pem" ||
		cfg.Signing.CurrentKeyPEM != "/current.key" || cfg.Signing.CurrentChainPEM != "/current.chain" ||
		cfg.Signing.NextKeyPEM != "/next.key" || cfg.Signing.NextChainPEM != "/next.chain" {
		t.Fatalf("signing aliases not loaded: %+v", cfg.Signing)
	}
}

func TestASOnlineSigningConfigHasNoIssuerKeyOrRawVerifierBundle(t *testing.T) {
	for _, legacy := range []string{"AS_SESSION_KEY_PEM", "AS_SESSION_KEY_NEXT_PEM"} {
		if _, exists := asEnvAliases[legacy]; exists {
			t.Fatalf("legacy raw-key alias %s is still active", legacy)
		}
	}
	typeOfSigning := reflect.TypeOf(ASAuthSigning{})
	for i := 0; i < typeOfSigning.NumField(); i++ {
		name := strings.ToLower(typeOfSigning.Field(i).Name)
		if strings.Contains(name, "intermediate") || strings.Contains(name, "public") || strings.Contains(name, "bundle") {
			t.Fatalf("online signing config exposes forbidden field %q", typeOfSigning.Field(i).Name)
		}
	}
	for _, path := range []string{"../../config/authsvc.yaml", "../../config/authsvc.dev.yaml"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, forbidden := range []string{"session:\n", "key_next_pem", "signing_intermediate_key", "public_key_bundle"} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("%s contains forbidden online signer setting %q", path, forbidden)
			}
		}
	}
}

func TestLoadSigningCredentials(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	current := writeSigningFixture(t, "prod", now, "current")
	next := writeSigningFixtureForPKI(t, current.root, current.rootKey, current.intermediate, current.intermediateKey, "prod", now, "next")

	t.Run("current only", func(t *testing.T) {
		got, err := loadSigningCredentials(signingConfigFor(current), now)
		if err != nil {
			t.Fatal(err)
		}
		if got.Current == nil || got.Next != nil {
			t.Fatalf("credentials = %+v", got)
		}
	})

	t.Run("current and next", func(t *testing.T) {
		cfg := signingConfigFor(current)
		cfg.NextKeyPEM, cfg.NextChainPEM = next.keyPath, next.chainPath
		got, err := loadSigningCredentials(cfg, now)
		if err != nil || got.Current == nil || got.Next == nil {
			t.Fatalf("credentials = %+v, err = %v", got, err)
		}
	})

	t.Run("mismatched current key", func(t *testing.T) {
		cfg := signingConfigFor(current)
		cfg.CurrentKeyPEM = next.keyPath
		_, err := loadSigningCredentials(cfg, now)
		if err == nil || !strings.Contains(err.Error(), "current") || !strings.Contains(err.Error(), next.keyPath) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("reversed current chain", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "reversed.pem")
		writePEMCerts(t, path, current.intermediate, current.leaf)
		cfg := signingConfigFor(current)
		cfg.CurrentChainPEM = path
		_, err := loadSigningCredentials(cfg, now)
		if err == nil || !strings.Contains(err.Error(), "current") || !strings.Contains(err.Error(), path) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("chain containing root", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "root-in-chain.pem")
		writePEMCerts(t, path, current.leaf, current.intermediate, current.root)
		cfg := signingConfigFor(current)
		cfg.CurrentChainPEM = path
		_, err := loadSigningCredentials(cfg, now)
		if err == nil || !strings.Contains(err.Error(), "current") || !strings.Contains(err.Error(), path) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("next validates independently", func(t *testing.T) {
		wrongEnv := writeSigningFixtureForPKI(t, current.root, current.rootKey, current.intermediate, current.intermediateKey, "staging", now, "wrong-env")
		cfg := signingConfigFor(current)
		cfg.NextKeyPEM, cfg.NextChainPEM = wrongEnv.keyPath, wrongEnv.chainPath
		_, err := loadSigningCredentials(cfg, now)
		if err == nil || !strings.Contains(err.Error(), "next") || !strings.Contains(err.Error(), wrongEnv.chainPath) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("wrong root", func(t *testing.T) {
		other := writeSigningFixture(t, "prod", now, "other-root")
		cfg := signingConfigFor(current)
		cfg.RootPEM = other.rootPath
		_, err := loadSigningCredentials(cfg, now)
		if err == nil || !strings.Contains(err.Error(), "current") || !strings.Contains(err.Error(), current.chainPath) {
			t.Fatalf("error = %v", err)
		}
	})

	for _, tc := range []struct {
		name string
		at   time.Time
	}{
		{"expired", now.Add(100 * 24 * time.Hour)},
		{"not yet valid", now.Add(-2 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadSigningCredentials(signingConfigFor(current), tc.at)
			if err == nil || !strings.Contains(err.Error(), "current") || !strings.Contains(err.Error(), current.chainPath) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	t.Run("non-certificate block", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad-block.pem")
		raw, err := os.ReadFile(current.chainPath)
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("secret")})...)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := signingConfigFor(current)
		cfg.CurrentChainPEM = path
		_, err = loadSigningCredentials(cfg, now)
		if err == nil || !strings.Contains(err.Error(), "current") || !strings.Contains(err.Error(), path) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("junk prefix on root", func(t *testing.T) {
		path := writePrefixedTestFile(t, current.rootPath, []byte("junk before root\n"))
		cfg := signingConfigFor(current)
		cfg.RootPEM = path
		_, err := loadSigningCredentials(cfg, now)
		if err == nil || !strings.Contains(err.Error(), "signing.root_pem") || !strings.Contains(err.Error(), path) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("junk prefix on signer chain", func(t *testing.T) {
		path := writePrefixedTestFile(t, current.chainPath, []byte("junk before chain\n"))
		cfg := signingConfigFor(current)
		cfg.CurrentChainPEM = path
		_, err := loadSigningCredentials(cfg, now)
		if err == nil || !strings.Contains(err.Error(), "current") || !strings.Contains(err.Error(), path) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("whitespace prefix on root", func(t *testing.T) {
		path := writePrefixedTestFile(t, current.rootPath, []byte(" \n"))
		cfg := signingConfigFor(current)
		cfg.RootPEM = path
		if _, err := loadSigningCredentials(cfg, now); err == nil {
			t.Fatal("whitespace-prefixed root was accepted")
		}
	})

	t.Run("whitespace prefix on signer chain", func(t *testing.T) {
		path := writePrefixedTestFile(t, current.chainPath, []byte("\n"))
		cfg := signingConfigFor(current)
		cfg.CurrentChainPEM = path
		if _, err := loadSigningCredentials(cfg, now); err == nil {
			t.Fatal("whitespace-prefixed signer chain was accepted")
		}
	})

	t.Run("wrong intermediate purpose", func(t *testing.T) {
		wrong := writeWrongPurposeFixture(t, current, now)
		cfg := signingConfigFor(current)
		cfg.CurrentKeyPEM, cfg.CurrentChainPEM = wrong.keyPath, wrong.chainPath
		_, err := loadSigningCredentials(cfg, now)
		if err == nil || !strings.Contains(err.Error(), "current") || !strings.Contains(err.Error(), wrong.chainPath) {
			t.Fatalf("error = %v", err)
		}
	})
}

type signingFixture struct {
	root, intermediate, leaf     *x509.Certificate
	rootKey, intermediateKey     *ecdsa.PrivateKey
	keyPath, chainPath, rootPath string
}

func signingConfigFor(f signingFixture) ASAuthSigning {
	return ASAuthSigning{Environment: "prod", RootPEM: f.rootPath, CurrentKeyPEM: f.keyPath, CurrentChainPEM: f.chainPath}
}

func writeSigningFixture(t *testing.T, environment string, now time.Time, id string) signingFixture {
	t.Helper()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := issueCert(t, &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "root"}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(365 * 24 * time.Hour),
	}, nil, &rootKey.PublicKey, rootKey)
	interKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	inter := issueCert(t, &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "auth signing"}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		Policies:  []x509.OID{pki.AuthSigningIntermediatePolicyOID},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(180 * 24 * time.Hour),
	}, root, &interKey.PublicKey, rootKey)
	f := writeSigningFixtureForPKI(t, root, rootKey, inter, interKey, environment, now, id)
	f.rootPath = filepath.Join(t.TempDir(), "root.pem")
	writePEMCerts(t, f.rootPath, root)
	return f
}

func writeSigningFixtureForPKI(t *testing.T, root *x509.Certificate, rootKey *ecdsa.PrivateKey, intermediate *x509.Certificate, intermediateKey *ecdsa.PrivateKey, environment string, now time.Time, id string) signingFixture {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := url.Parse("spiffe://" + environment + ".spawnery.internal/signer/auth-artifact/" + id)
	if err != nil {
		t.Fatal(err)
	}
	leaf := issueCert(t, &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: id},
		KeyUsage: x509.KeyUsageDigitalSignature, Policies: []x509.OID{pki.AuthArtifactSignerPolicyOID},
		URIs: []*url.URL{uri}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(90 * 24 * time.Hour),
	}, intermediate, priv.Public(), intermediateKey)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, id+"-key.pem")
	keyPEM, err := token.MarshalSigningKeyPEM(priv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	chainPath := filepath.Join(dir, id+"-chain.pem")
	writePEMCerts(t, chainPath, leaf, intermediate)
	return signingFixture{root: root, rootKey: rootKey, intermediate: intermediate, intermediateKey: intermediateKey, leaf: leaf, keyPath: keyPath, chainPath: chainPath}
}

func writeWrongPurposeFixture(t *testing.T, base signingFixture, now time.Time) signingFixture {
	t.Helper()
	intermediateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	intermediate := issueCert(t, &x509.Certificate{
		SerialNumber: big.NewInt(99), Subject: pkix.Name{CommonName: "wrong purpose"}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(180 * 24 * time.Hour),
	}, base.root, &intermediateKey.PublicKey, base.rootKey)
	f := writeSigningFixtureForPKI(t, base.root, base.rootKey, intermediate, intermediateKey, "prod", now, "wrong-purpose")
	f.rootPath = base.rootPath
	return f
}

func issueCert(t *testing.T, tmpl, parent *x509.Certificate, publicKey any, signer any) *x509.Certificate {
	t.Helper()
	if parent == nil {
		parent = tmpl
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, publicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func writePEMCerts(t *testing.T, path string, certs ...*x509.Certificate) {
	t.Helper()
	var raw []byte
	for _, cert := range certs {
		raw = append(raw, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})...)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
func writePrefixedTestFile(t *testing.T, source string, prefix []byte) string {
	t.Helper()
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), filepath.Base(source))
	raw = append(append([]byte(nil), prefix...), raw...)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
