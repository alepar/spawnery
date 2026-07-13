package authsvc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc/githubfake"
	"spawnery/internal/authsvc/store"
	"spawnery/internal/authsvc/token"
)

func TestRevocationsFeedUsesBoundedSignedProtobufPages(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	pki := newTestArtifactPKI(t, now, "prod")
	signer := pki.signer(t, now, "revocation-pages")
	srv, _, st := testAS(t, fake, now, func(cfg *IdPConfig) { cfg.Signer = signer })
	verifier, err := token.NewVerifier(pki.root, "prod", nil)
	if err != nil {
		t.Fatal(err)
	}
	var firstSeq int64
	for n := range 257 {
		seq, err := st.Revocations().Append(context.Background(), store.RevocationEvent{
			AccountID: "acct", FamilyID: fmt.Sprintf("family-%03d", n), RevokedAt: now.Unix(),
			RevokedTokens: []store.RevokedToken{{TokenID: fmt.Sprintf("token-%03d", n), RetainUntil: now.Add(time.Hour).Unix()}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			firstSeq = seq
		}
	}

	resp, err := http.Get(srv.URL + "/revocations?since=0")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var page RevocationPage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || len(page.Entries) != 256 || !page.HasMore {
		t.Fatalf("default page: status=%d entries=%d has_more=%v", resp.StatusCode, len(page.Entries), page.HasMore)
	}
	entry := page.Entries[0]
	if entry.Seq != firstSeq || entry.Sig == "" {
		t.Fatalf("outer entry: %+v", entry)
	}
	payload, err := verifier.Verify(entry.Sig, token.ArtifactTypeRevocation, now)
	if err != nil {
		t.Fatal(err)
	}
	var body authv1.RevocationEntry
	if err := proto.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	if body.Seq != entry.Seq || body.AccountId != "acct" || body.FamilyId != "family-000" || len(body.RevokedTokens) != 1 || body.RevokedTokens[0].TokenId != "token-000" {
		t.Fatalf("verified body: %+v", &body)
	}
	deterministic, err := (proto.MarshalOptions{Deterministic: true}).Marshal(&body)
	if err != nil || string(deterministic) != string(payload) {
		t.Fatalf("payload is not deterministic protobuf: err=%v", err)
	}

	resp, err = http.Get(fmt.Sprintf("%s/revocations?since=%d&limit=1", srv.URL, page.Entries[255].Seq))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	page = RevocationPage{}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1 || page.HasMore {
		t.Fatalf("terminal explicit page: %+v", page)
	}
}

func TestRevocationsFeedBoundsSerializedPagesAndDrainsBacklog(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	pki := newTestArtifactPKI(t, now, "prod")
	signer := pki.signer(t, now, "revocation-page-bytes")
	srv, _, st := testAS(t, fake, now, func(cfg *IdPConfig) { cfg.Signer = signer })

	const eventCount = 80
	largeFamilyPrefix := strings.Repeat("f", 48<<10)
	for n := range eventCount {
		if _, err := st.Revocations().Append(context.Background(), store.RevocationEvent{
			AccountID: "acct", FamilyID: largeFamilyPrefix + fmt.Sprintf("-%03d", n), RevokedAt: now.Unix(),
			RevokedTokens: []store.RevokedToken{{TokenID: fmt.Sprintf("token-%03d", n), RetainUntil: now.Add(time.Hour).Unix()}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	checkpoint := int64(0)
	seen := 0
	for {
		resp, err := http.Get(fmt.Sprintf("%s/revocations?since=%d", srv.URL, checkpoint))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK || len(raw) > maxRevocationPageBytes {
			t.Fatalf("bounded response: status=%d bytes=%d", resp.StatusCode, len(raw))
		}
		var page RevocationPage
		if err := json.Unmarshal(raw, &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Entries) == 0 {
			t.Fatalf("empty page while draining: %+v", page)
		}
		seen += len(page.Entries)
		checkpoint = page.Entries[len(page.Entries)-1].Seq
		if !page.HasMore {
			break
		}
	}
	if seen != eventCount {
		t.Fatalf("drained events: want %d, got %d", eventCount, seen)
	}
}

func TestRevocationsFeedServesMaximumCardinalityAndContinues(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	pki := newTestArtifactPKI(t, now, "prod")
	signer := pki.signer(t, now, "revocation-cardinality")
	srv, _, st := testAS(t, fake, now, func(cfg *IdPConfig) { cfg.Signer = signer })

	tokens := make([]store.RevokedToken, 1024)
	for n := range tokens {
		tokens[n] = store.RevokedToken{TokenID: fmt.Sprintf("token-%04d", n), RetainUntil: now.Add(time.Hour).Unix()}
	}
	if _, err := st.Revocations().Append(context.Background(), store.RevocationEvent{
		AccountID: "acct", FamilyID: "maximum", RevokedAt: now.Unix(), RevokedTokens: tokens,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Revocations().Append(context.Background(), store.RevocationEvent{
		AccountID: "acct", FamilyID: "over", RevokedAt: now.Unix(),
		RevokedTokens: append(tokens, store.RevokedToken{TokenID: "over", RetainUntil: now.Add(time.Hour).Unix()}),
	}); err == nil {
		t.Fatal("appended revocation above maximum cardinality")
	}
	if _, err := st.Revocations().Append(context.Background(), store.RevocationEvent{
		AccountID: "acct", FamilyID: "after", RevokedAt: now.Unix(),
		RevokedTokens: []store.RevokedToken{{TokenID: "after", RetainUntil: now.Add(time.Hour).Unix()}},
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(srv.URL + "/revocations?since=0")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var page RevocationPage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || page.HasMore || len(page.Entries) != 2 {
		t.Fatalf("progressable feed: status=%d page=%+v", resp.StatusCode, page)
	}
}

func TestRevocationsFeedRejectsInvalidPageParameters(t *testing.T) {
	fake := githubfake.New()
	defer fake.Close()
	now := time.Unix(1770000000, 0)
	srv, _, _ := testAS(t, fake, now)
	for _, query := range []string{
		"since=-1", "since=wat", "since=0&limit=0", "since=0&limit=-1", "since=0&limit=257", "since=0&limit=wat",
	} {
		t.Run(query, func(t *testing.T) {
			resp, err := http.Get(srv.URL + "/revocations?" + query)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status: want 400, got %d", resp.StatusCode)
			}
		})
	}

	resp, err := http.Get(srv.URL + "/revocations?since=999&limit=256")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var page RevocationPage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if page.Entries == nil || len(page.Entries) != 0 || page.HasMore {
		t.Fatalf("empty page: %+v", page)
	}
}
