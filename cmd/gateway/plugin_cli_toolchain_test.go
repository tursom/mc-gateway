package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPluginInitCreatesBuildableTemplate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sample-plugin")
	handled, code := runPluginCLI([]string{
		"plugin", "init", dir,
		"--id", "sample-plugin",
		"--module", "example.com/sample-plugin",
	})
	if !handled {
		t.Fatal("runPluginCLI() handled = false")
	}
	if code != 0 {
		t.Fatalf("runPluginCLI(init) code = %d, want 0", code)
	}
	for _, name := range []string{"manifest.yaml", "go.mod", "main.go", "main_test.go", "README.md", "testdata/config.json"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name))); err != nil {
			t.Fatalf("generated file %s stat error = %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err == nil {
		t.Fatal("plugin init generated manifest.json by default, want manifest.yaml")
	}
	if _, err := validatePluginDirectoryForCLI(dir, ""); err != nil {
		t.Fatalf("validatePluginDirectoryForCLI() error = %v", err)
	}
}

func TestPluginBuildSourcePackagesTemplate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "source-plugin")
	handled, code := runPluginCLI([]string{
		"plugin", "init", dir,
		"--id", "source-plugin",
		"--module", "example.com/source-plugin",
	})
	if !handled || code != 0 {
		t.Fatalf("runPluginCLI(init) = (%v, %d), want handled code 0", handled, code)
	}
	out := filepath.Join(t.TempDir(), "source-plugin.mcgp")
	handled, code = runPluginCLI([]string{
		"plugin", "build", dir,
		"--type", "source",
		"--out", out,
		"--skip-tests",
		"--vendor=false",
	})
	if !handled {
		t.Fatal("runPluginCLI() handled = false")
	}
	if code != 0 {
		t.Fatalf("runPluginCLI(build source) code = %d, want 0", code)
	}
	if _, err := validatePluginPathForCLI(out, "source"); err != nil {
		t.Fatalf("validatePluginPathForCLI(source) error = %v", err)
	}
	assertZipContains(t, out, "manifest.json", "go.mod", "main.go", "main_test.go", "README.md", "testdata/config.json")
	assertZipNotContains(t, out, "manifest.yaml")
}

func TestPluginBuildBothAcceptsOutDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "both-plugin")
	handled, code := runPluginCLI([]string{
		"plugin", "init", dir,
		"--id", "both-plugin",
		"--module", "example.com/both-plugin",
	})
	if !handled || code != 0 {
		t.Fatalf("runPluginCLI(init) = (%v, %d), want handled code 0", handled, code)
	}
	outDir := filepath.Join(t.TempDir(), "packages")
	handled, code = runPluginCLI([]string{
		"plugin", "build", dir,
		"--type", "both",
		"--out", outDir,
		"--skip-tests",
		"--vendor=false",
	})
	if !handled || code != 0 {
		t.Fatalf("runPluginCLI(build both) = (%v, %d), want handled code 0", handled, code)
	}
	binaryOut := filepath.Join(outDir, "both-plugin.mcgp")
	sourceOut := filepath.Join(outDir, "both-plugin-source.mcgp")
	if _, err := validatePluginPathForCLI(binaryOut, "binary"); err != nil {
		t.Fatalf("validatePluginPathForCLI(binary) error = %v", err)
	}
	if _, err := validatePluginPathForCLI(sourceOut, "source"); err != nil {
		t.Fatalf("validatePluginPathForCLI(source) error = %v", err)
	}
	assertZipNotContains(t, binaryOut, "manifest.yaml")
	assertZipNotContains(t, sourceOut, "manifest.yaml")
}

