// cmd/gateway/plugin_cli_toolchain_test.go 包含用于约束 plugin cli toolchain 行为的测试。

package main

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tursom/mc-gateway/internal/pluginmanager"
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

func TestCapabilityContractChecksValidateIngressSchema(t *testing.T) {
	manifest := pluginmanager.Manifest{
		ExtensionPoints: []pluginmanager.ExtensionPoint{{Type: "service", Key: pluginmanager.ExtensionIngressService}},
		Capabilities:    json.RawMessage(`{"extension_points":["ingress.service/v1"],"ingress":{"protocol":"tcp","bind":"127.0.0.1","port":25566}}`),
	}
	checks := capabilityContractChecks(manifest)
	hasValidCheck := false
	for _, check := range checks {
		if check["code"] == "ingress_service_schema" && check["severity"] == "info" {
			hasValidCheck = true
		}
	}
	if !hasValidCheck {
		t.Fatalf("capabilityContractChecks(valid ingress) = %+v, want ingress_service_schema info", checks)
	}

	manifest.Secrets = []pluginmanager.SecretSpec{{Name: "tls-cert", Required: true}}
	manifest.Capabilities = json.RawMessage(`{"extension_points":["ingress.service/v1"],"ingress":{"protocol":"smtp","bind":"localhost","port":70000,"tls":{"enabled":true,"cert_secret":"tls-cert","key_secret":"missing-key"}}}`)
	checks = capabilityContractChecks(manifest)
	hasBlockingCheck := false
	for _, check := range checks {
		if check["code"] == "ingress_service_schema" && check["severity"] == "blocking" {
			hasBlockingCheck = true
		}
	}
	if !hasBlockingCheck {
		t.Fatalf("capabilityContractChecks(invalid ingress) = %+v, want ingress_service_schema blocking", checks)
	}
}

