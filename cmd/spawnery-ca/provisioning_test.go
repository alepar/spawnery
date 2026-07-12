package main

import (
	"os"
	"strings"
	"testing"
)

func TestProvisioningDeclaresGeneratedInternalMTLSTopology(t *testing.T) {
	common := readRepoFile(t, "../../scripts/e2e-vm/provision/env/common.env")
	justfile := readRepoFile(t, "../../Justfile")
	genPKI := readRepoFile(t, "../../scripts/e2e-vm/provision/gen-pki.sh")
	for _, required := range []string{
		"CP_AUTH_ROOT_CA=/etc/spawnery/cp/root.pem",
		"CP_INTERNAL_LISTEN=127.0.0.1:8081",
		"CP_INTERNAL_TLS_CERT=/etc/spawnery/cp/cp-service.pem",
		"CP_INTERNAL_TLS_CHAIN=/etc/spawnery/cp/cp-service-chain.pem",
		"CP_INTERNAL_REVOCATION_CRLS=/etc/spawnery/cp/service.crl.pem",
		"CP_AS_URL=https://127.0.0.1:8091",
	} {
		if !strings.Contains(common, required) {
			t.Errorf("common.env missing %q", required)
		}
	}
	for _, required := range []string{
		"AS_INTERNAL_LISTEN={{addr_as_internal}}",
		"AS_INTERNAL_CERT={{devca}}/authsvc-service.pem",
		"AS_AUTH_SIGNING_CURRENT_CHAIN_PEM={{devca}}/auth-signer-current-chain.pem",
		"AS_CP_URL=https://{{addr_cp_node}} AS_CP_SERVER_NAME=cp.internal",
		"CP_INTERNAL_TLS_CERT={{devca}}/cp-service.pem",
		"CP_AS_URL=https://{{addr_as_internal}}",
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
	for _, retired := range []string{"AS_CP_RPC_SECRET", "AS_DEV_RELAX_NODE_AUTH", "CP_AS_RPC_SECRET", "CP_NODE_LISTEN", "CP_NODE_TLS_CERT", "CP_NODE_TLS_KEY"} {
		if strings.Contains(common, retired) || strings.Contains(justfile, retired) {
			t.Errorf("retired internal authentication variable %s remains", retired)
		}
	}
}

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
