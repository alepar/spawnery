package sidecar

import "sync/atomic"

// Inflight tracks active upstream inference requests for turn-boundary gating.
type Inflight struct {
	active atomic.Int64
}

func NewInflight() *Inflight {
	return &Inflight{}
}

func (i *Inflight) Begin() {
	if i == nil {
		return
	}
	i.active.Add(1)
}

func (i *Inflight) End() {
	if i == nil {
		return
	}
	for {
		n := i.active.Load()
		if n <= 0 {
			return
		}
		if i.active.CompareAndSwap(n, n-1) {
			return
		}
	}
}

func (i *Inflight) Active() int64 {
	if i == nil {
		return 0
	}
	return i.active.Load()
}

func (i *Inflight) Busy() bool {
	return i.Active() > 0
}

func firstInflight(trackers []*Inflight) *Inflight {
	if len(trackers) == 0 {
		return nil
	}
	return trackers[0]
}
