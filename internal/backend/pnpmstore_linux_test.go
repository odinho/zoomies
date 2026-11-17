package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/eyupio/zoomies/internal/store"
)

func writeStoreFile(t testing.TB, root, name, contents string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readStoreFile(t testing.TB, root, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func sameStoreFile(t testing.TB, a, b string) bool {
	t.Helper()
	x, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	y, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}
	return os.SameFile(x, y)
}

func TestPNPMWorkspaceLinksContentAndKeepsMetadataPrivate(t *testing.T) {
	shared, work, sibling := t.TempDir(), t.TempDir(), t.TempDir()
	writeStoreFile(t, shared, "pnpm-store/v10/files/ab/content", "package source")
	writeStoreFile(t, shared, "pnpm-store/v10/index/ab/package.json", "original index")
	writeStoreFile(t, shared, "pnpm-store/v10/https+++codeload.github.com/pkg/integrity.json", "git cache")
	for _, dir := range []string{work, sibling} {
		if err := seedPNPMStore(t.Context(), shared, dir); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"files/ab/content", "index/ab/package.json", "https+++codeload.github.com/pkg/integrity.json"} {
		a := filepath.Join(shared, "pnpm-store/v10", name)
		b := filepath.Join(work, ".pnpm-store/v10", name)
		if got, want := sameStoreFile(t, a, b), name == "files/ab/content"; got != want {
			t.Errorf("%s shares inode = %t, want %t", name, got, want)
		}
	}
	writeStoreFile(t, work, ".pnpm-store/v10/index/ab/package.json", "private edit")
	if got := readStoreFile(t, shared, "pnpm-store/v10/index/ab/package.json"); got != "original index" {
		t.Fatalf("shared metadata changed: %q", got)
	}
	if got := readStoreFile(t, sibling, ".pnpm-store/v10/index/ab/package.json"); got != "original index" {
		t.Fatalf("sibling metadata changed: %q", got)
	}
	writeStoreFile(t, work, ".pnpm-store/v10/files/cd/new", "new package")
	writeStoreFile(t, work, ".pnpm-store/v10/index/cd/new.json", "new metadata")
	if err := keepPNPMStore(t.Context(), work, shared); err != nil {
		t.Fatal(err)
	}
	if !sameStoreFile(t, filepath.Join(work, ".pnpm-store/v10/files/cd/new"), filepath.Join(shared, "pnpm-store/v10/files/cd/new")) {
		t.Fatal("new package content was copied instead of linked")
	}
	if got := readStoreFile(t, shared, "pnpm-store/v10/index/cd/new.json"); got != "new metadata" {
		t.Fatalf("new metadata = %q", got)
	}
	if got := readStoreFile(t, shared, "pnpm-store/v10/index/ab/package.json"); got != "original index" {
		t.Fatalf("stale private metadata replaced the shared entry: %q", got)
	}
	if err := os.RemoveAll(work); err != nil {
		t.Fatal(err)
	}
	if got := readStoreFile(t, shared, "pnpm-store/v10/files/cd/new"); got != "new package" {
		t.Fatalf("cleanup lost the newly cached package: %q", got)
	}
}

func TestPNPMStorePreservesInternalLinksAndRejectsEscapes(t *testing.T) {
	for _, link := range []string{"../files/ab/content", "../../../../outside", "/etc/passwd"} {
		t.Run(link, func(t *testing.T) {
			shared, work := t.TempDir(), t.TempDir()
			writeStoreFile(t, shared, "pnpm-store/v10/files/ab/content", "source")
			writeStoreFile(t, shared, "pnpm-store/v10/git-cache/placeholder", "metadata")
			if err := os.Symlink(link, filepath.Join(shared, "pnpm-store/v10/git-cache/link")); err != nil {
				t.Fatal(err)
			}
			err := seedPNPMStore(t.Context(), shared, work)
			if link != "../files/ab/content" {
				if err == nil {
					t.Fatal("accepted a link outside the package store")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := readStoreFile(t, work, ".pnpm-store/v10/git-cache/link"); got != "source" {
				t.Fatalf("internal link resolves to %q", got)
			}
		})
	}
}

func TestPNPMStoreDoesNotPublishAReplacedStoreDirectory(t *testing.T) {
	for _, name := range []string{".pnpm-store", ".pnpm-store/v10"} {
		t.Run(name, func(t *testing.T) {
			shared, work := t.TempDir(), t.TempDir()
			writeStoreFile(t, shared, "pnpm-store/v10/files/ab/content", "source")
			if err := seedPNPMStore(t.Context(), shared, work); err != nil {
				t.Fatal(err)
			}
			writeStoreFile(t, work, "credentials/token", "private")
			if err := os.RemoveAll(filepath.Join(work, name)); err != nil {
				t.Fatal(err)
			}
			target := "credentials"
			if name == ".pnpm-store/v10" {
				target = "../credentials"
			}
			if err := os.Symlink(target, filepath.Join(work, name)); err != nil {
				t.Fatal(err)
			}
			if err := keepPNPMStore(t.Context(), work, shared); err == nil {
				t.Fatal("accepted the replaced store directory")
			}
			if _, err := os.Stat(filepath.Join(shared, "pnpm-store/v10/token")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("private token reached shared store: %v", err)
			}
		})
	}
}

func TestPNPMStoreHonorsCancellation(t *testing.T) {
	shared, work := t.TempDir(), t.TempDir()
	writeStoreFile(t, shared, "pnpm-store/v10/files/ab/content", "source")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := seedPNPMStore(ctx, shared, work); !errors.Is(err, context.Canceled) {
		t.Fatalf("seed error = %v, want cancellation", err)
	}
}

func TestPNPMStoreExcludesProjectRegistrationsAndPartialDownloads(t *testing.T) {
	shared, work := t.TempDir(), t.TempDir()
	writeStoreFile(t, shared, "pnpm-store/v10/files/ab/content", "source")
	writeStoreFile(t, shared, "pnpm-store/v10/tmp/partial", "partial download")
	if err := os.Mkdir(filepath.Join(shared, "pnpm-store/v10/projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(shared, "pnpm-store/v10/projects/old")); err != nil {
		t.Fatal(err)
	}
	if err := seedPNPMStore(t.Context(), shared, work); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"projects", "tmp"} {
		if _, err := os.Stat(filepath.Join(work, ".pnpm-store/v10", name)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("seed retained %s: %v", name, err)
		}
	}
	writeStoreFile(t, work, ".pnpm-store/v10/tmp/new", "new partial")
	writeStoreFile(t, work, ".pnpm-store/v10/projects/new", "new project")
	if err := keepPNPMStore(t.Context(), work, shared); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"projects/new", "tmp/new"} {
		if _, err := os.Stat(filepath.Join(shared, "pnpm-store/v10", name)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("promotion retained %s: %v", name, err)
		}
	}
}

func TestPNPMLateContainerMustUseTheCurrentWorkspace(t *testing.T) {
	spec := jitSpec()
	old := buildRunnerConfig(spec, dockerFlavor(), containerOptions{WorkDirMount: "/agent/pnpm-old", WorkDirOwned: true, PNPMWorkspaceStore: true})
	got := echoInspect("old", containerName(spec.Name), old, "")
	if !adoptable(&got, old) {
		t.Fatal("matching private workspace was rejected")
	}
	for _, opts := range []containerOptions{
		{WorkDirMount: "/agent/pnpm-new", WorkDirOwned: true, PNPMWorkspaceStore: true},
		{},
	} {
		if adoptable(&got, buildRunnerConfig(spec, dockerFlavor(), opts)) {
			t.Fatal("a previous attempt's workspace was accepted")
		}
	}
}

func TestPNPMStaleContainerRemovalAlsoRemovesItsWorkspace(t *testing.T) {
	was := runningInContainer
	runningInContainer = func() bool { return false }
	t.Cleanup(func() { runningInContainer = was })
	spec := jitSpec()
	spec.Env[EnvPNPMWorkspaceStore] = "v10"
	spec.Cache = store.CacheConfig{Enabled: true, Scope: store.CacheScopePool, Source: t.TempDir()}
	cache, _ := cacheDirectory(spec)
	writeStoreFile(t, cache, "pnpm-store/v10/files/ab/content", "content")
	var mu sync.Mutex
	oldExists := true
	var oldCfg, newCfg ContainerCreateRequest
	f := newFakeEngine(t, map[string]http.HandlerFunc{
		"GET " + v + "/images/{ref...}": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 200, map[string]string{"Id": "sha256:cached"})
		},
		"GET " + v + "/containers/{id}/json": func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			if !oldExists || (r.PathValue("id") != "old" && r.PathValue("id") != containerName(spec.Name)) {
				w.WriteHeader(404)
				return
			}
			insp := echoInspect("old", containerName(spec.Name), oldCfg, "")
			insp.State = &ContainerState{Status: "exited"}
			writeJSON(w, 200, insp)
		},
		"DELETE " + v + "/containers/old": func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			oldExists = false
			mu.Unlock()
			w.WriteHeader(204)
		},
		"POST " + v + "/containers/create": func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&newCfg); err != nil {
				t.Error(err)
			}
			writeJSON(w, 201, map[string]string{"Id": "new"})
		},
		"POST " + v + "/containers/new/start": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) },
	})
	b := dockerBackendFor(t, f, DockerOptions{WorkDir: t.TempDir(), PullPolicy: PullNever})
	oldSpec, oldDir := b.preparePNPMWorkspace(t.Context(), spec)
	if oldDir == "" {
		t.Fatal("fixture did not seed a workspace")
	}
	oldCfg = buildRunnerConfig(oldSpec, b.fl, containerOptions{WorkDirMount: oldDir, WorkDirOwned: true, PNPMWorkspaceStore: true})
	writeStoreFile(t, oldDir, "repo/token", "job credential")
	if _, err := b.CreateWithResult(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if newCfg.Labels[LabelWorkDir] == oldDir {
		t.Fatal("replacement reused the old workspace")
	}
	if _, err := os.Stat(oldDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("old workspace survived replacement: %v", err)
	}
}

