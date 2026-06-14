package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/docker/docker/api/types/container"
)

const rootMaterializeScript = `
set -eu

target="/spawnery-target/${SPAWNERY_TARGET_NAME}"
work="/spawnery-work"

rm -rf "$work"
mkdir -p "$work"

if [ "${SPAWNERY_HAS_SOURCE}" = "1" ]; then
	cp -a --no-preserve=ownership /spawnery-source/. "$work"/
fi

find "$work" -type d -exec chmod "${SPAWNERY_DIR_MODE}" {} +
find "$work" -type f -exec chmod "${SPAWNERY_FILE_MODE}" {} +

rm -rf "$target"
mkdir -p "$target"
cp -a --no-preserve=ownership "$work"/. "$target"/

find "$target" -type d -exec chmod "${SPAWNERY_DIR_MODE}" {} +
find "$target" -type f -exec chmod "${SPAWNERY_FILE_MODE}" {} +
`

// MaterializeRootOwned recreates spec.TargetPath using files created by a
// short-lived container running as container-root. On a daemon with userns-remap,
// those files appear as root:root inside later remapped containers even though
// the rootless spawnlet cannot chown host-owned files into the remap range.
func (d *Docker) MaterializeRootOwned(ctx context.Context, spec RootMaterializeSpec) error {
	if spec.Image == "" {
		return fmt.Errorf("materialize root-owned: helper image is empty")
	}
	targetPath, err := filepath.Abs(spec.TargetPath)
	if err != nil {
		return fmt.Errorf("materialize root-owned: target path: %w", err)
	}
	targetParent := filepath.Dir(targetPath)
	targetName := filepath.Base(targetPath)
	if targetName == "." || targetName == string(filepath.Separator) {
		return fmt.Errorf("materialize root-owned: invalid target path %q", spec.TargetPath)
	}
	if err := os.MkdirAll(targetParent, 0o777); err != nil {
		return fmt.Errorf("materialize root-owned: mkdir target parent: %w", err)
	}
	if err := os.Chmod(targetParent, 0o777); err != nil {
		return fmt.Errorf("materialize root-owned: chmod target parent: %w", err)
	}

	dirMode := spec.DirMode.Perm()
	if dirMode == 0 {
		dirMode = 0o777
	}
	fileMode := spec.FileMode.Perm()
	if fileMode == 0 {
		fileMode = 0o666
	}

	env := []string{
		"SPAWNERY_TARGET_NAME=" + targetName,
		"SPAWNERY_HAS_SOURCE=0",
		fmt.Sprintf("SPAWNERY_DIR_MODE=%04o", dirMode),
		fmt.Sprintf("SPAWNERY_FILE_MODE=%04o", fileMode),
	}
	binds := []string{targetParent + ":/spawnery-target:z"}
	if spec.SourcePath != "" {
		sourcePath, err := filepath.Abs(spec.SourcePath)
		if err != nil {
			return fmt.Errorf("materialize root-owned: source path: %w", err)
		}
		if st, err := os.Stat(sourcePath); err != nil {
			return fmt.Errorf("materialize root-owned: stat source: %w", err)
		} else if !st.IsDir() {
			return fmt.Errorf("materialize root-owned: source %q is not a directory", sourcePath)
		}
		binds = append(binds, sourcePath+":/spawnery-source:ro")
		env[1] = "SPAWNERY_HAS_SOURCE=1"
	}

	created, err := d.cli.ContainerCreate(ctx,
		&container.Config{
			Image:      spec.Image,
			Entrypoint: []string{"sh", "-ceu", rootMaterializeScript},
			Env:        env,
		},
		&container.HostConfig{
			Binds: binds,
		},
		nil, nil, "",
	)
	if err != nil {
		return fmt.Errorf("materialize root-owned: create helper: %w", err)
	}
	defer func() {
		_ = d.cli.ContainerRemove(context.WithoutCancel(ctx), created.ID, container.RemoveOptions{Force: true})
	}()

	if err := d.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("materialize root-owned: start helper: %w", err)
	}

	statusCh, errCh := d.cli.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("materialize root-owned: wait helper: %w", err)
		}
	case status := <-statusCh:
		if status.StatusCode != 0 {
			logs := d.helperLogs(ctx, created.ID)
			return fmt.Errorf("materialize root-owned: helper exited with status %d: %s", status.StatusCode, logs)
		}
	case <-ctx.Done():
		return fmt.Errorf("materialize root-owned: wait helper: %w", ctx.Err())
	}
	return nil
}

func (d *Docker) helperLogs(ctx context.Context, id string) string {
	rc, err := d.cli.ContainerLogs(ctx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return err.Error()
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return err.Error()
	}
	return string(b)
}
