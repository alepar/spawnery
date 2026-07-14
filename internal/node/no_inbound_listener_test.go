package node

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"testing"
)

// listenerCalls are the ways Go code binds a socket. The node's github control plane must contain NONE
// of them: after sp-2tx8.9 the node DIALS the sidecar and never listens. The old per-spawn listener sat
// on the CNI bridge, reachable from every pod, and its TCP lane could not even bind (sp-2tx8.8).
var listenerCalls = []string{
	"net.Listen",
	"net.ListenTCP",
	"net.ListenUnix",
	"net.ListenPacket",
	"http.ListenAndServe",
	"http.ListenAndServeTLS",
	"http.Serve",
}

// githubPlaneFiles are the source files of the node's github control plane.
var githubPlaneFiles = []string{
	"githubcontrol.go",
	"githubpush.go",
	"githubevents.go",
	"github_refresh.go",
}

// TestGitHubControlPlaneBindsNoListener is the adversarial guard required by sp-2tx8.9.5: the node must
// expose NO pod-reachable control endpoint. A behavioural test cannot prove a negative here (there is no
// API left to call), so this asserts it at the source level — any re-introduced bind in the github
// control plane fails the build's tests, loudly, with the reason.
func TestGitHubControlPlaneBindsNoListener(t *testing.T) {
	fset := token.NewFileSet()
	for _, name := range githubPlaneFiles {
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			qualified := pkg.Name + "." + sel.Sel.Name
			for _, bad := range listenerCalls {
				if qualified == bad {
					t.Errorf("%s: %s binds a listener in the node's github control plane. "+
						"sp-2tx8.9 deleted the node's inbound control endpoint on purpose: a node "+
						"cannot bind a pod IP, and any address it CAN bind is reachable from every pod "+
						"on the bridge. The node pushes to the sidecar (githubpush.go) — it must not listen.",
						fset.Position(call.Pos()), qualified)
				}
			}
			return true
		})
	}
}

// TestGitHubControlServerHasNoServeMethod: the spawnlet.GitHubControlServer interface no longer has
// Serve, so a stray Serve method would compile silently. Assert the method set directly.
func TestGitHubControlServerHasNoServeMethod(t *testing.T) {
	typ := reflect.TypeOf(&githubControlServer{})
	for i := range typ.NumMethod() {
		name := typ.Method(i).Name
		if name == "Serve" || strings.HasPrefix(name, "ListenAndServe") {
			t.Fatalf("githubControlServer has method %s — the node's inbound control listener is back", name)
		}
	}
}