func TestPNPMWorkspaceMountHasTheStoreAndNormalWorkPathsTogether(t *testing.T) {
	spec := Spec{Name: "r1", Env: map[string]string{"pnpm_config_store_dir": RunnerWorkMount + "/.pnpm-store"}}
	cfg := buildRunnerConfig(spec, dockerFlavor(), containerOptions{WorkDirMount: "/srv/zoomies/pnpm-one", WorkDirOwned: true, PNPMWorkspaceStore: true})
	if !slices.Contains(cfg.HostConfig.Binds, "/srv/zoomies/pnpm-one:"+RunnerWorkMount) {
		t.Fatalf("workspace bind missing: %v", cfg.HostConfig.Binds)
	}
	if cfg.Labels[labelPNPMWorkspaceStore] != "v10" || cfg.Labels[LabelWorkDir] != "/srv/zoomies/pnpm-one" {
		t.Fatalf("cleanup labels = %v", cfg.Labels)
	}
	if !slices.Contains(cfg.Env, "pnpm_config_store_dir="+RunnerWorkMount+"/.pnpm-store") {
		t.Fatal("package store is not inside the ordinary workspace mount")
	}
}

func TestPNPMWorkspaceOptInPreservesTheOriginalSpecAndFallsBackOnAMiss(t *testing.T) {
	was := runningInContainer
	runningInContainer = func() bool { return false }
	t.Cleanup(func() { runningInContainer = was })
	spec := jitSpec()
	spec.Ephemeral, spec.DockerMode = true, store.DockerNone
	spec.Cache = store.CacheConfig{Enabled: true, Scope: store.CacheScopePool, Source: t.TempDir()}
	spec.Env = map[string]string{EnvPNPMWorkspaceStore: "v10", "pnpm_config_store_dir": "original"}
	dir, ok := cacheDirectory(spec)
	if !ok {
		t.Fatal("fixture needs an absolute host cache")
	}
	b := &DockerBackend{workDir: t.TempDir(), log: quietLogger()}
	if _, got := b.preparePNPMWorkspace(t.Context(), spec); got != "" {
		t.Fatalf("a cold store created workspace %q", got)
	}
	if entries, err := os.ReadDir(b.workDir); err != nil || len(entries) != 0 {
		t.Fatalf("failed seed left scratch: %v, %v", entries, err)
	}
	writeStoreFile(t, dir, "pnpm-store/v10/files/ab/content", "source")
	updated, work := b.preparePNPMWorkspace(t.Context(), spec)
	if work == "" {
		t.Fatal("warm cache did not create a private workspace")
	}
	if updated.Env["pnpm_config_store_dir"] != RunnerWorkMount+"/.pnpm-store" || spec.Env["pnpm_config_store_dir"] != "original" {
		t.Fatalf("updated/original store paths = %q/%q", updated.Env["pnpm_config_store_dir"], spec.Env["pnpm_config_store_dir"])
	}
	for _, change := range []func(*Spec){
		func(s *Spec) { delete(s.Env, EnvPNPMWorkspaceStore) },
		func(s *Spec) { s.WorkDir = t.TempDir() },
		func(s *Spec) { s.DockerMode = store.DockerDinD },
		func(s *Spec) { s.Ephemeral = false },
		func(s *Spec) { s.Tmpfs.Work.Enabled = true },
	} {
		copy := spec
		copy.Env = map[string]string{EnvPNPMWorkspaceStore: "v10"}
		change(&copy)
		if _, got := b.preparePNPMWorkspace(t.Context(), copy); got != "" {
			t.Fatalf("unsupported layout created workspace %q", got)
		}
	}
}

