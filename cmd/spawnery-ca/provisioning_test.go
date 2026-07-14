package main

import (
	"os"
	"strings"
	"testing"
)

func TestProductionAndDefaultDevProvisionInternalMTLS(t *testing.T) {
	common := readRepoFile(t, "../../scripts/e2e-vm/provision/env/common.env")
	genPKI := readRepoFile(t, "../../scripts/e2e-vm/provision/gen-pki.sh")
	for _, required := range []string{
		"AS_AUTH_SIGNING_ROOT_PEM=/etc/spawnery/authsvc/root.pem",
		"AS_AUTH_SIGNING_CURRENT_CHAIN_PEM=/etc/spawnery/authsvc/auth-signer-current-chain.pem",
		"AS_INTERNAL_LISTEN=127.0.0.1:8091",
		"AS_INTERNAL_CERT=/etc/spawnery/authsvc/authsvc-service.pem",
		"AS_INTERNAL_CHAIN=/etc/spawnery/authsvc/authsvc-service-chain.pem",
		"AS_INTERNAL_SERVER_NAME=authsvc.internal",
		"AS_CP_URL=https://127.0.0.1:8081",
		"AS_CP_SERVER_NAME=cp.internal",
		"AS_INTERNAL_REVOCATION_STATE=/var/lib/spawnery/authsvc-revocations/state.json",
		"CP_AUTH_ROOT_CA=/etc/spawnery/cp/root.pem",
		"CP_AUTH_SIGNER_REVOCATION_STATE=/var/lib/spawnery/cp-signer-revocations/state.json",
		"CP_INTERNAL_LISTEN=127.0.0.1:8081",
		"CP_INTERNAL_TLS_CERT=/etc/spawnery/cp/cp-service.pem",
		"CP_INTERNAL_TLS_CHAIN=/etc/spawnery/cp/cp-service-chain.pem",
		"CP_INTERNAL_TLS_KEY=/etc/spawnery/cp/cp-service-key.pem",
		"CP_INTERNAL_REVOCATION_STATE=/var/lib/spawnery/cp-revocations/state.json",
		"NODE_CERTIFICATE_REVOCATION_STATE=/var/lib/spawnlet/certificate-revocations/state.json",
		"NODE_SIGNER_REVOCATION_STATE=/var/lib/spawnlet/signer-revocations/state.json",
		"AS_URL=https://127.0.0.1:8091",
		"AS_SERVER_NAME=authsvc.internal",
		"CP_SERVER_NAME=cp.internal",
	} {
		if !strings.Contains(common, required) {
			t.Errorf("common.env missing %q", required)
		}
	}
	justfile := readRepoFile(t, "../../Justfile")
	for _, required := range []string{
		"AS_AUTH_SIGNING_CURRENT_CHAIN_PEM={{devca}}/auth-signer-current-chain.pem",
		"AS_INTERNAL_LISTEN={{addr_as_internal}}",
		"CP_AUTH_ROOT_CA={{devca}}/root.pem",
		"CP_INTERNAL_LISTEN={{addr_cp_node}}",
		"AS_CP_URL=https://{{addr_cp_node}}",
		"AS_URL=https://{{addr_as_internal}}",
		"NODE_CERTIFICATE_REVOCATION_STATE=",
	} {
		if !strings.Contains(justfile, required) {
			t.Errorf("Justfile missing %q", required)
		}
	}
	for _, required := range []string{
		"cp-service.pem cp-service-chain.pem cp-service-key.pem",
		"authsvc-service.pem authsvc-service-chain.pem authsvc-service-key.pem",
		"service.crl.pem cloud-node.crl.pem self-hosted-node.crl.pem",
	} {
		if !strings.Contains(genPKI, required) {
			t.Errorf("gen-pki.sh missing %q", required)
		}
	}
	operatorSurfaces := []string{
		common,
		justfile,
		readRepoFile(t, "../../PROVISIONING.md"),
		readRepoFile(t, "../../deploy/authsvc/README.md"),
		readRepoFile(t, "../../deploy/cp/README.md"),
		readRepoFile(t, "../../scripts/e2e-vm/provision/RECONCILE-NOTES.md"),
	}
	for _, retired := range []string{
		"AS_CP_RPC_SECRET",
		"AS_DEV_RELAX_NODE_AUTH",
		"NODE_GITHUB_MINT_DEV_NODE_ID",
		"AS_SESSION_KEY_PEM",
		"CP_AS_SESSION_PUBKEYS",
		"NODE_AS_PUBKEYS",
		"CP_DEV_AS_KEY",
		"CP_NODE_LISTEN",
		"CP_NODE_ROOT_CA",
		"CP_NODE_TLS_CERT",
		"CP_NODE_TLS_CHAIN",
		"CP_NODE_TLS_KEY",
	} {
		for _, surface := range operatorSurfaces {
			if strings.Contains(surface, retired) {
				t.Errorf("retired internal authentication variable %s remains", retired)
			}
		}
	}
}

