package pluginmanager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tursom/mc-gateway/plugin/api"
)

const (
	defaultWarningOverrideTTL = 24 * time.Hour
)

type PreflightAdapter interface {
	RunPreflight(ctx context.Context, artifact ArtifactRecord, pluginRecord PluginRecord, profile, action string) (api.PreflightResult, error)
}

type SelfTestAdapter interface {
	RunSelfTest(ctx context.Context, artifact ArtifactRecord, pluginRecord PluginRecord, profile string) (api.SelfTestResult, error)
}

func (m *Manager) EvaluateGovernance(ctx context.Context, pluginID, artifactID, action, profile, configJSON string) (GovernanceDecision, error) {
	decision, _, err := m.evaluateGovernance(ctx, pluginID, artifactID, action, profile, configJSON, false)
	return decision, err
}

func (m *Manager) SetPolicyProfile(profile string) error {
	switch profile {
	case "":
		m.policyProfile = PolicyProfileProd
	case PolicyProfileDev, PolicyProfileStaging, PolicyProfileProd:
		m.policyProfile = profile
	default:
		return fmt.Errorf("invalid policy profile %q", profile)
	}
	return nil
}

func (m *Manager) PolicyProfile() string {
	return m.currentPolicyProfile()
}

func (m *Manager) currentPolicyProfile() string {
	if m.policyProfile == "" {
		return PolicyProfileProd
	}
	return m.policyProfile
}

func (m *Manager) GovernanceStatus(ctx context.Context, pluginID, artifactID, profile string) (GovernanceStatus, error) {
	plugin, err := m.repo.Plugin(ctx, pluginID)
	if err != nil {
		return GovernanceStatus{}, err
	}
	if artifactID == "" {
		artifactID = plugin.DesiredArtifactID
	}
	decision, conflict, err := m.evaluateGovernance(ctx, pluginID, artifactID, GovernanceActionEnable, profile, plugin.ConfigJSON, true)
	if err != nil {
		return GovernanceStatus{}, err
	}
	reviews, err := m.repo.ListReviews(ctx, pluginID)
	if err != nil {
		return GovernanceStatus{}, err
	}
	overrides, err := m.repo.ListWarningOverrides(ctx, pluginID)
	if err != nil {
		return GovernanceStatus{}, err
	}
	preflights, err := m.repo.ListPreflights(ctx, pluginID)
	if err != nil {
		return GovernanceStatus{}, err
	}
	benchmarks, err := m.repo.ListBenchmarks(ctx, pluginID)
	if err != nil {
		return GovernanceStatus{}, err
	}
	advisories, err := m.repo.ListAdvisories(ctx, pluginID)
	if err != nil {
		return GovernanceStatus{}, err
	}
	return GovernanceStatus{
		Decision:         decision,
		Policy:           policyForProfile(profile, m.repo.now()),
		Reviews:          reviews,
		WarningOverrides: overrides,
		Preflights:       preflights,
		Benchmarks:       benchmarks,
		Advisories:       advisories,
		Conflicts:        conflict,
	}, nil
}

func (m *Manager) CreateReview(ctx context.Context, actor, pluginID string, req GovernanceReviewRequest) (ReviewRecord, error) {
	plugin, err := m.repo.Plugin(ctx, pluginID)
	if err != nil {
		return ReviewRecord{}, err
	}
	if req.ArtifactID == "" {
		req.ArtifactID = plugin.DesiredArtifactID
	}
	if req.Profile == "" {
		req.Profile = m.currentPolicyProfile()
	}
	if req.Decision == "" {
		req.Decision = ReviewDecisionApproved
	}
	switch req.Decision {
	case ReviewDecisionApproved, ReviewDecisionRejected:
	default:
		return ReviewRecord{}, fmt.Errorf("invalid review decision %q", req.Decision)
	}
	artifact, manifest, err := m.artifactManifest(ctx, pluginID, req.ArtifactID)
	if err != nil {
		return ReviewRecord{}, err
	}
	fingerprint := governanceFingerprint(plugin, artifact, manifest, policyHash(policyForProfile(req.Profile, m.repo.now())))
	review := ReviewRecord{
		PluginID:          pluginID,
		ArtifactID:        req.ArtifactID,
		Profile:           normalizeProfile(req.Profile),
		RiskLevel:         riskLevel(manifest, artifact),
		ConfigHash:        fingerprint.ConfigHash,
		ScopeHash:         fingerprint.ScopeHash,
		RolloutHash:       fingerprint.RolloutHash,
		RuntimeLimitsHash: fingerprint.RuntimeLimitsHash,
		FeaturesHash:      fingerprint.FeaturesHash,
		PolicyHash:        fingerprint.PolicyHash,
		Decision:          req.Decision,
		Notes:             req.Notes,
		ReviewedBy:        actor,
	}
	review, err = m.repo.SaveReview(ctx, review)
	if err != nil {
		return ReviewRecord{}, err
	}
	_ = m.repo.RecordOperation(ctx, pluginID, req.ArtifactID, "governance_review", "succeeded", actor, "plugin governance review recorded", map[string]any{
		"profile":     review.Profile,
		"risk_level":  review.RiskLevel,
		"decision":    review.Decision,
		"policy_hash": review.PolicyHash,
	})
	return review, nil
}