func BenchmarkPNPMWorkspace(b *testing.B) {
	shared := b.TempDir()
	for i := range 64 {
		writeStoreFile(b, shared, filepath.Join("pnpm-store/v10/files", fmt.Sprintf("%02x/content", i)), strings.Repeat("source", 8192))
	}
	writeStoreFile(b, shared, "pnpm-store/v10/index/ab/package.json", "metadata")
	for _, linked := range []bool{false, true} {
		name := "Copy"
		if linked {
			name = "Hardlink"
		}
		b.Run(name, func(b *testing.B) {
			parent := b.TempDir()
			b.ResetTimer()
			for range b.N {
				work, err := os.MkdirTemp(parent, "pnpm-")
				if err != nil {
					b.Fatal(err)
				}
				if linked {
					err = seedPNPMStore(b.Context(), shared, work)
				} else {
					err = os.CopyFS(filepath.Join(work, ".pnpm-store/v10"), os.DirFS(filepath.Join(shared, "pnpm-store/v10")))
				}
				if err != nil {
					b.Fatal(err)
				}
				x := filepath.Join(shared, "pnpm-store/v10/files/00/content")
				y := filepath.Join(work, ".pnpm-store/v10/files/00/content")
				if sameStoreFile(b, x, y) != linked {
					b.Fatal("benchmark did not use the requested import method")
				}
				if linked {
					if err := keepPNPMStore(b.Context(), work, shared); err != nil {
						b.Fatal(err)
					}
				}
				if err := os.RemoveAll(work); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
