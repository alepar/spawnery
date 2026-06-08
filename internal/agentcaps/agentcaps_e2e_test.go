//go:build e2e_fixtures

package agentcaps

import "testing"

// Under the e2e_fixtures build tag the stub TEST FIXTURE is registered (see agentcaps_e2e.go).
func TestStubAcpRunnableRegistered(t *testing.T) {
	rs, ok := Runnables("stub")
	if !ok || len(rs) != 1 {
		t.Fatalf("Runnables(\"stub\") = %v, %v; want one runnable", rs, ok)
	}
	if rs[0].ID != "stub-acp" || rs[0].Mode != ModeACP || rs[0].Relay != RelayPump {
		t.Fatalf("stub runnable = %+v; want id=stub-acp mode=acp relay=pump", rs[0])
	}
	if r, ok := FindRunnable("stub-acp"); !ok || r.Mode != ModeACP {
		t.Fatalf("FindRunnable(stub-acp) = %+v ok=%v", r, ok)
	}
}
