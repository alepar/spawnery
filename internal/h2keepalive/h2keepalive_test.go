package h2keepalive_test

import (
	"testing"

	"golang.org/x/net/http2"

	"spawnery/internal/h2keepalive"
)

func TestConfigureServer(t *testing.T) {
	s := &http2.Server{}
	h2keepalive.ConfigureServer(s)
	if s.ReadIdleTimeout != h2keepalive.ReadIdleTimeout {
		t.Errorf("ReadIdleTimeout: got %v, want %v", s.ReadIdleTimeout, h2keepalive.ReadIdleTimeout)
	}
	if s.PingTimeout != h2keepalive.PingTimeout {
		t.Errorf("PingTimeout: got %v, want %v", s.PingTimeout, h2keepalive.PingTimeout)
	}
}

func TestConfigureTransport(t *testing.T) {
	tr := &http2.Transport{}
	h2keepalive.ConfigureTransport(tr)
	if tr.ReadIdleTimeout != h2keepalive.ReadIdleTimeout {
		t.Errorf("ReadIdleTimeout: got %v, want %v", tr.ReadIdleTimeout, h2keepalive.ReadIdleTimeout)
	}
	if tr.PingTimeout != h2keepalive.PingTimeout {
		t.Errorf("PingTimeout: got %v, want %v", tr.PingTimeout, h2keepalive.PingTimeout)
	}
}

func TestPingTimeoutLessThanReadIdleTimeout(t *testing.T) {
	if h2keepalive.PingTimeout >= h2keepalive.ReadIdleTimeout {
		t.Errorf("invariant violated: PingTimeout (%v) must be < ReadIdleTimeout (%v)",
			h2keepalive.PingTimeout, h2keepalive.ReadIdleTimeout)
	}
}
