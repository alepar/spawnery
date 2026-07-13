package store

import (
	"context"
	"reflect"
	"testing"
)

func TestRevocationPageAfterBoundsPrunesAndPreservesGaps(t *testing.T) {
	st := NewTestStore(t)
	appendEvent := func(ev RevocationEvent) int64 {
		t.Helper()
		seq, err := st.Revocations().Append(ctxT(), ev)
		if err != nil {
			t.Fatal(err)
		}
		return seq
	}
	expiredSeq := appendEvent(RevocationEvent{
		AccountID: "acct", FamilyID: "expired-family", RevokedAt: 1,
		RevokedTokens: []RevokedToken{{TokenID: "expired", RetainUntil: 10}},
	})
	liveSeq := appendEvent(RevocationEvent{
		AccountID: "acct", FamilyID: "live-family", RevokedAt: 2,
		RevokedTokens: []RevokedToken{{TokenID: "live", RetainUntil: 100}},
	})
	oldAccountSeq := appendEvent(RevocationEvent{
		AccountID: "acct", RevokedAt: 3, RevokeTokensIssuedBefore: 3,
	})
	newAccountSeq := appendEvent(RevocationEvent{
		AccountID: "acct", RevokedAt: 4, RevokeTokensIssuedBefore: 4,
	})

	page, hasMore, err := st.Revocations().PageAfter(ctxT(), 0, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !hasMore || len(page) != 1 || page[0].Seq != liveSeq {
		t.Fatalf("first page: entries=%+v has_more=%v", page, hasMore)
	}
	page, hasMore, err = st.Revocations().PageAfter(ctxT(), liveSeq, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if hasMore || len(page) != 1 || page[0].Seq != newAccountSeq {
		t.Fatalf("second page: entries=%+v has_more=%v", page, hasMore)
	}

	var seqs []int64
	if err := st.(*bunStore).db.NewSelect().Table("revocation_events").Column("seq").OrderExpr("seq ASC").Scan(context.Background(), &seqs); err != nil {
		t.Fatal(err)
	}
	if want := []int64{liveSeq, newAccountSeq}; !reflect.DeepEqual(seqs, want) {
		t.Fatalf("retained sequences: want %v, got %v (expired=%d old-account=%d)", want, seqs, expiredSeq, oldAccountSeq)
	}
}

func TestRevocationPageAfterUsesLookahead(t *testing.T) {
	st := NewTestStore(t)
	var seqs []int64
	for i := range 3 {
		seq, err := st.Revocations().Append(ctxT(), RevocationEvent{
			AccountID: "acct", FamilyID: "family-" + string(rune('a'+i)), RevokedAt: 1,
			RevokedTokens: []RevokedToken{{TokenID: "token-" + string(rune('a'+i)), RetainUntil: 100}},
		})
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, seq)
	}
	page, hasMore, err := st.Revocations().PageAfter(ctxT(), 0, 2, 50)
	if err != nil {
		t.Fatal(err)
	}
	if !hasMore || len(page) != 2 || page[0].Seq != seqs[0] || page[1].Seq != seqs[1] {
		t.Fatalf("bounded page: entries=%+v has_more=%v", page, hasMore)
	}
	page, hasMore, err = st.Revocations().PageAfter(ctxT(), seqs[1], 2, 50)
	if err != nil || hasMore || len(page) != 1 || page[0].Seq != seqs[2] {
		t.Fatalf("terminal page: entries=%+v has_more=%v err=%v", page, hasMore, err)
	}
}

func TestRevocationPageAfterRollsBackFailedPrune(t *testing.T) {
	st := NewTestStore(t)
	seq, err := st.Revocations().Append(ctxT(), RevocationEvent{
		AccountID: "acct", FamilyID: "family", RevokedAt: 1,
		RevokedTokens: []RevokedToken{{TokenID: "expired", RetainUntil: 10}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.(*bunStore).db.ExecContext(ctxT(), `
		CREATE TRIGGER fail_token_prune BEFORE DELETE ON revocation_event_tokens
		BEGIN SELECT RAISE(ABORT, 'forced prune failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Revocations().PageAfter(ctxT(), 0, 256, 10); err == nil {
		t.Fatal("failed prune returned a page")
	}
	var count int
	if err := st.(*bunStore).db.NewSelect().Table("revocation_events").ColumnExpr("COUNT(*)").Where("seq = ?", seq).Scan(ctxT(), &count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("failed prune deleted the event header")
	}
}

func TestRevocationAppendRollsBackFailedTokenInsert(t *testing.T) {
	st := NewTestStore(t)
	if _, err := st.(*bunStore).db.ExecContext(ctxT(), `
		CREATE TRIGGER fail_token_insert BEFORE INSERT ON revocation_event_tokens
		BEGIN SELECT RAISE(ABORT, 'forced token failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Revocations().Append(ctxT(), RevocationEvent{
		AccountID: "acct", FamilyID: "family", RevokedAt: 1,
		RevokedTokens: []RevokedToken{{TokenID: "token", RetainUntil: 10}},
	}); err == nil {
		t.Fatal("failed token insert reported success")
	}
	var count int
	if err := st.(*bunStore).db.NewSelect().Table("revocation_events").ColumnExpr("COUNT(*)").Scan(ctxT(), &count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("orphan event headers: %d", count)
	}
}
