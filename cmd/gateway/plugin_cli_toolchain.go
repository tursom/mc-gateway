package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tursom/mc-gateway/internal/pluginmanager"
	"github.com/tursom/mc-gateway/plugin/api"
)

type pluginBuildCLIOptions struct {
	Dir        string
	BuildType  string
	Out        string
	FromSource string
	SkipTests  bool
	Vendor     bool
}

type pluginInitCLIOptions struct {
	Dir       string
	ID        string
	Name      string
	Template  string
	Runtime   string
	Module    string
	Extension string
}

type pluginGovernanceCLIOptions struct {
	Target              string
	ConfigPath          string
	ConfigJSON          string
	Profile             string
	Action              string
	Priority            int
	BenchmarkProfile    string
	P95MS               float64
	P99MS               float64
	ErrorRate           float64
	ActiveProxyCapacity int64
	BaselineDiff        float64
}

type pluginRuntimeCLIAdapter interface {
	RuntimeType() string
	Init(context.Context, pluginInitCLIOptions) error
	BuildBinary(context.Context, pluginBuildRuntimeRequest) (pluginmanager.ArtifactRecord, error)
	BuildSource(context.Context, pluginBuildRuntimeRequest) (pluginmanager.ArtifactRecord, error)
	Test(context.Context, pluginTestCLIOptions) error
}

type pluginBuildRuntimeRequest struct {
	Dir      string
	Manifest pluginmanager.Manifest
	Raw      map[string]any
	OutPath  string
	Vendor   bool
}

type pluginTestCLIOptions struct {
	Target      string
	Profile     string
	ConfigPath  string
	FixturePath string
	Manifest    pluginmanager.Manifest
}

type cliStaticRuntimeAdapter struct{}

func (cliStaticRuntimeAdapter) Load(context.Context, pluginmanager.ArtifactRecord, pluginmanager.PluginRecord, *pluginmanager.Gateway) (api.Plugin, error) {
	return nil, errors.New("local CLI governance adapter does not load plugin code")
}

type goPluginCLIAdapter struct{}

func (goPluginCLIAdapter) RuntimeType() string {
	return pluginmanager.RuntimeGoPlugin
}

func (goPluginCLIAdapter) Init(ctx context.Context, opts pluginInitCLIOptions) error {
	return writeGoPluginTemplate(ctx, opts)
}

func (goPluginCLIAdapter) BuildBinary(ctx context.Context, req pluginBuildRuntimeRequest) (pluginmanager.ArtifactRecord, error) {
	return buildBinaryPluginPackageContext(ctx, req.Dir, req.Manifest, req.Raw, req.OutPath)
}

func (goPluginCLIAdapter) BuildSource(ctx context.Context, req pluginBuildRuntimeRequest) (pluginmanager.ArtifactRecord, error) {
	return buildSourcePluginPackageContext(ctx, req.Dir, req.Manifest, req.Raw, req.OutPath, req.Vendor)
}

func (goPluginCLIAdapter) Test(ctx context.Context, opts pluginTestCLIOptions) error {
	for _, item := range strings.Split(opts.Profile, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		switch item {
		case "unit":
			if err := runGoCommand(ctx, opts.Target, "go", "test", "./..."); err != nil {
				return err
			}
		case "manifest":
			if _, err := validatePluginDirectoryForCLI(opts.Target); err != nil {
				return err
			}
		case "harness", "protocol-smoke", "conformance":
			if err := validateTestFileIfSet(opts.ConfigPath, "config"); err != nil {
				return err
			}
			if err := validateTestFileIfSet(opts.FixturePath, "fixture"); err != nil {
				return err
			}
			if _, err := validatePluginDirectoryForCLI(opts.Target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported test profile %q", item)
		}
	}
	return nil
}

func pluginCLIAdapterForRuntime(runtimeType string) (pluginRuntimeCLIAdapter, error) {
	if runtimeType == "" {
		runtimeType = pluginmanager.RuntimeGoPlugin
	}
	switch runtimeType {
	case pluginmanager.RuntimeGoPlugin:
		return goPluginCLIAdapter{}, nil
	case pluginmanager.RuntimeBuiltin, pluginmanager.RuntimeSandbox, pluginmanager.RuntimeWASM:
		return nil, fmt.Errorf("runtime %q is reserved; no CLI build/test adapter is implemented yet", runtimeType)
	default:
		return nil, fmt.Errorf("unsupported runtime %q", runtimeType)
	}
}

func runPluginFeaturesCLI(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("features does not accept positional arguments")
	}
	features := map[string]any{
		"schema_version": pluginmanager.SchemaVersion,
		"api_version":    pluginmanager.APIVersion,
		"artifact_types": []string{
			pluginmanager.ArtifactTypeBinary,
			pluginmanager.ArtifactTypeSource,
		},
		"runtime_types": []map[string]any{
			{"type": pluginmanager.RuntimeGoPlugin, "implemented": true, "entry": pluginmanager.RuntimeEntry},
			{"type": pluginmanager.RuntimeBuiltin, "implemented": false},
			{"type": pluginmanager.RuntimeSandbox, "implemented": false},
			{"type": pluginmanager.RuntimeWASM, "implemented": false, "entry": pluginmanager.RuntimeWASMEntry},
		},
		"service_modes": []map[string]any{
			{"mode": pluginmanager.PluginServiceModeInProcess, "implemented": true},
			{"mode": pluginmanager.PluginServiceModeGoPluginProcess, "implemented": false},
			{"mode": pluginmanager.PluginServiceModeSandboxProcess, "implemented": false},
		},
		"extension_points": []map[string]string{
			{"type": "hook", "key": pluginmanager.ExtensionUpstreamConnect},
			{"type": "hook", "key": pluginmanager.ExtensionRouteResolve},
			{"type": "provider", "key": pluginmanager.ExtensionRouteResolver},
			{"type": "hook", "key": pluginmanager.ExtensionRuleEvaluate},
			{"type": "hook", "key": pluginmanager.ExtensionConfigValidate},
			{"type": "hook", "key": pluginmanager.ExtensionStatusPing},
			{"type": "hook", "key": pluginmanager.ExtensionConnectionFilter},
			{"type": "hook", "key": pluginmanager.ExtensionHandshakeFilter},
			{"type": "subscriber", "key": pluginmanager.ExtensionEventSubscriber},
			{"type": "provider", "key": pluginmanager.ExtensionProvider},
			{"type": "provider", "key": pluginmanager.ExtensionAuthProvider},
			{"type": "provider", "key": pluginmanager.ExtensionAdminAuthProvider},
		},
		"build": map[string]any{
			"implemented_runtimes": []string{pluginmanager.RuntimeGoPlugin},
			"builder_types": []string{
				pluginmanager.BuilderTypeLocalProcess,
				pluginmanager.BuilderTypeContainer,
			},
			"default_runtime_entry": pluginmanager.RuntimeEntry,
			"default_build_entry":   pluginmanager.SourceBuildEntry,
		},
		"cli": map[string]any{
			"implemented_commands": []string{
				"init",
				"features",
				"manifest format",
				"manifest explain",
				"build",
				"test",
				"preflight",
				"self-test",
				"benchmark",
				"status",
				"upload",
				"enable",
				"disable",
				"delete",
				"rollback",
				"config validate",
				"secret check",
				"logs",
				"events",
				"metrics",
				"diagnose",
				"task list",
				"task run",
				"data inspect",
				"data gc",
				"files inspect",
				"files gc",
				"gc",
				"review status",
				"review approve",
				"review reject",
				"review override",
				"advisory scan",
				"advisory import",
				"repo list",
				"repo import",
				"sbom verify",
				"verify",
				"runtime features",
				"runtime status",
				"runtime mode",
				"runtime apply",
				"inspect",
				"validate",
				"compat",
				"source-validate",
				"source-build",
			},
			"reserved_commands": []string{
				"contract",
				"conformance",
				"schema export",
				"repo search",
				"repo show",
				"sbom generate",
				"sign",
				"promotion export/import/diff/drift/dr-drill",
				"task cancel",
			},
		},
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(features)
}

func runPluginManifestCLI(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: gateway plugin manifest format|explain ...")
	}
	switch args[0] {
	case "format":
		return runPluginManifestFormatCLI(args[1:])
	case "explain":
		return runPluginManifestExplainCLI(args[1:])
	default:
		return fmt.Errorf("unknown manifest command %q", args[0])
	}
}

