// internal/pluginmanager/artifact_test.go 包含用于约束 artifact 行为的测试。

package pluginmanager

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestArtifactStoreValidateAndStore(t *testing.T) {
	packagePath := writeTestMCGP(t, map[string][]byte{
		"manifest.json": testManifestBytes(t, "test-plugin"),
		"plugin.so":     []byte("fake plugin bytes"),
	})
	store := NewArtifactStore(t.TempDir())
	artifact, err := store.ValidateAndStore(ArtifactUpload{
		SourcePath: packagePath,
		FileName:   "test-plugin.mcgp",
		Actor:      "admin",
	})
	if err != nil {
		t.Fatalf("ValidateAndStore() error = %v", err)
	}
	if artifact.PluginID != "test-plugin" || artifact.Status != ArtifactStatusLoadable {
		t.Fatalf("artifact = %+v, want loadable test-plugin", artifact)
	}
	if artifact.SHA256 == "" || artifact.PackageSHA256 == "" {
		t.Fatalf("artifact hashes not set: %+v", artifact)
	}
	if _, err := os.Stat(artifact.FilePath); err != nil {
		t.Fatalf("stored runtime entry stat error = %v", err)
	}
	if !strings.HasSuffix(artifact.FilePath, filepath.Join("test-plugin", artifact.ID, "plugin.so")) {
		t.Fatalf("artifact file path = %q", artifact.FilePath)
	}
}

func TestArtifactStoreValidateAndStoreSource(t *testing.T) {
	packagePath := writeTestMCGP(t, map[string][]byte{
		"manifest.json": testSourceManifestBytes(t, "source-plugin"),
		"go.mod":        []byte("module example.com/source-plugin\n\ngo 1.24.0\n"),
		"main.go":       []byte("package main\n"),
		"README.md":     []byte("source fixture"),
	})
	store := NewArtifactStore(t.TempDir())
	source, err := store.ValidateAndStoreSource(ArtifactUpload{
		SourcePath: packagePath,
		FileName:   "source-plugin.mcgp",
		Actor:      "admin",
	})
	if err != nil {
		t.Fatalf("ValidateAndStoreSource() error = %v", err)
	}
	if source.ArtifactType != ArtifactTypeSource || source.Status != ArtifactStatusValidated {
		t.Fatalf("source = %+v, want validated source", source)
	}
	if _, err := os.Stat(filepath.Join(source.FilePath, "go.mod")); err != nil {
		t.Fatalf("stored source go.mod stat error = %v", err)
	}
}

func TestArtifactStoreRejectsUnsafePackage(t *testing.T) {
	tests := []struct {
		name    string
		entries map[string][]byte
		want    string
	}{
		{
			name: "zip slip",
			entries: map[string][]byte{
				"manifest.json": testManifestBytes(t, "test-plugin"),
				"../plugin.so":  []byte("fake"),
			},
			want: "unsafe zip entry",
		},
		{
			name: "normalized escape",
			entries: map[string][]byte{
				"manifest.json":       testManifestBytes(t, "test-plugin"),
				"nested/../plugin.so": []byte("fake"),
			},
			want: "unsafe zip entry",
		},
		{
			name: "missing manifest",
			entries: map[string][]byte{
				"plugin.so": []byte("fake"),
			},
			want: "manifest.json is required",
		},
		{
			name: "missing runtime",
			entries: map[string][]byte{
				"manifest.json": testManifestBytes(t, "test-plugin"),
			},
			want: `runtime entry "plugin.so" is required`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewArtifactStore(t.TempDir())
			_, err := store.ValidateAndStore(ArtifactUpload{
				SourcePath: writeTestMCGP(t, tt.entries),
				FileName:   "bad.mcgp",
				Actor:      "admin",
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateAndStore() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestArtifactStoreRejectsSourceShellScripts(t *testing.T) {
	store := NewArtifactStore(t.TempDir())
	_, err := store.ValidateAndStoreSource(ArtifactUpload{
		SourcePath: writeTestMCGP(t, map[string][]byte{
			"manifest.json": testSourceManifestBytes(t, "test-plugin"),
			"go.mod":        []byte("module example.com/test\n"),
			"main.go":       []byte("package main\n"),
			"build.sh":      []byte("go build"),
		}),
		FileName: "bad-source.mcgp",
		Actor:    "admin",
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported source package entry") {
		t.Fatalf("ValidateAndStoreSource() error = %v, want unsupported source entry", err)
	}
}

func testSourceManifestBytes(t *testing.T, pluginID string) []byte {
	t.Helper()
	manifest := Manifest{
		SchemaVersion: SchemaVersion,
		ID:            pluginID,
		Name:          "Source Plugin",
		Version:       "0.1.0",
		ArtifactType:  ArtifactTypeSource,
		Runtime: RuntimeManifest{
			Type:        RuntimeGoPlugin,
			EntrySymbol: "Plugin",
		},
		Build: BuildManifest{
			Type:           BuildTypeGo,
			Entry:          ".",
			GoVersion:      runtime.Version(),
			Tags:           []string{},
			VendorRequired: false,
			Output:         RuntimeEntry,
		},
		APIVersion: APIVersion,
		GoVersion:  runtime.Version(),
		GOOS:       runtime.GOOS,
		GOARCH:     runtime.GOARCH,
		ExtensionPoints: []ExtensionPoint{{
			Type: "hook",
			Key:  ExtensionUpstreamConnect,
		}},
		Capabilities: json.RawMessage(`{"extension_points":["upstream.connect/v1"]}`),
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal source manifest error = %v", err)
	}
	return data
}

func testManifestBytes(t *testing.T, pluginID string) []byte {
	return testManifestBytesWithCapabilities(t, pluginID, json.RawMessage(`{"extension_points":["upstream.connect/v1"]}`))
}

func testManifestBytesWithCapabilities(t *testing.T, pluginID string, capabilities json.RawMessage) []byte {
	t.Helper()
	if len(capabilities) == 0 {
		capabilities = json.RawMessage(`{"extension_points":["upstream.connect/v1"]}`)
	}
	manifest := Manifest{
		SchemaVersion: SchemaVersion,
		ID:            pluginID,
		Name:          "Test Plugin",
		Version:       "0.1.0",
		ArtifactType:  ArtifactTypeBinary,
		Runtime: RuntimeManifest{
			Type:        RuntimeGoPlugin,
			Entry:       RuntimeEntry,
			EntrySymbol: "Plugin",
		},
		APIVersion: APIVersion,
		GoVersion:  runtime.Version(),
		GOOS:       runtime.GOOS,
		GOARCH:     runtime.GOARCH,
		ExtensionPoints: []ExtensionPoint{{
			Type: "hook",
			Key:  ExtensionUpstreamConnect,
		}},
		Capabilities:  capabilities,
		ConfigSchema:  json.RawMessage(`{"type":"object"}`),
		RuntimeLimits: RuntimeLimits{HandlerTimeoutMS: 3000},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal manifest error = %v", err)
	}
	return data
}

func writeTestMCGP(t *testing.T, entries map[string][]byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plugin.mcgp")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create package error = %v", err)
	}
	zipWriter := zip.NewWriter(file)
	for name, data := range entries {
		writer, err := zipWriter.Create(name)
		if err != nil {
			t.Fatalf("Create zip entry error = %v", err)
		}
		if _, err := writer.Write(data); err != nil {
			t.Fatalf("Write zip entry error = %v", err)
		}
	}
	if err := zipWriter.Close(); err != nil {
		t.Fatalf("Close zip error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close package error = %v", err)
	}
	return path
}