func TestPluginFeaturesAndManifestCommands(t *testing.T) {
	output := captureStdout(t, func() {
		handled, code := runPluginCLI([]string{"plugin", "features"})
		if !handled {
			t.Fatal("runPluginCLI() handled = false")
		}
		if code != 0 {
			t.Fatalf("runPluginCLI(features) code = %d, want 0", code)
		}
	})
	var features struct {
		RuntimeTypes    []pluginmanager.RuntimeFeature              `json:"runtime_types"`
		ServiceModes    []pluginmanager.PluginServiceModeFeature    `json:"service_modes"`
		RuntimeAdapters []pluginmanager.RuntimeAdapterFactoryStatus `json:"runtime_adapters"`
		PluginHost      pluginmanager.PluginHostFeature             `json:"plugin_host"`
		ExtensionPoints []pluginmanager.ExtensionPointFeature       `json:"extension_points"`
		Build           struct {
			BuilderTypes                      []string `json:"builder_types"`
			ProdDefaultBuilder                string   `json:"prod_default_builder"`
			ProdBlocksLocalProcess            bool     `json:"prod_blocks_local_process"`
			ContainerDigestPinGate            bool     `json:"container_digest_pin_gate"`
			ContainerReleaseBindingGate       bool     `json:"container_release_binding_gate"`
			ContainerReleaseBindingStatus     string   `json:"container_release_binding_status"`
			OfficialBuilderImagePublished     bool     `json:"official_builder_image_published"`
			ExternalCIReleaseAttestationChain bool     `json:"external_ci_release_attestation_chain"`
		} `json:"build"`
		Repository struct {
			ImplementedTypes       []string `json:"implemented_types"`
			ImportAutoEnable       bool     `json:"import_auto_enable"`
			TargetDesiredApply     bool     `json:"target_desired_apply"`
			ApplyAutoEnable        bool     `json:"apply_auto_enable"`
			ApplyGovernanceGate    bool     `json:"apply_governance_gate"`
			UpdateAvailability     bool     `json:"update_availability"`
			CLIApply               bool     `json:"cli_apply"`
			CrossNodeApply         bool     `json:"cross_node_apply"`
			RemoteArtifactTransfer bool     `json:"remote_artifact_transfer"`
		} `json:"repository"`
		SupplyChain struct {
			LicensePolicy struct {
				AllowDeny        bool `json:"allow_deny"`
				ReviewRequired   bool `json:"review_required"`
				OrganizationSync bool `json:"organization_sync"`
			} `json:"license_policy"`
			SBOM struct {
				Generate              bool `json:"generate"`
				VulnerabilityDatabase bool `json:"vulnerability_database"`
				FeedSync              bool `json:"feed_sync"`
				ExternalFeedScheduler bool `json:"external_feed_scheduler"`
			} `json:"sbom"`
			ExternalCI struct {
				ProvenanceGate         bool `json:"provenance_gate"`
				SignatureRequired      bool `json:"signature_required"`
				SourceSHARequired      bool `json:"source_sha_required"`
				ArtifactSHARequired    bool `json:"artifact_sha_required"`
				TrustedBuilderRequired bool `json:"trusted_builder_required"`
			} `json:"external_ci"`
			Advisory struct {
				LocalGate             bool `json:"local_gate"`
				FeedSync              bool `json:"feed_sync"`
				Rescan                bool `json:"rescan"`
				ExternalFeedScheduler bool `json:"external_feed_scheduler"`
			} `json:"advisory"`
		} `json:"supply_chain"`
		Operations struct {
			BackgroundTaskLease      bool     `json:"background_task_lease"`
			TaskRunPolicies          []string `json:"task_run_policies"`
			LeaseStore               string   `json:"lease_store"`
			NodeState                bool     `json:"node_state"`
			ArtifactDistribution     bool     `json:"artifact_distribution"`
			ArtifactDistributionMode string   `json:"artifact_distribution_mode"`
			PartialRolloutFailure    bool     `json:"partial_rollout_failure"`
			BackgroundTaskRetry      bool     `json:"background_task_retry"`
			EventLowCardinalityGate  bool     `json:"event_low_cardinality_gate"`
			MetricLabelGate          bool     `json:"metric_label_gate"`
			TraceSummary             bool     `json:"trace_summary"`
			ExternalExporters        []struct {
				Type                string `json:"type"`
				Enabled             bool   `json:"enabled"`
				Maturity            string `json:"maturity"`
				FailureIsFailOpen   bool   `json:"failure_is_fail_open"`
				OutputLabelGate     bool   `json:"output_label_gate"`
				OutputSensitiveGate bool   `json:"output_sensitive_gate"`
				RollbackSupported   bool   `json:"rollback_supported"`
				UnsupportedReason   string `json:"unsupported_reason"`
			} `json:"external_exporters"`
			PluginDataFileRetentionGC     bool   `json:"plugin_data_file_retention_gc"`
			CrossCategoryRetentionRules   bool   `json:"cross_category_retention_rules"`
			GCDryRunExplainsProtected     bool   `json:"gc_dry_run_explains_protected"`
			OperationsGCApply             bool   `json:"operations_gc_apply"`
			DiagnosticPackage             bool   `json:"diagnostic_package"`
			DiagnosticStructuredRedaction bool   `json:"diagnostic_structured_redaction"`
			DiagnosticRunbook             bool   `json:"diagnostic_runbook"`
			DiagnosticRetentionGC         bool   `json:"diagnostic_retention_gc"`
			ExternalClientBoundary        string `json:"external_client_boundary"`
			CrossNodeApply                bool   `json:"cross_node_apply"`
		} `json:"operations"`
		Promotion struct {
			Export                        bool   `json:"export"`
			ImportDryRun                  bool   `json:"import_dry_run"`
			Diff                          bool   `json:"diff"`
			Drift                         bool   `json:"drift"`
			DRDrill                       bool   `json:"dr_drill"`
			TargetDesiredApply            bool   `json:"target_desired_apply"`
			ConfigHashRequired            bool   `json:"config_hash_required"`
			TargetConfigRequired          bool   `json:"target_config_required"`
			ConfigPlaintextExport         bool   `json:"config_plaintext_export"`
			GovernanceGate                bool   `json:"governance_gate"`
			LocalArtifactRequired         bool   `json:"local_artifact_required"`
			AutoEnable                    bool   `json:"auto_enable"`
			CLIApply                      bool   `json:"cli_apply"`
			CrossNodeApply                bool   `json:"cross_node_apply"`
			CrossNodeApplyMode            string `json:"cross_node_apply_mode"`
			RemoteArtifactTransfer        bool   `json:"remote_artifact_transfer"`
			AutomaticArtifactDistribution bool   `json:"automatic_artifact_distribution"`
			AutomaticClusterApply         bool   `json:"automatic_cluster_apply"`
		} `json:"promotion"`
		Instrumentation struct {
			Metadata                             bool `json:"metadata"`
			RuntimePlugin                        bool `json:"runtime_plugin"`
			AvailableRequiresGeneratedDiffHash   bool `json:"available_requires_generated_diff_hash"`
			AvailableRequiresGatewayBinarySHA256 bool `json:"available_requires_gateway_binary_sha256"`
			AvailableRequiresCIArtifactSHA256    bool `json:"available_requires_ci_artifact_sha256"`
			AvailableRequiresConformance         bool `json:"available_requires_conformance"`
			AvailableRequiresBenchmark           bool `json:"available_requires_benchmark"`
			AvailableRequiresSmoke               bool `json:"available_requires_smoke"`
			CIArtifactBinding                    bool `json:"ci_artifact_binding"`
		} `json:"instrumentation"`
		Ingress struct {
			SchemaValidation                   bool `json:"schema_validation"`
			PreflightSchemaGate                bool `json:"preflight_schema_gate"`
			GovernancePluginPortConflict       bool `json:"governance_plugin_port_conflict"`
			GovernanceReservedListenerConflict bool `json:"governance_reserved_listener_conflict"`
			GatewayListenerLifecycle           bool `json:"gateway_listener_lifecycle"`
			TLSSecretRefsRedacted              bool `json:"tls_secret_refs_redacted"`
			DisableDrain                       bool `json:"disable_drain"`
			ReservedListenerRuntimeRefresh     bool `json:"reserved_listener_runtime_refresh"`
			FutureRuntimeGate                  bool `json:"future_runtime_gate"`
			DataPlane                          bool `json:"data_plane"`
		} `json:"ingress"`
		Sandbox struct {
			RuntimeTypeReserved      bool `json:"runtime_type_reserved"`
			ServiceModeReserved      bool `json:"service_mode_reserved"`
			RequiredCapabilityGate   bool `json:"required_capability_gate"`
			FutureRuntimeGate        bool `json:"future_runtime_gate"`
			Supervisor               bool `json:"supervisor"`
			ControlRPC               bool `json:"control_rpc"`
			FilesystemEnforcement    bool `json:"filesystem_enforcement"`
			NetworkEnforcement       bool `json:"network_enforcement"`
			EnvEnforcement           bool `json:"env_enforcement"`
			CPUMemoryEnforcement     bool `json:"cpu_memory_enforcement"`
			SecretRPC                bool `json:"secret_rpc"`
			CrashLoopPolicyDataPlane bool `json:"crash_loop_policy_data_plane"`
			DiagnosticSummary        bool `json:"diagnostic_summary"`
			DataPlane                bool `json:"data_plane"`
		} `json:"sandbox"`
		WASM struct {
			RuntimeTypeReserved       bool `json:"runtime_type_reserved"`
			ContainedValidation       bool `json:"contained_validation"`
			HighRiskExtensionRejected bool `json:"high_risk_extension_rejected"`
			FutureRuntimeGate         bool `json:"future_runtime_gate"`
			RuntimeAdapter            bool `json:"runtime_adapter"`
			HostABI                   bool `json:"host_abi"`
			ModuleCache               bool `json:"module_cache"`
			FuelTimeMemoryLimits      bool `json:"fuel_time_memory_limits"`
			DefaultNoFileNetwork      bool `json:"default_no_file_network"`
			DataPlane                 bool `json:"data_plane"`
		} `json:"wasm"`
		Conformance struct {
			StableJSON                           bool     `json:"stable_json"`
			GoldenFixtureFile                    bool     `json:"golden_fixture_file"`
			DefaultReleaseGate                   bool     `json:"default_release_gate"`
			PackageFixtureFailuresBlockPreflight bool     `json:"package_fixture_failures_block_preflight"`
			MissingFixtureRequired               bool     `json:"missing_fixture_required"`
			MissingFixturePolicyGate             bool     `json:"missing_fixture_policy_gate"`
			MissingFixturePreflightFlag          string   `json:"missing_fixture_preflight_flag"`
			ManifestContractFixture              bool     `json:"manifest_contract_fixture"`
			InvalidConfigFromSchema              bool     `json:"invalid_config_from_schema"`
			MissingSecretFromManifest            bool     `json:"missing_secret_from_manifest"`
			RouteDecisions                       bool     `json:"route_decisions"`
			StatusHosts                          bool     `json:"status_hosts"`
			StreamProxyProtocol                  string   `json:"stream_proxy_protocol"`
			StreamProxySemantics                 []string `json:"stream_proxy_semantics"`
			ProtocolProxyScenarios               []string `json:"protocol_proxy_scenarios"`
			RuleEvaluationOutcomes               []string `json:"rule_evaluation_outcomes"`
			ConnectionFilterScenarios            []string `json:"connection_filter_scenarios"`
			HandshakeFilterScenarios             []string `json:"handshake_filter_scenarios"`
			GovernanceGateScenarios              []string `json:"governance_gate_scenarios"`
			EventDelivery                        []string `json:"event_delivery"`
			ProviderRegistry                     []string `json:"provider_registry"`
		} `json:"conformance"`
		CLI struct {
			ImplementedCommands []string `json:"implemented_commands"`
			ReservedCommands    []string `json:"reserved_commands"`
		} `json:"cli"`
	}
	if err := json.Unmarshal([]byte(output), &features); err != nil {
		t.Fatalf("Unmarshal(features) error = %v\n%s", err, output)
	}
	goPlugin := findRuntimeFeature(features.RuntimeTypes, pluginmanager.RuntimeGoPlugin)
	if !goPlugin.Implemented || goPlugin.Maturity != pluginmanager.FeatureMaturityImplemented || !goPlugin.DataPlane || goPlugin.RequiresRestart {
		t.Fatalf("go-plugin feature = %+v, want implemented data-plane", goPlugin)
	}
	if len(features.RuntimeTypes) != len(pluginmanager.RuntimeTypeFeatures()) {
		t.Fatalf("runtime feature count = %d, want shared fact source count %d", len(features.RuntimeTypes), len(pluginmanager.RuntimeTypeFeatures()))
	}
	if len(features.ExtensionPoints) != len(pluginmanager.ExtensionPointFeatures()) {
		t.Fatalf("extension feature count = %d, want shared fact source count %d", len(features.ExtensionPoints), len(pluginmanager.ExtensionPointFeatures()))
	}
	wasm := findRuntimeFeature(features.RuntimeTypes, pluginmanager.RuntimeWASM)
	expectedWASM := pluginmanager.RuntimeTypeFeature(pluginmanager.RuntimeWASM)
	if wasm.Implemented != expectedWASM.Implemented ||
		wasm.Maturity != expectedWASM.Maturity ||
		wasm.DataPlane != expectedWASM.DataPlane ||
		wasm.RequiresRestart != expectedWASM.RequiresRestart ||
		wasm.UnsupportedReason != expectedWASM.UnsupportedReason ||
		wasm.Entry != expectedWASM.Entry {
		t.Fatalf("wasm feature = %+v, want shared runtime fact source %+v", wasm, expectedWASM)
	}
	if wasm.Implemented || wasm.Maturity != pluginmanager.FeatureMaturityReserved || wasm.DataPlane ||
		!strings.Contains(wasm.UnsupportedReason, "WASM data-plane") {
		t.Fatalf("wasm feature = %+v, want reserved non-data-plane runtime", wasm)
	}
	sandboxRuntime := findRuntimeFeature(features.RuntimeTypes, pluginmanager.RuntimeSandbox)
	if sandboxRuntime.Implemented || sandboxRuntime.Maturity != pluginmanager.FeatureMaturityReserved || sandboxRuntime.DataPlane ||
		!strings.Contains(sandboxRuntime.UnsupportedReason, "sandbox data-plane") {
		t.Fatalf("sandbox runtime feature = %+v, want reserved non-data-plane runtime", sandboxRuntime)
	}
	inProcess := findCLIServiceModeFeature(features.ServiceModes, pluginmanager.PluginServiceModeInProcess)
	if !inProcess.Implemented || inProcess.Maturity != pluginmanager.FeatureMaturityImplemented || !inProcess.DataPlane || inProcess.RequiresRestart {
		t.Fatalf("in-process feature = %+v, want implemented data-plane", inProcess)
	}
	processMode := findCLIServiceModeFeature(features.ServiceModes, pluginmanager.PluginServiceModeGoPluginProcess)
	if !processMode.Implemented || processMode.Maturity != pluginmanager.FeatureMaturityPartial || !processMode.DataPlane || !strings.Contains(processMode.UnsupportedReason, "protocol-proxy drain-only") {
		t.Fatalf("go-plugin-process feature = %+v, want partial process data plane", processMode)
	}
	sandboxMode := findCLIServiceModeFeature(features.ServiceModes, pluginmanager.PluginServiceModeSandboxProcess)
	if sandboxMode.Implemented || sandboxMode.Maturity != pluginmanager.FeatureMaturityReserved || sandboxMode.DataPlane ||
		!strings.Contains(sandboxMode.UnsupportedReason, "current data-plane modes") {
		t.Fatalf("sandbox service mode = %+v, want reserved non-data-plane service mode", sandboxMode)
	}
	inProcessAdapter := findRuntimeAdapterStatus(features.RuntimeAdapters, pluginmanager.PluginServiceModeInProcess, pluginmanager.RuntimeGoPlugin)
	if !inProcessAdapter.Implemented || inProcessAdapter.Maturity != pluginmanager.FeatureMaturityImplemented || !inProcessAdapter.DataPlane || !inProcessAdapter.Lifecycle || inProcessAdapter.Adapter != "go-plugin-in-process" {
		t.Fatalf("in-process runtime adapter = %+v, want implemented lifecycle data-plane adapter", inProcessAdapter)
	}
	processAdapter := findRuntimeAdapterStatus(features.RuntimeAdapters, pluginmanager.PluginServiceModeGoPluginProcess, pluginmanager.RuntimeGoPlugin)
	if !processAdapter.Implemented || processAdapter.Maturity != pluginmanager.FeatureMaturityPartial || !processAdapter.DataPlane || !processAdapter.Lifecycle || !strings.Contains(processAdapter.UnsupportedReason, "fd-live") {
		t.Fatalf("process runtime adapter = %+v, want partial process host adapter", processAdapter)
	}
	if processAdapter.HostProtocol != pluginmanager.PluginHostProtocol || processAdapter.ControlChannel != pluginmanager.PluginHostControlChannelUnix {
		t.Fatalf("process runtime adapter = %+v, want host protocol metadata", processAdapter)
	}
	wasmSandboxAdapter := findRuntimeAdapterStatus(features.RuntimeAdapters, pluginmanager.PluginServiceModeSandboxProcess, pluginmanager.RuntimeWASM)
	expectedWASMAdapter := findRuntimeAdapterStatus(pluginmanager.RuntimeAdapterFactoryStatuses(), pluginmanager.PluginServiceModeSandboxProcess, pluginmanager.RuntimeWASM)
	if wasmSandboxAdapter.Implemented != expectedWASMAdapter.Implemented ||
		wasmSandboxAdapter.Maturity != expectedWASMAdapter.Maturity ||
		wasmSandboxAdapter.DataPlane != expectedWASMAdapter.DataPlane ||
		wasmSandboxAdapter.Lifecycle != expectedWASMAdapter.Lifecycle ||
		wasmSandboxAdapter.RequiresRestart != expectedWASMAdapter.RequiresRestart ||
		wasmSandboxAdapter.Adapter != expectedWASMAdapter.Adapter ||
		wasmSandboxAdapter.ControlChannel != expectedWASMAdapter.ControlChannel ||
		wasmSandboxAdapter.UnsupportedReason != expectedWASMAdapter.UnsupportedReason {
		t.Fatalf("wasm sandbox runtime adapter = %+v, want shared adapter fact source %+v", wasmSandboxAdapter, expectedWASMAdapter)
	}
	ingressPoint := findExtensionPointFeature(features.ExtensionPoints, pluginmanager.ExtensionIngressService)
	expectedIngress := findExtensionPointFeature(pluginmanager.ExtensionPointFeatures(), pluginmanager.ExtensionIngressService)
	if ingressPoint.Key != expectedIngress.Key ||
		ingressPoint.Type != expectedIngress.Type ||
		ingressPoint.Implemented != expectedIngress.Implemented ||
		ingressPoint.Maturity != expectedIngress.Maturity ||
		ingressPoint.DataPlane != expectedIngress.DataPlane ||
		ingressPoint.RequiresRestart != expectedIngress.RequiresRestart ||
		ingressPoint.UnsupportedReason != expectedIngress.UnsupportedReason {
		t.Fatalf("extension point features = %+v, want shared ingress fact source %+v", features.ExtensionPoints, expectedIngress)
	}
	if ingressPoint.Implemented || ingressPoint.Maturity != pluginmanager.FeatureMaturityReserved || ingressPoint.DataPlane ||
		!strings.Contains(ingressPoint.UnsupportedReason, "listener data-plane") {
		t.Fatalf("ingress extension point = %+v, want reserved non-data-plane extension point", ingressPoint)
	}
	adminAuthPoint := findExtensionPointFeature(features.ExtensionPoints, pluginmanager.ExtensionAdminAuthProvider)
	if adminAuthPoint.Type != "provider" || adminAuthPoint.Implemented || adminAuthPoint.Maturity != pluginmanager.FeatureMaturityReserved || adminAuthPoint.DataPlane || adminAuthPoint.RequiresRestart || !strings.Contains(adminAuthPoint.UnsupportedReason, "break-glass") {
		t.Fatalf("admin auth extension point = %+v, want reserved provider with local break-glass note", adminAuthPoint)
	}
	authPoint := findExtensionPointFeature(features.ExtensionPoints, pluginmanager.ExtensionAuthProvider)
	if authPoint.Type != "provider" || !authPoint.Implemented || authPoint.Maturity != pluginmanager.FeatureMaturityPartial || authPoint.DataPlane || authPoint.RequiresRestart || !strings.Contains(authPoint.UnsupportedReason, "Minecraft login") {
		t.Fatalf("auth extension point = %+v, want partial provider registry without login data plane", authPoint)
	}
	if features.PluginHost.Protocol != pluginmanager.PluginHostProtocol ||
		features.PluginHost.Command != "plugin-host" ||
		!features.PluginHost.Handshake ||
		!features.PluginHost.ControlChannelImplemented ||
		!features.PluginHost.SupervisorStartStop ||
		!features.PluginHost.CrashTracking ||
		!features.PluginHost.CrashPolicy ||
		!features.PluginHost.SupervisorDataPlane ||
		!features.PluginHost.ProcessTableOrphanCleanup ||
		len(features.PluginHost.OrphanDiscoveryOrder) != len(pluginmanager.PluginHostOrphanDiscoveryOrder()) ||
		!features.PluginHost.Backoff ||
		!features.PluginHost.StreamDeadline ||
		!features.PluginHost.StreamCancel ||
		!features.PluginHost.StreamByteAccounting ||
		!features.PluginHost.DataPlane ||
		!features.PluginHost.LifecycleImplemented {
		t.Fatalf("plugin host feature = %+v, want partial lifecycle and dialer data plane", features.PluginHost)
	}
	for i, want := range pluginmanager.PluginHostOrphanDiscoveryOrder() {
		if features.PluginHost.OrphanDiscoveryOrder[i] != want {
			t.Fatalf("plugin host orphan discovery order = %+v, want %q at %d", features.PluginHost.OrphanDiscoveryOrder, want, i)
		}
	}
	if features.PluginHost.Maturity != pluginmanager.FeatureMaturityPartial ||
		!strings.Contains(features.PluginHost.UnsupportedReason, "per-node crash isolation") ||
		strings.Contains(features.PluginHost.UnsupportedReason, "cross-node crash policy coordination are not implemented") {
		t.Fatalf("plugin host feature = %+v, want partial process boundary without stale cross-node unsupported text", features.PluginHost)
	}
	if !containsString(features.Build.BuilderTypes, pluginmanager.BuilderTypeContainer) ||
		features.Build.ProdDefaultBuilder != pluginmanager.BuilderTypeContainer ||
		!features.Build.ProdBlocksLocalProcess ||
		!features.Build.ContainerDigestPinGate ||
		!features.Build.ContainerReleaseBindingGate ||
		features.Build.ContainerReleaseBindingStatus != pluginmanager.GateSeverityWarning ||
		!features.Build.OfficialBuilderImagePublished ||
		!features.Build.ExternalCIReleaseAttestationChain {
		t.Fatalf("build feature = %+v, want prod container builder and external CI release gates", features.Build)
	}
	if !containsString(features.Repository.ImplementedTypes, pluginmanager.RepositoryTypeFile) ||
		!containsString(features.Repository.ImplementedTypes, pluginmanager.RepositoryTypeURL) ||
		features.Repository.ImportAutoEnable ||
		!features.Repository.TargetDesiredApply ||
		features.Repository.ApplyAutoEnable ||
		!features.Repository.ApplyGovernanceGate ||
		!features.Repository.UpdateAvailability ||
		!features.Repository.CLIApply ||
		!features.Repository.CrossNodeApply ||
		!features.Repository.RemoteArtifactTransfer {
		t.Fatalf("repository feature = %+v, want desired-state apply with governance, remote artifact transfer, and cross-node reconcile", features.Repository)
	}
	if !features.SupplyChain.LicensePolicy.AllowDeny ||
		!features.SupplyChain.LicensePolicy.ReviewRequired ||
		features.SupplyChain.LicensePolicy.OrganizationSync ||
		!features.SupplyChain.SBOM.Generate ||
		!features.SupplyChain.SBOM.VulnerabilityDatabase ||
		!features.SupplyChain.SBOM.FeedSync ||
		!features.SupplyChain.SBOM.ExternalFeedScheduler ||
		!features.SupplyChain.ExternalCI.ProvenanceGate ||
		!features.SupplyChain.ExternalCI.SignatureRequired ||
		!features.SupplyChain.ExternalCI.SourceSHARequired ||
		!features.SupplyChain.ExternalCI.ArtifactSHARequired ||
		!features.SupplyChain.ExternalCI.TrustedBuilderRequired ||
		!features.SupplyChain.Advisory.LocalGate ||
		!features.SupplyChain.Advisory.FeedSync ||
		!features.SupplyChain.Advisory.Rescan ||
		!features.SupplyChain.Advisory.ExternalFeedScheduler {
		t.Fatalf("supply-chain feature = %+v, want local license/advisory/vulnerability gates with external feed scheduler", features.SupplyChain)
	}
	if !features.Operations.BackgroundTaskLease ||
		!containsString(features.Operations.TaskRunPolicies, pluginmanager.TaskRunPolicyPerNode) ||
		!containsString(features.Operations.TaskRunPolicies, pluginmanager.TaskRunPolicySingleton) ||
		!containsString(features.Operations.TaskRunPolicies, pluginmanager.TaskRunPolicySharded) ||
		features.Operations.LeaseStore != "admin-sqlite" ||
		!features.Operations.NodeState ||
		!features.Operations.ArtifactDistribution ||
		features.Operations.ArtifactDistributionMode != "local-content-store" ||
		!features.Operations.PartialRolloutFailure ||
		!features.Operations.BackgroundTaskRetry ||
		!features.Operations.EventLowCardinalityGate ||
		!features.Operations.MetricLabelGate ||
		!features.Operations.TraceSummary ||
		!features.Operations.PluginDataFileRetentionGC ||
		!features.Operations.CrossCategoryRetentionRules ||
		!features.Operations.GCDryRunExplainsProtected ||
		!features.Operations.OperationsGCApply ||
		!features.Operations.DiagnosticPackage ||
		!features.Operations.DiagnosticStructuredRedaction ||
		!features.Operations.DiagnosticRunbook ||
		!features.Operations.DiagnosticRetentionGC ||
		!strings.Contains(features.Operations.ExternalClientBoundary, "not strong network isolation") ||
		!features.Operations.CrossNodeApply {
		t.Fatalf("operations feature = %+v, want task lease/node state/local distribution/diagnostics/partial rollout and cross-node desired apply", features.Operations)
	}
	exporters := map[string]struct {
		Enabled             bool
		Maturity            string
		FailureIsFailOpen   bool
		OutputLabelGate     bool
		OutputSensitiveGate bool
		RollbackSupported   bool
		UnsupportedReason   string
	}{}
	for _, exporter := range features.Operations.ExternalExporters {
		exporters[exporter.Type] = struct {
			Enabled             bool
			Maturity            string
			FailureIsFailOpen   bool
			OutputLabelGate     bool
			OutputSensitiveGate bool
			RollbackSupported   bool
			UnsupportedReason   string
		}{
			Enabled:             exporter.Enabled,
			Maturity:            exporter.Maturity,
			FailureIsFailOpen:   exporter.FailureIsFailOpen,
			OutputLabelGate:     exporter.OutputLabelGate,
			OutputSensitiveGate: exporter.OutputSensitiveGate,
			RollbackSupported:   exporter.RollbackSupported,
			UnsupportedReason:   exporter.UnsupportedReason,
		}
	}
	for _, exporterType := range []string{pluginmanager.OperationsExporterPrometheus, pluginmanager.OperationsExporterOTel} {
		exporter, ok := exporters[exporterType]
		if !ok || exporter.Enabled || exporter.Maturity != pluginmanager.FeatureMaturityReserved ||
			!exporter.FailureIsFailOpen || !exporter.OutputLabelGate || !exporter.OutputSensitiveGate ||
			!exporter.RollbackSupported || !strings.Contains(exporter.UnsupportedReason, "not enabled") {
			t.Fatalf("operations exporters = %+v, want reserved fail-open %s boundary", features.Operations.ExternalExporters, exporterType)
		}
	}
	if !features.Promotion.Export ||
		!features.Promotion.ImportDryRun ||
		!features.Promotion.Diff ||
		!features.Promotion.Drift ||
		!features.Promotion.DRDrill ||
		!features.Promotion.TargetDesiredApply ||
		!features.Promotion.ConfigHashRequired ||
		!features.Promotion.TargetConfigRequired ||
		features.Promotion.ConfigPlaintextExport ||
		!features.Promotion.GovernanceGate ||
		!features.Promotion.LocalArtifactRequired ||
		features.Promotion.AutoEnable ||
		!features.Promotion.CLIApply ||
		!features.Promotion.CrossNodeApply ||
		features.Promotion.CrossNodeApplyMode != "shared_desired_state_cluster_reconcile" ||
		!features.Promotion.RemoteArtifactTransfer ||
		!features.Promotion.AutomaticArtifactDistribution ||
		!features.Promotion.AutomaticClusterApply {
		t.Fatalf("promotion feature = %+v, want cross-node desired apply with automatic artifact distribution and no auto-enable", features.Promotion)
	}
	if !features.Instrumentation.Metadata ||
		features.Instrumentation.RuntimePlugin ||
		!features.Instrumentation.AvailableRequiresGeneratedDiffHash ||
		!features.Instrumentation.AvailableRequiresGatewayBinarySHA256 ||
		!features.Instrumentation.AvailableRequiresCIArtifactSHA256 ||
		!features.Instrumentation.AvailableRequiresConformance ||
		!features.Instrumentation.AvailableRequiresBenchmark ||
		!features.Instrumentation.AvailableRequiresSmoke ||
		!features.Instrumentation.CIArtifactBinding {
		t.Fatalf("instrumentation feature = %+v, want metadata gate with gateway/CI artifact binding", features.Instrumentation)
	}
	if !features.Ingress.SchemaValidation ||
		!features.Ingress.PreflightSchemaGate ||
		!features.Ingress.GovernancePluginPortConflict ||
		!features.Ingress.GovernanceReservedListenerConflict ||
		!features.Ingress.GatewayListenerLifecycle ||
		!features.Ingress.TLSSecretRefsRedacted ||
		!features.Ingress.DisableDrain ||
		!features.Ingress.ReservedListenerRuntimeRefresh ||
		!features.Ingress.FutureRuntimeGate ||
		features.Ingress.DataPlane {
		t.Fatalf("ingress feature = %+v, want reserved listener lifecycle without current data plane", features.Ingress)
	}
	if !features.Sandbox.RuntimeTypeReserved ||
		!features.Sandbox.ServiceModeReserved ||
		!features.Sandbox.RequiredCapabilityGate ||
		!features.Sandbox.FutureRuntimeGate ||
		!features.Sandbox.Supervisor ||
		!features.Sandbox.ControlRPC ||
		!features.Sandbox.FilesystemEnforcement ||
		!features.Sandbox.NetworkEnforcement ||
		!features.Sandbox.EnvEnforcement ||
		!features.Sandbox.CPUMemoryEnforcement ||
		!features.Sandbox.SecretRPC ||
		features.Sandbox.CrashLoopPolicyDataPlane ||
		!features.Sandbox.DiagnosticSummary ||
		features.Sandbox.DataPlane {
		t.Fatalf("sandbox feature = %+v, want reserved sandbox controls without current data plane", features.Sandbox)
	}
	if !features.WASM.RuntimeTypeReserved ||
		!features.WASM.ContainedValidation ||
		!features.WASM.HighRiskExtensionRejected ||
		!features.WASM.FutureRuntimeGate ||
		!features.WASM.RuntimeAdapter ||
		!features.WASM.HostABI ||
		!features.WASM.ModuleCache ||
		!features.WASM.FuelTimeMemoryLimits ||
		!features.WASM.DefaultNoFileNetwork ||
		features.WASM.DataPlane {
		t.Fatalf("wasm feature = %+v, want reserved WASM controls without current data plane", features.WASM)
	}
	if !features.Conformance.StableJSON ||
		!features.Conformance.GoldenFixtureFile ||
		!features.Conformance.DefaultReleaseGate ||
		!features.Conformance.PackageFixtureFailuresBlockPreflight ||
		features.Conformance.MissingFixtureRequired ||
		!features.Conformance.MissingFixturePolicyGate ||
		features.Conformance.MissingFixturePreflightFlag != "--require-conformance-fixture" ||
		!features.Conformance.ManifestContractFixture ||
		!features.Conformance.InvalidConfigFromSchema ||
		!features.Conformance.MissingSecretFromManifest ||
		!features.Conformance.RouteDecisions ||
		!features.Conformance.StatusHosts ||
		features.Conformance.StreamProxyProtocol != pluginmanager.StreamProxyProtocolV1 ||
		!containsString(features.Conformance.StreamProxySemantics, "half_close") ||
		!containsString(features.Conformance.StreamProxySemantics, "backpressure") ||
		!containsString(features.Conformance.StreamProxySemantics, "byte_accounting") ||
		!containsString(features.Conformance.ProtocolProxyScenarios, "force_close_draining") ||
		!containsString(features.Conformance.ProtocolProxyScenarios, "backpressure_large_packet") ||
		!containsString(features.Conformance.RuleEvaluationOutcomes, "timeout_fail_closed") ||
		!containsString(features.Conformance.ConnectionFilterScenarios, "error_fail_closed") ||
		!containsString(features.Conformance.HandshakeFilterScenarios, "rewrite_host") ||
		!containsString(features.Conformance.GovernanceGateScenarios, "promotion_apply_gate") ||
		!containsString(features.Conformance.EventDelivery, "replay") ||
		!containsString(features.Conformance.EventDelivery, "drop") ||
		!containsString(features.Conformance.ProviderRegistry, "admin.auth.provider/v1") {
		t.Fatalf("conformance feature = %+v, want packaged fixture gate plus opt-in missing fixture policy gate", features.Conformance)
	}
	if !containsString(features.CLI.ImplementedCommands, "sign key-rotation") ||
		!containsString(features.CLI.ImplementedCommands, "sign revoke") ||
		!containsString(features.CLI.ImplementedCommands, "transfer") ||
		!containsString(features.CLI.ImplementedCommands, "promotion apply") ||
		!containsString(features.CLI.ImplementedCommands, "repo apply") ||
		!containsString(features.CLI.ImplementedCommands, "repo updates") ||
		!containsString(features.CLI.ImplementedCommands, "advisory rescan") ||
		!containsString(features.CLI.ImplementedCommands, "advisory sync") ||
		!containsString(features.CLI.ImplementedCommands, "vulnerability rescan") ||
		!containsString(features.CLI.ImplementedCommands, "vulnerability sync") ||
		!containsString(features.CLI.ImplementedCommands, "external health-check") ||
		containsString(features.CLI.ReservedCommands, "sign key-rotation/revoke") {
		t.Fatalf("cli feature = %+v, want implemented sign rotation/revoke without reserved marker", features.CLI)
	}
	handled, code := runPluginCLI([]string{"plugin", "manifest", "explain", "upstream.connect/v1"})
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

func TestPluginSchemaContractAndConformanceCommands(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "contract-plugin")
	handled, code := runPluginCLI([]string{
		"plugin", "init", dir,
		"--id", "contract-plugin",
		"--module", "example.com/contract-plugin",
	})
	if !handled || code != 0 {
		t.Fatalf("runPluginCLI(init) = (%v, %d), want handled code 0", handled, code)
	}
	schemaOutput := captureStdout(t, func() {
		handled, code := runPluginCLI([]string{"plugin", "schema", "export"})
		if !handled || code != 0 {
			t.Fatalf("runPluginCLI(schema export) = (%v, %d), want handled code 0", handled, code)
		}
	})
	var schemaReport struct {
		Command string         `json:"command"`
		Schemas map[string]any `json:"schemas"`
	}
	if err := json.Unmarshal([]byte(schemaOutput), &schemaReport); err != nil {
		t.Fatalf("Unmarshal(schema) error = %v\n%s", err, schemaOutput)
	}
	if schemaReport.Command != "schema export" || schemaReport.Schemas["manifest"] == nil || schemaReport.Schemas["cli"] == nil || schemaReport.Schemas["admin_api"] == nil || schemaReport.Schemas["conformance_fixture"] == nil {
		t.Fatalf("schema report = %+v, want manifest/cli/admin_api/conformance_fixture schemas", schemaReport)
	}
	cliSchema, ok := schemaReport.Schemas["cli"].(map[string]any)
	if !ok {
		t.Fatalf("cli schema = %#v, want object", schemaReport.Schemas["cli"])
	}
	cliCommands, ok := cliSchema["commands"].([]any)
	if !ok ||
		!containsAnyString(cliCommands, "enable") ||
		!containsAnyString(cliCommands, "repo apply") ||
		!containsAnyString(cliCommands, "task cancel") ||
		!containsAnyString(cliCommands, "promotion apply") {
		t.Fatalf("cli schema commands = %+v, want full implemented command list", cliSchema["commands"])
	}
	adminSchema, ok := schemaReport.Schemas["admin_api"].(map[string]any)
	if !ok {
		t.Fatalf("admin_api schema = %#v, want object", schemaReport.Schemas["admin_api"])
	}
	responseSchemas, ok := adminSchema["response_schemas"].(map[string]any)
	if !ok || responseSchemas["plugin_features"] == nil || responseSchemas["plugin_service"] == nil || responseSchemas["plugin_governance"] == nil {
		t.Fatalf("admin_api response_schemas = %+v, want plugin_features/plugin_service/plugin_governance schema", responseSchemas)
	}
	manifestSchema, ok := schemaReport.Schemas["manifest"].(map[string]any)
	if !ok {
		t.Fatalf("manifest schema = %#v, want object", schemaReport.Schemas["manifest"])
	}
	manifestProperties, ok := manifestSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("manifest schema properties = %#v, want object", manifestSchema["properties"])
	}
	runtimeTypeSchema, ok := manifestProperties["runtime.type"].(map[string]any)
	if !ok {
		t.Fatalf("manifest runtime.type schema = %#v, want object", manifestProperties["runtime.type"])
	}
	runtimeTypeEnums, ok := runtimeTypeSchema["enum"].([]any)
	if !ok {
		t.Fatalf("manifest runtime.type enum = %#v, want array", runtimeTypeSchema["enum"])
	}
	for _, feature := range pluginmanager.RuntimeTypeFeatures() {
		if !containsAnyString(runtimeTypeEnums, feature.Type) {
			t.Fatalf("manifest runtime.type enum = %+v, missing feature runtime %q", runtimeTypeEnums, feature.Type)
		}
	}
	extensionPointSchema, ok := manifestProperties["extension_points"].(map[string]any)
	if !ok {
		t.Fatalf("manifest extension_points schema = %#v, want object", manifestProperties["extension_points"])
	}
	extensionPointItems, ok := extensionPointSchema["items"].(map[string]any)
	if !ok {
		t.Fatalf("manifest extension_points items = %#v, want object", extensionPointSchema["items"])
	}
	for _, feature := range pluginmanager.ExtensionPointFeatures() {
		if extensionPointItems[feature.Key] != true {
			t.Fatalf("manifest extension point items = %+v, missing feature point %q", extensionPointItems, feature.Key)
		}
	}
	defs, ok := adminSchema["$defs"].(map[string]any)
	if !ok {
		t.Fatalf("admin_api defs = %+v, want schema definitions", adminSchema["$defs"])
	}
	assertSchemaProperties(t, defs, "plugin_feature_facts", "schema_version", "api_version", "runtime_types", "service_modes", "runtime_adapters", "plugin_host", "extension_points")
	assertSchemaProperties(t, defs, "extension_point_feature", "key", "type", "implemented", "maturity", "data_plane", "requires_restart", "unsupported_reason")
	assertSchemaProperties(t, defs, "plugin_service_state", "desired_mode", "active_mode", "data_plane_mode", "crash_policy", "restart_required")
	assertSchemaProperties(t, defs, "plugin_host_crash_policy", "backoff_seconds", "max_crashes", "window_seconds")
	assertSchemaProperties(t, defs, "runtime_adapter_status", "service_mode", "runtime_type", "maturity", "host_protocol", "control_channel", "unsupported_reason")
	assertSchemaProperties(t, defs, "plugin_host_runtime_summary", "plugin_id", "pid", "crash_loop", "backoff_until", "isolated")
	assertSchemaProperties(t, defs, "plugin_host_feature", "protocol", "orphan_discovery_order", "orphan_discovery_unsupported_reason", "unsupported_reason")
	assertSchemaProperties(t, defs, "plugin_node_state", "node_id", "service_mode", "data_plane_mode", "stale")
	assertSchemaProperties(t, defs, "plugin_governance_status", "decision", "policy", "conflicts")
	assertSchemaProperties(t, defs, "policy_snapshot", "profile", "review_required_risk", "require_conformance_fixture")
	fixtureSchemaOutput := captureStdout(t, func() {
		handled, code := runPluginCLI([]string{"plugin", "schema", "export", "--section", "conformance-fixture"})
		if !handled || code != 0 {
			t.Fatalf("runPluginCLI(schema export conformance-fixture) = (%v, %d), want handled code 0", handled, code)
		}
	})
	var fixtureSchemaReport struct {
		Schemas map[string]any `json:"schemas"`
	}
	if err := json.Unmarshal([]byte(fixtureSchemaOutput), &fixtureSchemaReport); err != nil {
		t.Fatalf("Unmarshal(conformance fixture schema) error = %v\n%s", err, fixtureSchemaOutput)
	}
	fixtureSchema, ok := fixtureSchemaReport.Schemas["conformance_fixture"].(map[string]any)
	if !ok {
		t.Fatalf("fixture schema report = %+v, want conformance_fixture schema", fixtureSchemaReport)
	}
	properties, ok := fixtureSchema["properties"].(map[string]any)
	if !ok ||
		properties["protocol_proxy_scenarios"] == nil ||
		properties["rule_evaluation_outcomes"] == nil ||
		properties["connection_filter_scenarios"] == nil ||
		properties["handshake_filter_scenarios"] == nil ||
		properties["governance_gate_scenarios"] == nil {
		t.Fatalf("fixture schema properties = %+v, want protocol/rule/middleware/governance scenario fields", properties)
	}

	contractOutput := captureStdout(t, func() {
		handled, code := runPluginCLI([]string{"plugin", "contract", dir})
		if !handled || code != 0 {
			t.Fatalf("runPluginCLI(contract) = (%v, %d), want handled code 0", handled, code)
		}
	})
	var contractReport struct {
		Command string           `json:"command"`
		OK      bool             `json:"ok"`
		Checks  []map[string]any `json:"checks"`
	}
	if err := json.Unmarshal([]byte(contractOutput), &contractReport); err != nil {
		t.Fatalf("Unmarshal(contract) error = %v\n%s", err, contractOutput)
	}
	if contractReport.Command != "contract" || !contractReport.OK || len(contractReport.Checks) == 0 {
		t.Fatalf("contract report = %+v, want ok with checks", contractReport)
	}

	conformanceOutput := captureStdout(t, func() {
		handled, code := runPluginCLI([]string{"plugin", "conformance", dir})
		if !handled || code != 0 {
			t.Fatalf("runPluginCLI(conformance) = (%v, %d), want handled code 0", handled, code)
		}
	})
	var conformanceReport struct {
		Command  string           `json:"command"`
		OK       bool             `json:"ok"`
		Fixtures []map[string]any `json:"fixtures"`
	}
	if err := json.Unmarshal([]byte(conformanceOutput), &conformanceReport); err != nil {
		t.Fatalf("Unmarshal(conformance) error = %v\n%s", err, conformanceOutput)
	}
	if conformanceReport.Command != "conformance" ||
		!conformanceReport.OK ||
		!hasFixture(conformanceReport.Fixtures, "governance_gate", "pass") ||
		!hasFixture(conformanceReport.Fixtures, "invalid_config", "pass") ||
		!hasFixture(conformanceReport.Fixtures, "missing_secret", "skip") {
		t.Fatalf("conformance report = %+v, want passing governance/config fixtures and skipped missing_secret", conformanceReport)
	}
	strictPreflightMissing := captureStdout(t, func() {
		handled, code := runPluginCLI([]string{"plugin", "preflight", dir, "--require-conformance-fixture"})
		if !handled || code != 0 {
			t.Fatalf("runPluginCLI(strict preflight missing fixture) = (%v, %d), want handled code 0", handled, code)
		}
	})
	var strictPreflightReport struct {
		Preflight struct {
			OK     bool             `json:"ok"`
			Checks []map[string]any `json:"checks"`
		} `json:"preflight"`
	}
	if err := json.Unmarshal([]byte(strictPreflightMissing), &strictPreflightReport); err != nil {
		t.Fatalf("Unmarshal(strict preflight missing fixture) error = %v\n%s", err, strictPreflightMissing)
	}
	if strictPreflightReport.Preflight.OK || !hasCheck(strictPreflightReport.Preflight.Checks, "conformance_fixture_missing", "blocking") {
		t.Fatalf("strict preflight missing = %+v, want conformance_fixture_missing", strictPreflightReport.Preflight)
	}

	if err := os.WriteFile(filepath.Join(dir, "conformance.json"), []byte(`{
		"fixtures":[{"name":"panic","status":"pass","expected":"recovered"}],
		"governance_gate_scenarios":["promotion_apply_gate"]
	}`), 0644); err != nil {
		t.Fatalf("WriteFile(conformance) error = %v", err)
	}
	conformanceWithFixture := captureStdout(t, func() {
		handled, code := runPluginCLI([]string{"plugin", "conformance", dir})
		if !handled || code != 0 {
			t.Fatalf("runPluginCLI(conformance fixture) = (%v, %d), want handled code 0", handled, code)
		}
	})
	var fixtureReport struct {
		OK            bool             `json:"ok"`
		FixtureSource string           `json:"fixture_source"`
		Fixtures      []map[string]any `json:"fixtures"`
	}
	if err := json.Unmarshal([]byte(conformanceWithFixture), &fixtureReport); err != nil {
		t.Fatalf("Unmarshal(conformance fixture) error = %v\n%s", err, conformanceWithFixture)
	}
	if !fixtureReport.OK ||
		fixtureReport.FixtureSource == "" ||
		!hasFixture(fixtureReport.Fixtures, "panic", "pass") ||
		!hasFixture(fixtureReport.Fixtures, "governance.promotion_apply_gate", "pass") {
		t.Fatalf("fixture report = %+v, want loaded golden fixtures", fixtureReport)
	}
	strictPreflightWithFixture := captureStdout(t, func() {
		handled, code := runPluginCLI([]string{"plugin", "preflight", dir, "--require-conformance-fixture"})
		if !handled || code != 0 {
			t.Fatalf("runPluginCLI(strict preflight with fixture) = (%v, %d), want handled code 0", handled, code)
		}
	})
	if err := json.Unmarshal([]byte(strictPreflightWithFixture), &strictPreflightReport); err != nil {
		t.Fatalf("Unmarshal(strict preflight with fixture) error = %v\n%s", err, strictPreflightWithFixture)
	}
	if !strictPreflightReport.Preflight.OK || !hasCheck(strictPreflightReport.Preflight.Checks, "conformance_fixture_passed", "info") {
		t.Fatalf("strict preflight with fixture = %+v, want conformance_fixture_passed", strictPreflightReport.Preflight)
	}

	sourcePackage := filepath.Join(t.TempDir(), "contract-plugin-source.mcgp")
	handled, code = runPluginCLI([]string{"plugin", "build", dir, "--type", "source", "--out", sourcePackage, "--skip-tests", "--vendor=false"})
	if !handled || code != 0 {
		t.Fatalf("runPluginCLI(build source with fixture) = (%v, %d), want handled code 0", handled, code)
	}
	packagedConformance := captureStdout(t, func() {
		handled, code := runPluginCLI([]string{"plugin", "conformance", sourcePackage})
		if !handled || code != 0 {
			t.Fatalf("runPluginCLI(conformance packaged fixture) = (%v, %d), want handled code 0", handled, code)
		}
	})
	if err := json.Unmarshal([]byte(packagedConformance), &fixtureReport); err != nil {
		t.Fatalf("Unmarshal(packaged conformance) error = %v\n%s", err, packagedConformance)
	}
	if !strings.Contains(fixtureReport.FixtureSource, ":conformance.json") ||
		!hasFixture(fixtureReport.Fixtures, "panic", "pass") ||
		!hasFixture(fixtureReport.Fixtures, "governance.promotion_apply_gate", "pass") {
		t.Fatalf("packaged fixture report = %+v, want source package fixture", fixtureReport)
	}
}

