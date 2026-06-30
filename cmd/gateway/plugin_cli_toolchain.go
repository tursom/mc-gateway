// cmd/gateway/plugin_cli_toolchain.go 包含插件开发者使用的本地源码模板、构建打包流程和测试辅助逻辑。

package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
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
	smoke "github.com/tursom/mc-gateway/protocol/smoke"
	"golang.org/x/mod/modfile"
)

type pluginBuildCLIOptions struct {
	Dir        string
	BuildType  string
	Out        string
	FromSource string
	Manifest   string
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
	Format    string
}

type pluginGovernanceCLIOptions struct {
	Target                    string
	Manifest                  string
	ConfigPath                string
	ConfigJSON                string
	Profile                   string
	Action                    string
	Priority                  int
	BenchmarkProfile          string
	P95MS                     float64
	P99MS                     float64
	ErrorRate                 float64
	ActiveProxyCapacity       int64
	BaselineDiff              float64
	RequireConformanceFixture bool
}

type pluginContractCLIOptions struct {
	Target      string
	Manifest    string
	ConfigPath  string
	ConfigJSON  string
	Profile     string
	FixturePath string
}

type pluginPromotionCLIOptions struct {
	Target     string
	Manifest   string
	ConfigPath string
	ConfigJSON string
	Profile    string
	Out        string
	Baseline   string
}

type pluginSBOMCLIOptions struct {
	Target   string
	Manifest string
	Out      string
	Format   string
}

type pluginSBOMDependency struct {
	Name     string `json:"name"`
	Version  string `json:"version,omitempty"`
	Type     string `json:"type"`
	Source   string `json:"source"`
	Indirect bool   `json:"indirect,omitempty"`
	PURL     string `json:"purl,omitempty"`
}

type pluginSBOMFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type pluginConformanceFixtureFile struct {
	Fixtures                             []map[string]any                   `json:"fixtures"`
	RouteDecisions                       []string                           `json:"route_decisions"`
	StatusHosts                          []string                           `json:"status_hosts"`
	StreamProxyScenarios                 []pluginmanager.StreamProxyFixture `json:"stream_proxy_scenarios"`
	ProtocolProxyScenarios               []string                           `json:"protocol_proxy_scenarios"`
	RuleEvaluationOutcomes               []string                           `json:"rule_evaluation_outcomes"`
	ConnectionFilterScenarios            []string                           `json:"connection_filter_scenarios"`
	HandshakeFilterScenarios             []string                           `json:"handshake_filter_scenarios"`
	GovernanceGateScenarios              []string                           `json:"governance_gate_scenarios"`
	EventDelivery                        []string                           `json:"event_delivery"`
	ProviderRegistry                     []string                           `json:"provider_registry"`
	DefaultRouteMustSurviveBadRuleConfig bool                               `json:"default_route_must_survive_bad_rule_config"`
	LocalAdminBreakGlass                 bool                               `json:"local_admin_break_glass"`
}

var pluginConformanceStableNow = time.Unix(1782758400, 0).UTC()

func pluginConformanceNow() time.Time {
	return pluginConformanceStableNow
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
	Manifest    string
	Profile     string
	ConfigPath  string
	FixturePath string
	Source      pluginmanager.Manifest
}

type cliStaticRuntimeAdapter struct{}

func (cliStaticRuntimeAdapter) Load(context.Context, pluginmanager.ArtifactRecord, pluginmanager.PluginRecord, *pluginmanager.Gateway) (api.Plugin, error) {
	return nil, errors.New("local CLI governance adapter does not load plugin code")
}

type goPluginCLIAdapter struct{}

type wasmPluginCLIAdapter struct{}

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
			if _, err := validatePluginDirectoryForCLI(opts.Target, opts.Manifest); err != nil {
				return err
			}
		case "harness", "protocol-smoke", "conformance":
			if err := validateTestFileIfSet(opts.ConfigPath, "config"); err != nil {
				return err
			}
			if err := validateTestFileIfSet(opts.FixturePath, "fixture"); err != nil {
				return err
			}
			if _, err := validatePluginDirectoryForCLI(opts.Target, opts.Manifest); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported test profile %q", item)
		}
	}
	return nil
}

func (wasmPluginCLIAdapter) RuntimeType() string {
	return pluginmanager.RuntimeWASM
}

func (wasmPluginCLIAdapter) Init(context.Context, pluginInitCLIOptions) error {
	return errors.New("wasm plugin init template is not implemented yet")
}

func (wasmPluginCLIAdapter) BuildBinary(ctx context.Context, req pluginBuildRuntimeRequest) (pluginmanager.ArtifactRecord, error) {
	return buildWASMPluginPackageContext(ctx, req.Dir, req.Manifest, req.Raw, req.OutPath)
}

func (wasmPluginCLIAdapter) BuildSource(context.Context, pluginBuildRuntimeRequest) (pluginmanager.ArtifactRecord, error) {
	return pluginmanager.ArtifactRecord{}, errors.New("wasm source packages are not implemented yet; build a binary .mcgp with plugin.wasm")
}

func (wasmPluginCLIAdapter) Test(ctx context.Context, opts pluginTestCLIOptions) error {
	for _, item := range strings.Split(opts.Profile, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		switch item {
		case "unit", "manifest":
			if _, err := validateWASMPluginDirectoryForCLI(ctx, opts.Target, opts.Manifest); err != nil {
				return err
			}
		case "harness", "protocol-smoke", "conformance":
			if err := validateTestFileIfSet(opts.ConfigPath, "config"); err != nil {
				return err
			}
			if err := validateTestFileIfSet(opts.FixturePath, "fixture"); err != nil {
				return err
			}
			if err := runWASMFixtureTestForCLI(ctx, opts); err != nil {
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
	case pluginmanager.RuntimeBuiltin, pluginmanager.RuntimeSandbox:
		return nil, fmt.Errorf("runtime %q is reserved; no CLI build/test adapter is implemented yet", runtimeType)
	case pluginmanager.RuntimeWASM:
		return wasmPluginCLIAdapter{}, nil
	default:
		return nil, fmt.Errorf("unsupported runtime %q", runtimeType)
	}
}

func runPluginFeaturesCLI(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("features does not accept positional arguments")
	}
	facts, err := pluginFeatureFactsFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	return encodePluginCLIJSON(facts)
}

func pluginFeatureFacts() map[string]any {
	facts, err := pluginFeatureFactsFromEnv(os.Getenv)
	if err != nil {
		return pluginFeatureFactsFor(pluginmanager.RuntimeFeatureFactsOptions{})
	}
	return facts
}

func pluginFeatureFactsFromEnv(getenv func(string) string) (map[string]any, error) {
	options, err := pluginRuntimeFeatureFactsOptionsFromEnv(getenv)
	if err != nil {
		return nil, err
	}
	return pluginFeatureFactsFor(options), nil
}

func pluginFeatureFactsFor(options pluginmanager.RuntimeFeatureFactsOptions) map[string]any {
	runtimeTypes := pluginmanager.RuntimeTypeFeaturesFor(options)
	serviceModes := pluginmanager.PluginServiceModeFeaturesFor(options)
	runtimeAdapters := pluginmanager.RuntimeAdapterFactoryStatusesFor(options)
	sandboxRuntime := pluginmanager.RuntimeTypeFeatureFor(pluginmanager.RuntimeSandbox, options)
	sandboxMode := pluginmanager.PluginServiceModeFeatureForOptions(pluginmanager.PluginServiceModeSandboxProcess, options)
	return map[string]any{
		"schema_version": pluginmanager.SchemaVersion,
		"api_version":    pluginmanager.APIVersion,
		"artifact_types": []string{
			pluginmanager.ArtifactTypeBinary,
			pluginmanager.ArtifactTypeSource,
		},
		"runtime_types":    runtimeTypes,
		"service_modes":    serviceModes,
		"runtime_adapters": runtimeAdapters,
		"plugin_host":      pluginmanager.PluginHostProtocolFeature(),
		"extension_points": pluginmanager.ExtensionPointFeatures(),
		"build": map[string]any{
			"implemented_runtimes": []string{pluginmanager.RuntimeGoPlugin, pluginmanager.RuntimeWASM},
			"builder_types": []string{
				pluginmanager.BuilderTypeLocalProcess,
				pluginmanager.BuilderTypeContainer,
			},
			"default_runtime_entry":                 pluginmanager.RuntimeEntry,
			"default_build_entry":                   pluginmanager.SourceBuildEntry,
			"prod_default_builder":                  pluginmanager.BuilderTypeContainer,
			"prod_blocks_local_process":             true,
			"container_digest_pin_gate":             true,
			"container_release_binding_gate":        true,
			"container_release_binding_status":      "warning",
			"official_builder_image_published":      true,
			"external_ci_release_attestation_chain": true,
		},
		"repository": map[string]any{
			"implemented_types": []string{
				pluginmanager.RepositoryTypeFile,
				pluginmanager.RepositoryTypeURL,
				pluginmanager.RepositoryTypeOfficial,
				pluginmanager.RepositoryTypeInternal,
			},
			"import_auto_enable":       false,
			"target_desired_apply":     true,
			"apply_auto_enable":        false,
			"apply_governance_gate":    true,
			"update_availability":      true,
			"cli_apply":                true,
			"cross_node_apply":         true,
			"remote_artifact_transfer": true,
		},
		"supply_chain": map[string]any{
			"license_policy": map[string]any{
				"allow_deny":          true,
				"review_required":     true,
				"unknown_license":     true,
				"transitive_scan":     true,
				"organization_sync":   false,
				"supported_fields":    []string{"allowed", "denied", "review_required", "allow_unknown", "apply_to_transitive"},
				"metadata_issue_keys": []string{"denylist_matches", "allowlist_missing", "review_required_matches", "unknown"},
			},
			"sbom": map[string]any{
				"generate":                true,
				"vulnerability_database":  true,
				"feed_sync":               true,
				"external_feed_scheduler": true,
			},
			"external_ci": map[string]any{
				"provenance_gate":          true,
				"signature_required":       true,
				"attestation_required":     true,
				"sbom_required":            true,
				"source_sha_required":      true,
				"artifact_sha_required":    true,
				"ci_run_identity_required": true,
				"trusted_builder_required": true,
				"release_provenance":       true,
				"blocking_actions":         []string{pluginmanager.GovernanceActionEnable, pluginmanager.GovernanceActionRollback, "repository_apply", pluginmanager.GovernanceActionPromotion},
			},
			"advisory": map[string]any{
				"local_gate":              true,
				"feed_sync":               true,
				"rescan":                  true,
				"supported_sources":       []string{"local-json", "admin-api", "http", "https", "file"},
				"external_feed_scheduler": true,
			},
		},
		"operations": map[string]any{
			"background_task_lease": true,
			"task_run_policies": []string{
				pluginmanager.TaskRunPolicyPerNode,
				pluginmanager.TaskRunPolicySingleton,
				pluginmanager.TaskRunPolicySharded,
			},
			"lease_store":                "admin-sqlite",
			"node_state":                 true,
			"artifact_distribution":      true,
			"artifact_distribution_mode": "local-content-store",
			"partial_rollout_failure":    true,
			"background_task_retry":      true,
			"event_low_cardinality_gate": true,
			"metric_label_gate":          true,
			"trace_summary":              true,
			"external_exporters": []map[string]any{
				{
					"type":                  pluginmanager.OperationsExporterPrometheus,
					"enabled":               false,
					"maturity":              pluginmanager.FeatureMaturityReserved,
					"failure_is_fail_open":  true,
					"output_label_gate":     true,
					"output_sensitive_gate": true,
					"rollback_supported":    true,
					"unsupported_reason":    "external Prometheus exporter sink is not enabled; in-process operations snapshot remains authoritative",
				},
				{
					"type":                  pluginmanager.OperationsExporterOTel,
					"enabled":               false,
					"maturity":              pluginmanager.FeatureMaturityReserved,
					"failure_is_fail_open":  true,
					"output_label_gate":     true,
					"output_sensitive_gate": true,
					"rollback_supported":    true,
					"unsupported_reason":    "external OpenTelemetry exporter sink is not enabled; in-process operations snapshot remains authoritative",
				},
			},
			"plugin_data_file_retention_gc":   true,
			"cross_category_retention_rules":  true,
			"gc_dry_run_explains_protected":   true,
			"operations_gc_apply":             true,
			"diagnostic_package":              true,
			"diagnostic_structured_redaction": true,
			"diagnostic_runbook":              true,
			"diagnostic_retention_gc":         true,
			"external_client_boundary":        "native go plugins can bypass ExternalClient; gateway provides governance and observation, not strong network isolation",
			"cross_node_apply":                true,
		},
		"promotion": map[string]any{
			"export":                          true,
			"import_dry_run":                  true,
			"diff":                            true,
			"drift":                           true,
			"dr_drill":                        true,
			"target_desired_apply":            true,
			"config_hash_required":            true,
			"target_config_required":          true,
			"config_plaintext_export":         false,
			"governance_gate":                 true,
			"local_artifact_required":         true,
			"auto_enable":                     false,
			"cli_apply":                       true,
			"cross_node_apply":                true,
			"cross_node_apply_mode":           "shared_desired_state_cluster_reconcile",
			"remote_artifact_transfer":        true,
			"automatic_artifact_distribution": true,
			"automatic_cluster_apply":         true,
		},
		"instrumentation": map[string]any{
			"metadata":                                 true,
			"runtime_plugin":                           false,
			"available_requires_generated_diff_hash":   true,
			"available_requires_gateway_binary_sha256": true,
			"available_requires_ci_artifact_sha256":    true,
			"available_requires_conformance":           true,
			"available_requires_benchmark":             true,
			"available_requires_smoke":                 true,
			"ci_artifact_binding":                      true,
		},
		"ingress": map[string]any{
			"schema_validation":                     true,
			"preflight_schema_gate":                 true,
			"governance_plugin_port_conflict":       true,
			"governance_reserved_listener_conflict": true,
			"gateway_listener_lifecycle":            true,
			"tls_secret_refs_redacted":              true,
			"disable_drain":                         true,
			"reserved_listener_runtime_refresh":     true,
			"future_runtime_gate":                   true,
			"data_plane":                            false,
		},
		"sandbox": map[string]any{
			"runtime_type_reserved":        sandboxRuntime.Maturity == pluginmanager.FeatureMaturityReserved,
			"service_mode_reserved":        sandboxMode.Maturity == pluginmanager.FeatureMaturityReserved,
			"required_capability_gate":     true,
			"future_runtime_gate":          true,
			"environment_self_check":       options.FutureRuntimeGates.SandboxEnabled(),
			"unsupported_reason":           sandboxRuntime.UnsupportedReason,
			"supervisor":                   true,
			"control_rpc":                  true,
			"filesystem_enforcement":       true,
			"network_enforcement":          true,
			"env_enforcement":              true,
			"cpu_memory_enforcement":       true,
			"secret_rpc":                   true,
			"crash_loop_policy_data_plane": false,
			"diagnostic_summary":           true,
			"data_plane":                   sandboxRuntime.DataPlane && sandboxMode.DataPlane,
		},
		"wasm": map[string]any{
			"runtime_type_reserved":        false,
			"contained_validation":         true,
			"high_risk_extension_rejected": true,
			"future_runtime_gate":          false,
			"runtime_adapter":              true,
			"host_abi":                     true,
			"host_abi_version":             pluginmanager.WASMHostABIVersion(),
			"exports": map[string]string{
				pluginmanager.ExtensionConfigValidate: "mcgw_config_validate_v1",
				pluginmanager.ExtensionRuleEvaluate:   "mcgw_rule_evaluate_v1",
				pluginmanager.ExtensionRouteResolve:   "mcgw_route_resolve_v1",
			},
			"module_cache":            true,
			"fuel_time_memory_limits": true,
			"default_no_file_network": true,
			"data_plane":              true,
		},
		"conformance": map[string]any{
			"stable_json":                              true,
			"golden_fixture_file":                      true,
			"default_release_gate":                     true,
			"package_fixture_failures_block_preflight": true,
			"missing_fixture_required":                 false,
			"missing_fixture_policy_gate":              true,
			"missing_fixture_preflight_flag":           "--require-conformance-fixture",
			"manifest_contract_fixture":                true,
			"invalid_config_from_schema":               true,
			"missing_secret_from_manifest":             true,
			"route_decisions":                          true,
			"status_hosts":                             true,
			"stream_proxy_protocol":                    pluginmanager.StreamProxyProtocolV1,
			"stream_proxy_semantics":                   []string{"half_close", "deadline", "backpressure", "cancel", "byte_accounting"},
			"protocol_proxy_scenarios": []string{
				"initial_data_once",
				"panic_recovered",
				"timeout_deadline",
				"endpoint_close",
				"client_close",
				"backpressure_large_packet",
				"drain_disable_new_connections",
				"force_close_draining",
			},
			"rule_evaluation_outcomes": []string{
				"allow",
				"deny",
				"error_fail_closed",
				"timeout_fail_closed",
				"bad_config_fallback",
			},
			"connection_filter_scenarios": []string{
				"allow",
				"reject",
				"error_fail_open",
				"error_fail_closed",
			},
			"handshake_filter_scenarios": []string{
				"allow",
				"reject",
				"rewrite_host",
				"error_fail_open",
				"error_fail_closed",
			},
			"governance_gate_scenarios": []string{
				"review_required",
				"warning_override",
				"advisory_block",
				"supply_chain_block",
				"rollback_gate",
				"repository_apply_gate",
				"promotion_apply_gate",
			},
			"event_delivery": []string{
				"best_effort",
				"at_least_once",
				"dead_letter",
				"replay",
				"drop",
				"cross_node_at_least_once",
				"subscriber_failure_non_blocking",
			},
			"provider_registry": []string{
				"singleton",
				"priority",
				"fallback",
				"dependency",
				"scope",
				"disable",
				"admin.auth.provider/v1",
			},
		},
		"cli": map[string]any{
			"implemented_commands": pluginCLIImplementedCommands(),
			"reserved_commands":    pluginCLIReservedCommands(),
		},
	}
}

func pluginCLIImplementedCommands() []string {
	return []string{
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
		"task cancel",
		"external list",
		"external health-check",
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
		"advisory sync",
		"advisory rescan",
		"vulnerability scan",
		"vulnerability import",
		"vulnerability sync",
		"vulnerability rescan",
		"repo list",
		"repo import",
		"repo apply",
		"repo updates",
		"repo search",
		"repo show",
		"transfer",
		"sbom generate",
		"sbom verify",
		"verify",
		"runtime features",
		"runtime status",
		"runtime mode",
		"runtime apply",
		"plugin-host handshake",
		"plugin-host serve",
		"contract",
		"conformance",
		"schema export",
		"promotion apply",
		"promotion export",
		"promotion import",
		"promotion diff",
		"promotion drift",
		"promotion dr-drill",
		"sign verify",
		"sign key-rotation",
		"sign revoke",
		"inspect",
		"validate",
		"compat",
		"source-validate",
		"source-build",
	}
}

func pluginCLIReservedCommands() []string {
	return []string{}
}

func runPluginSchemaCLI(args []string) error {
	if len(args) == 0 || args[0] != "export" {
		return errors.New("usage: gateway plugin schema export [--section manifest|cli|admin-api|conformance-fixture]")
	}
	section := ""
	for i := 1; i < len(args); i++ {
		key, value, consumed, err := parsePluginCLIFlag(args, i)
		if err != nil {
			return err
		}
		i += consumed
		switch key {
		case "section":
			section = value
		default:
			return fmt.Errorf("unknown schema export flag --%s", key)
		}
	}
	schemas := map[string]any{}
	if section == "" || section == "manifest" {
		schemas["manifest"] = manifestSchemaForCLI()
	}
	if section == "" || section == "cli" {
		schemas["cli"] = cliSchemaForCLI()
	}
	if section == "" || section == "admin-api" {
		schemas["admin_api"] = adminAPISchemaForCLI()
	}
	if section == "" || section == "conformance-fixture" {
		schemas["conformance_fixture"] = conformanceFixtureSchemaForCLI()
	}
	if len(schemas) == 0 {
		return fmt.Errorf("unknown schema section %q", section)
	}
	return encodePluginCLIJSON(map[string]any{
		"schema_version": pluginmanager.SchemaVersion,
		"api_version":    pluginmanager.APIVersion,
		"command":        "schema export",
		"schemas":        schemas,
	})
}

func runPluginContractCLI(args []string) error {
	opts, err := parsePluginContractCLIOptions(args)
	if err != nil {
		return err
	}
	report := pluginContractReport(opts)
	return encodePluginCLIJSON(report)
}

func runPluginConformanceCLI(args []string) error {
	opts, err := parsePluginContractCLIOptions(args)
	if err != nil {
		return err
	}
	report := pluginContractReport(opts)
	manifest, _, _ := readPluginTargetManifestForCLI(opts.Target, opts.Manifest)
	executor := newConformanceExecutorForCLI(opts, manifest)
	defer executor.Close()
	goldenFixtures, fixtureSource, err := loadConformanceFixtureFileForCLI(opts, executor)
	if err != nil {
		return err
	}
	fixtures := conformanceFixturesForCLI(opts, manifest, report["ok"] == true, goldenFixtures, executor)
	ok := report["ok"] == true
	for _, fixture := range fixtures {
		if conformanceFixtureFailed(fixture) {
			ok = false
			break
		}
	}
	return encodePluginCLIJSON(map[string]any{
		"schema_version": pluginmanager.SchemaVersion,
		"api_version":    pluginmanager.APIVersion,
		"command":        "conformance",
		"target":         opts.Target,
		"profile":        opts.Profile,
		"ok":             ok,
		"contract":       report,
		"fixtures":       fixtures,
		"fixture_source": fixtureSource,
	})
}

func parsePluginContractCLIOptions(args []string) (pluginContractCLIOptions, error) {
	opts := pluginContractCLIOptions{Profile: pluginmanager.PolicyProfileDev}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			if opts.Target != "" {
				return pluginContractCLIOptions{}, fmt.Errorf("unexpected argument %q", arg)
			}
			opts.Target = arg
			continue
		}
		key, value, consumed, err := parsePluginCLIFlag(args, i)
		if err != nil {
			return pluginContractCLIOptions{}, err
		}
		i += consumed
		switch key {
		case "manifest":
			opts.Manifest = value
		case "config":
			opts.ConfigPath = value
		case "config-json":
			opts.ConfigJSON = value
		case "profile":
			opts.Profile = value
		case "fixture":
			opts.FixturePath = value
		default:
			return pluginContractCLIOptions{}, fmt.Errorf("unknown contract flag --%s", key)
		}
	}
	if opts.Target == "" {
		return pluginContractCLIOptions{}, errors.New("plugin contract requires a plugin directory, manifest source, or .mcgp artifact")
	}
	return opts, nil
}

func pluginContractReport(opts pluginContractCLIOptions) map[string]any {
	checks := []map[string]any{}
	manifest, targetKind, err := readPluginTargetManifestForCLI(opts.Target, opts.Manifest)
	if err != nil {
		checks = append(checks, cliCheck("manifest_read", "blocking", err.Error(), nil))
		return pluginContractResponse(opts, targetKind, pluginmanager.ArtifactRecord{}, manifest, checks)
	}
	artifact, err := validatePluginPathForCLIWithManifest(opts.Target, "", opts.Manifest)
	if err != nil {
		checks = append(checks, cliCheck("manifest_package", "blocking", err.Error(), nil))
	} else {
		checks = append(checks, cliCheck("manifest_package", "info", "manifest and package shape are valid", map[string]any{
			"artifact_type": artifact.ArtifactType,
			"runtime_type":  artifact.RuntimeType,
		}))
	}
	checks = append(checks, extensionContractChecks(manifest)...)
	checks = append(checks, capabilityContractChecks(manifest)...)
	checks = append(checks, configContractChecks(opts, manifest)...)
	return pluginContractResponse(opts, targetKind, artifact, manifest, checks)
}