func (m *Manager) CreateWarningOverride(ctx context.Context, actor, pluginID string, req WarningOverrideRequest) (WarningOverrideRecord, error) {
	plugin, err := m.repo.Plugin(ctx, pluginID)
	if err != nil {
		return WarningOverrideRecord{}, err
	}
	if req.ArtifactID == "" {
		req.ArtifactID = plugin.DesiredArtifactID
	}
	if req.Profile == "" {
		req.Profile = m.currentPolicyProfile()
	}
	if req.Action == "" {
		req.Action = GovernanceActionEnable
	}
	if strings.TrimSpace(req.Reason) == "" {
		return WarningOverrideRecord{}, errors.New("reason is required")
	}
	policy := policyForProfile(req.Profile, m.repo.now())
	if req.TTLSeconds <= 0 || req.TTLSeconds > policy.WarningOverrideTTLSeconds {
		req.TTLSeconds = policy.WarningOverrideTTLSeconds
	}
	decision, err := m.EvaluateGovernance(ctx, pluginID, req.ArtifactID, req.Action, req.Profile, plugin.ConfigJSON)
	if err != nil {
		return WarningOverrideRecord{}, err
	}
	if hasBlockingIssue(decision.Issues) {
		return WarningOverrideRecord{}, errors.New("blocking governance issues cannot be overridden")
	}
	if !hasWarningIssue(decision.Issues) {
		return WarningOverrideRecord{}, errors.New("no warning governance issues require override")
	}
	now := m.repo.now().Unix()
	override := WarningOverrideRecord{
		PluginID:   pluginID,
		ArtifactID: req.ArtifactID,
		Profile:    normalizeProfile(req.Profile),
		Action:     req.Action,
		PolicyHash: decision.PolicyHash,
		Reason:     req.Reason,
		CreatedBy:  actor,
		ExpiresAt:  now + req.TTLSeconds,
		CreatedAt:  now,
	}
	override, err = m.repo.SaveWarningOverride(ctx, override)
	if err != nil {
		return WarningOverrideRecord{}, err
	}
	_ = m.repo.RecordOperation(ctx, pluginID, req.ArtifactID, "governance_warning_override", "succeeded", actor, "governance warning override recorded", map[string]any{
		"profile":     override.Profile,
		"action":      override.Action,
		"policy_hash": override.PolicyHash,
		"expires_at":  override.ExpiresAt,
	})
	return override, nil
}

func (m *Manager) RunPreflight(ctx context.Context, actor, pluginID string, req PreflightRequest) (PreflightResult, error) {
	plugin, err := m.repo.Plugin(ctx, pluginID)
	if err != nil {
		return PreflightResult{}, err
	}
	if req.ArtifactID == "" {
		req.ArtifactID = plugin.DesiredArtifactID
	}
	if req.ConfigJSON == "" {
		req.ConfigJSON = plugin.ConfigJSON
	}
	if req.Profile == "" {
		req.Profile = m.currentPolicyProfile()
	}
	if req.Action == "" {
		req.Action = GovernanceActionEnable
	}
	artifact, manifest, err := m.artifactManifest(ctx, pluginID, req.ArtifactID)
	if err != nil {
		return PreflightResult{}, err
	}
	result := m.preflightChecks(ctx, plugin, artifact, manifest, req.Profile, req.Action, req.ConfigJSON)
	if adapter, ok := m.adapter.(PreflightAdapter); ok {
		pluginResult, err := adapter.RunPreflight(ctx, artifact, pluginRecordWithConfig(plugin, artifact.ID, req.ConfigJSON), req.Profile, req.Action)
		result.Checks = append(result.Checks, apiPreflightChecks(pluginResult.Checks)...)
		if err != nil {
			result.Checks = append(result.Checks, PreflightCheck{
				Code:     "plugin_preflight_error",
				Severity: GateSeverityBlocking,
				Message:  err.Error(),
			})
		}
	}
	result.OK = !preflightHasBlocking(result.Checks)
	result.CreatedAt = m.repo.now().Unix()
	data, _ := json.Marshal(result)
	status := "succeeded"
	if !result.OK {
		status = "failed"
	}
	_, err = m.repo.SavePreflight(ctx, PreflightRecord{
		PluginID:   pluginID,
		ArtifactID: req.ArtifactID,
		Profile:    normalizeProfile(req.Profile),
		Status:     status,
		ResultJSON: string(data),
		CreatedBy:  actor,
	})
	if err != nil {
		return PreflightResult{}, err
	}
	_ = m.repo.RecordOperation(ctx, pluginID, req.ArtifactID, "governance_preflight", status, actor, "plugin preflight completed", map[string]any{
		"profile": req.Profile,
		"ok":      result.OK,
	})
	return result, nil
}

func (m *Manager) RunSelfTest(ctx context.Context, actor, pluginID string, req SelfTestRequest) (PreflightResult, error) {
	plugin, err := m.repo.Plugin(ctx, pluginID)
	if err != nil {
		return PreflightResult{}, err
	}
	if req.ArtifactID == "" {
		req.ArtifactID = plugin.DesiredArtifactID
	}
	if req.Profile == "" {
		req.Profile = m.currentPolicyProfile()
	}
	artifact, _, err := m.artifactManifest(ctx, pluginID, req.ArtifactID)
	if err != nil {
		return PreflightResult{}, err
	}
	result := PreflightResult{Profile: normalizeProfile(req.Profile)}
	if adapter, ok := m.adapter.(SelfTestAdapter); ok {
		pluginResult, err := adapter.RunSelfTest(ctx, artifact, plugin, req.Profile)
		result.Checks = append(result.Checks, apiPreflightChecks(pluginResult.Checks)...)
		if err != nil {
			result.Checks = append(result.Checks, PreflightCheck{Code: "plugin_self_test_error", Severity: GateSeverityBlocking, Message: err.Error()})
		}
	} else {
		result.Checks = append(result.Checks, PreflightCheck{Code: "self_test_not_implemented", Severity: GateSeverityWarning, Message: "plugin does not implement SelfTester"})
	}
	result.OK = !preflightHasBlocking(result.Checks)
	result.CreatedAt = m.repo.now().Unix()
	data, _ := json.Marshal(result)
	status := "succeeded"
	if !result.OK {
		status = "failed"
	}
	_, err = m.repo.SavePreflight(ctx, PreflightRecord{
		PluginID:   pluginID,
		ArtifactID: req.ArtifactID,
		Profile:    normalizeProfile(req.Profile),
		Status:     status,
		ResultJSON: string(data),
		CreatedBy:  actor,
	})
	if err != nil {
		return PreflightResult{}, err
	}
	_ = m.repo.RecordOperation(ctx, pluginID, req.ArtifactID, "governance_self_test", status, actor, "plugin self-test completed", map[string]any{
		"profile": req.Profile,
		"ok":      result.OK,
	})
	return result, nil
}