func TestPluginConformanceRequiredSecretFixture(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secret-conformance-plugin")
	handled, code := runPluginCLI([]string{
		"plugin", "init", dir,
		"--id", "secret-conformance-plugin",
		"--module", "example.com/secret-conformance-plugin",
		"--manifest-format", "json",
	})
	if !handled || code != 0 {
		t.Fatalf("runPluginCLI(init) = (%v, %d), want handled code 0", handled, code)
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(`{"schema_version":"mc-gateway.plugin/v1","id":"secret-conformance-plugin","name":"Secret Conformance Plugin","version":"0.1.0","artifact_type":"source","runtime":{"type":"go-plugin","entry":"plugin.so","entry_symbol":"Plugin"},"build":{"type":"go","entry":".","output":"plugin.so"},"api_version":"plugin-api/v1","sdk_module":"github.com/tursom/mc-gateway/plugin/api","sdk_module_version":"v0.1.0","extension_points":[{"type":"hook","key":"config.validate/v1"}],"capabilities":{"extension_points":["config.validate/v1"]},"secrets":[{"name":"api_token","required":true}],"runtime_limits":{"handler_timeout_ms":3000},"config_schema":{"type":"object","required":["host"],"properties":{"host":{"type":"string"},"api_secret_ref":{"type":"string"}}}}`), 0644); err != nil {
		t.Fatalf("WriteFile(manifest) error = %v", err)
	}
	output := captureStdout(t, func() {
		handled, code := runPluginCLI([]string{"plugin", "conformance", dir})
		if !handled || code != 0 {
			t.Fatalf("runPluginCLI(conformance) = (%v, %d), want handled code 0", handled, code)
		}
	})
	var report struct {
		OK       bool             `json:"ok"`
		Fixtures []map[string]any `json:"fixtures"`
	}
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatalf("Unmarshal(conformance) error = %v\n%s", err, output)
	}
	if !report.OK || !hasFixture(report.Fixtures, "invalid_config", "pass") || !hasFixture(report.Fixtures, "missing_secret", "pass") {
		t.Fatalf("conformance report = %+v, want invalid_config and missing_secret negative fixtures to pass", report)
	}
}