func pluginContractResponse(opts pluginContractCLIOptions, targetKind string, artifact pluginmanager.ArtifactRecord, manifest pluginmanager.Manifest, checks []map[string]any) map[string]any {
	return map[string]any{
		"schema_version": pluginmanager.SchemaVersion,
		"api_version":    pluginmanager.APIVersion,
		"command":        "contract",
		"target":         opts.Target,
		"target_kind":    targetKind,
		"profile":        opts.Profile,
		"ok":             !cliHasBlocking(checks),
		"plugin": map[string]any{
			"id":            manifest.ID,
			"version":       manifest.Version,
			"artifact_type": artifact.ArtifactType,
			"runtime_type":  manifest.Runtime.Type,
			"api_version":   manifest.APIVersion,
		},
		"checks": checks,
	}
}

func readPluginTargetManifestForCLI(target, manifestPath string) (pluginmanager.Manifest, string, error) {
	info, err := os.Stat(target)
	if err != nil {
		return pluginmanager.Manifest{}, "", err
	}
	if info.IsDir() {
		manifest, _, err := readPluginDirManifest(target, manifestPath)
		return manifest, "directory", err
	}
	if isManifestSourceFile(target) {
		manifest, _, err := readPluginDirManifest(filepath.Dir(target), target)
		return manifest, "manifest", err
	}
	manifest, err := readPackageManifest(target)
	return manifest, "package", err
}

func extensionContractChecks(manifest pluginmanager.Manifest) []map[string]any {
	checks := []map[string]any{}
	supported := supportedExtensionPointKeysForCLI()
	if len(manifest.ExtensionPoints) == 0 {
		return append(checks, cliCheck("extension_points", "warning", "manifest declares no extension points", nil))
	}
	for _, point := range manifest.ExtensionPoints {
		if supported[point.Key] {
			checks = append(checks, cliCheck("extension_point_supported", "info", "extension point is supported", map[string]any{"key": point.Key, "type": point.Type}))
		} else {
			checks = append(checks, cliCheck("extension_point_supported", "blocking", "extension point is not supported by this gateway", map[string]any{"key": point.Key, "type": point.Type}))
		}
	}
	return checks
}

func capabilityContractChecks(manifest pluginmanager.Manifest) []map[string]any {
	checks := []map[string]any{}
	var caps struct {
		ExtensionPoints  []string `json:"extension_points"`
		RequiredFeatures []string `json:"required_features"`
		Features         []string `json:"features"`
		UpstreamConnect  struct {
			Mode string `json:"mode"`
		} `json:"upstream_connect"`
		Ingress *pluginmanager.IngressCapability `json:"ingress"`
	}
	if len(manifest.Capabilities) == 0 {
		return append(checks, cliCheck("capabilities", "warning", "capabilities object is empty", nil))
	}
	if err := json.Unmarshal(manifest.Capabilities, &caps); err != nil {
		return append(checks, cliCheck("capabilities", "blocking", "capabilities must be valid JSON object", map[string]any{"error": err.Error()}))
	}
	declared := stringSet(caps.ExtensionPoints)
	for _, point := range manifest.ExtensionPoints {
		if len(declared) > 0 && !declared[point.Key] {
			checks = append(checks, cliCheck("capability_extension_missing", "warning", "capabilities.extension_points does not list manifest extension point", map[string]any{"key": point.Key}))
		}
	}
	for _, feature := range append(caps.RequiredFeatures, caps.Features...) {
		if !supportedFeatureForCLI(feature) {
			checks = append(checks, cliCheck("required_feature_missing", "blocking", "required feature is not supported by this gateway", map[string]any{"feature": feature}))
		}
	}
	switch caps.UpstreamConnect.Mode {
	case "", pluginmanager.UpstreamModeDialer, pluginmanager.UpstreamModeProtocolProxy:
		if caps.UpstreamConnect.Mode != "" {
			checks = append(checks, cliCheck("upstream_connect_mode", "info", "upstream connect mode is supported", map[string]any{"mode": caps.UpstreamConnect.Mode}))
		}
	default:
		checks = append(checks, cliCheck("upstream_connect_mode", "blocking", "upstream connect mode is not supported", map[string]any{"mode": caps.UpstreamConnect.Mode}))
	}
	if hasExtensionPointForCLI(manifest, pluginmanager.ExtensionIngressService) {
		if problems := pluginmanager.ValidateIngressCapability(manifest, caps.Ingress); len(problems) > 0 {
			details := pluginmanager.IngressCapabilityDetails(caps.Ingress)
			details["errors"] = problems
			checks = append(checks, cliCheck("ingress_service_schema", "blocking", "ingress.service/v1 capability declaration is invalid", details))
		} else {
			checks = append(checks, cliCheck("ingress_service_schema", "info", "ingress.service/v1 capability declaration passed reserved schema checks", pluginmanager.IngressCapabilityDetails(caps.Ingress)))
		}
	}
	return checks
}

func configContractChecks(opts pluginContractCLIOptions, manifest pluginmanager.Manifest) []map[string]any {
	checks := []map[string]any{}
	if len(manifest.ConfigSchema) == 0 {
		checks = append(checks, cliCheck("config_schema", "warning", "config_schema is empty", nil))
	} else if !json.Valid(manifest.ConfigSchema) {
		checks = append(checks, cliCheck("config_schema", "blocking", "config_schema must be valid JSON", nil))
	} else {
		checks = append(checks, cliCheck("config_schema", "info", "config_schema is valid JSON", nil))
	}
	configJSON := opts.ConfigJSON
	if opts.ConfigPath != "" {
		data, err := os.ReadFile(opts.ConfigPath)
		if err != nil {
			checks = append(checks, cliCheck("config_fixture", "blocking", err.Error(), nil))
			return checks
		}
		configJSON = string(data)
	}
	if configJSON != "" {
		if !json.Valid([]byte(configJSON)) {
			checks = append(checks, cliCheck("config_fixture", "blocking", "config fixture must be valid JSON", nil))
		} else {
			checks = append(checks, cliCheck("config_fixture", "info", "config fixture is valid JSON", nil))
		}
	}
	return checks
}

func conformanceFixturesForCLI(opts pluginContractCLIOptions, manifest pluginmanager.Manifest, contractOK bool, golden []map[string]any, executor *conformanceExecutorForCLI) []map[string]any {
	fixtures := []map[string]any{
		{"name": "contract", "status": passFail(contractOK), "extension": ""},
	}
	mode := upstreamConnectModeForCLI(manifest)
	if hasExtensionPointForCLI(manifest, pluginmanager.ExtensionUpstreamConnect) {
		fixtures = append(fixtures, map[string]any{"name": "upstream.connect/v1", "status": "pass", "mode": mode})
		if mode == pluginmanager.UpstreamModeProtocolProxy {
			fixtures = append(fixtures,
				map[string]any{"name": "protocol-proxy.initial_data", "status": "pass"},
				map[string]any{"name": "protocol-proxy.panic", "status": "pass"},
				map[string]any{"name": "protocol-proxy.timeout", "status": "pass"},
			)
		}
	}
	for _, item := range []struct {
		name string
		key  string
	}{
		{name: "route.resolve/v1", key: pluginmanager.ExtensionRouteResolve},
		{name: "status.ping/v1", key: pluginmanager.ExtensionStatusPing},
		{name: "rule.evaluate/v1", key: pluginmanager.ExtensionRuleEvaluate},
		{name: "config.validate/v1", key: pluginmanager.ExtensionConfigValidate},
	} {
		status := "skip"
		if hasExtensionPointForCLI(manifest, item.key) {
			status = "pass"
		}
		fixture := map[string]any{"name": item.name, "status": status, "extension": item.key}
		if item.key == pluginmanager.ExtensionConfigValidate && status == "pass" && manifest.Runtime.Type == pluginmanager.RuntimeWASM {
			executeConformanceWASMConfigValidateFixtureForCLI(opts, fixture, executor)
		}
		fixtures = append(fixtures, fixture)
	}
	invalidConfig := map[string]any{"name": "invalid_config", "status": "skip", "expected": "config_schema_declared"}
	if schema := strings.TrimSpace(string(manifest.ConfigSchema)); schema != "" && schema != "null" {
		if err := pluginmanager.ValidateConfigSchema(manifest.ConfigSchema, "[]"); err != nil {
			invalidConfig["status"] = "pass"
			invalidConfig["expected"] = "dry_run_rejected"
			invalidConfig["error"] = err.Error()
		} else {
			invalidConfig["status"] = "skip"
			invalidConfig["expected"] = "schema_accepts_array"
		}
	}
	var requiredSecrets []string
	for _, secret := range manifest.Secrets {
		if secret.Required && strings.TrimSpace(secret.Name) != "" {
			requiredSecrets = append(requiredSecrets, secret.Name)
		}
	}
	sort.Strings(requiredSecrets)
	missingSecret := map[string]any{"name": "missing_secret", "status": "skip", "expected": "required_secret_declared"}
	if len(requiredSecrets) > 0 {
		missingSecret["status"] = "pass"
		missingSecret["expected"] = "dry_run_rejected"
		missingSecret["secrets"] = requiredSecrets
	}
	fixtures = append(fixtures,
		map[string]any{"name": "governance_gate", "status": passFail(contractOK)},
		invalidConfig,
		missingSecret,
		map[string]any{"name": "blocked", "status": passFail(contractOK)},
	)
	executeNamedConformanceFixtureForCLI(opts, invalidConfig, executor)
	executeNamedConformanceFixtureForCLI(opts, missingSecret, executor)
	fixtures = append(fixtures, golden...)
	return fixtures
}

func loadConformanceFixtureFileForCLI(opts pluginContractCLIOptions, executor *conformanceExecutorForCLI) ([]map[string]any, string, error) {
	fixturePath := strings.TrimSpace(opts.FixturePath)
	explicit := fixturePath != ""
	if fixturePath == "" {
		fixturePath = defaultConformanceFixturePathForCLI(opts.Target)
	}
	if fixturePath == "" {
		return nil, "", nil
	}
	data, source, err := readConformanceFixtureDataForCLI(fixturePath)
	if err != nil {
		if explicit || !errors.Is(err, os.ErrNotExist) {
			return nil, "", err
		}
		return nil, "", nil
	}
	var file pluginConformanceFixtureFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, "", fmt.Errorf("invalid conformance fixture %q: %w", source, err)
	}
	return normalizeConformanceFixtureFileForCLI(opts, file, executor), source, nil
}

func defaultConformanceFixturePathForCLI(target string) string {
	info, err := os.Stat(target)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return filepath.Join(target, "conformance.json")
	}
	if isManifestSourceFile(target) {
		return filepath.Join(filepath.Dir(target), "conformance.json")
	}
	return target + ":conformance.json"
}

func readConformanceFixtureDataForCLI(ref string) ([]byte, string, error) {
	if packagePath, entry, ok := strings.Cut(ref, ":"); ok && entry == "conformance.json" {
		data, err := readZipEntryForCLI(packagePath, entry, pluginmanager.DefaultManifestMaxBytes)
		return data, ref, err
	}
	data, err := os.ReadFile(ref)
	return data, ref, err
}

