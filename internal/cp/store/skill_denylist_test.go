package store_test

import (
	"context"
	"errors"
	"testing"

	"spawnery/internal/cp/store"
)

func TestSkillDenylist_DenyThenDenied(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	sha := repeatHex("a")

	if err := st.SkillDenylist().Deny(ctx, store.SkillObjectDenial{
		SHA256:    sha,
		Reason:    "malicious payload",
		DeniedBy:  "admin",
		CreatedAt: 1000,
	}); err != nil {
		t.Fatalf("Deny: %v", err)
	}

	got, err := st.SkillDenylist().Denied(ctx, []string{sha})
	if err != nil {
		t.Fatalf("Denied: %v", err)
	}
	d, ok := got[sha]
	if !ok {
		t.Fatalf("Denied: sha %q not in result %v", sha, got)
	}
	if d.Reason != "malicious payload" || d.DeniedBy != "admin" {
		t.Fatalf("Denied: got %+v, want reason=malicious payload denied_by=admin", d)
	}
}

func TestSkillDenylist_DeniedMixedSlice_OnlyDeniedReturned(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	shaDenied := repeatHex("b")
	shaClean := repeatHex("c")

	if err := st.SkillDenylist().Deny(ctx, store.SkillObjectDenial{
		SHA256: shaDenied, Reason: "bad", DeniedBy: "admin", CreatedAt: 1000,
	}); err != nil {
		t.Fatalf("Deny: %v", err)
	}

	got, err := st.SkillDenylist().Denied(ctx, []string{shaDenied, shaClean})
	if err != nil {
		t.Fatalf("Denied: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Denied: got %d entries, want 1: %v", len(got), got)
	}
	if _, ok := got[shaDenied]; !ok {
		t.Fatalf("Denied: expected %q present, got %v", shaDenied, got)
	}
	if _, ok := got[shaClean]; ok {
		t.Fatalf("Denied: expected %q absent, got %v", shaClean, got)
	}
}

func TestSkillDenylist_DeniedEmptyOrNil_NoQuery(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	got, err := st.SkillDenylist().Denied(ctx, nil)
	if err != nil {
		t.Fatalf("Denied(nil): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Denied(nil) = %v, want empty", got)
	}

	got, err = st.SkillDenylist().Denied(ctx, []string{})
	if err != nil {
		t.Fatalf("Denied(empty): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Denied(empty) = %v, want empty", got)
	}
}

func TestSkillDenylist_DenyTwiceUpdatesReason(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	sha := repeatHex("d")

	if err := st.SkillDenylist().Deny(ctx, store.SkillObjectDenial{
		SHA256: sha, Reason: "first reason", DeniedBy: "admin", CreatedAt: 1000,
	}); err != nil {
		t.Fatalf("first Deny: %v", err)
	}
	if err := st.SkillDenylist().Deny(ctx, store.SkillObjectDenial{
		SHA256: sha, Reason: "updated reason", DeniedBy: "admin2", CreatedAt: 2000,
	}); err != nil {
		t.Fatalf("second Deny (re-deny): %v", err)
	}

	got, err := st.SkillDenylist().Denied(ctx, []string{sha})
	if err != nil {
		t.Fatalf("Denied: %v", err)
	}
	d, ok := got[sha]
	if !ok {
		t.Fatalf("Denied: sha not present after re-deny")
	}
	if d.Reason != "updated reason" || d.DeniedBy != "admin2" || d.CreatedAt != 2000 {
		t.Fatalf("Denied after re-deny: got %+v, want reason=updated reason denied_by=admin2 created_at=2000", d)
	}
}

func TestSkillDenylist_Allow_RemovesDenial(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	sha := repeatHex("e")
	if err := st.SkillDenylist().Deny(ctx, store.SkillObjectDenial{
		SHA256: sha, Reason: "bad", DeniedBy: "admin", CreatedAt: 1000,
	}); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	if err := st.SkillDenylist().Allow(ctx, sha); err != nil {
		t.Fatalf("Allow: %v", err)
	}

	got, err := st.SkillDenylist().Denied(ctx, []string{sha})
	if err != nil {
		t.Fatalf("Denied: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Denied after Allow = %v, want empty", got)
	}
}

func TestSkillDenylist_Allow_UnknownSha_ErrNotFound(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	err := st.SkillDenylist().Allow(ctx, repeatHex("f"))
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Allow(unknown) = %v, want ErrNotFound", err)
	}
}

func TestSkillDenylist_List_NewestFirst(t *testing.T) {
	st := store.NewTestStore(t)
	ctx := context.Background()

	sha1 := repeatHex("1")
	sha2 := repeatHex("2")
	sha3 := repeatHex("3")

	if err := st.SkillDenylist().Deny(ctx, store.SkillObjectDenial{
		SHA256: sha1, Reason: "r1", DeniedBy: "admin", CreatedAt: 1000,
	}); err != nil {
		t.Fatalf("Deny sha1: %v", err)
	}
	if err := st.SkillDenylist().Deny(ctx, store.SkillObjectDenial{
		SHA256: sha2, Reason: "r2", DeniedBy: "admin", CreatedAt: 2000,
	}); err != nil {
		t.Fatalf("Deny sha2: %v", err)
	}
	if err := st.SkillDenylist().Deny(ctx, store.SkillObjectDenial{
		SHA256: sha3, Reason: "r3", DeniedBy: "admin", CreatedAt: 3000,
	}); err != nil {
		t.Fatalf("Deny sha3: %v", err)
	}

	list, err := st.SkillDenylist().List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("List: got %d entries, want 3", len(list))
	}
	if list[0].SHA256 != sha3 || list[1].SHA256 != sha2 || list[2].SHA256 != sha1 {
		t.Fatalf("List order = %v, want newest-first (sha3, sha2, sha1)", list)
	}
}

// repeatHex returns a 64-char lowercase hex-ish string built from the given single char, for
// test sha256 stand-ins. Not real hex validation — the store layer does not validate sha shape
// (the handler layer does, per D2).
func repeatHex(c string) string {
	out := ""
	for i := 0; i < 64; i++ {
		out += c
	}
	return out
}
