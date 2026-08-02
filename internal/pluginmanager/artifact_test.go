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
	"time"
)

func TestManifestRejectsRemovedUpstreamConnectContracts(t *testing.T) {
	for _, key := range []string{"upstream.connect/v1", "upstream"} {
		t.Run(key, func(t *testing.T) {
			var manifest Manifest
			if err := json.Unmarshal(testManifestBytes(t, "legacy-contract"), &manifest); err != nil {
				t.Fatal(err)
			}
			manifest.ExtensionPoints[0].Key = key
			if err := validateManifest(manifest); err == nil || !strings.Contains(err.Error(), "was removed") {
				t.Fatalf("validateManifest(%q) error = %v, want explicit removal error", key, err)
			}
		})
	}
	if _, err := capabilitiesSummaryJSON(json.RawMessage(`{"extension_points":["upstream.connect/v2"],"upstream_connect":{"mode":"dialer"}}`)); err == nil || !strings.Contains(err.Error(), "was removed") {
		t.Fatalf("legacy capabilities error = %v, want explicit removal error", err)
	}
	for _, key := range []string{"upstream.connect/v1", "upstream"} {
		var manifest Manifest
		if err := json.Unmarshal(testManifestBytes(t, "legacy-capability"), &manifest); err != nil {
			t.Fatal(err)
		}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["` + key + `"]}`)
		if err := validateManifest(manifest); err == nil || !strings.Contains(err.Error(), "was removed") {
			t.Fatalf("validateManifest(capabilities %q) error = %v, want explicit removal error", key, err)
		}
	}
}

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

func TestArtifactStoreStoresConformanceSummary(t *testing.T) {
	packagePath := writeTestMCGP(t, map[string][]byte{
		"manifest.json": testManifestBytes(t, "conformance-plugin"),
		"plugin.so":     []byte("fake plugin bytes"),
		"conformance.json": []byte(`{
			"fixtures":[
				{"name":"contract","status":"pass"},
				{"name":"optional","status":"skip"},
				{"name":"takeover.panic","status":"fail","extension":"upstream.connect/v2","expected":"panic_recovered"}
			]
		}`),
	})
	store := NewArtifactStore(t.TempDir())
	artifact, err := store.ValidateAndStore(ArtifactUpload{
		SourcePath: packagePath,
		FileName:   "conformance-plugin.mcgp",
		Actor:      "admin",
	})
	if err != nil {
		t.Fatalf("ValidateAndStore() error = %v", err)
	}
	var metadata struct {
		ID          string             `json:"id"`
		Conformance ConformanceSummary `json:"conformance"`
	}
	if err := json.Unmarshal([]byte(artifact.MetadataJSON), &metadata); err != nil {
		t.Fatalf("Unmarshal metadata error = %v\n%s", err, artifact.MetadataJSON)
	}
	if metadata.ID != "conformance-plugin" {
		t.Fatalf("metadata id = %q, want conformance-plugin", metadata.ID)
	}
	if metadata.Conformance.OK ||
		metadata.Conformance.Total != 3 ||
		metadata.Conformance.Passed != 1 ||
		metadata.Conformance.Skipped != 1 ||
		metadata.Conformance.Failed != 1 ||
		len(metadata.Conformance.FailedFixtures) != 1 ||
		metadata.Conformance.FailedFixtures[0].Name != "takeover.panic" {
		t.Fatalf("conformance summary = %+v, want one failed fixture", metadata.Conformance)
	}
}

func TestArtifactStoreMergesProvenanceMetadata(t *testing.T) {
	packagePath := writeTestMCGP(t, map[string][]byte{
		"manifest.json": testManifestBytes(t, "provenance-plugin"),
		"plugin.so":     []byte("fake plugin bytes"),
		"provenance.json": []byte(`{
			"signature":{"verified":true},
			"sbom":{"required":true,"scan_ok":true},
			"external_ci":{
				"required":true,
				"trusted":true,
				"source_sha256":"src-sha",
				"artifact_sha256":"artifact-sha",
				"run_id":"github-actions/run-1",
				"builder_id":"builder",
				"attestation":"attested",
				"sbom":"sbom.spdx.json",
				"release_provenance":"release.json"
			}
		}`),
	})
	store := NewArtifactStore(t.TempDir())
	artifact, err := store.ValidateAndStore(ArtifactUpload{
		SourcePath: packagePath,
		FileName:   "provenance-plugin.mcgp",
		Actor:      "admin",
	})
	if err != nil {
		t.Fatalf("ValidateAndStore() error = %v", err)
	}
	metadata := jsonMap(artifact.MetadataJSON)
	externalCI := jsonMapFromAny(metadata["external_ci"])
	signature := jsonMapFromAny(metadata["signature"])
	sbom := jsonMapFromAny(metadata["sbom"])
	if externalCI == nil ||
		externalCI["run_id"] != "github-actions/run-1" ||
		signature["verified"] != true ||
		sbom["scan_ok"] != true {
		t.Fatalf("metadata = %+v, want merged provenance.json external_ci, signature and SBOM", metadata)
	}
}

func TestArtifactStoreRejectsWASMMissingOrMismatchedRuntimeABI(t *testing.T) {
	tests := []struct {
		name string
		abi  string
	}{
		{name: "missing runtime abi"},
		{name: "mismatched runtime abi", abi: "mc-gateway.wasm.host/v0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewArtifactStore(t.TempDir())
			_, err := store.ValidateAndStore(ArtifactUpload{
				SourcePath: writeTestMCGP(t, map[string][]byte{
					"manifest.json":  testWASMManifestBytes(t, "wasm-abi-plugin", tt.abi),
					RuntimeWASMEntry: wasmOKModule,
				}),
				FileName: "wasm-abi-plugin.mcgp",
				Actor:    "admin",
			})
			if err == nil || !strings.Contains(err.Error(), "unsupported runtime.abi") {
				t.Fatalf("ValidateAndStore(%s) error = %v, want unsupported runtime.abi", tt.name, err)
			}
		})
	}
}

func TestArtifactStoreRejectsWASMMissingEntryAndExport(t *testing.T) {
	store := NewArtifactStore(t.TempDir())
	_, err := store.ValidateAndStore(ArtifactUpload{
		SourcePath: writeTestMCGP(t, map[string][]byte{
			"manifest.json": testWASMManifestBytes(t, "wasm-missing-entry", wasmHostABIV1),
			RuntimeEntry:    wasmOKModule,
		}),
		FileName: "wasm-missing-entry.mcgp",
		Actor:    "admin",
	})
	if err == nil || !strings.Contains(err.Error(), `runtime entry "plugin.wasm" is required`) {
		t.Fatalf("ValidateAndStore(missing wasm entry) error = %v, want plugin.wasm required", err)
	}

	store = NewArtifactStore(t.TempDir())
	_, err = store.ValidateAndStore(ArtifactUpload{
		SourcePath: writeTestMCGP(t, map[string][]byte{
			"manifest.json":  testWASMManifestBytes(t, "wasm-missing-export", wasmHostABIV1),
			RuntimeWASMEntry: wasmLegacyValidateModule(),
		}),
		FileName: "wasm-missing-export.mcgp",
		Actor:    "admin",
	})
	if err == nil || !strings.Contains(err.Error(), "required export") {
		t.Fatalf("ValidateAndStore(missing wasm export) error = %v, want required export block", err)
	}
}

func TestArtifactStoreStoresWASMMetadata(t *testing.T) {
	packagePath := writeTestMCGP(t, map[string][]byte{
		"manifest.json":  testWASMManifestBytes(t, "wasm-metadata", wasmHostABIV1),
		RuntimeWASMEntry: wasmOKModule,
	})
	store := NewArtifactStore(t.TempDir())
	artifact, err := store.ValidateAndStore(ArtifactUpload{
		SourcePath: packagePath,
		FileName:   "wasm-metadata.mcgp",
		Actor:      "admin",
	})
	if err != nil {
		t.Fatalf("ValidateAndStore(wasm metadata) error = %v", err)
	}
	if !strings.HasSuffix(artifact.FilePath, filepath.Join("wasm-metadata", artifact.ID, RuntimeWASMEntry)) {
		t.Fatalf("artifact file path = %q, want plugin.wasm path", artifact.FilePath)
	}
	metadata := jsonMap(artifact.MetadataJSON)
	wasm := jsonMapFromAny(metadata["wasm"])
	if wasm == nil ||
		wasm["abi"] != wasmHostABIV1 ||
		wasm["entry"] != RuntimeWASMEntry ||
		wasm["module_sha256"] != artifact.SHA256 ||
		wasm["artifact_sha256"] != artifact.SHA256 ||
		wasm["package_sha256"] != artifact.PackageSHA256 {
		t.Fatalf("wasm metadata = %+v artifact=%+v, want ABI/module/package hashes", wasm, artifact)
	}
	exports, ok := wasm["required_exports"].([]any)
	if !ok || len(exports) != 1 || exports[0] != wasmExportRuleEvaluateV1 {
		t.Fatalf("required_exports = %#v, want rule export", wasm["required_exports"])
	}
	limits := jsonMapFromAny(wasm["limits"])
	if limits["memory_bytes"] != float64(64*1024) || limits["handler_timeout_ms"] != float64(3000) {
		t.Fatalf("wasm limits = %+v, want manifest limits", limits)
	}
}

func TestArtifactStoreStoresSandboxMetadata(t *testing.T) {
	packagePath := writeTestMCGPWithModes(t, map[string][]byte{
		"manifest.json": testSandboxManifestBytes(t, "sandbox-metadata", runtime.GOOS, runtime.GOARCH, SandboxProcessABIVersionV1),
		"bin/plugin":    []byte("sandbox native executable bytes"),
	}, map[string]os.FileMode{"bin/plugin": 0755})
	store := NewArtifactStore(t.TempDir())
	artifact, err := store.ValidateAndStore(ArtifactUpload{
		SourcePath: packagePath,
		FileName:   "sandbox-metadata.mcgp",
		Actor:      "admin",
	})
	if err != nil {
		t.Fatalf("ValidateAndStore(sandbox metadata) error = %v", err)
	}
	if artifact.RuntimeType != RuntimeSandbox || artifact.RuntimeEntry != "bin/plugin" {
		t.Fatalf("artifact runtime = %s entry = %s, want sandbox bin/plugin", artifact.RuntimeType, artifact.RuntimeEntry)
	}
	if artifact.GOOS != runtime.GOOS || artifact.GOARCH != runtime.GOARCH {
		t.Fatalf("artifact target = %s/%s, want %s/%s", artifact.GOOS, artifact.GOARCH, runtime.GOOS, runtime.GOARCH)
	}
	metadata := jsonMap(artifact.MetadataJSON)
	sandbox := jsonMapFromAny(metadata["sandbox"])
	if sandbox == nil ||
		sandbox["type"] != RuntimeSandbox ||
		sandbox["protocol"] != SandboxProcessProtocolV1 ||
		sandbox["entry"] != "bin/plugin" ||
		sandbox["entry_sha256"] != artifact.SHA256 ||
		sandbox["artifact_sha256"] != artifact.SHA256 ||
		sandbox["package_sha256"] != artifact.PackageSHA256 ||
		sandbox["os"] != runtime.GOOS ||
		sandbox["arch"] != runtime.GOARCH ||
		sandbox["abi_version"] != SandboxProcessABIVersionV1 {
		t.Fatalf("sandbox metadata = %+v artifact=%+v, want runtime target and hashes", sandbox, artifact)
	}
}

func TestArtifactStoreStoresSandboxConformanceCoverage(t *testing.T) {
	packagePath := writeTestMCGPWithModes(t, map[string][]byte{
		"manifest.json":    testSandboxManifestBytes(t, "sandbox-conformance", runtime.GOOS, runtime.GOARCH, SandboxProcessABIVersionV1),
		"bin/plugin":       []byte("sandbox native executable bytes"),
		"conformance.json": sandboxConformanceFixtureBytesForTest(t),
	}, map[string]os.FileMode{"bin/plugin": 0755})
	store := NewArtifactStore(t.TempDir())
	artifact, err := store.ValidateAndStore(ArtifactUpload{
		SourcePath: packagePath,
		FileName:   "sandbox-conformance.mcgp",
		Actor:      "admin",
	})
	if err != nil {
		t.Fatalf("ValidateAndStore(sandbox conformance) error = %v", err)
	}
	var metadata struct {
		Conformance ConformanceSummary `json:"conformance"`
	}
	if err := json.Unmarshal([]byte(artifact.MetadataJSON), &metadata); err != nil {
		t.Fatalf("Unmarshal metadata error = %v\n%s", err, artifact.MetadataJSON)
	}
	coverage := map[string]bool{}
	for _, item := range metadata.Conformance.Coverage {
		coverage[item] = true
	}
	for _, item := range SandboxConformanceRequiredCoverage() {
		if !coverage[item] {
			t.Fatalf("conformance coverage = %+v, missing %s", metadata.Conformance.Coverage, item)
		}
	}
	if !metadata.Conformance.OK || metadata.Conformance.Failed != 0 {
		t.Fatalf("conformance summary = %+v, want passing sandbox coverage", metadata.Conformance)
	}
}

func TestArtifactStoreDoesNotCountRawSandboxConformanceDeclarations(t *testing.T) {
	data, err := json.Marshal(map[string]any{
		"fixtures": []map[string]any{
			{"name": "contract", "status": "pass"},
			{"name": "sandbox.self_declared", "status": "pass", "coverage": SandboxConformanceRequiredCoverage()},
		},
		"sandbox_fixtures":       SandboxConformanceRequiredCoverage(),
		"stream_proxy_scenarios": []StreamProxyFixture{{Name: "cancel", Protocol: StreamProxyProtocolV1, Expected: "cancel", Frames: []StreamProxyFrame{{Type: StreamFrameCancel}}}},
		"takeover_scenarios":     []string{"endpoint_close", "backpressure_large_packet"},
	})
	if err != nil {
		t.Fatalf("Marshal raw sandbox conformance declarations error = %v", err)
	}
	packagePath := writeTestMCGPWithModes(t, map[string][]byte{
		"manifest.json":    testSandboxManifestBytes(t, "sandbox-raw-conformance", runtime.GOOS, runtime.GOARCH, SandboxProcessABIVersionV1),
		"bin/plugin":       []byte("sandbox native executable bytes"),
		"conformance.json": data,
	}, map[string]os.FileMode{"bin/plugin": 0755})
	store := NewArtifactStore(t.TempDir())
	artifact, err := store.ValidateAndStore(ArtifactUpload{
		SourcePath: packagePath,
		FileName:   "sandbox-raw-conformance.mcgp",
		Actor:      "admin",
	})
	if err != nil {
		t.Fatalf("ValidateAndStore(raw sandbox conformance) error = %v", err)
	}
	var metadata struct {
		Conformance ConformanceSummary `json:"conformance"`
	}
	if err := json.Unmarshal([]byte(artifact.MetadataJSON), &metadata); err != nil {
		t.Fatalf("Unmarshal metadata error = %v\n%s", err, artifact.MetadataJSON)
	}
	if len(metadata.Conformance.Coverage) != 0 {
		t.Fatalf("raw sandbox conformance coverage = %+v, want no validated coverage", metadata.Conformance.Coverage)
	}
}

func TestArtifactStoreRejectsInvalidSandboxPackage(t *testing.T) {
	baseManifest := testSandboxManifestBytes(t, "sandbox-invalid", runtime.GOOS, runtime.GOARCH, SandboxProcessABIVersionV1)
	tests := []struct {
		name    string
		entries map[string][]byte
		modes   map[string]os.FileMode
		want    string
	}{
		{
			name: "missing entry",
			entries: map[string][]byte{
				"manifest.json": baseManifest,
				RuntimeEntry:    []byte("wrong entry"),
			},
			want: `runtime entry "bin/plugin" is required`,
		},
		{
			name: "non executable entry",
			entries: map[string][]byte{
				"manifest.json": baseManifest,
				"bin/plugin":    []byte("sandbox bytes"),
			},
			modes: map[string]os.FileMode{"bin/plugin": 0644},
			want:  `sandbox-process runtime entry "bin/plugin" is not executable`,
		},
		{
			name: "script entry",
			entries: map[string][]byte{
				"manifest.json": baseManifest,
				"bin/plugin":    []byte("#!/bin/sh\nexit 0\n"),
			},
			modes: map[string]os.FileMode{"bin/plugin": 0755},
			want:  `sandbox-process runtime entry "bin/plugin" must be a native executable; scripts are not allowed`,
		},
		{
			name: "symlink entry",
			entries: map[string][]byte{
				"manifest.json": baseManifest,
				"bin/plugin":    []byte("../escape"),
			},
			modes: map[string]os.FileMode{"bin/plugin": os.ModeSymlink | 0777},
			want:  `unsupported zip entry type "bin/plugin"`,
		},
		{
			name: "os mismatch",
			entries: map[string][]byte{
				"manifest.json": testSandboxManifestBytes(t, "sandbox-invalid", "not-"+runtime.GOOS, runtime.GOARCH, SandboxProcessABIVersionV1),
				"bin/plugin":    []byte("sandbox bytes"),
			},
			modes: map[string]os.FileMode{"bin/plugin": 0755},
			want:  "runtime.os",
		},
		{
			name: "abi mismatch",
			entries: map[string][]byte{
				"manifest.json": testSandboxManifestBytes(t, "sandbox-invalid", runtime.GOOS, runtime.GOARCH, "mc-gateway.sandbox-process.abi/v0"),
				"bin/plugin":    []byte("sandbox bytes"),
			},
			modes: map[string]os.FileMode{"bin/plugin": 0755},
			want:  "unsupported runtime.abi_version",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewArtifactStore(t.TempDir())
			_, err := store.ValidateAndStore(ArtifactUpload{
				SourcePath: writeTestMCGPWithModes(t, tt.entries, tt.modes),
				FileName:   "sandbox-invalid.mcgp",
				Actor:      "admin",
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateAndStore(%s) error = %v, want containing %q", tt.name, err, tt.want)
			}
		})
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

func testWASMManifestBytes(t *testing.T, pluginID, abi string) []byte {
	t.Helper()
	manifest := Manifest{
		SchemaVersion: SchemaVersion,
		ID:            pluginID,
		Name:          "WASM Test Plugin",
		Version:       "0.1.0",
		ArtifactType:  ArtifactTypeBinary,
		Runtime: RuntimeManifest{
			Type:  RuntimeWASM,
			Entry: RuntimeWASMEntry,
			ABI:   abi,
		},
		APIVersion: APIVersion,
		ExtensionPoints: []ExtensionPoint{{
			Type: "rule",
			Key:  ExtensionRuleEvaluate,
		}},
		Capabilities:  json.RawMessage(`{"extension_points":["rule.evaluate/v1"]}`),
		ConfigSchema:  json.RawMessage(`{"type":"object"}`),
		RuntimeLimits: RuntimeLimits{HandlerTimeoutMS: 3000, MemoryBytes: 64 * 1024},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal wasm manifest error = %v", err)
	}
	return data
}

func testSandboxManifestBytes(t *testing.T, pluginID, goos, goarch, abiVersion string) []byte {
	t.Helper()
	manifest := Manifest{
		SchemaVersion: SchemaVersion,
		ID:            pluginID,
		Name:          "Sandbox Test Plugin",
		Version:       "0.1.0",
		ArtifactType:  ArtifactTypeBinary,
		Runtime: RuntimeManifest{
			Type:       RuntimeSandbox,
			Entry:      "bin/plugin",
			Protocol:   SandboxProcessProtocolV1,
			OS:         goos,
			Arch:       goarch,
			ABIVersion: abiVersion,
		},
		APIVersion: APIVersion,
		ExtensionPoints: []ExtensionPoint{{
			Type: "hook",
			Key:  ExtensionConfigValidate,
		}},
		Capabilities:  json.RawMessage(`{"extension_points":["config.validate/v1"]}`),
		ConfigSchema:  json.RawMessage(`{"type":"object"}`),
		RuntimeLimits: RuntimeLimits{HandlerTimeoutMS: 3000},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal sandbox manifest error = %v", err)
	}
	return data
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
		Capabilities: json.RawMessage(`{"extension_points":["upstream.connect/v2"]}`),
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal source manifest error = %v", err)
	}
	return data
}

func testManifestBytes(t *testing.T, pluginID string) []byte {
	return testManifestBytesWithCapabilities(t, pluginID, json.RawMessage(`{"extension_points":["upstream.connect/v2"]}`))
}

func testManifestBytesWithCapabilities(t *testing.T, pluginID string, capabilities json.RawMessage) []byte {
	t.Helper()
	if len(capabilities) == 0 {
		capabilities = json.RawMessage(`{"extension_points":["upstream.connect/v2"]}`)
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
	return writeTestMCGPWithModes(t, entries, nil)
}

func writeTestMCGPWithModes(t *testing.T, entries map[string][]byte, modes map[string]os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plugin.mcgp")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create package error = %v", err)
	}
	zipWriter := zip.NewWriter(file)
	for name, data := range entries {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		mode := os.FileMode(0644)
		if modes != nil && modes[name] != 0 {
			mode = modes[name]
		}
		header.SetMode(mode)
		header.Modified = time.Unix(0, 0).UTC()
		writer, err := zipWriter.CreateHeader(header)
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