func TestPluginConformanceExecutesGovernanceReviewFixture(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "review-conformance-plugin")
	handled, code := runPluginCLI([]string{
		"plugin", "init", dir,
		"--id", "review-conformance-plugin",
		"--module", "example.com/review-conformance-plugin",
		"--manifest-format", "json",
	})
	if !handled || code != 0 {
		t.Fatalf("runPluginCLI(init) = (%v, %d), want handled code 0", handled, code)
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(`{"schema_version":"mc-gateway.plugin/v1","id":"review-conformance-plugin","name":"Review Conformance Plugin","version":"0.1.0","artifact_type":"source","runtime":{"type":"go-plugin","entry":"plugin.so","entry_symbol":"Plugin"},"build":{"type":"go","entry":".","output":"plugin.so"},"api_version":"plugin-api/v1","sdk_module":"github.com/tursom/mc-gateway/plugin/api","sdk_module_version":"v0.1.0","extension_points":[{"type":"hook","key":"upstream.connect/v1"}],"capabilities":{"upstream_connect":{"mode":"protocol-proxy"},"scope":{"type":"host","values":["play.example"]},"rollout":{"mode":"canary"},"minecraft":{"protocol_versions":{"tested":[767]},"forwarding":{"supported":["none"],"default":"none"}}},"runtime_limits":{"handler_timeout_ms":3000},"config_schema":{"type":"object"}}`), 0644); err != nil {
		t.Fatalf("WriteFile(manifest) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "conformance.json"), []byte(`{"governance_gate_scenarios":["review_required"]}`), 0644); err != nil {
		t.Fatalf("WriteFile(conformance) error = %v", err)
	}
	output := captureStdout(t, func() {
		handled, code := runPluginCLI([]string{"plugin", "conformance", dir, "--profile", pluginmanager.PolicyProfileProd})
		if !handled || code != 0 {
			t.Fatalf("runPluginCLI(conformance) = (%v, %d), want handled code 0", handled, code)
		}
	})
	var report struct {
		OK       bool             `json:"ok"`
		Fixtures []map[string]any `json:"fixtures"`
	}
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatalf("Unmarshal(conformance) error = %v\n%s", err, output)
	}
	if !report.OK || !hasFixture(report.Fixtures, "governance.review_required", "pass") {
		t.Fatalf("conformance report = %+v, want executable review_required fixture pass", report)
	}
	fixture, ok := findFixture(report.Fixtures, "governance.review_required")
	if !ok ||
		fixture["mode"] != "executable" ||
		fixture["actual_issue"] != "review_required" ||
		fixture["actual_ok"] != false {
		t.Fatalf("governance.review_required fixture = %+v, want executed blocking review gate", fixture)
	}
}

