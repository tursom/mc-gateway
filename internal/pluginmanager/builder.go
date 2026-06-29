// internal/pluginmanager/builder.go 把源码插件包构建为网关可加载制品，并记录构建元数据。

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
	manifest, err := prepareSourceBuild(source, req)
	if err != nil {
		return BuildResult{}, err
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
	modMode := sourceBuildModMode(source, req)
	args := []string{"build", "-mod=" + modMode, "-buildmode=plugin", "-trimpath", "-buildvcs=false", "-o", outPath}
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
	if _, moduleLog, err := commandOutput(ctx, source.FilePath, env, "go", "list", "-mod="+modMode, "-m", "-json", "all"); err == nil {
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
		"go_mod_mode":     modMode,
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

type ContainerBuilder struct {
	DockerCommand string
	DefaultImage  string
}

func (b ContainerBuilder) Build(ctx context.Context, source ArtifactRecord, req BuildRequest, build BuildRecord) (BuildResult, error) {
	manifest, err := prepareSourceBuild(source, req)
	if err != nil {
		return BuildResult{}, err
	}
	if req.BuilderImage == "" {
		req.BuilderImage = b.DefaultImage
	}
	if req.BuilderImage == "" {
		req.BuilderImage = defaultContainerBuilderImage()
	}
	if strings.TrimSpace(req.BuilderImage) == "" {
		return BuildResult{}, errors.New("container builder requires builder_image")
	}
	docker := b.DockerCommand
	if docker == "" {
		docker = "docker"
	}
	outDir, err := os.MkdirTemp("", "mc-gateway-plugin-container-build-*")
	if err != nil {
		return BuildResult{}, err
	}
	defer os.RemoveAll(outDir)
	outPath := filepath.Join(outDir, RuntimeEntry)
	mounts := []string{
		source.FilePath + ":/src:ro",
		outDir + ":/out",
	}
	env := containerBuildEnvironment(req)
	goVersion, goVersionOutput, err := dockerRunOutput(ctx, docker, req.BuilderImage, nil, nil, "", "go", "version")
	if err != nil {
		return BuildResult{LogSummary: sanitizeLog(goVersionOutput)}, err
	}
	goVersion = parseGoVersionOutput(goVersion)
	if manifest.GoVersion != "" && manifest.GoVersion != goVersion {
		return BuildResult{GoVersion: goVersion, LogSummary: sanitizeLog(goVersionOutput)}, fmt.Errorf("source go_version %q does not match builder %q", manifest.GoVersion, goVersion)
	}
	manifest.GoVersion = goVersion
	manifest.GOOS = req.GOOS
	manifest.GOARCH = req.GOARCH
	manifest.ArtifactType = ArtifactTypeBinary
	manifest.Runtime.Entry = RuntimeEntry

	modMode := sourceBuildModMode(source, req)
	args := []string{"go", "build", "-mod=" + modMode, "-buildmode=plugin", "-trimpath", "-buildvcs=false", "-o", "/out/" + RuntimeEntry}
	if req.BuildTags != "" {
		args = append(args, "-tags", req.BuildTags)
	}
	buildEntry := sourceBuildEntry(manifest)
	args = append(args, buildEntry)
	_, buildLog, buildErr := dockerRunOutput(ctx, docker, req.BuilderImage, mounts, env, "/src", args...)
	if buildErr != nil {
		return BuildResult{GoVersion: goVersion, LogSummary: sanitizeLog(buildLog)}, buildErr
	}
	_, nmLog, err := dockerRunOutput(ctx, docker, req.BuilderImage, []string{outDir + ":/out:ro"}, nil, "/", "go", "tool", "nm", "/out/"+RuntimeEntry)
	if err != nil {
		return BuildResult{GoVersion: goVersion, LogSummary: sanitizeLog(buildLog + "\n" + nmLog)}, fmt.Errorf("inspect built plugin symbols: %w", err)
	}
	if err := validateBuiltSymbols(manifest, nmLog); err != nil {
		return BuildResult{GoVersion: goVersion, LogSummary: sanitizeLog(buildLog + "\n" + nmLog)}, err
	}
	artifactBytes, err := os.ReadFile(outPath)
	if err != nil {
		return BuildResult{}, err
	}
	artifactSum := sha256.Sum256(artifactBytes)
	artifactSHA := hex.EncodeToString(artifactSum[:])
	moduleSummary := "[]"
	if _, moduleLog, err := dockerRunOutput(ctx, docker, req.BuilderImage, mounts, env, "/src", "go", "list", "-mod="+modMode, "-m", "-json", "all"); err == nil {
		moduleSummary = summarizeGoModules(moduleLog)
	} else {
		buildLog += "\n" + moduleLog
	}
	goVersionM := "{}"
	if _, versionMLog, err := dockerRunOutput(ctx, docker, req.BuilderImage, []string{outDir + ":/out:ro"}, nil, "/", "go", "version", "-m", "/out/"+RuntimeEntry); err == nil {
		goVersionM = summarizeGoVersionM(versionMLog)
	} else {
		buildLog += "\n" + versionMLog
	}
	imageDigest, digestLog := dockerImageDigest(ctx, docker, req.BuilderImage)
	if digestLog != "" {
		buildLog += "\n" + digestLog
	}
	abi := abiFingerprint(manifest, goVersion)
	metadata := map[string]any{
		"source_sha256":        source.SHA256,
		"artifact_sha256":      artifactSHA,
		"builder_type":         BuilderTypeContainer,
		"builder_image":        req.BuilderImage,
		"builder_image_digest": imageDigest,
		"builder_version":      build.BuilderVersion,
		"go_version":           goVersion,
		"go_os":                req.GOOS,
		"go_arch":              req.GOARCH,
		"go_amd64":             req.GOAMD64,
		"go_arm64":             req.GOARM64,
		"cgo_enabled":          req.CGOEnabled,
		"build_tags":           req.BuildTags,
		"go_mod_mode":          modMode,
		"vendor_required":      req.VendorRequired,
		"abi_fingerprint":      abi,
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

func buildEnvironment(req BuildRequest) []string {
	env := whitelistedHostBuildEnvironment()
	env = appendBuildEnv(env, "GOOS", req.GOOS)
	env = appendBuildEnv(env, "GOARCH", req.GOARCH)
	env = appendBuildEnv(env, "CGO_ENABLED", req.CGOEnabled)
	if req.GOAMD64 != "" {
		env = appendBuildEnv(env, "GOAMD64", req.GOAMD64)
	}
	if req.GOARM64 != "" {
		env = appendBuildEnv(env, "GOARM64", req.GOARM64)
	}
	if req.GOPROXY != "" {
		env = appendBuildEnv(env, "GOPROXY", req.GOPROXY)
	}
	if req.GONOSUMDB != "" {
		env = appendBuildEnv(env, "GONOSUMDB", req.GONOSUMDB)
	}
	if req.GOPRIVATE != "" {
		env = appendBuildEnv(env, "GOPRIVATE", req.GOPRIVATE)
	}
	return env
}

func whitelistedHostBuildEnvironment() []string {
	keys := []string{
		"PATH",
		"GOROOT",
		"GOPATH",
		"GOCACHE",
		"GOMODCACHE",
		"GOTMPDIR",
		"TMPDIR",
		"TEMP",
		"TMP",
		"HOME",
		"XDG_CACHE_HOME",
		"GOWORK",
		"GOTOOLCHAIN",
		"SSL_CERT_FILE",
		"SSL_CERT_DIR",
		"CC",
		"CXX",
		"AR",
		"PKG_CONFIG",
	}
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func appendBuildEnv(env []string, key, value string) []string {
	prefix := key + "="
	for idx, item := range env {
		if strings.HasPrefix(item, prefix) {
			env[idx] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func containerBuildEnvironment(req BuildRequest) []string {
	env := []string{
		"GOOS=" + req.GOOS,
		"GOARCH=" + req.GOARCH,
		"CGO_ENABLED=" + req.CGOEnabled,
		"GOCACHE=/tmp/gocache",
		"GOMODCACHE=/tmp/gomodcache",
		"HOME=/tmp",
		"GOTOOLCHAIN=local",
	}
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
		return parseGoVersionOutput(output), output, nil
	}
	return strings.TrimSpace(output), output, err
}

func dockerRunOutput(ctx context.Context, docker, image string, mounts, env []string, workdir string, args ...string) (string, string, error) {
	dockerArgs := dockerRunArgs(image, mounts, env, workdir, args...)
	return commandOutput(ctx, "", nil, docker, dockerArgs...)
}

func dockerRunArgs(image string, mounts, env []string, workdir string, args ...string) []string {
	dockerArgs := []string{"run", "--rm", "--read-only", "--tmpfs", "/tmp:rw,exec,nosuid,size=1g"}
	for _, mount := range mounts {
		dockerArgs = append(dockerArgs, "-v", mount)
	}
	for _, item := range env {
		dockerArgs = append(dockerArgs, "-e", item)
	}
	if workdir != "" {
		dockerArgs = append(dockerArgs, "-w", workdir)
	}
	dockerArgs = append(dockerArgs, image)
	dockerArgs = append(dockerArgs, args...)
	return dockerArgs
}

func dockerImageDigest(ctx context.Context, docker, image string) (string, string) {
	out, raw, err := commandOutput(ctx, "", nil, docker, "image", "inspect", "--format", "{{json .RepoDigests}}", image)
	if err != nil {
		return "", raw
	}
	var digests []string
	if err := json.Unmarshal([]byte(out), &digests); err != nil || len(digests) == 0 {
		return "", raw
	}
	return digests[0], ""
}

func parseGoVersionOutput(output string) string {
	fields := strings.Fields(output)
	if len(fields) >= 3 && fields[0] == "go" && fields[1] == "version" {
		return fields[2]
	}
	return strings.TrimSpace(output)
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
	sanitized = redactPrivatePaths(sanitized)
	for _, part := range strings.Fields(sanitized) {
		replacement, ok := sanitizedFieldReplacement(part)
		if ok {
			sanitized = strings.ReplaceAll(sanitized, part, replacement)
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

func redactPrivatePaths(log string) string {
	fields := strings.Fields(log)
	redacted := log
	for _, field := range fields {
		trimmed := strings.Trim(field, `"'`)
		lower := strings.ToLower(trimmed)
		if strings.Contains(lower, "/private/") || strings.Contains(lower, `\private\`) {
			redacted = strings.ReplaceAll(redacted, trimmed, "<redacted-private-path>")
		}
	}
	return redacted
}

func sanitizedFieldReplacement(field string) (string, bool) {
	trimmed := strings.Trim(field, `"'`)
	key, value, ok := strings.Cut(trimmed, "=")
	if ok {
		upperKey := strings.ToUpper(strings.TrimSpace(key))
		switch upperKey {
		case "GOPRIVATE", "GONOSUMDB", "GONOPROXY", "GOPROXY":
			return key + "=<policy:configured>", true
		}
		for _, marker := range []string{"TOKEN", "SECRET", "PASSWORD", "PRIVATE", "CREDENTIAL", "AUTHORIZATION", "SESSION"} {
			if strings.Contains(upperKey, marker) || strings.Contains(strings.ToUpper(value), marker) {
				return key + "=<redacted>", true
			}
		}
	}
	lower := strings.ToLower(trimmed)
	if strings.Contains(lower, "/private/") || strings.Contains(lower, `\private\`) {
		return "<redacted-private-path>", true
	}
	return "", false
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

func prepareSourceBuild(source ArtifactRecord, req BuildRequest) (Manifest, error) {
	if source.ArtifactType != ArtifactTypeSource {
		return Manifest{}, fmt.Errorf("artifact %s is %q, want source", source.ID, source.ArtifactType)
	}
	var manifest Manifest
	if err := json.Unmarshal([]byte(source.MetadataJSON), &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode source manifest: %w", err)
	}
	if err := validateManifest(manifest); err != nil {
		return Manifest{}, err
	}
	if manifest.Build.Type != "" && manifest.Build.Type != BuildTypeGo {
		return Manifest{}, fmt.Errorf("unsupported build.type %q", manifest.Build.Type)
	}
	if manifest.Build.Output != "" && manifest.Build.Output != RuntimeEntry {
		return Manifest{}, fmt.Errorf("unsupported build.output %q", manifest.Build.Output)
	}
	if req.VendorRequired {
		if _, err := os.Stat(filepath.Join(source.FilePath, "vendor")); err != nil {
			return Manifest{}, errors.New("vendor is required but source package has no vendor directory")
		}
	}
	return manifest, nil
}

func sourceBuildModMode(source ArtifactRecord, req BuildRequest) string {
	if req.VendorRequired {
		return "vendor"
	}
	if info, err := os.Stat(filepath.Join(source.FilePath, "vendor")); err == nil && info.IsDir() {
		return "vendor"
	}
	return "readonly"
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
	return defaultBuildRequestForProfile(req, source, PolicyProfileDev)
}

func defaultBuildRequestForProfile(req BuildRequest, source ArtifactRecord, profile string) BuildRequest {
	var manifest Manifest
	_ = json.Unmarshal([]byte(source.MetadataJSON), &manifest)
	if req.BuilderType == "" {
		if normalizeProfile(profile) == PolicyProfileDev {
			req.BuilderType = BuilderTypeLocalProcess
		} else {
			req.BuilderType = BuilderTypeContainer
		}
	}
	if req.BuilderType == BuilderTypeContainer && req.BuilderImage == "" {
		req.BuilderImage = defaultContainerBuilderImage()
	}
	if req.BuilderVersion == "" {
		switch req.BuilderType {
		case BuilderTypeContainer:
			req.BuilderVersion = "container/go-buildmode-plugin"
		default:
			req.BuilderVersion = "local-process/go-buildmode-plugin"
		}
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

func defaultContainerBuilderImage() string {
	if image := strings.TrimSpace(os.Getenv("MC_GATEWAY_PLUGIN_BUILDER_IMAGE")); image != "" {
		return image
	}
	version := strings.TrimPrefix(runtime.Version(), "go")
	if version == "" || strings.Contains(version, "devel") || strings.ContainsAny(version, " \t\n") {
		version = "1.25.0"
	}
	return strings.Join([]string{
		"ghcr.io/tursom/mc-gateway-plugin-builder:release",
		builderImageToken(GatewayRelease),
		builderImageToken(APIVersion),
		"go" + builderImageToken(version),
		runtime.GOOS,
		runtime.GOARCH,
	}, "-")
}

func buildDurationMS(start time.Time) int64 {
	return time.Since(start).Milliseconds()
}