func TestPlainDevRecipesShareAuthorizationTrust(t *testing.T) {
	justfile := readRepoFile(t, "../../Justfile")
	for recipe, required := range map[string][]string{
		"cp": {
			"test -f {{devca}}/root.pem || just gen-dev-ca",
			"CP_AUTH_ENVIRONMENT=dev",
			"CP_AUTH_ROOT_CA={{devca}}/root.pem",
			"CP_AUTH_SIGNER_REVOCATION_STATE={{repo}}/.envs/dev/revocations/cp-plain-signers/state.json",
		},
		"authsvc": {
			"test -f {{devca}}/root.pem || just gen-dev-ca",
			"AS_AUTH_SIGNING_ENVIRONMENT=dev",
			"AS_AUTH_SIGNING_ROOT_PEM={{devca}}/root.pem",
			"AS_AUTH_SIGNING_CURRENT_KEY_PEM={{devca}}/auth-signer-current-key.pem",
			"AS_AUTH_SIGNING_CURRENT_CHAIN_PEM={{devca}}/auth-signer-current-chain.pem",
		},
		"node": {
			"test -f {{devca}}/root.pem || just gen-dev-ca",
			"NODE_ROOT_CA={{devca}}/root.pem",
			"NODE_AUTH_ENVIRONMENT=dev",
			"NODE_SIGNER_REVOCATION_STATE={{repo}}/.envs/dev/revocations/node-plain-signers/state.json",
		},
	} {
		body := justfileRecipeBody(t, justfile, recipe)
		for _, value := range required {
			if !strings.Contains(body, value) {
				t.Errorf("Justfile %s recipe missing %q", recipe, value)
			}
		}
	}
}

func TestProductionSystemdIsolatesPrivatePKI(t *testing.T) {
	provision := readRepoFile(t, "../../scripts/e2e-vm/provision/provision.sh")
	for _, expected := range []string{
		"sudo install -d -m0700 -o root -g root /var/lib/spawnery-offline",
		"SPAWNERY_OFFLINE_PKI_DIR=/var/lib/spawnery-offline",
	} {
		if !strings.Contains(provision, expected) {
			t.Errorf("production provisioning missing ceremony isolation %q", expected)
		}
	}
	for _, test := range []struct {
		unit         string
		inaccessible string
		readOnly     string
	}{
		{
			unit:         "spawnery-authsvc.service",
			inaccessible: "InaccessiblePaths=/var/lib/spawnery-offline /etc/spawnery/caddy /etc/spawnery/cp /etc/spawnery/node /etc/spawnery/pki",
			readOnly:     "ReadOnlyPaths=/etc/spawnery/authsvc",
		},
		{
			unit:         "spawnery-cp.service",
			inaccessible: "InaccessiblePaths=/var/lib/spawnery-offline /etc/spawnery/authsvc /etc/spawnery/caddy /etc/spawnery/node /etc/spawnery/pki",
			readOnly:     "ReadOnlyPaths=/etc/spawnery/cp",
		},
		{
			unit:         "spawnery-node.service",
			inaccessible: "InaccessiblePaths=/var/lib/spawnery-offline /etc/spawnery/authsvc /etc/spawnery/caddy /etc/spawnery/cp /etc/spawnery/pki",
			readOnly:     "ReadOnlyPaths=/etc/spawnery/node",
		},
	} {
		unit := systemdUnitBody(t, provision, test.unit)
		for _, expected := range []string{
			test.inaccessible,
			test.readOnly,
			"NoNewPrivileges=yes",
			"CapabilityBoundingSet=~CAP_SYS_ADMIN CAP_SYS_PTRACE",
		} {
			if !strings.Contains(unit, expected) {
				t.Errorf("%s missing %q", test.unit, expected)
			}
		}
	}

	common := readRepoFile(t, "../../scripts/e2e-vm/provision/env/common.env")
	for _, forbidden := range []string{
		"/var/lib/spawnery-offline",
		"root-key.pem",
		"service-intermediate-key.pem",
		"cloud-intermediate-key.pem",
		"auth-signing-intermediate-key.pem",
	} {
		if strings.Contains(common, forbidden) {
			t.Errorf("runtime environment references offline key material %q", forbidden)
		}
	}
}

