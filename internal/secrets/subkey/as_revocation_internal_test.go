package subkey

import (
	"testing"
	"time"
)

func TestASRevocationCheckerDefaultClientHasTimeout(t *testing.T) {
	c := NewASRevocationChecker("http://127.0.0.1/node-revocations", nil, 0)
	if c.client == nil {
		t.Fatal("client is nil")
	}
	if c.client.Timeout != 10*time.Second {
		t.Fatalf("default client timeout = %s, want 10s", c.client.Timeout)
	}
}