func runPluginManifestFormatCLI(args []string) error {
	target := "manifest.json"
	write := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			if target != "manifest.json" {
				return fmt.Errorf("unexpected argument %q", arg)
			}
			target = arg
			continue
		}
		key, value, consumed, err := parsePluginCLIFlag(args, i)
		if err != nil {
			return err
		}
		i += consumed
		switch key {
		case "write":
			write = parsePluginBoolFlag(value)
		default:
			return fmt.Errorf("unknown manifest format flag --%s", key)
		}
	}
	manifestPath, err := resolveManifestPath(target)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("invalid manifest.json: %w", err)
	}
	formatted, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	formatted = append(formatted, '\n')
	if write {
		if bytes.Equal(data, formatted) {
			fmt.Fprintf(os.Stdout, "ok manifest=%s unchanged\n", manifestPath)
			return nil
		}
		if err := os.WriteFile(manifestPath, formatted, 0644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "ok manifest=%s formatted\n", manifestPath)
		return nil
	}
	_, err = os.Stdout.Write(formatted)
	return err
}

func runPluginManifestExplainCLI(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: gateway plugin manifest explain <field-or-key>")
	}
	key := strings.TrimSpace(args[0])
	explanation, ok := manifestExplanation(key)
	if !ok {
		return fmt.Errorf("no manifest explanation for %q", key)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(explanation)
}

func manifestExplanation(key string) (map[string]any, bool) {
	explanations := map[string]map[string]any{
		"schema_version": {
			"key":      "schema_version",
			"required": true,
			"value":    pluginmanager.SchemaVersion,
			"summary":  "Manifest schema contract version supported by this gateway.",
		},
		"artifact_type": {
			"key":      "artifact_type",
			"required": true,
			"values":   []string{pluginmanager.ArtifactTypeBinary, pluginmanager.ArtifactTypeSource},
			"summary":  "Declares whether the .mcgp package carries a built runtime artifact or source package.",
		},
		"runtime.type": {
			"key":      "runtime.type",
			"required": true,
			"values":   []string{pluginmanager.RuntimeGoPlugin, pluginmanager.RuntimeBuiltin, pluginmanager.RuntimeSandbox, pluginmanager.RuntimeWASM},
			"summary":  "Selects the runtime adapter. Only go-plugin build/test is implemented by this CLI slice.",
		},
		"runtime.entry": {
			"key":      "runtime.entry",
			"required": true,
			"default":  pluginmanager.RuntimeEntry,
			"summary":  "Path of the runtime entry inside a binary .mcgp package.",
		},
		"runtime.entry_symbol": {
			"key":     "runtime.entry_symbol",
			"default": "Plugin",
			"summary": "Go plugin factory symbol used to create a plugin instance.",
		},
		"build.entry": {
			"key":     "build.entry",
			"default": pluginmanager.SourceBuildEntry,
			"summary": "Go package path built from a source .mcgp package.",
		},
		"api_version": {
			"key":      "api_version",
			"required": true,
			"value":    pluginmanager.APIVersion,
			"summary":  "Plugin API contract version supported by this gateway.",
		},
		"extension_points": {
			"key":      "extension_points",
			"required": true,
			"summary":  "Declared extension points used for static validation, governance, conflict analysis, and Admin display.",
		},
		"upstream.connect/v1": {
			"key":     pluginmanager.ExtensionUpstreamConnect,
			"type":    "hook",
			"summary": "Extension point for replacing upstream connection creation or returning a protocol-proxy stream endpoint.",
		},
		"config_schema": {
			"key":     "config_schema",
			"summary": "JSON schema used by Admin UI and preflight to validate plugin configuration before enable/reload.",
		},
		"capabilities": {
			"key":     "capabilities",
			"summary": "Runtime, network, filesystem, Minecraft, and feature declarations used for review and policy gates.",
		},
	}
	explanation, ok := explanations[key]
	return explanation, ok
}