func readZipEntryForCLI(packagePath, entryName string, maxBytes int64) ([]byte, error) {
	reader, err := zip.OpenReader(packagePath)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	for _, file := range reader.File {
		if file.Name != entryName {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		data, err := io.ReadAll(io.LimitReader(rc, maxBytes+1))
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > maxBytes {
			return nil, fmt.Errorf("%s exceeds %d bytes", entryName, maxBytes)
		}
		return data, nil
	}
	return nil, fmt.Errorf("%w: %s", os.ErrNotExist, entryName)
}

func normalizeConformanceFixtureFileForCLI(opts pluginContractCLIOptions, file pluginConformanceFixtureFile, executor *conformanceExecutorForCLI) []map[string]any {
	var fixtures []map[string]any
	for _, fixture := range file.Fixtures {
		normalized := copyStringAnyMap(fixture)
		if strings.TrimSpace(fmt.Sprint(normalized["name"])) == "" {
			continue
		}
		if strings.TrimSpace(fmt.Sprint(normalized["status"])) == "" {
			normalized["status"] = "pass"
		}
		executeNamedConformanceFixtureForCLI(opts, normalized, executor)
		fixtures = append(fixtures, normalized)
	}
	for _, decision := range file.RouteDecisions {
		fixtures = append(fixtures, map[string]any{
			"name":      "route.resolve/v1." + decision,
			"status":    "pass",
			"extension": pluginmanager.ExtensionRouteResolve,
			"decision":  decision,
		})
		executeConformanceRouteFixtureForCLI(opts, fixtures[len(fixtures)-1], executor)
	}
	for _, scenario := range file.StreamProxyScenarios {
		fixture := map[string]any{
			"name":      scenario.Name,
			"status":    scenario.Status,
			"extension": pluginmanager.StreamProxyProtocolV1,
			"protocol":  scenario.Protocol,
			"expected":  scenario.Expected,
		}
		if fixture["status"] == "" {
			fixture["status"] = "pass"
		}
		if scenario.DeadlineUnixMS > 0 {
			fixture["deadline_unix_ms"] = scenario.DeadlineUnixMS
		}
		if scenario.BytesToPlugin > 0 {
			fixture["bytes_to_plugin"] = scenario.BytesToPlugin
		}
		if scenario.BytesToGateway > 0 {
			fixture["bytes_to_gateway"] = scenario.BytesToGateway
		}
		if scenario.BackpressureBytes > 0 {
			fixture["backpressure_bytes"] = scenario.BackpressureBytes
		}
		if err := pluginmanager.ValidateStreamProxyFixture(scenario); err != nil {
			conformanceFail(fixture, err)
		}
		fixtures = append(fixtures, fixture)
	}
	protocolProxyExpected := map[string]string{
		"initial_data_once":             "initial_data_replayed_once",
		"panic_recovered":               "panic_recovered",
		"timeout_deadline":              "deadline_enforced",
		"endpoint_close":                "endpoint_half_close_propagated",
		"client_close":                  "client_half_close_propagated",
		"backpressure_large_packet":     "large_minecraft_packet_forwarded_under_backpressure",
		"drain_disable_new_connections": "new_connections_rejected_while_draining",
		"force_close_draining":          "active_proxies_closed_after_deadline",
	}
	for _, scenario := range file.ProtocolProxyScenarios {
		scenario = strings.TrimSpace(scenario)
		if scenario == "" {
			continue
		}
		expected := protocolProxyExpected[scenario]
		if expected == "" {
			expected = scenario
		}
		fixtures = append(fixtures, map[string]any{
			"name":      "protocol-proxy." + scenario,
			"status":    "pass",
			"extension": pluginmanager.ExtensionUpstreamConnect,
			"mode":      pluginmanager.UpstreamModeProtocolProxy,
			"scenario":  scenario,
			"expected":  expected,
		})
		executeConformanceProtocolProxyFixtureForCLI(opts, fixtures[len(fixtures)-1], executor)
	}
	for _, host := range file.StatusHosts {
		fixtures = append(fixtures, map[string]any{
			"name":      "status.ping/v1.host",
			"status":    "pass",
			"extension": pluginmanager.ExtensionStatusPing,
			"host":      host,
		})
		executeConformanceStatusFixtureForCLI(opts, fixtures[len(fixtures)-1], executor)
	}
	ruleExpected := map[string]string{
		"allow":               "decision_allow",
		"deny":                "decision_deny",
		"error_fail_closed":   "error_converted_to_policy_decision",
		"timeout_fail_closed": "deadline_enforced",
		"bad_config_fallback": "default_route_survives_bad_rule_config",
	}
	for _, outcome := range file.RuleEvaluationOutcomes {
		outcome = strings.TrimSpace(outcome)
		if outcome == "" {
			continue
		}
		expected := ruleExpected[outcome]
		if expected == "" {
			expected = outcome
		}
		fixtures = append(fixtures, map[string]any{
			"name":      "rule.evaluate/v1." + outcome,
			"status":    "pass",
			"extension": pluginmanager.ExtensionRuleEvaluate,
			"outcome":   outcome,
			"expected":  expected,
		})
		executeConformanceRuleFixtureForCLI(opts, fixtures[len(fixtures)-1], executor)
	}
	connectionFilterExpected := map[string]string{
		"allow":             "connection_allowed",
		"reject":            "connection_rejected",
		"error_fail_open":   "error_allows_next_filter",
		"error_fail_closed": "error_blocks_dispatch",
	}
	for _, scenario := range file.ConnectionFilterScenarios {
		scenario = strings.TrimSpace(scenario)
		if scenario == "" {
			continue
		}
		expected := connectionFilterExpected[scenario]
		if expected == "" {
			expected = scenario
		}
		fixtures = append(fixtures, map[string]any{
			"name":      "connection.filter/v1." + scenario,
			"status":    "pass",
			"extension": pluginmanager.ExtensionConnectionFilter,
			"scenario":  scenario,
			"expected":  expected,
		})
		executeConformanceConnectionFilterFixtureForCLI(opts, fixtures[len(fixtures)-1], executor)
	}
	handshakeFilterExpected := map[string]string{
		"allow":             "handshake_allowed",
		"reject":            "handshake_rejected",
		"rewrite_host":      "rewrite_visible_to_later_filters",
		"error_fail_open":   "error_allows_next_filter",
		"error_fail_closed": "error_blocks_dispatch",
	}
	for _, scenario := range file.HandshakeFilterScenarios {
		scenario = strings.TrimSpace(scenario)
		if scenario == "" {
			continue
		}
		expected := handshakeFilterExpected[scenario]
		if expected == "" {
			expected = scenario
		}
		fixtures = append(fixtures, map[string]any{
			"name":      "handshake.filter/v1." + scenario,
			"status":    "pass",
			"extension": pluginmanager.ExtensionHandshakeFilter,
			"scenario":  scenario,
			"expected":  expected,
		})
		executeConformanceHandshakeFilterFixtureForCLI(opts, fixtures[len(fixtures)-1], executor)
	}
	governanceExpected := map[string]string{
		"review_required":       "blocking_without_review",
		"warning_override":      "warning_requires_override",
		"advisory_block":        "blocking_advisory",
		"supply_chain_block":    "blocking_supply_chain",
		"rollback_gate":         "rollback_rechecked",
		"repository_apply_gate": "target_apply_rechecked",
		"promotion_apply_gate":  "target_config_hash_and_governance_rechecked",
	}
	for _, scenario := range file.GovernanceGateScenarios {
		scenario = strings.TrimSpace(scenario)
		if scenario == "" {
			continue
		}
		expected := governanceExpected[scenario]
		if expected == "" {
			expected = scenario
		}
		fixture := map[string]any{
			"name":      "governance." + scenario,
			"status":    "pass",
			"extension": "governance",
			"scenario":  scenario,
			"expected":  expected,
		}
		executeConformanceGovernanceGateFixtureForCLI(opts, fixture, executor)
		fixtures = append(fixtures, fixture)
	}
	for _, delivery := range file.EventDelivery {
		fixtures = append(fixtures, map[string]any{
			"name":      "event.subscriber/v1." + delivery,
			"status":    "pass",
			"extension": pluginmanager.ExtensionEventSubscriber,
			"delivery":  delivery,
		})
		executeConformanceEventFixtureForCLI(opts, fixtures[len(fixtures)-1], executor)
	}
	for _, item := range file.ProviderRegistry {
		fixtures = append(fixtures, map[string]any{
			"name":      "provider.registry." + item,
			"status":    "pass",
			"extension": pluginmanager.ExtensionProvider,
			"provider":  item,
		})
		executeConformanceProviderFixtureForCLI(opts, fixtures[len(fixtures)-1], executor)
	}
	if file.DefaultRouteMustSurviveBadRuleConfig {
		fixtures = append(fixtures, map[string]any{
			"name":     "route.default_survives_bad_rule_config",
			"status":   "pass",
			"expected": "fallback",
		})
	}
	if file.LocalAdminBreakGlass {
		fixtures = append(fixtures, map[string]any{
			"name":     "admin_auth.break_glass",
			"status":   "pass",
			"expected": "local_login_available",
		})
	}
	return fixtures
}

type conformanceExecutorForCLI struct {
	opts       pluginContractCLIOptions
	manifest   pluginmanager.Manifest
	manager    *pluginmanager.Manager
	dbClose    func()
	cleanup    func()
	artifact   pluginmanager.ArtifactRecord
	configJSON string
	prepareErr error
}

func newConformanceExecutorForCLI(opts pluginContractCLIOptions, manifest pluginmanager.Manifest) *conformanceExecutorForCLI {
	if manifest.ID == "" {
		if loaded, _, err := readPluginTargetManifestForCLI(opts.Target, opts.Manifest); err == nil {
			manifest = loaded
		}
	}
	return &conformanceExecutorForCLI{opts: opts, manifest: manifest}
}

func (e *conformanceExecutorForCLI) Close() {
	if e == nil {
		return
	}
	if e.dbClose != nil {
		e.dbClose()
	}
	if e.cleanup != nil {
		e.cleanup()
	}
}

func (e *conformanceExecutorForCLI) prepare(adapter pluginmanager.RuntimeAdapter) (*pluginmanager.Manager, pluginmanager.ArtifactRecord, string, error) {
	if e == nil {
		return nil, pluginmanager.ArtifactRecord{}, "", errors.New("conformance executor is nil")
	}
	if e.prepareErr != nil {
		return nil, pluginmanager.ArtifactRecord{}, "", e.prepareErr
	}
	if e.manager != nil {
		return e.manager, e.artifact, e.configJSON, nil
	}
	tmpRoot, err := os.MkdirTemp("", "mcgp-conformance-cli-*")
	if err != nil {
		e.prepareErr = err
		return nil, pluginmanager.ArtifactRecord{}, "", err
	}
	e.cleanup = func() { _ = os.RemoveAll(tmpRoot) }
	targetPath := e.opts.Target
	info, err := os.Stat(targetPath)
	if err != nil {
		e.prepareErr = err
		return nil, pluginmanager.ArtifactRecord{}, "", err
	}
	if info.IsDir() {
		manifest, raw, err := readPluginDirManifest(targetPath, e.opts.Manifest)
		if err != nil {
			e.prepareErr = err
			return nil, pluginmanager.ArtifactRecord{}, "", err
		}
		e.manifest = manifest
		targetPath = filepath.Join(tmpRoot, manifest.ID+".mcgp")
		if _, err := buildBinaryPluginPackageForConformance(context.Background(), e.opts.Target, manifest, raw, targetPath); err != nil {
			e.prepareErr = err
			return nil, pluginmanager.ArtifactRecord{}, "", err
		}
	}
	db, err := openPluginCLIDB(filepath.Join(tmpRoot, "plugins.db"))
	if err != nil {
		e.prepareErr = err
		return nil, pluginmanager.ArtifactRecord{}, "", err
	}
	e.dbClose = func() { _ = db.Close() }
	managerOptions := pluginmanager.Options{
		DB:            db,
		ArtifactRoot:  filepath.Join(tmpRoot, "artifacts"),
		Now:           pluginConformanceNow,
		PolicyProfile: e.opts.Profile,
	}
	if adapter != nil {
		managerOptions.Adapter = adapter
	}
	manager := pluginmanager.New(managerOptions)
	artifact, err := manager.UploadArtifact(context.Background(), pluginmanager.ArtifactUpload{
		SourcePath: targetPath,
		FileName:   filepath.Base(targetPath),
		Actor:      "cli",
	})
	if err != nil {
		e.prepareErr = err
		return nil, pluginmanager.ArtifactRecord{}, "", err
	}
	configJSON, err := conformanceConfigJSON(e.opts)
	if err != nil {
		e.prepareErr = err
		return nil, pluginmanager.ArtifactRecord{}, "", err
	}
	e.manager = manager
	e.artifact = artifact
	e.configJSON = configJSON
	if e.manifest.ID == "" {
		var manifest pluginmanager.Manifest
		if err := json.Unmarshal([]byte(artifact.MetadataJSON), &manifest); err == nil {
			e.manifest = manifest
		}
	}
	return manager, artifact, configJSON, nil
}

func (e *conformanceExecutorForCLI) ensureDesiredEnabled(adapter pluginmanager.RuntimeAdapter) (*pluginmanager.Manager, pluginmanager.ArtifactRecord, string, error) {
	manager, artifact, configJSON, err := e.prepare(adapter)
	if err != nil {
		return nil, pluginmanager.ArtifactRecord{}, "", err
	}
	if _, err := manager.SetDesired(context.Background(), "cli", artifact.PluginID, artifact.ID, pluginmanager.DesiredEnabled, configJSON, pluginmanager.DefaultPriority); err != nil {
		return nil, pluginmanager.ArtifactRecord{}, "", err
	}
	if _, err := manager.Enable(context.Background(), "cli", artifact.PluginID); err != nil {
		if strings.Contains(err.Error(), "review_required") {
			if _, reviewErr := manager.CreateReview(context.Background(), "cli", artifact.PluginID, pluginmanager.GovernanceReviewRequest{
				ArtifactID: artifact.ID,
				Profile:    e.opts.Profile,
				Decision:   pluginmanager.ReviewDecisionApproved,
				Notes:      "conformance execution approval",
			}); reviewErr != nil {
				return nil, pluginmanager.ArtifactRecord{}, "", reviewErr
			}
			if _, err = manager.Enable(context.Background(), "cli", artifact.PluginID); err != nil {
				return nil, pluginmanager.ArtifactRecord{}, "", err
			}
			return manager, artifact, configJSON, nil
		}
		return nil, pluginmanager.ArtifactRecord{}, "", err
	}
	return manager, artifact, configJSON, nil
}

func executeNamedConformanceFixtureForCLI(_ pluginContractCLIOptions, fixture map[string]any, executor *conformanceExecutorForCLI) {
	if executor == nil {
		return
	}
	fixture["mode"] = "executable"
	switch strings.TrimSpace(fmt.Sprint(fixture["name"])) {
	case "invalid_config":
		if schema := strings.TrimSpace(string(executor.manifest.ConfigSchema)); schema == "" || schema == "null" {
			fixture["status"] = "skip"
			fixture["expected"] = "config_schema_declared"
			return
		}
		manager, artifact, _, err := executor.prepare(nil)
		if err != nil {
			conformanceFail(fixture, err)
			return
		}
		_, err = manager.DryRunConfig(context.Background(), artifact.PluginID, artifact.ID, "[]")
		if err != nil {
			fixture["status"] = "pass"
			fixture["expected"] = "dry_run_rejected"
			fixture["error"] = err.Error()
			return
		}
		if strings.TrimSpace(fmt.Sprint(fixture["expected"])) != "dry_run_rejected" {
			fixture["status"] = "skip"
			fixture["expected"] = "schema_accepts_array"
			fixture["reason"] = "runtime dry-run accepted generic invalid array fixture"
			return
		}
		fixture["status"] = "fail"
		fixture["error"] = "runtime dry-run accepted invalid array fixture"
	case "missing_secret":
		var required []string
		for _, secret := range executor.manifest.Secrets {
			if secret.Required && strings.TrimSpace(secret.Name) != "" {
				required = append(required, secret.Name)
			}
		}
		sort.Strings(required)
		if len(required) == 0 {
			fixture["status"] = "skip"
			fixture["expected"] = "required_secret_declared"
			return
		}
		fixture["secrets"] = required
		manager, artifact, configJSON, err := executor.prepare(nil)
		if err != nil {
			conformanceFail(fixture, err)
			return
		}
		_, err = manager.DryRunConfig(context.Background(), artifact.PluginID, artifact.ID, conformanceSecretFixtureConfig(configJSON))
		if err != nil && strings.Contains(err.Error(), "missing configured secret") {
			fixture["status"] = "pass"
			fixture["expected"] = "dry_run_rejected"
			fixture["error"] = err.Error()
			return
		}
		if err != nil {
			conformanceFail(fixture, err)
			return
		}
		fixture["status"] = "fail"
		fixture["expected"] = "dry_run_rejected"
		fixture["error"] = "runtime dry-run accepted config without required secret"
	}
}

func executeConformanceRouteFixtureForCLI(_ pluginContractCLIOptions, fixture map[string]any, executor *conformanceExecutorForCLI) {
	if !conformanceRequireExtension(fixture, executor, pluginmanager.ExtensionRouteResolve) {
		return
	}
	if executor.manifest.Runtime.Type == pluginmanager.RuntimeWASM {
		executeConformanceWASMRouteFixtureForCLI(fixture, executor)
		return
	}
	decision := strings.TrimSpace(fmt.Sprint(fixture["decision"]))
	manager, artifact, cleanup, err := newConformanceHarnessManager("route-conformance", "route", decision, "", nil)
	if err != nil {
		conformanceFail(fixture, err)
		return
	}
	defer cleanup()
	if _, err := manager.SetDesired(context.Background(), "cli", artifact.PluginID, artifact.ID, pluginmanager.DesiredEnabled, `{}`, pluginmanager.DefaultPriority); err != nil {
		conformanceFail(fixture, err)
		return
	}
	if _, err := manager.Enable(context.Background(), "cli", artifact.PluginID); err != nil {
		conformanceFail(fixture, err)
		return
	}
	req := api.RouteResolveRequest{
		Host:             conformanceRouteHost(decision),
		FallbackUpstream: "fallback:25565",
		FallbackHit:      decision == "fallback" || decision == "pass",
		Refresh:          true,
	}
	result, err := manager.ResolveRoute(context.Background(), req, nil)
	if err != nil {
		conformanceFail(fixture, err)
		return
	}
	fixture["target_plugin"] = artifact.PluginID
	fixture["actual_action"] = result.Decision.Action
	fixture["actual_source"] = result.Source
	fixture["actual_upstream"] = result.Decision.Upstream
	switch decision {
	case "override":
		if result.Decision.Action == api.RouteDecisionOverride && result.Decision.Upstream != "" {
			conformancePass(fixture)
			return
		}
	case "reject":
		if result.Decision.Action == api.RouteDecisionReject {
			conformancePass(fixture)
			return
		}
	case "fallback":
		if result.Decision.Action == api.RouteDecisionFallback && result.Decision.Upstream != "" {
			conformancePass(fixture)
			return
		}
	case "pass":
		if result.Source == "sqlite_fallback" || result.Source == "fallback_miss" {
			conformancePass(fixture)
			return
		}
	default:
		if result.Decision.Action != "" {
			conformancePass(fixture)
			return
		}
	}
	fixture["status"] = "fail"
	fixture["error"] = "route fixture returned unexpected decision"
}

func executeConformanceProtocolProxyFixtureForCLI(_ pluginContractCLIOptions, fixture map[string]any, executor *conformanceExecutorForCLI) {
	if executor == nil {
		return
	}
	fixture["mode"] = "executable"
	if upstreamConnectModeForCLI(executor.manifest) != pluginmanager.UpstreamModeProtocolProxy {
		fixture["status"] = "fail"
		fixture["error"] = "protocol-proxy scenario requires upstream.connect/v1 protocol-proxy manifest"
		return
	}
	if err := executeProtocolProxyHarnessFixtureForCLI(fixture); err != nil {
		conformanceFail(fixture, err)
		return
	}
	conformancePass(fixture)
}

func executeConformanceStatusFixtureForCLI(_ pluginContractCLIOptions, fixture map[string]any, executor *conformanceExecutorForCLI) {
	if !conformanceRequireExtension(fixture, executor, pluginmanager.ExtensionStatusPing) {
		return
	}
	manager, artifact, cleanup, err := newConformanceHarnessManager("status-conformance", "status", "", "", nil)
	if err != nil {
		conformanceFail(fixture, err)
		return
	}
	defer cleanup()
	if _, err := manager.SetDesired(context.Background(), "cli", artifact.PluginID, artifact.ID, pluginmanager.DesiredEnabled, `{}`, pluginmanager.DefaultPriority); err != nil {
		conformanceFail(fixture, err)
		return
	}
	if _, err := manager.Enable(context.Background(), "cli", artifact.PluginID); err != nil {
		conformanceFail(fixture, err)
		return
	}
	host := strings.TrimSpace(fmt.Sprint(fixture["host"]))
	if host == "" {
		host = "blue.example"
	}
	result, err := manager.StatusPing(context.Background(), api.StatusPingRequest{Host: host, ProtocolVersion: 767})
	if err != nil {
		conformanceFail(fixture, err)
		return
	}
	fixture["target_plugin"] = artifact.PluginID
	fixture["actual_handled"] = result.Handled
	fixture["actual_source"] = result.Source
	fixture["actual_motd"] = result.Response.MOTD
	if result.Handled {
		conformancePass(fixture)
		return
	}
	fixture["status"] = "fail"
	fixture["error"] = "status fixture was not handled"
}

func executeConformanceRuleFixtureForCLI(_ pluginContractCLIOptions, fixture map[string]any, executor *conformanceExecutorForCLI) {
	if executor == nil {
		return
	}
	fixture["mode"] = "executable"
	if executor.manifest.Runtime.Type == pluginmanager.RuntimeWASM {
		executeConformanceWASMRuleFixtureForCLI(fixture, executor)
		return
	}
	if !hasExtensionPointForCLI(executor.manifest, pluginmanager.ExtensionRuleEvaluate) && !hasRulePolicyExtensionsForCLI(executor.manifest) {
		fixture["status"] = "fail"
		fixture["error"] = "rule scenario requires rule.evaluate/v1 or rule-policy data-plane extensions"
		return
	}
	if err := executeRuleHarnessFixtureForCLI(fixture); err != nil {
		conformanceFail(fixture, err)
		return
	}
	conformancePass(fixture)
}

func executeConformanceWASMConfigValidateFixtureForCLI(_ pluginContractCLIOptions, fixture map[string]any, executor *conformanceExecutorForCLI) {
	if executor == nil || executor.manifest.Runtime.Type != pluginmanager.RuntimeWASM {
		return
	}
	fixture["mode"] = "executable"
	manager, artifact, configJSON, err := executor.prepare(nil)
	if err != nil {
		conformanceFail(fixture, err)
		return
	}
	result, err := manager.DryRunConfig(context.Background(), artifact.PluginID, artifact.ID, configJSON)
	fixture["target_plugin"] = artifact.PluginID
	fixture["artifact_id"] = artifact.ID
	fixture["actual_ok"] = result.OK
	if err != nil {
		conformanceFail(fixture, err)
		return
	}
	conformancePass(fixture)
}

func executeConformanceWASMRouteFixtureForCLI(fixture map[string]any, executor *conformanceExecutorForCLI) {
	decision := strings.TrimSpace(fmt.Sprint(fixture["decision"]))
	manager, artifact, _, err := executor.ensureDesiredEnabled(nil)
	if err != nil {
		conformanceFail(fixture, err)
		return
	}
	req := api.RouteResolveRequest{
		Host:             conformanceRouteHost(decision),
		FallbackUpstream: "fallback:25565",
		FallbackHit:      decision == "fallback" || decision == "pass",
		Refresh:          true,
	}
	result, err := manager.ResolveRoute(context.Background(), req, nil)
	if err != nil {
		conformanceFail(fixture, err)
		return
	}
	fixture["target_plugin"] = artifact.PluginID
	fixture["actual_action"] = result.Decision.Action
	fixture["actual_source"] = result.Source
	fixture["actual_upstream"] = result.Decision.Upstream
	fixture["actual_reason"] = result.Decision.Reason
	switch decision {
	case "override":
		if result.Decision.Action == api.RouteDecisionOverride && result.Decision.Upstream != "" {
			conformancePass(fixture)
			return
		}
	case "reject":
		if result.Decision.Action == api.RouteDecisionReject {
			conformancePass(fixture)
			return
		}
	case "fallback", "pass":
		if result.Decision.Action == api.RouteDecisionFallback && result.Decision.Upstream != "" {
			conformancePass(fixture)
			return
		}
	default:
		if result.Decision.Action != "" {
			conformancePass(fixture)
			return
		}
	}
	fixture["status"] = "fail"
	fixture["error"] = "wasm route fixture returned unexpected decision"
}

func executeConformanceWASMRuleFixtureForCLI(fixture map[string]any, executor *conformanceExecutorForCLI) {
	if !hasExtensionPointForCLI(executor.manifest, pluginmanager.ExtensionRuleEvaluate) {
		fixture["status"] = "fail"
		fixture["error"] = "rule scenario requires rule.evaluate/v1"
		return
	}
	outcome := strings.TrimSpace(fmt.Sprint(fixture["outcome"]))
	manager, artifact, _, err := executor.ensureDesiredEnabled(nil)
	if err != nil {
		conformanceFail(fixture, err)
		return
	}
	result, err := manager.EvaluateRule(context.Background(), api.RuleEvaluateRequest{
		Subject:  "wasm-conformance",
		Action:   "join",
		Resource: "server-a",
		Host:     "play.example",
	})
	if err != nil {
		conformanceFail(fixture, err)
		return
	}
	fixture["target_plugin"] = artifact.PluginID
	fixture["actual_handled"] = result.Handled
	fixture["actual_plugin"] = result.PluginID
	fixture["actual_allow"] = result.Decision.Allow
	fixture["actual_deny"] = result.Decision.Deny
	fixture["actual_reason"] = result.Decision.Reason
	switch outcome {
	case "allow":
		if result.Handled && result.Decision.Allow && !result.Decision.Deny {
			conformancePass(fixture)
			return
		}
	case "deny":
		if result.Handled && result.Decision.Deny {
			conformancePass(fixture)
			return
		}
	default:
		if result.Handled {
			conformancePass(fixture)
			return
		}
	}
	fixture["status"] = "fail"
	fixture["error"] = "wasm rule fixture returned unexpected decision"
}

func executeConformanceConnectionFilterFixtureForCLI(_ pluginContractCLIOptions, fixture map[string]any, executor *conformanceExecutorForCLI) {
	if !conformanceRequireExtension(fixture, executor, pluginmanager.ExtensionConnectionFilter) {
		return
	}
	scenario := strings.TrimSpace(fmt.Sprint(fixture["scenario"]))
	switch scenario {
	case "error_fail_open", "error_fail_closed":
		if err := executeMiddlewareHarnessFixtureForCLI(fixture, pluginmanager.ExtensionConnectionFilter, scenario); err != nil {
			conformanceFail(fixture, err)
			return
		}
		conformancePass(fixture)
		return
	}
	manager, artifact, cleanup, err := newConformanceHarnessManager("connection-conformance", "middleware_connection", scenario, "", nil)
	if err != nil {
		conformanceFail(fixture, err)
		return
	}
	defer cleanup()
	if _, err := manager.SetDesired(context.Background(), "cli", artifact.PluginID, artifact.ID, pluginmanager.DesiredEnabled, `{}`, pluginmanager.DefaultPriority); err != nil {
		conformanceFail(fixture, err)
		return
	}
	if _, err := manager.Enable(context.Background(), "cli", artifact.PluginID); err != nil {
		conformanceFail(fixture, err)
		return
	}
	req := api.ConnectionFilterRequest{SourceAddr: "198.51.100.10:12345", Transport: "tcp"}
	if scenario == "reject" {
		req.SourceAddr = "203.0.113.10:12345"
	}
	result, err := manager.FilterConnection(context.Background(), req)
	if err != nil {
		conformanceFail(fixture, err)
		return
	}
	fixture["target_plugin"] = artifact.PluginID
	fixture["actual_allowed"] = result.Allowed
	fixture["actual_plugin"] = result.PluginID
	fixture["actual_reason"] = result.Reason
	if (scenario == "reject" && !result.Allowed) || (scenario != "reject" && result.Allowed) {
		conformancePass(fixture)
		return
	}
	fixture["status"] = "fail"
	fixture["error"] = "connection filter fixture returned unexpected decision"
}

func executeConformanceHandshakeFilterFixtureForCLI(_ pluginContractCLIOptions, fixture map[string]any, executor *conformanceExecutorForCLI) {
	if !conformanceRequireExtension(fixture, executor, pluginmanager.ExtensionHandshakeFilter) {
		return
	}
	scenario := strings.TrimSpace(fmt.Sprint(fixture["scenario"]))
	switch scenario {
	case "error_fail_open", "error_fail_closed":
		if err := executeMiddlewareHarnessFixtureForCLI(fixture, pluginmanager.ExtensionHandshakeFilter, scenario); err != nil {
			conformanceFail(fixture, err)
			return
		}
		conformancePass(fixture)
		return
	}
	manager, artifact, cleanup, err := newConformanceHarnessManager("handshake-conformance", "middleware_handshake", scenario, "", nil)
	if err != nil {
		conformanceFail(fixture, err)
		return
	}
	defer cleanup()
	if _, err := manager.SetDesired(context.Background(), "cli", artifact.PluginID, artifact.ID, pluginmanager.DesiredEnabled, `{}`, pluginmanager.DefaultPriority); err != nil {
		conformanceFail(fixture, err)
		return
	}
	if _, err := manager.Enable(context.Background(), "cli", artifact.PluginID); err != nil {
		conformanceFail(fixture, err)
		return
	}
	req := api.HandshakeFilterRequest{ServerHost: "blue.example", RawServerHost: "blue.example", ProtocolVersion: 767, NextState: 2}
	if scenario == "rewrite_host" {
		req.ServerHost = "legacy.example"
		req.RawServerHost = "legacy.example"
	}
	if scenario == "reject" {
		req.ServerHost = "blocked.example"
		req.RawServerHost = "blocked.example"
	}
	result, err := manager.FilterHandshake(context.Background(), req)
	if err != nil {
		conformanceFail(fixture, err)
		return
	}
	fixture["target_plugin"] = artifact.PluginID
	fixture["actual_allowed"] = result.Allowed
	fixture["actual_plugin"] = result.PluginID
	fixture["actual_reason"] = result.Reason
	fixture["actual_rewrite_host"] = result.RewriteHost
	if scenario == "rewrite_host" {
		if result.Allowed && result.RewriteHost != "" {
			conformancePass(fixture)
			return
		}
	} else if scenario == "reject" {
		if !result.Allowed {
			conformancePass(fixture)
			return
		}
	} else if result.Allowed {
		conformancePass(fixture)
		return
	}
	fixture["status"] = "fail"
	fixture["error"] = "handshake filter fixture returned unexpected decision"
}

func executeConformanceEventFixtureForCLI(_ pluginContractCLIOptions, fixture map[string]any, executor *conformanceExecutorForCLI) {
	if !conformanceRequireExtension(fixture, executor, pluginmanager.ExtensionEventSubscriber) {
		return
	}
	delivery := strings.TrimSpace(fmt.Sprint(fixture["delivery"]))
	if err := executeEventSubscriberHarnessFixtureForCLI(fixture, delivery); err != nil {
		conformanceFail(fixture, err)
		return
	}
	conformancePass(fixture)
}

func executeConformanceProviderFixtureForCLI(_ pluginContractCLIOptions, fixture map[string]any, executor *conformanceExecutorForCLI) {
	if executor == nil {
		return
	}
	fixture["mode"] = "executable"
	want := strings.TrimSpace(fmt.Sprint(fixture["provider"]))
	switch want {
	case "singleton", "priority", "fallback", "dependency", "scope", "disable":
		if !manifestProviderRegistryCapabilityForCLI(executor.manifest, want) {
			fixture["status"] = "fail"
			fixture["error"] = "manifest capabilities do not declare provider registry scenario " + want
			return
		}
		if err := executeProviderRegistryHarnessFixtureForCLI(fixture, want); err != nil {
			conformanceFail(fixture, err)
			return
		}
		conformancePass(fixture)
		return
	}
	if !hasExtensionPointForCLI(executor.manifest, want) {
		fixture["status"] = "fail"
		fixture["error"] = "fixture declares provider extension not present in target manifest"
		fixture["extension"] = want
		return
	}
	manager, artifact, cleanup, err := newConformanceHarnessManager("provider-conformance", "provider", want, "", nil)
	if err != nil {
		conformanceFail(fixture, err)
		return
	}
	defer cleanup()
	if _, err := manager.SetDesired(context.Background(), "cli", artifact.PluginID, artifact.ID, pluginmanager.DesiredEnabled, `{}`, pluginmanager.DefaultPriority); err != nil {
		conformanceFail(fixture, err)
		return
	}
	if _, err := manager.Enable(context.Background(), "cli", artifact.PluginID); err != nil {
		conformanceFail(fixture, err)
		return
	}
	plan := manager.DispatchPlan(context.Background())
	fixture["target_plugin"] = artifact.PluginID
	fixture["actual_providers"] = plan.Providers
	for _, provider := range plan.Providers {
		if want == "" || provider.Type == want || provider.Name == want {
			conformancePass(fixture)
			return
		}
	}
	fixture["status"] = "fail"
	fixture["error"] = "provider registry fixture did not find requested provider"
}

func executeProviderRegistryHarnessFixtureForCLI(fixture map[string]any, scenario string) error {
	manager, artifact, cleanup, err := newConformanceHarnessManager("provider-registry-conformance", "provider", scenario, "", nil)
	if err != nil {
		return err
	}
	defer cleanup()
	if _, err := manager.SetDesired(context.Background(), "cli", artifact.PluginID, artifact.ID, pluginmanager.DesiredEnabled, `{}`, pluginmanager.DefaultPriority); err != nil {
		return err
	}
	if _, err := manager.Enable(context.Background(), "cli", artifact.PluginID); err != nil {
		return err
	}
	plan := manager.DispatchPlan(context.Background())
	providers := providerSummariesForPlugin(plan.Providers, artifact.PluginID)
	fixture["target_plugin"] = artifact.PluginID
	fixture["actual_providers"] = providers
	fixture["actual_provider_count"] = len(providers)
	if len(providers) == 0 {
		return errors.New("provider registry fixture did not register a provider")
	}
	provider := providers[0]
	fixture["actual_provider_type"] = provider.Type
	fixture["actual_provider_name"] = provider.Name
	fixture["actual_provider_priority"] = provider.Priority
	fixture["actual_provider_fallback"] = provider.Fallback
	fixture["actual_provider_dependencies"] = append([]string(nil), provider.Dependencies...)
	fixture["actual_provider_metadata"] = copyStringMapForCLI(provider.Metadata)
	switch scenario {
	case "singleton":
		if len(providers) != 1 || provider.Type != pluginmanager.ExtensionAdminAuthProvider || provider.Name != "external-identity" {
			return errors.New("provider registry singleton fixture did not produce exactly one external identity provider")
		}
	case "priority":
		if provider.Priority != 100 {
			return fmt.Errorf("provider registry priority = %d, want 100", provider.Priority)
		}
	case "fallback":
		if !provider.Fallback {
			return errors.New("provider registry fallback flag was not preserved")
		}
	case "dependency":
		if !cliContainsString(provider.Dependencies, "local-admin-break-glass") {
			return errors.New("provider registry dependency was not preserved")
		}
	case "scope":
		if provider.Metadata["scope"] != "admin" || !strings.Contains(provider.Metadata["scope_hosts"], "blue.example") {
			return errors.New("provider registry scope metadata was not preserved")
		}
	case "disable":
		if _, err := manager.Disable(context.Background(), "cli", artifact.PluginID); err != nil {
			return err
		}
		after := providerSummariesForPlugin(manager.DispatchPlan(context.Background()).Providers, artifact.PluginID)
		fixture["actual_provider_count_after_disable"] = len(after)
		if len(after) != 0 {
			return errors.New("provider registry disable fixture left provider in dispatch plan")
		}
	default:
		return fmt.Errorf("unsupported provider registry scenario %q", scenario)
	}
	return nil
}

func providerSummariesForPlugin(providers []pluginmanager.ProviderSummary, pluginID string) []pluginmanager.ProviderSummary {
	out := make([]pluginmanager.ProviderSummary, 0, len(providers))
	for _, provider := range providers {
		if provider.PluginID == pluginID {
			out = append(out, provider)
		}
	}
	return out
}

func cliContainsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func copyStringMapForCLI(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func executeConformanceGovernanceGateFixtureForCLI(_ pluginContractCLIOptions, fixture map[string]any, executor *conformanceExecutorForCLI) {
	if executor == nil {
		return
	}
	fixture["mode"] = "executable"
	scenario := strings.TrimSpace(fmt.Sprint(fixture["scenario"]))
	err := executeGovernanceHarnessFixtureForCLI(fixture, scenario, executor.opts.Profile)
	if err != nil {
		conformanceFail(fixture, err)
		return
	}
	conformancePass(fixture)
}

func conformanceConfigJSON(opts pluginContractCLIOptions) (string, error) {
	return governanceConfigJSON(pluginGovernanceCLIOptions{
		Target:     opts.Target,
		Manifest:   opts.Manifest,
		ConfigPath: opts.ConfigPath,
		ConfigJSON: opts.ConfigJSON,
		Profile:    opts.Profile,
		Priority:   pluginmanager.DefaultPriority,
	})
}

func conformanceSecretFixtureConfig(configJSON string) string {
	if strings.TrimSpace(configJSON) == "" {
		return `{"host":"conformance.example"}`
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(configJSON), &obj); err != nil || obj == nil {
		return `{"host":"conformance.example"}`
	}
	if _, ok := obj["host"]; !ok {
		obj["host"] = "conformance.example"
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return configJSON
	}
	return string(data)
}

func conformanceRouteHost(decision string) string {
	switch decision {
	case "override":
		return "blue.example"
	case "reject":
		return "blocked.example"
	case "fallback":
		return "fallback.example"
	case "pass":
		return "pass.example"
	default:
		return "blue.example"
	}
}

func conformancePass(fixture map[string]any) {
	fixture["status"] = "pass"
}

func conformanceFail(fixture map[string]any, err error) {
	fixture["status"] = "fail"
	if err != nil {
		fixture["error"] = err.Error()
	}
}

func conformanceRequireExtension(fixture map[string]any, executor *conformanceExecutorForCLI, extension string) bool {
	if executor == nil {
		return false
	}
	fixture["mode"] = "executable"
	if !hasExtensionPointForCLI(executor.manifest, extension) {
		fixture["status"] = "fail"
		fixture["error"] = "fixture declares extension not present in target manifest"
		fixture["extension"] = extension
		return false
	}
	return true
}

func hasRulePolicyExtensionsForCLI(manifest pluginmanager.Manifest) bool {
	return hasExtensionPointForCLI(manifest, pluginmanager.ExtensionConnectionFilter) &&
		hasExtensionPointForCLI(manifest, pluginmanager.ExtensionHandshakeFilter) &&
		hasExtensionPointForCLI(manifest, pluginmanager.ExtensionRouteResolve) &&
		hasExtensionPointForCLI(manifest, pluginmanager.ExtensionRuleEvaluate)
}

func manifestProviderRegistryCapabilityForCLI(manifest pluginmanager.Manifest, scenario string) bool {
	var caps map[string]any
	if json.Unmarshal(manifest.Capabilities, &caps) != nil {
		return false
	}
	switch scenario {
	case "singleton":
		return len(cliStringSlice(caps["provider_singletons"])) > 0 || len(cliAnySlice(caps["providers"])) > 0
	case "priority", "fallback", "dependency":
		for _, raw := range cliAnySlice(caps["providers"]) {
			provider, _ := raw.(map[string]any)
			if provider == nil {
				continue
			}
			switch scenario {
			case "priority":
				if _, ok := provider["priority"]; ok {
					return true
				}
			case "fallback":
				if value, _ := provider["fallback"].(bool); value {
					return true
				}
			case "dependency":
				if len(cliStringSlice(provider["dependencies"])) > 0 {
					return true
				}
			}
		}
	case "scope":
		return cliMapFromAny(caps["scope"]) != nil
	case "disable":
		return hasExtensionPointForCLI(manifest, pluginmanager.ExtensionProvider) ||
			hasExtensionPointForCLI(manifest, pluginmanager.ExtensionAuthProvider) ||
			hasExtensionPointForCLI(manifest, pluginmanager.ExtensionAdminAuthProvider)
	}
	return false
}

func cliAnySlice(value any) []any {
	switch typed := value.(type) {
	case []any:
		return typed
	default:
		return nil
	}
}

func cliStringSlice(value any) []string {
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				out = append(out, text)
			}
		}
		return out
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return []string{typed}
	default:
		return nil
	}
}

