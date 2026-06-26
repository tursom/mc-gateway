package pluginmanager

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type SourceBuilder interface {
	Build(ctx context.Context, source ArtifactRecord, req BuildRequest, build BuildRecord) (BuildResult, error)
}

type BuildResult struct {
	Manifest       Manifest
	ArtifactBytes  []byte
	ArtifactSHA256 string
	GoVersion      string
	ModuleSummary  string
	GoVersionM     string
	ABIFingerprint string
	LogSummary     string
	Metadata       map[string]any
}

type LocalProcessBuilder struct {
	StoreRoot string
}

func (b LocalProcessBuilder) Build(ctx context.Context, source ArtifactRecord, req BuildRequest, build BuildRecord) (BuildResult, error) {
	if source.ArtifactType != ArtifactTypeSource {
		return BuildResult{}, fmt.Errorf("artifact %s is %q, want source", source.ID, source.ArtifactType)
	}
	var manifest Manifest
	if err := json.Unmarshal([]byte(source.MetadataJSON), &manifest); err != nil {
		return BuildResult{}, fmt.Errorf("decode source manifest: %w", err)
	}
	if err := validateManifest(manifest); err != nil {
		return BuildResult{}, err
	}
	if manifest.Build.Type != "" && manifest.Build.Type != BuildTypeGo {
		return BuildResult{}, fmt.Errorf("unsupported build.type %q", manifest.Build.Type)
	}
	if manifest.Build.Output != "" && manifest.Build.Output != RuntimeEntry {
		return BuildResult{}, fmt.Errorf("unsupported build.output %q", manifest.Build.Output)
	}
	if req.VendorRequired {
		if _, err := os.Stat(filepath.Join(source.FilePath, "vendor")); err != nil {
			return BuildResult{}, errors.New("vendor is required but source package has no vendor directory")
		}
	}
	goVersion, goVersionOutput, err := commandOutput(ctx, source.FilePath, nil, "go", "version")
	if err != nil {
		return BuildResult{LogSummary: sanitizeLog(goVersionOutput)}, err
	}
	if manifest.GoVersion != "" && manifest.GoVersion != goVersion {
		return BuildResult{GoVersion: goVersion, LogSummary: sanitizeLog(goVersionOutput)}, fmt.Errorf("source go_version %q does not match builder %q", manifest.GoVersion, goVersion)
	}
	manifest.GoVersion = goVersion
	manifest.GOOS = req.GOOS
	manifest.GOARCH = req.GOARCH
	manifest.ArtifactType = ArtifactTypeBinary
	manifest.Runtime.Entry = RuntimeEntry

	outDir, err := os.MkdirTemp("", "mc-gateway-plugin-build-*")
	if err != nil {
		return BuildResult{}, err
	}
	defer os.RemoveAll(outDir)
	outPath := filepath.Join(outDir, RuntimeEntry)
	env := buildEnvironment(req)
	args := []string{"build", "-buildmode=plugin", "-trimpath", "-buildvcs=false", "-o", outPath}
	if req.BuildTags != "" {
		args = append(args, "-tags", req.BuildTags)
	}
	buildEntry := sourceBuildEntry(manifest)
	args = append(args, buildEntry)
	_, buildLog, buildErr := commandOutput(ctx, source.FilePath, env, "go", args...)
	if buildErr != nil {
		return BuildResult{GoVersion: goVersion, LogSummary: sanitizeLog(buildLog)}, buildErr
	}
	if _, nmLog, err := commandOutput(ctx, source.FilePath, env, "go", "tool", "nm", outPath); err != nil {
		return BuildResult{GoVersion: goVersion, LogSummary: sanitizeLog(buildLog + "\n" + nmLog)}, fmt.Errorf("inspect built plugin symbols: %w", err)
	} else if err := validateBuiltSymbols(manifest, nmLog); err != nil {
		return BuildResult{GoVersion: goVersion, LogSummary: sanitizeLog(buildLog + "\n" + nmLog)}, err
	}
	artifactBytes, err := os.ReadFile(outPath)
	if err != nil {
		return BuildResult{}, err
	}
	artifactSum := sha256.Sum256(artifactBytes)
	artifactSHA := hex.EncodeToString(artifactSum[:])
	moduleSummary := "[]"
	if _, moduleLog, err := commandOutput(ctx, source.FilePath, env, "go", "list", "-m", "-json", "all"); err == nil {
		moduleSummary = summarizeGoModules(moduleLog)
	} else {
		buildLog += "\n" + moduleLog
	}
	goVersionM := "{}"
	if _, versionMLog, err := commandOutput(ctx, source.FilePath, env, "go", "version", "-m", outPath); err == nil {
		goVersionM = summarizeGoVersionM(versionMLog)
	} else {
		buildLog += "\n" + versionMLog
	}
	abi := abiFingerprint(manifest, goVersion)
	metadata := map[string]any{
		"source_sha256":   source.SHA256,
		"artifact_sha256": artifactSHA,
		"builder_type":    BuilderTypeLocalProcess,
		"builder_version": build.BuilderVersion,
		"go_version":      goVersion,
		"go_os":           req.GOOS,
		"go_arch":         req.GOARCH,
		"go_amd64":        req.GOAMD64,
		"go_arm64":        req.GOARM64,
		"cgo_enabled":     req.CGOEnabled,
		"build_tags":      req.BuildTags,
		"vendor_required": req.VendorRequired,
		"abi_fingerprint": abi,
	}
	return BuildResult{
		Manifest:       manifest,
		ArtifactBytes:  artifactBytes,
		ArtifactSHA256: artifactSHA,
		GoVersion:      goVersion,
		ModuleSummary:  moduleSummary,
		GoVersionM:     goVersionM,
		ABIFingerprint: abi,
		LogSummary:     sanitizeLog(buildLog),
		Metadata:       metadata,
	}, nil
}