func (m *Manager) SaveBenchmark(ctx context.Context, actor string, req BenchmarkRequest) (BenchmarkRecord, error) {
	record, err := m.repo.SaveBenchmark(ctx, actor, req)
	if err != nil {
		return BenchmarkRecord{}, err
	}
	_ = m.repo.RecordOperation(ctx, record.PluginID, record.ArtifactID, "governance_benchmark", "succeeded", actor, "plugin benchmark recorded", map[string]any{
		"profile":            record.Profile,
		"benchmark_profile":  record.BenchmarkProfile,
		"p95_ms":             record.P95MS,
		"p99_ms":             record.P99MS,
		"error_rate":         record.ErrorRate,
		"active_proxy_limit": record.ActiveProxyCapacity,
		"baseline_diff":      record.BaselineDiff,
	})
	return record, nil
}

func (m *Manager) UpsertAdvisory(ctx context.Context, actor string, req AdvisoryRequest) (AdvisoryRecord, error) {
	record, err := m.repo.UpsertAdvisory(ctx, actor, req)
	if err != nil {
		return AdvisoryRecord{}, err
	}
	if record.Action == AdvisoryActionQuarantine || record.Action == AdvisoryActionRevoke {
		m.quarantineAffected(ctx, record)
	}
	targetPlugin := record.PluginID
	if targetPlugin == "" && record.ArtifactSHA256 != "" {
		if artifacts, listErr := m.repo.ListArtifacts(ctx, ""); listErr == nil {
			for _, artifact := range artifacts {
				if artifact.SHA256 == record.ArtifactSHA256 {
					targetPlugin = artifact.PluginID
					break
				}
			}
		}
	}
	_ = m.repo.RecordOperation(ctx, targetPlugin, "", "governance_advisory", "succeeded", actor, "plugin advisory upserted", map[string]any{
		"advisory_id":        record.AdvisoryID,
		"status":             record.Status,
		"action":             record.Action,
		"artifact_sha256":    record.ArtifactSHA256,
		"plugin_id":          record.PluginID,
		"version_range":      record.VersionRange,
		"dependency_name":    record.DependencyName,
		"dependency_range":   record.DependencyRange,
		"recommended_action": record.RecommendedAction,
		"fixed_version":      record.FixedVersion,
		"mitigation":         record.Mitigation,
	})
	return record, nil
}

func (m *Manager) ListReviews(ctx context.Context, pluginID string) ([]ReviewRecord, error) {
	return m.repo.ListReviews(ctx, pluginID)
}

func (m *Manager) ListWarningOverrides(ctx context.Context, pluginID string) ([]WarningOverrideRecord, error) {
	return m.repo.ListWarningOverrides(ctx, pluginID)
}

func (m *Manager) ListAdvisories(ctx context.Context, pluginID string) ([]AdvisoryRecord, error) {
	return m.repo.ListAdvisories(ctx, pluginID)
}

func (m *Manager) ListPreflights(ctx context.Context, pluginID string) ([]PreflightRecord, error) {
	return m.repo.ListPreflights(ctx, pluginID)
}

func (m *Manager) ListBenchmarks(ctx context.Context, pluginID string) ([]BenchmarkRecord, error) {
	return m.repo.ListBenchmarks(ctx, pluginID)
}

func (m *Manager) evaluateGovernance(ctx context.Context, pluginID, artifactID, action, profile, configJSON string, preview bool) (GovernanceDecision, ConflictAnalysis, error) {
	if action == "" {
		action = GovernanceActionEnable
	}
	profile = normalizeProfile(profile)
	policy := policyForProfile(profile, m.repo.now())
	policyHash := policyHash(policy)
	plugin, err := m.repo.Plugin(ctx, pluginID)
	if err != nil {
		return GovernanceDecision{}, ConflictAnalysis{}, err
	}
	if artifactID == "" {
		artifactID = plugin.DesiredArtifactID
	}
	if configJSON == "" {
		configJSON = plugin.ConfigJSON
	}
	artifact, manifest, err := m.artifactManifest(ctx, pluginID, artifactID)
	if err != nil {
		return GovernanceDecision{}, ConflictAnalysis{}, err
	}
	now := m.repo.now().Unix()
	decision := GovernanceDecision{
		OK:         false,
		Action:     action,
		Profile:    profile,
		RiskLevel:  riskLevel(manifest, artifact),
		PolicyHash: policyHash,
		CreatedAt:  now,
	}
	var issues []GovernanceIssue
	if err := m.validateArtifactGate(artifact); err != nil {
		issues = append(issues, issue("artifact_not_loadable", GateSeverityBlocking, err.Error(), pluginID, artifactID, nil))
	}
	preflight := m.preflightChecks(ctx, plugin, artifact, manifest, profile, action, configJSON)
	for _, check := range preflight.Checks {
		issues = append(issues, issue(check.Code, check.Severity, check.Message, pluginID, artifactID, check.Details))
		decision.Checks = append(decision.Checks, issue(check.Code, check.Severity, check.Message, pluginID, artifactID, check.Details))
	}
	advisoryIssues, err := m.advisoryIssues(ctx, artifact, manifest)
	if err != nil {
		return GovernanceDecision{}, ConflictAnalysis{}, err
	}
	issues = append(issues, advisoryIssues...)
	benchmarkIssues, err := m.benchmarkIssues(ctx, artifact, manifest, policy)
	if err != nil {
		return GovernanceDecision{}, ConflictAnalysis{}, err
	}
	issues = append(issues, benchmarkIssues...)
	conflict, err := m.conflictAnalysis(ctx, plugin, artifact, manifest)
	if err != nil {
		return GovernanceDecision{}, ConflictAnalysis{}, err
	}
	issues = append(issues, conflict.Issues...)
	if reviewRequired(profile, decision.RiskLevel, policy) {
		decision.ReviewRequired = true
		fingerprint := governanceFingerprint(pluginRecordWithConfig(plugin, artifactID, configJSON), artifact, manifest, policyHash)
		reviewOK, err := m.hasMatchingReview(ctx, pluginID, artifactID, profile, fingerprint)
		if err != nil {
			return GovernanceDecision{}, ConflictAnalysis{}, err
		}
		if !reviewOK {
			issues = append(issues, issue("review_required", GateSeverityBlocking, "high risk plugin requires approved review for this policy snapshot", pluginID, artifactID, map[string]any{
				"profile":     profile,
				"risk_level":  decision.RiskLevel,
				"policy_hash": policyHash,
			}))
		}
	}
	decision.Issues = sortedIssues(issues)
	if hasBlockingIssue(decision.Issues) {
		decision.OK = false
		return decision, conflict, nil
	}
	if hasWarningIssue(decision.Issues) && !preview {
		if _, ok, err := m.repo.ActiveWarningOverride(ctx, pluginID, artifactID, profile, action, policyHash); err != nil {
			return GovernanceDecision{}, ConflictAnalysis{}, err
		} else if !ok {
			decision.OK = false
			return decision, conflict, nil
		}
		decision.WarningOverrideUsed = true
	}
	decision.OK = true
	return decision, conflict, nil
}

