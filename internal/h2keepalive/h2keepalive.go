// Package h2keepalive centralises HTTP/2 keepalive PING tuning for all CP and node endpoints.
//
// Detection math: after ReadIdleTimeout (10 s) of inactivity the peer receives a PING frame.
// If no ACK arrives within PingTimeout (5 s) the connection is closed — giving a worst-case
// black-holed-peer detection time of ~15 s.  Both values sit well above typical Go
// stop-the-world GC pauses (sub-ms to low-ms), so false-positive flaps are not a concern in
// practice.  Do not tune lower without measuring real GC pause times in the target environment.
package h2keepalive

import (
	"time"

	"golang.org/x/net/http2"
)

const (
	// ReadIdleTimeout is the period of inactivity after which an HTTP/2 peer is sent a PING.
	ReadIdleTimeout = 10 * time.Second

	// PingTimeout is how long the local side waits for a PING ACK before closing the
	// connection.  Must be less than ReadIdleTimeout.
	PingTimeout = 5 * time.Second
)

// ConfigureServer applies the keepalive timeouts to an http2.Server.  Call this before passing
// the server to h2c.NewHandler or http2.ConfigureServer.
func ConfigureServer(s *http2.Server) {
	s.ReadIdleTimeout = ReadIdleTimeout
	s.PingTimeout = PingTimeout
}

// ConfigureTransport applies the keepalive timeouts to an http2.Transport.  Call this after
// constructing the transport and before using it.
func ConfigureTransport(t *http2.Transport) {
	t.ReadIdleTimeout = ReadIdleTimeout
	t.PingTimeout = PingTimeout
}
