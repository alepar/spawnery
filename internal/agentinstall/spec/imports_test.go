package spec_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSpecLeafInvariant asserts internal/agentinstall/spec imports ONLY the Go
// standard library: no spawnery packages and no third-party modules (go-toml,
// hujson, gen, …). This is the load-bearing guarantee that lets the control
// plane import spec without pulling the agentinstall emitter dependency tree.
func TestSpecLeafInvariant(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse dir %s: %v", dir, err)
	}
	for pkgName, pkg := range pkgs {
		for filename, file := range pkg.Files {
			for _, imp := range file.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				// Stdlib import paths have no dot in their first segment
				// (e.g. "encoding/json"); everything else is a module path.
				first, _, _ := strings.Cut(path, "/")
				// Stdlib paths have no dot in the first segment AND are not the
				// local module (module name is "spawnery", no dot, so the dot
				// check alone silently passes spawnery/internal/* and spawnery/gen/*).
				if strings.Contains(first, ".") || first == "spawnery" {
					t.Errorf("non-stdlib import in pkg %s file %s: %q (spec must be stdlib-only)",
						pkgName, filepath.Base(filename), path)
				}
			}
		}
	}
}