func (m *Manager) preflightChecks(ctx context.Context, plugin PluginRecord, artifact ArtifactRecord, manifest Manifest, profile, action, configJSON string) PreflightResult {
	result := PreflightResult{Profile: normalizeProfile(profile), CreatedAt: m.repo.now().Unix()}
	if configJSON == "" {
		configJSON = "{}"
	}
	if !json.Valid([]byte(configJSON)) {
		result.Checks = append(result.Checks, PreflightCheck{Code: "config_invalid", Severity: GateSeverityBlocking, Message: "config_json must be valid JSON"})
		return result
	}
	if err := validateConfigSchema(manifest.ConfigSchema, configJSON); err != nil {
		result.Checks = append(result.Checks, PreflightCheck{Code: "config_schema_failed", Severity: GateSeverityBlocking, Message: err.Error()})
	}
	if err := m.validateSecretRefs(ctx, manifest, configJSON); err != nil {
		result.Checks = append(result.Checks, PreflightCheck{Code: "secret_missing", Severity: GateSeverityBlocking, Message: err.Error()})
	}
	if missing := requiredFeatures(manifest); len(missing) > 0 {
		result.Checks = append(result.Checks, PreflightCheck{
			Code:     "feature_missing",
			Severity: GateSeverityBlocking,
			Message:  "required feature declaration is not supported by gateway",
			Details:  map[string]any{"features": missing},
		})
	}
	if manifest.RuntimeLimits.HandlerTimeoutMS > int(DefaultHandlerTimeout.Milliseconds()) {
		result.Checks = append(result.Checks, PreflightCheck{
			Code:     "runtime_limits_warning",
			Severity: GateSeverityWarning,
			Message:  "handler timeout is unset or exceeds default runtime limit",
			Details: map[string]any{
				"handler_timeout_ms": manifest.RuntimeLimits.HandlerTimeoutMS,
				"default_ms":         DefaultHandlerTimeout.Milliseconds(),
			},
		})
	}
	scope := manifestScope(manifest)
	if upstreamModeFromArtifact(artifact) == UpstreamModeProtocolProxy && len(scope.Values) == 0 {
		result.Checks = append(result.Checks, PreflightCheck{Code: "scope_global", Severity: GateSeverityWarning, Message: "plugin scope defaults to global"})
	}
	rollout := manifestRollout(manifest)
	if upstreamModeFromArtifact(artifact) == UpstreamModeProtocolProxy && rollout.Mode == "all" && normalizeProfile(profile) == PolicyProfileProd {
		result.Checks = append(result.Checks, PreflightCheck{Code: "rollout_all_prod", Severity: GateSeverityWarning, Message: "prod rollout applies to all traffic"})
	}
	if upstreamModeFromArtifact(artifact) == UpstreamModeProtocolProxy {
		var summary CapabilitySummary
		_ = json.Unmarshal([]byte(artifact.CapabilitiesSummaryJSON), &summary)
		if summary.Minecraft == nil {
			result.Checks = append(result.Checks, PreflightCheck{Code: "minecraft_capability_missing", Severity: GateSeverityBlocking, Message: "protocol-proxy plugins must declare minecraft capability"})
		} else {
			if summary.Minecraft.ProtocolVersions.Min == 0 && summary.Minecraft.ProtocolVersions.Max == 0 && len(summary.Minecraft.ProtocolVersions.Tested) == 0 {
				result.Checks = append(result.Checks, PreflightCheck{Code: "minecraft_protocol_untested", Severity: GateSeverityWarning, Message: "minecraft capability does not declare tested protocol versions"})
			}
			if len(summary.Minecraft.Forwarding.Supported) == 0 {
				result.Checks = append(result.Checks, PreflightCheck{Code: "backend_forwarding_warning", Severity: GateSeverityWarning, Message: "backend forwarding behavior is not declared"})
			}
			if summary.Minecraft.Forwarding.RequiresSecret {
				hasSecret := false
				for _, spec := range manifest.Secrets {
					if spec.Required {
						hasSecret = true
						break
					}
				}
				if !hasSecret {
					result.Checks = append(result.Checks, PreflightCheck{Code: "backend_forwarding_secret_missing", Severity: GateSeverityBlocking, Message: "minecraft forwarding requires a declared required secret"})
				}
			}
		}
	}
	if external := externalDependencies(manifest); len(external) > 0 {
		result.Checks = append(result.Checks, PreflightCheck{Code: "external_dependencies_declared", Severity: GateSeverityInfo, Message: "plugin declares external dependencies", Details: map[string]any{"dependencies": external}})
	}
	result.OK = !preflightHasBlocking(result.Checks)
	return result
}