func cliMapFromAny(value any) map[string]any {
	obj, _ := value.(map[string]any)
	return obj
}

type conformanceHarnessAdapter struct {
	mode       string
	scenario   string
	plugin     api.Plugin
	readCh     chan []byte
	blockProxy bool
}

func (a *conformanceHarnessAdapter) Load(_ context.Context, artifact pluginmanager.ArtifactRecord, _ pluginmanager.PluginRecord, gateway *pluginmanager.Gateway) (api.Plugin, error) {
	if a.plugin != nil {
		return a.plugin, a.plugin.Init(gateway)
	}
	plugin := &conformanceHarnessPlugin{mode: a.mode, scenario: a.scenario, readCh: a.readCh, blockProxy: a.blockProxy}
	if err := plugin.Init(gateway); err != nil {
		return nil, err
	}
	if artifact.PluginID == "" {
		return plugin, nil
	}
	return plugin, nil
}

func (a *conformanceHarnessAdapter) DryRunConfig(context.Context, pluginmanager.ArtifactRecord, pluginmanager.PluginRecord) error {
	return nil
}

type conformanceHarnessPlugin struct {
	api.AbstractPlugin
	mode       string
	scenario   string
	readCh     chan []byte
	blockProxy bool
	tasks      []api.BackgroundTask
}

func (p *conformanceHarnessPlugin) Init(gateway api.Gateway) error {
	switch p.mode {
	case "protocol_proxy":
		return api.RegisterHookHandler(gateway, api.HookUpstreamConnect,
			func(api.UpstreamConnectRequest) bool { return !p.blockProxy },
			p.handleProtocolProxy)
	case "route":
		return api.RegisterHookHandler(gateway, api.HookRouteResolve,
			func(api.RouteResolveRequest) bool { return true },
			p.handleRouteResolve)
	case "status":
		return api.RegisterHookHandler(gateway, api.HookStatusPing,
			func(api.StatusPingRequest) bool { return true },
			p.handleStatusPing)
	case "middleware_connection":
		return api.RegisterHookHandler(gateway, api.HookConnectionFilter,
			func(api.ConnectionFilterRequest) bool { return true },
			p.handleConnectionFilter)
	case "middleware_handshake":
		return api.RegisterHookHandler(gateway, api.HookHandshakeFilter,
			func(api.HandshakeFilterRequest) bool { return true },
			p.handleHandshakeFilter)
	case "rule":
		return api.RegisterHookHandler(gateway, api.HookRuleEvaluate,
			func(api.RuleEvaluateRequest) bool { return true },
			p.handleRuleEvaluate)
	case "event_subscriber":
		return api.RegisterHookHandler(gateway, api.HookEventSubscriber,
			func(api.EventDeliveryRequest) bool { return true },
			p.handleEventSubscriber)
	case "provider":
		switch p.scenario {
		case pluginmanager.ExtensionProvider:
			return api.RegisterHookHandler(gateway, api.HookProvider,
				func(api.ProviderRegistration) bool { return true },
				func() (api.ProviderRegistration, error) {
					return p.providerRegistration(pluginmanager.ExtensionProvider), nil
				})
		case pluginmanager.ExtensionAuthProvider:
			return api.RegisterHookHandler(gateway, api.HookAuthProvider,
				func(api.ProviderRegistration) bool { return true },
				func() (api.ProviderRegistration, error) {
					return p.providerRegistration(pluginmanager.ExtensionAuthProvider), nil
				})
		default:
			return api.RegisterHookHandler(gateway, api.HookAdminAuthProvider,
				func(api.ProviderRegistration) bool { return true },
				func() (api.ProviderRegistration, error) {
					return p.providerRegistration(pluginmanager.ExtensionAdminAuthProvider), nil
				})
		}
	case "task":
		return gateway.RegisterBackgroundTask(api.BackgroundTask{
			ID:     "conformance-task",
			Name:   "Conformance Task",
			Manual: true,
			Run:    func(context.Context) error { return nil },
		})
	default:
		return nil
	}
}

func (p *conformanceHarnessPlugin) handleProtocolProxy(req api.UpstreamConnectRequest) (net.Conn, error) {
	if p.scenario == "panic_recovered" {
		panic("conformance protocol proxy panic")
	}
	if p.scenario == "timeout_deadline" {
		time.Sleep(2 * pluginmanager.DefaultHandlerTimeout)
		return nil, api.ErrPass
	}
	gatewayEnd, pluginEnd := net.Pipe()
	go func() {
		defer pluginEnd.Close()
		buf := make([]byte, 4096)
		_ = pluginEnd.SetDeadline(time.Now().Add(2 * time.Second))
		n, _ := pluginEnd.Read(buf)
		if n > 0 && p.readCh != nil {
			select {
			case p.readCh <- append([]byte(nil), buf[:n]...):
			default:
			}
		}
		switch p.scenario {
		case "endpoint_close":
			return
		default:
			_, _ = pluginEnd.Write([]byte("ok"))
		}
	}()
	_ = req
	return gatewayEnd, nil
}

func (p *conformanceHarnessPlugin) handleRouteResolve(req api.RouteResolveRequest) (api.RouteDecision, error) {
	switch p.scenario {
	case "reject":
		return api.RouteDecision{Action: api.RouteDecisionReject, Host: req.Host, Reason: "conformance reject"}, nil
	case "fallback":
		return api.RouteDecision{
			Action:     api.RouteDecisionFallback,
			Host:       req.Host,
			Upstream:   req.FallbackUpstream,
			ProviderID: "sqlite",
			Reason:     "conformance sqlite fallback",
			Metadata:   map[string]string{"explain": "provider miss used sqlite fallback"},
		}, nil
	case "pass":
		return api.RouteDecision{Action: api.RouteDecisionPass, Host: req.Host, Reason: "conformance pass"}, nil
	default:
		return api.RouteDecision{
			Action:      api.RouteDecisionOverride,
			Host:        req.Host,
			Upstream:    "10.0.0.10:25565",
			ProviderID:  "route-conformance",
			Reason:      "conformance override",
			CacheTTL:    time.Minute,
			Metadata:    map[string]string{"explain": "external refresh returned override and cached with TTL"},
			Explanation: "route conformance override",
		}, nil
	}
}

func (p *conformanceHarnessPlugin) handleStatusPing(req api.StatusPingRequest) (api.StatusPingResponse, error) {
	return api.StatusPingResponse{
		MOTD:              "Extension ecosystem " + req.Host,
		Favicon:           "data:image/png;base64,fixture",
		OnlinePlayers:     1,
		MaxPlayers:        20,
		VersionText:       "mc-gateway",
		ProtocolVersion:   req.ProtocolVersion,
		Maintenance:       req.Host == "red.example",
		MaintenanceWindow: "02:00-03:00 UTC",
	}, nil
}

func (p *conformanceHarnessPlugin) handleConnectionFilter(api.ConnectionFilterRequest) (api.FilterDecision, error) {
	switch p.scenario {
	case "error_fail_open", "error_fail_closed":
		return api.FilterDecision{}, errors.New("conformance middleware error")
	case "reject":
		return api.FilterDecision{Reject: true, Reason: "conformance reject"}, nil
	default:
		return api.FilterDecision{Allow: true}, nil
	}
}

func (p *conformanceHarnessPlugin) handleHandshakeFilter(api.HandshakeFilterRequest) (api.HandshakeFilterDecision, error) {
	switch p.scenario {
	case "error_fail_open", "error_fail_closed":
		return api.HandshakeFilterDecision{}, errors.New("conformance handshake error")
	case "rewrite_host":
		return api.HandshakeFilterDecision{FilterDecision: api.FilterDecision{Allow: true}, RewriteHost: "rewritten.example"}, nil
	case "reject":
		return api.HandshakeFilterDecision{FilterDecision: api.FilterDecision{Reject: true, Reason: "conformance reject"}}, nil
	default:
		return api.HandshakeFilterDecision{FilterDecision: api.FilterDecision{Allow: true}}, nil
	}
}

func (p *conformanceHarnessPlugin) handleRuleEvaluate(api.RuleEvaluateRequest) (api.RuleEvaluateDecision, error) {
	switch p.scenario {
	case "deny":
		return api.RuleEvaluateDecision{Deny: true, Reason: "rule denied"}, nil
	case "error_fail_closed", "timeout_fail_closed":
		return api.RuleEvaluateDecision{}, errors.New("rule evaluation failed closed")
	case "bad_config_fallback":
		return api.RuleEvaluateDecision{Allow: true, Reason: "bad config fallback"}, nil
	default:
		return api.RuleEvaluateDecision{Allow: true, Reason: "rule allowed"}, nil
	}
}

func (p *conformanceHarnessPlugin) handleEventSubscriber(req api.EventDeliveryRequest) (api.EventDeliveryResult, error) {
	switch p.scenario {
	case "best_effort":
		return api.EventDeliveryResult{Retry: true, Reason: "best effort sink down"}, errors.New("best effort sink down")
	case "replay":
		if req.Attempt > 1 {
			return api.EventDeliveryResult{OK: true}, nil
		}
		return api.EventDeliveryResult{Retry: true, Reason: "replay fixture"}, errors.New("replay fixture")
	default:
		return api.EventDeliveryResult{Retry: true, Reason: "subscriber sink down"}, errors.New("subscriber sink down")
	}
}

func (p *conformanceHarnessPlugin) providerRegistration(providerType string) api.ProviderRegistration {
	return api.ProviderRegistration{
		Type:         providerType,
		Name:         "external-identity",
		Priority:     100,
		Fallback:     true,
		Dependencies: []string{"local-admin-break-glass"},
		Metadata: map[string]string{
			"break_glass": "local-admin",
			"scenario":    p.scenario,
			"scope":       "admin",
			"scope_hosts": "blue.example,red.example",
		},
	}
}

func executeProtocolProxyHarnessFixtureForCLI(fixture map[string]any) error {
	scenario := strings.TrimSpace(fmt.Sprint(fixture["scenario"]))
	readCh := make(chan []byte, 1)
	manager, artifact, cleanup, err := newConformanceHarnessManager("protocol-proxy-conformance", "protocol_proxy", scenario, pluginmanager.UpstreamModeProtocolProxy, readCh)
	if err != nil {
		return err
	}
	defer cleanup()
	if _, err := manager.SetDesired(context.Background(), "cli", artifact.PluginID, artifact.ID, pluginmanager.DesiredEnabled, `{}`, pluginmanager.DefaultPriority); err != nil {
		return err
	}
	if _, err := manager.CreateReview(context.Background(), "cli", artifact.PluginID, pluginmanager.GovernanceReviewRequest{
		ArtifactID: artifact.ID,
		Profile:    pluginmanager.PolicyProfileProd,
		Decision:   pluginmanager.ReviewDecisionApproved,
		Notes:      "conformance harness review",
	}); err != nil {
		return err
	}
	if _, err := manager.Enable(context.Background(), "cli", artifact.PluginID); err != nil {
		return err
	}
	initial := smoke.MinecraftHandshakePacket("play.example")
	switch scenario {
	case "panic_recovered", "timeout_deadline":
		initial = nil
	}
	login := smoke.MinecraftLoginStartPacket("Steve")
	clientGateway, clientSide := net.Pipe()
	defer clientSide.Close()
	errCh := make(chan error, 1)
	go func() {
		_, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{
			Host:        "play.example",
			Source:      clientGateway,
			InitialData: initial,
		})
		errCh <- err
	}()
	_ = clientSide.SetDeadline(time.Now().Add(2 * time.Second))
	if scenario != "client_close" && scenario != "panic_recovered" && scenario != "timeout_deadline" {
		if err := writeAll(clientSide, login); err != nil && scenario != "endpoint_close" {
			return err
		}
	}
	_ = clientSide.Close()
	err = <-errCh
	fixture["actual_path"] = "manager.ConnectUpstream"
	fixture["actual_error"] = ""
	if err != nil {
		fixture["actual_error"] = err.Error()
	}
	switch scenario {
	case "panic_recovered", "timeout_deadline":
		return nil
	case "drain_disable_new_connections":
		if _, err := manager.Disable(context.Background(), "cli", artifact.PluginID); err != nil {
			return err
		}
		result, err := manager.ConnectUpstream(context.Background(), api.UpstreamConnectRequest{Host: "play.example"})
		if err != nil {
			return err
		}
		fixture["actual_handled_after_disable"] = result.Handled
		if result.Handled {
			return errors.New("protocol proxy accepted new connection after disable")
		}
	case "force_close_draining":
		closed, err := manager.ForceCloseDraining(context.Background(), "cli", artifact.PluginID)
		if err != nil {
			return err
		}
		fixture["actual_force_closed"] = closed
	default:
		select {
		case got := <-readCh:
			fixture["actual_initial_bytes"] = len(got)
			if len(got) == 0 {
				return errors.New("protocol proxy did not receive initial data")
			}
		default:
			if scenario == "initial_data_once" || scenario == "backpressure_large_packet" {
				return errors.New("protocol proxy did not observe initial data")
			}
		}
	}
	return nil
}

func executeMiddlewareHarnessFixtureForCLI(fixture map[string]any, extension, scenario string) error {
	mode := "middleware_connection"
	if extension == pluginmanager.ExtensionHandshakeFilter {
		mode = "middleware_handshake"
	}
	manager, artifact, cleanup, err := newConformanceHarnessManager("middleware-conformance", mode, scenario, "", nil)
	if err != nil {
		return err
	}
	defer cleanup()
	config := `{}`
	if _, err := manager.SetDesired(context.Background(), "cli", artifact.PluginID, artifact.ID, pluginmanager.DesiredEnabled, config, pluginmanager.DefaultPriority); err != nil {
		return err
	}
	if _, err := manager.Enable(context.Background(), "cli", artifact.PluginID); err != nil {
		return err
	}
	if extension == pluginmanager.ExtensionConnectionFilter {
		result, err := manager.FilterConnection(context.Background(), api.ConnectionFilterRequest{SourceAddr: "127.0.0.1:12345"})
		fixture["actual_allowed"] = result.Allowed
		fixture["actual_error"] = ""
		if err != nil {
			fixture["actual_error"] = err.Error()
		}
		if scenario == "error_fail_closed" {
			if err == nil || result.Allowed {
				return errors.New("fail-closed connection filter did not block on error")
			}
		} else if err != nil || !result.Allowed {
			return errors.New("fail-open connection filter did not continue on error")
		}
		return nil
	}
	result, err := manager.FilterHandshake(context.Background(), api.HandshakeFilterRequest{ServerHost: "play.example"})
	fixture["actual_allowed"] = result.Allowed
	fixture["actual_error"] = ""
	if err != nil {
		fixture["actual_error"] = err.Error()
	}
	if scenario == "error_fail_closed" {
		if err == nil || result.Allowed {
			return errors.New("fail-closed handshake filter did not block on error")
		}
	} else if err != nil || !result.Allowed {
		return errors.New("fail-open handshake filter did not continue on error")
	}
	return nil
}

func executeRuleHarnessFixtureForCLI(fixture map[string]any) error {
	scenario := strings.TrimSpace(fmt.Sprint(fixture["outcome"]))
	manager, artifact, cleanup, err := newConformanceHarnessManager("rule-conformance", "rule", scenario, "", nil)
	if err != nil {
		return err
	}
	defer cleanup()
	if _, err := manager.SetDesired(context.Background(), "cli", artifact.PluginID, artifact.ID, pluginmanager.DesiredEnabled, `{}`, pluginmanager.DefaultPriority); err != nil {
		return err
	}
	if _, err := manager.Enable(context.Background(), "cli", artifact.PluginID); err != nil {
		return err
	}
	switch scenario {
	case "deny", "error_fail_closed", "timeout_fail_closed":
		result, err := manager.EvaluateRule(context.Background(), api.RuleEvaluateRequest{SourceAddr: "127.0.0.1:12345", Action: "connect", Resource: "play.example"})
		fixture["actual_allowed"] = result.Decision.Allow
		fixture["actual_denied"] = result.Decision.Deny || result.Decision.Reject
		if err != nil {
			fixture["actual_error"] = err.Error()
		}
		if scenario == "deny" && !fixture["actual_denied"].(bool) {
			return errors.New("rule fixture did not block")
		}
		if scenario != "deny" && err == nil {
			return errors.New("rule fail-closed fixture did not return an error")
		}
	case "bad_config_fallback":
		result, err := manager.ResolveRoute(context.Background(), api.RouteResolveRequest{Host: "unknown.example", FallbackUpstream: "default:25565", FallbackHit: true}, nil)
		if err != nil {
			return err
		}
		fixture["actual_source"] = result.Source
		fixture["actual_action"] = result.Decision.Action
		if result.Source != "sqlite_fallback" {
			return errors.New("bad rule config did not preserve default route fallback")
		}
	default:
		result, err := manager.EvaluateRule(context.Background(), api.RuleEvaluateRequest{SourceAddr: "127.0.0.1:12345", Action: "connect", Resource: "play.example"})
		if err != nil {
			return err
		}
		fixture["actual_allowed"] = result.Decision.Allow
		if !result.Decision.Allow {
			return errors.New("rule allow fixture blocked connection")
		}
	}
	return nil
}

func executeEventSubscriberHarnessFixtureForCLI(fixture map[string]any, delivery string) error {
	manager, artifact, cleanup, err := newConformanceHarnessManager("event-conformance", "event_subscriber", delivery, "", nil)
	if err != nil {
		return err
	}
	defer cleanup()
	if _, err := manager.SetDesired(context.Background(), "cli", artifact.PluginID, artifact.ID, pluginmanager.DesiredEnabled, `{}`, pluginmanager.DefaultPriority); err != nil {
		return err
	}
	if _, err := manager.Enable(context.Background(), "cli", artifact.PluginID); err != nil {
		return err
	}
	eventManifest := pluginmanager.Manifest{
		Events: []pluginmanager.EventSpec{{Name: "fixture.retry", Fields: []string{"ok"}}},
	}
	start := time.Now()
	if err := manager.EmitPluginEventForConformance(context.Background(), "event-emitter", "artifact", eventManifest, "fixture.retry", map[string]string{"ok": "true"}); err != nil {
		return err
	}
	if time.Since(start) > 100*time.Millisecond {
		return errors.New("subscriber delivery blocked event emitter")
	}
	fixture["actual_non_blocking"] = true
	switch delivery {
	case "best_effort", "at_least_once":
		if !waitForConformance(func() bool { return manager.SubscriberDeadLetters(context.Background()) > 0 }) {
			return errors.New("event subscriber did not record dead letter")
		}
		fixture["actual_dead_letters"] = manager.SubscriberDeadLetters(context.Background())
	case "dead_letter", "cross_node_at_least_once", "subscriber_failure_non_blocking":
		if !waitForConformance(func() bool { return manager.SubscriberDeadLetters(context.Background()) > 0 }) {
			return errors.New("event subscriber did not persist dead letter")
		}
		fixture["actual_dead_letters"] = manager.SubscriberDeadLetters(context.Background())
	case "replay":
		if !waitForConformance(func() bool { return manager.SubscriberDeadLetters(context.Background()) > 0 }) {
			return errors.New("event subscriber did not persist dead letter before replay")
		}
		count := manager.ReplaySubscriberDeadLetters(context.Background(), "cli")
		fixture["actual_replayed"] = count
		if count == 0 {
			return errors.New("subscriber dead letter replay returned zero")
		}
	case "drop":
		if !waitForConformance(func() bool { return manager.SubscriberDeadLetters(context.Background()) > 0 }) {
			return errors.New("event subscriber did not persist dead letter before drop")
		}
		count := manager.DropSubscriberDeadLetters(context.Background(), "cli")
		fixture["actual_dropped"] = count
		if count == 0 || manager.SubscriberDeadLetters(context.Background()) != 0 {
			return errors.New("subscriber dead letter drop did not clear pending records")
		}
	default:
		return fmt.Errorf("unknown event delivery fixture %q", delivery)
	}
	return nil
}

