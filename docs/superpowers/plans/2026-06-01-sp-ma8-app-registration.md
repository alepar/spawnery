# E5 Slice 2 — App-Version Registration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let creators register an app version via a structured `RegisterAppVersion` API (the source of truth), persisting it at trust tier `unverified`, with a `spawnctl register` reference client mapping `spawneryapp.yml` → the API.

**Architecture:** Pure CP, no network/fetch. Contracts (`AppManifest` proto + RPC) → store (registration columns + migration `0003`) → validation (pure fn) → CP handler (`registration.go`) → reference client (`spawnctl register`). Same TDD/review rhythm as the merged slice 1 (`sp-0sc`).

**Tech Stack:** Go, ConnectRPC, buf, uptrace/bun (sqlite/pg), goose, yaml.v3.

**Source spec:** `docs/superpowers/specs/2026-06-01-e5-app-registration-slice2.md`

**Conventions:** commit `--no-verify`; codegen tools on PATH via `export PATH="$PATH:$(go env GOPATH)/bin"` (install pinned versions if missing, never stub around); bead `sp-ma8`; branch `git checkout -b sp-ma8-app-registration` off master.

> **Placement note (deviation from spec §4):** the manifest *validator* lives in package `cp` (`internal/cp/validate.go`), operating on `*cpv1.AppManifest`, so the leaf `internal/manifest` package stays YAML-only and proto-free. `internal/manifest` is only extended for the `spawnctl` YAML→proto mapping.

---

## File Structure

| File | Responsibility | Action |
|------|----------------|--------|
| `proto/cp/v1/cp.proto` | `AppManifest` + sub-messages + `RegisterAppVersion` RPC | Modify |
| `gen/cp/v1/*` | Generated (via `make gen`) | Regenerated |
| `internal/cp/store/types.go` | `App.CreatorID`, `AppVersion.Manifest`, `MountDecl.Path/Seed` | Modify |
| `internal/cp/store/migrations/{sqlite,pg}/0003_registration.sql` | schema | Create |
| `internal/cp/store/apps.go` | Upsert/UpsertVersion new cols; `Creator()` | Modify |
| `internal/cp/store/store.go` | `AppRepo.Creator` | Modify |
| `internal/cp/store/drift_test.go` | new column-set assertions | Modify |
| `internal/cp/store/registration_test.go` | store round-trip tests | Create |
| `internal/cp/seed.go` | set `CreatorID: "spawnery"` | Modify |
| `internal/cp/validate.go` | `validateManifest(*cpv1.AppManifest, version, ref) error` | Create |
| `internal/cp/validate_test.go` | table test | Create |
| `internal/cp/registration.go` | `RegisterAppVersion` handler | Create |
| `internal/cp/registration_test.go` | handler tests | Create |
| `internal/manifest/manifest.go` | full E0 §3 schema | Modify |
| `internal/manifest/manifest_test.go` | full-parse test | Modify |
| `cmd/spawnctl/main.go` | `register` mode + YAML→proto | Modify |
| `cmd/spawnctl/register_test.go` | YAML→proto mapping test | Create |

---

## Task 1: Contracts — `AppManifest` + `RegisterAppVersion`

**Files:** Modify `proto/cp/v1/cp.proto`; regenerated `gen/cp/v1/*`.

- [ ] **Step 1: Add the RPC** to `service SpawnService`, after the `GetApp` line:
```proto
  rpc RegisterAppVersion(RegisterAppVersionRequest) returns (RegisterAppVersionResponse);
```

- [ ] **Step 2: Append the messages** at the end of `proto/cp/v1/cp.proto`:
```proto
// AppManifest is the structured app-version manifest — the registration source of truth
// (mirrors spawneryapp.yml, E0 §3). CI maps YAML -> this and calls RegisterAppVersion.
message AppManifest {
  string api_version          = 1;  // "spawnery/v1"
  string id                   = 2;  // "creator/app"
  string title                = 3;
  string description          = 4;
  repeated string tags        = 5;
  string visibility           = 6;  // "open" | "private"
  ManifestAgents agents       = 7;
  repeated string tools       = 8;
  string persona              = 9;
  repeated string skills      = 10;
  ManifestModel model         = 11;
  string runtime_base_version = 12;
  repeated ManifestMount mounts = 13;
}
message ManifestAgents {
  repeated string support      = 1;
  repeated string exclude      = 2;
  repeated string requires_acp = 3;
}
message ManifestModel {
  bool tool_use              = 1;
  int64 min_context_tokens   = 2;
  bool vision                = 3;
  string recommended_default = 4;
}
message ManifestMount { string name = 1; string path = 2; string seed = 3; }

message RegisterAppVersionRequest {
  AppManifest manifest = 1;
  string version       = 2;
  string ref           = 3;
}
message RegisterAppVersionResponse {
  string app_id  = 1;
  string version = 2;
  TrustTier tier = 3;
}
```