func TestPluginTestManifestProfile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "test-plugin")
	handled, code := runPluginCLI([]string{
		"plugin", "init", dir,
		"--id", "test-plugin",
		"--module", "example.com/test-plugin",
	})
	if !handled || code != 0 {
		t.Fatalf("runPluginCLI(init) = (%v, %d), want handled code 0", handled, code)
	}
	handled, code = runPluginCLI([]string{"plugin", "test", dir, "--profile", "manifest"})
	if !handled {
		t.Fatal("runPluginCLI() handled = false")
	}
	if code != 0 {
		t.Fatalf("runPluginCLI(test manifest) code = %d, want 0", code)
	}
}

func TestPluginFeaturesAndManifestCommands(t *testing.T) {
	handled, code := runPluginCLI([]string{"plugin", "features"})
	if !handled {
		t.Fatal("runPluginCLI() handled = false")
	}
	if code != 0 {
		t.Fatalf("runPluginCLI(features) code = %d, want 0", code)
	}
	handled, code = runPluginCLI([]string{"plugin", "manifest", "explain", "upstream.connect/v1"})
	if !handled {
		t.Fatal("runPluginCLI() handled = false")
	}
	if code != 0 {
		t.Fatalf("runPluginCLI(manifest explain) code = %d, want 0", code)
	}
}

func TestPluginManifestFormatWrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "format-plugin")
	handled, code := runPluginCLI([]string{
		"plugin", "init", dir,
		"--id", "format-plugin",
		"--module", "example.com/format-plugin",
		"--manifest-format", "json",
	})
	if !handled || code != 0 {
		t.Fatalf("runPluginCLI(init) = (%v, %d), want handled code 0", handled, code)
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(`{"schema_version":"mc-gateway.plugin/v1","id":"format-plugin","name":"Format Plugin","version":"0.1.0","artifact_type":"source","runtime":{"type":"go-plugin","entry":"plugin.so","entry_symbol":"Plugin"},"build":{"type":"go","entry":".","output":"plugin.so"},"api_version":"plugin-api/v1","sdk_module":"github.com/tursom/mc-gateway/plugin/api","sdk_module_version":"v0.1.0","extension_points":[{"type":"hook","key":"upstream.connect/v1"}],"capabilities":{"upstream_connect":{"mode":"dialer"}},"runtime_limits":{"handler_timeout_ms":3000},"config_schema":{"type":"object"}}`), 0644); err != nil {
		t.Fatalf("WriteFile(manifest) error = %v", err)
	}
	handled, code = runPluginCLI([]string{"plugin", "manifest", "format", dir, "--write"})
	if !handled {
		t.Fatal("runPluginCLI() handled = false")
	}
	if code != 0 {
		t.Fatalf("runPluginCLI(manifest format) code = %d, want 0", code)
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("ReadFile(manifest) error = %v", err)
	}
	if !strings.Contains(string(data), "\n  \"schema_version\"") {
		t.Fatalf("manifest was not formatted:\n%s", data)
	}
}

func TestPluginManifestSourceFormats(t *testing.T) {
	for _, format := range []string{"yaml", "toml", "jsonc", "json"} {
		t.Run(format, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "format-"+format)
			handled, code := runPluginCLI([]string{
				"plugin", "init", dir,
				"--id", "format-" + format,
				"--module", "example.com/format-" + format,
				"--manifest-format", format,
			})
			if !handled || code != 0 {
				t.Fatalf("runPluginCLI(init %s) = (%v, %d), want handled code 0", format, handled, code)
			}
			source, err := readPluginManifestSource(dir, "")
			if err != nil {
				t.Fatalf("readPluginManifestSource(%s) error = %v", format, err)
			}
			if source.Manifest.ID != "format-"+format {
				t.Fatalf("manifest id = %q, want format-%s", source.Manifest.ID, format)
			}
			if !json.Valid(source.CanonicalJSON) {
				t.Fatalf("canonical JSON for %s is invalid:\n%s", format, source.CanonicalJSON)
			}
			packaged, err := materializedManifestJSON(source.Raw, source.Manifest, "binary", false)
			if err != nil {
				t.Fatalf("materializedManifestJSON(%s) error = %v", format, err)
			}
			var raw map[string]any
			if err := json.Unmarshal(packaged, &raw); err != nil {
				t.Fatalf("Unmarshal(materialized %s) error = %v", format, err)
			}
			if raw["artifact_type"] != "binary" || raw["go_version"] == "" || raw["go_os"] == "" || raw["go_arch"] == "" {
				t.Fatalf("materialized %s manifest missing package fields: %s", format, packaged)
			}
			handled, code = runPluginCLI([]string{"plugin", "validate", dir})
			if !handled || code != 0 {
				t.Fatalf("runPluginCLI(validate %s) = (%v, %d), want handled code 0", format, handled, code)
			}
			handled, code = runPluginCLI([]string{"plugin", "test", dir, "--profile", "manifest"})
			if !handled || code != 0 {
				t.Fatalf("runPluginCLI(test %s) = (%v, %d), want handled code 0", format, handled, code)
			}
		})
	}
}