func runPluginPreflightCLI(args []string) error {
	opts, err := parseGovernanceCLIOptions(args)
	if err != nil {
		return err
	}
	if opts.Target == "" {
		return errors.New("preflight requires a plugin directory or binary .mcgp")
	}
	manager, cleanup, artifact, configJSON, err := prepareLocalGovernanceManager(opts)
	if err != nil {
		return err
	}
	defer cleanup()
	result, err := manager.RunPreflight(context.Background(), "cli", artifact.PluginID, pluginmanager.PreflightRequest{
		ArtifactID: artifact.ID,
		Profile:    opts.Profile,
		Action:     opts.Action,
		ConfigJSON: configJSON,
	})
	if err != nil {
		return err
	}
	return encodePluginCLIJSON(map[string]any{
		"plugin_id":   artifact.PluginID,
		"artifact_id": artifact.ID,
		"preflight":   result,
	})
}

func runPluginSelfTestCLI(args []string) error {
	opts, err := parseGovernanceCLIOptions(args)
	if err != nil {
		return err
	}
	if opts.Target == "" {
		return errors.New("self-test requires a plugin directory or binary .mcgp")
	}
	manager, cleanup, artifact, _, err := prepareLocalGovernanceManager(opts)
	if err != nil {
		return err
	}
	defer cleanup()
	result, err := manager.RunSelfTest(context.Background(), "cli", artifact.PluginID, pluginmanager.SelfTestRequest{
		ArtifactID: artifact.ID,
		Profile:    opts.Profile,
	})
	if err != nil {
		return err
	}
	return encodePluginCLIJSON(map[string]any{
		"plugin_id":   artifact.PluginID,
		"artifact_id": artifact.ID,
		"self_test":   result,
	})
}

func runPluginBenchmarkCLI(args []string) error {
	opts, err := parseGovernanceCLIOptions(args)
	if err != nil {
		return err
	}
	if opts.Target == "" {
		return errors.New("benchmark requires a plugin directory or binary .mcgp")
	}
	if opts.BenchmarkProfile == "" {
		opts.BenchmarkProfile = "local-fast"
	}
	manager, cleanup, artifact, configJSON, err := prepareLocalGovernanceManager(opts)
	if err != nil {
		return err
	}
	defer cleanup()
	record, err := manager.SaveBenchmark(context.Background(), "cli", pluginmanager.BenchmarkRequest{
		ArtifactID:          artifact.ID,
		Profile:             opts.Profile,
		BenchmarkProfile:    opts.BenchmarkProfile,
		P95MS:               opts.P95MS,
		P99MS:               opts.P99MS,
		ErrorRate:           opts.ErrorRate,
		ActiveProxyCapacity: opts.ActiveProxyCapacity,
		BaselineDiff:        opts.BaselineDiff,
	})
	if err != nil {
		return err
	}
	decision, err := manager.EvaluateGovernance(context.Background(), artifact.PluginID, artifact.ID, pluginmanager.GovernanceActionEnable, opts.Profile, configJSON)
	if err != nil {
		return err
	}
	return encodePluginCLIJSON(map[string]any{
		"plugin_id":   artifact.PluginID,
		"artifact_id": artifact.ID,
		"benchmark":   record,
		"decision":    decision,
	})
}

func runPluginInitCLI(args []string) error {
	opts := pluginInitCLIOptions{
		Template:  "upstream-dialer",
		Runtime:   pluginmanager.RuntimeGoPlugin,
		Extension: pluginmanager.ExtensionUpstreamConnect,
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			if opts.Dir != "" {
				return fmt.Errorf("unexpected argument %q", arg)
			}
			opts.Dir = arg
			continue
		}
		key, value, consumed, err := parsePluginCLIFlag(args, i)
		if err != nil {
			return err
		}
		i += consumed
		switch key {
		case "id":
			opts.ID = value
		case "name":
			opts.Name = value
		case "template":
			opts.Template = value
		case "runtime":
			opts.Runtime = value
		case "module":
			opts.Module = value
		case "extension":
			opts.Extension = value
		default:
			return fmt.Errorf("unknown init flag --%s", key)
		}
	}
	if opts.Dir == "" {
		return errors.New("plugin init requires a target directory")
	}
	if opts.ID == "" {
		opts.ID = filepath.Base(opts.Dir)
	}
	if opts.Name == "" {
		opts.Name = titleFromPluginID(opts.ID)
	}
	if opts.Module == "" {
		opts.Module = "example.com/" + opts.ID
	}
	adapter, err := pluginCLIAdapterForRuntime(opts.Runtime)
	if err != nil {
		return err
	}
	return adapter.Init(context.Background(), opts)
}

