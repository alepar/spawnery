package main

import (
	"crypto/tls"
	"strings"
	"testing"
)

// TestASConfig_TLSDefaultsEmpty pins the default (byte-for-byte plain HTTP) contract: with none of
// AS_TLS_CERT/AS_TLS_KEY/AS_CLIENT_CA set, the fields decode empty and config load succeeds.
func TestASConfig_TLSDefaultsEmpty(t *testing.T) {
	cfg, err := loadASTest(t, "dev", map[string]string{"AS_DEV": "1"})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.TLS.Cert != "" || cfg.TLS.Key != "" || cfg.TLS.ClientCA != "" {
		t.Errorf("expected empty TLS defaults, got cert=%q key=%q client_ca=%q", cfg.TLS.Cert, cfg.TLS.Key, cfg.TLS.ClientCA)
	}
}

func TestASConfig_TLSEnvAliases(t *testing.T) {
	cfg, err := loadASTest(t, "dev", map[string]string{
		"AS_DEV":       "1",
		"AS_TLS_CERT":  "/etc/spawnery/as/tls-cert.pem",
		"AS_TLS_KEY":   "/etc/spawnery/as/tls-key.pem",
		"AS_CLIENT_CA": "/etc/spawnery/as/client-ca.pem",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.TLS.Cert != "/etc/spawnery/as/tls-cert.pem" {
		t.Errorf("TLS.Cert = %q", cfg.TLS.Cert)
	}
	if cfg.TLS.Key != "/etc/spawnery/as/tls-key.pem" {
		t.Errorf("TLS.Key = %q", cfg.TLS.Key)
	}
	if cfg.TLS.ClientCA != "/etc/spawnery/as/client-ca.pem" {
		t.Errorf("TLS.ClientCA = %q", cfg.TLS.ClientCA)
	}
}

// TestASConfig_TLSExactlyOneOfCertKeySet_FatalAtLoad is constraint 4 (bd sp-hsqs): a
// half-configured TLS listener that quietly serves HTTP is the single worst outcome for an auth
// service, so setting exactly one of AS_TLS_CERT/AS_TLS_KEY must fail config.Load — a fatal
// startup error, not a silent downgrade to plaintext.
func TestASConfig_TLSExactlyOneOfCertKeySet_FatalAtLoad(t *testing.T) {
	t.Run("cert only", func(t *testing.T) {
		_, err := loadASTest(t, "dev", map[string]string{"AS_DEV": "1", "AS_TLS_CERT": "/tmp/cert.pem"})
		if err == nil {
			t.Fatal("expected a fatal config error with only AS_TLS_CERT set")
		}
		if !strings.Contains(err.Error(), "tls.cert") && !strings.Contains(err.Error(), "tls.key") {
			t.Errorf("error %q does not mention tls.cert/tls.key", err.Error())
		}
	})
	t.Run("key only", func(t *testing.T) {
		_, err := loadASTest(t, "dev", map[string]string{"AS_DEV": "1", "AS_TLS_KEY": "/tmp/key.pem"})
		if err == nil {
			t.Fatal("expected a fatal config error with only AS_TLS_KEY set")
		}
	})
	t.Run("both set: no error from the pairing check", func(t *testing.T) {
		// Both set is a valid pairing (whether the files actually exist/parse is checked later, at
		// buildTLSConfig time — config.Validate is deliberately hermetic, no filesystem access).
		_, err := loadASTest(t, "dev", map[string]string{"AS_DEV": "1", "AS_TLS_CERT": "/tmp/cert.pem", "AS_TLS_KEY": "/tmp/key.pem"})
		if err != nil {
			t.Fatalf("both cert+key set should pass the pairing check: %v", err)
		}
	})
}

// TestBuildTLSConfig_Unset_ReturnsNil pins buildTLSConfig's contract for main()'s "plain HTTP,
// byte-for-byte" default: nil, nil when tls.cert/tls.key are both empty.
func TestBuildTLSConfig_Unset_ReturnsNil(t *testing.T) {
	cfg := &AS{}
	tlsConfig, err := buildTLSConfig(cfg)
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if tlsConfig != nil {
		t.Fatalf("expected nil TLS config with cert+key unset, got %+v", tlsConfig)
	}
}

// TestBuildTLSConfig_ExactlyOneSet_Errors is buildTLSConfig's own defense-in-depth copy of the
// exactly-one-set check (AS.Validate enforces it on the config.Load path; this pins the function
// itself for callers that construct an *AS directly, e.g. tests).
func TestBuildTLSConfig_ExactlyOneSet_Errors(t *testing.T) {
	cfg := &AS{}
	cfg.TLS.Cert = "/tmp/cert.pem"
	if _, err := buildTLSConfig(cfg); err == nil {
		t.Fatal("expected an error with only cert set")
	}
}

// TestBuildTLSConfig_ClientAuthIsVerifyIfGiven_NeverRequire pins the security-critical mode
// (constraint 1, bd sp-hsqs): AS serves browsers/CLIs with no client cert on the SAME mux as nodes,
// so ClientAuth must be VerifyClientCertIfGiven — RequireAndVerifyClientCert would reject every
// human caller.
func TestBuildTLSConfig_ClientAuthIsVerifyIfGiven_NeverRequire(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSelfSignedForTest(t, dir)
	cfg := &AS{}
	cfg.TLS.Cert = certPath
	cfg.TLS.Key = keyPath
	// tls.client_ca intentionally left unset: buildTLSConfig must warn, not fail (constraint 4's
	// counterpart — TLS-without-node-clients is a legitimate deployment).
	tlsConfig, err := buildTLSConfig(cfg)
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if tlsConfig.ClientAuth != tls.VerifyClientCertIfGiven {
		t.Fatalf("ClientAuth = %v, want VerifyClientCertIfGiven", tlsConfig.ClientAuth)
	}
	if tlsConfig.ClientAuth == tls.RequireAndVerifyClientCert {
		t.Fatal("ClientAuth must never be RequireAndVerifyClientCert — it would reject every browser/CLI caller")
	}
}