func TestProductionNodeRuntimeBundleIsAllowListed(t *testing.T) {
	provision := readRepoFile(t, "../../scripts/e2e-vm/provision/provision.sh")
	common := readRepoFile(t, "../../scripts/e2e-vm/provision/env/common.env")
	for _, expected := range []string{
		"sudo install -d -m0700 /etc/spawnery/node",
		"/etc/spawnery/pki/node-cloud/{cert.pem,chain.pem,key.pem,root.pem}",
		"service-intermediate.pem,cloud-intermediate.pem,self-hosted-intermediate.pem,service.crl.pem,cloud-node.crl.pem,self-hosted-node.crl.pem",
		"sudo install -d -m0750 -o root -g caddy /etc/spawnery/caddy",
		"tls /etc/spawnery/caddy/wildcard.crt /etc/spawnery/caddy/wildcard.key",
		"sudo rm -rf /etc/spawnery/pki/*",
	} {
		if !strings.Contains(provision, expected) {
			t.Errorf("production provisioning missing node/caddy custody step %q", expected)
		}
	}
	for _, expected := range []string{
		"NODE_ID_DIR=/etc/spawnery/node",
		"NODE_ROOT_CA=/etc/spawnery/node/root.pem",
		"NODE_CERTIFICATE_REVOCATION_ISSUERS=/etc/spawnery/node/service-intermediate.pem,/etc/spawnery/node/cloud-intermediate.pem,/etc/spawnery/node/self-hosted-intermediate.pem",
		"NODE_CERTIFICATE_REVOCATION_CRLS=/etc/spawnery/node/service.crl.pem,/etc/spawnery/node/cloud-node.crl.pem,/etc/spawnery/node/self-hosted-node.crl.pem",
	} {
		if !strings.Contains(common, expected) {
			t.Errorf("common.env missing node-only runtime path %q", expected)
		}
	}
	for _, forbidden := range []string{
		"NODE_ID_DIR=/etc/spawnery/pki",
		"NODE_ROOT_CA=/etc/spawnery/pki",
		"NODE_CERTIFICATE_REVOCATION_ISSUERS=/etc/spawnery/pki",
		"NODE_CERTIFICATE_REVOCATION_CRLS=/etc/spawnery/pki",
	} {
		if strings.Contains(common, forbidden) {
			t.Errorf("node runtime still references shared staging: %q", forbidden)
		}
	}
	for _, line := range strings.Split(provision, "\n") {
		if !strings.Contains(line, "cp ") || !strings.Contains(line, "/etc/spawnery/node/") {
			continue
		}
		for _, forbidden := range []string{
			"authsvc-service",
			"cp-service",
			"self-hosted-intermediate-key",
			"auth-signer",
			"wildcard",
			"root-key",
			"service-intermediate-key",
			"cloud-intermediate-key",
			"/etc/spawnery/pki/. ",
			"/etc/spawnery/pki/node/",
		} {
			if strings.Contains(line, forbidden) {
				t.Errorf("node runtime copy exposes forbidden peer/ceremony material %q: %s", forbidden, line)
			}
		}
	}
}

