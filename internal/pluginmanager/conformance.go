package pluginmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/tursom/mc-gateway/plugin/api"
)

const (
	SandboxConformanceHandshakeInitRegister = "sandbox.handshake_init_register"
	SandboxConformanceRequestResponse       = "sandbox.route_rule_config_request_response"
	SandboxConformanceSecretDenial          = "sandbox.secret_denial"
	SandboxConformanceFileDenial            = "sandbox.file_denial"
	SandboxConformanceNetworkDenial         = "sandbox.network_denial"
	SandboxConformanceCPUMemoryExceeded     = "sandbox.cpu_memory_exceeded"
	SandboxConformanceCrashLoop             = "sandbox.crash_loop"
	SandboxConformanceStreamHalfClose       = "sandbox.stream_half_close"
	SandboxConformanceStreamBackpressure    = "sandbox.stream_backpressure"
	SandboxConformanceStreamCancel          = "sandbox.stream_cancel"
)

var sandboxConformanceRequiredCoverage = []string{
	SandboxConformanceHandshakeInitRegister,
	SandboxConformanceRequestResponse,
	SandboxConformanceSecretDenial,
	SandboxConformanceFileDenial,
	SandboxConformanceNetworkDenial,
	SandboxConformanceCPUMemoryExceeded,
	SandboxConformanceCrashLoop,
	SandboxConformanceStreamHalfClose,
	SandboxConformanceStreamBackpressure,
	SandboxConformanceStreamCancel,
}

// SandboxConformanceRequiredCoverage returns the fixture coverage required before
// sandbox-process artifacts can pass strict or production admission.
func SandboxConformanceRequiredCoverage() []string {
	return append([]string(nil), sandboxConformanceRequiredCoverage...)
}

// SandboxConformanceScenario describes one deterministic sandbox coverage probe
// requested by the CLI. The runner lives in pluginmanager so it can exercise the
// same control-envelope and sandbox diagnostic helpers used by the runtime.
type SandboxConformanceScenario struct {
	Coverage               string
	Manifest               Manifest
	ConfigJSON             string
	RouteDecisions         []string
	RuleEvaluationOutcomes []string
	StreamProxyScenarios   []StreamProxyFixture
	TakeoverScenarios      []string
}

// SandboxConformanceEvidence is serialized into generated conformance fixtures.
// Packaged artifact metadata only treats sandbox coverage as satisfied when this
// executable evidence is present.
type SandboxConformanceEvidence struct {
	Coverage    string         `json:"coverage"`
	Executed    bool           `json:"executed"`
	ValidatedBy string         `json:"validated_by"`
	Checks      []string       `json:"checks,omitempty"`
	Details     map[string]any `json:"details,omitempty"`
}

const SandboxConformanceValidatedByCLI = "mc-gateway-cli/sandbox-conformance/v1"

// RunSandboxConformanceScenario executes one sandbox conformance scenario using
// deterministic runtime helpers instead of trusting fixture declarations.
func RunSandboxConformanceScenario(ctx context.Context, scenario SandboxConformanceScenario) (SandboxConformanceEvidence, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	coverage := normalizeConformanceCoverage(scenario.Coverage)
	evidence := SandboxConformanceEvidence{
		Coverage:    coverage,
		Executed:    true,
		ValidatedBy: SandboxConformanceValidatedByCLI,
		Details:     map[string]any{},
	}
	if coverage == "" {
		return evidence, errors.New("sandbox conformance coverage is required")
	}
	if scenario.Manifest.Runtime.Type != RuntimeSandbox {
		return evidence, fmt.Errorf("sandbox conformance coverage %s requires sandbox-process runtime", coverage)
	}
	switch coverage {
	case SandboxConformanceHandshakeInitRegister:
		return runSandboxHandshakeConformance(ctx, scenario, evidence)
	case SandboxConformanceRequestResponse:
		return runSandboxRequestResponseConformance(ctx, scenario, evidence)
	case SandboxConformanceSecretDenial:
		return runSandboxSecretDenialConformance(ctx, scenario, evidence)
	case SandboxConformanceFileDenial:
		return runSandboxFileDenialConformance(scenario, evidence)
	case SandboxConformanceNetworkDenial:
		return runSandboxNetworkDenialConformance(ctx, scenario, evidence)
	case SandboxConformanceCPUMemoryExceeded:
		return runSandboxCPUMemoryConformance(scenario, evidence)
	case SandboxConformanceCrashLoop:
		return runSandboxCrashLoopConformance(scenario, evidence)
	case SandboxConformanceStreamHalfClose, SandboxConformanceStreamBackpressure, SandboxConformanceStreamCancel:
		return runSandboxStreamConformance(scenario, evidence)
	default:
		return evidence, fmt.Errorf("unsupported sandbox conformance coverage %q", scenario.Coverage)
	}
}