func TestPluginManifestFormatWritePreservesComments(t *testing.T) {
	for _, tc := range []struct {
		name    string
		file    string
		content string
		comment string
	}{
		{
			name:    "yaml",
			file:    "manifest.yaml",
			content: "# keep yaml comment\n" + manifestYAMLTemplate(pluginInitCLIOptions{ID: "comment-yaml", Name: "Comment YAML", Extension: "upstream.connect/v1"}),
			comment: "# keep yaml comment",
		},
		{
			name:    "toml",
			file:    "manifest.toml",
			content: "# keep toml comment\n" + manifestTOMLTemplate(pluginInitCLIOptions{ID: "comment-toml", Name: "Comment TOML", Extension: "upstream.connect/v1"}),
			comment: "# keep toml comment",
		},
		{
			name: "jsonc",
			file: "manifest.jsonc",
			content: strings.Replace(
				"// keep jsonc comment\n"+strings.TrimSuffix(manifestSourceJSONTemplate(pluginInitCLIOptions{ID: "comment-jsonc", Name: "Comment JSONC", Extension: "upstream.connect/v1"}, true), "\n"),
				"\n  }\n}",
				"\n  },\n}",
				1,
			),
			comment: "// keep jsonc comment",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			manifestPath := filepath.Join(dir, tc.file)
			if err := os.WriteFile(manifestPath, []byte(tc.content), 0644); err != nil {
				t.Fatalf("WriteFile(%s) error = %v", tc.file, err)
			}
			before, err := readPluginManifestSource(dir, "")
			if err != nil {
				t.Fatalf("readPluginManifestSource(before) error = %v", err)
			}
			handled, code := runPluginCLI([]string{"plugin", "manifest", "format", dir, "--write"})
			if !handled || code != 0 {
				t.Fatalf("runPluginCLI(manifest format %s) = (%v, %d), want handled code 0", tc.name, handled, code)
			}
			data, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatalf("ReadFile(%s) error = %v", tc.file, err)
			}
			if !strings.Contains(string(data), tc.comment) {
				t.Fatalf("formatted %s lost comment:\n%s", tc.file, data)
			}
			after, err := readPluginManifestSource(dir, "")
			if err != nil {
				t.Fatalf("readPluginManifestSource(after) error = %v", err)
			}
			if !bytes.Equal(before.CanonicalJSON, after.CanonicalJSON) {
				t.Fatalf("canonical JSON changed after format\nbefore=%s\nafter=%s", before.CanonicalJSON, after.CanonicalJSON)
			}
		})
	}
}