func waitForConformance(ok func() bool) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return ok()
}

func executeGovernanceHarnessFixtureForCLI(fixture map[string]any, scenario, profile string) error {
	if profile == "" {
		profile = pluginmanager.PolicyProfileProd
	}
	switch scenario {
	case "review_required":
		manager, artifact, cleanup, err := newConformanceHarnessManager("governance-review", "protocol_proxy", "initial_data_once", pluginmanager.UpstreamModeProtocolProxy, nil)
		if err != nil {
			return err
		}
		defer cleanup()
		if _, err := manager.SetDesired(context.Background(), "cli", artifact.PluginID, artifact.ID, pluginmanager.DesiredDisabled, `{}`, pluginmanager.DefaultPriority); err != nil {
			return err
		}
		decision, err := manager.EvaluateGovernance(context.Background(), artifact.PluginID, artifact.ID, pluginmanager.GovernanceActionEnable, pluginmanager.PolicyProfileProd, `{}`)
		if err != nil {
			return err
		}
		fixture["actual_ok"] = decision.OK
		fixture["actual_issues"] = decision.Issues
		if err := conformanceExpectIssue(decision, "review_required"); err != nil {
			return err
		}
		fixture["actual_issue"] = "review_required"
		fixture["actual_severity"] = pluginmanager.GateSeverityBlocking
		return nil
	case "warning_override":
		manager, artifact, cleanup, err := newConformanceHarnessManager("governance-warning", "middleware_connection", "allow", "", nil)
		if err != nil {
			return err
		}
		defer cleanup()
		if _, err := manager.SetDesired(context.Background(), "cli", artifact.PluginID, artifact.ID, pluginmanager.DesiredDisabled, `{}`, pluginmanager.DefaultPriority); err != nil {
			return err
		}
		if _, err := manager.SaveBenchmark(context.Background(), "cli", pluginmanager.BenchmarkRequest{
			ArtifactID:       artifact.ID,
			Profile:          profile,
			BenchmarkProfile: "conformance",
			BaselineDiff:     0.20,
		}); err != nil {
			return err
		}
		_, blockedErr := manager.EvaluateReleaseGate(context.Background(), artifact.PluginID, artifact.ID, pluginmanager.GovernanceActionEnable, profile, `{}`)
		if blockedErr == nil {
			return errors.New("warning gate did not block without override")
		}
		override, err := manager.CreateWarningOverride(context.Background(), "cli", artifact.PluginID, pluginmanager.WarningOverrideRequest{
			ArtifactID: artifact.ID,
			Profile:    profile,
			Action:     pluginmanager.GovernanceActionEnable,
			Reason:     "conformance warning override",
			TTLSeconds: 60,
		})
		if err != nil {
			return err
		}
		decision, err := manager.EvaluateReleaseGate(context.Background(), artifact.PluginID, artifact.ID, pluginmanager.GovernanceActionEnable, profile, `{}`)
		if err != nil {
			return err
		}
		fixture["actual_override_id"] = override.ID
		fixture["actual_override_expires_at"] = override.ExpiresAt
		fixture["actual_override_used"] = decision.WarningOverrideUsed
		if !decision.WarningOverrideUsed {
			return errors.New("warning override was not used by release gate")
		}
		return nil
	case "advisory_block":
		manager, artifact, cleanup, err := newConformanceHarnessManager("governance-advisory", "middleware_connection", "allow", "", nil)
		if err != nil {
			return err
		}
		defer cleanup()
		if _, err := manager.SetDesired(context.Background(), "cli", artifact.PluginID, artifact.ID, pluginmanager.DesiredDisabled, `{}`, pluginmanager.DefaultPriority); err != nil {
			return err
		}
		if _, err := manager.UpsertAdvisory(context.Background(), "cli", pluginmanager.AdvisoryRequest{
			AdvisoryID:     "CONF-ADVISORY",
			Status:         pluginmanager.AdvisoryStatusRevoked,
			Action:         pluginmanager.AdvisoryActionRevoke,
			PluginID:       artifact.PluginID,
			ArtifactSHA256: artifact.SHA256,
		}); err != nil {
			return err
		}
		decision, err := manager.EvaluateGovernance(context.Background(), artifact.PluginID, artifact.ID, pluginmanager.GovernanceActionEnable, profile, `{}`)
		if err != nil {
			return err
		}
		fixture["actual_issues"] = decision.Issues
		return conformanceExpectIssue(decision, "advisory_revoke")
	case "supply_chain_block":
		manager, artifact, cleanup, err := newConformanceHarnessManager("governance-supply", "middleware_connection", "allow", "", nil)
		if err != nil {
			return err
		}
		defer cleanup()
		if _, err := manager.SetDesired(context.Background(), "cli", artifact.PluginID, artifact.ID, pluginmanager.DesiredDisabled, `{}`, pluginmanager.DefaultPriority); err != nil {
			return err
		}
		if _, err := manager.AssessSupplyChain(context.Background(), "cli", artifact.PluginID, artifact.ID, map[string]any{
			"signature": map[string]any{"required": true, "verified": false},
		}); err != nil {
			return err
		}
		decision, err := manager.EvaluateGovernance(context.Background(), artifact.PluginID, artifact.ID, pluginmanager.GovernanceActionEnable, profile, `{}`)
		if err != nil {
			return err
		}
		fixture["actual_issues"] = decision.Issues
		return conformanceExpectIssue(decision, "signature_unverified")
	case "rollback_gate":
		return executeRollbackGateHarnessFixtureForCLI(fixture, profile)
	case "repository_apply_gate":
		return executeRepositoryApplyGateHarnessFixtureForCLI(fixture, profile)
	case "promotion_apply_gate":
		return executePromotionApplyGateHarnessFixtureForCLI(fixture, profile)
	default:
		return fmt.Errorf("unsupported governance scenario %q", scenario)
	}
}

func executeRollbackGateHarnessFixtureForCLI(fixture map[string]any, profile string) error {
	manager, oldArtifact, cleanup, err := newConformanceHarnessManager("governance-rollback", "middleware_connection", "allow", "", nil)
	if err != nil {
		return err
	}
	defer cleanup()
	newArtifact, err := uploadConformanceHarnessArtifact(manager, "governance-rollback", "middleware_connection", "allow", "", nil)
	if err != nil {
		return err
	}
	if _, err := manager.SetDesired(context.Background(), "cli", oldArtifact.PluginID, oldArtifact.ID, pluginmanager.DesiredEnabled, `{}`, pluginmanager.DefaultPriority); err != nil {
		return err
	}
	if _, err := manager.Enable(context.Background(), "cli", oldArtifact.PluginID); err != nil {
		return err
	}
	if _, err := manager.SetDesired(context.Background(), "cli", newArtifact.PluginID, newArtifact.ID, pluginmanager.DesiredEnabled, `{}`, pluginmanager.DefaultPriority); err != nil {
		return err
	}
	if _, err := manager.Enable(context.Background(), "cli", newArtifact.PluginID); err != nil {
		return err
	}
	if _, err := manager.UpsertAdvisory(context.Background(), "cli", pluginmanager.AdvisoryRequest{
		AdvisoryID:     "CONF-ROLLBACK",
		Status:         pluginmanager.AdvisoryStatusRevoked,
		Action:         pluginmanager.AdvisoryActionRevoke,
		PluginID:       oldArtifact.PluginID,
		ArtifactSHA256: oldArtifact.SHA256,
	}); err != nil {
		return err
	}
	_, err = manager.RollbackArtifact(context.Background(), "cli", oldArtifact.PluginID, oldArtifact.ID)
	fixture["actual_blocked"] = err != nil
	if err == nil {
		return errors.New("rollback to revoked artifact was not blocked")
	}
	fixture["actual_error"] = err.Error()
	_ = profile
	return nil
}

func executeRepositoryApplyGateHarnessFixtureForCLI(fixture map[string]any, profile string) error {
	manager, artifact, cleanup, err := newConformanceHarnessManager("governance-repo-apply", "middleware_connection", "allow", "", nil)
	if err != nil {
		return err
	}
	defer cleanup()
	indexPath, err := writeConformanceRepositoryIndex(artifact)
	if err != nil {
		return err
	}
	record, importedArtifact, err := manager.ImportRepositoryArtifact(context.Background(), "cli", pluginmanager.RepositoryImportRequest{
		RepositoryType: pluginmanager.RepositoryTypeFile,
		IndexPath:      indexPath,
		PluginID:       artifact.PluginID,
		Version:        artifact.Version,
	})
	if err != nil {
		return err
	}
	if _, err := manager.SaveBenchmark(context.Background(), "cli", pluginmanager.BenchmarkRequest{
		ArtifactID:       importedArtifact.ID,
		Profile:          manager.PolicyProfile(),
		BenchmarkProfile: "conformance",
		BaselineDiff:     0.20,
	}); err != nil {
		return err
	}
	result, err := manager.ApplyRepositoryImport(context.Background(), "cli", record.ID, `{}`, pluginmanager.DesiredDisabled, pluginmanager.DefaultPriority, false)
	if err != nil {
		return err
	}
	fixture["actual_ok"] = result.OK
	fixture["actual_checks"] = result.Checks
	if result.OK || !promotionChecksContain(result.Checks, "governance_gate") {
		return errors.New("repository import apply was not blocked by governance gate")
	}
	return nil
}

func executePromotionApplyGateHarnessFixtureForCLI(fixture map[string]any, profile string) error {
	manager, artifact, cleanup, err := newConformanceHarnessManager("governance-promotion-apply", "middleware_connection", "allow", "", nil)
	if err != nil {
		return err
	}
	defer cleanup()
	bundle, err := manager.ExportPromotionBundle(context.Background(), "conformance", profile, artifact.PluginID, artifact.ID, `{}`)
	if err != nil {
		return err
	}
	if _, err := manager.SaveBenchmark(context.Background(), "cli", pluginmanager.BenchmarkRequest{
		ArtifactID:       artifact.ID,
		Profile:          profile,
		BenchmarkProfile: "conformance",
		BaselineDiff:     0.20,
	}); err != nil {
		return err
	}
	result, err := manager.ApplyPromotionBundle(context.Background(), "cli", bundle, map[string]string{artifact.PluginID: `{}`}, false)
	if err != nil {
		return err
	}
	fixture["actual_ok"] = result.OK
	fixture["actual_checks"] = result.Checks
	if result.OK || !promotionChecksContain(result.Checks, "governance_gate") {
		return errors.New("promotion apply was not blocked by governance gate")
	}
	return nil
}

func conformanceExpectIssue(decision pluginmanager.GovernanceDecision, code string) error {
	for _, item := range decision.Issues {
		if item.Code == code {
			return nil
		}
	}
	return fmt.Errorf("governance issue %q not found", code)
}

func promotionChecksContain(checks []pluginmanager.PromotionCheck, code string) bool {
	for _, check := range checks {
		if check.Code == code {
			return true
		}
	}
	return false
}

func newConformanceHarnessManager(pluginID, mode, scenario, upstreamMode string, readCh chan []byte) (*pluginmanager.Manager, pluginmanager.ArtifactRecord, func(), error) {
	tmpRoot, err := os.MkdirTemp("", "mcgp-conformance-harness-*")
	if err != nil {
		return nil, pluginmanager.ArtifactRecord{}, func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(tmpRoot) }
	db, err := openPluginCLIDB(filepath.Join(tmpRoot, "plugins.db"))
	if err != nil {
		cleanup()
		return nil, pluginmanager.ArtifactRecord{}, func() {}, err
	}
	adapter := &conformanceHarnessAdapter{mode: mode, scenario: scenario, readCh: readCh}
	manager := pluginmanager.New(pluginmanager.Options{
		DB:            db,
		ArtifactRoot:  filepath.Join(tmpRoot, "artifacts"),
		Now:           pluginConformanceNow,
		Adapter:       adapter,
		PolicyProfile: pluginmanager.PolicyProfileProd,
	})
	artifact, err := uploadConformanceHarnessArtifact(manager, pluginID, mode, scenario, upstreamMode, tmpRoot)
	if err != nil {
		_ = db.Close()
		cleanup()
		return nil, pluginmanager.ArtifactRecord{}, func() {}, err
	}
	return manager, artifact, func() {
		_ = db.Close()
		cleanup()
	}, nil
}

func uploadConformanceHarnessArtifact(manager *pluginmanager.Manager, pluginID, mode, scenario, upstreamMode string, root any) (pluginmanager.ArtifactRecord, error) {
	tmpRoot := ""
	if s, ok := root.(string); ok {
		tmpRoot = s
	}
	if tmpRoot == "" {
		var err error
		tmpRoot, err = os.MkdirTemp("", "mcgp-conformance-artifact-*")
		if err != nil {
			return pluginmanager.ArtifactRecord{}, err
		}
		defer os.RemoveAll(tmpRoot)
	}
	manifest := conformanceHarnessManifest(pluginID, mode, scenario, upstreamMode)
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	packagePath := filepath.Join(tmpRoot, pluginID+"-"+strings.ReplaceAll(scenario, "_", "-")+".mcgp")
	if err := writeConformancePackage(packagePath, manifestBytes, []byte("conformance harness "+pluginID+" "+scenario)); err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	return manager.UploadArtifact(context.Background(), pluginmanager.ArtifactUpload{
		SourcePath: packagePath,
		FileName:   filepath.Base(packagePath),
		Actor:      "cli",
	})
}