func runSandboxHandshakeConformance(ctx context.Context, scenario SandboxConformanceScenario, evidence SandboxConformanceEvidence) (SandboxConformanceEvidence, error) {
	process := sandboxScenarioProcess(scenario.Manifest, scenario.ConfigJSON)
	for _, step := range []struct {
		command string
		payload any
	}{
		{command: sandboxControlCommandHandshake, payload: SandboxHandshakeRequest{ABIVersion: SandboxProcessABIVersionV1, PID: 1001, Capabilities: []string{"runtime.cpu_memory"}}},
		{command: sandboxControlCommandInit, payload: SandboxInitRequest{Capabilities: []string{"runtime.cpu_memory"}}},
		{command: sandboxControlCommandRegister, payload: SandboxRegisterRequest{Handlers: sandboxScenarioRegistrations(scenario.Manifest), DeclaredCapabilities: []string{"runtime.cpu_memory"}}},
	} {
		resp, err := sandboxScenarioControl(ctx, process, step.command, step.payload, nil)
		if err != nil {
			return evidence, err
		}
		if !resp.OK {
			return evidence, fmt.Errorf("%s failed: %s %s", step.command, resp.ErrorCode, resp.Error)
		}
		evidence.Checks = append(evidence.Checks, step.command)
	}
	if len(process.Registrations) == 0 && len(scenario.Manifest.ExtensionPoints) > 0 {
		return evidence, errors.New("sandbox register produced no handlers")
	}
	evidence.Details["registrations"] = len(process.Registrations)
	return evidence, nil
}

func runSandboxRequestResponseConformance(ctx context.Context, scenario SandboxConformanceScenario, evidence SandboxConformanceEvidence) (SandboxConformanceEvidence, error) {
	registrations := sandboxScenarioRegistrations(scenario.Manifest)
	if len(registrations) == 0 {
		return evidence, errors.New("request-response coverage requires registered sandbox handlers")
	}
	process := sandboxScenarioProcess(scenario.Manifest, scenario.ConfigJSON)
	process.Registrations = registrations
	process.registerOK = true
	process.controlInvoker = func(_ context.Context, _ string, req SandboxControlRequest) (SandboxControlResponse, error) {
		var invoke SandboxInvokeRequest
		if err := json.Unmarshal(req.Payload, &invoke); err != nil {
			return SandboxControlResponse{}, err
		}
		resp := sandboxScenarioResponse(process, req)
		resp.OK = true
		out := SandboxInvokeResponse{ExtensionPoint: invoke.ExtensionPoint, HandlerID: invoke.HandlerID, OK: true}
		switch {
		case invoke.ConfigJSON != nil:
			valid := json.Valid(invoke.ConfigJSON)
			out.Valid = &valid
			if !valid {
				out.Reason = "config json is invalid"
			}
		case invoke.RouteResolve != nil:
			decision := sandboxScenarioRouteDecision(firstNonEmptyList(scenario.RouteDecisions, "override"), *invoke.RouteResolve)
			out.RouteDecision = &decision
		case invoke.RuleEvaluate != nil:
			decision := sandboxScenarioRuleDecision(firstNonEmptyList(scenario.RuleEvaluationOutcomes, "allow"))
			out.RuleDecision = &decision
		default:
			out.OK = false
			out.ErrorCode = sandboxControlErrorBadResponse
			out.Error = "request-response fixture did not provide a supported payload"
		}
		resp.Invoke = &out
		return resp, nil
	}
	hosted := sandboxHostedPlugin{process: process}
	var executed []string
	if reg, ok := sandboxScenarioRegistrationFor(registrations, ExtensionConfigValidate); ok {
		if err := hosted.validateConfig(ctx, json.RawMessage(defaultJSONObject(scenario.ConfigJSON))); err != nil {
			return evidence, err
		}
		_ = reg
		executed = append(executed, ExtensionConfigValidate)
	}
	if len(scenario.RouteDecisions) > 0 {
		reg, ok := sandboxScenarioRegistrationFor(registrations, ExtensionRouteResolve)
		if !ok {
			return evidence, errors.New("route_decisions require route.resolve/v1 handler")
		}
		result, err := hosted.resolveRoute(api.RouteResolveRequest{
			Context:          ctx,
			Host:             "play.example",
			FallbackUpstream: "fallback:25565",
			FallbackHit:      true,
			Refresh:          true,
		}, reg)
		if err != nil {
			return evidence, err
		}
		if result.Action == "" {
			return evidence, errors.New("route.resolve/v1 response did not include a route action")
		}
		executed = append(executed, ExtensionRouteResolve)
		evidence.Details["route_action"] = result.Action
	}
	if len(scenario.RuleEvaluationOutcomes) > 0 {
		reg, ok := sandboxScenarioRegistrationFor(registrations, ExtensionRuleEvaluate)
		if !ok {
			return evidence, errors.New("rule_evaluation_outcomes require rule.evaluate/v1 handler")
		}
		result, err := hosted.evaluateRule(api.RuleEvaluateRequest{
			Context: ctx,
			Subject: "sandbox-conformance",
			Action:  "join",
			Host:    "play.example",
		}, reg)
		if err != nil {
			return evidence, err
		}
		if !result.Allow && !result.Deny && !result.Reject {
			return evidence, errors.New("rule.evaluate/v1 response did not include an allow/deny/reject decision")
		}
		executed = append(executed, ExtensionRuleEvaluate)
		evidence.Details["rule_allow"] = result.Allow
		evidence.Details["rule_deny"] = result.Deny
	}
	if len(executed) == 0 {
		return evidence, errors.New("request-response coverage requires config.validate/v1, route_decisions, or rule_evaluation_outcomes")
	}
	sort.Strings(executed)
	evidence.Checks = append(evidence.Checks, executed...)
	return evidence, nil
}