type ContainerBuilder struct{}

func (ContainerBuilder) Build(context.Context, ArtifactRecord, BuildRequest, BuildRecord) (BuildResult, error) {
	return BuildResult{}, errors.New("container builder is not configured in this phase")
}

func buildEnvironment(req BuildRequest) []string {
	env := os.Environ()
	env = append(env,
		"GOOS="+req.GOOS,
		"GOARCH="+req.GOARCH,
		"CGO_ENABLED="+req.CGOEnabled,
	)
	if req.GOAMD64 != "" {
		env = append(env, "GOAMD64="+req.GOAMD64)
	}
	if req.GOARM64 != "" {
		env = append(env, "GOARM64="+req.GOARM64)
	}
	if req.GOPROXY != "" {
		env = append(env, "GOPROXY="+req.GOPROXY)
	}
	if req.GONOSUMDB != "" {
		env = append(env, "GONOSUMDB="+req.GONOSUMDB)
	}
	if req.GOPRIVATE != "" {
		env = append(env, "GOPRIVATE="+req.GOPRIVATE)
	}
	return env
}

func commandOutput(ctx context.Context, dir string, env []string, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	output := out.String()
	if name == "go" && len(args) == 1 && args[0] == "version" && err == nil {
		fields := strings.Fields(output)
		if len(fields) >= 3 {
			return fields[2], output, nil
		}
	}
	return strings.TrimSpace(output), output, err
}