- [ ] **Step 3:** `export PATH="$PATH:$(go env GOPATH)/bin" && make gen` (install missing tools, don't work around).

- [ ] **Step 4: Verify:** `go build ./... && grep -c "RegisterAppVersion\|AppManifest\|ManifestMount" gen/cp/v1/cp.pb.go` — builds clean, symbols present.

- [ ] **Step 5: Commit:**
```bash
git add proto/cp/v1/cp.proto gen/cp/v1
git commit --no-verify -m "feat(cp): AppManifest + RegisterAppVersion contracts (sp-ma8)"
```
(Pure-codegen task; verification is the build + grep, no behavioral test.)

---

## Task 2: Store model + migration `0003`

**Files:** Modify `types.go`, `apps.go`, `store.go`, `drift_test.go`, `seed.go`; create `migrations/{sqlite,pg}/0003_registration.sql`, `registration_test.go`.

> Consistency-sensitive (migration + `drift_test.go` asserts exact column sets). Use a capable model.

- [ ] **Step 1: Failing store test** — create `internal/cp/store/registration_test.go`:
```go
package store

import (
	"context"
	"errors"
	"testing"
)

func TestCreatorStickyAndManifestRoundTrip(t *testing.T) {
	st := NewTestStore(t)
	ctx := context.Background()
	if _, err := st.Apps().Creator(ctx, "creator/app"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Creator on missing app = %v (want ErrNotFound)", err)
	}
	if err := st.Apps().Upsert(ctx, App{ID: "creator/app", DisplayName: "App", Visibility: "public", Listed: true, CreatorID: "alice", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	// re-upsert with a different CreatorID must NOT change the sticky creator.
	if err := st.Apps().Upsert(ctx, App{ID: "creator/app", DisplayName: "App2", Visibility: "public", Listed: true, CreatorID: "mallory", CreatedAt: 2}); err != nil {
		t.Fatal(err)
	}
	creator, err := st.Apps().Creator(ctx, "creator/app")
	if err != nil || creator != "alice" {
		t.Fatalf("creator = %q err=%v (want alice, sticky)", creator, err)
	}
	if err := st.Apps().UpsertVersion(ctx,
		AppVersion{AppID: "creator/app", Version: "1.0.0", Ref: "creator/app@sha", Tier: TierUnverified, Manifest: `{"id":"creator/app"}`, CreatedAt: 3},
		[]MountDecl{{AppID: "creator/app", Version: "1.0.0", Name: "main", Path: "data", Seed: "seed", Required: true}}); err != nil {
		t.Fatal(err)
	}
	v, err := st.Apps().GetVersion(ctx, "creator/app", "1.0.0")
	if err != nil || v.Manifest != `{"id":"creator/app"}` || v.Tier != TierUnverified {
		t.Fatalf("version = %+v err=%v", v, err)
	}
	mounts, err := st.Apps().DeclaredMounts(ctx, "creator/app", "1.0.0")
	if err != nil || len(mounts) != 1 || mounts[0].Path != "data" || mounts[0].Seed != "seed" {
		t.Fatalf("mounts = %+v err=%v", mounts, err)
	}
}
```

- [ ] **Step 2: Confirm compile failure:** `go test ./internal/cp/store/ -run TestCreatorSticky 2>&1 | head` (no `CreatorID`/`Manifest`/`Path`/`Seed`/`Creator`).

- [ ] **Step 3: Update `types.go`** — add the fields:
```go
// in App:
	CreatorID     string `bun:"creator_id,notnull"`
// in AppVersion:
	Manifest      string `bun:"manifest,notnull"`
// in MountDecl:
	Path          string `bun:"path,notnull"`
	Seed          string `bun:"seed,notnull"`
```
(Place `CreatorID` after `Listed`; `Manifest` after `Tier`; `Path`/`Seed` after `Name` in `MountDecl`.)

- [ ] **Step 4: Create `internal/cp/store/migrations/sqlite/0003_registration.sql`:**
```sql
-- +goose Up
ALTER TABLE apps               ADD COLUMN creator_id TEXT NOT NULL DEFAULT '';
ALTER TABLE app_versions       ADD COLUMN manifest   TEXT NOT NULL DEFAULT '';
ALTER TABLE app_version_mounts ADD COLUMN path       TEXT NOT NULL DEFAULT '';
ALTER TABLE app_version_mounts ADD COLUMN seed       TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE app_version_mounts DROP COLUMN seed;
ALTER TABLE app_version_mounts DROP COLUMN path;
ALTER TABLE app_versions       DROP COLUMN manifest;
ALTER TABLE apps               DROP COLUMN creator_id;
```

- [ ] **Step 5: Create `internal/cp/store/migrations/pg/0003_registration.sql`** (identical SQL; pg accepts the same `ALTER TABLE ... ADD/DROP COLUMN`):
```sql
-- +goose Up
ALTER TABLE apps               ADD COLUMN creator_id TEXT NOT NULL DEFAULT '';
ALTER TABLE app_versions       ADD COLUMN manifest   TEXT NOT NULL DEFAULT '';
ALTER TABLE app_version_mounts ADD COLUMN path       TEXT NOT NULL DEFAULT '';
ALTER TABLE app_version_mounts ADD COLUMN seed       TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE app_version_mounts DROP COLUMN seed;
ALTER TABLE app_version_mounts DROP COLUMN path;
ALTER TABLE app_versions       DROP COLUMN manifest;
ALTER TABLE apps               DROP COLUMN creator_id;
```

- [ ] **Step 6: Update `apps.go`:**
  - `Upsert`: keep existing `.Set(...)` for display_name/summary/tags/visibility/listed; **do NOT** add `creator_id` to the conflict-update set (sticky — only the INSERT carries it). The `NewInsert().Model(&a)` already inserts `creator_id` from the struct on first insert; on conflict the `DO UPDATE` set list omits it, so it's preserved.
  - `UpsertVersion`: add `.Set("manifest = EXCLUDED.manifest")` to the version conflict-update; in the mounts loop add `.Set("path = EXCLUDED.path").Set("seed = EXCLUDED.seed")`.
  - Add:
```go
func (r *appRepo) Creator(ctx context.Context, appID string) (string, error) {
	var a App
	err := r.db.NewSelect().Model(&a).Column("creator_id").Where("id = ?", appID).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return a.CreatorID, err
}
```

- [ ] **Step 7: Add to the `AppRepo` interface in `store.go`:**
```go
	Creator(ctx context.Context, appID string) (string, error)
```

- [ ] **Step 8: Update `drift_test.go`** — extend the `check(...)` column lists:
```go
	check("apps", "id", "display_name", "summary", "tags", "visibility", "listed", "created_at", "creator_id")
	check("app_versions", "app_id", "version", "ref", "tier", "created_at", "manifest")
	check("app_version_mounts", "app_id", "version", "name", "required", "path", "seed")
```
(Order in `check` is order-insensitive if the helper sorts; if it's positional, match the actual `PRAGMA table_info` order — new columns appear last in the order they were added. Read the helper and match its comparison; the added columns are `creator_id`, `manifest`, then `path`,`seed`.)

- [ ] **Step 9: Update `seed.go`** — in the App `Upsert`, add `CreatorID: "spawnery"`.

- [ ] **Step 10: Build + tests:** `go build ./... && go test ./internal/cp/store/` — PASS (incl. drift + the new round-trip test).

- [ ] **Step 11: Commit:**
```bash
git add internal/cp/store internal/cp/seed.go
git commit --no-verify -m "feat(store): registration columns (creator_id, manifest, mount path/seed) + Creator (sp-ma8)"
```

---

## Task 3: Manifest validation (pure)

**Files:** Create `internal/cp/validate.go`, `internal/cp/validate_test.go`.

- [ ] **Step 1: Failing table test** — create `internal/cp/validate_test.go`:
```go
package cp

import (
	"testing"

	cpv1 "spawnery/gen/cp/v1"
)

func validManifest() *cpv1.AppManifest {
	return &cpv1.AppManifest{
		ApiVersion: "spawnery/v1", Id: "alice/wiki", Title: "Wiki", Visibility: "open",
		Mounts: []*cpv1.ManifestMount{{Name: "main", Path: "data", Seed: "seed"}},
	}
}

func TestValidateManifest(t *testing.T) {
	if err := validateManifest(validManifest(), "1.0.0", "alice/wiki@sha"); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	cases := []struct {
		name    string
		mutate  func(*cpv1.AppManifest)
		version string
		ref     string
	}{
		{"bad apiVersion", func(m *cpv1.AppManifest) { m.ApiVersion = "spawnery/v2" }, "1.0.0", "r"},
		{"empty id", func(m *cpv1.AppManifest) { m.Id = "" }, "1.0.0", "r"},
		{"id no slash", func(m *cpv1.AppManifest) { m.Id = "wiki" }, "1.0.0", "r"},
		{"id two slashes", func(m *cpv1.AppManifest) { m.Id = "a/b/c" }, "1.0.0", "r"},
		{"empty title", func(m *cpv1.AppManifest) { m.Title = "" }, "1.0.0", "r"},
		{"bad semver", func(m *cpv1.AppManifest) {}, "v1", "r"},
		{"empty ref", func(m *cpv1.AppManifest) {}, "1.0.0", ""},
		{"private", func(m *cpv1.AppManifest) { m.Visibility = "private" }, "1.0.0", "r"},
		{"dup mount", func(m *cpv1.AppManifest) {
			m.Mounts = append(m.Mounts, &cpv1.ManifestMount{Name: "main", Path: "x"})
		}, "1.0.0", "r"},
		{"empty mount path", func(m *cpv1.AppManifest) { m.Mounts[0].Path = "" }, "1.0.0", "r"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := validManifest()
			c.mutate(m)
			if err := validateManifest(m, c.version, c.ref); err == nil {
				t.Fatalf("%s: expected error, got nil", c.name)
			}
		})
	}
	// storage-less app is valid.
	m := validManifest()
	m.Mounts = nil
	if err := validateManifest(m, "1.0.0", "r"); err != nil {
		t.Fatalf("storage-less app rejected: %v", err)
	}
}
```

- [ ] **Step 2: Confirm failure:** `go test ./internal/cp/ -run TestValidateManifest 2>&1 | head` (no `validateManifest`).

- [ ] **Step 3: Implement `internal/cp/validate.go`:**
```go
package cp

import (
	"fmt"
	"regexp"
	"strings"

	cpv1 "spawnery/gen/cp/v1"
)

var (
	idSideRe = regexp.MustCompile(`^[a-z0-9._-]+$`)
	semverRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)
)

// validateManifest runs the structural checks (E5 §5) on a submitted app-version manifest.
// Pure function; returns a single descriptive error or nil.
func validateManifest(m *cpv1.AppManifest, version, ref string) error {
	if m == nil {
		return fmt.Errorf("manifest is required")
	}
	if m.ApiVersion != "spawnery/v1" {
		return fmt.Errorf("apiVersion must be \"spawnery/v1\", got %q", m.ApiVersion)
	}
	parts := strings.Split(m.Id, "/")
	if len(parts) != 2 || !idSideRe.MatchString(parts[0]) || !idSideRe.MatchString(parts[1]) {
		return fmt.Errorf("id must be \"creator/app\" (lowercase [a-z0-9._-]), got %q", m.Id)
	}
	if strings.TrimSpace(m.Title) == "" {
		return fmt.Errorf("title is required")
	}
	if !semverRe.MatchString(version) {
		return fmt.Errorf("version must be semver MAJOR.MINOR.PATCH, got %q", version)
	}
	if strings.TrimSpace(ref) == "" {
		return fmt.Errorf("ref is required")
	}
	switch m.Visibility {
	case "open":
	case "private":
		return fmt.Errorf("private apps are post-MVP; visibility must be \"open\"")
	default:
		return fmt.Errorf("visibility must be \"open\", got %q", m.Visibility)
	}
	seen := map[string]bool{}
	for _, mt := range m.Mounts {
		if strings.TrimSpace(mt.Name) == "" {
			return fmt.Errorf("mount name is required")
		}
		if seen[mt.Name] {
			return fmt.Errorf("duplicate mount name %q", mt.Name)
		}
		seen[mt.Name] = true
		if strings.TrimSpace(mt.Path) == "" {
			return fmt.Errorf("mount %q: path is required", mt.Name)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run:** `go test ./internal/cp/ -run TestValidateManifest` — PASS.

- [ ] **Step 5: Commit:**
```bash
git add internal/cp/validate.go internal/cp/validate_test.go
git commit --no-verify -m "feat(cp): manifest structural validation (sp-ma8)"
```

---

## Task 4: CP handler — `RegisterAppVersion`

**Files:** Create `internal/cp/registration.go`, `internal/cp/registration_test.go`.

- [ ] **Step 1: Failing handler test** — create `internal/cp/registration_test.go`:
```go
package cp

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
)

func regReq() *cpv1.RegisterAppVersionRequest {
	return &cpv1.RegisterAppVersionRequest{
		Manifest: &cpv1.AppManifest{
			ApiVersion: "spawnery/v1", Id: "alice/wiki", Title: "Alice Wiki",
			Description: "notes", Tags: []string{"notes"}, Visibility: "open",
			Mounts: []*cpv1.ManifestMount{{Name: "main", Path: "data", Seed: "seed"}},
		},
		Version: "1.0.0", Ref: "alice/wiki@sha1",
	}
}

func TestRegisterAppVersionNewApp(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.RegisterAppVersion(ctx, connect.NewRequest(regReq()))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.AppId != "alice/wiki" || resp.Msg.Tier != cpv1.TrustTier_TRUST_TIER_UNVERIFIED {
		t.Fatalf("resp = %+v", resp.Msg)
	}
	// now visible in the catalog (unverified).
	got, err := s.GetApp(ctx, connect.NewRequest(&cpv1.GetAppRequest{Id: "alice/wiki"}))
	if err != nil || got.Msg.App.DisplayName != "Alice Wiki" || got.Msg.Versions[0].Tier != cpv1.TrustTier_TRUST_TIER_UNVERIFIED {
		t.Fatalf("getapp = %+v err=%v", got.Msg, err)
	}
}

func TestRegisterAppVersionCreatorGuard(t *testing.T) {
	s, _, _ := newTestServer(t)
	alice := auth.WithOwner(context.Background(), "alice")
	if _, err := s.RegisterAppVersion(alice, connect.NewRequest(regReq())); err != nil {
		t.Fatal(err)
	}
	// same owner, new version -> ok
	r2 := regReq()
	r2.Version = "1.1.0"
	r2.Ref = "alice/wiki@sha2"
	if _, err := s.RegisterAppVersion(alice, connect.NewRequest(r2)); err != nil {
		t.Fatalf("same-owner new version rejected: %v", err)
	}
	// different owner -> PermissionDenied
	mallory := auth.WithOwner(context.Background(), "mallory")
	_, err := s.RegisterAppVersion(mallory, connect.NewRequest(regReq()))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
}

func TestRegisterAppVersionRejections(t *testing.T) {
	s, _, _ := newTestServer(t)
	alice := auth.WithOwner(context.Background(), "alice")
	bad := regReq()
	bad.Manifest.ApiVersion = "nope"
	if _, err := s.RegisterAppVersion(alice, connect.NewRequest(bad)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
	if _, err := s.RegisterAppVersion(context.Background(), connect.NewRequest(regReq())); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}
}
```

- [ ] **Step 2: Confirm failure:** `go test ./internal/cp/ -run TestRegisterAppVersion 2>&1 | head` (handler is the embedded Unimplemented).

- [ ] **Step 3: Implement `internal/cp/registration.go`:**
```go
package cp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
)

