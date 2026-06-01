# E5 Slice 4 — Catalog Enrichment + Listing Moderation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Surface the stored manifest in `GetApp` detail, and let a creator take down / relist their app.

**Architecture:** Pure-CP, no schema change. Contracts (`GetAppResponse.manifest` + `SetAppListing` RPC) → store (`SetListed`) → CP handlers (`GetApp` parses the manifest blob; new `SetAppListing` creator-guarded).

**Tech Stack:** Go, ConnectRPC, buf, bun, protojson.

**Source spec:** `docs/superpowers/specs/2026-06-01-e5-catalog-enrich-moderation-slice4.md`

**Conventions:** commit `--no-verify`; codegen `export PATH="$PATH:$(go env GOPATH)/bin" && make gen`; bead `sp-pending` (set at claim); branch `sp-7hl-slice4` off master.

---

## Task 1: Contracts

**Files:** Modify `proto/cp/v1/cp.proto`; regenerated `gen/cp/v1/*`.

- [ ] **Step 1:** Add the RPC to `service SpawnService` (after `RegisterAppVersion`):
```proto
  rpc SetAppListing(SetAppListingRequest) returns (SetAppListingResponse);
```

- [ ] **Step 2:** In `message GetAppResponse { ... }` add a field `AppManifest manifest = 3;` (keep `app = 1`, `versions = 2`). Append the new messages at the end of the file:
```proto
message SetAppListingRequest  { string app_id = 1; bool listed = 2; }
message SetAppListingResponse {}
```
(`AppManifest` already exists from slice 2 — reuse it.)

- [ ] **Step 3:** `export PATH="$PATH:$(go env GOPATH)/bin" && make gen`

- [ ] **Step 4:** Verify: `go build ./... && grep -c "func (x \*GetAppResponse) GetManifest\|SetAppListingRequest" gen/cp/v1/cp.pb.go` — builds clean, symbols present.

- [ ] **Step 5:** Commit:
```bash
git add proto/cp/v1/cp.proto gen/cp/v1
git commit --no-verify -m "feat(cp): GetAppResponse.manifest + SetAppListing contracts (sp-7hl slice4)"
```

---

## Task 2: Store `SetListed` + CP `SetAppListing` handler

**Files:** Modify `internal/cp/store/store.go` (interface), `internal/cp/store/apps.go`; create `internal/cp/moderation.go`, `internal/cp/moderation_test.go`; add a store test to `internal/cp/store/registration_test.go` (or a new file).

- [ ] **Step 1: Failing store test** — append to `internal/cp/store/registration_test.go`:
```go
func TestSetListed(t *testing.T) {
	st := NewTestStore(t)
	ctx := context.Background()
	if err := st.Apps().Upsert(ctx, App{ID: "c/a", DisplayName: "A", Visibility: "public", Listed: true, CreatorID: "alice", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.Apps().SetListed(ctx, "c/a", false); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Apps().Get(ctx, "c/a")
	if got.Listed {
		t.Fatal("expected unlisted")
	}
	if err := st.Apps().SetListed(ctx, "missing", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound for missing app, got %v", err)
	}
}
```
(`errors` is already imported in that file.)

- [ ] **Step 2: Confirm failure:** `go test ./internal/cp/store/ -run TestSetListed 2>&1 | head` (no `SetListed`).

- [ ] **Step 3: Add `SetListed`** to `AppRepo` interface (`store.go`): `SetListed(ctx context.Context, appID string, listed bool) error`. Implement in `apps.go`:
```go
func (r *appRepo) SetListed(ctx context.Context, appID string, listed bool) error {
	res, err := r.db.NewUpdate().Model((*App)(nil)).Set("listed = ?", listed).Where("id = ?", appID).Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
```

- [ ] **Step 4: Run:** `go test ./internal/cp/store/ -run TestSetListed` — PASS.