func TestPluginPromotionCommands(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "promotion-plugin")
	handled, code := runPluginCLI([]string{
		"plugin", "init", dir,
		"--id", "promotion-plugin",
		"--module", "example.com/promotion-plugin",
	})
	if !handled || code != 0 {
		t.Fatalf("runPluginCLI(init) = (%v, %d), want handled code 0", handled, code)
	}
	bundlePath := filepath.Join(t.TempDir(), "promotion-bundle.json")
	exportOutput := captureStdout(t, func() {
		handled, code = runPluginCLI([]string{"plugin", "export", dir, "--out", bundlePath, "--config-json", `{"upstream":"127.0.0.1:25566"}`})
		if !handled || code != 0 {
			t.Fatalf("runPluginCLI(export) = (%v, %d), want handled code 0", handled, code)
		}
	})
	var exportReport struct {
		Command  string `json:"command"`
		BundleID string `json:"bundle_id"`
	}
	if err := json.Unmarshal([]byte(exportOutput), &exportReport); err != nil {
		t.Fatalf("Unmarshal(export) error = %v\n%s", err, exportOutput)
	}
	if exportReport.Command != "promotion export" || exportReport.BundleID == "" {
		t.Fatalf("export report = %+v, want bundle id", exportReport)
	}
	data, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatalf("ReadFile(bundle) error = %v", err)
	}
	if strings.Contains(string(data), "127.0.0.1:25566") {
		t.Fatalf("promotion bundle leaked config value:\n%s", data)
	}

	importOutput := captureStdout(t, func() {
		handled, code = runPluginCLI([]string{"plugin", "import", bundlePath, "--dry-run"})
		if !handled || code != 0 {
			t.Fatalf("runPluginCLI(import) = (%v, %d), want handled code 0", handled, code)
		}
	})
	var importReport struct {
		Command string `json:"command"`
		DryRun  bool   `json:"dry_run"`
		Report  struct {
			OK bool `json:"ok"`
		} `json:"report"`
	}
	if err := json.Unmarshal([]byte(importOutput), &importReport); err != nil {
		t.Fatalf("Unmarshal(import) error = %v\n%s", err, importOutput)
	}
	if importReport.Command != "promotion import" || !importReport.DryRun || !importReport.Report.OK {
		t.Fatalf("import report = %+v, want dry-run ok", importReport)
	}

	diffOutput := captureStdout(t, func() {
		handled, code = runPluginCLI([]string{"plugin", "diff", bundlePath, "--target", dir})
		if !handled || code != 0 {
			t.Fatalf("runPluginCLI(diff) = (%v, %d), want handled code 0", handled, code)
		}
	})
	var diffReport struct {
		OK   bool              `json:"ok"`
		Diff []json.RawMessage `json:"diff"`
	}
	if err := json.Unmarshal([]byte(diffOutput), &diffReport); err != nil {
		t.Fatalf("Unmarshal(diff) error = %v\n%s", err, diffOutput)
	}
	if diffReport.OK || len(diffReport.Diff) == 0 {
		t.Fatalf("diff report = %+v, want config drift when target omits exported config hash", diffReport)
	}

	driftOutput := captureStdout(t, func() {
		handled, code = runPluginCLI([]string{"plugin", "drift", "--baseline", bundlePath, "--target", bundlePath})
		if !handled || code != 0 {
			t.Fatalf("runPluginCLI(drift) = (%v, %d), want handled code 0", handled, code)
		}
	})
	var driftReport struct {
		Report struct {
			OK bool `json:"ok"`
		} `json:"report"`
	}
	if err := json.Unmarshal([]byte(driftOutput), &driftReport); err != nil {
		t.Fatalf("Unmarshal(drift) error = %v\n%s", err, driftOutput)
	}
	if !driftReport.Report.OK {
		t.Fatalf("drift report = %+v, want no drift for identical bundle", driftReport)
	}

	drillOutput := captureStdout(t, func() {
		handled, code = runPluginCLI([]string{"plugin", "dr-drill", bundlePath})
		if !handled || code != 0 {
			t.Fatalf("runPluginCLI(dr-drill) = (%v, %d), want handled code 0", handled, code)
		}
	})
	var drillReport struct {
		Report struct {
			OK bool `json:"ok"`
		} `json:"report"`
	}
	if err := json.Unmarshal([]byte(drillOutput), &drillReport); err != nil {
		t.Fatalf("Unmarshal(dr-drill) error = %v\n%s", err, drillOutput)
	}
	if !drillReport.Report.OK {
		t.Fatalf("drill report = %+v, want dry drill ok", drillReport)
	}
}