func TestProductionCaddyProxiesPublicEnrollmentTokenIssuanceOnly(t *testing.T) {
	provision := readRepoFile(t, "../../scripts/e2e-vm/provision/provision.sh")
	var asMatcher string
	for _, line := range strings.Split(provision, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "@as ") {
			asMatcher = line
			break
		}
	}
	if asMatcher == "" {
		t.Fatal("Caddy AS matcher not found")
	}
	paths := map[string]bool{}
	for _, field := range strings.Fields(asMatcher)[2:] {
		paths[field] = true
		if forbiddenCaddyEnrollmentPath(field) {
			t.Errorf("Caddy AS matcher exposes internal enrollment path %s: %s", field, asMatcher)
		}
	}
	if !paths["/enrollment-tokens"] {
		t.Errorf("Caddy AS matcher does not proxy public /enrollment-tokens: %s", asMatcher)
	}
	asHandleStart := strings.Index(provision, "handle @as {")
	asHandleEnd := -1
	if asHandleStart >= 0 {
		asHandleEnd = strings.Index(provision[asHandleStart:], "}")
	}
	if asHandleStart < 0 || asHandleEnd < 0 ||
		!strings.Contains(provision[asHandleStart:asHandleStart+asHandleEnd], "reverse_proxy 127.0.0.1:8090") {
		t.Error("Caddy AS matcher is not proxied to the public authsvc listener")
	}
}

func TestProductionCaddyUsesProtocolSpecificControlPlaneTransports(t *testing.T) {
	for _, script := range []string{
		"../../scripts/e2e-vm/provision/provision.sh",
		"../../scripts/e2e-vm/roll.sh",
	} {
		content := strings.ReplaceAll(readRepoFile(t, script), "'\\''", " ")
		caddy := strings.Join(strings.Fields(content), " ")
		if strings.Contains(caddy, "@cp path /cp.v1.* /ws*") {
			t.Errorf("%s sends WebSocket upgrades through the h2c control-plane route", script)
		}
		for _, required := range []string{
			"@cp path /cp.v1.*",
			"@ws path /ws*",
			"handle @cp { reverse_proxy h2c://127.0.0.1:8080 }",
			"handle @ws { reverse_proxy 127.0.0.1:8080 }",
		} {
			if !strings.Contains(caddy, required) {
				t.Errorf("%s Caddy config missing %q", script, required)
			}
		}
	}
}

func TestForbiddenCaddyEnrollmentPath(t *testing.T) {
	for _, test := range []struct {
		path      string
		forbidden bool
	}{
		{path: "/enrollment-tokens"},
		{path: "/oauth*"},
		{path: "/enroll", forbidden: true},
		{path: "/enroll*", forbidden: true},
		{path: "/enroll/*", forbidden: true},
		{path: "/enroll/private", forbidden: true},
		{path: "/enrollment-tokens/private", forbidden: true},
	} {
		if got := forbiddenCaddyEnrollmentPath(test.path); got != test.forbidden {
			t.Errorf("forbiddenCaddyEnrollmentPath(%q) = %v, want %v", test.path, got, test.forbidden)
		}
	}
}

func forbiddenCaddyEnrollmentPath(path string) bool {
	return strings.HasPrefix(path, "/enroll") && path != "/enrollment-tokens"
}

func systemdUnitBody(t *testing.T, provision, unit string) string {
	t.Helper()
	marker := "sudo tee /etc/systemd/system/" + unit
	start := strings.Index(provision, marker)
	if start < 0 {
		t.Fatalf("systemd unit %s not found", unit)
	}
	bodyStart := strings.Index(provision[start:], "\n")
	if bodyStart < 0 {
		t.Fatalf("systemd unit %s body not found", unit)
	}
	body := provision[start+bodyStart+1:]
	end := strings.Index(body, "\nEOF")
	if end < 0 {
		t.Fatalf("systemd unit %s terminator not found", unit)
	}
	return body[:end]
}

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func justfileRecipeBody(t *testing.T, justfile, recipe string) string {
	t.Helper()
	lines := strings.Split(justfile, "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, recipe+":") || strings.HasPrefix(line, recipe+" ") {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("Justfile recipe %s not found", recipe)
	}
	var body []string
	for _, line := range lines[start:] {
		if line != "" && line[0] != ' ' && line[0] != '\t' {
			break
		}
		body = append(body, line)
	}
	return strings.Join(body, "\n")
}