func TestPluginManifestMultipleSourcesRequireExplicitManifest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "multi-manifest")
	handled, code := runPluginCLI([]string{
		"plugin", "init", dir,
		"--id", "multi-manifest",
		"--module", "example.com/multi-manifest",
	})
	if !handled || code != 0 {
		t.Fatalf("runPluginCLI(init) = (%v, %d), want handled code 0", handled, code)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifestSourceJSONTemplate(pluginInitCLIOptions{ID: "multi-manifest", Name: "Multi Manifest", Extension: "upstream.connect/v1"}, false)), 0644); err != nil {
		t.Fatalf("WriteFile(manifest.json) error = %v", err)
	}
	handled, code = runPluginCLI([]string{"plugin", "validate", dir})
	if !handled {
		t.Fatal("runPluginCLI(validate) handled = false")
	}
	if code == 0 {
		t.Fatal("runPluginCLI(validate) code = 0, want failure for multiple manifests")
	}
	handled, code = runPluginCLI([]string{"plugin", "validate", dir, "--manifest", "manifest.yaml"})
	if !handled || code != 0 {
		t.Fatalf("runPluginCLI(validate --manifest) = (%v, %d), want handled code 0", handled, code)
	}
	handled, code = runPluginCLI([]string{"plugin", "test", dir, "--profile", "manifest", "--manifest", "manifest.yaml"})
	if !handled || code != 0 {
		t.Fatalf("runPluginCLI(test --manifest) = (%v, %d), want handled code 0", handled, code)
	}
	out := filepath.Join(t.TempDir(), "multi-manifest-source.mcgp")
	handled, code = runPluginCLI([]string{"plugin", "build", dir, "--type", "source", "--out", out, "--skip-tests", "--vendor=false", "--manifest", "manifest.yaml"})
	if !handled || code != 0 {
		t.Fatalf("runPluginCLI(build --manifest) = (%v, %d), want handled code 0", handled, code)
	}
	if _, err := validatePluginPathForCLI(out, "source"); err != nil {
		t.Fatalf("validatePluginPathForCLI(source) error = %v", err)
	}
	handled, code = runPluginCLI([]string{"plugin", "manifest", "format", dir, "--manifest", "manifest.yaml", "--canonical-json", "--type", "source"})
	if !handled || code != 0 {
		t.Fatalf("runPluginCLI(manifest format --manifest --canonical-json) = (%v, %d), want handled code 0", handled, code)
	}
}

func TestPluginGovernanceCommands(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "governance-plugin")
	handled, code := runPluginCLI([]string{
		"plugin", "init", dir,
		"--id", "governance-plugin",
		"--module", "example.com/governance-plugin",
	})
	if !handled || code != 0 {
		t.Fatalf("runPluginCLI(init) = (%v, %d), want handled code 0", handled, code)
	}
	artifact := filepath.Join(t.TempDir(), "governance-plugin.mcgp")
	handled, code = runPluginCLI([]string{
		"plugin", "build", dir,
		"--type", "binary",
		"--out", artifact,
		"--skip-tests",
	})
	if !handled || code != 0 {
		t.Fatalf("runPluginCLI(build binary) = (%v, %d), want handled code 0", handled, code)
	}
	for _, tc := range [][]string{
		{"plugin", "preflight", artifact, "--config-json", `{"upstream":"127.0.0.1:25566"}`, "--profile", "dev"},
		{"plugin", "self-test", artifact, "--profile", "dev"},
		{"plugin", "benchmark", artifact, "--profile", "dev", "--benchmark-profile", "local-fast", "--p95-ms", "1", "--p99-ms", "2", "--error-rate", "0", "--baseline-diff", "0.1"},
		{"plugin", "preflight", dir, "--manifest", "manifest.yaml", "--config-json", `{"upstream":"127.0.0.1:25566"}`, "--profile", "dev"},
	} {
		handled, code = runPluginCLI(tc)
		if !handled {
			t.Fatalf("runPluginCLI(%v) handled = false", tc)
		}
		if code != 0 {
			t.Fatalf("runPluginCLI(%v) code = %d, want 0", tc, code)
		}
	}
}