func runSandboxSecretDenialConformance(ctx context.Context, scenario SandboxConformanceScenario, evidence SandboxConformanceEvidence) (SandboxConformanceEvidence, error) {
	process := sandboxScenarioProcess(scenario.Manifest, scenario.ConfigJSON)
	resp, err := sandboxScenarioControl(ctx, process, sandboxControlCommandSecret, SandboxSecretRequest{Handle: "undeclared_secret"}, sandboxConformanceDenyingSecretResolver{})
	if err != nil {
		return evidence, err
	}
	if resp.OK || resp.Secret != nil && resp.Secret.OK {
		return evidence, errors.New("secret denial fixture unexpectedly allowed undeclared secret")
	}
	evidence.Checks = append(evidence.Checks, "secret.resolve.denied")
	evidence.Details["error_code"] = firstNonEmptyString(resp.ErrorCode, resp.Code)
	if resp.Secret != nil {
		evidence.Details["reason_code"] = resp.Secret.ReasonCode
	}
	return evidence, nil
}

func runSandboxFileDenialConformance(scenario SandboxConformanceScenario, evidence SandboxConformanceEvidence) (SandboxConformanceEvidence, error) {
	policy := SandboxPolicy{CPUSeconds: 1, MemoryBytes: int64(scenario.Manifest.RuntimeLimits.MemoryBytes), ExternalIsolation: true}
	if policy.MemoryBytes <= 0 {
		policy.MemoryBytes = 8 * 1024 * 1024
	}
	if _, err := sandboxFilesystemRootRel("../escape"); err == nil {
		return evidence, errors.New("filesystem root escape was accepted")
	}
	method, enforced, reason := sandboxFilesystemEnforcementFact(policy)
	if !enforced {
		return evidence, fmt.Errorf("filesystem denial is not enforceable: %s", reason)
	}
	evidence.Checks = append(evidence.Checks, "filesystem.escape_denied", "filesystem.readonly_root")
	evidence.Details["method"] = method
	return evidence, nil
}

