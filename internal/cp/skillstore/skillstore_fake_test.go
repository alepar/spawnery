package skillstore_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"spawnery/internal/cp/skillstore"
)

// TestFakeSkillStore_StatObject_Present verifies StatObject returns nil for a sha that was put.
func TestFakeSkillStore_StatObject_Present(t *testing.T) {
	fake := skillstore.NewFakeSkillStore()
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := fake.PutIfAbsent(context.Background(), sha, []byte("x"), nil); err != nil {
		t.Fatalf("PutIfAbsent: %v", err)
	}

	if err := fake.StatObject(context.Background(), sha); err != nil {
		t.Fatalf("StatObject(present) = %v, want nil", err)
	}
}

// TestFakeSkillStore_StatObject_Absent verifies StatObject returns ErrObjectMissing for an
// unknown sha, and that it is errors.Is-detectable.
func TestFakeSkillStore_StatObject_Absent(t *testing.T) {
	fake := skillstore.NewFakeSkillStore()
	sha := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	err := fake.StatObject(context.Background(), sha)
	if err == nil {
		t.Fatal("StatObject(absent) = nil, want ErrObjectMissing")
	}
	if !errors.Is(err, skillstore.ErrObjectMissing) {
		t.Fatalf("StatObject(absent) = %v, want errors.Is(..., ErrObjectMissing)", err)
	}
}

// TestFakeSkillStore_StatObject_HookTransportError verifies that a StatHook-injected error is
// returned as-is and is NOT classified as ErrObjectMissing.
func TestFakeSkillStore_StatObject_HookTransportError(t *testing.T) {
	fake := skillstore.NewFakeSkillStore()
	sha := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	wantErr := fmt.Errorf("dial tcp: connection refused")
	fake.StatHook = func(_ context.Context, sha string) error { return wantErr }

	err := fake.StatObject(context.Background(), sha)
	if err == nil {
		t.Fatal("StatObject with StatHook = nil, want transport error")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("StatObject with StatHook = %v, want wrapping %v", err, wantErr)
	}
	if errors.Is(err, skillstore.ErrObjectMissing) {
		t.Fatal("StatHook transport error must NOT be classified as ErrObjectMissing")
	}
}

// TestFakeSkillStore_Get_RoundTrip verifies Get returns the exact bytes a prior PutIfAbsent
// stored.
func TestFakeSkillStore_Get_RoundTrip(t *testing.T) {
	fake := skillstore.NewFakeSkillStore()
	sha := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	want := []byte("compressed-tar-bytes")
	if err := fake.PutIfAbsent(context.Background(), sha, want, nil); err != nil {
		t.Fatalf("PutIfAbsent: %v", err)
	}

	got, err := fake.Get(context.Background(), sha)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("Get = %q, want %q", got, want)
	}
}

// TestFakeSkillStore_Get_Absent verifies Get returns ErrObjectMissing for an unknown sha.
func TestFakeSkillStore_Get_Absent(t *testing.T) {
	fake := skillstore.NewFakeSkillStore()
	sha := "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"

	_, err := fake.Get(context.Background(), sha)
	if err == nil {
		t.Fatal("Get(absent) = nil error, want ErrObjectMissing")
	}
	if !errors.Is(err, skillstore.ErrObjectMissing) {
		t.Fatalf("Get(absent) = %v, want errors.Is(..., ErrObjectMissing)", err)
	}
}

// TestFakeSkillStore_StatObject_RecordsCalls verifies StatObject records "stat:<sha>" in Calls.
func TestFakeSkillStore_StatObject_RecordsCalls(t *testing.T) {
	fake := skillstore.NewFakeSkillStore()
	sha := "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	_ = fake.StatObject(context.Background(), sha)

	calls := fake.Calls()
	var found bool
	for _, c := range calls {
		if c == "stat:"+sha {
			found = true
		}
	}
	if !found {
		t.Errorf("Calls() = %v, want to contain %q", calls, "stat:"+sha)
	}
}