func (m *Manager) conflictAnalysis(ctx context.Context, target PluginRecord, artifact ArtifactRecord, manifest Manifest) (ConflictAnalysis, error) {
	analysis := ConflictAnalysis{CreatedAt: m.repo.now().Unix(), Plan: m.DispatchPlan(ctx)}
	if upstreamModeFromArtifact(artifact) != UpstreamModeProtocolProxy {
		analysis.OK = true
		return analysis, nil
	}
	targetScope := manifestScope(manifest)
	plugins, err := m.repo.ListPlugins(ctx)
	if err != nil {
		return ConflictAnalysis{}, err
	}
	for _, plugin := range plugins {
		if plugin.ID == target.ID || plugin.RuntimeState != RuntimeEnabled || plugin.ActiveArtifactID == "" {
			continue
		}
		otherArtifact, err := m.repo.Artifact(ctx, plugin.ActiveArtifactID)
		if err != nil {
			continue
		}
		if upstreamModeFromArtifact(otherArtifact) != UpstreamModeProtocolProxy {
			continue
		}
		var otherManifest Manifest
		if json.Unmarshal([]byte(otherArtifact.MetadataJSON), &otherManifest) != nil {
			continue
		}
		otherScope := manifestScope(otherManifest)
		if scopesOverlap(targetScope, otherScope) {
			analysis.Issues = append(analysis.Issues, issue("scope_overlap", GateSeverityBlocking, "protocol-proxy scope overlaps enabled plugin", target.ID, artifact.ID, map[string]any{
				"other_plugin_id":   plugin.ID,
				"other_artifact_id": otherArtifact.ID,
				"scope":             targetScope.Values,
			}))
			analysis.Issues = append(analysis.Issues, issue("protocol_proxy_singleton", GateSeverityBlocking, "only one protocol-proxy plugin can own an overlapping scope", target.ID, artifact.ID, map[string]any{
				"other_plugin_id": plugin.ID,
			}))
		} else if target.Priority >= plugin.Priority {
			analysis.Issues = append(analysis.Issues, issue("shadowed_handler", GateSeverityWarning, "protocol-proxy handler may be shadowed by a higher priority plugin", target.ID, artifact.ID, map[string]any{
				"other_plugin_id": plugin.ID,
				"other_priority":  plugin.Priority,
				"priority":        target.Priority,
			}))
		}
	}
	providers := providerSingletons(manifest)
	if len(providers) > 0 {
		for _, plugin := range plugins {
			if plugin.ID == target.ID || plugin.RuntimeState != RuntimeEnabled || plugin.ActiveArtifactID == "" {
				continue
			}
			otherArtifact, err := m.repo.Artifact(ctx, plugin.ActiveArtifactID)
			if err != nil {
				continue
			}
			var otherManifest Manifest
			if json.Unmarshal([]byte(otherArtifact.MetadataJSON), &otherManifest) != nil {
				continue
			}
			for _, provider := range intersectStrings(providers, providerSingletons(otherManifest)) {
				analysis.Issues = append(analysis.Issues, issue("provider_singleton", GateSeverityBlocking, "provider singleton is already owned by enabled plugin", target.ID, artifact.ID, map[string]any{
					"provider":        provider,
					"other_plugin_id": plugin.ID,
				}))
			}
		}
	}
	if middlewareCycle(manifest) {
		analysis.Issues = append(analysis.Issues, issue("middleware_ordering_cycle", GateSeverityBlocking, "middleware ordering declaration contains a cycle", target.ID, artifact.ID, nil))
	}
	analysis.Issues = sortedIssues(analysis.Issues)
	analysis.OK = !hasBlockingIssue(analysis.Issues)
	return analysis, nil
}

func (m *Manager) advisoryIssues(ctx context.Context, artifact ArtifactRecord, manifest Manifest) ([]GovernanceIssue, error) {
	advisories, err := m.repo.ListAdvisories(ctx, artifact.PluginID)
	if err != nil {
		return nil, err
	}
	var issues []GovernanceIssue
	for _, advisory := range advisories {
		if advisory.Status == AdvisoryStatusAcked || advisory.Status == "" && advisory.Action == AdvisoryActionMitigate {
			continue
		}
		if !advisoryMatches(advisory, artifact, manifest) {
			continue
		}
		severity := GateSeverityWarning
		switch advisory.Action {
		case AdvisoryActionDenylist, AdvisoryActionQuarantine, AdvisoryActionRevoke:
			severity = GateSeverityBlocking
		}
		code := "advisory_" + advisory.Action
		if advisory.Status == AdvisoryStatusRevoked {
			code = "advisory_revoke"
			severity = GateSeverityBlocking
		}
		issues = append(issues, issue(code, severity, "artifact matches local security advisory", artifact.PluginID, artifact.ID, map[string]any{
			"advisory_id":        advisory.AdvisoryID,
			"recommended_action": advisory.RecommendedAction,
			"fixed_version":      advisory.FixedVersion,
			"mitigation":         advisory.Mitigation,
		}))
	}
	return issues, nil
}