func TestPluginSignVerifyCommand(t *testing.T) {
	dir := t.TempDir()
	artifact := filepath.Join(dir, "artifact.mcgp")
	data := []byte("signed artifact bytes")
	if err := os.WriteFile(artifact, data, 0644); err != nil {
		t.Fatalf("WriteFile(artifact) error = %v", err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	signature := ed25519.Sign(privateKey, data)
	signaturePath := filepath.Join(dir, "artifact.sig")
	publicKeyPath := filepath.Join(dir, "artifact.pub")
	if err := os.WriteFile(signaturePath, []byte(base64.StdEncoding.EncodeToString(signature)), 0644); err != nil {
		t.Fatalf("WriteFile(signature) error = %v", err)
	}
	if err := os.WriteFile(publicKeyPath, []byte(base64.StdEncoding.EncodeToString(publicKey)), 0644); err != nil {
		t.Fatalf("WriteFile(public key) error = %v", err)
	}
	output := captureStdout(t, func() {
		handled, code := runPluginCLI([]string{"plugin", "sign", "verify", artifact, "--signature", signaturePath, "--public-key", publicKeyPath})
		if !handled || code != 0 {
			t.Fatalf("runPluginCLI(sign verify) = (%v, %d), want handled code 0", handled, code)
		}
	})
	var report struct {
		Command  string `json:"command"`
		Verified bool   `json:"verified"`
	}
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatalf("Unmarshal(sign verify) error = %v\n%s", err, output)
	}
	if report.Command != "sign verify" || !report.Verified {
		t.Fatalf("sign verify report = %+v, want verified", report)
	}

	trustStorePath := filepath.Join(dir, "trust-store.json")
	rotationOutput := captureStdout(t, func() {
		handled, code := runPluginCLI([]string{
			"plugin", "sign", "key-rotation",
			"--trust-store", trustStorePath,
			"--key-id", "ci-key",
			"--public-key", publicKeyPath,
		})
		if !handled || code != 0 {
			t.Fatalf("runPluginCLI(sign key-rotation) = (%v, %d), want handled code 0", handled, code)
		}
	})
	var rotationReport struct {
		Command           string `json:"command"`
		KeyID             string `json:"key_id"`
		ActiveKeyCount    int    `json:"active_key_count"`
		TrustedKeyCount   int    `json:"trusted_key_count"`
		PublicKeySHA256   string `json:"public_key_sha256"`
		PreviousKeySHA256 string `json:"previous_key_sha256"`
	}
	if err := json.Unmarshal([]byte(rotationOutput), &rotationReport); err != nil {
		t.Fatalf("Unmarshal(sign key-rotation) error = %v\n%s", err, rotationOutput)
	}
	if rotationReport.Command != "sign key-rotation" || rotationReport.KeyID != "ci-key" || rotationReport.ActiveKeyCount != 1 || rotationReport.TrustedKeyCount != 1 || rotationReport.PublicKeySHA256 == "" || rotationReport.PreviousKeySHA256 != "" {
		t.Fatalf("sign key-rotation report = %+v, want ci-key active", rotationReport)
	}

	trustedOutput := captureStdout(t, func() {
		handled, code := runPluginCLI([]string{
			"plugin", "sign", "verify", artifact,
			"--signature", signaturePath,
			"--trust-store", trustStorePath,
			"--key-id", "ci-key",
		})
		if !handled || code != 0 {
			t.Fatalf("runPluginCLI(sign verify trust-store) = (%v, %d), want handled code 0", handled, code)
		}
	})
	var trustedReport struct {
		Command        string `json:"command"`
		KeyID          string `json:"key_id"`
		Verified       bool   `json:"verified"`
		SignatureValid bool   `json:"signature_valid"`
		Trusted        bool   `json:"trusted"`
		TrustStatus    string `json:"trust_status"`
		Revoked        bool   `json:"revoked"`
	}
	if err := json.Unmarshal([]byte(trustedOutput), &trustedReport); err != nil {
		t.Fatalf("Unmarshal(sign verify trust-store) error = %v\n%s", err, trustedOutput)
	}
	if trustedReport.Command != "sign verify" || trustedReport.KeyID != "ci-key" || !trustedReport.Verified || !trustedReport.SignatureValid || !trustedReport.Trusted || trustedReport.TrustStatus != "trusted" || trustedReport.Revoked {
		t.Fatalf("trust-store sign verify report = %+v, want trusted verified key", trustedReport)
	}

	revokeOutput := captureStdout(t, func() {
		handled, code := runPluginCLI([]string{
			"plugin", "sign", "revoke",
			"--trust-store", trustStorePath,
			"--key-id", "ci-key",
			"--reason", "compromised",
		})
		if !handled || code != 0 {
			t.Fatalf("runPluginCLI(sign revoke) = (%v, %d), want handled code 0", handled, code)
		}
	})
	var revokeReport struct {
		Command          string `json:"command"`
		KeyID            string `json:"key_id"`
		Revoked          bool   `json:"revoked"`
		RevocationReason string `json:"revocation_reason"`
		ActiveKeyCount   int    `json:"active_key_count"`
	}
	if err := json.Unmarshal([]byte(revokeOutput), &revokeReport); err != nil {
		t.Fatalf("Unmarshal(sign revoke) error = %v\n%s", err, revokeOutput)
	}
	if revokeReport.Command != "sign revoke" || revokeReport.KeyID != "ci-key" || !revokeReport.Revoked || revokeReport.RevocationReason != "compromised" || revokeReport.ActiveKeyCount != 0 {
		t.Fatalf("sign revoke report = %+v, want revoked inactive key", revokeReport)
	}

	revokedOutput := captureStdout(t, func() {
		handled, code := runPluginCLI([]string{
			"plugin", "sign", "verify", artifact,
			"--signature", signaturePath,
			"--trust-store", trustStorePath,
			"--key-id", "ci-key",
		})
		if !handled || code != 0 {
			t.Fatalf("runPluginCLI(sign verify revoked trust-store) = (%v, %d), want handled code 0", handled, code)
		}
	})
	var revokedReport struct {
		Verified         bool   `json:"verified"`
		SignatureValid   bool   `json:"signature_valid"`
		Trusted          bool   `json:"trusted"`
		TrustStatus      string `json:"trust_status"`
		Revoked          bool   `json:"revoked"`
		RevocationReason string `json:"revocation_reason"`
	}
	if err := json.Unmarshal([]byte(revokedOutput), &revokedReport); err != nil {
		t.Fatalf("Unmarshal(sign verify revoked trust-store) error = %v\n%s", err, revokedOutput)
	}
	if revokedReport.Verified || !revokedReport.SignatureValid || revokedReport.Trusted || revokedReport.TrustStatus != "revoked" || !revokedReport.Revoked || revokedReport.RevocationReason != "compromised" {
		t.Fatalf("revoked trust-store verify report = %+v, want valid signature but untrusted revoked key", revokedReport)
	}
}

func TestPluginSBOMGenerateCommand(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sbom-plugin")
	handled, code := runPluginCLI([]string{
		"plugin", "init", dir,
		"--id", "sbom-plugin",
		"--module", "example.com/sbom-plugin",
	})
	if !handled || code != 0 {
		t.Fatalf("runPluginCLI(init) = (%v, %d), want handled code 0", handled, code)
	}
	sbomPath := filepath.Join(t.TempDir(), "sbom.json")
	output := captureStdout(t, func() {
		handled, code = runPluginCLI([]string{"plugin", "sbom", "generate", dir, "--out", sbomPath})
		if !handled || code != 0 {
			t.Fatalf("runPluginCLI(sbom generate) = (%v, %d), want handled code 0", handled, code)
		}
	})
	var summary struct {
		Command      string `json:"command"`
		Out          string `json:"out"`
		Dependencies int    `json:"dependencies"`
	}
	if err := json.Unmarshal([]byte(output), &summary); err != nil {
		t.Fatalf("Unmarshal(sbom summary) error = %v\n%s", err, output)
	}
	if summary.Command != "sbom generate" || summary.Out != sbomPath || summary.Dependencies == 0 {
		t.Fatalf("sbom summary = %+v, want generated document with dependencies", summary)
	}
	data, err := os.ReadFile(sbomPath)
	if err != nil {
		t.Fatalf("ReadFile(sbom) error = %v", err)
	}
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("findRepoRoot() error = %v", err)
	}
	if strings.Contains(string(data), repoRoot) {
		t.Fatalf("sbom leaked local replace path %q:\n%s", repoRoot, data)
	}
	var doc struct {
		SchemaVersion string                 `json:"schema_version"`
		Dependencies  []pluginSBOMDependency `json:"dependencies"`
		Scan          map[string]string      `json:"scan"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("Unmarshal(sbom) error = %v\n%s", err, data)
	}
	if doc.SchemaVersion != "mc-gateway.sbom/v1" || len(doc.Dependencies) == 0 || doc.Scan["vulnerability_status"] != "not_scanned" {
		t.Fatalf("sbom doc = %+v, want schema, deps and truthful scan status", doc)
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
	requests := make(chan observedRemoteRequest, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want bearer token", got)
		}
		observed := observedRemoteRequest{
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
					var fileData bytes.Buffer
					_, _ = io.Copy(&fileData, file)
					observed.FileBytes = fileData.Bytes()
					_ = file.Close()
				}
			}
		}
		requests <- observed
		if r.Method == http.MethodGet && r.URL.Path == "/admin/api/plugin-artifacts/art-1/package" {
			w.Header().Set("Content-Disposition", `attachment; filename="source-art.mcgp"`)
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte("package-bytes"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/operations") {
			_, _ = io.WriteString(w, `{"operations":{"logs":[{"message":"ok"}],"traces":[],"events":[{"name":"evt"}],"event_queue":{"queued":1},"handlers":[{"plugin_id":"demo"}],"custom_metrics":[],"background_tasks":[{"id":"sync"}],"external_dependencies":[{"name":"session","circuit_state":"closed"}],"plugin_data":[{"key":"k"}],"plugin_files":[{"name":"f"}],"gc":[]}}`)
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
	if string(uploadReq.FileBytes) != "artifact" {
		t.Fatalf("upload bytes = %q, want artifact", uploadReq.FileBytes)
	}

	runRemotePluginCLI(t, "plugin", "transfer", "art-1", "--target-gateway", server.URL)
	downloadReq := <-requests
	assertRemoteRequest(t, downloadReq, http.MethodGet, "/admin/api/plugin-artifacts/art-1/package")
	transferReq := <-requests
	assertRemoteRequest(t, transferReq, http.MethodPost, "/admin/api/plugin-artifacts")
	if transferReq.FileName != "source-art.mcgp" || string(transferReq.FileBytes) != "package-bytes" {
		t.Fatalf("transfer upload = file %q bytes %q, want downloaded package", transferReq.FileName, transferReq.FileBytes)
	}

	bundlePath := filepath.Join(t.TempDir(), "promotion-bundle.json")
	bundle := pluginmanager.PromotionBundle{
		SchemaVersion: pluginmanager.SchemaVersion,
		APIVersion:    pluginmanager.APIVersion,
		BundleID:      "bundle-1",
		Profile:       pluginmanager.PolicyProfileProd,
		Plugins: []pluginmanager.PromotionPlugin{{
			PluginID:       "demo",
			ArtifactType:   pluginmanager.ArtifactTypeBinary,
			RuntimeType:    pluginmanager.RuntimeGoPlugin,
			ArtifactSHA256: "art-1",
			DesiredState:   pluginmanager.DesiredDisabled,
		}},
	}
	bundleBytes, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("Marshal(bundle) error = %v", err)
	}
	if err := os.WriteFile(bundlePath, bundleBytes, 0644); err != nil {
		t.Fatalf("WriteFile(bundle) error = %v", err)
	}
	runRemotePluginCLI(t, "plugin", "apply", bundlePath, "--config-json", `{"host":"target"}`, "--dry-run=false")
	promotionApplyReq := <-requests
	assertRemoteRequest(t, promotionApplyReq, http.MethodPost, "/admin/api/plugin-promotions")
	if promotionApplyReq.Body["action"] != "apply" ||
		promotionApplyReq.Body["plugin_id"] != "demo" ||
		promotionApplyReq.Body["config_json"] != `{"host":"target"}` ||
		promotionApplyReq.Body["dry_run"] != false {
		t.Fatalf("promotion apply body = %#v", promotionApplyReq.Body)
	}
	runRemotePluginCLI(t, "plugin", "apply", bundlePath, "--config-json", `{"host":"target"}`, "--target-gateway", server.URL, "--dry-run=false")
	crossDownloadReq := <-requests
	assertRemoteRequest(t, crossDownloadReq, http.MethodGet, "/admin/api/plugin-artifacts/art-1/package")
	crossTransferReq := <-requests
	assertRemoteRequest(t, crossTransferReq, http.MethodPost, "/admin/api/plugin-artifacts")
	if crossTransferReq.FileName != "source-art.mcgp" || string(crossTransferReq.FileBytes) != "package-bytes" {
		t.Fatalf("cross-gateway promotion transfer = file %q bytes %q, want downloaded package", crossTransferReq.FileName, crossTransferReq.FileBytes)
	}
	crossApplyReq := <-requests
	assertRemoteRequest(t, crossApplyReq, http.MethodPost, "/admin/api/plugin-promotions")
	if crossApplyReq.Body["action"] != "apply" ||
		crossApplyReq.Body["plugin_id"] != "demo" ||
		crossApplyReq.Body["config_json"] != `{"host":"target"}` ||
		crossApplyReq.Body["dry_run"] != false {
		t.Fatalf("cross-gateway promotion apply body = %#v", crossApplyReq.Body)
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
	runRemotePluginCLI(t, "plugin", "task", "cancel", "demo", "sync")
	assertRemoteRequest(t, <-requests, http.MethodPost, "/admin/api/plugins/demo/operations/tasks/sync/cancel")

	runRemotePluginCLI(t, "plugin", "external", "list", "demo")
	assertRemoteRequest(t, <-requests, http.MethodGet, "/admin/api/plugins/demo/operations")

	runRemotePluginCLI(t, "plugin", "external", "health-check", "demo", "session")
	assertRemoteRequest(t, <-requests, http.MethodPost, "/admin/api/plugins/demo/operations/external/session/health-check")

	runRemotePluginCLI(t, "plugin", "repo", "import", "demo", "--repository-type", "file", "--index", "repo.json", "--artifact", "candidate-1", "--version", "1.2.3")
	repoReq := <-requests
	assertRemoteRequest(t, repoReq, http.MethodPost, "/admin/api/plugin-repositories/imports")
	if repoReq.Body["plugin_id"] != "demo" || repoReq.Body["repository_type"] != "file" || repoReq.Body["artifact_id"] != "candidate-1" {
		t.Fatalf("repo body = %#v", repoReq.Body)
	}
	runRemotePluginCLI(t, "plugin", "repo", "apply", "--import-id", "42", "--config-json", `{"host":"target"}`, "--dry-run=false")
	repoApplyReq := <-requests
	assertRemoteRequest(t, repoApplyReq, http.MethodPost, "/admin/api/plugin-repositories/imports")
	if repoApplyReq.Body["action"] != "apply" ||
		int(repoApplyReq.Body["import_id"].(float64)) != 42 ||
		repoApplyReq.Body["desired_state"] != pluginmanager.DesiredDisabled ||
		repoApplyReq.Body["config_json"] != `{"host":"target"}` ||
		repoApplyReq.Body["dry_run"] != false {
		t.Fatalf("repo apply body = %#v", repoApplyReq.Body)
	}
	runRemotePluginCLI(t, "plugin", "repo", "updates", "demo", "--repository-type", "file", "--index", "repo.json", "--artifact", "candidate-2")
	repoUpdatesReq := <-requests
	assertRemoteRequest(t, repoUpdatesReq, http.MethodPost, "/admin/api/plugin-repositories/imports")
	if repoUpdatesReq.Body["action"] != "updates" ||
		repoUpdatesReq.Body["plugin_id"] != "demo" ||
		repoUpdatesReq.Body["repository_type"] != "file" ||
		repoUpdatesReq.Body["index_path"] != "repo.json" ||
		repoUpdatesReq.Body["artifact_id"] != "candidate-2" {
		t.Fatalf("repo updates body = %#v", repoUpdatesReq.Body)
	}
	repoIndex := filepath.Join(t.TempDir(), "repo-index.json")
	if err := os.WriteFile(repoIndex, []byte(`{"name":"local","artifacts":[{"id":"candidate-1","plugin_id":"demo","version":"1.2.3","artifact_path":"demo.mcgp"}]}`), 0644); err != nil {
		t.Fatalf("WriteFile(repo index) error = %v", err)
	}
	repoSearchOutput := captureStdout(t, func() {
		runRemotePluginCLI(t, "plugin", "repo", "search", "demo", "--index", repoIndex)
	})
	if !strings.Contains(repoSearchOutput, `"candidate-1"`) {
		t.Fatalf("repo search output = %s, want candidate-1", repoSearchOutput)
	}
	repoShowOutput := captureStdout(t, func() {
		runRemotePluginCLI(t, "plugin", "repo", "show", "candidate-1", "--index", repoIndex)
	})
	if !strings.Contains(repoShowOutput, `"plugin_id": "demo"`) {
		t.Fatalf("repo show output = %s, want demo candidate", repoShowOutput)
	}
	repoIndexServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/index.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"url-local","artifacts":[{"id":"candidate-url","plugin_id":"demo","version":"1.2.4","artifact_path":"demo-url.mcgp","sha256":"abc"}]}`)
	}))
	defer repoIndexServer.Close()
	repoURLSearchOutput := captureStdout(t, func() {
		runRemotePluginCLI(t, "plugin", "repo", "search", "demo", "--index", repoIndexServer.URL+"/index.json")
	})
	if !strings.Contains(repoURLSearchOutput, `"candidate-url"`) || !strings.Contains(repoURLSearchOutput, `"url-local"`) {
		t.Fatalf("repo url search output = %s, want url-local candidate", repoURLSearchOutput)
	}

	runRemotePluginCLI(t, "plugin", "review", "status", "demo", "--artifact", "art-1", "--profile", "prod")
	assertRemoteRequest(t, <-requests, http.MethodGet, "/admin/api/plugins/demo/governance?artifact_id=art-1&profile=prod")

	runRemotePluginCLI(t, "plugin", "sbom", "verify", "demo", "--artifact", "art-1", "--metadata-json", `{"sbom":{"format":"spdx"}}`)
	supplyReq := <-requests
	assertRemoteRequest(t, supplyReq, http.MethodPost, "/admin/api/plugin-supply-chain")
	if supplyReq.Body["plugin_id"] != "demo" || supplyReq.Body["artifact_id"] != "art-1" {
		t.Fatalf("supply-chain body = %#v", supplyReq.Body)
	}

	runRemotePluginCLI(t, "plugin", "advisory", "import", "--metadata-json", `{"source":"unit-feed","advisories":[{"advisory_id":"MCG-FEED","action":"denylist","plugin_id":"demo","version_range":"*"}]}`)
	advisoryFeedReq := <-requests
	assertRemoteRequest(t, advisoryFeedReq, http.MethodPost, "/admin/api/plugin-advisories")
	if advisoryFeedReq.Body["source"] != "unit-feed" || len(advisoryFeedReq.Body["advisories"].([]any)) != 1 {
		t.Fatalf("advisory feed body = %#v", advisoryFeedReq.Body)
	}

	runRemotePluginCLI(t, "plugin", "advisory", "sync", "--feed-url", "https://feeds.example.test/advisories.json", "--feed-source", "unit-http-feed")
	advisorySyncReq := <-requests
	assertRemoteRequest(t, advisorySyncReq, http.MethodPost, "/admin/api/plugin-advisories")
	if advisorySyncReq.Body["feed_url"] != "https://feeds.example.test/advisories.json" || advisorySyncReq.Body["source"] != "unit-http-feed" {
		t.Fatalf("advisory sync body = %#v", advisorySyncReq.Body)
	}

	runRemotePluginCLI(t, "plugin", "advisory", "rescan", "demo", "--artifact", "art-1")
	advisoryRescanReq := <-requests
	assertRemoteRequest(t, advisoryRescanReq, http.MethodPost, "/admin/api/plugin-advisories")
	if advisoryRescanReq.Body["rescan"] != true || advisoryRescanReq.Body["plugin_id"] != "demo" || advisoryRescanReq.Body["artifact_id"] != "art-1" {
		t.Fatalf("advisory rescan body = %#v", advisoryRescanReq.Body)
	}

	runRemotePluginCLI(t, "plugin", "vulnerability", "import", "--metadata-json", `{"source":"unit-vuln-db","vulnerabilities":[{"vulnerability_id":"CVE-UNIT","action":"denylist","package_name":"example.com/bad","version_range":"<2.0.0"}]}`)
	vulnerabilityImportReq := <-requests
	assertRemoteRequest(t, vulnerabilityImportReq, http.MethodPost, "/admin/api/plugin-vulnerabilities")
	if vulnerabilityImportReq.Body["source"] != "unit-vuln-db" || len(vulnerabilityImportReq.Body["vulnerabilities"].([]any)) != 1 {
		t.Fatalf("vulnerability import body = %#v", vulnerabilityImportReq.Body)
	}

	runRemotePluginCLI(t, "plugin", "vulnerability", "sync", "--feed-url", "https://feeds.example.test/vulnerabilities.json", "--feed-source", "unit-vuln-http")
	vulnerabilitySyncReq := <-requests
	assertRemoteRequest(t, vulnerabilitySyncReq, http.MethodPost, "/admin/api/plugin-vulnerabilities")
	if vulnerabilitySyncReq.Body["feed_url"] != "https://feeds.example.test/vulnerabilities.json" || vulnerabilitySyncReq.Body["source"] != "unit-vuln-http" {
		t.Fatalf("vulnerability sync body = %#v", vulnerabilitySyncReq.Body)
	}

	runRemotePluginCLI(t, "plugin", "vulnerability", "rescan", "demo", "--artifact", "art-1")
	vulnerabilityRescanReq := <-requests
	assertRemoteRequest(t, vulnerabilityRescanReq, http.MethodPost, "/admin/api/plugin-vulnerabilities")
	if vulnerabilityRescanReq.Body["rescan"] != true || vulnerabilityRescanReq.Body["plugin_id"] != "demo" || vulnerabilityRescanReq.Body["artifact_id"] != "art-1" {
		t.Fatalf("vulnerability rescan body = %#v", vulnerabilityRescanReq.Body)
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

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe() error = %v", err)
	}
	os.Stdout = writer
	defer func() {
		os.Stdout = old
		_ = writer.Close()
		_ = reader.Close()
	}()
	fn()
	if err := writer.Close(); err != nil {
		t.Fatalf("Close(stdout pipe writer) error = %v", err)
	}
	os.Stdout = old
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll(stdout pipe) error = %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close(stdout pipe reader) error = %v", err)
	}
	return string(data)
}