func sanitizeLog(log string) string {
	replacers := []string{
		os.Getenv("HOME"), "$HOME",
		os.TempDir(), "$TMPDIR",
	}
	sanitized := log
	for i := 0; i+1 < len(replacers); i += 2 {
		if replacers[i] != "" {
			sanitized = strings.ReplaceAll(sanitized, replacers[i], replacers[i+1])
		}
	}
	for _, key := range []string{"TOKEN", "SECRET", "PASSWORD", "PRIVATE"} {
		for _, part := range strings.Fields(sanitized) {
			if strings.Contains(strings.ToUpper(part), key+"=") {
				sanitized = strings.ReplaceAll(sanitized, part, key+"=<redacted>")
			}
		}
	}
	if len(sanitized) > DefaultBuildLogMaxBytes {
		sanitized = sanitized[len(sanitized)-DefaultBuildLogMaxBytes:]
	}
	lines := strings.Split(sanitized, "\n")
	if len(lines) > 80 {
		lines = lines[len(lines)-80:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func summarizeGoModules(raw string) string {
	decoder := json.NewDecoder(strings.NewReader(raw))
	var modules []map[string]any
	for decoder.More() {
		var module struct {
			Path    string `json:"Path"`
			Version string `json:"Version"`
			Main    bool   `json:"Main"`
			Replace *struct {
				Path    string `json:"Path"`
				Version string `json:"Version"`
			} `json:"Replace"`
		}
		if err := decoder.Decode(&module); err != nil {
			break
		}
		item := map[string]any{"path": module.Path, "version": module.Version, "main": module.Main}
		if module.Replace != nil {
			item["replace"] = map[string]string{"path": module.Replace.Path, "version": module.Replace.Version}
		}
		modules = append(modules, item)
	}
	data, err := json.Marshal(modules)
	if err != nil || len(data) == 0 {
		return "[]"
	}
	return string(data)
}

func summarizeGoVersionM(raw string) string {
	summary := map[string]any{"raw_summary": sanitizeLog(raw)}
	data, err := json.Marshal(summary)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func abiFingerprint(manifest Manifest, goVersion string) string {
	payload := strings.Join([]string{
		manifest.ID,
		manifest.APIVersion,
		manifest.SDKModule,
		manifest.SDKModuleVersion,
		goVersion,
		runtime.GOOS,
		runtime.GOARCH,
		extensionPointsFingerprint(manifest),
	}, "\x00")
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

func extensionPointsFingerprint(manifest Manifest) string {
	keys := extensionPointKeys(manifest)
	data, _ := json.Marshal(keys)
	return string(data)
}

func validateBuiltSymbols(manifest Manifest, nmLog string) error {
	entrySymbol := manifest.Runtime.EntrySymbol
	if entrySymbol == "" {
		entrySymbol = "Plugin"
	}
	if !strings.Contains(nmLog, entrySymbol) {
		return fmt.Errorf("built plugin is missing entry symbol %q", entrySymbol)
	}
	return nil
}

func defaultBuildRequest(req BuildRequest, source ArtifactRecord) BuildRequest {
	var manifest Manifest
	_ = json.Unmarshal([]byte(source.MetadataJSON), &manifest)
	if req.BuilderType == "" {
		req.BuilderType = BuilderTypeLocalProcess
	}
	if req.BuilderVersion == "" {
		req.BuilderVersion = "local-process/go-buildmode-plugin"
	}
	if req.GOOS == "" {
		req.GOOS = runtime.GOOS
	}
	if req.GOARCH == "" {
		req.GOARCH = runtime.GOARCH
	}
	if req.CGOEnabled == "" {
		if manifest.Build.CGOEnabled != nil && !*manifest.Build.CGOEnabled {
			req.CGOEnabled = "0"
		} else {
			req.CGOEnabled = "1"
		}
	}
	if req.BuildTags == "" && len(manifest.Build.Tags) > 0 {
		req.BuildTags = strings.Join(manifest.Build.Tags, ",")
	}
	if manifest.Build.VendorRequired {
		req.VendorRequired = true
	}
	if req.SDKModule == "" {
		req.SDKModule = manifest.SDKModule
		req.SDKVersion = manifest.SDKModuleVersion
	}
	return req
}

func buildDurationMS(start time.Time) int64 {
	return time.Since(start).Milliseconds()
}
