# E5 Slice 5 — Creator "My Apps" View Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** A creator can list their own apps (including taken-down ones) to manage/relist them.

**Architecture:** Pure-CP, no schema change. Contracts (`AppSummary.listed` + `ListMyApps`) → store (`ListByCreator`) → CP handler + populate `listed` in existing mappers.

**Source spec:** `docs/superpowers/specs/2026-06-01-e5-my-apps-slice5.md`

**Conventions:** commit `--no-verify`; codegen `export PATH="$PATH:$(go env GOPATH)/bin" && make gen`; bead `sp-pending`; branch `sp-7hl-slice5` off master.

---

## Task 1: Contracts

**Files:** Modify `proto/cp/v1/cp.proto`; regenerated `gen/cp/v1/*`.

- [ ] **Step 1:** In `message AppSummary { ... }` (fields 1–6: id, display_name, summary, tags, latest_version, latest_tier), add:
```proto
  bool listed = 7;
```
- [ ] **Step 2:** Add to `service SpawnService` (after `SetAppListing`):
```proto
  rpc ListMyApps(ListMyAppsRequest) returns (ListMyAppsResponse);
```
Append at end of file:
```proto
message ListMyAppsRequest  {}
message ListMyAppsResponse { repeated AppSummary apps = 1; }
```
- [ ] **Step 3:** `export PATH="$PATH:$(go env GOPATH)/bin" && make gen`
- [ ] **Step 4:** Verify: `go build ./... && grep -c "func (x \*AppSummary) GetListed\|ListMyAppsRequest" gen/cp/v1/cp.pb.go` — builds clean, symbols present.
- [ ] **Step 5:** Commit:
```bash
git add proto/cp/v1/cp.proto gen/cp/v1
git commit --no-verify -m "feat(cp): AppSummary.listed + ListMyApps contracts (sp-7hl slice5)"
```

---

## Task 2: Store `ListByCreator` + `ListMyApps` handler + populate `listed`

**Files:** Modify `internal/cp/store/store.go`, `internal/cp/store/apps.go`, `internal/cp/catalog.go`; create `internal/cp/my_apps_test.go`.

- [ ] **Step 1: Failing handler test** — create `internal/cp/my_apps_test.go`:
```go
package cp

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
)

func TestListMyApps(t *testing.T) {
	s, _, _ := newTestServer(t)
	alice := auth.WithOwner(context.Background(), "alice")
	registerApp(t, s, "alice", "alice/one") // helper from moderation_test.go (same package)
	registerApp(t, s, "alice", "alice/two")
	registerApp(t, s, "bob", "bob/app")
	// take down alice/two
	if _, err := s.SetAppListing(alice, connect.NewRequest(&cpv1.SetAppListingRequest{AppId: "alice/two", Listed: false})); err != nil {
		t.Fatal(err)
	}

	resp, err := s.ListMyApps(alice, connect.NewRequest(&cpv1.ListMyAppsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{} // id -> listed
	for _, a := range resp.Msg.Apps {
		got[a.Id] = a.Listed
	}
	if len(got) != 2 {
		t.Fatalf("want alice's 2 apps, got %d (%v)", len(got), got)
	}
	if _, ok := got["bob/app"]; ok {
		t.Fatal("bob's app leaked into alice's ListMyApps")
	}
	if v, ok := got["alice/one"]; !ok || !v {
		t.Fatalf("alice/one should be listed: %v", got)
	}
	if v, ok := got["alice/two"]; !ok || v {
		t.Fatalf("alice/two should be present and unlisted: %v", got)
	}
}

func TestListMyAppsUnauthenticated(t *testing.T) {
	s, _, _ := newTestServer(t)
	if _, err := s.ListMyApps(context.Background(), connect.NewRequest(&cpv1.ListMyAppsRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}
}

func TestListAppsCarriesListed(t *testing.T) {
	s, _, _ := newTestServer(t)
	registerApp(t, s, "alice", "alice/pub")
	resp, err := s.ListApps(auth.WithOwner(context.Background(), "alice"), connect.NewRequest(&cpv1.ListAppsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range resp.Msg.Apps {
		if !a.Listed {
			t.Fatalf("public ListApps result %q should be listed=true", a.Id)
		}
	}
}
```
(`registerApp` is the helper from `moderation_test.go`, same package.)

- [ ] **Step 2: Confirm failure:** `go test ./internal/cp/ -run 'TestListMyApps|TestListAppsCarriesListed' 2>&1 | head` (no `ListMyApps`, `Listed` always false).

