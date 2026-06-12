package cp

import (
	"testing"
	"time"
)

func TestDeliveryPendingMarkAndCheck(t *testing.T) {
	now := time.Unix(1000, 0)
	tr := &deliveryPendingTracker{m: map[string]deliveryPendingEntry{}, now: func() time.Time { return now }}

	if tr.isPending("sp1") {
		t.Fatal("want not pending before mark")
	}
	tr.mark("sp1")
	if !tr.isPending("sp1") {
		t.Fatal("want pending after mark")
	}
}

func TestDeliveryPendingClear(t *testing.T) {
	tr := newDeliveryPendingTracker()
	tr.mark("sp1")
	tr.clear("sp1")
	if tr.isPending("sp1") {
		t.Fatal("want not pending after clear")
	}
}

func TestDeliveryPendingExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	tr := &deliveryPendingTracker{m: map[string]deliveryPendingEntry{}, now: func() time.Time { return now }}
	tr.mark("sp1")

	// Before deadline.
	now = time.Unix(1000, 0).Add(deliveryPendingDeadline - time.Second)
	if !tr.isPending("sp1") {
		t.Fatal("want pending before deadline")
	}

	// After deadline.
	now = time.Unix(1000, 0).Add(deliveryPendingDeadline + time.Second)
	if tr.isPending("sp1") {
		t.Fatal("want not pending after deadline")
	}
}

func TestDeliveryPendingMarkRefreshesDeadline(t *testing.T) {
	now := time.Unix(1000, 0)
	tr := &deliveryPendingTracker{m: map[string]deliveryPendingEntry{}, now: func() time.Time { return now }}
	tr.mark("sp1")

	// Advance past original deadline, then mark again.
	now = time.Unix(1000, 0).Add(deliveryPendingDeadline + time.Second)
	if tr.isPending("sp1") {
		t.Fatal("want expired before re-mark")
	}
	tr.mark("sp1") // re-mark from current time
	if !tr.isPending("sp1") {
		t.Fatal("want pending after re-mark")
	}
}

func TestDeliveryPendingClearOnUnknownIsNoop(t *testing.T) {
	tr := newDeliveryPendingTracker()
	tr.clear("nonexistent") // must not panic
}
