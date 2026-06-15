package store

import (
	"reflect"
	"testing"
)

func TestNodeRevocationsRevokeAndListSorted(t *testing.T) {
	st := NewTestStore(t)
	ctx := ctxT()

	if err := st.NodeRevocations().Revoke(ctx, "node-b", "stolen host", 200); err != nil {
		t.Fatal(err)
	}
	if err := st.NodeRevocations().Revoke(ctx, "node-a", "decommissioned", 100); err != nil {
		t.Fatal(err)
	}

	revoked, err := st.NodeRevocations().IsRevoked(ctx, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Fatal("node-a should be revoked")
	}

	revoked, err = st.NodeRevocations().IsRevoked(ctx, "node-z")
	if err != nil {
		t.Fatal(err)
	}
	if revoked {
		t.Fatal("node-z should not be revoked")
	}

	rows, err := st.NodeRevocations().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{rows[0].NodeID, rows[1].NodeID}; !reflect.DeepEqual(got, []string{"node-a", "node-b"}) {
		t.Fatalf("sorted node ids = %v", got)
	}
}

func TestNodeRevocationsRevokeIsIdempotentUpdate(t *testing.T) {
	st := NewTestStore(t)
	ctx := ctxT()

	if err := st.NodeRevocations().Revoke(ctx, "node-a", "old", 100); err != nil {
		t.Fatal(err)
	}
	if err := st.NodeRevocations().Revoke(ctx, "node-a", "new", 200); err != nil {
		t.Fatal(err)
	}

	rows, err := st.NodeRevocations().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Reason != "new" || rows[0].RevokedAt != 200 {
		t.Fatalf("rows = %+v", rows)
	}
}