func runPluginBuildCLI(args []string) error {
	opts := pluginBuildCLIOptions{Dir: ".", BuildType: "binary", Vendor: true}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			if opts.Dir != "." {
				return fmt.Errorf("unexpected argument %q", arg)
			}
			opts.Dir = arg
			continue
		}
		key, value, consumed, err := parsePluginCLIFlag(args, i)
		if err != nil {
			return err
		}
		i += consumed
		switch key {
		case "type":
			opts.BuildType = value
		case "out":
			opts.Out = value
		case "from-source":
			opts.FromSource = value
		case "skip-tests":
			opts.SkipTests = parsePluginBoolFlag(value)
		case "vendor":
			opts.Vendor = parsePluginBoolFlag(value)
		default:
			return fmt.Errorf("unknown build flag --%s", key)
		}
	}
	if opts.FromSource != "" {
		build, out, err := buildSourcePackageForCLI(opts.FromSource, opts.Out)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "ok build=%d plugin=%s source_sha256=%s artifact_sha256=%s builder=%s go=%s status=%s out=%s\n",
			build.ID, build.PluginID, build.SourceSHA256, build.ArtifactSHA256, build.BuilderType, build.GoVersion, build.Status, out)
		return nil
	}
	switch opts.BuildType {
	case "binary", "source", "both":
	default:
		return fmt.Errorf("unsupported build --type %q", opts.BuildType)
	}
	if !opts.SkipTests {
		if err := runGoCommand(context.Background(), opts.Dir, "go", "test", "./..."); err != nil {
			return err
		}
	}
	manifest, raw, err := readPluginDirManifest(opts.Dir)
	if err != nil {
		return err
	}
	adapter, err := pluginCLIAdapterForRuntime(manifest.Runtime.Type)
	if err != nil {
		return err
	}
	outDir := opts.Out
	if outDir == "" || strings.HasSuffix(outDir, string(os.PathSeparator)) {
		if outDir == "" {
			outDir = filepath.Join(opts.Dir, "dist")
		}
	} else if opts.BuildType == "both" {
		return errors.New("--out must be a directory when --type both")
	}
	if opts.BuildType == "binary" || opts.BuildType == "both" {
		outPath := opts.Out
		if outPath == "" || opts.BuildType == "both" || strings.HasSuffix(outPath, string(os.PathSeparator)) {
			outPath = filepath.Join(outDir, manifest.ID+".mcgp")
		}
		artifact, err := adapter.BuildBinary(context.Background(), pluginBuildRuntimeRequest{
			Dir:      opts.Dir,
			Manifest: manifest,
			Raw:      raw,
			OutPath:  outPath,
			Vendor:   opts.Vendor,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "ok plugin=%s version=%s sha256=%s api=%s go=%s %s/%s out=%s\n",
			artifact.PluginID, artifact.Version, artifact.SHA256, artifact.APIVersion, artifact.GoVersion, artifact.GOOS, artifact.GOARCH, outPath)
	}
	if opts.BuildType == "source" || opts.BuildType == "both" {
		outPath := opts.Out
		if outPath == "" || opts.BuildType == "both" || strings.HasSuffix(outPath, string(os.PathSeparator)) {
			outPath = filepath.Join(outDir, manifest.ID+"-source.mcgp")
		}
		source, err := adapter.BuildSource(context.Background(), pluginBuildRuntimeRequest{
			Dir:      opts.Dir,
			Manifest: manifest,
			Raw:      raw,
			OutPath:  outPath,
			Vendor:   opts.Vendor,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "ok source plugin=%s version=%s source_sha256=%s api=%s go=%s %s/%s out=%s\n",
			source.PluginID, source.Version, source.SHA256, source.APIVersion, source.GoVersion, source.GOOS, source.GOARCH, outPath)
	}
	return nil
}

func runPluginTestCLI(args []string) error {
	target := "."
	profile := "unit,manifest"
	configPath := ""
	fixturePath := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			if target != "." {
				return fmt.Errorf("unexpected argument %q", arg)
			}
			target = arg
			continue
		}
		key, value, consumed, err := parsePluginCLIFlag(args, i)
		if err != nil {
			return err
		}
		i += consumed
		switch key {
		case "profile":
			profile = value
		case "config":
			configPath = value
		case "fixture":
			fixturePath = value
		default:
			return fmt.Errorf("unknown test flag --%s", key)
		}
	}
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		for _, item := range strings.Split(profile, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			switch item {
			case "manifest", "compat", "conformance":
				if _, err := validatePluginPathForCLI(target, ""); err != nil {
					return err
				}
			default:
				return fmt.Errorf("test profile %q requires a plugin source directory", item)
			}
		}
		fmt.Fprintf(os.Stdout, "ok test target=%s profile=%s\n", target, profile)
		return nil
	}
	manifest, _, err := readPluginDirManifest(target)
	if err != nil {
		return err
	}
	adapter, err := pluginCLIAdapterForRuntime(manifest.Runtime.Type)
	if err != nil {
		return err
	}
	if err := adapter.Test(context.Background(), pluginTestCLIOptions{
		Target:      target,
		Profile:     profile,
		ConfigPath:  configPath,
		FixturePath: fixturePath,
		Manifest:    manifest,
	}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "ok test target=%s profile=%s\n", target, profile)
	return nil
}

func parseGovernanceCLIOptions(args []string) (pluginGovernanceCLIOptions, error) {
	opts := pluginGovernanceCLIOptions{
		Profile:  pluginmanager.PolicyProfileDev,
		Action:   pluginmanager.GovernanceActionEnable,
		Priority: pluginmanager.DefaultPriority,
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			if opts.Target != "" {
				return pluginGovernanceCLIOptions{}, fmt.Errorf("unexpected argument %q", arg)
			}
			opts.Target = arg
			continue
		}
		key, value, consumed, err := parsePluginCLIFlag(args, i)
		if err != nil {
			return pluginGovernanceCLIOptions{}, err
		}
		i += consumed
		switch key {
		case "config":
			opts.ConfigPath = value
		case "config-json":
			opts.ConfigJSON = value
		case "profile":
			opts.Profile = value
		case "action":
			opts.Action = value
		case "priority":
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return pluginGovernanceCLIOptions{}, fmt.Errorf("invalid --priority %q: %w", value, err)
			}
			opts.Priority = parsed
		case "benchmark-profile":
			opts.BenchmarkProfile = value
		case "p95-ms":
			parsed, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return pluginGovernanceCLIOptions{}, fmt.Errorf("invalid --p95-ms %q: %w", value, err)
			}
			opts.P95MS = parsed
		case "p99-ms":
			parsed, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return pluginGovernanceCLIOptions{}, fmt.Errorf("invalid --p99-ms %q: %w", value, err)
			}
			opts.P99MS = parsed
		case "error-rate":
			parsed, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return pluginGovernanceCLIOptions{}, fmt.Errorf("invalid --error-rate %q: %w", value, err)
			}
			opts.ErrorRate = parsed
		case "active-proxy-capacity":
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return pluginGovernanceCLIOptions{}, fmt.Errorf("invalid --active-proxy-capacity %q: %w", value, err)
			}
			opts.ActiveProxyCapacity = parsed
		case "baseline-diff":
			parsed, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return pluginGovernanceCLIOptions{}, fmt.Errorf("invalid --baseline-diff %q: %w", value, err)
			}
			opts.BaselineDiff = parsed
		default:
			return pluginGovernanceCLIOptions{}, fmt.Errorf("unknown governance flag --%s", key)
		}
	}
	if opts.ConfigPath != "" && opts.ConfigJSON != "" {
		return pluginGovernanceCLIOptions{}, errors.New("--config and --config-json are mutually exclusive")
	}
	return opts, nil
}

