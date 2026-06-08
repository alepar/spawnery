// Package sidecar is the slice's minimal OpenAI-compatible inference proxy.
package sidecar

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
)

// NewHandler proxies requests to upstream, injecting the bearer key. When ov holds a
// model override, the top-level "model" of each request body is rewritten to it; when
// unset the request body is forwarded byte-identical (zero overhead).
func NewHandler(upstream, key string, ov *Override) http.Handler {
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
	// Surface upstream (OpenRouter) ERROR responses in the sidecar logs — e.g. a 503
	// "Provider returned error" from the model provider. 2xx (incl. streaming chat completions) is
	// left untouched; only >=400 bodies are buffered (they are small JSON) and restored for the agent.
	rp.ModifyResponse = func(resp *http.Response) error {
		if resp.StatusCode < 400 || resp.Body == nil {
			return nil
		}
		b, rerr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if rerr != nil {
			log.Printf("warn: sidecar: upstream %s %s -> %d (body read error: %v)", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, rerr)
			resp.Body = io.NopCloser(bytes.NewReader(nil))
			return nil
		}
		resp.Body = io.NopCloser(bytes.NewReader(b)) // restore the full body for the agent
		snippet := strings.TrimSpace(string(b))
		if len(snippet) > 512 {
			snippet = snippet[:512] + "…"
		}
		log.Printf("warn: sidecar: upstream %s %s -> %d: %s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, snippet)
		return nil
	}
	// Log (and 502) when upstream is unreachable (DNS/connection failure) rather than a non-2xx body.
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("warn: sidecar: upstream request %s %s failed: %v", r.Method, r.URL.Path, err)
		w.WriteHeader(http.StatusBadGateway)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m := ov.Get(); m != "" {
			rewriteRequestModel(r, m)
		}
		rp.ServeHTTP(w, r)
	})
}

// rewriteRequestModel buffers r's JSON body, replaces the top-level "model" with model,
// and fixes ContentLength/Content-Length. Request bodies are complete JSON (only responses
// stream), so buffering is safe. On any error (no body / non-JSON) the original body is
// left intact so the request still forwards unchanged.
func rewriteRequestModel(r *http.Request, model string) {
	if r.Body == nil {
		return
	}
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		log.Printf("warn: sidecar: read request body for model override: %v", err)
		return
	}
	patched, err := patchModelJSON(body, model)
	if err != nil {
		// Not a JSON object (e.g. GET with empty body): forward the original bytes.
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(patched))
	r.ContentLength = int64(len(patched))
	r.Header.Set("Content-Length", strconv.Itoa(len(patched)))
}
