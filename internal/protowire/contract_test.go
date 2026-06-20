package protowire

import (
	"testing"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	sidecarv1 "spawnery/gen/sidecar/v1"
)

func TestMintResponseCarriesLoginAndUserID(t *testing.T) {
	in := &authv1.MintGitHubAccessTokenResponse{Login: "octocat", UserId: 583231}
	b, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out authv1.MintGitHubAccessTokenResponse
	if err := proto.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.GetLogin() != "octocat" || out.GetUserId() != 583231 {
		t.Fatalf("round-trip lost login/id: login=%q id=%d", out.GetLogin(), out.GetUserId())
	}
}

func TestGetTokenControlMessagesRoundTrip(t *testing.T) {
	req := &sidecarv1.GetTokenRequest{SpawnId: "s1", MinRemainingSeconds: 300}
	resp := &sidecarv1.GetTokenResponse{Token: "ghu_x", AccessExpiresAtUnix: 1234567890}
	rb, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("marshal req: %v", err)
	}
	var rout sidecarv1.GetTokenRequest
	if err := proto.Unmarshal(rb, &rout); err != nil {
		t.Fatalf("unmarshal req: %v", err)
	}
	if rout.GetSpawnId() != "s1" || rout.GetMinRemainingSeconds() != 300 {
		t.Fatalf("req round-trip: %+v", &rout)
	}
	pb, err := proto.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal resp: %v", err)
	}
	var pout sidecarv1.GetTokenResponse
	if err := proto.Unmarshal(pb, &pout); err != nil {
		t.Fatalf("unmarshal resp: %v", err)
	}
	if pout.GetToken() != "ghu_x" || pout.GetAccessExpiresAtUnix() != 1234567890 {
		t.Fatalf("resp round-trip: %+v", &pout)
	}
}

func TestSpawnCADeliveryRoundTrip(t *testing.T) {
	ca := &sidecarv1.SpawnCADelivery{CaCertPem: []byte("CERT"), CaKeyPem: []byte("KEY")}
	b, err := proto.Marshal(ca)
	if err != nil {
		t.Fatalf("marshal ca: %v", err)
	}
	var out sidecarv1.SpawnCADelivery
	if err := proto.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal ca: %v", err)
	}
	if string(out.GetCaCertPem()) != "CERT" || string(out.GetCaKeyPem()) != "KEY" {
		t.Fatalf("ca round-trip: %+v", &out)
	}
	// GetSpawnCARequest exists and round-trips.
	r := &sidecarv1.GetSpawnCARequest{SpawnId: "s1"}
	rb, err := proto.Marshal(r)
	if err != nil {
		t.Fatalf("marshal ca-req: %v", err)
	}
	var rout sidecarv1.GetSpawnCARequest
	if err := proto.Unmarshal(rb, &rout); err != nil {
		t.Fatalf("unmarshal ca-req: %v", err)
	}
	if rout.GetSpawnId() != "s1" {
		t.Fatalf("ca-req round-trip: %+v", &rout)
	}
}