func prepareLocalGovernanceManager(opts pluginGovernanceCLIOptions) (*pluginmanager.Manager, func(), pluginmanager.ArtifactRecord, string, error) {
	tmpRoot, err := os.MkdirTemp("", "mcgp-governance-cli-*")
	if err != nil {
		return nil, func() {}, pluginmanager.ArtifactRecord{}, "", err
	}
	cleanup := func() { _ = os.RemoveAll(tmpRoot) }
	targetPath := opts.Target
	info, err := os.Stat(targetPath)
	if err != nil {
		cleanup()
		return nil, func() {}, pluginmanager.ArtifactRecord{}, "", err
	}
	if info.IsDir() {
		manifest, raw, err := readPluginDirManifest(targetPath)
		if err != nil {
			cleanup()
			return nil, func() {}, pluginmanager.ArtifactRecord{}, "", err
		}
		targetPath = filepath.Join(tmpRoot, manifest.ID+".mcgp")
		if _, err := buildBinaryPluginPackage(opts.Target, manifest, raw, targetPath); err != nil {
			cleanup()
			return nil, func() {}, pluginmanager.ArtifactRecord{}, "", err
		}
	}
	db, err := openPluginCLIDB(filepath.Join(tmpRoot, "plugins.db"))
	if err != nil {
		cleanup()
		return nil, func() {}, pluginmanager.ArtifactRecord{}, "", err
	}
	manager := pluginmanager.New(pluginmanager.Options{
		DB:            db,
		ArtifactRoot:  filepath.Join(tmpRoot, "artifacts"),
		Adapter:       cliStaticRuntimeAdapter{},
		PolicyProfile: opts.Profile,
	})
	artifact, err := manager.UploadArtifact(context.Background(), pluginmanager.ArtifactUpload{
		SourcePath: targetPath,
		FileName:   filepath.Base(targetPath),
		Actor:      "cli",
	})
	if err != nil {
		_ = db.Close()
		cleanup()
		return nil, func() {}, pluginmanager.ArtifactRecord{}, "", err
	}
	if artifact.ArtifactType != pluginmanager.ArtifactTypeBinary {
		_ = db.Close()
		cleanup()
		return nil, func() {}, pluginmanager.ArtifactRecord{}, "", fmt.Errorf("governance target must be a binary artifact, got %q", artifact.ArtifactType)
	}
	configJSON, err := governanceConfigJSON(opts)
	if err != nil {
		_ = db.Close()
		cleanup()
		return nil, func() {}, pluginmanager.ArtifactRecord{}, "", err
	}
	if _, err := manager.SetDesired(context.Background(), "cli", artifact.PluginID, artifact.ID, pluginmanager.DesiredDisabled, configJSON, opts.Priority); err != nil {
		_ = db.Close()
		cleanup()
		return nil, func() {}, pluginmanager.ArtifactRecord{}, "", err
	}
	return manager, func() {
		_ = db.Close()
		cleanup()
	}, artifact, configJSON, nil
}

func governanceConfigJSON(opts pluginGovernanceCLIOptions) (string, error) {
	if opts.ConfigJSON != "" {
		if !json.Valid([]byte(opts.ConfigJSON)) {
			return "", errors.New("--config-json must be valid JSON")
		}
		return opts.ConfigJSON, nil
	}
	if opts.ConfigPath != "" {
		data, err := os.ReadFile(opts.ConfigPath)
		if err != nil {
			return "", err
		}
		if !json.Valid(data) {
			return "", fmt.Errorf("config file %q must contain valid JSON", opts.ConfigPath)
		}
		return string(data), nil
	}
	if info, err := os.Stat(opts.Target); err == nil && info.IsDir() {
		defaultConfig := filepath.Join(opts.Target, "testdata", "config.json")
		if data, err := os.ReadFile(defaultConfig); err == nil {
			if !json.Valid(data) {
				return "", fmt.Errorf("default config file %q must contain valid JSON", defaultConfig)
			}
			return string(data), nil
		}
	}
	return "{}", nil
}

func encodePluginCLIJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func validatePluginPathForCLI(targetPath, expectedArtifactType string) (pluginmanager.ArtifactRecord, error) {
	info, err := os.Stat(targetPath)
	if err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	if info.IsDir() {
		artifact, err := validatePluginDirectoryForCLI(targetPath)
		if err != nil {
			return pluginmanager.ArtifactRecord{}, err
		}
		if expectedArtifactType != "" && artifact.ArtifactType != expectedArtifactType {
			return pluginmanager.ArtifactRecord{}, fmt.Errorf("artifact_type %q does not match expected %q", artifact.ArtifactType, expectedArtifactType)
		}
		return artifact, nil
	}
	if filepath.Base(targetPath) == "manifest.json" {
		dir := filepath.Dir(targetPath)
		artifact, err := validatePluginDirectoryForCLI(dir)
		if err != nil {
			return pluginmanager.ArtifactRecord{}, err
		}
		if expectedArtifactType != "" && artifact.ArtifactType != expectedArtifactType {
			return pluginmanager.ArtifactRecord{}, fmt.Errorf("artifact_type %q does not match expected %q", artifact.ArtifactType, expectedArtifactType)
		}
		return artifact, nil
	}
	tmpRoot, err := os.MkdirTemp("", "mcgp-cli-*")
	if err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	defer os.RemoveAll(tmpRoot)
	store := pluginmanager.NewArtifactStore(tmpRoot)
	upload := pluginmanager.ArtifactUpload{SourcePath: targetPath, FileName: filepath.Base(targetPath), Actor: "cli"}
	if expectedArtifactType == pluginmanager.ArtifactTypeSource {
		return store.ValidateAndStoreSource(upload)
	}
	if expectedArtifactType == pluginmanager.ArtifactTypeBinary {
		return store.ValidateAndStoreBinary(upload)
	}
	return store.ValidateAndStore(upload)
}

func validatePluginDirectoryForCLI(dir string) (pluginmanager.ArtifactRecord, error) {
	manifest, raw, err := readPluginDirManifest(dir)
	if err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	tmpRoot, err := os.MkdirTemp("", "mcgp-dir-validate-*")
	if err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	defer os.RemoveAll(tmpRoot)
	packagePath := filepath.Join(tmpRoot, manifest.ID+"-source.mcgp")
	if _, err := writeSourcePackage(dir, manifest, raw, packagePath, false); err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	store := pluginmanager.NewArtifactStore(filepath.Join(tmpRoot, "store"))
	return store.ValidateAndStoreSource(pluginmanager.ArtifactUpload{SourcePath: packagePath, FileName: filepath.Base(packagePath), Actor: "cli"})
}

func buildBinaryPluginPackage(dir string, manifest pluginmanager.Manifest, raw map[string]any, outPath string) (pluginmanager.ArtifactRecord, error) {
	return buildBinaryPluginPackageContext(context.Background(), dir, manifest, raw, outPath)
}

