package agentinstall_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLeafInvariant asserts that internal/agentinstall imports NO other
// spawnery/internal/* or spawnery/gen/* package.
// This enforces the leaf posture: agentinstall is standalone and go-install-able.
func TestLeafInvariant(t *testing.T) {
	// Find the package directory relative to this test file.
	// The test runs with the package dir as the working directory.
	dir, err := packageDir()
	if err != nil {
		t.Fatalf("cannot find package dir: %v", err)
	}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		// Skip test files — they may import from the package itself.
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse dir %s: %v", dir, err)
	}

	for pkgName, pkg := range pkgs {
		for filename, file := range pkg.Files {
			for _, imp := range file.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				// agentinstall may import its own stdlib-only sub-packages
				// (e.g. .../spec, separately guarded by TestSpecLeafInvariant);
				// it must not import any OTHER spawnery/internal/* package.
				if strings.HasPrefix(path, "spawnery/internal/") &&
					path != "spawnery/internal/agentinstall" &&
					!strings.HasPrefix(path, "spawnery/internal/agentinstall/") {
					t.Errorf("leaf violation in pkg %s file %s: import %q (must not import other spawnery/internal/* packages)",
						pkgName, filepath.Base(filename), path)
				}
				if strings.HasPrefix(path, "spawnery/gen/") {
					t.Errorf("leaf violation in pkg %s file %s: import %q (must not import spawnery/gen/*)",
						pkgName, filepath.Base(filename), path)
				}
			}
		}
	}
}

// packageDir returns the package directory; go test sets cwd to the package dir.
func packageDir() (string, error) {
	return os.Getwd()
}