func (m *Manager) benchmarkIssues(ctx context.Context, artifact ArtifactRecord, manifest Manifest, policy PolicySnapshot) ([]GovernanceIssue, error) {
	benchmarks, err := m.repo.ListBenchmarks(ctx, artifact.PluginID)
	if err != nil {
		return nil, err
	}
	var latest *BenchmarkRecord
	for idx := range benchmarks {
		benchmark := benchmarks[idx]
		if benchmark.ArtifactID == artifact.ID && benchmark.Profile == policy.Profile {
			latest = &benchmark
			break
		}
	}
	if latest == nil {
		return nil, nil
	}
	var issues []GovernanceIssue
	if latest.BaselineDiff >= policy.BlockBenchmarkRegression {
		issues = append(issues, issue("benchmark_regression_blocking", GateSeverityBlocking, "benchmark regression exceeds blocking threshold", artifact.PluginID, artifact.ID, map[string]any{"baseline_diff": latest.BaselineDiff}))
	} else if latest.BaselineDiff >= policy.WarnBenchmarkRegression {
		issues = append(issues, issue("benchmark_regression_warning", GateSeverityWarning, "benchmark regression exceeds warning threshold", artifact.PluginID, artifact.ID, map[string]any{"baseline_diff": latest.BaselineDiff}))
	}
	if manifest.RuntimeLimits.HandlerTimeoutMS > 0 && latest.P99MS > float64(manifest.RuntimeLimits.HandlerTimeoutMS) {
		issues = append(issues, issue("benchmark_runtime_limit_exceeded", GateSeverityBlocking, "P99 exceeds handler runtime limit", artifact.PluginID, artifact.ID, map[string]any{
			"p99_ms": latest.P99MS,
			"limit":  manifest.RuntimeLimits.HandlerTimeoutMS,
		}))
	}
	if latest.ActiveProxyCapacity > 0 {
		active := m.activeProxyCountLocked(artifact.PluginID)
		if int64(active) > latest.ActiveProxyCapacity {
			issues = append(issues, issue("active_proxy_capacity_exceeded", GateSeverityBlocking, "active proxy connections exceed benchmarked capacity", artifact.PluginID, artifact.ID, map[string]any{
				"active":   active,
				"capacity": latest.ActiveProxyCapacity,
			}))
		}
	}
	if latest.ErrorRate >= 0.05 {
		issues = append(issues, issue("benchmark_error_rate_blocking", GateSeverityBlocking, "benchmark error rate exceeds blocking threshold", artifact.PluginID, artifact.ID, map[string]any{"error_rate": latest.ErrorRate}))
	} else if latest.ErrorRate >= 0.01 {
		issues = append(issues, issue("benchmark_error_rate_warning", GateSeverityWarning, "benchmark error rate exceeds warning threshold", artifact.PluginID, artifact.ID, map[string]any{"error_rate": latest.ErrorRate}))
	}
	return issues, nil
}

func (m *Manager) hasMatchingReview(ctx context.Context, pluginID, artifactID, profile string, fingerprint governanceFingerprintValue) (bool, error) {
	reviews, err := m.repo.ListReviews(ctx, pluginID)
	if err != nil {
		return false, err
	}
	for _, review := range reviews {
		if review.PluginID != pluginID || review.ArtifactID != artifactID || review.Profile != profile || review.Decision != ReviewDecisionApproved {
			continue
		}
		if review.ConfigHash == fingerprint.ConfigHash &&
			review.ScopeHash == fingerprint.ScopeHash &&
			review.RolloutHash == fingerprint.RolloutHash &&
			review.RuntimeLimitsHash == fingerprint.RuntimeLimitsHash &&
			review.FeaturesHash == fingerprint.FeaturesHash &&
			review.PolicyHash == fingerprint.PolicyHash {
			return true, nil
		}
	}
	return false, nil
}

func (m *Manager) artifactManifest(ctx context.Context, pluginID, artifactID string) (ArtifactRecord, Manifest, error) {
	artifact, err := m.repo.Artifact(ctx, artifactID)
	if err != nil {
		return ArtifactRecord{}, Manifest{}, err
	}
	if artifact.PluginID != pluginID {
		return ArtifactRecord{}, Manifest{}, errors.New("artifact plugin_id does not match")
	}
	var manifest Manifest
	if err := json.Unmarshal([]byte(artifact.MetadataJSON), &manifest); err != nil {
		return ArtifactRecord{}, Manifest{}, err
	}
	return artifact, manifest, nil
}

type governanceFingerprintValue struct {
	ConfigHash        string
	ScopeHash         string
	RolloutHash       string
	RuntimeLimitsHash string
	FeaturesHash      string
	PolicyHash        string
}

func governanceFingerprint(plugin PluginRecord, artifact ArtifactRecord, manifest Manifest, policyHash string) governanceFingerprintValue {
	_ = artifact
	return governanceFingerprintValue{
		ConfigHash:        stableHashJSONRaw(defaultJSONObject(plugin.ConfigJSON)),
		ScopeHash:         stableHash(manifestScope(manifest).Values),
		RolloutHash:       stableHash(manifestRollout(manifest)),
		RuntimeLimitsHash: stableHash(manifest.RuntimeLimits),
		FeaturesHash:      stableHash(requiredFeatures(manifest)),
		PolicyHash:        policyHash,
	}
}

func policyForProfile(profile string, now time.Time) PolicySnapshot {
	profile = normalizeProfile(profile)
	policy := PolicySnapshot{
		Profile:                   profile,
		WarningOverrideTTLSeconds: int64(defaultWarningOverrideTTL.Seconds()),
		ReviewRequiredRisk:        RiskHigh,
		WarnBenchmarkRegression:   0.20,
		BlockBenchmarkRegression:  0.50,
		CreatedAt:                 now.Unix(),
	}
	switch profile {
	case PolicyProfileDev:
		policy.ReviewRequiredRisk = ""
		policy.WarningOverrideTTLSeconds = int64((7 * 24 * time.Hour).Seconds())
	case PolicyProfileStaging:
		policy.WarningOverrideTTLSeconds = int64((72 * time.Hour).Seconds())
	case PolicyProfileProd:
		policy.WarningOverrideTTLSeconds = int64((24 * time.Hour).Seconds())
	}
	return policy
}

func policyHash(policy PolicySnapshot) string {
	policy.CreatedAt = 0
	return stableHash(policy)
}

func normalizeProfile(profile string) string {
	switch profile {
	case PolicyProfileDev, PolicyProfileStaging, PolicyProfileProd:
		return profile
	case "":
		return PolicyProfileProd
	default:
		return PolicyProfileProd
	}
}

func riskLevel(manifest Manifest, artifact ArtifactRecord) string {
	var caps map[string]any
	_ = json.Unmarshal(manifest.Capabilities, &caps)
	if governance, ok := caps["governance"].(map[string]any); ok {
		if risk, _ := governance["risk_level"].(string); risk != "" {
			switch risk {
			case RiskLow, RiskMedium, RiskHigh:
				return risk
			}
		}
	}
	if upstreamModeFromArtifact(artifact) == UpstreamModeProtocolProxy {
		return RiskHigh
	}
	if len(manifest.Secrets) > 0 || len(externalDependencies(manifest)) > 0 {
		return RiskMedium
	}
	return RiskLow
}