func findRuntimeFeature(features []pluginmanager.RuntimeFeature, runtimeType string) pluginmanager.RuntimeFeature {
	for _, feature := range features {
		if feature.Type == runtimeType {
			return feature
		}
	}
	return pluginmanager.RuntimeFeature{}
}

func findCLIServiceModeFeature(features []pluginmanager.PluginServiceModeFeature, mode string) pluginmanager.PluginServiceModeFeature {
	for _, feature := range features {
		if feature.Mode == mode {
			return feature
		}
	}
	return pluginmanager.PluginServiceModeFeature{}
}

func findRuntimeAdapterStatus(statuses []pluginmanager.RuntimeAdapterFactoryStatus, mode, runtimeType string) pluginmanager.RuntimeAdapterFactoryStatus {
	for _, status := range statuses {
		if status.ServiceMode == mode && status.RuntimeType == runtimeType {
			return status
		}
	}
	return pluginmanager.RuntimeAdapterFactoryStatus{}
}

func findExtensionPointFeature(features []pluginmanager.ExtensionPointFeature, key string) pluginmanager.ExtensionPointFeature {
	for _, feature := range features {
		if feature.Key == key {
			return feature
		}
	}
	return pluginmanager.ExtensionPointFeature{}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsAnyString(values []any, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func assertSchemaProperties(t *testing.T, defs map[string]any, name string, keys ...string) {
	t.Helper()
	def, ok := defs[name].(map[string]any)
	if !ok {
		t.Fatalf("schema def %s = %#v, want object", name, defs[name])
	}
	properties, ok := def["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema def %s properties = %#v, want object", name, def["properties"])
	}
	for _, key := range keys {
		if properties[key] == nil {
			t.Fatalf("schema def %s properties = %+v, missing %s", name, properties, key)
		}
	}
}

func hasFixture(fixtures []map[string]any, name, status string) bool {
	for _, fixture := range fixtures {
		if fixture["name"] == name && fixture["status"] == status {
			return true
		}
	}
	return false
}

func findFixture(fixtures []map[string]any, name string) (map[string]any, bool) {
	for _, fixture := range fixtures {
		if fixture["name"] == name {
			return fixture, true
		}
	}
	return nil, false
}

func hasCheck(checks []map[string]any, code, severity string) bool {
	for _, check := range checks {
		if check["code"] == code && check["severity"] == severity {
			return true
		}
	}
	return false
}

type observedRemoteRequest struct {
	Method      string
	RequestURI  string
	ContentType string
	Body        map[string]any
	FileName    string
	FileBytes   []byte
}

func assertRemoteRequest(t *testing.T, got observedRemoteRequest, wantMethod, wantURI string) {
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
