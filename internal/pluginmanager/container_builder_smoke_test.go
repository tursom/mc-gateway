// internal/pluginmanager/container_builder_smoke_test.go provides opt-in Docker smoke coverage for the source builder.

package pluginmanager

import (
	"archive/zip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/tursom/mc-gateway/internal/admindb"
)

func TestContainerBuilderBuildsUpstreamRewriteSourcePackageSmoke(t *testing.T) {
	if os.Getenv("MC_GATEWAY_PLUGIN_CONTAINER_SMOKE") != "1" {
		t.Skip("set MC_GATEWAY_PLUGIN_CONTAINER_SMOKE=1 to run Docker-backed container builder smoke")
	}
	image := os.Getenv("MC_GATEWAY_PLUGIN_CONTAINER_SMOKE_IMAGE")
	if image == "" {
		image = "mc-gateway-plugin-builder:test"
	}
	goVersion := os.Getenv("MC_GATEWAY_PLUGIN_CONTAINER_SMOKE_GO_VERSION")
	if goVersion == "" {
		goVersion = "go1.25.0"
	}
	root := repoRootForSmokeTest(t)
	sourcePath := filepath.Join(root, "examples/plugins/upstream-rewrite/dist/upstream-rewrite-source.mcgp")
	if _, err := os.Stat(sourcePath); err != nil {
		t.Fatalf("source package fixture %s stat error = %v", sourcePath, err)
	}
	patchedSource := filepath.Join(t.TempDir(), "upstream-rewrite-source.mcgp")
	writeSmokeSourcePackageWithGoVersion(t, sourcePath, patchedSource, goVersion)

	db, err := admindb.Open(filepath.Join(t.TempDir(), "gateway.sqlite3"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := admindb.Migrate(db); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	t.Setenv("MC_GATEWAY_PLUGIN_BUILDER_IMAGE", image)
	manager := New(Options{
		DB:            db,
		ArtifactRoot:  t.TempDir(),
		Adapter:       &fakeAdapter{},
		PolicyProfile: PolicyProfileProd,
		Builders: map[string]SourceBuilder{
			BuilderTypeContainer: ContainerBuilder{DefaultImage: image},
		},
	})
	source, err := manager.UploadSource(context.Background(), ArtifactUpload{
		SourcePath: patchedSource,
		FileName:   filepath.Base(patchedSource),
		Actor:      "m3-smoke",
	})
	if err != nil {
		t.Fatalf("UploadSource() error = %v", err)
	}
	builds, err := manager.ListBuilds(context.Background(), source.PluginID)
	if err != nil {
		t.Fatalf("ListBuilds() error = %v", err)
	}
	if len(builds) != 1 || builds[0].BuilderType != BuilderTypeContainer {
		t.Fatalf("builds = %+v, want one container build", builds)
	}
	build, err := manager.RunBuild(context.Background(), "m3-smoke", builds[0].ID)
	if err != nil {
		t.Fatalf("RunBuild() error = %v", err)
	}
	if build.Status != BuildStatusSucceeded || build.ArtifactID == "" || build.GoVersion != goVersion {
		t.Fatalf("build = %+v, want succeeded container build with go %s", build, goVersion)
	}
	metadata := sourceBuildMetadata(build)
	if metadata["go_mod_mode"] != "vendor" {
		t.Fatalf("build metadata = %+v, want go_mod_mode=vendor", metadata)
	}
}

func repoRootForSmokeTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		next := filepath.Dir(dir)
		if next == dir {
			t.Fatal("repo root with go.mod not found")
		}
		dir = next
	}
}

func writeSmokeSourcePackageWithGoVersion(t *testing.T, sourcePath, outPath, goVersion string) {
	t.Helper()
	reader, err := zip.OpenReader(sourcePath)
	if err != nil {
		t.Fatalf("OpenReader(%s) error = %v", sourcePath, err)
	}
	defer reader.Close()
	out, err := os.Create(outPath)
	if err != nil {
		t.Fatalf("Create(%s) error = %v", outPath, err)
	}
	defer out.Close()
	writer := zip.NewWriter(out)
	defer writer.Close()
	for _, file := range reader.File {
		rc, err := file.Open()
		if err != nil {
			t.Fatalf("open zip entry %s: %v", file.Name, err)
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read zip entry %s: %v", file.Name, err)
		}
		if file.Name == "manifest.json" {
			data = smokeManifestWithGoVersion(t, data, goVersion)
		}
		header := &zip.FileHeader{Name: file.Name, Method: zip.Deflate}
		header.SetMode(file.Mode())
		w, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatalf("create zip entry %s: %v", file.Name, err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("write zip entry %s: %v", file.Name, err)
		}
	}
}

func smokeManifestWithGoVersion(t *testing.T, data []byte, goVersion string) []byte {
	t.Helper()
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	manifest["go_version"] = goVersion
	build, _ := manifest["build"].(map[string]any)
	if build == nil {
		build = map[string]any{}
	}
	build["go_version"] = goVersion
	build["vendor_required"] = true
	manifest["build"] = build
	out, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	return append(out, '\n')
}