func reviewRequired(profile, risk string, policy PolicySnapshot) bool {
	if profile != PolicyProfileProd || policy.ReviewRequiredRisk == "" {
		return false
	}
	return riskRank(risk) >= riskRank(policy.ReviewRequiredRisk)
}

func riskRank(risk string) int {
	switch risk {
	case RiskHigh:
		return 3
	case RiskMedium:
		return 2
	case RiskLow:
		return 1
	default:
		return 0
	}
}

type scopeSpec struct {
	Type   string
	Values []string
}

type rolloutSpec struct {
	Mode string `json:"mode"`
}

func manifestScope(manifest Manifest) scopeSpec {
	var caps map[string]any
	_ = json.Unmarshal(manifest.Capabilities, &caps)
	raw, _ := caps["scope"].(map[string]any)
	spec := scopeSpec{Type: "global"}
	if typ, _ := raw["type"].(string); typ != "" {
		spec.Type = typ
	}
	for _, key := range []string{"values", "hosts", "routes", "listeners"} {
		spec.Values = append(spec.Values, stringSlice(raw[key])...)
	}
	if value, _ := raw["value"].(string); value != "" {
		spec.Values = append(spec.Values, value)
	}
	spec.Values = uniqueSortedStrings(spec.Values)
	return spec
}

func manifestRollout(manifest Manifest) rolloutSpec {
	var caps map[string]any
	_ = json.Unmarshal(manifest.Capabilities, &caps)
	raw, _ := caps["rollout"].(map[string]any)
	mode, _ := raw["mode"].(string)
	if mode == "" {
		mode = "all"
	}
	return rolloutSpec{Mode: mode}
}

func scopesOverlap(a, b scopeSpec) bool {
	if len(a.Values) == 0 || len(b.Values) == 0 || a.Type == "global" || b.Type == "global" {
		return true
	}
	for _, left := range a.Values {
		for _, right := range b.Values {
			if left == right || left == "*" || right == "*" {
				return true
			}
		}
	}
	return false
}

func requiredFeatures(manifest Manifest) []string {
	var caps map[string]any
	_ = json.Unmarshal(manifest.Capabilities, &caps)
	var features []string
	for _, rawKey := range []string{"required_features", "features"} {
		for _, feature := range stringSlice(caps[rawKey]) {
			if !supportedFeature(feature) {
				features = append(features, feature)
			}
		}
	}
	return uniqueSortedStrings(features)
}

func supportedFeature(feature string) bool {
	switch feature {
	case "", ExtensionUpstreamConnect, ExtensionRouteResolve, ExtensionRouteResolver, ExtensionStatusPing,
		ExtensionConnectionFilter, ExtensionHandshakeFilter, ExtensionEventSubscriber,
		ExtensionProvider, ExtensionAuthProvider, ExtensionAdminAuthProvider,
		"upstream.connect", "minecraft", "config", "secret", "preflight", "self-test":
		return true
	default:
		return false
	}
}

func externalDependencies(manifest Manifest) []string {
	var caps map[string]any
	_ = json.Unmarshal(manifest.Capabilities, &caps)
	var deps []string
	for _, key := range []string{"external_dependencies", "external_deps", "dependencies"} {
		deps = append(deps, stringSlice(caps[key])...)
	}
	return uniqueSortedStrings(deps)
}

func providerSingletons(manifest Manifest) []string {
	var caps map[string]any
	_ = json.Unmarshal(manifest.Capabilities, &caps)
	var providers []string
	for _, key := range []string{"provider_singletons", "providers"} {
		providers = append(providers, stringSlice(caps[key])...)
	}
	return uniqueSortedStrings(providers)
}

func middlewareCycle(manifest Manifest) bool {
	var caps map[string]any
	_ = json.Unmarshal(manifest.Capabilities, &caps)
	raw, ok := caps["middleware_order"].([]any)
	if !ok || len(raw) == 0 {
		return false
	}
	graph := map[string][]string{}
	for _, item := range raw {
		edge, _ := item.(map[string]any)
		before, _ := edge["before"].(string)
		after, _ := edge["after"].(string)
		if before != "" && after != "" {
			graph[before] = append(graph[before], after)
		}
	}
	visiting := map[string]bool{}
	visited := map[string]bool{}
	var visit func(string) bool
	visit = func(node string) bool {
		if visiting[node] {
			return true
		}
		if visited[node] {
			return false
		}
		visiting[node] = true
		for _, next := range graph[node] {
			if visit(next) {
				return true
			}
		}
		visiting[node] = false
		visited[node] = true
		return false
	}
	for node := range graph {
		if visit(node) {
			return true
		}
	}
	return false
}

func advisoryMatches(advisory AdvisoryRecord, artifact ArtifactRecord, manifest Manifest) bool {
	if advisory.ArtifactSHA256 != "" && advisory.ArtifactSHA256 == artifact.SHA256 {
		return true
	}
	if advisory.PluginID != "" && advisory.PluginID == artifact.PluginID {
		return versionInRange(artifact.Version, advisory.VersionRange)
	}
	if advisory.DependencyName != "" {
		for _, dep := range sbomDependencies(manifest) {
			if dep.Name == advisory.DependencyName && versionInRange(dep.Version, advisory.DependencyRange) {
				return true
			}
		}
	}
	return false
}

type dependencySpec struct {
	Name    string
	Version string
}

func sbomDependencies(manifest Manifest) []dependencySpec {
	var raw map[string]any
	if len(manifest.SupplyChain) == 0 || json.Unmarshal(manifest.SupplyChain, &raw) != nil {
		return nil
	}
	var deps []dependencySpec
	for _, key := range []string{"dependencies", "sbom_dependencies", "modules"} {
		items, _ := raw[key].([]any)
		for _, item := range items {
			obj, _ := item.(map[string]any)
			name, _ := obj["name"].(string)
			if name == "" {
				name, _ = obj["path"].(string)
			}
			version, _ := obj["version"].(string)
			if name != "" {
				deps = append(deps, dependencySpec{Name: name, Version: version})
			}
		}
	}
	return deps
}