- [ ] **Step 3: Add `ListByCreator`** to `AppRepo` interface (`store.go`):
```go
	ListByCreator(ctx context.Context, creatorID string) ([]CatalogEntry, error)
```
Implement in `apps.go` (mirror `Catalog`, creator-scoped, no listed/public filter):
```go
func (r *appRepo) ListByCreator(ctx context.Context, creatorID string) ([]CatalogEntry, error) {
	var apps []App
	if err := r.db.NewSelect().Model(&apps).
		Where("creator_id = ?", creatorID).Order("display_name ASC").Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]CatalogEntry, 0, len(apps))
	for _, a := range apps {
		var v AppVersion
		err := r.db.NewSelect().Model(&v).
			Where("app_id = ?", a.ID).Order("created_at DESC").Limit(1).Scan(ctx)
		e := CatalogEntry{App: a}
		if err == nil {
			e.LatestVersion, e.LatestTier = v.Version, v.Tier
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}
```

- [ ] **Step 4: Add the `ListMyApps` handler + populate `Listed`** in `internal/cp/catalog.go`.
  Add a small mapper to DRY the summary construction, OR inline. Implementer's choice; the simplest is a helper:
```go
func catalogEntryToSummary(e store.CatalogEntry) *cpv1.AppSummary {
	return &cpv1.AppSummary{
		Id: e.App.ID, DisplayName: e.App.DisplayName, Summary: e.App.Summary,
		Tags: splitTags(e.App.Tags), LatestVersion: e.LatestVersion, LatestTier: tierToProto(e.LatestTier),
		Listed: e.App.Listed,
	}
}
```
  - Refactor `ListApps` to build its `out` entries via `catalogEntryToSummary(e)` (this sets `Listed` there too — always true for public results).
  - In `GetApp`, set `Listed: app.Listed` on the `summary` it builds (the detail path doesn't use a `CatalogEntry`; just add the field to the existing `&cpv1.AppSummary{...}` literal).
  - Add the handler:
```go
// ListMyApps returns the authenticated owner's apps (including unlisted/taken-down) for management.
func (s *Server) ListMyApps(ctx context.Context, _ *connect.Request[cpv1.ListMyAppsRequest]) (*connect.Response[cpv1.ListMyAppsResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	entries, err := s.st.Apps().ListByCreator(ctx, owner)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*cpv1.AppSummary, len(entries))
	for i, e := range entries {
		out[i] = catalogEntryToSummary(e)
	}
	return connect.NewResponse(&cpv1.ListMyAppsResponse{Apps: out}), nil
}
```

- [ ] **Step 5: Run:** `go test ./internal/cp/ -run 'TestListMyApps|TestListAppsCarriesListed|TestListApps|TestGetApp'` — PASS (new + slice-1 catalog tests still green after the mapper refactor).

- [ ] **Step 6: Full package + race + build:** `go test ./internal/cp/ -race && go build ./...` — PASS/clean.

- [ ] **Step 7: Commit:**
```bash
git add internal/cp/store internal/cp/catalog.go internal/cp/my_apps_test.go
git commit --no-verify -m "feat(cp): ListMyApps creator view + AppSummary.listed (sp-7hl slice5)"
```

---

## Final Verification
- [ ] `export PATH="$PATH:$(go env GOPATH)/bin" && make gen` — no diff.
- [ ] `go build ./... && go build -tags e2e ./... && go vet ./...` — clean.
- [ ] `go test ./...` — pass; `go test ./internal/cp/ -race` — race-clean.

Then **superpowers:finishing-a-development-branch** (Option 1: merge to master locally).

---

## Self-Review Notes
- **Spec coverage:** §2 contracts → T1; §3 store → T2 (S3); §4 handler + mappers → T2 (S4); §5 testing → T2. Out-of-scope (pagination, metadata edit, ownership transfer) absent. ✓
- **Types:** `AppSummary.Listed`, `ListMyApps{Request,Response}`, `store.ListByCreator`, `catalogEntryToSummary` consistent. ✓
- **Refactor safety:** `catalogEntryToSummary` reuses `splitTags`/`tierToProto` (existing in catalog.go); `ListApps` behavior unchanged except `Listed` now populated (slice-1 `TestGetApp`/`TestListApps*` still pass — they don't assert on `Listed`). ✓
- **Dep:** `registerApp` helper from `moderation_test.go` (slice 4, same package). ✓