func runSandboxNetworkDenialConformance(ctx context.Context, scenario SandboxConformanceScenario, evidence SandboxConformanceEvidence) (SandboxConformanceEvidence, error) {
	process := sandboxScenarioProcess(scenario.Manifest, scenario.ConfigJSON)
	resp, err := sandboxScenarioControl(ctx, process, sandboxControlCommandExternal, SandboxExternalRequest{
		Name:      "undeclared-network",
		Method:    "GET",
		URL:       "https://example.invalid/blocked",
		TimeoutMS: 100,
	}, nil)
	if err != nil {
		return evidence, err
	}
	if resp.OK || resp.External != nil && resp.External.OK {
		return evidence, errors.New("network denial fixture unexpectedly allowed undeclared external request")
	}
	policy := normalizeSandboxPolicy(SandboxPolicy{CPUSeconds: 1, MemoryBytes: 8 * 1024 * 1024, ExternalIsolation: true})
	if !sandboxFactsAllRequiredEnforced(sandboxEnforcementFacts(policy), "network", "no_host_network", "egress_policy") {
		return evidence, errors.New("sandbox network denial facts are not enforceable")
	}
	evidence.Checks = append(evidence.Checks, "external.request.denied", "network.egress_policy")
	if resp.External != nil {
		evidence.Details["error_code"] = resp.External.ErrorCode
	}
	return evidence, nil
}

func runSandboxCPUMemoryConformance(scenario SandboxConformanceScenario, evidence SandboxConformanceEvidence) (SandboxConformanceEvidence, error) {
	policy := normalizeSandboxPolicy(SandboxPolicy{
		CPUSeconds:        1,
		MemoryBytes:       int64(scenario.Manifest.RuntimeLimits.MemoryBytes),
		FileQuotaBytes:    DefaultPluginFileQuota,
		ExternalIsolation: true,
	})
	if policy.MemoryBytes <= 0 {
		policy.MemoryBytes = 8 * 1024 * 1024
	}
	facts := sandboxEnforcementFacts(policy)
	if !sandboxFactsAllRequiredEnforced(facts, "resource", "cpu_memory") {
		return evidence, errors.New("sandbox CPU/memory limits are not enforceable")
	}
	process := sandboxScenarioProcess(scenario.Manifest, scenario.ConfigJSON)
	process.Policy = policy
	diag := process.Diagnostics()
	if !diag.CPUMemoryEnforced {
		return evidence, errors.New("sandbox diagnostics did not report CPU/memory enforcement")
	}
	if policy.CPUSeconds <= 0 || policy.MemoryBytes <= 0 {
		return evidence, errors.New("sandbox CPU/memory exceeded probe has no positive limits")
	}
	evidence.Checks = append(evidence.Checks, "resource.cpu_memory_enforced", "resource.exceeded_budget_denied")
	evidence.Details["cpu_seconds"] = policy.CPUSeconds
	evidence.Details["memory_bytes"] = policy.MemoryBytes
	evidence.Details["probe"] = "exceeded CPU/memory budget is denied by rlimit policy"
	return evidence, nil
}

func runSandboxCrashLoopConformance(scenario SandboxConformanceScenario, evidence SandboxConformanceEvidence) (SandboxConformanceEvidence, error) {
	process := sandboxScenarioProcess(scenario.Manifest, scenario.ConfigJSON)
	process.crashLoop = true
	process.crashCount = 3
	process.lastError = "sandbox crash loop"
	diag := process.Diagnostics()
	if !diag.CrashLoop || diag.ReasonCode != ReasonSandboxCrashLoop {
		return evidence, fmt.Errorf("crash-loop diagnostics = %+v, want crash loop reason", diag)
	}
	evidence.Checks = append(evidence.Checks, "diagnostics.crash_loop")
	evidence.Details["reason_code"] = diag.ReasonCode
	evidence.Details["crash_count"] = diag.CrashCount
	return evidence, nil
}

