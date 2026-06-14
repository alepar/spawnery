package runtime

import (
	"context"
	"os"
)

// RootMaterializeSpec asks the runtime to recreate TargetPath as files created by
// container-root. It is used by Docker userns-remap nodes where the rootless
// spawnlet cannot chown host files into the remapped uid range.
type RootMaterializeSpec struct {
	Image      string
	SourcePath string
	TargetPath string
	DirMode    os.FileMode
	FileMode   os.FileMode
}

// RootMaterializer is an optional runtime extension. Docker implements it by
// running a short-lived helper container with SourcePath mounted read-only and
// TargetPath's parent mounted read-write.
type RootMaterializer interface {
	MaterializeRootOwned(ctx context.Context, spec RootMaterializeSpec) error
}
