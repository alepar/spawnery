package sidecar

import (
	"testing"
)

func TestClassifyGitHubHost(t *testing.T) {
	tests := []struct {
		hostport string
		want     ghAction
		desc     string
	}{
		// git smart-HTTP / LFS batch → inject Basic
		{"github.com", actionMitmBasic, "github.com plain"},
		{"codeload.github.com", actionMitmBasic, "codeload plain"},
		{"github.com:443", actionMitmBasic, "github.com:443 port stripped"},
		{"GitHub.com", actionMitmBasic, "GitHub.com case-folded"},
		{"GitHub.com:443", actionMitmBasic, "GitHub.com:443 both"},

		// REST/GraphQL/raw → inject Bearer
		{"api.github.com", actionMitmBearer, "api.github.com"},
		{"uploads.github.com", actionMitmBearer, "uploads.github.com"},
		{"gist.github.com", actionMitmBearer, "gist.github.com"},
		{"raw.githubusercontent.com", actionMitmBearer, "raw.githubusercontent.com"},
		{"api.github.com:443", actionMitmBearer, "api.github.com:443 port stripped"},

		// Presigned object stores → tunnel only (no inject; S3 "Only one auth mechanism")
		{"objects.githubusercontent.com", actionTunnel, "objects.githubusercontent.com"},
		{"lfs.github.com", actionTunnel, "lfs.github.com"},

		// *-cloud.githubusercontent.com family → tunnel
		{"media-cloud.githubusercontent.com", actionTunnel, "-cloud suffix"},
		{"private-cloud.githubusercontent.com", actionTunnel, "private-cloud suffix"},

		// *.s3.amazonaws.com family → tunnel
		{"github-cloud.s3.amazonaws.com", actionTunnel, "s3 amazonaws"},
		{"objects-origin.githubusercontent.com", actionTunnel, "objects-origin via .githubusercontent.com suffix would be tunnel"},

		// SECURITY: look-alikes that must NEVER receive inject
		{"github.com.attacker.example", actionTunnel, "subdomain superset"},
		{"evilgithubusercontent.com", actionTunnel, "substring match not allowed"},
		{"notgithub.com", actionTunnel, "unrelated .com"},
		{"api.github.com.evil.com", actionTunnel, "subdomain evil extension"},
		{"xgithub.com", actionTunnel, "xgithub prefix"},
		{"notraw.githubusercontent.com", actionTunnel, "not raw.githubusercontent.com — unregistered subdomain is tunnel"},

		// Non-GitHub → tunnel
		{"example.com", actionTunnel, "random host"},
		{"gitlab.com", actionTunnel, "gitlab"},
		{"bitbucket.org", actionTunnel, "bitbucket"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.desc, func(t *testing.T) {
			got := classifyGitHubHost(tc.hostport)
			if got != tc.want {
				t.Errorf("classifyGitHubHost(%q) = %v, want %v", tc.hostport, got, tc.want)
			}
		})
	}
}
