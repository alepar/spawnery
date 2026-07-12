package pki

import (
	"net/url"
	"reflect"
	"testing"
)

func TestParsePrincipal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		id   string
		want Principal
	}{
		{
			name: "control plane service",
			id:   "spiffe://prod.spawnery.internal/service/cp/cp-a",
			want: Principal{TrustDomain: "prod.spawnery.internal", Kind: "service", Role: "cp", InstanceID: "cp-a"},
		},
		{
			name: "auth service",
			id:   "spiffe://prod.spawnery.internal/service/authsvc/as-a",
			want: Principal{TrustDomain: "prod.spawnery.internal", Kind: "service", Role: "authsvc", InstanceID: "as-a"},
		},
		{
			name: "cloud node",
			id:   "spiffe://prod.spawnery.internal/node/cloud/spawnery-system/n1",
			want: Principal{TrustDomain: "prod.spawnery.internal", Kind: "node", Role: "cloud", AccountID: "spawnery-system", NodeID: "n1"},
		},
		{
			name: "self-hosted node",
			id:   "spiffe://prod.spawnery.internal/node/self-hosted/acct-1/n2",
			want: Principal{TrustDomain: "prod.spawnery.internal", Kind: "node", Role: "self-hosted", AccountID: "acct-1", NodeID: "n2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := url.Parse(tt.id)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ParsePrincipal(id, "prod.spawnery.internal")
			if err != nil {
				t.Fatalf("ParsePrincipal: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("principal = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParsePrincipalRejectsNonCanonicalIDs(t *testing.T) {
	t.Parallel()

	tests := []string{
		"spiffe://prod.spawnery.internal/service/cp/",
		"spiffe://prod.spawnery.internal/service/cp/cp%2Da",
		"spiffe://prod.spawnery.internal/service/cp/cp-a?x=1",
		"spiffe://prod.spawnery.internal/service/cp/cp-a#frag",
		"spiffe://user@prod.spawnery.internal/service/cp/cp-a",
		"spiffe://other.spawnery.internal/service/cp/cp-a",
		"spiffe://prod.spawnery.internal/service/cp/cp-a/extra",
		"spiffe://prod.spawnery.internal/service/other/x",
		"spiffe://prod.spawnery.internal/node/cloud/acct/node/extra",
		"spiffe://prod.spawnery.internal/node/self-hosted/acct/node:bad",
		"https://prod.spawnery.internal/service/cp/cp-a",
	}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			id, err := url.Parse(raw)
			if err != nil {
				return
			}
			if _, err := ParsePrincipal(id, "prod.spawnery.internal"); err == nil {
				t.Fatalf("ParsePrincipal(%q) succeeded", raw)
			}
		})
	}
}

func TestPrincipalIDConstructors(t *testing.T) {
	t.Parallel()

	service, err := ServiceID("prod.spawnery.internal", "cp", "cp-a")
	if err != nil {
		t.Fatalf("ServiceID: %v", err)
	}
	if got, want := service.String(), "spiffe://prod.spawnery.internal/service/cp/cp-a"; got != want {
		t.Fatalf("ServiceID = %q, want %q", got, want)
	}
	node, err := NodeID("prod.spawnery.internal", "self-hosted", "acct-1", "n2")
	if err != nil {
		t.Fatalf("NodeID: %v", err)
	}
	if got, want := node.String(), "spiffe://prod.spawnery.internal/node/self-hosted/acct-1/n2"; got != want {
		t.Fatalf("NodeID = %q, want %q", got, want)
	}
	if _, err := ServiceID("prod.spawnery.internal", "other", "x"); err == nil {
		t.Fatal("ServiceID accepted unknown role")
	}
	if _, err := NodeID("prod.spawnery.internal", "cloud", "", "n"); err == nil {
		t.Fatal("NodeID accepted an empty account")
	}
}