func TestPluginRemoteCLIRequests(t *testing.T) {
	type observedRequest struct {
		Method      string
		RequestURI  string
		ContentType string
		Body        map[string]any
		FileName    string
	}
	requests := make(chan observedRequest, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want bearer token", got)
		}
		observed := observedRequest{
			Method:      r.Method,
			RequestURI:  r.URL.RequestURI(),
			ContentType: r.Header.Get("Content-Type"),
			Body:        map[string]any{},
		}
		switch {
		case strings.HasPrefix(observed.ContentType, "application/json"):
			if err := json.NewDecoder(r.Body).Decode(&observed.Body); err != nil {
				t.Errorf("Decode JSON body error = %v", err)
			}
		case strings.HasPrefix(observed.ContentType, "multipart/form-data"):
			if err := r.ParseMultipartForm(64 << 20); err != nil {
				t.Errorf("ParseMultipartForm error = %v", err)
			} else {
				file, header, err := r.FormFile("artifact")
				if err != nil {
					t.Errorf("FormFile(artifact) error = %v", err)
				} else {
					observed.FileName = header.Filename
					_, _ = io.Copy(io.Discard, file)
					_ = file.Close()
				}
			}
		}
		requests <- observed
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/operations") {
			_, _ = io.WriteString(w, `{"operations":{"logs":[{"message":"ok"}],"traces":[],"events":[{"name":"evt"}],"event_queue":{"queued":1},"handlers":[{"plugin_id":"demo"}],"custom_metrics":[],"background_tasks":[{"id":"sync"}],"plugin_data":[{"key":"k"}],"plugin_files":[{"name":"f"}],"gc":[]}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	t.Setenv("MC_GATEWAY_ADMIN_URL", server.URL)
	t.Setenv("MC_GATEWAY_ADMIN_TOKEN", "test-token")

	runRemotePluginCLI(t, "plugin", "status", "demo")
	assertRemoteRequest(t, <-requests, http.MethodGet, "/admin/api/plugins/demo")

	artifactPath := filepath.Join(t.TempDir(), "demo.mcgp")
	if err := os.WriteFile(artifactPath, []byte("artifact"), 0644); err != nil {
		t.Fatalf("WriteFile(artifact) error = %v", err)
	}
	runRemotePluginCLI(t, "plugin", "upload", artifactPath)
	uploadReq := <-requests
	assertRemoteRequest(t, uploadReq, http.MethodPost, "/admin/api/plugin-artifacts")
	if uploadReq.FileName != "demo.mcgp" {
		t.Fatalf("upload file name = %q, want demo.mcgp", uploadReq.FileName)
	}

	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"upstream":"127.0.0.1:25565"}`), 0644); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	runRemotePluginCLI(t, "plugin", "enable", "demo", "--artifact", "art-1", "--config", configPath, "--priority", "7")
	enableReq := <-requests
	assertRemoteRequest(t, enableReq, http.MethodPut, "/admin/api/plugins/demo")
	if enableReq.Body["artifact_id"] != "art-1" || enableReq.Body["desired_state"] != "enabled" || enableReq.Body["priority"].(float64) != 7 {
		t.Fatalf("enable body = %#v", enableReq.Body)
	}

	runRemotePluginCLI(t, "plugin", "logs", "demo")
	assertRemoteRequest(t, <-requests, http.MethodGet, "/admin/api/plugins/demo/operations")

	runRemotePluginCLI(t, "plugin", "task", "run", "demo", "sync", "--confirm-token", "confirm")
	taskReq := <-requests
	assertRemoteRequest(t, taskReq, http.MethodPost, "/admin/api/plugins/demo/operations/tasks/sync/trigger")
	if taskReq.Body["confirm_token"] != "confirm" {
		t.Fatalf("task body = %#v", taskReq.Body)
	}

	runRemotePluginCLI(t, "plugin", "repo", "import", "demo", "--repository-type", "file", "--index", "repo.json", "--artifact", "candidate-1", "--version", "1.2.3")
	repoReq := <-requests
	assertRemoteRequest(t, repoReq, http.MethodPost, "/admin/api/plugin-repositories/imports")
	if repoReq.Body["plugin_id"] != "demo" || repoReq.Body["repository_type"] != "file" || repoReq.Body["artifact_id"] != "candidate-1" {
		t.Fatalf("repo body = %#v", repoReq.Body)
	}

	runRemotePluginCLI(t, "plugin", "review", "status", "demo", "--artifact", "art-1", "--profile", "prod")
	assertRemoteRequest(t, <-requests, http.MethodGet, "/admin/api/plugins/demo/governance?artifact_id=art-1&profile=prod")

	runRemotePluginCLI(t, "plugin", "sbom", "verify", "demo", "--artifact", "art-1", "--metadata-json", `{"sbom":{"format":"spdx"}}`)
	supplyReq := <-requests
	assertRemoteRequest(t, supplyReq, http.MethodPost, "/admin/api/plugin-supply-chain")
	if supplyReq.Body["plugin_id"] != "demo" || supplyReq.Body["artifact_id"] != "art-1" {
		t.Fatalf("supply-chain body = %#v", supplyReq.Body)
	}

	runRemotePluginCLI(t, "plugin", "runtime", "mode", "--mode", "go-plugin-process")
	runtimeReq := <-requests
	assertRemoteRequest(t, runtimeReq, http.MethodPut, "/admin/api/plugin-service")
	if runtimeReq.Body["desired_mode"] != "go-plugin-process" {
		t.Fatalf("runtime body = %#v", runtimeReq.Body)
	}
}