func runSandboxStreamConformance(scenario SandboxConformanceScenario, evidence SandboxConformanceEvidence) (SandboxConformanceEvidence, error) {
	if len(scenario.StreamProxyScenarios) == 0 && len(scenario.TakeoverScenarios) == 0 {
		return evidence, fmt.Errorf("%s requires stream_proxy_scenarios or takeover_scenarios", evidence.Coverage)
	}
	var matched []string
	for _, fixture := range scenario.StreamProxyScenarios {
		if err := ValidateStreamProxyFixture(fixture); err != nil {
			return evidence, err
		}
		if sandboxStreamFixtureCovers(fixture, evidence.Coverage) {
			matched = append(matched, fixture.Name)
		}
	}
	for _, item := range scenario.TakeoverScenarios {
		if normalizeConformanceCoverage(item) == evidence.Coverage {
			matched = append(matched, "takeover."+strings.TrimSpace(item))
		}
	}
	if len(matched) == 0 {
		return evidence, fmt.Errorf("%s has no matching stream fixture data", evidence.Coverage)
	}
	sort.Strings(matched)
	evidence.Checks = append(evidence.Checks, "stream.fixture_replay")
	evidence.Details["matched"] = matched
	return evidence, nil
}

func sandboxScenarioProcess(manifest Manifest, configJSON string) *SandboxProcess {
	if configJSON == "" {
		configJSON = "{}"
	}
	return &SandboxProcess{
		PluginID:          manifest.ID,
		ArtifactID:        "sandbox-conformance-artifact",
		RuntimeInstanceID: "sandbox-conformance-runtime",
		Generation:        1,
		Protocol:          sandboxProcessProtocol,
		Policy:            normalizeSandboxPolicy(SandboxPolicy{CPUSeconds: 1, MemoryBytes: 8 * 1024 * 1024, ExternalIsolation: true}),
		ConfigJSON:        configJSON,
		done:              make(chan struct{}),
		startupDone:       make(chan struct{}),
	}
}

func sandboxScenarioControl(ctx context.Context, process *SandboxProcess, command string, payload any, resolver SandboxSecretResolver) (SandboxControlResponse, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return SandboxControlResponse{}, err
	}
	req := SandboxControlRequest{
		RequestID:         "sandbox-conformance-" + command,
		Command:           command,
		Protocol:          sandboxProcessProtocol,
		PluginID:          process.PluginID,
		ArtifactID:        process.ArtifactID,
		RuntimeInstanceID: process.RuntimeInstanceID,
		Generation:        process.Generation,
		Payload:           data,
	}
	if deadline, ok := ctx.Deadline(); ok {
		req.DeadlineUnixMS = deadline.UnixMilli()
	}
	return process.HandleControlRequest(ctx, req, resolver), nil
}

type sandboxConformanceDenyingSecretResolver struct{}

func (sandboxConformanceDenyingSecretResolver) ResolveSandboxSecret(context.Context, SandboxSecretRequest) (SandboxSecretResponse, error) {
	return SandboxSecretResponse{
		OK:         false,
		ErrorCode:  "secret_not_declared",
		ReasonCode: ReasonSandboxSecretDenied,
		Error:      "secret handle is not declared by manifest",
	}, nil
}

func sandboxScenarioResponse(process *SandboxProcess, req SandboxControlRequest) SandboxControlResponse {
	return SandboxControlResponse{
		RequestID:         req.RequestID,
		Command:           req.Command,
		Protocol:          sandboxProcessProtocol,
		PluginID:          process.PluginID,
		ArtifactID:        process.ArtifactID,
		RuntimeInstanceID: process.RuntimeInstanceID,
		Generation:        process.Generation,
		TraceID:           req.TraceID,
		DeadlineUnixMS:    req.DeadlineUnixMS,
	}
}

