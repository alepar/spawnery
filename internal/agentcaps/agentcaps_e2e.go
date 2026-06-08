//go:build e2e_fixtures

package agentcaps

// This file registers the deterministic, credential-free TEST FIXTURE binary
// (cmd/stubagent) used by the multi-session e2e (sp-npxq.5). It is compiled ONLY
// under the e2e_fixtures build tag, so RELEASE binaries (built without the tag)
// never expose the stub. stub-acp serves canonical ACP (via acpmux) and echoes
// prompts ("ECHO: <text>") with no model/sidecar — so the live e2e runs with no
// keys. NOT a real agent.
func init() {
	registry["stub"] = []Runnable{
		{ID: "stub-acp", Mode: ModeACP, Launch: []string{"stubagent"}, Relay: RelayPump, Label: "Stub · echo"},
	}
}