func TestNormalizeAdminAPIBase(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{raw: "http://127.0.0.1:8080", want: "http://127.0.0.1:8080/admin/api"},
		{raw: "http://127.0.0.1:8080/admin", want: "http://127.0.0.1:8080/admin/api"},
		{raw: "http://127.0.0.1:8080/admin/api/", want: "http://127.0.0.1:8080/admin/api"},
	} {
		got, err := normalizeAdminAPIBase(tc.raw)
		if err != nil {
			t.Fatalf("normalizeAdminAPIBase(%q) error = %v", tc.raw, err)
		}
		if got != tc.want {
			t.Fatalf("normalizeAdminAPIBase(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func runRemotePluginCLI(t *testing.T, args ...string) {
	t.Helper()
	handled, code := runPluginCLI(args)
	if !handled {
		t.Fatalf("runPluginCLI(%v) handled = false", args)
	}
	if code != 0 {
		t.Fatalf("runPluginCLI(%v) code = %d, want 0", args, code)
	}
}

func assertRemoteRequest(t *testing.T, got struct {
	Method      string
	RequestURI  string
	ContentType string
	Body        map[string]any
	FileName    string
}, wantMethod, wantURI string) {
	t.Helper()
	if got.Method != wantMethod || got.RequestURI != wantURI {
		t.Fatalf("request = %s %s, want %s %s", got.Method, got.RequestURI, wantMethod, wantURI)
	}
}

func assertZipContains(t *testing.T, zipPath string, names ...string) {
	t.Helper()
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("OpenReader(%s) error = %v", zipPath, err)
	}
	defer reader.Close()
	seen := make(map[string]bool, len(reader.File))
	for _, file := range reader.File {
		seen[file.Name] = true
		if strings.Contains(file.Name, `\`) {
			t.Fatalf("zip entry %q uses backslash", file.Name)
		}
	}
	for _, name := range names {
		if !seen[name] {
			t.Fatalf("zip %s missing entry %s; entries=%v", zipPath, name, seen)
		}
	}
}

func assertZipNotContains(t *testing.T, zipPath string, names ...string) {
	t.Helper()
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("OpenReader(%s) error = %v", zipPath, err)
	}
	defer reader.Close()
	seen := make(map[string]bool, len(reader.File))
	for _, file := range reader.File {
		seen[file.Name] = true
	}
	for _, name := range names {
		if seen[name] {
			t.Fatalf("zip %s unexpectedly contains entry %s", zipPath, name)
		}
	}
}
