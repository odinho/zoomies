package backend

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/eyupio/zoomies/internal/store"
)

// EnvPNPMWorkspaceStore opts a pool into a private v10 store beside its work.
// A store mounted separately cannot hardlink to the workspace, even when both
// mounts come from the same filesystem.
const EnvPNPMWorkspaceStore = "ZOOMIES_PNPM_WORKSPACE_STORE"

const labelPNPMWorkspaceStore = LabelPrefix + "pnpm-workspace-store"
const pnpmWorkspaceStore = ".pnpm-store"

// preparePNPMWorkspace leaves the existing layout in place on a cache miss.
// Only an ephemeral runner's new, owned directory is ever used as workspace.
func (b *DockerBackend) preparePNPMWorkspace(ctx context.Context, spec Spec) (Spec, string) {
	if spec.Env[EnvPNPMWorkspaceStore] != "v10" {
		return spec, ""
	}
	cacheDir, hostCache := cacheDirectory(spec)
	if runtime.GOOS != "linux" || runningInContainer() || !hostCache || !filepath.IsAbs(b.workDir) ||
		!spec.Ephemeral || spec.WorkDir != "" || spec.DockerMode != store.DockerNone || spec.Tmpfs.Work.Enabled {
		b.log.Warn("PNPM workspace store needs a host-run Linux agent, a host directory cache and an ephemeral runner without a work mount or work tmpfs; using the existing package store", "runner", spec.Name)
		return spec, ""
	}
	if err := os.MkdirAll(b.workDir, 0o750); err != nil {
		b.log.Warn("could not prepare the PNPM workspace; using the existing package store", "runner", spec.Name, "error", err)
		return spec, ""
	}
	dir, err := os.MkdirTemp(b.workDir, "pnpm-")
	if err != nil {
		b.log.Warn("could not create the PNPM workspace; using the existing package store", "runner", spec.Name, "error", err)
		return spec, ""
	}
	started := time.Now()
	err = seedPNPMStore(ctx, cacheDir, dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		b.log.Warn("could not seed the PNPM workspace; using the existing package store", "runner", spec.Name, "error", err)
		return spec, ""
	}
	spec.Env = maps.Clone(spec.Env)
	for _, key := range []string{"npm_config_store_dir", "pnpm_config_store_dir"} {
		spec.Env[key] = RunnerWorkMount + "/" + pnpmWorkspaceStore
	}
	b.log.Debug("seeded a private PNPM workspace store", "runner", spec.Name, "duration", time.Since(started))
	return spec, dir
}

func seedPNPMStore(ctx context.Context, cacheDir, workDir string) error {
	shared, err := os.OpenRoot(cacheDir)
	if err != nil {
		return err
	}
	defer shared.Close()
	source, err := shared.OpenRoot("pnpm-store/v10")
	if err != nil {
		return err
	}
	defer source.Close()
	if err := os.Chmod(workDir, 0o777); err != nil {
		return err
	}
	work, err := os.OpenRoot(workDir)
	if err != nil {
		return err
	}
	defer work.Close()
	for _, name := range []string{pnpmWorkspaceStore, pnpmWorkspaceStore + "/v10"} {
		if err := mkdirPNPM(work, name); err != nil {
			return err
		}
	}
	target, err := work.OpenRoot(pnpmWorkspaceStore + "/v10")
	if err != nil {
		return err
	}
	defer target.Close()
	return syncPNPMStore(ctx, source, target)
}

// keepPNPMStore runs only after the container is stopped. New immutable entries
// warm the shared store; existing metadata is not replaced by a stale snapshot.
func keepPNPMStore(ctx context.Context, workDir, cacheDir string) error {
	work, err := os.OpenRoot(workDir)
	if err != nil {
		return err
	}
	defer work.Close()
	// The stopped job may have replaced either directory with a symlink.
	for _, name := range []string{pnpmWorkspaceStore, pnpmWorkspaceStore + "/v10"} {
		info, err := work.Lstat(name)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("package store path %q is not a directory", name)
		}
	}
	source, err := work.OpenRoot(pnpmWorkspaceStore + "/v10")
	if err != nil {
		return err
	}
	defer source.Close()
	shared, err := os.OpenRoot(cacheDir)
	if err != nil {
		return err
	}
	defer shared.Close()
	target, err := shared.OpenRoot("pnpm-store/v10")
	if err != nil {
		return err
	}
	defer target.Close()
	return syncPNPMStore(ctx, source, target)
}

func mkdirPNPM(root *os.Root, name string) error {
	if err := root.Mkdir(name, 0o777); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return err
	}
	return root.Chmod(name, 0o777)
}

func syncPNPMStore(ctx context.Context, source, target *os.Root) error {
	return fs.WalkDir(source.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		// Project registrations point to workspaces outside the store. Partial
		// downloads and project registrations are not reusable cache entries.
		if name == "projects" || name == "tmp" {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return mkdirPNPM(target, name)
		}
		if _, err := target.Lstat(name); err == nil {
			return nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			link, err := source.Readlink(name)
			if err != nil {
				return err
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(name), link))
			if filepath.IsAbs(link) || resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
				return fmt.Errorf("package store link %q leaves the store", name)
			}
			if err := target.Symlink(link, name); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("package store entry %q is not a regular file", name)
		}
		in, err := source.Open(name)
		if err != nil {
			return err
		}
		defer in.Close()
		info, err := in.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("package store entry %q is not a regular file", name)
		}
		if strings.HasPrefix(name, "files/") {
			return linkPNPMFile(in, target, name)
		}
		return copyPNPMMetadata(in, target, name)
	})
}

func copyPNPMMetadata(in *os.File, target *os.Root, name string) error {
	info, err := in.Stat()
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(name), ".pnpm-"+rand.Text())
	out, err := target.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer target.Remove(tmp)
	_, copyErr := io.Copy(out, in)
	modeErr := out.Chmod(info.Mode().Perm() | 0o666)
	closeErr := out.Close()
	if err := errors.Join(copyErr, modeErr, closeErr); err != nil {
		return err
	}
	if err := target.Link(tmp, name); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}
