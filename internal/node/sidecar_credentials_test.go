package node

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestPostSidecarCredentialsPostsCredentialEndpoint(t *testing.T) {
	doer := stubDoerOK()

	if err := postSidecarCredentials(context.Background(), doer, "http://10.0.0.5:8081/control/model", "tok-abc", []byte("sk-test"), ""); err != nil {
		t.Fatalf("postSidecarCredentials returned error: %v", err)
	}

	if doer.calls != 1 {
		t.Fatalf("POST calls = %d, want 1", doer.calls)
	}
	if got := doer.gotReq.Method; got != http.MethodPost {
		t.Fatalf("method = %s, want POST", got)
	}
	if got := doer.gotReq.URL.String(); got != "http://10.0.0.5:8081/control/credentials" {
		t.Fatalf("url = %s, want credentials endpoint", got)
	}
	if got := doer.gotReq.Header.Get("Authorization"); got != "Bearer tok-abc" {
		t.Fatalf("authorization = %q, want bearer token", got)
	}
	if got := doer.gotReq.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	var body struct {
		Key      string `json:"key"`
		Upstream string `json:"upstream"`
	}
	if err := json.Unmarshal([]byte(doer.gotBody), &body); err != nil {
		t.Fatalf("body not JSON: %v (%q)", err, doer.gotBody)
	}
	if body.Key != "sk-test" || body.Upstream != "" {
		t.Fatalf("body = %+v, want key and empty upstream", body)
	}
}

func TestPostSidecarCredentialsFailsClosed(t *testing.T) {
	tests := []struct {
		name       string
		doer       httpDoer
		controlURL string
		key        []byte
	}{
		{name: "empty key", doer: stubDoerOK(), controlURL: "http://10.0.0.5:8081/control/model", key: nil},
		{name: "empty control url", doer: stubDoerOK(), controlURL: "", key: []byte("sk-test")},
		{name: "transport error", doer: &stubDoer{err: io.ErrUnexpectedEOF}, controlURL: "http://10.0.0.5:8081/control/model", key: []byte("sk-test")},
		{name: "non 2xx", doer: &stubDoer{status: 503}, controlURL: "http://10.0.0.5:8081/control/model", key: []byte("sk-test")},
		{name: "bad url", doer: stubDoerOK(), controlURL: "://bad-url", key: []byte("sk-test")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := postSidecarCredentials(context.Background(), tt.doer, tt.controlURL, "tok", tt.key, "")
			if err == nil {
				t.Fatal("postSidecarCredentials returned nil, want error")
			}
			if strings.TrimSpace(err.Error()) == "" {
				t.Fatal("error detail must be non-empty")
			}
		})
	}
}