func buildBinaryPluginPackageContext(ctx context.Context, dir string, manifest pluginmanager.Manifest, raw map[string]any, outPath string) (pluginmanager.ArtifactRecord, error) {
	tmpRoot, err := os.MkdirTemp("", "mcgp-build-binary-*")
	if err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	defer os.RemoveAll(tmpRoot)
	pluginPath := filepath.Join(tmpRoot, pluginmanager.RuntimeEntry)
	buildEntry := sourceBuildEntryForCLI(manifest)
	if err := runGoCommand(ctx, dir, "go", "build", "-buildmode=plugin", "-trimpath", "-buildvcs=false", "-o", pluginPath, buildEntry); err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	if err := validatePluginSymbol(ctx, dir, pluginPath, manifest); err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	manifestBytes, err := materializedManifestJSON(raw, manifest, pluginmanager.ArtifactTypeBinary, false)
	if err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	if err := writeBinaryPackage(pluginPath, manifestBytes, dir, outPath); err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	return validatePluginPathForCLI(outPath, pluginmanager.ArtifactTypeBinary)
}

func buildSourcePluginPackage(dir string, manifest pluginmanager.Manifest, raw map[string]any, outPath string, vendor bool) (pluginmanager.ArtifactRecord, error) {
	return buildSourcePluginPackageContext(context.Background(), dir, manifest, raw, outPath, vendor)
}

func buildSourcePluginPackageContext(ctx context.Context, dir string, manifest pluginmanager.Manifest, raw map[string]any, outPath string, vendor bool) (pluginmanager.ArtifactRecord, error) {
	if _, err := writeSourcePackageContext(ctx, dir, manifest, raw, outPath, vendor); err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	return validatePluginPathForCLI(outPath, pluginmanager.ArtifactTypeSource)
}

func writeGoPluginTemplate(ctx context.Context, opts pluginInitCLIOptions) error {
	_ = ctx
	if err := os.MkdirAll(opts.Dir, 0755); err != nil {
		return err
	}
	existing, err := os.ReadDir(opts.Dir)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		return fmt.Errorf("target directory %q is not empty", opts.Dir)
	}
	repoRoot, err := findRepoRoot()
	if err != nil {
		return err
	}
	files := map[string]string{
		"go.mod":               goModTemplate(opts.Module, repoRoot),
		"main.go":              goPluginMainTemplate(opts),
		"main_test.go":         goPluginTestTemplate(),
		"README.md":            readmeTemplate(opts),
		"manifest.json":        manifestTemplate(opts),
		"testdata/config.json": configTemplate(opts.Template),
	}
	for name, content := range files {
		target := filepath.Join(opts.Dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(target, []byte(content), 0644); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stdout, "ok init plugin=%s template=%s dir=%s\n", opts.ID, opts.Template, opts.Dir)
	return nil
}

func readPluginDirManifest(dir string) (pluginmanager.Manifest, map[string]any, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return pluginmanager.Manifest{}, nil, err
	}
	var manifest pluginmanager.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return pluginmanager.Manifest{}, nil, fmt.Errorf("invalid manifest.json: %w", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return pluginmanager.Manifest{}, nil, fmt.Errorf("invalid manifest.json object: %w", err)
	}
	if manifest.ID == "" {
		return pluginmanager.Manifest{}, nil, errors.New("manifest id is required")
	}
	return manifest, raw, nil
}

func resolveManifestPath(target string) (string, error) {
	info, err := os.Stat(target)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return filepath.Join(target, "manifest.json"), nil
	}
	if filepath.Base(target) != "manifest.json" {
		return "", fmt.Errorf("manifest path must be manifest.json or a plugin directory, got %q", target)
	}
	return target, nil
}

