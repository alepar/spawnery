//go:build spike_vectors

// Spike helper (build-tagged out of normal builds): emit Go proto.Marshal hex for two IntentBody cases so the TS
// protobuf-es spike can assert byte-parity. The no-mounts case must match the committed
// testdata/intent_vectors.json; the mounts case is the new coverage (repeated MountRef).
package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"

	authv1 "spawnery/gen/auth/v1"
	"google.golang.org/protobuf/proto"
)

func main() {
	noMounts := &authv1.IntentBody{
		Jti:          "fixed-jti-for-vectors",
		IssuedAt:     1770000000,
		SpawnId:      "sp-vec-001",
		Generation:   1,
		TargetNodeId: "node-vec-1",
		Op:           "create-spawn",
		AppRef:       "app/test@sha256:deadbeef",
		Image:        "registry/img@sha256:cafebabe",
		Model:        "claude-test",
	}
	withMounts := &authv1.IntentBody{
		Jti:        "mounts-vec",
		IssuedAt:   1770000001,
		SpawnId:    "sp-vec-002",
		Generation: 7,
		Op:         "create-spawn",
		AppRef:     "app/m@sha256:1",
		Mounts: []*authv1.MountRef{
			{Name: "scratch", BackendUri: "scratch://s", CreateIfMissing: true},
			{Name: "gh", BackendUri: "gh://o/r", CredentialSecretId: "sec-1", RepositoryId: "12345"},
		},
	}
	out := map[string]string{}
	for k, b := range map[string]*authv1.IntentBody{"noMounts": noMounts, "withMounts": withMounts} {
		by, err := proto.Marshal(b)
		if err != nil {
			panic(err)
		}
		out[k] = hex.EncodeToString(by)
	}
	j, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(j))
}