// RegisterAppVersion registers (or updates) an app version from a structured manifest — the
// registration source of truth. Fresh versions enter at tier `unverified`. CP does not fetch the
// definition repo; CI maps spawneryapp.yml -> this API.
func (s *Server) RegisterAppVersion(ctx context.Context, req *connect.Request[cpv1.RegisterAppVersionRequest]) (*connect.Response[cpv1.RegisterAppVersionResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	m := req.Msg.Manifest
	if err := validateManifest(m, req.Msg.Version, req.Msg.Ref); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	// Creator-ownership guard (sticky): only the original creator publishes new versions.
	creator := owner
	switch existing, err := s.st.Apps().Creator(ctx, m.Id); {
	case err == nil:
		if existing != owner {
			return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not the creator of %q", m.Id))
		}
		creator = existing
	case isNotFound(err):
		// new app; owner becomes creator
	default:
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	blob, err := protojson.Marshal(m)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	now := time.Now().Unix()
	mounts := make([]store.MountDecl, len(m.Mounts))
	for i, mt := range m.Mounts {
		mounts[i] = store.MountDecl{AppID: m.Id, Version: req.Msg.Version, Name: mt.Name, Path: mt.Path, Seed: mt.Seed, Required: true}
	}

	if err := s.st.WithTx(ctx, func(tx store.Store) error {
		if err := tx.Apps().Upsert(ctx, store.App{
			ID: m.Id, DisplayName: m.Title, Summary: m.Description, Tags: strings.Join(m.Tags, ","),
			Visibility: "public", Listed: true, CreatorID: creator, CreatedAt: now,
		}); err != nil {
			return err
		}
		return tx.Apps().UpsertVersion(ctx, store.AppVersion{
			AppID: m.Id, Version: req.Msg.Version, Ref: req.Msg.Ref,
			Tier: store.TierUnverified, Manifest: string(blob), CreatedAt: now,
		}, mounts)
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&cpv1.RegisterAppVersionResponse{
		AppId: m.Id, Version: req.Msg.Version, Tier: cpv1.TrustTier_TRUST_TIER_UNVERIFIED,
	}), nil
}

func isNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }
```
> Imports needed: add `"errors"` and `"strings"` to the import block (the snippet above uses `errors.Is`, `strings.Join`). Use `encoding/json` only if you prefer it over `protojson` — `protojson` is correct for a proto message and is already a transitive dep; if `google.golang.org/protobuf/encoding/protojson` isn't resolvable, run `go get google.golang.org/protobuf/encoding/protojson` (it ships with the protobuf module already in go.mod — it will be). Drop the `encoding/json` import if unused.

- [ ] **Step 4: Run:** `go test ./internal/cp/ -run TestRegisterAppVersion` — PASS.

- [ ] **Step 5: Full CP package + race:** `go test ./internal/cp/ -race` — PASS.

- [ ] **Step 6: Commit:**
```bash
git add internal/cp/registration.go internal/cp/registration_test.go
git commit --no-verify -m "feat(cp): RegisterAppVersion handler (sp-ma8)"
```

---

## Task 5: `spawnctl register` reference client + full manifest schema

**Files:** Modify `internal/manifest/manifest.go`, `internal/manifest/manifest_test.go`, `cmd/spawnctl/main.go`; create `cmd/spawnctl/register_test.go`.

- [ ] **Step 1: Failing manifest-parse test** — in `internal/manifest/manifest_test.go`, add a test asserting the full schema parses from `examples/secret-app/spawneryapp.yml` (read the existing test first to match its style/helpers):
```go
func TestParseFullSchema(t *testing.T) {
	m, err := Parse("../../examples/secret-app")
	if err != nil {
		t.Fatal(err)
	}
	if m.APIVersion != "spawnery/v1" || m.ID != "spawnery/secret" || m.Title != "Secret" {
		t.Fatalf("manifest = %+v", m)
	}
	if len(m.Storage.Mounts) != 1 || m.Storage.Mounts[0].Name != "main" || m.Storage.Mounts[0].Path != "data" {
		t.Fatalf("mounts = %+v", m.Storage.Mounts)
	}
	if m.Visibility != "open" {
		t.Fatalf("visibility = %q", m.Visibility)
	}
}
```

- [ ] **Step 2: Confirm failure:** `go test ./internal/manifest/ -run TestParseFullSchema 2>&1 | head` (no `APIVersion`/`Title`/`Visibility`).

- [ ] **Step 3: Extend `internal/manifest/manifest.go`** to the full E0 §3 schema (keep `Parse` as-is):
```go
type Mount struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
	Seed string `yaml:"seed"`
}
type Storage struct {
	Mounts []Mount `yaml:"mounts"`
}
type Agents struct {
	Support     []string `yaml:"support"`
	Exclude     []string `yaml:"exclude"`
	RequiresAcp []string `yaml:"requiresAcp"`
}
type Model struct {
	Requires struct {
		ToolUse          bool  `yaml:"toolUse"`
		MinContextTokens int64 `yaml:"minContextTokens"`
		Vision           bool  `yaml:"vision"`
	} `yaml:"requires"`
	RecommendedDefault string `yaml:"recommendedDefault"`
}
type Manifest struct {
	APIVersion  string   `yaml:"apiVersion"`
	Kind        string   `yaml:"kind"`
	ID          string   `yaml:"id"`
	Title       string   `yaml:"title"`
	Description string   `yaml:"description"`
	Tags        []string `yaml:"tags"`
	Visibility  string   `yaml:"visibility"`
	Agents      Agents   `yaml:"agents"`
	Tools       []string `yaml:"tools"`
	Persona     string   `yaml:"persona"`
	Skills      []string `yaml:"skills"`
	Model       Model    `yaml:"model"`
	Runtime     struct {
		BaseVersion string `yaml:"baseVersion"`
	} `yaml:"runtime"`
	Storage Storage `yaml:"storage"`
}
```
> NOTE: the example manifest uses `agents: { support: any, ... }` — `support` is a scalar `any` there, but a list elsewhere. yaml.v3 will error unmarshalling scalar `any` into `[]string`. Handle this: declare `Support` as `[]string` AND, if parsing the example fails, update `examples/secret-app/spawneryapp.yml` to `support: [any]` (canonical list form) so the schema is consistent. Prefer fixing the example to the list form over adding a custom unmarshaler. Confirm `requiresAcp: [prompt]` (list) already parses.

- [ ] **Step 4: Run:** `go test ./internal/manifest/` — PASS (fix the example YAML to `support: [any]` if needed, then re-run).

- [ ] **Step 5: Failing spawnctl mapping test** — create `cmd/spawnctl/register_test.go`:
```go
package main

import "testing"

func TestManifestToProto(t *testing.T) {
	pm, err := manifestToProto("../../examples/secret-app")
	if err != nil {
		t.Fatal(err)
	}
	if pm.ApiVersion != "spawnery/v1" || pm.Id != "spawnery/secret" || pm.Title != "Secret" {
		t.Fatalf("proto = %+v", pm)
	}
	if len(pm.Mounts) != 1 || pm.Mounts[0].Name != "main" || pm.Mounts[0].Path != "data" {
		t.Fatalf("mounts = %+v", pm.Mounts)
	}
}
```

- [ ] **Step 6: Confirm failure:** `go test ./cmd/spawnctl/ -run TestManifestToProto 2>&1 | head` (no `manifestToProto`).

- [ ] **Step 7: Add the `register` mode + mapping to `cmd/spawnctl/main.go`.** Add a `manifestToProto` helper and wire a `register` subcommand. Since the existing `main` uses top-level flags, implement `register` as a check on `flag.Arg(0)` OR a dedicated flag; the simplest that fits the current structure: add flags `-register` (bool), `-version`, `-ref`, reuse `-app`/`-cp`/`-token`. In `main`, if `*register` is set and `*cpAddr != ""`, call `runRegister`. Code:
```go
import (
	// add:
	"spawnery/internal/manifest"
)

func manifestToProto(appDir string) (*cpv1.AppManifest, error) {
	m, err := manifest.Parse(appDir)
	if err != nil {
		return nil, err
	}
	mounts := make([]*cpv1.ManifestMount, len(m.Storage.Mounts))
	for i, mt := range m.Storage.Mounts {
		mounts[i] = &cpv1.ManifestMount{Name: mt.Name, Path: mt.Path, Seed: mt.Seed}
	}
	return &cpv1.AppManifest{
		ApiVersion: m.APIVersion, Id: m.ID, Title: m.Title, Description: m.Description,
		Tags: m.Tags, Visibility: m.Visibility,
		Agents: &cpv1.ManifestAgents{Support: m.Agents.Support, Exclude: m.Agents.Exclude, RequiresAcp: m.Agents.RequiresAcp},
		Tools:  m.Tools, Persona: m.Persona, Skills: m.Skills,
		Model: &cpv1.ManifestModel{
			ToolUse: m.Model.Requires.ToolUse, MinContextTokens: m.Model.Requires.MinContextTokens,
			Vision: m.Model.Requires.Vision, RecommendedDefault: m.Model.RecommendedDefault,
		},
		RuntimeBaseVersion: m.Runtime.BaseVersion,
		Mounts:             mounts,
	}, nil
}

func runRegister(ctx context.Context, cpAddr, appDir, version, ref, token string) {
	pm, err := manifestToProto(appDir)
	if err != nil {
		log.Fatalf("manifest: %v", err)
	}
	client := cpv1connect.NewSpawnServiceClient(h2cClient(), cpAddr,
		connect.WithGRPC(), connect.WithInterceptors(cpBearer(token)))
	resp, err := client.RegisterAppVersion(ctx, connect.NewRequest(&cpv1.RegisterAppVersionRequest{Manifest: pm, Version: version, Ref: ref}))
	if err != nil {
		log.Fatalf("register: %v", err)
	}
	fmt.Printf("registered %s@%s tier=%s\n", resp.Msg.AppId, resp.Msg.Version, resp.Msg.Tier)
}
```
Wire flags in `main` (add near the others):
```go
	register := flag.Bool("register", false, "register the -app manifest with the CP and exit (CP mode)")
	version := flag.String("version", "1.0.0", "app version to register (with -register)")
	ref := flag.String("ref", "", "immutable app ref creator/app@sha (with -register)")
```
and, right after `flag.Parse()` and before the existing CP-mode branch:
```go
	if *register {
		if *cpAddr == "" {
			log.Fatal("-register requires -cp")
		}
		runRegister(ctx, *cpAddr, *appPath, *version, *ref, *token)
		return
	}
```
> Confirmed: `runCP` (main.go:92) builds the client as `cpv1connect.NewSpawnServiceClient(h2cClient(), addr, connect.WithGRPC(), connect.WithInterceptors(cpBearer(token)))`. `h2cClient()` and `cpBearer(token)` both already exist — reuse them exactly as shown in `runRegister` (the server speaks gRPC, so `WithGRPC()` is required and `cpBearer` sets the bearer header).

- [ ] **Step 8: Run + build:** `go test ./cmd/spawnctl/ && go build ./... && go vet ./...` — PASS/clean.

- [ ] **Step 9: Commit:**
```bash
git add internal/manifest cmd/spawnctl examples/secret-app/spawneryapp.yml
git commit --no-verify -m "feat(spawnctl): register reference client + full manifest schema (sp-ma8)"
```

---

## Final Verification

- [ ] `export PATH="$PATH:$(go env GOPATH)/bin" && make gen` — no diff.
- [ ] `go build ./... && go build -tags e2e ./... && go vet ./...` — clean.
- [ ] `go test ./...` — pass.
- [ ] `go test ./internal/cp/ ./internal/cp/store/ -race` — race-clean.

Then **superpowers:finishing-a-development-branch** (Option 1: merge to master locally; no remote).

---

## Self-Review Notes

- **Spec coverage:** §2 contracts → T1; §3 store → T2; §4 validation → T3; §5 handler → T4; §6 spawnctl → T5; §7 testing → tests across T2–T5. Out-of-scope (fetch/poll/scanner/review/private) absent. ✓
- **Type consistency:** `validateManifest(*cpv1.AppManifest, version, ref)`, `App.CreatorID`, `AppVersion.Manifest`, `MountDecl.Path/Seed`, `Creator(ctx,appID)`, `manifestToProto`, `cpv1.AppManifest`/`ManifestMount`/`ManifestAgents`/`ManifestModel`, `TrustTier_TRUST_TIER_UNVERIFIED` consistent T1–T5. ✓
- **Compile-per-commit:** T2 adds struct fields + fixes drift_test + seed in one commit; T3/T4/T5 are additive. ✓
- **Known risk:** the `support: any` scalar-vs-list YAML wrinkle (T5 Step 3) — fix the example to list form. The `protojson` import for the manifest blob (T4) ships with the protobuf module already in go.mod.
