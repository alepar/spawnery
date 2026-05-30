// Package sidecar is the slice's minimal OpenAI-compatible inference proxy.
package sidecar

import (
	"net/http"
	"net/http/httputil"
	"net/url"
)

// NewHandler proxies requests to upstream, injecting the bearer key.
func NewHandler(upstream, key string) http.Handler {
	target, err := url.Parse(upstream)
	if err != nil {
		panic(err)
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	orig := rp.Director
	rp.Director = func(r *http.Request) {
		orig(r)
		r.Host = target.Host
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Del("X-Api-Key")
	}
	return rp
}
