//go:build !e2e_fixtures

package agentcaps

import "testing"

// In a default (untagged, RELEASE) build the stub TEST FIXTURE must NOT be registered.
// It is only compiled in under the e2e_fixtures tag — see agentcaps_e2e.go and the
// tagged TestStubAcpRunnableRegistered in agentcaps_e2e_test.go.
func TestStubNotRegisteredInDefaultBuild(t *testing.T) {
	if _, ok := Runnables("stub"); ok {
		t.Fatalf("stub fixture must not be registered without the e2e_fixtures build tag")
	}
	if _, ok := FindRunnable("stub-acp"); ok {
		t.Fatalf("stub-acp must not be findable without the e2e_fixtures build tag")
	}
}