func versionInRange(version, expr string) bool {
	expr = strings.TrimSpace(expr)
	if expr == "" || expr == "*" {
		return true
	}
	for _, part := range strings.Split(expr, ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, "<="):
			if compareVersion(version, strings.TrimSpace(strings.TrimPrefix(part, "<="))) > 0 {
				return false
			}
		case strings.HasPrefix(part, ">="):
			if compareVersion(version, strings.TrimSpace(strings.TrimPrefix(part, ">="))) < 0 {
				return false
			}
		case strings.HasPrefix(part, "<"):
			if compareVersion(version, strings.TrimSpace(strings.TrimPrefix(part, "<"))) >= 0 {
				return false
			}
		case strings.HasPrefix(part, ">"):
			if compareVersion(version, strings.TrimSpace(strings.TrimPrefix(part, ">"))) <= 0 {
				return false
			}
		default:
			if version != part {
				return false
			}
		}
	}
	return true
}

func compareVersion(a, b string) int {
	as := versionParts(a)
	bs := versionParts(b)
	for i := 0; i < len(as) || i < len(bs); i++ {
		var av, bv int
		if i < len(as) {
			av = as[i]
		}
		if i < len(bs) {
			bv = bs[i]
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	return 0
}

func versionParts(version string) []int {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	fields := strings.FieldsFunc(version, func(r rune) bool {
		return r == '.' || r == '-' || r == '+'
	})
	var parts []int
	for _, field := range fields {
		n, err := strconv.Atoi(field)
		if err != nil {
			break
		}
		parts = append(parts, n)
	}
	return parts
}

func apiPreflightChecks(checks []api.PreflightCheck) []PreflightCheck {
	result := make([]PreflightCheck, 0, len(checks))
	for _, check := range checks {
		severity := check.Severity
		if severity == "" {
			severity = GateSeverityWarning
		}
		result = append(result, PreflightCheck{
			Code:     check.Code,
			Severity: severity,
			Message:  check.Message,
		})
	}
	return result
}

func pluginRecordWithConfig(plugin PluginRecord, artifactID, configJSON string) PluginRecord {
	plugin.DesiredArtifactID = artifactID
	plugin.ConfigJSON = configJSON
	return plugin
}

func issue(code, severity, message, pluginID, artifactID string, details map[string]any) GovernanceIssue {
	return GovernanceIssue{Code: code, Severity: severity, Message: message, PluginID: pluginID, ArtifactID: artifactID, Details: details}
}

func sortedIssues(issues []GovernanceIssue) []GovernanceIssue {
	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].Severity != issues[j].Severity {
			return severityRank(issues[i].Severity) > severityRank(issues[j].Severity)
		}
		if issueRank(issues[i].Code) != issueRank(issues[j].Code) {
			return issueRank(issues[i].Code) > issueRank(issues[j].Code)
		}
		if issues[i].Code != issues[j].Code {
			return issues[i].Code < issues[j].Code
		}
		return issues[i].PluginID < issues[j].PluginID
	})
	return issues
}

func issueRank(code string) int {
	switch code {
	case "scope_overlap":
		return 10
	case "review_required":
		return 9
	case "feature_missing", "secret_missing":
		return 8
	case "advisory_revoke":
		return 7
	default:
		return 0
	}
}

func severityRank(severity string) int {
	switch severity {
	case GateSeverityBlocking:
		return 3
	case GateSeverityWarning:
		return 2
	case GateSeverityInfo:
		return 1
	default:
		return 0
	}
}

func hasBlockingIssue(issues []GovernanceIssue) bool {
	for _, issue := range issues {
		if issue.Severity == GateSeverityBlocking {
			return true
		}
	}
	return false
}

func hasWarningIssue(issues []GovernanceIssue) bool {
	for _, issue := range issues {
		if issue.Severity == GateSeverityWarning {
			return true
		}
	}
	return false
}

func preflightHasBlocking(checks []PreflightCheck) bool {
	for _, check := range checks {
		if check.Severity == GateSeverityBlocking {
			return true
		}
	}
	return false
}

func stableHash(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func stableHashJSONRaw(raw string) string {
	var value any
	if json.Unmarshal([]byte(defaultJSONObject(raw)), &value) != nil {
		return stableHash(raw)
	}
	return stableHash(value)
}

func stringSlice(value any) []string {
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		result := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok && text != "" {
				result = append(result, text)
			}
		}
		return result
	case string:
		if typed != "" {
			return []string{typed}
		}
	}
	return nil
}

func manifestCapabilitiesRaw(artifact ArtifactRecord) string {
	var manifest Manifest
	if json.Unmarshal([]byte(artifact.MetadataJSON), &manifest) != nil {
		return "{}"
	}
	if len(manifest.Capabilities) == 0 {
		return "{}"
	}
	return string(manifest.Capabilities)
}

func jsonObjectFromRaw(raw, key string) any {
	var value map[string]any
	if json.Unmarshal([]byte(defaultJSONObject(raw)), &value) != nil {
		return nil
	}
	return value[key]
}

func uniqueSortedStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	var result []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func intersectStrings(left, right []string) []string {
	set := make(map[string]bool, len(left))
	for _, value := range left {
		set[value] = true
	}
	var result []string
	for _, value := range right {
		if set[value] {
			result = append(result, value)
		}
	}
	return uniqueSortedStrings(result)
}

func governanceBlockedError(decision GovernanceDecision) error {
	for _, issue := range decision.Issues {
		if issue.Severity == GateSeverityBlocking {
			return fmt.Errorf("governance gate blocked: %s: %s", issue.Code, issue.Message)
		}
	}
	for _, issue := range decision.Issues {
		if issue.Severity == GateSeverityWarning {
			return fmt.Errorf("governance warning requires override: %s: %s", issue.Code, issue.Message)
		}
	}
	return errors.New("governance gate blocked")
}