- [ ] **Step 5: Failing handler test** — create `internal/cp/moderation_test.go`:
```go
package cp

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
)

// register an app owned by alice, return its id.
func registerApp(t *testing.T, s *Server, owner, id string) {
	t.Helper()
	ctx := auth.WithOwner(context.Background(), owner)
	_, err := s.RegisterAppVersion(ctx, connect.NewRequest(&cpv1.RegisterAppVersionRequest{
		Manifest: &cpv1.AppManifest{ApiVersion: "spawnery/v1", Id: id, Title: "T", Visibility: "open",
			Mounts: []*cpv1.ManifestMount{{Name: "main", Path: "data", Seed: "seed"}}},
		Version: "1.0.0", Ref: id + "@sha",
	}))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
}

func TestSetAppListingTakedownRelist(t *testing.T) {
	s, _, _ := newTestServer(t)
	alice := auth.WithOwner(context.Background(), "alice")
	registerApp(t, s, "alice", "alice/app")

	// takedown
	if _, err := s.SetAppListing(alice, connect.NewRequest(&cpv1.SetAppListingRequest{AppId: "alice/app", Listed: false})); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetApp(alice, connect.NewRequest(&cpv1.GetAppRequest{Id: "alice/app"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unlisted GetApp want NotFound, got %v", err)
	}
	// relist
	if _, err := s.SetAppListing(alice, connect.NewRequest(&cpv1.SetAppListingRequest{AppId: "alice/app", Listed: true})); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetApp(alice, connect.NewRequest(&cpv1.GetAppRequest{Id: "alice/app"})); err != nil {
		t.Fatalf("relisted GetApp should succeed: %v", err)
	}
}

func TestSetAppListingGuards(t *testing.T) {
	s, _, _ := newTestServer(t)
	registerApp(t, s, "alice", "alice/app")
	mallory := auth.WithOwner(context.Background(), "mallory")
	if _, err := s.SetAppListing(mallory, connect.NewRequest(&cpv1.SetAppListingRequest{AppId: "alice/app", Listed: false})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("non-creator want PermissionDenied, got %v", err)
	}
	alice := auth.WithOwner(context.Background(), "alice")
	if _, err := s.SetAppListing(alice, connect.NewRequest(&cpv1.SetAppListingRequest{AppId: "nope", Listed: false})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("missing app want NotFound, got %v", err)
	}
	if _, err := s.SetAppListing(context.Background(), connect.NewRequest(&cpv1.SetAppListingRequest{AppId: "alice/app", Listed: false})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("unauth want Unauthenticated, got %v", err)
	}
}
```

- [ ] **Step 6: Confirm failure:** `go test ./internal/cp/ -run TestSetAppListing 2>&1 | head` (no `SetAppListing` on `*Server`).

- [ ] **Step 7: Implement `internal/cp/moderation.go`:**
```go
package cp

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
)

// SetAppListing takes down (listed=false) or relists (listed=true) an app. Creator-only.
func (s *Server) SetAppListing(ctx context.Context, req *connect.Request[cpv1.SetAppListingRequest]) (*connect.Response[cpv1.SetAppListingResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	creator, err := s.st.Apps().Creator(ctx, req.Msg.AppId)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if creator != owner {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not the creator of %q", req.Msg.AppId))
	}
	if err := s.st.Apps().SetListed(ctx, req.Msg.AppId, req.Msg.Listed); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&cpv1.SetAppListingResponse{}), nil
}
```

- [ ] **Step 8: Run:** `go test ./internal/cp/ -run TestSetAppListing` — PASS.

- [ ] **Step 9: Commit:**
```bash
git add internal/cp/store internal/cp/moderation.go internal/cp/moderation_test.go
git commit --no-verify -m "feat(cp): SetAppListing takedown/relist (creator-guarded) + store SetListed (sp-7hl slice4)"
```

---

## Task 3: `GetApp` manifest enrichment

**Files:** Modify `internal/cp/catalog.go`; create `internal/cp/catalog_enrich_test.go`.