func materializedManifestJSON(raw map[string]any, manifest pluginmanager.Manifest, artifactType string, vendor bool) ([]byte, error) {
	clone := make(map[string]any, len(raw)+4)
	for key, value := range raw {
		clone[key] = value
	}
	clone["artifact_type"] = artifactType
	clone["go_version"] = runtime.Version()
	clone["go_os"] = runtime.GOOS
	clone["go_arch"] = runtime.GOARCH
	runtimeMap, _ := clone["runtime"].(map[string]any)
	if runtimeMap == nil {
		runtimeMap = make(map[string]any)
	}
	if runtimeMap["type"] == nil || runtimeMap["type"] == "" {
		runtimeMap["type"] = pluginmanager.RuntimeGoPlugin
	}
	if runtimeMap["entry_symbol"] == nil || runtimeMap["entry_symbol"] == "" {
		runtimeMap["entry_symbol"] = "Plugin"
	}
	if artifactType == pluginmanager.ArtifactTypeBinary {
		runtimeMap["entry"] = pluginmanager.RuntimeEntry
	} else if runtimeMap["entry"] == nil || runtimeMap["entry"] == "" {
		runtimeMap["entry"] = pluginmanager.RuntimeEntry
	}
	clone["runtime"] = runtimeMap
	if artifactType == pluginmanager.ArtifactTypeSource {
		buildMap, _ := clone["build"].(map[string]any)
		if buildMap == nil {
			buildMap = make(map[string]any)
		}
		if buildMap["type"] == nil || buildMap["type"] == "" {
			buildMap["type"] = pluginmanager.BuildTypeGo
		}
		if buildMap["entry"] == nil || buildMap["entry"] == "" {
			buildMap["entry"] = sourceBuildEntryForCLI(manifest)
		}
		if buildMap["output"] == nil || buildMap["output"] == "" {
			buildMap["output"] = pluginmanager.RuntimeEntry
		}
		if buildMap["go_version"] == nil || buildMap["go_version"] == "" {
			buildMap["go_version"] = runtime.Version()
		}
		if buildMap["tags"] == nil {
			buildMap["tags"] = []string{}
		}
		if buildMap["vendor_required"] == nil {
			buildMap["vendor_required"] = false
		}
		clone["build"] = buildMap
	}
	data, err := json.MarshalIndent(clone, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func writeBinaryPackage(pluginPath string, manifestBytes []byte, sourceDir, outPath string) error {
	if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
		return err
	}
	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	if err := addZipBytes(zw, "manifest.json", manifestBytes); err != nil {
		zw.Close()
		return err
	}
	if err := addZipFileStable(zw, pluginmanager.RuntimeEntry, pluginPath); err != nil {
		zw.Close()
		return err
	}
	for _, name := range optionalPackageDocs(sourceDir) {
		if err := addZipFileStable(zw, filepath.ToSlash(name), filepath.Join(sourceDir, name)); err != nil {
			zw.Close()
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return out.Close()
}

func writeSourcePackage(dir string, manifest pluginmanager.Manifest, raw map[string]any, outPath string, vendor bool) (string, error) {
	return writeSourcePackageContext(context.Background(), dir, manifest, raw, outPath, vendor)
}

func writeSourcePackageContext(ctx context.Context, dir string, manifest pluginmanager.Manifest, raw map[string]any, outPath string, vendor bool) (string, error) {
	manifestBytes, err := materializedManifestJSON(raw, manifest, pluginmanager.ArtifactTypeSource, vendor)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
		return "", err
	}
	tmpRoot, err := os.MkdirTemp("", "mcgp-source-package-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpRoot)
	var vendorDir string
	if vendor {
		vendorDir = filepath.Join(tmpRoot, "vendor")
		if err := runGoCommand(ctx, dir, "go", "mod", "vendor", "-o", vendorDir); err != nil {
			return "", err
		}
	}
	files, err := collectSourcePackageFiles(dir, vendorDir)
	if err != nil {
		return "", err
	}
	out, err := os.Create(outPath)
	if err != nil {
		return "", err
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	if err := addZipBytes(zw, "manifest.json", manifestBytes); err != nil {
		zw.Close()
		return "", err
	}
	for _, file := range files {
		if err := addZipFileStable(zw, file.Name, file.Path); err != nil {
			zw.Close()
			return "", err
		}
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	sum, err := fileSHA256ForCLI(outPath)
	if err != nil {
		return "", err
	}
	return sum, nil
}

type sourcePackageFile struct {
	Name string
	Path string
}

func collectSourcePackageFiles(dir, vendorDir string) ([]sourcePackageFile, error) {
	var files []sourcePackageFile
	err := filepath.WalkDir(dir, func(filePath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, filePath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			switch rel {
			case ".git", "dist", "vendor":
				return filepath.SkipDir
			}
			if strings.HasPrefix(path.Base(rel), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if rel == "manifest.json" {
			return nil
		}
		if sourcePackageEntryAllowed(rel) {
			files = append(files, sourcePackageFile{Name: rel, Path: filePath})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if vendorDir != "" {
		err = filepath.WalkDir(vendorDir, func(filePath string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(vendorDir, filePath)
			if err != nil {
				return err
			}
			files = append(files, sourcePackageFile{Name: path.Join("vendor", filepath.ToSlash(rel)), Path: filePath})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, nil
}

func sourcePackageEntryAllowed(name string) bool {
	base := path.Base(name)
	lower := strings.ToLower(base)
	switch {
	case name == "go.mod", name == "go.sum":
		return true
	case strings.HasSuffix(name, ".go"):
		return true
	case strings.EqualFold(base, "README.md"), strings.EqualFold(base, "LICENSE"):
		return true
	case strings.Contains(lower, "sbom"):
		return true
	case strings.HasPrefix(name, "testdata/"):
		return true
	default:
		return false
	}
}

func optionalPackageDocs(sourceDir string) []string {
	var docs []string
	for _, name := range []string{"README.md", "LICENSE"} {
		if info, err := os.Stat(filepath.Join(sourceDir, name)); err == nil && !info.IsDir() {
			docs = append(docs, name)
		}
	}
	return docs
}

func validatePluginSymbol(ctx context.Context, dir, pluginPath string, manifest pluginmanager.Manifest) error {
	output, err := commandOutputForCLI(ctx, dir, "go", "tool", "nm", pluginPath)
	if err != nil {
		return fmt.Errorf("inspect built plugin symbols: %w\n%s", err, output)
	}
	entrySymbol := manifest.Runtime.EntrySymbol
	if entrySymbol == "" {
		entrySymbol = "Plugin"
	}
	if !strings.Contains(output, entrySymbol) {
		return fmt.Errorf("built plugin is missing entry symbol %q", entrySymbol)
	}
	return nil
}

func runGoCommand(ctx context.Context, dir string, name string, args ...string) error {
	output, err := commandOutputForCLI(ctx, dir, name, args...)
	if err != nil {
		if strings.TrimSpace(output) == "" {
			return err
		}
		return fmt.Errorf("%s %s failed: %w\n%s", name, strings.Join(args, " "), err, output)
	}
	return nil
}

func commandOutputForCLI(ctx context.Context, dir string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

func addZipBytes(zw *zip.Writer, name string, data []byte) error {
	header := &zip.FileHeader{Name: name, Method: zip.Deflate}
	header.SetMode(0644)
	header.Modified = time.Unix(0, 0).UTC()
	writer, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = writer.Write(data)
	return err
}

func addZipFileStable(zw *zip.Writer, name, filePath string) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	mode := fs.FileMode(0644)
	if info, err := os.Stat(filePath); err == nil {
		mode = info.Mode().Perm()
	}
	header := &zip.FileHeader{Name: filepath.ToSlash(name), Method: zip.Deflate}
	header.SetMode(mode)
	header.Modified = time.Unix(0, 0).UTC()
	writer, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = writer.Write(data)
	return err
}

func parsePluginCLIFlag(args []string, index int) (string, string, int, error) {
	arg := args[index]
	if !strings.HasPrefix(arg, "--") {
		return "", "", 0, fmt.Errorf("expected flag, got %q", arg)
	}
	body := strings.TrimPrefix(arg, "--")
	if body == "" {
		return "", "", 0, fmt.Errorf("invalid empty flag %q", arg)
	}
	if key, value, ok := strings.Cut(body, "="); ok {
		return key, value, 0, nil
	}
	if isPluginBoolFlag(body) {
		return body, "true", 0, nil
	}
	if index+1 >= len(args) || strings.HasPrefix(args[index+1], "--") {
		return "", "", 0, fmt.Errorf("flag --%s requires a value", body)
	}
	return body, args[index+1], 1, nil
}

func isPluginBoolFlag(name string) bool {
	switch name {
	case "skip-tests", "vendor", "json", "quiet", "dry-run", "write", "source", "full-desired":
		return true
	default:
		return false
	}
}

func parsePluginBoolFlag(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "1", "t", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func sourceBuildEntryForCLI(manifest pluginmanager.Manifest) string {
	entry := strings.TrimSpace(manifest.Build.Entry)
	if entry == "" {
		entry = strings.TrimSpace(manifest.Runtime.BuildEntry)
	}
	if entry == "" {
		entry = pluginmanager.SourceBuildEntry
	}
	return entry
}

func validateTestFileIfSet(filePath, label string) error {
	if filePath == "" {
		return nil
	}
	info, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("%s %q is not readable: %w", label, filePath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s %q is a directory", label, filePath)
	}
	return nil
}

func fileSHA256ForCLI(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func titleFromPluginID(id string) string {
	parts := strings.FieldsFunc(id, func(r rune) bool { return r == '-' || r == '_' || r == '.' })
	for i, part := range parts {
		if part == "" {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	if len(parts) == 0 {
		return id
	}
	return strings.Join(parts, " ")
}

func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.Contains(string(data), "module github.com/tursom/mc-gateway") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("could not locate mc-gateway repository root")
		}
		dir = parent
	}
}

func goModTemplate(module, repoRoot string) string {
	return fmt.Sprintf(`module %s

go 1.24.0

require github.com/tursom/mc-gateway v0.0.0

replace github.com/tursom/mc-gateway => %s
`, module, filepath.ToSlash(repoRoot))
}

func manifestTemplate(opts pluginInitCLIOptions) string {
	mode := pluginmanager.UpstreamModeDialer
	if opts.Template == "protocol-proxy" {
		mode = pluginmanager.UpstreamModeProtocolProxy
	}
	manifest := fmt.Sprintf(`{
  "schema_version": "mc-gateway.plugin/v1",
  "id": %q,
  "name": %q,
  "version": "0.1.0",
  "description": %q,
  "artifact_type": "source",
  "runtime": {
    "type": "go-plugin",
    "entry": "plugin.so",
    "entry_symbol": "Plugin"
  },
  "build": {
    "type": "go",
    "entry": ".",
    "output": "plugin.so",
    "tags": [],
    "vendor_required": false
  },
  "api_version": "plugin-api/v1",
  "sdk_module": "github.com/tursom/mc-gateway/plugin/api",
  "sdk_module_version": "v0.1.0",
  "extension_points": [
    { "type": "hook", "key": %q }
  ],
  "capabilities": {
    "upstream_connect": { "mode": %q }
  },
  "runtime_limits": {
    "handler_timeout_ms": 3000,
    "initial_write_timeout_ms": 1000
  },
  "config_schema": {
    "type": "object",
    "properties": {
      "match_host": { "type": "string" },
      "upstream": { "type": "string" }
    }
  }
}
`, opts.ID, opts.Name, opts.Name+" plugin.", opts.Extension, mode)
	return manifest
}

func goPluginMainTemplate(opts pluginInitCLIOptions) string {
	if opts.Template == "protocol-proxy" {
		return protocolProxyMainTemplate()
	}
	return upstreamDialerMainTemplate()
}

func upstreamDialerMainTemplate() string {
	return `package main

import (
	"net"

	"github.com/tursom/mc-gateway/plugin/api"
)

type PluginImpl struct {
	api.AbstractPlugin
	config Config
}

type Config struct {
	MatchHost string ` + "`json:\"match_host\"`" + `
	Upstream  string ` + "`json:\"upstream\"`" + `
}

func Plugin() api.Plugin {
	return &PluginImpl{}
}

func (p *PluginImpl) NewConfigObj() any {
	return &Config{}
}

func (p *PluginImpl) ReloadConfig(config any) error {
	if cfg, ok := config.(*Config); ok {
		p.config = *cfg
	}
	return nil
}

func (p *PluginImpl) Init(gateway api.Gateway) error {
	return api.RegisterHookHandler(
		gateway,
		api.HookUpstreamConnect,
		func(req api.UpstreamConnectRequest) bool {
			return p.config.MatchHost == "" || req.Host == p.config.MatchHost || req.Upstream == p.config.MatchHost
		},
		func(req api.UpstreamConnectRequest) (net.Conn, error) {
			if p.config.Upstream == "" {
				return nil, api.ErrPass
			}
			if p.config.MatchHost != "" && req.Host != p.config.MatchHost && req.Upstream != p.config.MatchHost {
				return nil, api.ErrPass
			}
			return net.Dial("tcp", p.config.Upstream)
		},
	)
}
`
}

func protocolProxyMainTemplate() string {
	return `package main

import (
	"net"

	"github.com/tursom/mc-gateway/plugin/api"
)

type PluginImpl struct {
	api.AbstractPlugin
	config Config
}

type Config struct {
	MatchHost string ` + "`json:\"match_host\"`" + `
}

func Plugin() api.Plugin {
	return &PluginImpl{}
}

func (p *PluginImpl) NewConfigObj() any {
	return &Config{}
}

func (p *PluginImpl) ReloadConfig(config any) error {
	if cfg, ok := config.(*Config); ok {
		p.config = *cfg
	}
	return nil
}

func (p *PluginImpl) Init(gateway api.Gateway) error {
	return api.RegisterHookHandler(
		gateway,
		api.HookUpstreamConnect,
		func(req api.UpstreamConnectRequest) bool {
			return p.config.MatchHost == "" || req.Host == p.config.MatchHost
		},
		func(req api.UpstreamConnectRequest) (net.Conn, error) {
			return nil, api.ErrPass
		},
	)
}
`
}

func goPluginTestTemplate() string {
	return `package main

import (
	"testing"

	"github.com/tursom/mc-gateway/plugin/api"
)

func TestPluginFactory(t *testing.T) {
	if plugin := Plugin(); plugin == nil {
		t.Fatal("Plugin() returned nil")
	} else if _, ok := plugin.(api.Plugin); !ok {
		t.Fatal("Plugin() did not return api.Plugin")
	}
}
`
}

func readmeTemplate(opts pluginInitCLIOptions) string {
	return fmt.Sprintf(`# %s

Build and test:

`+"```sh"+`
gateway plugin test .
gateway plugin build . --type both
`+"```"+`
`, opts.Name)
}

func configTemplate(template string) string {
	if template == "protocol-proxy" {
		return "{\n  \"match_host\": \"play.example\"\n}\n"
	}
	return "{\n  \"match_host\": \"play.example\",\n  \"upstream\": \"127.0.0.1:25566\"\n}\n"
}