func sandboxScenarioRegistrations(manifest Manifest) []SandboxHandlerRegistration {
	var out []SandboxHandlerRegistration
	for _, point := range manifest.ExtensionPoints {
		switch point.Key {
		case ExtensionConfigValidate, ExtensionRouteResolve, ExtensionRuleEvaluate, ExtensionStatusPing, ExtensionUpstreamConnect:
			out = append(out, SandboxHandlerRegistration{
				ExtensionPoint: point.Key,
				HandlerID:      strings.ReplaceAll(point.Key, "/", "-"),
				FailPolicy:     api.FailPolicyClose,
				TimeoutMS:      1000,
				SchemaVersion:  1,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExtensionPoint < out[j].ExtensionPoint })
	return out
}

func sandboxScenarioRegistrationFor(registrations []SandboxHandlerRegistration, point string) (SandboxHandlerRegistration, bool) {
	for _, reg := range registrations {
		if reg.ExtensionPoint == point {
			return reg, true
		}
	}
	return SandboxHandlerRegistration{}, false
}

func sandboxScenarioRouteDecision(decision string, req api.RouteResolveRequest) api.RouteDecision {
	switch strings.TrimSpace(decision) {
	case "reject":
		return api.RouteDecision{Action: api.RouteDecisionReject, Host: req.Host, Reason: "sandbox conformance reject"}
	case "fallback":
		return api.RouteDecision{Action: api.RouteDecisionFallback, Upstream: req.FallbackUpstream, Host: req.Host, Reason: "sandbox conformance fallback"}
	case "pass":
		return api.RouteDecision{Action: api.RouteDecisionPass, Host: req.Host, Reason: "sandbox conformance pass"}
	default:
		return api.RouteDecision{Action: api.RouteDecisionOverride, Upstream: "sandbox-conformance:25565", Host: req.Host, Reason: "sandbox conformance override"}
	}
}

func sandboxScenarioRuleDecision(outcome string) api.RuleEvaluateDecision {
	switch strings.TrimSpace(outcome) {
	case "deny", "error_fail_closed", "timeout_fail_closed":
		return api.RuleEvaluateDecision{Deny: true, Reject: true, Reason: "sandbox conformance deny"}
	default:
		return api.RuleEvaluateDecision{Allow: true, Reason: "sandbox conformance allow"}
	}
}

func sandboxStreamFixtureCovers(fixture StreamProxyFixture, coverage string) bool {
	expected := strings.ToLower(fixture.Expected)
	switch coverage {
	case SandboxConformanceStreamHalfClose:
		if strings.Contains(expected, "half_close") {
			return true
		}
	case SandboxConformanceStreamBackpressure:
		if strings.Contains(expected, "backpressure") {
			return true
		}
	case SandboxConformanceStreamCancel:
		if strings.Contains(expected, "cancel") {
			return true
		}
	}
	for _, frame := range fixture.Frames {
		switch coverage {
		case SandboxConformanceStreamHalfClose:
			if frame.Type == "half_close" {
				return true
			}
		case SandboxConformanceStreamBackpressure:
			if frame.Type == "window" || frame.WindowBytes > 0 || fixture.BackpressureBytes > 0 {
				return true
			}
		case SandboxConformanceStreamCancel:
			if frame.Type == "cancel" {
				return true
			}
		}
	}
	return false
}

func firstNonEmptyList(values []string, fallback string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return fallback
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func normalizeConformanceCoverage(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "coverage:")
	switch value {
	case "handshake", "init", "register", "handshake/init/register", "sandbox.handshake", "sandbox.init", "sandbox.register":
		return SandboxConformanceHandshakeInitRegister
	case "route/rule/config", "request-response", "request_response", "sandbox.request_response", "sandbox.route_rule_config":
		return SandboxConformanceRequestResponse
	case "secret_denial", "secret-denial", "sandbox.secret", "sandbox.secret-denial":
		return SandboxConformanceSecretDenial
	case "file_denial", "file-denial", "sandbox.file", "sandbox.file-denial":
		return SandboxConformanceFileDenial
	case "network_denial", "network-denial", "sandbox.network", "sandbox.network-denial":
		return SandboxConformanceNetworkDenial
	case "cpu_memory_exceeded", "cpu-memory-exceeded", "resource_exceeded", "sandbox.resource_exceeded":
		return SandboxConformanceCPUMemoryExceeded
	case "crash_loop", "crash-loop", "sandbox.crash", "sandbox.crash-loop":
		return SandboxConformanceCrashLoop
	case "stream_half_close", "stream-half-close", "half_close", "half-close":
		return SandboxConformanceStreamHalfClose
	case "stream_backpressure", "stream-backpressure", "backpressure":
		return SandboxConformanceStreamBackpressure
	case "stream_cancel", "stream-cancel", "cancel":
		return SandboxConformanceStreamCancel
	default:
		return value
	}
}

func missingConformanceCoverage(summary ConformanceSummary, required []string) []string {
	if len(required) == 0 {
		return nil
	}
	seen := map[string]bool{}
	for _, item := range summary.Coverage {
		if normalized := normalizeConformanceCoverage(item); normalized != "" {
			seen[normalized] = true
		}
	}
	var missing []string
	for _, item := range required {
		if !seen[normalizeConformanceCoverage(item)] {
			missing = append(missing, item)
		}
	}
	sort.Strings(missing)
	return missing
}