- [ ] **Step 1: Failing test** — create `internal/cp/catalog_enrich_test.go`:
```go
package cp

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
)

func TestGetAppReturnsManifest(t *testing.T) {
	s, _, _ := newTestServer(t)
	registerApp(t, s, "alice", "alice/app") // helper from moderation_test.go (same package)
	resp, err := s.GetApp(auth.WithOwner(context.Background(), "alice"), connect.NewRequest(&cpv1.GetAppRequest{Id: "alice/app"}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.Manifest == nil {
		t.Fatal("expected a manifest for a registered app")
	}
	if resp.Msg.Manifest.Id != "alice/app" || resp.Msg.Manifest.Title != "T" {
		t.Fatalf("manifest = %+v", resp.Msg.Manifest)
	}
	if len(resp.Msg.Manifest.Mounts) != 1 || resp.Msg.Manifest.Mounts[0].Name != "main" {
		t.Fatalf("manifest mounts = %+v", resp.Msg.Manifest.Mounts)
	}
}

func TestGetAppSeedAppNilManifest(t *testing.T) {
	s, _, _ := newTestServer(t)
	// secret-app is seeded directly (no stored manifest blob) -> nil manifest, summary still ok.
	resp, err := s.GetApp(auth.WithOwner(context.Background(), "alice"), connect.NewRequest(&cpv1.GetAppRequest{Id: "secret-app"}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.Manifest != nil {
		t.Fatalf("seed app should have nil manifest, got %+v", resp.Msg.Manifest)
	}
	if resp.Msg.App.Id != "secret-app" {
		t.Fatalf("summary missing: %+v", resp.Msg.App)
	}
}
```
> `registerApp` is defined in `moderation_test.go` (Task 2) — same package `cp`, so it's shared. If Task 2 isn't present yet when writing, the test won't compile; Tasks run in order, so it's available.

- [ ] **Step 2: Confirm failure:** `go test ./internal/cp/ -run TestGetAppReturnsManifest 2>&1 | head` (`resp.Msg.Manifest` is always nil — handler doesn't set it).

- [ ] **Step 3: Enrich `GetApp`** in `internal/cp/catalog.go`. Add `"google.golang.org/protobuf/encoding/protojson"` and `"log"` to imports. Before the final `return`, parse the latest version's manifest:
```go
	resp := &cpv1.GetAppResponse{App: summary, Versions: vout}
	if len(versions) > 0 && versions[0].Manifest != "" {
		var m cpv1.AppManifest
		if err := protojson.Unmarshal([]byte(versions[0].Manifest), &m); err != nil {
			log.Printf("GetApp %s: manifest parse: %v", req.Msg.Id, err) // non-fatal
		} else {
			resp.Manifest = &m
		}
	}
	return connect.NewResponse(resp), nil
```
(Replace the existing `return connect.NewResponse(&cpv1.GetAppResponse{App: summary, Versions: vout}), nil`.)

- [ ] **Step 4: Run:** `go test ./internal/cp/ -run TestGetApp` — PASS (enrichment + the slice-1 `TestGetApp`).

- [ ] **Step 5: Full package + race:** `go test ./internal/cp/ -race` — PASS.

- [ ] **Step 6: Commit:**
```bash
git add internal/cp/catalog.go internal/cp/catalog_enrich_test.go
git commit --no-verify -m "feat(cp): GetApp returns the latest version's manifest (sp-7hl slice4)"
```

---

## Final Verification
- [ ] `export PATH="$PATH:$(go env GOPATH)/bin" && make gen` — no diff.
- [ ] `go build ./... && go build -tags e2e ./... && go vet ./...` — clean.
- [ ] `go test ./...` — pass; `go test ./internal/cp/ -race` — race-clean.

Then **superpowers:finishing-a-development-branch** (Option 1: merge to master locally).

---

## Self-Review Notes
- **Spec coverage:** §2 contracts → T1; §3 store `SetListed` → T2; §4 `SetAppListing` handler → T2, `GetApp` enrichment → T3; §5 testing → T2/T3. Out-of-scope (admin moderation, flag flow, per-version manifest, ListApps manifest) absent. ✓
- **Types:** `GetAppResponse.Manifest` (`*cpv1.AppManifest`), `SetAppListingRequest{AppId,Listed}`, `store.SetListed`, `Creator` guard reused. ✓
- **Cross-task dep:** `registerApp` helper defined in `moderation_test.go` (T2), used by T3's test — same package, T2 precedes T3. ✓
- **protojson round-trip:** slice 2 stored the blob via `protojson.Marshal`; T3 reads it back via `protojson.Unmarshal` — symmetric. ✓