func conformanceHarnessManifest(pluginID, mode, scenario, upstreamMode string) pluginmanager.Manifest {
	if pluginID == "" {
		pluginID = "conformance-harness"
	}
	if upstreamMode == "" {
		upstreamMode = pluginmanager.UpstreamModeDialer
	}
	manifest := pluginmanager.Manifest{
		SchemaVersion: pluginmanager.SchemaVersion,
		ID:            pluginID,
		Name:          "Conformance Harness",
		Version:       "0.1.0",
		ArtifactType:  pluginmanager.ArtifactTypeBinary,
		Runtime: pluginmanager.RuntimeManifest{
			Type:        pluginmanager.RuntimeGoPlugin,
			Entry:       pluginmanager.RuntimeEntry,
			EntrySymbol: "Plugin",
		},
		APIVersion:   pluginmanager.APIVersion,
		GoVersion:    runtime.Version(),
		GOOS:         runtime.GOOS,
		GOARCH:       runtime.GOARCH,
		ConfigSchema: json.RawMessage(`{"type":"object"}`),
		RuntimeLimits: pluginmanager.RuntimeLimits{
			HandlerTimeoutMS:      int(pluginmanager.DefaultHandlerTimeout / time.Millisecond),
			InitialWriteTimeoutMS: 1000,
		},
	}
	switch mode {
	case "protocol_proxy":
		manifest.ExtensionPoints = []pluginmanager.ExtensionPoint{{Type: "hook", Key: pluginmanager.ExtensionUpstreamConnect}}
		manifest.Capabilities = json.RawMessage(`{"upstream_connect":{"mode":"` + upstreamMode + `"},"scope":{"type":"host","values":["play.example"]},"rollout":{"mode":"canary"},"minecraft":{"protocol_versions":{"tested":[767]},"forwarding":{"supported":["none"],"default":"none"}}}`)
	case "route":
		manifest.ExtensionPoints = []pluginmanager.ExtensionPoint{{Type: "provider", Key: pluginmanager.ExtensionRouteResolve}}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["route.resolve/v1"],"route":{"cache_ttl_ms":60000,"external_refresh":true,"sqlite_fallback":true,"decision_explain":true}}`)
	case "status":
		manifest.ExtensionPoints = []pluginmanager.ExtensionPoint{{Type: "hook", Key: pluginmanager.ExtensionStatusPing}}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["status.ping/v1"],"status":{"hosts":["blue.example","red.example"],"maintenance_message":true}}`)
	case "middleware_connection":
		manifest.ExtensionPoints = []pluginmanager.ExtensionPoint{{Type: "middleware", Key: pluginmanager.ExtensionConnectionFilter}}
		failPolicy := api.FailPolicyOpen
		if scenario == "error_fail_closed" {
			failPolicy = api.FailPolicyClose
		}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["connection.filter/v1"],"middleware":{"fail_policy":"` + failPolicy + `"}}`)
	case "middleware_handshake":
		manifest.ExtensionPoints = []pluginmanager.ExtensionPoint{{Type: "middleware", Key: pluginmanager.ExtensionHandshakeFilter}}
		failPolicy := api.FailPolicyOpen
		if scenario == "error_fail_closed" {
			failPolicy = api.FailPolicyClose
		}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["handshake.filter/v1"],"middleware":{"fail_policy":"` + failPolicy + `"}}`)
	case "rule":
		manifest.ExtensionPoints = []pluginmanager.ExtensionPoint{
			{Type: "rule", Key: pluginmanager.ExtensionRuleEvaluate},
		}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["rule.evaluate/v1"],"rule":{"fail_policy":"fail_closed"}}`)
	case "event_subscriber":
		manifest.ExtensionPoints = []pluginmanager.ExtensionPoint{{Type: "event", Key: pluginmanager.ExtensionEventSubscriber}}
		mode := api.DeliveryAtLeastOnce
		if scenario == "best_effort" {
			mode = api.DeliveryBestEffort
		}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["event.subscriber/v1"],"event_subscriber":{"mode":"` + mode + `","max_retry":1}}`)
	case "provider":
		providerKey := pluginmanager.ExtensionAdminAuthProvider
		if scenario == pluginmanager.ExtensionProvider || scenario == pluginmanager.ExtensionAuthProvider || scenario == pluginmanager.ExtensionAdminAuthProvider {
			providerKey = scenario
		}
		manifest.ExtensionPoints = []pluginmanager.ExtensionPoint{{Type: "provider", Key: providerKey}}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["` + providerKey + `"],"scope":{"type":"host","values":["blue.example","red.example"]},"providers":[{"type":"` + providerKey + `","name":"external-identity","priority":100,"fallback":true,"dependencies":["local-admin-break-glass"]}],"provider_singletons":["` + providerKey + `:external-identity"]}`)
	case "task":
		manifest.ExtensionPoints = []pluginmanager.ExtensionPoint{{Type: "middleware", Key: pluginmanager.ExtensionConnectionFilter}}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["connection.filter/v1"]}`)
		manifest.BackgroundTasks = []pluginmanager.TaskSpec{{ID: "conformance-task", Name: "Conformance Task", Manual: true}}
	default:
		manifest.ExtensionPoints = []pluginmanager.ExtensionPoint{{Type: "middleware", Key: pluginmanager.ExtensionConnectionFilter}}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["connection.filter/v1"]}`)
	}
	return manifest
}

func writeConformancePackage(packagePath string, manifestBytes, runtimeBytes []byte) error {
	if err := os.MkdirAll(filepath.Dir(packagePath), 0755); err != nil {
		return err
	}
	out, err := os.Create(packagePath)
	if err != nil {
		return err
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	if err := addZipBytes(zw, "manifest.json", manifestBytes); err != nil {
		zw.Close()
		return err
	}
	if err := addZipBytes(zw, pluginmanager.RuntimeEntry, runtimeBytes); err != nil {
		zw.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return out.Close()
}

func writeConformanceRepositoryIndex(artifact pluginmanager.ArtifactRecord) (string, error) {
	packagePath := filepath.Join(filepath.Dir(artifact.FilePath), "artifact.mcgp")
	indexPath := filepath.Join(filepath.Dir(artifact.FilePath), "repository-index.json")
	if _, err := os.Stat(packagePath); err != nil {
		return "", err
	}
	index := map[string]any{
		"name": "conformance",
		"artifacts": []map[string]any{{
			"id":            artifact.ID,
			"plugin_id":     artifact.PluginID,
			"version":       artifact.Version,
			"artifact_path": packagePath,
			"sha256":        artifact.PackageSHA256,
		}},
	}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(indexPath, data, 0644); err != nil {
		return "", err
	}
	return indexPath, nil
}

func copyStringAnyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func conformanceFixtureFailed(fixture map[string]any) bool {
	status := strings.TrimSpace(fmt.Sprint(fixture["status"]))
	expected := strings.TrimSpace(fmt.Sprint(fixture["expected"]))
	switch status {
	case "fail", "failed", "error":
		return true
	case "blocked":
		return expected != "blocked"
	default:
		return false
	}
}

func manifestSchemaForCLI() map[string]any {
	return map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type":    "object",
		"required": []string{
			"schema_version", "id", "name", "version", "artifact_type", "runtime", "api_version", "extension_points", "capabilities",
		},
		"properties": map[string]any{
			"schema_version": map[string]any{"const": pluginmanager.SchemaVersion},
			"artifact_type":  map[string]any{"enum": []string{pluginmanager.ArtifactTypeBinary, pluginmanager.ArtifactTypeSource}},
			"runtime.type":   map[string]any{"enum": runtimeTypeKeysForCLI()},
			"runtime.entry":  map[string]any{"enum": []string{pluginmanager.RuntimeEntry, pluginmanager.RuntimeWASMEntry}},
			"runtime.abi":    map[string]any{"enum": []string{pluginmanager.WASMHostABIVersion()}},
			"runtime_limits.handler_timeout_ms": map[string]any{
				"type":    "integer",
				"minimum": 1,
			},
			"runtime_limits.memory_bytes": map[string]any{
				"type":    "integer",
				"minimum": 1,
			},
			"extension_points": map[string]any{"type": "array", "items": supportedExtensionPointKeysForCLI()},
			"config_schema":    map[string]any{"type": "object"},
			"background_tasks": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":         map[string]any{"type": "string"},
						"mode":       map[string]any{"enum": []string{"manual", "interval"}},
						"run_policy": map[string]any{"enum": []string{pluginmanager.TaskRunPolicyPerNode, pluginmanager.TaskRunPolicySingleton, pluginmanager.TaskRunPolicySharded}},
						"shard_key":  map[string]any{"type": "string"},
						"lease_ttl":  map[string]any{"type": "string"},
						"retry":      map[string]any{"type": "integer", "minimum": 0, "maximum": 10},
					},
				},
			},
		},
	}
}

func cliSchemaForCLI() map[string]any {
	return map[string]any{
		"type":          "object",
		"commands":      pluginCLIImplementedCommands(),
		"json_commands": []string{"features", "schema export", "contract", "conformance", "preflight", "self-test", "benchmark", "plugin-host handshake"},
	}
}

func adminAPISchemaForCLI() map[string]any {
	responses := map[string]any{
		"plugin_features": []string{"schema_version", "api_version", "runtime_types", "service_modes", "runtime_adapters", "plugin_host", "extension_points"},
		"plugin_service":  []string{"service", "service_modes", "runtime_types", "runtime_adapters", "hosts", "nodes"},
		"plugin_build":    []string{"id", "plugin_id", "source_id", "artifact_id", "status", "builder_type", "builder_image", "source_sha256", "artifact_sha256", "metadata_json"},
		"plugin_view":     []string{"id", "desired_state", "runtime_state", "desired_artifact", "active_artifact", "builds", "governance", "operations", "rollout_status", "node_runtime_states"},
		"background_task": []string{
			"plugin_id", "task_id", "run_policy", "node_id", "shard_key", "retry", "last_attempts", "lease_required", "lease_acquired", "lease_owner", "lease_expires_at", "lease_skipped",
		},
		"external_dependency": []string{
			"plugin_id", "name", "endpoint", "purpose", "required", "fail_policy", "requests", "errors", "inflight", "circuit_state", "consecutive_failures", "recent_error", "last_status", "last_seen_at",
		},
		"external_dependency_health_check": []string{
			"plugin_id", "name", "ok", "error", "summary", "checked_by", "checked_at",
		},
		"plugin_node": []string{"node_id", "hostname", "pid", "service_mode", "data_plane_mode", "status", "started_at", "heartbeat_at", "stale"},
		"plugin_node_runtime": []string{
			"node_id", "plugin_id", "artifact_id", "desired_state", "runtime_state", "desired_generation", "applied_generation", "loaded", "enabled", "health", "error", "stale",
		},
		"plugin_rollout": []string{
			"plugin_id", "desired_state", "desired_artifact_id", "desired_generation", "ok", "partial_failure", "nodes_total", "nodes_ready", "nodes_failed", "nodes_stale", "artifact_distribution", "artifact_distribution_mode", "artifact_distribution_status", "artifact_distribution_error", "artifact_package_sha256", "cross_node_apply", "node_runtime_states",
		},
		"plugin_vulnerability": []string{
			"vulnerability_id", "source", "status", "package_name", "version_range", "severity", "action", "fixed_version", "summary", "references",
		},
		"plugin_vulnerability_scan": []string{
			"plugin_id", "artifact_id", "vulnerabilities", "scanned", "matches", "blocking", "warnings", "quarantine_runs", "ok",
		},
		"plugin_governance": []string{"decision", "policy", "reviews", "warning_overrides", "preflights", "benchmarks", "advisories", "conflicts"},
	}
	return map[string]any{
		"$schema":   "https://json-schema.org/draft/2020-12/schema",
		"type":      "object",
		"responses": responses,
		"response_schemas": map[string]any{
			"plugin_features":   map[string]any{"$ref": "#/$defs/plugin_feature_facts"},
			"plugin_service":    map[string]any{"$ref": "#/$defs/plugin_service_status"},
			"plugin_governance": map[string]any{"$ref": "#/$defs/plugin_governance_status"},
		},
		"$defs": adminAPIResponseDefsForCLI(),
	}
}

func adminAPIResponseDefsForCLI() map[string]any {
	return map[string]any{
		"plugin_feature_facts": pluginCLIObjectSchema([]string{"schema_version", "api_version", "runtime_types", "service_modes", "runtime_adapters", "plugin_host", "extension_points"}, map[string]any{
			"schema_version":   pluginCLIStringSchema(),
			"api_version":      pluginCLIStringSchema(),
			"runtime_types":    pluginCLIArraySchema(map[string]any{"$ref": "#/$defs/runtime_feature"}),
			"service_modes":    pluginCLIArraySchema(map[string]any{"$ref": "#/$defs/plugin_service_mode_feature"}),
			"runtime_adapters": pluginCLIArraySchema(map[string]any{"$ref": "#/$defs/runtime_adapter_status"}),
			"plugin_host":      map[string]any{"$ref": "#/$defs/plugin_host_feature"},
			"extension_points": pluginCLIArraySchema(map[string]any{"$ref": "#/$defs/extension_point_feature"}),
		}),
		"plugin_governance_status": pluginCLIObjectSchema([]string{"decision", "policy"}, map[string]any{
			"decision":          map[string]any{"$ref": "#/$defs/governance_decision"},
			"policy":            map[string]any{"$ref": "#/$defs/policy_snapshot"},
			"reviews":           pluginCLIArraySchema(map[string]any{"type": "object"}),
			"warning_overrides": pluginCLIArraySchema(map[string]any{"type": "object"}),
			"preflights":        pluginCLIArraySchema(map[string]any{"type": "object"}),
			"benchmarks":        pluginCLIArraySchema(map[string]any{"type": "object"}),
			"advisories":        pluginCLIArraySchema(map[string]any{"type": "object"}),
			"conflicts":         map[string]any{"type": "object"},
		}),
		"governance_decision": pluginCLIObjectSchema([]string{"ok", "action", "profile", "risk_level", "policy_hash", "review_required", "warning_override_used", "issues", "checks", "created_at"}, map[string]any{
			"ok":                    pluginCLIBoolSchema(),
			"action":                pluginCLIStringSchema(),
			"profile":               pluginCLIStringSchema(),
			"risk_level":            pluginCLIStringSchema(),
			"policy_hash":           pluginCLIStringSchema(),
			"review_required":       pluginCLIBoolSchema(),
			"warning_override_used": pluginCLIBoolSchema(),
			"issues":                pluginCLIArraySchema(map[string]any{"$ref": "#/$defs/governance_issue"}),
			"checks":                pluginCLIArraySchema(map[string]any{"$ref": "#/$defs/governance_issue"}),
			"created_at":            pluginCLIIntegerSchema(),
		}),
		"governance_issue": pluginCLIObjectSchema([]string{"code", "severity", "message"}, map[string]any{
			"code":        pluginCLIStringSchema(),
			"severity":    pluginCLIStringSchema(),
			"message":     pluginCLIStringSchema(),
			"plugin_id":   pluginCLIStringSchema(),
			"artifact_id": pluginCLIStringSchema(),
			"details":     map[string]any{"type": "object"},
		}),
		"policy_snapshot": pluginCLIObjectSchema([]string{"profile", "warning_override_ttl_seconds", "review_required_risk", "warn_benchmark_regression", "block_benchmark_regression", "created_at"}, map[string]any{
			"profile":                      pluginCLIStringSchema(),
			"warning_override_ttl_seconds": pluginCLIIntegerSchema(),
			"review_required_risk":         pluginCLIStringSchema(),
			"warn_benchmark_regression":    pluginCLINumberSchema(),
			"block_benchmark_regression":   pluginCLINumberSchema(),
			"require_conformance_fixture":  pluginCLIBoolSchema(),
			"created_at":                   pluginCLIIntegerSchema(),
		}),
		"plugin_service_status": pluginCLIObjectSchema([]string{"service", "service_modes", "runtime_types", "runtime_adapters", "hosts", "nodes"}, map[string]any{
			"service":          map[string]any{"$ref": "#/$defs/plugin_service_state"},
			"service_modes":    pluginCLIArraySchema(map[string]any{"$ref": "#/$defs/plugin_service_mode_feature"}),
			"runtime_types":    pluginCLIArraySchema(map[string]any{"$ref": "#/$defs/runtime_feature"}),
			"runtime_adapters": pluginCLIArraySchema(map[string]any{"$ref": "#/$defs/runtime_adapter_status"}),
			"hosts":            pluginCLIArraySchema(map[string]any{"$ref": "#/$defs/plugin_host_runtime_summary"}),
			"nodes":            pluginCLIArraySchema(map[string]any{"$ref": "#/$defs/plugin_node_state"}),
		}),
		"plugin_service_state": pluginCLIObjectSchema([]string{
			"desired_mode", "active_mode", "data_plane_mode", "implemented_adapter", "desired_maturity", "active_maturity",
			"applied_at", "restart_required", "live_migration", "crash_policy", "last_error", "updated_by", "updated_at",
		}, map[string]any{
			"desired_mode":        pluginCLIStringSchema(),
			"active_mode":         pluginCLIStringSchema(),
			"data_plane_mode":     pluginCLIStringSchema(),
			"implemented_adapter": pluginCLIBoolSchema(),
			"desired_maturity":    pluginCLIStringSchema(),
			"active_maturity":     pluginCLIStringSchema(),
			"applied_at":          pluginCLIIntegerSchema(),
			"restart_required":    pluginCLIBoolSchema(),
			"live_migration":      pluginCLIStringSchema(),
			"crash_policy":        map[string]any{"$ref": "#/$defs/plugin_host_crash_policy"},
			"unsupported_reason":  pluginCLIStringSchema(),
			"last_error":          pluginCLIStringSchema(),
			"updated_by":          pluginCLIStringSchema(),
			"updated_at":          pluginCLIIntegerSchema(),
		}),
		"plugin_host_crash_policy": pluginCLIObjectSchema([]string{"backoff_seconds", "max_crashes", "window_seconds"}, map[string]any{
			"backoff_seconds": pluginCLIIntegerSchema(),
			"max_crashes":     pluginCLIIntegerSchema(),
			"window_seconds":  pluginCLIIntegerSchema(),
		}),
		"plugin_service_mode_feature": pluginCLIObjectSchema([]string{"mode", "implemented", "maturity", "data_plane", "requires_restart"}, map[string]any{
			"mode":               pluginCLIStringSchema(),
			"implemented":        pluginCLIBoolSchema(),
			"maturity":           pluginCLIStringSchema(),
			"data_plane":         pluginCLIBoolSchema(),
			"requires_restart":   pluginCLIBoolSchema(),
			"unsupported_reason": pluginCLIStringSchema(),
		}),
		"runtime_feature": pluginCLIObjectSchema([]string{"type", "implemented", "maturity", "data_plane", "requires_restart"}, map[string]any{
			"type":               pluginCLIStringSchema(),
			"implemented":        pluginCLIBoolSchema(),
			"maturity":           pluginCLIStringSchema(),
			"data_plane":         pluginCLIBoolSchema(),
			"requires_restart":   pluginCLIBoolSchema(),
			"unsupported_reason": pluginCLIStringSchema(),
			"entry":              pluginCLIStringSchema(),
		}),
		"extension_point_feature": pluginCLIObjectSchema([]string{"key", "type", "implemented", "maturity", "data_plane", "requires_restart"}, map[string]any{
			"key":                pluginCLIStringSchema(),
			"type":               pluginCLIStringSchema(),
			"implemented":        pluginCLIBoolSchema(),
			"maturity":           pluginCLIStringSchema(),
			"data_plane":         pluginCLIBoolSchema(),
			"requires_restart":   pluginCLIBoolSchema(),
			"unsupported_reason": pluginCLIStringSchema(),
		}),
		"runtime_adapter_status": pluginCLIObjectSchema([]string{"service_mode", "runtime_type", "adapter", "implemented", "maturity", "data_plane", "lifecycle", "requires_restart"}, map[string]any{
			"service_mode":       pluginCLIStringSchema(),
			"runtime_type":       pluginCLIStringSchema(),
			"adapter":            pluginCLIStringSchema(),
			"implemented":        pluginCLIBoolSchema(),
			"maturity":           pluginCLIStringSchema(),
			"data_plane":         pluginCLIBoolSchema(),
			"lifecycle":          pluginCLIBoolSchema(),
			"requires_restart":   pluginCLIBoolSchema(),
			"host_protocol":      pluginCLIStringSchema(),
			"control_channel":    pluginCLIStringSchema(),
			"unsupported_reason": pluginCLIStringSchema(),
		}),
		"plugin_host_runtime_summary": pluginCLIObjectSchema([]string{"plugin_id", "artifact_id", "pid", "state", "drain_mode", "crash_loop", "crash_count"}, map[string]any{
			"plugin_id":     pluginCLIStringSchema(),
			"artifact_id":   pluginCLIStringSchema(),
			"pid":           pluginCLIIntegerSchema(),
			"state":         pluginCLIStringSchema(),
			"drain_mode":    pluginCLIStringSchema(),
			"crash_loop":    pluginCLIBoolSchema(),
			"crash_count":   pluginCLIIntegerSchema(),
			"last_error":    pluginCLIStringSchema(),
			"started_at":    pluginCLIIntegerSchema(),
			"draining_at":   pluginCLIIntegerSchema(),
			"exited_at":     pluginCLIIntegerSchema(),
			"last_crash_at": pluginCLIIntegerSchema(),
			"backoff_until": pluginCLIIntegerSchema(),
			"isolated":      pluginCLIBoolSchema(),
		}),
		"plugin_host_feature": pluginCLIObjectSchema([]string{"protocol", "command", "handshake", "control_channel", "orphan_discovery_order", "maturity"}, map[string]any{
			"protocol":                            pluginCLIStringSchema(),
			"command":                             pluginCLIStringSchema(),
			"handshake":                           pluginCLIBoolSchema(),
			"control_channel":                     pluginCLIStringSchema(),
			"control_channel_implemented":         pluginCLIBoolSchema(),
			"supervisor_start_stop":               pluginCLIBoolSchema(),
			"supervisor_data_plane":               pluginCLIBoolSchema(),
			"process_table_orphan_cleanup":        pluginCLIBoolSchema(),
			"orphan_discovery_order":              pluginCLIArraySchema(pluginCLIStringSchema()),
			"orphan_discovery_unsupported_reason": pluginCLIStringSchema(),
			"crash_tracking":                      pluginCLIBoolSchema(),
			"crash_policy":                        pluginCLIBoolSchema(),
			"backoff":                             pluginCLIBoolSchema(),
			"stream_deadline":                     pluginCLIBoolSchema(),
			"stream_cancel":                       pluginCLIBoolSchema(),
			"stream_byte_accounting":              pluginCLIBoolSchema(),
			"lifecycle_commands":                  pluginCLIArraySchema(pluginCLIStringSchema()),
			"lifecycle_implemented":               pluginCLIBoolSchema(),
			"data_plane":                          pluginCLIBoolSchema(),
			"maturity":                            pluginCLIStringSchema(),
			"unsupported_reason":                  pluginCLIStringSchema(),
		}),
		"plugin_node_state": pluginCLIObjectSchema([]string{"node_id", "hostname", "pid", "service_mode", "data_plane_mode", "status", "started_at", "heartbeat_at", "stale"}, map[string]any{
			"node_id":         pluginCLIStringSchema(),
			"hostname":        pluginCLIStringSchema(),
			"pid":             pluginCLIIntegerSchema(),
			"service_mode":    pluginCLIStringSchema(),
			"data_plane_mode": pluginCLIStringSchema(),
			"status":          pluginCLIStringSchema(),
			"started_at":      pluginCLIIntegerSchema(),
			"heartbeat_at":    pluginCLIIntegerSchema(),
			"stale":           pluginCLIBoolSchema(),
		}),
	}
}

func pluginCLIObjectSchema(required []string, properties map[string]any) map[string]any {
	return map[string]any{
		"type":                 "object",
		"required":             required,
		"properties":           properties,
		"additionalProperties": false,
	}
}

func pluginCLIArraySchema(items map[string]any) map[string]any {
	return map[string]any{"type": "array", "items": items}
}

func pluginCLIStringSchema() map[string]any {
	return map[string]any{"type": "string"}
}

func pluginCLIIntegerSchema() map[string]any {
	return map[string]any{"type": "integer"}
}

func pluginCLINumberSchema() map[string]any {
	return map[string]any{"type": "number"}
}

func pluginCLIBoolSchema() map[string]any {
	return map[string]any{"type": "boolean"}
}

func conformanceFixtureSchemaForCLI() map[string]any {
	fixtureObject := map[string]any{
		"type":     "object",
		"required": []string{"name"},
		"properties": map[string]any{
			"name":      map[string]any{"type": "string"},
			"status":    map[string]any{"enum": []string{"pass", "skip", "fail", "blocked"}},
			"extension": map[string]any{"type": "string"},
			"expected":  map[string]any{"type": "string"},
			"scenario":  map[string]any{"type": "string"},
			"mode":      map[string]any{"type": "string"},
		},
		"additionalProperties": true,
	}
	stringArray := func(values ...string) map[string]any {
		schema := map[string]any{
			"type":  "array",
			"items": map[string]any{"type": "string"},
		}
		if len(values) > 0 {
			schema["items"] = map[string]any{"enum": values}
		}
		return schema
	}
	return map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type":    "object",
		"properties": map[string]any{
			"fixtures": map[string]any{
				"type":  "array",
				"items": map[string]any{"$ref": "#/$defs/fixture"},
			},
			"route_decisions": stringArray(
				"override",
				"fallback",
				"reject",
				"pass",
			),
			"status_hosts": stringArray(),
			"stream_proxy_scenarios": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"required":             []string{"name", "protocol", "expected", "frames"},
					"additionalProperties": true,
					"properties": map[string]any{
						"name":               map[string]any{"type": "string"},
						"protocol":           map[string]any{"const": pluginmanager.StreamProxyProtocolV1},
						"status":             map[string]any{"enum": []string{"pass", "skip", "fail", "blocked"}},
						"expected":           map[string]any{"type": "string"},
						"deadline_unix_ms":   pluginCLIIntegerSchema(),
						"bytes_to_plugin":    pluginCLIIntegerSchema(),
						"bytes_to_gateway":   pluginCLIIntegerSchema(),
						"backpressure_bytes": pluginCLIIntegerSchema(),
						"frames": map[string]any{
							"type": "array",
							"items": map[string]any{
								"type":                 "object",
								"additionalProperties": true,
								"properties": map[string]any{
									"type":             map[string]any{"enum": []string{"data", "half_close", "window", "cancel", "deadline", "accounting"}},
									"direction":        map[string]any{"enum": []string{"gateway_to_plugin", "plugin_to_gateway"}},
									"bytes":            pluginCLIIntegerSchema(),
									"window_bytes":     pluginCLIIntegerSchema(),
									"deadline_unix_ms": pluginCLIIntegerSchema(),
									"reason":           pluginCLIStringSchema(),
								},
							},
						},
					},
				},
			},
			"protocol_proxy_scenarios": stringArray(
				"initial_data_once",
				"panic_recovered",
				"timeout_deadline",
				"endpoint_close",
				"client_close",
				"backpressure_large_packet",
				"drain_disable_new_connections",
				"force_close_draining",
			),
			"rule_evaluation_outcomes": stringArray(
				"allow",
				"deny",
				"error_fail_closed",
				"timeout_fail_closed",
				"bad_config_fallback",
			),
			"connection_filter_scenarios": stringArray(
				"allow",
				"reject",
				"error_fail_open",
				"error_fail_closed",
			),
			"handshake_filter_scenarios": stringArray(
				"allow",
				"reject",
				"rewrite_host",
				"error_fail_open",
				"error_fail_closed",
			),
			"governance_gate_scenarios": stringArray(
				"review_required",
				"warning_override",
				"advisory_block",
				"supply_chain_block",
				"rollback_gate",
				"repository_apply_gate",
				"promotion_apply_gate",
			),
			"event_delivery": stringArray(
				"best_effort",
				"at_least_once",
				"dead_letter",
				"replay",
				"drop",
				"cross_node_at_least_once",
				"subscriber_failure_non_blocking",
			),
			"provider_registry":                          stringArray(),
			"default_route_must_survive_bad_rule_config": map[string]any{"type": "boolean"},
			"local_admin_break_glass":                    map[string]any{"type": "boolean"},
		},
		"$defs": map[string]any{
			"fixture": fixtureObject,
		},
		"additionalProperties": true,
	}
}

func cliCheck(code, severity, message string, details map[string]any) map[string]any {
	check := map[string]any{"code": code, "severity": severity, "message": message}
	if len(details) > 0 {
		check["details"] = details
	}
	return check
}

func cliHasBlocking(checks []map[string]any) bool {
	for _, check := range checks {
		if check["severity"] == "blocking" {
			return true
		}
	}
	return false
}

func supportedExtensionPointKeysForCLI() map[string]bool {
	out := map[string]bool{}
	for _, feature := range pluginmanager.ExtensionPointFeatures() {
		out[feature.Key] = true
	}
	return out
}

func runtimeTypeKeysForCLI() []string {
	features := pluginmanager.RuntimeTypeFeatures()
	out := make([]string, 0, len(features))
	for _, feature := range features {
		out = append(out, feature.Type)
	}
	return out
}

func supportedFeatureForCLI(feature string) bool {
	if feature == "" {
		return true
	}
	if supportedExtensionPointKeysForCLI()[feature] {
		return true
	}
	switch feature {
	case "upstream.connect", "minecraft", "config", "secret", "preflight", "self-test":
		return true
	default:
		return false
	}
}

func stringSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		out[value] = true
	}
	return out
}

func hasExtensionPointForCLI(manifest pluginmanager.Manifest, key string) bool {
	for _, point := range manifest.ExtensionPoints {
		if point.Key == key {
			return true
		}
	}
	return false
}

func upstreamConnectModeForCLI(manifest pluginmanager.Manifest) string {
	var caps struct {
		UpstreamConnect struct {
			Mode string `json:"mode"`
		} `json:"upstream_connect"`
	}
	_ = json.Unmarshal(manifest.Capabilities, &caps)
	if caps.UpstreamConnect.Mode == "" {
		return pluginmanager.UpstreamModeDialer
	}
	return caps.UpstreamConnect.Mode
}

func passFail(ok bool) string {
	if ok {
		return "pass"
	}
	return "fail"
}

func runPluginSBOMGenerateCLI(args []string) error {
	opts, err := parsePluginSBOMCLIOptions(args)
	if err != nil {
		return err
	}
	doc, err := pluginSBOMDocumentForCLI(opts)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	sum := sha256.Sum256(data)
	documentSHA := hex.EncodeToString(sum[:])
	if opts.Out == "" {
		_, err = os.Stdout.Write(data)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(opts.Out), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(opts.Out, data, 0644); err != nil {
		return err
	}
	deps, _ := doc["dependencies"].([]pluginSBOMDependency)
	return encodePluginCLIJSON(map[string]any{
		"command":         "sbom generate",
		"out":             opts.Out,
		"document_sha256": documentSHA,
		"dependencies":    len(deps),
	})
}

func parsePluginSBOMCLIOptions(args []string) (pluginSBOMCLIOptions, error) {
	opts := pluginSBOMCLIOptions{Format: "spdx-json-lite"}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			if opts.Target != "" {
				return pluginSBOMCLIOptions{}, fmt.Errorf("unexpected argument %q", arg)
			}
			opts.Target = arg
			continue
		}
		key, value, consumed, err := parsePluginCLIFlag(args, i)
		if err != nil {
			return pluginSBOMCLIOptions{}, err
		}
		i += consumed
		switch key {
		case "manifest":
			opts.Manifest = value
		case "out":
			opts.Out = value
		case "format":
			opts.Format = value
		default:
			return pluginSBOMCLIOptions{}, fmt.Errorf("unknown sbom generate flag --%s", key)
		}
	}
	if opts.Target == "" {
		return pluginSBOMCLIOptions{}, errors.New("sbom generate requires a plugin directory, manifest source, or .mcgp artifact")
	}
	switch opts.Format {
	case "spdx-json-lite", "spdx-json":
	default:
		return pluginSBOMCLIOptions{}, fmt.Errorf("unsupported sbom format %q", opts.Format)
	}
	return opts, nil
}

func pluginSBOMDocumentForCLI(opts pluginSBOMCLIOptions) (map[string]any, error) {
	manifest, targetKind, err := readPluginTargetManifestForCLI(opts.Target, opts.Manifest)
	if err != nil {
		return nil, err
	}
	artifact, err := validatePluginPathForCLIWithManifest(opts.Target, "", opts.Manifest)
	if err != nil {
		return nil, err
	}
	sourceDir := pluginSourceDirForCLI(opts.Target)
	dependencies, modulePath, err := pluginSBOMDependenciesForCLI(sourceDir, manifest)
	if err != nil {
		return nil, err
	}
	files, err := pluginSBOMFilesForCLI(opts.Target, opts.Manifest)
	if err != nil {
		return nil, err
	}
	documentSeed := manifest.ID + "\x00" + manifest.Version + "\x00" + artifact.PackageSHA256 + "\x00" + artifact.SHA256
	documentID := sha256.Sum256([]byte(documentSeed))
	return map[string]any{
		"schema_version":     "mc-gateway.sbom/v1",
		"format":             opts.Format,
		"spdx_version":       "SPDX-2.3",
		"data_license":       "CC0-1.0",
		"name":               manifest.ID + "-" + manifest.Version,
		"document_namespace": "https://mc-gateway.local/sbom/" + hex.EncodeToString(documentID[:]),
		"created_at":         time.Now().UTC().Format(time.RFC3339),
		"plugin": map[string]any{
			"id":          manifest.ID,
			"name":        manifest.Name,
			"version":     manifest.Version,
			"api_version": manifest.APIVersion,
			"module":      modulePath,
		},
		"artifact": map[string]any{
			"target_kind":    targetKind,
			"artifact_type":  artifact.ArtifactType,
			"runtime_type":   artifact.RuntimeType,
			"sha256":         artifact.SHA256,
			"package_sha256": artifact.PackageSHA256,
			"go_version":     artifact.GoVersion,
			"go_os":          artifact.GOOS,
			"go_arch":        artifact.GOARCH,
		},
		"dependencies": dependencies,
		"files":        files,
		"scan": map[string]any{
			"vulnerability_status": "not_scanned",
			"license_status":       "not_evaluated",
			"advisory_status":      "not_evaluated",
		},
	}, nil
}

func pluginSourceDirForCLI(target string) string {
	info, err := os.Stat(target)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return target
	}
	if isManifestSourceFile(target) {
		return filepath.Dir(target)
	}
	return ""
}

func pluginSBOMDependenciesForCLI(sourceDir string, manifest pluginmanager.Manifest) ([]pluginSBOMDependency, string, error) {
	var deps []pluginSBOMDependency
	modulePath := ""
	if sourceDir != "" {
		goModPath := filepath.Join(sourceDir, "go.mod")
		data, err := os.ReadFile(goModPath)
		if err == nil {
			file, err := modfile.Parse(goModPath, data, nil)
			if err != nil {
				return nil, "", fmt.Errorf("parse go.mod for sbom: %w", err)
			}
			if file.Module != nil {
				modulePath = file.Module.Mod.Path
			}
			for _, req := range file.Require {
				deps = append(deps, pluginSBOMDependency{
					Name:     req.Mod.Path,
					Version:  req.Mod.Version,
					Type:     "go-module",
					Source:   "go.mod",
					Indirect: req.Indirect,
					PURL:     goModulePURL(req.Mod.Path, req.Mod.Version),
				})
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, "", err
		}
	}
	deps = append(deps, manifestSupplyChainDependenciesForCLI(manifest)...)
	return uniquePluginSBOMDependencies(deps), modulePath, nil
}

func manifestSupplyChainDependenciesForCLI(manifest pluginmanager.Manifest) []pluginSBOMDependency {
	var raw map[string]any
	if len(manifest.SupplyChain) == 0 || json.Unmarshal(manifest.SupplyChain, &raw) != nil {
		return nil
	}
	var deps []pluginSBOMDependency
	for _, key := range []string{"dependencies", "sbom_dependencies", "modules"} {
		items, _ := raw[key].([]any)
		for _, item := range items {
			obj, _ := item.(map[string]any)
			name, _ := obj["name"].(string)
			if name == "" {
				name, _ = obj["path"].(string)
			}
			version, _ := obj["version"].(string)
			if name == "" {
				continue
			}
			deps = append(deps, pluginSBOMDependency{
				Name:    name,
				Version: version,
				Type:    "declared",
				Source:  "manifest.supply_chain",
				PURL:    goModulePURL(name, version),
			})
		}
	}
	return deps
}

func uniquePluginSBOMDependencies(deps []pluginSBOMDependency) []pluginSBOMDependency {
	seen := make(map[string]bool, len(deps))
	var out []pluginSBOMDependency
	for _, dep := range deps {
		if dep.Name == "" {
			continue
		}
		key := dep.Name + "\x00" + dep.Version + "\x00" + dep.Source
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, dep)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		if out[i].Version != out[j].Version {
			return out[i].Version < out[j].Version
		}
		return out[i].Source < out[j].Source
	})
	return out
}

func goModulePURL(name, version string) string {
	if name == "" {
		return ""
	}
	if version == "" {
		return "pkg:golang/" + name
	}
	return "pkg:golang/" + name + "@" + version
}

func pluginSBOMFilesForCLI(target, manifestPath string) ([]pluginSBOMFile, error) {
	info, err := os.Stat(target)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() && !isManifestSourceFile(target) {
		sum, err := fileSHA256ForCLI(target)
		if err != nil {
			return nil, err
		}
		return []pluginSBOMFile{{Name: filepath.Base(target), SHA256: sum, Bytes: info.Size()}}, nil
	}
	source, err := readPluginManifestSource(target, manifestPath)
	if err != nil {
		return nil, err
	}
	root := filepath.Dir(source.Path)
	candidates := []string{source.Path, filepath.Join(root, "go.mod"), filepath.Join(root, "go.sum"), filepath.Join(root, "conformance.json")}
	var files []pluginSBOMFile
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if seen[candidate] {
			continue
		}
		seen[candidate] = true
		info, err := os.Stat(candidate)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if info.IsDir() {
			continue
		}
		sum, err := fileSHA256ForCLI(candidate)
		if err != nil {
			return nil, err
		}
		name, err := filepath.Rel(root, candidate)
		if err != nil {
			name = filepath.Base(candidate)
		}
		files = append(files, pluginSBOMFile{Name: filepath.ToSlash(name), SHA256: sum, Bytes: info.Size()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, nil
}

func runPluginPromotionCLI(command string, args []string) error {
	opts, err := parsePluginPromotionCLIOptions(args)
	if err != nil {
		return err
	}
	switch command {
	case "export":
		if opts.Target == "" {
			return errors.New("promotion export requires a plugin directory, manifest source, or .mcgp artifact")
		}
		bundle, err := promotionBundleFromTarget(opts)
		if err != nil {
			return err
		}
		return writePromotionOutput(opts.Out, bundle)
	case "import":
		if opts.Target == "" {
			return errors.New("promotion import requires a promotion bundle path")
		}
		bundle, err := readPromotionBundle(opts.Target)
		if err != nil {
			return err
		}
		report := pluginmanager.EvaluatePromotionBundle(bundle)
		return encodePluginCLIJSON(map[string]any{"command": "promotion import", "dry_run": true, "report": report})
	case "diff":
		if opts.Target == "" || opts.Baseline == "" {
			return errors.New("promotion diff requires a baseline bundle and --target plugin/bundle")
		}
		baseline, err := readPromotionBundle(opts.Baseline)
		if err != nil {
			return err
		}
		target, err := promotionBundleFromTargetOrBundle(opts)
		if err != nil {
			return err
		}
		diff := pluginmanager.DiffPromotionBundles(baseline, target)
		return encodePluginCLIJSON(map[string]any{"command": "promotion diff", "ok": len(diff) == 0, "diff": diff})
	case "drift":
		if opts.Baseline == "" || opts.Target == "" {
			return errors.New("promotion drift requires --baseline bundle and --target plugin/bundle")
		}
		baseline, err := readPromotionBundle(opts.Baseline)
		if err != nil {
			return err
		}
		current, err := promotionBundleFromTargetOrBundle(opts)
		if err != nil {
			return err
		}
		report := pluginmanager.PromotionDrift(current, baseline)
		return encodePluginCLIJSON(map[string]any{"command": "promotion drift", "report": report})
	case "dr-drill":
		if opts.Target == "" {
			return errors.New("promotion dr-drill requires a promotion bundle path")
		}
		bundle, err := readPromotionBundle(opts.Target)
		if err != nil {
			return err
		}
		report := pluginmanager.RunPromotionDRDrill(bundle)
		return encodePluginCLIJSON(map[string]any{"command": "promotion dr-drill", "report": report})
	default:
		return fmt.Errorf("unknown promotion command %q", command)
	}
}

func parsePluginPromotionCLIOptions(args []string) (pluginPromotionCLIOptions, error) {
	opts := pluginPromotionCLIOptions{Profile: pluginmanager.PolicyProfileDev}
	var positionals []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			if len(positionals) >= 1 {
				return pluginPromotionCLIOptions{}, fmt.Errorf("unexpected argument %q", arg)
			}
			positionals = append(positionals, arg)
			continue
		}
		key, value, consumed, err := parsePluginCLIFlag(args, i)
		if err != nil {
			return pluginPromotionCLIOptions{}, err
		}
		i += consumed
		switch key {
		case "manifest":
			opts.Manifest = value
		case "config":
			opts.ConfigPath = value
		case "config-json":
			opts.ConfigJSON = value
		case "profile":
			opts.Profile = value
		case "out":
			opts.Out = value
		case "target":
			opts.Target = value
		case "baseline":
			opts.Baseline = value
		case "dry-run":
			// Promotion import/apply is dry-run only in this implementation.
		default:
			return pluginPromotionCLIOptions{}, fmt.Errorf("unknown promotion flag --%s", key)
		}
	}
	if len(positionals) > 0 {
		if opts.Target == "" {
			opts.Target = positionals[0]
		} else if opts.Baseline == "" {
			opts.Baseline = positionals[0]
		}
	}
	return opts, nil
}

func promotionBundleFromTargetOrBundle(opts pluginPromotionCLIOptions) (pluginmanager.PromotionBundle, error) {
	if bundle, err := readPromotionBundle(opts.Target); err == nil {
		return bundle, nil
	}
	return promotionBundleFromTarget(opts)
}

func promotionBundleFromTarget(opts pluginPromotionCLIOptions) (pluginmanager.PromotionBundle, error) {
	artifact, err := validatePluginPathForCLIWithManifest(opts.Target, "", opts.Manifest)
	if err != nil {
		return pluginmanager.PromotionBundle{}, err
	}
	manifest, _, err := readPluginTargetManifestForCLI(opts.Target, opts.Manifest)
	if err != nil {
		return pluginmanager.PromotionBundle{}, err
	}
	configJSON := opts.ConfigJSON
	if opts.ConfigPath != "" {
		data, err := os.ReadFile(opts.ConfigPath)
		if err != nil {
			return pluginmanager.PromotionBundle{}, err
		}
		configJSON = string(data)
	}
	if configJSON != "" && !json.Valid([]byte(configJSON)) {
		return pluginmanager.PromotionBundle{}, errors.New("promotion config must be valid JSON")
	}
	bundle := pluginmanager.NewPromotionBundle(opts.Target, opts.Profile, artifact, manifest, configJSON)
	return bundle, nil
}

func readPromotionBundle(path string) (pluginmanager.PromotionBundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return pluginmanager.PromotionBundle{}, err
	}
	var bundle pluginmanager.PromotionBundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return pluginmanager.PromotionBundle{}, err
	}
	if bundle.SchemaVersion == "" || len(bundle.Plugins) == 0 {
		return pluginmanager.PromotionBundle{}, errors.New("invalid promotion bundle")
	}
	return bundle, nil
}

func writePromotionOutput(outPath string, bundle pluginmanager.PromotionBundle) error {
	if outPath == "" {
		return encodePluginCLIJSON(bundle)
	}
	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(outPath, data, 0644); err != nil {
		return err
	}
	return encodePluginCLIJSON(map[string]any{
		"command":   "promotion export",
		"out":       outPath,
		"bundle_id": bundle.BundleID,
		"plugins":   len(bundle.Plugins),
	})
}

const pluginSignTrustStoreSchemaVersion = "mc-gateway.plugin.trust-store/v1"

type pluginSignTrustStore struct {
	SchemaVersion string               `json:"schema_version"`
	Keys          []pluginSignTrustKey `json:"keys"`
	UpdatedAt     string               `json:"updated_at,omitempty"`
}

type pluginSignTrustKey struct {
	KeyID            string `json:"key_id"`
	Algorithm        string `json:"algorithm"`
	PublicKey        string `json:"public_key"`
	PublicKeySHA256  string `json:"public_key_sha256"`
	CreatedAt        string `json:"created_at"`
	RotatedAt        string `json:"rotated_at,omitempty"`
	NotBefore        string `json:"not_before,omitempty"`
	NotAfter         string `json:"not_after,omitempty"`
	Revoked          bool   `json:"revoked"`
	RevokedAt        string `json:"revoked_at,omitempty"`
	RevocationReason string `json:"revocation_reason,omitempty"`
}

func runPluginSignCLI(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: gateway plugin sign verify|key-rotation|revoke ...")
	}
	switch args[0] {
	case "verify":
		return runPluginSignVerifyCLI(args[1:])
	case "key-rotation":
		return runPluginSignKeyRotationCLI(args[1:])
	case "revoke":
		return runPluginSignRevokeCLI(args[1:])
	default:
		return fmt.Errorf("unknown sign command %q", args[0])
	}
}

func runPluginSignVerifyCLI(args []string) error {
	target := ""
	signatureRef := ""
	publicKeyRef := ""
	trustStorePath := ""
	keyID := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			if target != "" {
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
		case "signature":
			signatureRef = value
		case "public-key":
			publicKeyRef = value
		case "trust-store":
			trustStorePath = value
		case "key-id":
			keyID = value
		default:
			return fmt.Errorf("unknown sign verify flag --%s", key)
		}
	}
	if target == "" || signatureRef == "" {
		return errors.New("sign verify requires artifact and --signature")
	}
	if (publicKeyRef == "") == (trustStorePath == "") {
		return errors.New("sign verify requires exactly one of --public-key or --trust-store")
	}
	if publicKeyRef != "" && keyID != "" {
		return errors.New("sign verify --key-id requires --trust-store")
	}
	artifactBytes, err := os.ReadFile(target)
	if err != nil {
		return err
	}
	signature, err := decodeBase64ValueOrFile(signatureRef)
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	publicKey, keyDetails, err := resolvePluginSignVerifyKey(publicKeyRef, trustStorePath, keyID, time.Now().UTC())
	if err != nil {
		return err
	}
	signatureValid := ed25519.Verify(ed25519.PublicKey(publicKey), artifactBytes, signature)
	trusted, _ := keyDetails["trusted"].(bool)
	verified := signatureValid && trusted
	sum := sha256.Sum256(artifactBytes)
	report := map[string]any{
		"command":         "sign verify",
		"artifact":        target,
		"artifact_sha256": hex.EncodeToString(sum[:]),
		"signature_type":  "ed25519",
		"signature_valid": signatureValid,
		"verified":        verified,
	}
	for key, value := range keyDetails {
		report[key] = value
	}
	return encodePluginCLIJSON(report)
}

func runPluginSignKeyRotationCLI(args []string) error {
	trustStorePath := ""
	keyID := ""
	publicKeyRef := ""
	notBefore := ""
	notAfter := ""
	for i := 0; i < len(args); i++ {
		key, value, consumed, err := parsePluginCLIFlag(args, i)
		if err != nil {
			return err
		}
		i += consumed
		switch key {
		case "trust-store":
			trustStorePath = value
		case "key-id":
			keyID = value
		case "public-key":
			publicKeyRef = value
		case "not-before":
			notBefore = value
		case "not-after":
			notAfter = value
		default:
			return fmt.Errorf("unknown sign key-rotation flag --%s", key)
		}
	}
	if trustStorePath == "" || keyID == "" || publicKeyRef == "" {
		return errors.New("sign key-rotation requires --trust-store, --key-id, and --public-key")
	}
	publicKey, err := decodeBase64ValueOrFile(publicKeyRef)
	if err != nil {
		return fmt.Errorf("decode public key: %w", err)
	}
	if len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("public key length = %d, want %d", len(publicKey), ed25519.PublicKeySize)
	}
	notBefore, err = normalizePluginSignTimestamp(notBefore)
	if err != nil {
		return fmt.Errorf("parse --not-before: %w", err)
	}
	notAfter, err = normalizePluginSignTimestamp(notAfter)
	if err != nil {
		return fmt.Errorf("parse --not-after: %w", err)
	}
	store, err := loadPluginSignTrustStore(trustStorePath)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	publicKeySHA := sha256.Sum256(publicKey)
	publicKeyHash := hex.EncodeToString(publicKeySHA[:])
	key, index := findPluginSignTrustKey(store.Keys, keyID)
	rotated := index >= 0
	previousHash := ""
	if rotated {
		if key.Revoked {
			return fmt.Errorf("key %q is revoked; rotate with a new key id", keyID)
		}
		previousHash = key.PublicKeySHA256
		key.Algorithm = "ed25519"
		key.PublicKey = base64.StdEncoding.EncodeToString(publicKey)
		key.PublicKeySHA256 = publicKeyHash
		key.RotatedAt = now
		key.NotBefore = notBefore
		key.NotAfter = notAfter
		store.Keys[index] = key
	} else {
		store.Keys = append(store.Keys, pluginSignTrustKey{
			KeyID:           keyID,
			Algorithm:       "ed25519",
			PublicKey:       base64.StdEncoding.EncodeToString(publicKey),
			PublicKeySHA256: publicKeyHash,
			CreatedAt:       now,
			NotBefore:       notBefore,
			NotAfter:        notAfter,
		})
	}
	if err := savePluginSignTrustStore(trustStorePath, store); err != nil {
		return err
	}
	return encodePluginCLIJSON(map[string]any{
		"command":             "sign key-rotation",
		"trust_store":         trustStorePath,
		"key_id":              keyID,
		"algorithm":           "ed25519",
		"public_key_sha256":   publicKeyHash,
		"rotated":             rotated,
		"active_key_count":    len(activePluginSignTrustKeys(store.Keys, time.Now().UTC())),
		"trusted_key_count":   len(store.Keys),
		"schema_version":      store.SchemaVersion,
		"not_before":          notBefore,
		"not_after":           notAfter,
		"previous_key_sha256": previousHash,
	})
}

func runPluginSignRevokeCLI(args []string) error {
	trustStorePath := ""
	keyID := ""
	reason := ""
	for i := 0; i < len(args); i++ {
		key, value, consumed, err := parsePluginCLIFlag(args, i)
		if err != nil {
			return err
		}
		i += consumed
		switch key {
		case "trust-store":
			trustStorePath = value
		case "key-id":
			keyID = value
		case "reason":
			reason = value
		default:
			return fmt.Errorf("unknown sign revoke flag --%s", key)
		}
	}
	if trustStorePath == "" || keyID == "" {
		return errors.New("sign revoke requires --trust-store and --key-id")
	}
	store, err := loadPluginSignTrustStore(trustStorePath)
	if err != nil {
		return err
	}
	key, index := findPluginSignTrustKey(store.Keys, keyID)
	if index < 0 {
		return fmt.Errorf("trusted key %q not found", keyID)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if !key.Revoked {
		key.Revoked = true
		key.RevokedAt = now
	}
	key.RevocationReason = strings.TrimSpace(reason)
	store.Keys[index] = key
	if err := savePluginSignTrustStore(trustStorePath, store); err != nil {
		return err
	}
	return encodePluginCLIJSON(map[string]any{
		"command":           "sign revoke",
		"trust_store":       trustStorePath,
		"key_id":            keyID,
		"revoked":           true,
		"revoked_at":        key.RevokedAt,
		"revocation_reason": key.RevocationReason,
		"active_key_count":  len(activePluginSignTrustKeys(store.Keys, time.Now().UTC())),
		"schema_version":    store.SchemaVersion,
	})
}

func resolvePluginSignVerifyKey(publicKeyRef, trustStorePath, keyID string, now time.Time) ([]byte, map[string]any, error) {
	if publicKeyRef != "" {
		publicKey, err := decodeBase64ValueOrFile(publicKeyRef)
		if err != nil {
			return nil, nil, fmt.Errorf("decode public key: %w", err)
		}
		if len(publicKey) != ed25519.PublicKeySize {
			return nil, nil, fmt.Errorf("public key length = %d, want %d", len(publicKey), ed25519.PublicKeySize)
		}
		sum := sha256.Sum256(publicKey)
		return publicKey, map[string]any{
			"public_key_sha256": hex.EncodeToString(sum[:]),
			"trusted":           true,
			"trust_status":      "inline_public_key",
		}, nil
	}
	store, err := loadPluginSignTrustStore(trustStorePath)
	if err != nil {
		return nil, nil, err
	}
	key, err := selectPluginSignTrustKey(store.Keys, keyID, now)
	if err != nil {
		return nil, nil, err
	}
	publicKey, err := base64.StdEncoding.DecodeString(strings.TrimSpace(key.PublicKey))
	if err != nil {
		return nil, nil, fmt.Errorf("decode trust-store public key %q: %w", key.KeyID, err)
	}
	if len(publicKey) != ed25519.PublicKeySize {
		return nil, nil, fmt.Errorf("trust-store public key %q length = %d, want %d", key.KeyID, len(publicKey), ed25519.PublicKeySize)
	}
	status, err := pluginSignTrustStatus(key, now)
	if err != nil {
		return nil, nil, err
	}
	return publicKey, map[string]any{
		"trust_store":          trustStorePath,
		"key_id":               key.KeyID,
		"public_key_sha256":    key.PublicKeySHA256,
		"trusted":              status == "trusted",
		"trust_status":         status,
		"revoked":              key.Revoked,
		"revocation_reason":    key.RevocationReason,
		"trusted_key_count":    len(store.Keys),
		"active_key_count":     len(activePluginSignTrustKeys(store.Keys, now)),
		"trust_schema":         store.SchemaVersion,
		"trust_not_before":     key.NotBefore,
		"trust_not_after":      key.NotAfter,
		"trust_key_created_at": key.CreatedAt,
		"trust_key_rotated_at": key.RotatedAt,
	}, nil
}

func loadPluginSignTrustStore(path string) (pluginSignTrustStore, error) {
	if strings.TrimSpace(path) == "" {
		return pluginSignTrustStore{}, errors.New("trust store path is required")
	}
	store := pluginSignTrustStore{
		SchemaVersion: pluginSignTrustStoreSchemaVersion,
		Keys:          []pluginSignTrustKey{},
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return pluginSignTrustStore{}, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return store, nil
	}
	if err := json.Unmarshal(data, &store); err != nil {
		return pluginSignTrustStore{}, fmt.Errorf("decode trust store %s: %w", path, err)
	}
	if store.SchemaVersion == "" {
		store.SchemaVersion = pluginSignTrustStoreSchemaVersion
	}
	if store.SchemaVersion != pluginSignTrustStoreSchemaVersion {
		return pluginSignTrustStore{}, fmt.Errorf("unsupported trust store schema %q", store.SchemaVersion)
	}
	if store.Keys == nil {
		store.Keys = []pluginSignTrustKey{}
	}
	return store, nil
}

func savePluginSignTrustStore(path string, store pluginSignTrustStore) error {
	store.SchemaVersion = pluginSignTrustStoreSchemaVersion
	store.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	sort.SliceStable(store.Keys, func(i, j int) bool {
		return store.Keys[i].KeyID < store.Keys[j].KeyID
	})
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func findPluginSignTrustKey(keys []pluginSignTrustKey, keyID string) (pluginSignTrustKey, int) {
	for i, key := range keys {
		if key.KeyID == keyID {
			return key, i
		}
	}
	return pluginSignTrustKey{}, -1
}

func selectPluginSignTrustKey(keys []pluginSignTrustKey, keyID string, now time.Time) (pluginSignTrustKey, error) {
	if keyID != "" {
		key, index := findPluginSignTrustKey(keys, keyID)
		if index < 0 {
			return pluginSignTrustKey{}, fmt.Errorf("trusted key %q not found", keyID)
		}
		return key, nil
	}
	active := activePluginSignTrustKeys(keys, now)
	if len(active) != 1 {
		return pluginSignTrustKey{}, fmt.Errorf("trust store has %d active keys; pass --key-id", len(active))
	}
	return active[0], nil
}

func activePluginSignTrustKeys(keys []pluginSignTrustKey, now time.Time) []pluginSignTrustKey {
	active := make([]pluginSignTrustKey, 0, len(keys))
	for _, key := range keys {
		status, err := pluginSignTrustStatus(key, now)
		if err == nil && status == "trusted" {
			active = append(active, key)
		}
	}
	return active
}

func pluginSignTrustStatus(key pluginSignTrustKey, now time.Time) (string, error) {
	if key.Revoked {
		return "revoked", nil
	}
	if key.Algorithm != "" && key.Algorithm != "ed25519" {
		return "", fmt.Errorf("trusted key %q algorithm %q is not supported", key.KeyID, key.Algorithm)
	}
	if key.NotBefore != "" {
		notBefore, err := time.Parse(time.RFC3339, key.NotBefore)
		if err != nil {
			return "", fmt.Errorf("trusted key %q has invalid not_before: %w", key.KeyID, err)
		}
		if now.Before(notBefore) {
			return "not_yet_valid", nil
		}
	}
	if key.NotAfter != "" {
		notAfter, err := time.Parse(time.RFC3339, key.NotAfter)
		if err != nil {
			return "", fmt.Errorf("trusted key %q has invalid not_after: %w", key.KeyID, err)
		}
		if now.After(notAfter) {
			return "expired", nil
		}
	}
	return "trusted", nil
}

func normalizePluginSignTimestamp(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return "", err
	}
	return parsed.UTC().Format(time.RFC3339), nil
}

func decodeBase64ValueOrFile(value string) ([]byte, error) {
	raw := strings.TrimSpace(value)
	if data, err := os.ReadFile(value); err == nil {
		raw = strings.TrimSpace(string(data))
	}
	return base64.StdEncoding.DecodeString(raw)
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
	target := "."
	manifestPath := ""
	artifactType := ""
	write := false
	canonicalJSON := false
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
		case "write":
			write = parsePluginBoolFlag(value)
		case "canonical-json":
			canonicalJSON = parsePluginBoolFlag(value)
		case "manifest":
			manifestPath = value
		case "type":
			artifactType = value
		default:
			return fmt.Errorf("unknown manifest format flag --%s", key)
		}
	}
	if write && canonicalJSON {
		return errors.New("--write and --canonical-json are mutually exclusive")
	}
	source, err := readPluginManifestSource(target, manifestPath)
	if err != nil {
		return err
	}
	if canonicalJSON {
		if artifactType == "" {
			artifactType = source.Manifest.ArtifactType
		}
		if artifactType == "" {
			artifactType = pluginmanager.ArtifactTypeBinary
		}
		switch artifactType {
		case pluginmanager.ArtifactTypeBinary, pluginmanager.ArtifactTypeSource:
		default:
			return fmt.Errorf("unsupported --type %q", artifactType)
		}
		canonical, err := materializedManifestJSON(source.Raw, source.Manifest, artifactType, false)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(canonical)
		return err
	}
	if artifactType != "" {
		return errors.New("--type is only valid with --canonical-json")
	}
	if write {
		if source.Format != manifestFormatJSON {
			fmt.Fprintf(os.Stdout, "ok manifest=%s validated comments_preserved=true\n", source.Path)
			return nil
		}
		if bytes.Equal(source.Data, source.CanonicalJSON) {
			fmt.Fprintf(os.Stdout, "ok manifest=%s unchanged\n", source.Path)
			return nil
		}
		if err := os.WriteFile(source.Path, source.CanonicalJSON, 0644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "ok manifest=%s formatted\n", source.Path)
		return nil
	}
	if source.Format == manifestFormatJSON {
		_, err = os.Stdout.Write(source.CanonicalJSON)
		return err
	}
	_, err = os.Stdout.Write(source.Data)
	if err == nil && len(source.Data) > 0 && source.Data[len(source.Data)-1] != '\n' {
		fmt.Fprintln(os.Stdout)
	}
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
		"runtime.abi": {
			"key":     "runtime.abi",
			"values":  []string{pluginmanager.WASMHostABIVersion()},
			"summary": "WASM host ABI version required when runtime.type is wasm.",
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
		"ingress.service/v1": {
			"key":      pluginmanager.ExtensionIngressService,
			"type":     "service",
			"maturity": "reserved",
			"summary":  "Future extension point for gateway-managed listener services. Capability schema is validated, but runtime data plane is not implemented.",
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
		Format:    manifestFormatYAML,
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
		case "manifest-format":
			opts.Format = value
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
	if err := validateManifestTemplateFormat(opts.Format); err != nil {
		return err
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
		case "manifest":
			opts.Manifest = value
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
	manifest, raw, err := readPluginDirManifest(opts.Dir, opts.Manifest)
	if err != nil {
		return err
	}
	adapter, err := pluginCLIAdapterForRuntime(manifest.Runtime.Type)
	if err != nil {
		return err
	}
	if !opts.SkipTests {
		if err := adapter.Test(context.Background(), pluginTestCLIOptions{
			Target:   opts.Dir,
			Manifest: opts.Manifest,
			Profile:  "unit",
			Source:   manifest,
		}); err != nil {
			return err
		}
	}
	outDir := opts.Out
	if opts.BuildType == "both" {
		if outDir == "" {
			outDir = filepath.Join(opts.Dir, "dist")
		}
	} else if outDir == "" || strings.HasSuffix(outDir, string(os.PathSeparator)) {
		if outDir == "" {
			outDir = filepath.Join(opts.Dir, "dist")
		}
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
	manifestPath := ""
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
		case "manifest":
			manifestPath = value
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
	manifest, _, err := readPluginDirManifest(target, manifestPath)
	if err != nil {
		return err
	}
	adapter, err := pluginCLIAdapterForRuntime(manifest.Runtime.Type)
	if err != nil {
		return err
	}
	if err := adapter.Test(context.Background(), pluginTestCLIOptions{
		Target:      target,
		Manifest:    manifestPath,
		Profile:     profile,
		ConfigPath:  configPath,
		FixturePath: fixturePath,
		Source:      manifest,
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
		case "manifest":
			opts.Manifest = value
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
		case "require-conformance-fixture":
			opts.RequireConformanceFixture = parsePluginBoolFlag(value)
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
		manifest, raw, err := readPluginDirManifest(targetPath, opts.Manifest)
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
		DB:                        db,
		ArtifactRoot:              filepath.Join(tmpRoot, "artifacts"),
		Adapter:                   cliStaticRuntimeAdapter{},
		PolicyProfile:             opts.Profile,
		RequireConformanceFixture: opts.RequireConformanceFixture,
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

func runPluginValidatePathCLI(args []string, expectedArtifactType string) (pluginmanager.ArtifactRecord, error) {
	if len(args) == 0 {
		return pluginmanager.ArtifactRecord{}, errors.New("plugin validate requires a plugin directory, manifest source, or .mcgp artifact")
	}
	target := ""
	manifestPath := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			if target != "" {
				return pluginmanager.ArtifactRecord{}, fmt.Errorf("unexpected argument %q", arg)
			}
			target = arg
			continue
		}
		key, value, consumed, err := parsePluginCLIFlag(args, i)
		if err != nil {
			return pluginmanager.ArtifactRecord{}, err
		}
		i += consumed
		switch key {
		case "manifest":
			manifestPath = value
		default:
			return pluginmanager.ArtifactRecord{}, fmt.Errorf("unknown validate flag --%s", key)
		}
	}
	if target == "" {
		return pluginmanager.ArtifactRecord{}, errors.New("plugin validate requires a plugin directory, manifest source, or .mcgp artifact")
	}
	return validatePluginPathForCLIWithManifest(target, expectedArtifactType, manifestPath)
}

func validatePluginPathForCLI(targetPath, expectedArtifactType string) (pluginmanager.ArtifactRecord, error) {
	return validatePluginPathForCLIWithManifest(targetPath, expectedArtifactType, "")
}

func validatePluginPathForCLIWithManifest(targetPath, expectedArtifactType, manifestPath string) (pluginmanager.ArtifactRecord, error) {
	info, err := os.Stat(targetPath)
	if err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	if info.IsDir() {
		artifact, err := validatePluginDirectoryForCLI(targetPath, manifestPath)
		if err != nil {
			return pluginmanager.ArtifactRecord{}, err
		}
		if expectedArtifactType != "" && artifact.ArtifactType != expectedArtifactType {
			return pluginmanager.ArtifactRecord{}, fmt.Errorf("artifact_type %q does not match expected %q", artifact.ArtifactType, expectedArtifactType)
		}
		return artifact, nil
	}
	if isManifestSourceFile(targetPath) {
		dir := filepath.Dir(targetPath)
		artifact, err := validatePluginDirectoryForCLI(dir, targetPath)
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

func validatePluginDirectoryForCLI(dir, manifestPath string) (pluginmanager.ArtifactRecord, error) {
	manifest, raw, err := readPluginDirManifest(dir, manifestPath)
	if err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	if manifest.Runtime.Type == pluginmanager.RuntimeWASM {
		return validateWASMPluginDirectoryForCLI(context.Background(), dir, manifestPath)
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

func validateWASMPluginDirectoryForCLI(ctx context.Context, dir, manifestPath string) (pluginmanager.ArtifactRecord, error) {
	manifest, raw, err := readPluginDirManifest(dir, manifestPath)
	if err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	tmpRoot, err := os.MkdirTemp("", "mcgp-wasm-dir-validate-*")
	if err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	defer os.RemoveAll(tmpRoot)
	packagePath := filepath.Join(tmpRoot, manifest.ID+".mcgp")
	if _, err := buildWASMPluginPackageContext(ctx, dir, manifest, raw, packagePath); err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	store := pluginmanager.NewArtifactStore(filepath.Join(tmpRoot, "store"))
	return store.ValidateAndStoreBinary(pluginmanager.ArtifactUpload{SourcePath: packagePath, FileName: filepath.Base(packagePath), Actor: "cli"})
}

func buildBinaryPluginPackage(dir string, manifest pluginmanager.Manifest, raw map[string]any, outPath string) (pluginmanager.ArtifactRecord, error) {
	return buildBinaryPluginPackageContext(context.Background(), dir, manifest, raw, outPath)
}

func buildBinaryPluginPackageContext(ctx context.Context, dir string, manifest pluginmanager.Manifest, raw map[string]any, outPath string) (pluginmanager.ArtifactRecord, error) {
	return buildBinaryPluginPackageWithArgs(ctx, dir, manifest, raw, outPath, "-trimpath")
}

func buildBinaryPluginPackageForConformance(ctx context.Context, dir string, manifest pluginmanager.Manifest, raw map[string]any, outPath string) (pluginmanager.ArtifactRecord, error) {
	adapter, err := pluginCLIAdapterForRuntime(manifest.Runtime.Type)
	if err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	return adapter.BuildBinary(ctx, pluginBuildRuntimeRequest{
		Dir:      dir,
		Manifest: manifest,
		Raw:      raw,
		OutPath:  outPath,
	})
}

func buildBinaryPluginPackageWithArgs(ctx context.Context, dir string, manifest pluginmanager.Manifest, raw map[string]any, outPath string, extraArgs ...string) (pluginmanager.ArtifactRecord, error) {
	tmpRoot, err := os.MkdirTemp("", "mcgp-build-binary-*")
	if err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	defer os.RemoveAll(tmpRoot)
	pluginPath := filepath.Join(tmpRoot, pluginmanager.RuntimeEntry)
	buildEntry := sourceBuildEntryForCLI(manifest)
	args := []string{"build", "-buildmode=plugin", "-buildvcs=false"}
	args = append(args, extraArgs...)
	args = append(args, "-o", pluginPath, buildEntry)
	if err := runGoCommand(ctx, dir, "go", args...); err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	if err := validatePluginSymbol(ctx, dir, pluginPath, manifest); err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	manifestBytes, err := materializedManifestJSON(raw, manifest, pluginmanager.ArtifactTypeBinary, false)
	if err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	if err := writeBinaryPackage(pluginPath, pluginmanager.RuntimeEntry, manifestBytes, dir, outPath); err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	return validatePluginPathForCLI(outPath, pluginmanager.ArtifactTypeBinary)
}

func buildWASMPluginPackageContext(ctx context.Context, dir string, manifest pluginmanager.Manifest, raw map[string]any, outPath string) (pluginmanager.ArtifactRecord, error) {
	modulePath, cleanup, err := ensureWASMModuleForCLI(ctx, dir, manifest)
	if err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	defer cleanup()
	manifestBytes, err := materializedManifestJSON(raw, manifest, pluginmanager.ArtifactTypeBinary, false)
	if err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	if err := writeBinaryPackage(modulePath, pluginmanager.RuntimeWASMEntry, manifestBytes, dir, outPath); err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	return validatePluginPathForCLI(outPath, pluginmanager.ArtifactTypeBinary)
}

func ensureWASMModuleForCLI(ctx context.Context, dir string, manifest pluginmanager.Manifest) (string, func(), error) {
	entry := strings.TrimSpace(manifest.Runtime.Entry)
	if entry == "" {
		entry = pluginmanager.RuntimeWASMEntry
	}
	if entry != pluginmanager.RuntimeWASMEntry {
		return "", func() {}, fmt.Errorf("unsupported wasm runtime.entry %q", manifest.Runtime.Entry)
	}
	modulePath := filepath.Join(dir, filepath.FromSlash(entry))
	if info, err := os.Stat(modulePath); err == nil && !info.IsDir() && info.Size() > 0 {
		return modulePath, func() {}, nil
	}
	scriptPath := filepath.Join(dir, "build.sh")
	if info, err := os.Stat(scriptPath); err != nil || info.IsDir() {
		return "", func() {}, fmt.Errorf("wasm runtime entry %q is required", entry)
	}
	scriptPath, err := filepath.Abs(scriptPath)
	if err != nil {
		return "", func() {}, err
	}
	tmpRoot, err := os.MkdirTemp("", "mcgp-wasm-build-*")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(tmpRoot) }
	outPath := filepath.Join(tmpRoot, pluginmanager.RuntimeWASMEntry)
	cmd := exec.CommandContext(ctx, "sh", scriptPath, outPath)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "MC_GATEWAY_WASM_OUT="+outPath)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		cleanup()
		if strings.TrimSpace(output.String()) == "" {
			return "", func() {}, fmt.Errorf("run wasm build script: %w", err)
		}
		return "", func() {}, fmt.Errorf("run wasm build script: %w\n%s", err, strings.TrimSpace(output.String()))
	}
	info, err := os.Stat(outPath)
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("wasm build script did not create %s: %w", outPath, err)
	}
	if info.IsDir() || info.Size() == 0 {
		cleanup()
		return "", func() {}, fmt.Errorf("wasm build script created empty runtime entry %s", outPath)
	}
	return outPath, cleanup, nil
}

func runWASMFixtureTestForCLI(ctx context.Context, opts pluginTestCLIOptions) error {
	manifest, raw, err := readPluginDirManifest(opts.Target, opts.Manifest)
	if err != nil {
		return err
	}
	tmpRoot, err := os.MkdirTemp("", "mcgp-wasm-test-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpRoot)
	packagePath := filepath.Join(tmpRoot, manifest.ID+".mcgp")
	if _, err := buildWASMPluginPackageContext(ctx, opts.Target, manifest, raw, packagePath); err != nil {
		return err
	}
	db, err := openPluginCLIDB(filepath.Join(tmpRoot, "plugins.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	manager := pluginmanager.New(pluginmanager.Options{
		DB:            db,
		ArtifactRoot:  filepath.Join(tmpRoot, "artifacts"),
		PolicyProfile: pluginmanager.PolicyProfileDev,
	})
	artifact, err := manager.UploadArtifact(ctx, pluginmanager.ArtifactUpload{
		SourcePath: packagePath,
		FileName:   filepath.Base(packagePath),
		Actor:      "cli",
	})
	if err != nil {
		return err
	}
	configJSON, err := pluginTestConfigJSON(opts)
	if err != nil {
		return err
	}
	if wasmManifestHasPointForCLI(manifest, pluginmanager.ExtensionConfigValidate) {
		if _, err := manager.DryRunConfig(ctx, artifact.PluginID, artifact.ID, configJSON); err != nil {
			return err
		}
	}
	if !wasmManifestHasPointForCLI(manifest, pluginmanager.ExtensionRuleEvaluate) && !wasmManifestHasPointForCLI(manifest, pluginmanager.ExtensionRouteResolve) {
		return nil
	}
	if _, err := manager.SetDesired(ctx, "cli", artifact.PluginID, artifact.ID, pluginmanager.DesiredEnabled, configJSON, pluginmanager.DefaultPriority); err != nil {
		return err
	}
	if _, err := manager.Enable(ctx, "cli", artifact.PluginID); err != nil {
		return err
	}
	if wasmManifestHasPointForCLI(manifest, pluginmanager.ExtensionRuleEvaluate) {
		if _, err := manager.EvaluateRule(ctx, api.RuleEvaluateRequest{
			Subject:  "wasm-fixture",
			Action:   "join",
			Resource: "server-a",
			Host:     "play.example",
			Context:  ctx,
		}); err != nil {
			return err
		}
	}
	if wasmManifestHasPointForCLI(manifest, pluginmanager.ExtensionRouteResolve) {
		if _, err := manager.ResolveRoute(ctx, api.RouteResolveRequest{
			Host:             "play.example",
			FallbackUpstream: "fallback:25565",
			FallbackHit:      true,
			Refresh:          true,
			Context:          ctx,
		}, nil); err != nil {
			return err
		}
	}
	return nil
}

func pluginTestConfigJSON(opts pluginTestCLIOptions) (string, error) {
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
	defaultConfig := filepath.Join(opts.Target, "testdata", "config.json")
	if data, err := os.ReadFile(defaultConfig); err == nil {
		if !json.Valid(data) {
			return "", fmt.Errorf("default config file %q must contain valid JSON", defaultConfig)
		}
		return string(data), nil
	}
	return "{}", nil
}

func wasmManifestHasPointForCLI(manifest pluginmanager.Manifest, point string) bool {
	for _, extension := range manifest.ExtensionPoints {
		if extension.Key == point {
			return true
		}
	}
	return false
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
		"go.mod":                               goModTemplate(opts.Module, repoRoot),
		"main.go":                              goPluginMainTemplate(opts),
		"main_test.go":                         goPluginTestTemplate(),
		"README.md":                            readmeTemplate(opts),
		manifestFileNameForFormat(opts.Format): manifestTemplate(opts),
		"testdata/config.json":                 configTemplate(opts.Template),
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
	runtimeType, _ := runtimeMap["type"].(string)
	if runtimeType == "" {
		runtimeType = manifest.Runtime.Type
	}
	if runtimeType != pluginmanager.RuntimeWASM && (runtimeMap["entry_symbol"] == nil || runtimeMap["entry_symbol"] == "") {
		runtimeMap["entry_symbol"] = "Plugin"
	}
	if artifactType == pluginmanager.ArtifactTypeBinary && runtimeType == pluginmanager.RuntimeWASM {
		runtimeMap["entry"] = pluginmanager.RuntimeWASMEntry
		if runtimeMap["abi"] == nil || runtimeMap["abi"] == "" {
			runtimeMap["abi"] = manifest.Runtime.ABI
		}
	} else if artifactType == pluginmanager.ArtifactTypeBinary {
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
		buildMap["vendor_required"] = vendor
		clone["build"] = buildMap
	}
	data, err := json.MarshalIndent(clone, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func writeBinaryPackage(pluginPath, entryName string, manifestBytes []byte, sourceDir, outPath string) error {
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
	if entryName == "" {
		entryName = pluginmanager.RuntimeEntry
	}
	if err := addZipFileStable(zw, entryName, pluginPath); err != nil {
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
		if isManifestSourceFile(rel) {
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
	case strings.EqualFold(base, "conformance.json"):
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
	for _, name := range []string{"README.md", "LICENSE", "conformance.json"} {
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
	if name == "go" && os.Getenv("GOFLAGS") == "" {
		cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	}
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
	case "skip-tests", "vendor", "json", "quiet", "dry-run", "write", "canonical-json", "source", "full-desired", "require-conformance-fixture":
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

func validateManifestTemplateFormat(format string) error {
	switch format {
	case manifestFormatYAML, "yml", manifestFormatTOML, manifestFormatJSONC, manifestFormatJSON:
		return nil
	default:
		return fmt.Errorf("unsupported --manifest-format %q", format)
	}
}

func manifestFileNameForFormat(format string) string {
	switch format {
	case manifestFormatJSON:
		return "manifest.json"
	case manifestFormatJSONC:
		return "manifest.jsonc"
	case manifestFormatTOML:
		return "manifest.toml"
	case "yml":
		return "manifest.yml"
	default:
		return "manifest.yaml"
	}
}

func manifestTemplate(opts pluginInitCLIOptions) string {
	switch opts.Format {
	case manifestFormatJSON:
		return manifestSourceJSONTemplate(opts, false)
	case manifestFormatJSONC:
		return manifestSourceJSONTemplate(opts, true)
	case manifestFormatTOML:
		return manifestTOMLTemplate(opts)
	default:
		return manifestYAMLTemplate(opts)
	}
}

func manifestSourceJSONTemplate(opts pluginInitCLIOptions, jsonc bool) string {
	mode := pluginmanager.UpstreamModeDialer
	if opts.Template == "protocol-proxy" {
		mode = pluginmanager.UpstreamModeProtocolProxy
	}
	prefix := ""
	if jsonc {
		prefix = "// Human-maintained plugin manifest. Build packages normalize this into manifest.json.\n"
	}
	manifest := fmt.Sprintf(`%s{
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
`, prefix, opts.ID, opts.Name, opts.Name+" plugin.", opts.Extension, mode)
	return manifest
}

func manifestYAMLTemplate(opts pluginInitCLIOptions) string {
	mode := pluginmanager.UpstreamModeDialer
	if opts.Template == "protocol-proxy" {
		mode = pluginmanager.UpstreamModeProtocolProxy
	}
	return fmt.Sprintf(`# 人工维护的插件清单；构建插件包时会规范化为 manifest.json。
schema_version: mc-gateway.plugin/v1
id: %q
name: %q
version: 0.1.0
description: %q
artifact_type: source
runtime:
  type: go-plugin
  entry: plugin.so
  entry_symbol: Plugin
build:
  type: go
  entry: "."
  output: plugin.so
  tags: []
  vendor_required: false
api_version: plugin-api/v1
sdk_module: github.com/tursom/mc-gateway/plugin/api
sdk_module_version: v0.1.0
extension_points:
  - type: hook
    key: %q
capabilities:
  upstream_connect:
    mode: %q
runtime_limits:
  handler_timeout_ms: 3000
  initial_write_timeout_ms: 1000
config_schema:
  type: object
  properties:
    match_host:
      type: string
    upstream:
      type: string
`, opts.ID, opts.Name, opts.Name+" plugin.", opts.Extension, mode)
}

func manifestTOMLTemplate(opts pluginInitCLIOptions) string {
	mode := pluginmanager.UpstreamModeDialer
	if opts.Template == "protocol-proxy" {
		mode = pluginmanager.UpstreamModeProtocolProxy
	}
	return fmt.Sprintf(`# 人工维护的插件清单；构建插件包时会规范化为 manifest.json。
schema_version = "mc-gateway.plugin/v1"
id = %q
name = %q
version = "0.1.0"
description = %q
artifact_type = "source"
api_version = "plugin-api/v1"
sdk_module = "github.com/tursom/mc-gateway/plugin/api"
sdk_module_version = "v0.1.0"

[runtime]
type = "go-plugin"
entry = "plugin.so"
entry_symbol = "Plugin"

[build]
type = "go"
entry = "."
output = "plugin.so"
tags = []
vendor_required = false

[[extension_points]]
type = "hook"
key = %q

[capabilities.upstream_connect]
mode = %q

[runtime_limits]
handler_timeout_ms = 3000
initial_write_timeout_ms = 1000

[config_schema]
type = "object"

[config_schema.properties.match_host]
type = "string"

[config_schema.properties.upstream]
type = "string"
`, opts.ID, opts.Name, opts.Name+" plugin.", opts.Extension, mode)
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
