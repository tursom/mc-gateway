// internal/pluginmanager/governance.go 在涉及发布风险的操作前评估插件评审、公告、供应链和策略门禁。

package pluginmanager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"runtime"
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

func (m *Manager) EvaluateReleaseGate(ctx context.Context, pluginID, artifactID, action, profile, configJSON string) (GovernanceDecision, error) {
	decision, _, err := m.evaluateGovernance(ctx, pluginID, artifactID, action, profile, configJSON, true)
	if err != nil {
		return decision, err
	}
	if hasBlockingIssue(decision.Issues) {
		decision.OK = false
		return decision, governanceBlockedError(decision)
	}
	if hasWarningIssue(decision.Issues) {
		if _, ok, err := m.repo.ActiveWarningOverride(ctx, pluginID, artifactID, normalizeProfile(profile), action, decision.PolicyHash); err != nil {
			return decision, err
		} else if !ok {
			decision.OK = false
			return decision, governanceBlockedError(decision)
		}
		decision.WarningOverrideUsed = true
	}
	if !decision.OK {
		return decision, governanceBlockedError(decision)
	}
	return decision, nil
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
		Policy:           m.policySnapshot(profile),
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
	fingerprint := governanceFingerprint(plugin, artifact, manifest, policyHash(m.policySnapshot(req.Profile)))
	review := ReviewRecord{
		PluginID:          pluginID,
		ArtifactID:        req.ArtifactID,
		ArtifactHash:      fingerprint.ArtifactHash,
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
		"profile":       review.Profile,
		"risk_level":    review.RiskLevel,
		"decision":      review.Decision,
		"artifact_hash": review.ArtifactHash,
		"config_hash":   review.ConfigHash,
		"scope_hash":    review.ScopeHash,
		"rollout_hash":  review.RolloutHash,
		"runtime_hash":  review.RuntimeLimitsHash,
		"features_hash": review.FeaturesHash,
		"policy_hash":   review.PolicyHash,
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
	policy := m.policySnapshot(req.Profile)
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

func (m *Manager) SyncAdvisoryFeed(ctx context.Context, actor string, req AdvisoryFeedRequest) (AdvisoryFeedResult, error) {
	if strings.TrimSpace(req.Source) == "" {
		req.Source = "local-json"
	}
	if len(req.Advisories) == 0 {
		return AdvisoryFeedResult{}, errors.New("advisory feed contains no advisories")
	}
	records := make([]AdvisoryRecord, 0, len(req.Advisories))
	for _, advisoryReq := range req.Advisories {
		record, err := m.UpsertAdvisory(ctx, actor, advisoryReq)
		if err != nil {
			return AdvisoryFeedResult{}, err
		}
		records = append(records, record)
	}
	rescan, err := m.RescanAdvisories(ctx, actor, "", "")
	if err != nil {
		return AdvisoryFeedResult{}, err
	}
	result := AdvisoryFeedResult{
		Source:     req.Source,
		Imported:   len(records),
		Advisories: records,
		Rescan:     rescan,
		CreatedBy:  actor,
		CreatedAt:  m.repo.now().Unix(),
	}
	_ = m.repo.RecordOperation(ctx, "", "", "governance_advisory_feed", "succeeded", actor, "plugin advisory feed synced", map[string]any{
		"source":   result.Source,
		"imported": result.Imported,
		"matches":  len(result.Rescan.Matches),
		"blocking": result.Rescan.Blocking,
		"warnings": result.Rescan.Warnings,
	})
	return result, nil
}

func (m *Manager) RescanAdvisories(ctx context.Context, actor, pluginID, artifactID string) (AdvisoryRescanReport, error) {
	artifacts, err := m.repo.ListArtifacts(ctx, pluginID)
	if err != nil {
		return AdvisoryRescanReport{}, err
	}
	advisories, err := m.repo.ListAdvisories(ctx, pluginID)
	if err != nil {
		return AdvisoryRescanReport{}, err
	}
	plugins, err := m.repo.ListPlugins(ctx)
	if err != nil {
		return AdvisoryRescanReport{}, err
	}
	activeByArtifact := make(map[string]PluginRecord, len(plugins))
	for _, plugin := range plugins {
		if plugin.ActiveArtifactID != "" {
			activeByArtifact[plugin.ActiveArtifactID] = plugin
		}
	}
	report := AdvisoryRescanReport{
		PluginID:   pluginID,
		ArtifactID: artifactID,
		Advisories: len(advisories),
		OK:         true,
	}
	quarantined := map[string]bool{}
	for _, artifact := range artifacts {
		if artifactID != "" && artifact.ID != artifactID {
			continue
		}
		var manifest Manifest
		if err := json.Unmarshal([]byte(artifact.MetadataJSON), &manifest); err != nil {
			continue
		}
		report.Scanned++
		activePlugin, active := activeByArtifact[artifact.ID]
		for _, advisory := range advisories {
			if advisoryIgnored(advisory) || !advisoryMatches(advisory, artifact, manifest) {
				continue
			}
			severity := advisorySeverity(advisory)
			if severity == GateSeverityBlocking {
				report.Blocking++
				report.OK = false
			} else {
				report.Warnings++
			}
			runtimeState := ""
			if active {
				runtimeState = activePlugin.RuntimeState
			}
			report.Matches = append(report.Matches, AdvisoryRescanMatch{
				AdvisoryID:     advisory.AdvisoryID,
				Status:         advisory.Status,
				Action:         advisory.Action,
				Severity:       severity,
				PluginID:       artifact.PluginID,
				ArtifactID:     artifact.ID,
				ArtifactSHA256: artifact.SHA256,
				Version:        artifact.Version,
				RuntimeState:   runtimeState,
				Active:         active,
			})
			if active && (advisory.Action == AdvisoryActionQuarantine || advisory.Action == AdvisoryActionRevoke || advisory.Status == AdvisoryStatusRevoked) && !quarantined[advisory.AdvisoryID] {
				m.quarantineAffected(ctx, advisory)
				quarantined[advisory.AdvisoryID] = true
				report.QuarantineRuns++
			}
		}
	}
	sort.Slice(report.Matches, func(i, j int) bool {
		if report.Matches[i].PluginID != report.Matches[j].PluginID {
			return report.Matches[i].PluginID < report.Matches[j].PluginID
		}
		if report.Matches[i].ArtifactID != report.Matches[j].ArtifactID {
			return report.Matches[i].ArtifactID < report.Matches[j].ArtifactID
		}
		return report.Matches[i].AdvisoryID < report.Matches[j].AdvisoryID
	})
	if strings.TrimSpace(actor) == "" {
		actor = "system"
	}
	_ = m.repo.RecordOperation(ctx, pluginID, artifactID, "governance_advisory_rescan", "succeeded", actor, "plugin advisories rescanned", map[string]any{
		"scanned":         report.Scanned,
		"advisories":      report.Advisories,
		"matches":         len(report.Matches),
		"blocking":        report.Blocking,
		"warnings":        report.Warnings,
		"quarantine_runs": report.QuarantineRuns,
	})
	return report, nil
}

func (m *Manager) UpsertVulnerability(ctx context.Context, actor string, req VulnerabilityRequest) (VulnerabilityRecord, error) {
	record, err := m.repo.UpsertVulnerability(ctx, actor, req)
	if err != nil {
		return VulnerabilityRecord{}, err
	}
	if record.Action == AdvisoryActionQuarantine || record.Action == AdvisoryActionRevoke || record.Status == AdvisoryStatusRevoked {
		m.quarantineAffectedByVulnerability(ctx, record)
	}
	_ = m.repo.RecordOperation(ctx, "", "", "governance_vulnerability", "succeeded", actor, "plugin vulnerability upserted", map[string]any{
		"vulnerability_id": record.VulnerabilityID,
		"source":           record.Source,
		"status":           record.Status,
		"package_name":     record.PackageName,
		"version_range":    record.VersionRange,
		"severity":         record.Severity,
		"action":           record.Action,
		"fixed_version":    record.FixedVersion,
	})
	return record, nil
}

func (m *Manager) ImportVulnerabilityDB(ctx context.Context, actor string, req VulnerabilityDBRequest) (VulnerabilityDBResult, error) {
	if strings.TrimSpace(req.Source) == "" {
		req.Source = "local-json"
	}
	if len(req.Vulnerabilities) == 0 {
		return VulnerabilityDBResult{}, errors.New("vulnerability database contains no vulnerabilities")
	}
	records := make([]VulnerabilityRecord, 0, len(req.Vulnerabilities))
	for _, vulnerabilityReq := range req.Vulnerabilities {
		if vulnerabilityReq.Source == "" {
			vulnerabilityReq.Source = req.Source
		}
		record, err := m.repo.UpsertVulnerability(ctx, actor, vulnerabilityReq)
		if err != nil {
			return VulnerabilityDBResult{}, err
		}
		records = append(records, record)
	}
	scan, err := m.ScanVulnerabilities(ctx, actor, "", "")
	if err != nil {
		return VulnerabilityDBResult{}, err
	}
	result := VulnerabilityDBResult{
		Source:          req.Source,
		Imported:        len(records),
		Vulnerabilities: records,
		Scan:            scan,
		CreatedBy:       actor,
		CreatedAt:       m.repo.now().Unix(),
	}
	_ = m.repo.RecordOperation(ctx, "", "", "governance_vulnerability_db", "succeeded", actor, "plugin vulnerability database imported", map[string]any{
		"source":          result.Source,
		"imported":        result.Imported,
		"matches":         len(result.Scan.Matches),
		"blocking":        result.Scan.Blocking,
		"warnings":        result.Scan.Warnings,
		"quarantine_runs": result.Scan.QuarantineRuns,
	})
	return result, nil
}

func (m *Manager) ScanVulnerabilities(ctx context.Context, actor, pluginID, artifactID string) (VulnerabilityScanReport, error) {
	pluginID = strings.TrimSpace(pluginID)
	artifactID = strings.TrimSpace(artifactID)
	artifacts, err := m.repo.ListArtifacts(ctx, pluginID)
	if err != nil {
		return VulnerabilityScanReport{}, err
	}
	vulnerabilities, err := m.repo.ListVulnerabilities(ctx, "")
	if err != nil {
		return VulnerabilityScanReport{}, err
	}
	plugins, err := m.repo.ListPlugins(ctx)
	if err != nil {
		return VulnerabilityScanReport{}, err
	}
	activeByArtifact := make(map[string]PluginRecord, len(plugins))
	for _, plugin := range plugins {
		if plugin.ActiveArtifactID != "" {
			activeByArtifact[plugin.ActiveArtifactID] = plugin
		}
	}
	report := VulnerabilityScanReport{
		PluginID:        pluginID,
		ArtifactID:      artifactID,
		Vulnerabilities: len(vulnerabilities),
		OK:              true,
	}
	quarantined := map[string]bool{}
	for _, artifact := range artifacts {
		if artifactID != "" && artifact.ID != artifactID {
			continue
		}
		var manifest Manifest
		if err := json.Unmarshal([]byte(artifact.MetadataJSON), &manifest); err != nil {
			continue
		}
		report.Scanned++
		activePlugin, active := activeByArtifact[artifact.ID]
		runtimeState := ""
		if active {
			runtimeState = activePlugin.RuntimeState
		}
		for _, dep := range sbomDependencies(manifest) {
			for _, vulnerability := range vulnerabilities {
				if vulnerabilityIgnored(vulnerability) || !vulnerabilityMatchesDependency(vulnerability, dep) {
					continue
				}
				severity := vulnerabilityGateSeverity(vulnerability)
				if severity == GateSeverityBlocking {
					report.Blocking++
					report.OK = false
				} else {
					report.Warnings++
				}
				report.Matches = append(report.Matches, VulnerabilityScanMatch{
					VulnerabilityID: vulnerability.VulnerabilityID,
					Source:          vulnerability.Source,
					Status:          vulnerability.Status,
					PackageName:     vulnerability.PackageName,
					PackageVersion:  dep.Version,
					VersionRange:    vulnerability.VersionRange,
					Severity:        vulnerability.Severity,
					Action:          vulnerability.Action,
					PluginID:        artifact.PluginID,
					ArtifactID:      artifact.ID,
					ArtifactSHA256:  artifact.SHA256,
					RuntimeState:    runtimeState,
					Active:          active,
					FixedVersion:    vulnerability.FixedVersion,
					Summary:         vulnerability.Summary,
				})
				key := vulnerability.VulnerabilityID + "|" + vulnerability.PackageName
				if active && (vulnerability.Action == AdvisoryActionQuarantine || vulnerability.Action == AdvisoryActionRevoke || vulnerability.Status == AdvisoryStatusRevoked) && !quarantined[key] {
					m.quarantineAffectedByVulnerability(ctx, vulnerability)
					quarantined[key] = true
					report.QuarantineRuns++
				}
			}
		}
	}
	sort.Slice(report.Matches, func(i, j int) bool {
		if report.Matches[i].PluginID != report.Matches[j].PluginID {
			return report.Matches[i].PluginID < report.Matches[j].PluginID
		}
		if report.Matches[i].ArtifactID != report.Matches[j].ArtifactID {
			return report.Matches[i].ArtifactID < report.Matches[j].ArtifactID
		}
		if report.Matches[i].PackageName != report.Matches[j].PackageName {
			return report.Matches[i].PackageName < report.Matches[j].PackageName
		}
		return report.Matches[i].VulnerabilityID < report.Matches[j].VulnerabilityID
	})
	if strings.TrimSpace(actor) == "" {
		actor = "system"
	}
	_ = m.repo.RecordOperation(ctx, pluginID, artifactID, "governance_vulnerability_scan", "succeeded", actor, "plugin vulnerability database scanned", map[string]any{
		"scanned":         report.Scanned,
		"vulnerabilities": report.Vulnerabilities,
		"matches":         len(report.Matches),
		"blocking":        report.Blocking,
		"warnings":        report.Warnings,
		"quarantine_runs": report.QuarantineRuns,
	})
	return report, nil
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

func (m *Manager) ListVulnerabilities(ctx context.Context, packageName string) ([]VulnerabilityRecord, error) {
	return m.repo.ListVulnerabilities(ctx, packageName)
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
	policy := m.policySnapshot(profile)
	policyHash := policyHash(policy)
	plugin, err := m.repo.Plugin(ctx, pluginID)
	if err != nil {
		if !preview || !errors.Is(err, ErrPluginNotFound) {
			return GovernanceDecision{}, ConflictAnalysis{}, err
		}
		if configJSON == "" {
			configJSON = "{}"
		}
		plugin = PluginRecord{
			ID:                pluginID,
			DesiredArtifactID: artifactID,
			DesiredState:      DesiredDisabled,
			RuntimeState:      RuntimeDisabled,
			Priority:          DefaultPriority,
			ConfigJSON:        configJSON,
			DesiredGeneration: 1,
		}
	} else {
		if artifactID == "" {
			artifactID = plugin.DesiredArtifactID
		}
		if configJSON == "" {
			configJSON = plugin.ConfigJSON
		}
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
	vulnerabilityIssues, err := m.vulnerabilityIssues(ctx, artifact, manifest)
	if err != nil {
		return GovernanceDecision{}, ConflictAnalysis{}, err
	}
	issues = append(issues, vulnerabilityIssues...)
	supplyChainIssues, err := m.supplyChainGovernanceIssues(ctx, artifact)
	if err != nil {
		return GovernanceDecision{}, ConflictAnalysis{}, err
	}
	issues = append(issues, supplyChainIssues...)
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
	policy := m.policySnapshot(profile)
	if artifact.RuntimeType == RuntimeWASM && normalizeProfile(profile) == PolicyProfileProd {
		policy.RequireConformanceFixture = true
	}
	var requiredConformanceCoverage []string
	if artifact.RuntimeType == RuntimeSandbox {
		if normalizeProfile(profile) == PolicyProfileProd {
			policy.RequireConformanceFixture = true
		}
		if policy.RequireConformanceFixture {
			requiredConformanceCoverage = SandboxConformanceRequiredCoverage()
		}
	}
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
	if check, ok := conformancePreflightCheck(artifact.MetadataJSON, policy, requiredConformanceCoverage); ok {
		result.Checks = append(result.Checks, check)
	}
	if artifact.RuntimeType == RuntimeSandbox {
		if err := validateSandboxArtifactMetadata(artifact, manifest); err != nil {
			result.Checks = append(result.Checks, PreflightCheck{
				Code:     "sandbox_artifact_metadata_invalid",
				Severity: GateSeverityBlocking,
				Message:  err.Error(),
				Details: map[string]any{
					"reason_code":  reasonCodeFromError(err),
					"runtime_type": artifact.RuntimeType,
					"protocol":     manifest.Runtime.Protocol,
					"abi_version":  manifest.Runtime.ABIVersion,
					"os":           manifest.Runtime.OS,
					"arch":         manifest.Runtime.Arch,
				},
			})
		}
		if m.serviceMode != PluginServiceModeSandboxProcess {
			result.Checks = append(result.Checks, PreflightCheck{Code: "sandbox_runtime_disabled", Severity: GateSeverityBlocking, Message: "sandbox-process runtime is disabled by plugin service mode"})
		} else if reasonCode, message := m.validateSandboxServiceModeApply(); reasonCode != "" {
			result.Checks = append(result.Checks, PreflightCheck{Code: "sandbox_runtime_disabled", Severity: GateSeverityBlocking, Message: message, Details: map[string]any{"reason_code": reasonCode}})
		}
	}
	if artifact.RuntimeType == RuntimeWASM {
		wasmGateOK := true
		if err := validateWASMExtensionPoints(manifest); err != nil {
			wasmGateOK = false
			result.Checks = append(result.Checks, PreflightCheck{
				Code:     "wasm_extension_point_unsupported",
				Severity: GateSeverityBlocking,
				Message:  err.Error(),
			})
		}
		if blocked := wasmBlockedHostCapabilities(manifest, artifact); len(blocked) > 0 {
			wasmGateOK = false
			result.Checks = append(result.Checks, PreflightCheck{
				Code:     "wasm_capability_blocked",
				Severity: GateSeverityBlocking,
				Message:  "wasm runtime does not support file, network, env, or secret host capabilities",
				Details:  map[string]any{"capabilities": blocked},
			})
		}
		if err := validateWASMArtifactABI(ctx, artifact, manifest); err != nil {
			wasmGateOK = false
			result.Checks = append(result.Checks, PreflightCheck{
				Code:     "wasm_abi_invalid",
				Severity: GateSeverityBlocking,
				Message:  err.Error(),
				Details: map[string]any{
					"host_abi": wasmHostABIV1,
					"exports":  wasmABIExportMap(),
					"imports":  wasmABIImportMap(),
				},
			})
		}
		if wasmGateOK {
			result.Checks = append(result.Checks, m.wasmResourceLimitSmokePreflightCheck(ctx, artifact, manifest))
		}
	}
	if caps := requiredRuntimeCapabilities(artifact); artifact.RuntimeType == RuntimeSandbox {
		if missing := sandboxExternalDependencyCapabilityMissing(manifest, caps); len(missing) > 0 {
			result.Checks = append(result.Checks, PreflightCheck{
				Code:     "sandbox_external_dependency_capability_missing",
				Severity: GateSeverityBlocking,
				Message:  "sandbox-process external dependencies must declare runtime capability network.egress",
				Details:  map[string]any{"reason_code": ReasonSandboxExternalCapabilityMissing, "dependencies": missing, "required_capability": "network.egress"},
			})
		}
		if unsupported := unsupportedSandboxRequiredCapabilities(m.sandboxPolicy, caps); len(unsupported) > 0 {
			result.Checks = append(result.Checks, PreflightCheck{
				Code:     "capability_enforcement_unavailable",
				Severity: GateSeverityBlocking,
				Message:  "runtime required capabilities cannot be enforced by this gateway: " + strings.Join(unsupported, ","),
				Details:  map[string]any{"reason_code": ReasonSandboxCapabilityBlock, "runtime_type": artifact.RuntimeType, "capabilities": unsupported},
			})
		}
	} else if caps := requiredRuntimeCapabilities(artifact); runtimeRequiredCapabilitiesUnsupported(artifact.RuntimeType) && len(caps) > 0 {
		result.Checks = append(result.Checks, PreflightCheck{
			Code:     "capability_enforcement_unavailable",
			Severity: GateSeverityBlocking,
			Message:  "runtime required capabilities cannot be enforced by this gateway: " + strings.Join(caps, ","),
			Details:  map[string]any{"runtime_type": artifact.RuntimeType, "capabilities": caps},
		})
	}
	if manifestHasExtensionPoint(manifest, ExtensionIngressService) {
		var summary CapabilitySummary
		ingressDetails := IngressCapabilityDetails(nil)
		if err := json.Unmarshal([]byte(defaultJSONObject(artifact.CapabilitiesSummaryJSON)), &summary); err != nil {
			result.Checks = append(result.Checks, PreflightCheck{
				Code:     "ingress_service_invalid",
				Severity: GateSeverityBlocking,
				Message:  "ingress.service/v1 capability summary could not be parsed",
				Details:  map[string]any{"extension_point": ExtensionIngressService, "error": err.Error()},
			})
		} else {
			ingressDetails = IngressCapabilityDetails(summary.Ingress)
			if problems := ValidateIngressCapability(manifest, summary.Ingress); len(problems) > 0 {
				details := IngressCapabilityDetails(summary.Ingress)
				details["errors"] = problems
				result.Checks = append(result.Checks, PreflightCheck{
					Code:     "ingress_service_invalid",
					Severity: GateSeverityBlocking,
					Message:  "ingress.service/v1 capability declaration is invalid",
					Details:  details,
				})
			} else {
				result.Checks = append(result.Checks, PreflightCheck{
					Code:     "ingress_service_schema_valid",
					Severity: GateSeverityInfo,
					Message:  "ingress.service/v1 capability declaration passed reserved schema checks",
					Details:  ingressDetails,
				})
			}
		}
		if !m.futureGates.IngressEnabled() {
			result.Checks = append(result.Checks, PreflightCheck{
				Code:     "ingress_service_disabled",
				Severity: GateSeverityBlocking,
				Message:  "ingress.service/v1 data plane is disabled by feature gate",
				Details:  ingressDetails,
			})
		}
	}
	if normalizeProfile(profile) == PolicyProfileProd {
		result.Checks = append(result.Checks, m.sourceBuildProvenancePreflightChecks(ctx, artifact)...)
		result.Checks = append(result.Checks, m.externalCIProvenancePreflightChecks(ctx, artifact)...)
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
	if external := externalDependencies(manifest); len(external) > 0 {
		result.Checks = append(result.Checks, PreflightCheck{Code: "external_dependencies_declared", Severity: GateSeverityInfo, Message: "plugin declares external dependencies", Details: map[string]any{"dependencies": external}})
	}
	for _, dep := range manifest.ExternalDeps {
		policy := normalizeExternalFailPolicy(dep)
		details := map[string]any{
			"dependency":  dep.Name,
			"purpose":     dep.Purpose,
			"required":    dep.Required,
			"fail_policy": policy,
		}
		if !validExternalFailPolicy(policy) {
			result.Checks = append(result.Checks, PreflightCheck{Code: "external_dependency_policy_invalid", Severity: GateSeverityBlocking, Message: "external dependency fail policy is invalid", Details: details})
			continue
		}
		if dep.Required && policy == ExternalFailPolicyOpen {
			result.Checks = append(result.Checks, PreflightCheck{Code: "external_dependency_fail_open_required", Severity: GateSeverityWarning, Message: "required external dependency is configured fail-open", Details: details})
		}
		if dep.Name == "" || m.operations == nil {
			continue
		}
		summary, err := m.operations.ForPlugin(plugin.ID, artifact.ID, manifest).ExternalDependencySummary(dep.Name)
		if err != nil {
			continue
		}
		if summary.CircuitState == circuitOpen || summary.LastStatus == "health_failed" {
			details["last_status"] = summary.LastStatus
			details["circuit_state"] = summary.CircuitState
			details["recent_error"] = summary.RecentError
			severity := GateSeverityWarning
			code := "external_dependency_unhealthy"
			if dep.Required && policy == ExternalFailPolicyClosed {
				severity = GateSeverityBlocking
				code = "external_dependency_fail_closed_unhealthy"
			}
			result.Checks = append(result.Checks, PreflightCheck{Code: code, Severity: severity, Message: "external dependency is not healthy", Details: details})
		}
	}
	result.OK = !preflightHasBlocking(result.Checks)
	return result
}

func conformancePreflightCheck(metadataJSON string, policy PolicySnapshot, requiredCoverage []string) (PreflightCheck, bool) {
	summary, ok, err := conformanceSummaryFromMetadata(metadataJSON)
	if err != nil {
		return PreflightCheck{
			Code:     "conformance_fixture_invalid",
			Severity: GateSeverityBlocking,
			Message:  "packaged conformance evidence could not be parsed",
			Details:  map[string]any{"error": err.Error()},
		}, true
	}
	if !ok {
		if policy.RequireConformanceFixture {
			return PreflightCheck{
				Code:     "conformance_fixture_missing",
				Severity: GateSeverityBlocking,
				Message:  "packaged conformance fixture is required by policy",
				Details:  map[string]any{"profile": policy.Profile},
			}, true
		}
		return PreflightCheck{}, false
	}
	details := map[string]any{
		"source":  summary.Source,
		"total":   summary.Total,
		"passed":  summary.Passed,
		"skipped": summary.Skipped,
		"failed":  summary.Failed,
	}
	if len(summary.FailedFixtures) > 0 {
		details["failed_fixtures"] = summary.FailedFixtures
	}
	if !summary.OK {
		return PreflightCheck{
			Code:     "conformance_fixture_failed",
			Severity: GateSeverityBlocking,
			Message:  "packaged conformance fixture failed",
			Details:  details,
		}, true
	}
	if missing := missingConformanceCoverage(summary, requiredCoverage); len(missing) > 0 {
		details["coverage"] = summary.Coverage
		details["missing_coverage"] = missing
		return PreflightCheck{
			Code:     "sandbox_conformance_fixture_missing",
			Severity: GateSeverityBlocking,
			Message:  "packaged sandbox conformance fixture coverage is incomplete",
			Details:  details,
		}, true
	}
	return PreflightCheck{
		Code:     "conformance_fixture_passed",
		Severity: GateSeverityInfo,
		Message:  "packaged conformance fixtures passed",
		Details:  details,
	}, true
}

func conformanceSummaryFromMetadata(metadataJSON string) (ConformanceSummary, bool, error) {
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal([]byte(defaultJSONObject(metadataJSON)), &metadata); err != nil {
		return ConformanceSummary{}, false, err
	}
	raw, ok := metadata["conformance"]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return ConformanceSummary{}, false, nil
	}
	var summary ConformanceSummary
	if err := json.Unmarshal(raw, &summary); err != nil {
		return ConformanceSummary{}, true, err
	}
	return summary, true, nil
}

func (m *Manager) wasmResourceLimitSmokePreflightCheck(ctx context.Context, artifact ArtifactRecord, manifest Manifest) PreflightCheck {
	module, err := wasmArtifactModule(artifact)
	if err != nil {
		return PreflightCheck{
			Code:     "wasm_resource_limit_smoke_failed",
			Severity: GateSeverityBlocking,
			Message:  err.Error(),
		}
	}
	runner := m.wasmRunner
	if runner == nil {
		runner = NewWASMRunner()
	}
	moduleHash, cacheStatus, err := runner.PrepareModule(ctx, WASMInvocation{
		ArtifactID:  artifact.ID,
		Module:      module,
		Manifest:    manifest,
		Function:    wasmDefaultExport(manifest),
		Timeout:     wasmTimeout(manifest),
		MemoryBytes: manifest.RuntimeLimits.MemoryBytes,
	})
	if err != nil {
		return PreflightCheck{
			Code:     "wasm_resource_limit_smoke_failed",
			Severity: GateSeverityBlocking,
			Message:  err.Error(),
			Details: map[string]any{
				"handler_timeout_ms": manifest.RuntimeLimits.HandlerTimeoutMS,
				"memory_bytes":       manifest.RuntimeLimits.MemoryBytes,
			},
		}
	}
	return PreflightCheck{
		Code:     "wasm_resource_limit_smoke_passed",
		Severity: GateSeverityInfo,
		Message:  "wasm module compiled and validated within declared resource limits",
		Details: map[string]any{
			"module_sha256":      moduleHash,
			"module_cache":       cacheStatus,
			"handler_timeout_ms": manifest.RuntimeLimits.HandlerTimeoutMS,
			"memory_bytes":       manifest.RuntimeLimits.MemoryBytes,
		},
	}
}

func (m *Manager) sourceBuildProvenancePreflightChecks(ctx context.Context, artifact ArtifactRecord) []PreflightCheck {
	build, ok, err := m.sourceBuildForArtifact(ctx, artifact)
	if err != nil {
		return []PreflightCheck{{
			Code:     "source_build_provenance_unavailable",
			Severity: GateSeverityBlocking,
			Message:  "source build provenance could not be read",
			Details:  map[string]any{"error": err.Error()},
		}}
	}
	if !ok {
		return nil
	}
	details := map[string]any{
		"build_id":     build.ID,
		"source_id":    build.SourceID,
		"builder_type": build.BuilderType,
	}
	if build.BuilderImage != "" {
		details["builder_image"] = build.BuilderImage
	}
	if build.SourceSHA256 != "" {
		details["source_sha256"] = build.SourceSHA256
	}
	if build.ArtifactSHA256 != "" {
		details["artifact_sha256"] = build.ArtifactSHA256
	}
	if build.ABIFingerprint != "" {
		details["abi_fingerprint"] = build.ABIFingerprint
	}
	builderIdentity := sourceBuildBuilderIdentity(build)
	for k, v := range builderIdentity {
		details[k] = v
	}

	var checks []PreflightCheck
	switch build.BuilderType {
	case BuilderTypeLocalProcess:
		checks = append(checks, PreflightCheck{
			Code:     "source_build_local_process_prod",
			Severity: GateSeverityBlocking,
			Message:  "prod profile does not admit artifacts built by local-process source builder",
			Details:  details,
		})
	case BuilderTypeContainer:
		if strings.TrimSpace(build.BuilderImage) == "" {
			checks = append(checks, PreflightCheck{
				Code:     "source_build_provenance_incomplete",
				Severity: GateSeverityBlocking,
				Message:  "source build provenance is missing required fields for prod admission",
				Details:  map[string]any{"missing": []string{"builder_image"}, "build_id": build.ID, "builder_type": build.BuilderType},
			})
		}
		metadata := sourceBuildMetadata(build)
		digest, _ := metadata["builder_image_digest"].(string)
		if strings.TrimSpace(digest) == "" {
			checks = append(checks, PreflightCheck{
				Code:     "source_build_builder_digest_missing",
				Severity: GateSeverityWarning,
				Message:  "container source build does not include a builder image digest",
				Details:  details,
			})
		}
		if pinned, _ := builderIdentity["builder_image_pinned"].(bool); !pinned {
			checks = append(checks, PreflightCheck{
				Code:     "source_build_builder_image_not_pinned",
				Severity: GateSeverityWarning,
				Message:  "container source build does not use a digest-pinned builder image reference",
				Details:  details,
			})
		}
		if releaseBound, _ := builderIdentity["builder_image_release_bound"].(bool); !releaseBound {
			checks = append(checks, PreflightCheck{
				Code:     "source_build_builder_image_release_unbound",
				Severity: GateSeverityWarning,
				Message:  "container source build is not bound to the gateway plugin API and Go release",
				Details:  details,
			})
		}
	case "":
		checks = append(checks, PreflightCheck{
			Code:     "source_build_provenance_incomplete",
			Severity: GateSeverityBlocking,
			Message:  "source build provenance is missing required fields for prod admission",
			Details:  map[string]any{"missing": []string{"builder_type"}, "build_id": build.ID},
		})
	default:
		checks = append(checks, PreflightCheck{
			Code:     "source_build_builder_unsupported",
			Severity: GateSeverityBlocking,
			Message:  "source build uses an unsupported builder type for prod admission",
			Details:  details,
		})
	}
	if missing := missingSourceBuildProvenanceFields(build); len(missing) > 0 {
		checks = append(checks, PreflightCheck{
			Code:     "source_build_provenance_incomplete",
			Severity: GateSeverityBlocking,
			Message:  "source build provenance is missing required fields for prod admission",
			Details:  map[string]any{"missing": missing, "build_id": build.ID, "builder_type": build.BuilderType},
		})
	}
	if artifact.RuntimeType == RuntimeWASM {
		if missing := missingWASMSourceBuildProvenanceFields(artifact, build); len(missing) > 0 {
			checks = append(checks, PreflightCheck{
				Code:     "wasm_source_build_provenance_incomplete",
				Severity: GateSeverityBlocking,
				Message:  "wasm source build provenance is missing required fields for prod admission",
				Details:  map[string]any{"missing": missing, "build_id": build.ID, "builder_type": build.BuilderType},
			})
		}
		if moduleSHA := wasmSourceBuildModuleSHA(build); moduleSHA != "" && artifact.SHA256 != "" && !strings.EqualFold(moduleSHA, artifact.SHA256) {
			checks = append(checks, PreflightCheck{
				Code:     "wasm_source_build_module_hash_mismatch",
				Severity: GateSeverityBlocking,
				Message:  "wasm source build module hash does not match stored artifact",
				Details: map[string]any{
					"build_id":               build.ID,
					"build_module_sha256":    moduleSHA,
					"artifact_module_sha256": artifact.SHA256,
				},
			})
		}
	}
	if build.ArtifactSHA256 != "" && artifact.SHA256 != "" && build.ArtifactSHA256 != artifact.SHA256 {
		checks = append(checks, PreflightCheck{
			Code:     "source_build_artifact_hash_mismatch",
			Severity: GateSeverityBlocking,
			Message:  "source build artifact hash does not match stored artifact",
			Details: map[string]any{
				"build_id":              build.ID,
				"build_artifact_sha256": build.ArtifactSHA256,
				"artifact_sha256":       artifact.SHA256,
			},
		})
	}
	return checks
}

func (m *Manager) sourceBuildForArtifact(ctx context.Context, artifact ArtifactRecord) (BuildRecord, bool, error) {
	builds, err := m.repo.ListBuilds(ctx, artifact.PluginID)
	if err != nil {
		return BuildRecord{}, false, err
	}
	for _, build := range builds {
		if build.ArtifactID == artifact.ID && build.Status == BuildStatusSucceeded {
			return build, true, nil
		}
	}
	return BuildRecord{}, false, nil
}

func missingSourceBuildProvenanceFields(build BuildRecord) []string {
	var missing []string
	if strings.TrimSpace(build.SourceSHA256) == "" {
		missing = append(missing, "source_sha256")
	}
	if strings.TrimSpace(build.ArtifactSHA256) == "" {
		missing = append(missing, "artifact_sha256")
	}
	if strings.TrimSpace(build.GoVersion) == "" {
		missing = append(missing, "go_version")
	}
	if strings.TrimSpace(build.ModuleSummary) == "" {
		missing = append(missing, "module_summary")
	}
	if strings.TrimSpace(build.ABIFingerprint) == "" {
		missing = append(missing, "abi_fingerprint")
	}
	return missing
}

func missingWASMSourceBuildProvenanceFields(artifact ArtifactRecord, build BuildRecord) []string {
	metadata := sourceBuildMetadata(build)
	var missing []string
	if wasmSourceBuildToolchain(metadata) == "" {
		missing = append(missing, "wasm_toolchain")
	}
	if wasmSourceBuildTarget(metadata) == "" {
		missing = append(missing, "wasm_target")
	}
	if wasmSourceBuildModuleSHA(build) == "" {
		missing = append(missing, "wasm_module_sha256")
	}
	var manifest Manifest
	if json.Unmarshal([]byte(defaultJSONObject(artifact.MetadataJSON)), &manifest) != nil || len(sbomDependencies(manifest)) == 0 {
		if !metadataFieldPresent(metadata["sbom"]) && !metadataFieldPresent(metadata["sbom_dependencies"]) {
			missing = append(missing, "sbom")
		}
	}
	return missing
}

func sourceBuildBuilderIdentity(build BuildRecord) map[string]any {
	identity := map[string]any{
		"builder_image_pinned":                builderImageDigestPinned(build.BuilderImage),
		"builder_image_gateway_release_bound": builderImageReferencesGatewayRelease(build.BuilderImage),
		"builder_image_go_version_bound":      builderImageReferencesGoVersion(build.BuilderImage, build.GoVersion),
		"builder_image_api_version_bound":     builderImageReferencesAPIVersion(build.BuilderImage),
		"builder_image_target_bound":          builderImageReferencesTarget(build.BuilderImage, runtime.GOOS, runtime.GOARCH),
		"builder_image_release_bound":         builderImageReleaseBound(build.BuilderImage, build.GoVersion),
		"builder_image_official_name":         builderImageOfficialName(build.BuilderImage),
		"gateway_go_version":                  runtime.Version(),
		"gateway_go_os":                       runtime.GOOS,
		"gateway_go_arch":                     runtime.GOARCH,
	}
	if build.GoVersion != "" {
		identity["builder_go_version"] = build.GoVersion
		identity["builder_go_version_matches_gateway"] = build.GoVersion == runtime.Version()
	}
	if build.GOOS != "" || build.GOARCH != "" {
		identity["builder_target_go_os"] = build.GOOS
		identity["builder_target_go_arch"] = build.GOARCH
		identity["builder_target_matches_gateway"] = build.GOOS == runtime.GOOS && build.GOARCH == runtime.GOARCH
	}
	return identity
}

func builderImageDigestPinned(image string) bool {
	image = strings.TrimSpace(image)
	const marker = "@sha256:"
	idx := strings.LastIndex(image, marker)
	if idx < 0 {
		return false
	}
	digest := image[idx+len(marker):]
	if len(digest) != 64 {
		return false
	}
	for _, r := range digest {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			continue
		}
		return false
	}
	return true
}

func builderImageReferencesGoVersion(image, goVersion string) bool {
	image = strings.TrimSpace(image)
	goVersion = strings.TrimSpace(goVersion)
	if image == "" || goVersion == "" {
		return false
	}
	if before, _, ok := strings.Cut(image, "@"); ok {
		image = before
	}
	normalized := strings.TrimPrefix(goVersion, "go")
	if normalized == "" {
		return false
	}
	lower := strings.ToLower(image)
	return strings.Contains(lower, "go"+strings.ToLower(normalized)) ||
		strings.Contains(lower, ":"+strings.ToLower(normalized)) ||
		strings.Contains(lower, "-"+strings.ToLower(normalized)) ||
		strings.Contains(builderImageToken(image), builderImageToken("go"+normalized))
}

func builderImageReferencesAPIVersion(image string) bool {
	image = builderImageWithoutDigest(image)
	apiToken := builderImageToken(APIVersion)
	return image != "" && apiToken != "" && strings.Contains(builderImageToken(image), apiToken)
}

func builderImageReferencesGatewayRelease(image string) bool {
	image = builderImageWithoutDigest(image)
	releaseToken := builderImageToken(GatewayRelease)
	return image != "" && releaseToken != "" && strings.Contains(builderImageToken(image), releaseToken)
}

func builderImageReleaseBound(image, goVersion string) bool {
	return builderImageDigestPinned(image) &&
		builderImageReferencesGatewayRelease(image) &&
		builderImageReferencesGoVersion(image, goVersion) &&
		builderImageReferencesAPIVersion(image) &&
		builderImageReferencesTarget(image, runtime.GOOS, runtime.GOARCH)
}

func builderImageOfficialName(image string) bool {
	image = strings.ToLower(builderImageWithoutDigest(image))
	return strings.Contains(image, "mc-gateway-plugin-builder")
}

func builderImageReferencesTarget(image, goos, goarch string) bool {
	imageToken := builderImageToken(builderImageWithoutDigest(image))
	return imageToken != "" &&
		strings.Contains(imageToken, builderImageToken(goos)) &&
		strings.Contains(imageToken, builderImageToken(goarch))
}

func builderImageWithoutDigest(image string) string {
	image = strings.TrimSpace(image)
	if before, _, ok := strings.Cut(image, "@"); ok {
		return before
	}
	return image
}

func builderImageToken(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	lastDash := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			out.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			out.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(out.String(), "-")
}

func sourceBuildMetadata(build BuildRecord) map[string]any {
	var metadata map[string]any
	if json.Unmarshal([]byte(defaultJSONObject(build.MetadataJSON)), &metadata) != nil {
		return nil
	}
	return metadata
}

func wasmSourceBuildToolchain(metadata map[string]any) string {
	return firstMetadataString(metadata, "wasm_toolchain", "toolchain", "runtime_toolchain")
}

func wasmSourceBuildTarget(metadata map[string]any) string {
	return firstMetadataString(metadata, "wasm_target", "target", "runtime_target")
}

func wasmSourceBuildModuleSHA(build BuildRecord) string {
	metadata := sourceBuildMetadata(build)
	if value := firstMetadataString(metadata, "wasm_module_sha256", "module_sha256"); value != "" {
		return value
	}
	return strings.TrimSpace(build.ArtifactSHA256)
}

func (m *Manager) attachSourceBuildAssessmentMetadata(ctx context.Context, artifact ArtifactRecord, metadata map[string]any) (map[string]any, error) {
	build, ok, err := m.sourceBuildForArtifact(ctx, artifact)
	if err != nil || !ok {
		return metadata, err
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	sourceBuild := jsonMapFromAny(metadata["source_build"])
	if sourceBuild == nil {
		sourceBuild = map[string]any{}
	}
	buildMetadata := sourceBuildMetadata(build)
	digest, _ := buildMetadata["builder_image_digest"].(string)
	sourceBuild["build_id"] = build.ID
	sourceBuild["source_id"] = build.SourceID
	sourceBuild["builder_type"] = build.BuilderType
	sourceBuild["builder_image"] = build.BuilderImage
	sourceBuild["builder_image_digest"] = digest
	sourceBuild["builder_version"] = build.BuilderVersion
	sourceBuild["source_sha256"] = build.SourceSHA256
	sourceBuild["artifact_sha256"] = build.ArtifactSHA256
	sourceBuild["go_version"] = build.GoVersion
	sourceBuild["go_os"] = build.GOOS
	sourceBuild["go_arch"] = build.GOARCH
	sourceBuild["module_summary"] = json.RawMessage(defaultJSONArray(build.ModuleSummary))
	sourceBuild["abi_fingerprint"] = build.ABIFingerprint
	builderIdentity := sourceBuildBuilderIdentity(build)
	for k, v := range builderIdentity {
		sourceBuild[k] = v
	}
	if artifact.RuntimeType == RuntimeWASM {
		buildMetadata := sourceBuildMetadata(build)
		if toolchain := wasmSourceBuildToolchain(buildMetadata); toolchain != "" {
			sourceBuild["wasm_toolchain"] = toolchain
		}
		if target := wasmSourceBuildTarget(buildMetadata); target != "" {
			sourceBuild["wasm_target"] = target
		}
		if moduleSHA := wasmSourceBuildModuleSHA(build); moduleSHA != "" {
			sourceBuild["wasm_module_sha256"] = moduleSHA
			sourceBuild["wasm_module_sha256_matches"] = artifact.SHA256 == "" || strings.EqualFold(moduleSHA, artifact.SHA256)
		}
		if sbom := buildMetadata["sbom"]; metadataFieldPresent(sbom) {
			sourceBuild["sbom"] = sbom
		}
		if deps := buildMetadata["sbom_dependencies"]; metadataFieldPresent(deps) {
			sourceBuild["sbom_dependencies"] = deps
		}
		if wasmMissing := missingWASMSourceBuildProvenanceFields(artifact, build); len(wasmMissing) > 0 {
			sourceBuild["wasm_missing_fields"] = wasmMissing
			sourceBuild["wasm_provenance_complete"] = false
		} else {
			sourceBuild["wasm_provenance_complete"] = true
		}
	}
	missing := missingSourceBuildProvenanceFields(build)
	sourceBuild["provenance_complete"] = len(missing) == 0
	if len(missing) > 0 {
		sourceBuild["missing_fields"] = missing
	}
	builderImagePinned, _ := builderIdentity["builder_image_pinned"].(bool)
	builderImageReleaseBound, _ := builderIdentity["builder_image_release_bound"].(bool)
	switch {
	case build.BuilderType == BuilderTypeLocalProcess:
		sourceBuild["prod_admission"] = "blocked"
	case len(missing) > 0:
		sourceBuild["prod_admission"] = "blocked"
	case build.BuilderType == BuilderTypeContainer && (strings.TrimSpace(digest) == "" || !builderImagePinned || !builderImageReleaseBound):
		sourceBuild["prod_admission"] = "warning"
	default:
		sourceBuild["prod_admission"] = "allowed"
	}
	metadata["source_build"] = sourceBuild
	return metadata, nil
}

func (m *Manager) externalCIProvenancePreflightChecks(ctx context.Context, artifact ArtifactRecord) []PreflightCheck {
	assessments, err := m.repo.ListSupplyChainAssessments(ctx, artifact.PluginID, artifact.ID)
	if err != nil {
		return []PreflightCheck{{
			Code:     "external_ci_assessment_unavailable",
			Severity: GateSeverityBlocking,
			Message:  "external CI supply-chain assessment could not be read",
			Details:  map[string]any{"error": err.Error()},
		}}
	}
	var latest *SupplyChainAssessment
	for idx := range assessments {
		assessment := assessments[idx]
		if jsonMapFromAny(assessment.Metadata["external_ci"]) != nil {
			latest = &assessment
			break
		}
	}
	artifactMetadata := jsonMap(artifact.MetadataJSON)
	if latest == nil {
		if externalCI := jsonMapFromAny(artifactMetadata["external_ci"]); externalCI != nil {
			var manifest Manifest
			_ = json.Unmarshal([]byte(defaultJSONObject(artifact.MetadataJSON)), &manifest)
			issues := supplyChainIssues(artifact, manifest, map[string]any{
				"signature":   jsonMapFromAny(artifactMetadata["signature"]),
				"sbom":        jsonMapFromAny(artifactMetadata["sbom"]),
				"external_ci": externalCI,
			})
			return preflightChecksFromGovernanceIssues(issues)
		}
		return nil
	}
	return preflightChecksFromGovernanceIssues(latest.Issues)
}

func (m *Manager) conflictAnalysis(ctx context.Context, target PluginRecord, artifact ArtifactRecord, manifest Manifest) (ConflictAnalysis, error) {
	analysis := ConflictAnalysis{CreatedAt: m.repo.now().Unix(), Plan: m.DispatchPlan(ctx)}
	targetIngress := manifestHasExtensionPoint(manifest, ExtensionIngressService)
	if !targetIngress {
		analysis.OK = true
		return analysis, nil
	}
	plugins, err := m.repo.ListPlugins(ctx)
	if err != nil {
		return ConflictAnalysis{}, err
	}
	if targetIngress {
		targetIngressCapability := ingressCapabilityFromArtifact(artifact, manifest)
		for _, listener := range m.ingressReservedListeners {
			if ingressReservedListenerConflict(targetIngressCapability, listener) {
				analysis.Issues = append(analysis.Issues, issue("ingress_reserved_listener_conflict", GateSeverityBlocking, "ingress.service/v1 port overlaps reserved gateway listener", target.ID, artifact.ID, map[string]any{
					"service":          listener.Name,
					"listener_network": normalizedReservedListenerNetwork(listener.Network),
					"bind":             strings.TrimSpace(targetIngressCapability.Bind),
					"port":             targetIngressCapability.Port,
					"reserved_bind":    normalizedReservedListenerBind(listener.Bind),
					"reserved_port":    listener.Port,
				}))
			}
		}
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
			if !manifestHasExtensionPoint(otherManifest, ExtensionIngressService) {
				continue
			}
			otherIngressCapability := ingressCapabilityFromArtifact(otherArtifact, otherManifest)
			if ingressCapabilitiesConflict(targetIngressCapability, otherIngressCapability) {
				analysis.Issues = append(analysis.Issues, issue("ingress_port_conflict", GateSeverityBlocking, "ingress.service/v1 port overlaps enabled plugin ingress declaration", target.ID, artifact.ID, map[string]any{
					"other_plugin_id":   plugin.ID,
					"other_artifact_id": otherArtifact.ID,
					"protocol":          strings.TrimSpace(targetIngressCapability.Protocol),
					"bind":              strings.TrimSpace(targetIngressCapability.Bind),
					"port":              targetIngressCapability.Port,
					"other_bind":        strings.TrimSpace(otherIngressCapability.Bind),
					"listener_network":  ingressListenerNetwork(targetIngressCapability.Protocol),
				}))
			}
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

func ingressCapabilityFromArtifact(artifact ArtifactRecord, manifest Manifest) *IngressCapability {
	var summary CapabilitySummary
	if json.Unmarshal([]byte(defaultJSONObject(artifact.CapabilitiesSummaryJSON)), &summary) == nil && summary.Ingress != nil {
		return summary.Ingress
	}
	if summary, err := ManifestCapabilitySummary(manifest); err == nil {
		return summary.Ingress
	}
	return nil
}

func ingressCapabilitiesConflict(a, b *IngressCapability) bool {
	if a == nil || b == nil || a.Port <= 0 || b.Port <= 0 || a.Port != b.Port {
		return false
	}
	if ingressListenerNetwork(a.Protocol) != ingressListenerNetwork(b.Protocol) {
		return false
	}
	return ingressBindOverlaps(a.Bind, b.Bind)
}

func ingressReservedListenerConflict(ingress *IngressCapability, listener IngressReservedListener) bool {
	if ingress == nil || !listener.Enabled || ingress.Port <= 0 || listener.Port <= 0 || ingress.Port != listener.Port {
		return false
	}
	if ingressListenerNetwork(ingress.Protocol) != normalizedReservedListenerNetwork(listener.Network) {
		return false
	}
	return ingressBindOverlaps(ingress.Bind, normalizedReservedListenerBind(listener.Bind))
}

func ingressListenerNetwork(protocol string) string {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "udp":
		return "udp"
	default:
		return "tcp"
	}
}

func normalizedReservedListenerNetwork(network string) string {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case "udp":
		return "udp"
	default:
		return "tcp"
	}
}

func normalizedReservedListenerBind(bind string) string {
	if strings.TrimSpace(bind) == "" {
		return "0.0.0.0"
	}
	return strings.TrimSpace(bind)
}

func ingressBindOverlaps(a, b string) bool {
	aIP := net.ParseIP(strings.TrimSpace(a))
	bIP := net.ParseIP(strings.TrimSpace(b))
	if aIP == nil || bIP == nil {
		return false
	}
	if aIP.IsUnspecified() || bIP.IsUnspecified() {
		return true
	}
	return aIP.Equal(bIP)
}

func (m *Manager) advisoryIssues(ctx context.Context, artifact ArtifactRecord, manifest Manifest) ([]GovernanceIssue, error) {
	advisories, err := m.repo.ListAdvisories(ctx, artifact.PluginID)
	if err != nil {
		return nil, err
	}
	var issues []GovernanceIssue
	for _, advisory := range advisories {
		if advisoryIgnored(advisory) {
			continue
		}
		if !advisoryMatches(advisory, artifact, manifest) {
			continue
		}
		severity := advisorySeverity(advisory)
		code := "advisory_" + advisory.Action
		if severity == GateSeverityBlocking && advisory.Status == AdvisoryStatusRevoked {
			code = "advisory_revoke"
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

func (m *Manager) vulnerabilityIssues(ctx context.Context, artifact ArtifactRecord, manifest Manifest) ([]GovernanceIssue, error) {
	vulnerabilities, err := m.repo.ListVulnerabilities(ctx, "")
	if err != nil {
		return nil, err
	}
	deps := sbomDependencies(manifest)
	if len(deps) == 0 || len(vulnerabilities) == 0 {
		return nil, nil
	}
	var issues []GovernanceIssue
	for _, dep := range deps {
		for _, vulnerability := range vulnerabilities {
			if vulnerabilityIgnored(vulnerability) || !vulnerabilityMatchesDependency(vulnerability, dep) {
				continue
			}
			severity := vulnerabilityGateSeverity(vulnerability)
			code := "vulnerability_" + vulnerability.Action
			if vulnerability.Action == "" {
				code = "vulnerability_database"
			}
			if severity == GateSeverityBlocking && vulnerability.Status == AdvisoryStatusRevoked {
				code = "vulnerability_revoke"
			}
			issues = append(issues, issue(code, severity, "artifact dependency matches local vulnerability database", artifact.PluginID, artifact.ID, map[string]any{
				"vulnerability_id": vulnerability.VulnerabilityID,
				"source":           vulnerability.Source,
				"package_name":     vulnerability.PackageName,
				"package_version":  dep.Version,
				"version_range":    vulnerability.VersionRange,
				"severity":         vulnerability.Severity,
				"fixed_version":    vulnerability.FixedVersion,
				"summary":          vulnerability.Summary,
				"references":       vulnerability.References,
			}))
		}
	}
	return issues, nil
}

func advisoryIgnored(advisory AdvisoryRecord) bool {
	return advisory.Status == AdvisoryStatusAcked || advisory.Status == "" && advisory.Action == AdvisoryActionMitigate
}

func advisorySeverity(advisory AdvisoryRecord) string {
	if advisory.Status == AdvisoryStatusRevoked {
		return GateSeverityBlocking
	}
	switch advisory.Action {
	case AdvisoryActionDenylist, AdvisoryActionQuarantine, AdvisoryActionRevoke:
		return GateSeverityBlocking
	default:
		return GateSeverityWarning
	}
}

func vulnerabilityIgnored(record VulnerabilityRecord) bool {
	return record.Status == AdvisoryStatusAcked || record.Status == "" && record.Action == AdvisoryActionMitigate
}

func vulnerabilityGateSeverity(record VulnerabilityRecord) string {
	if record.Status == AdvisoryStatusRevoked {
		return GateSeverityBlocking
	}
	switch record.Action {
	case AdvisoryActionDenylist, AdvisoryActionQuarantine, AdvisoryActionRevoke:
		return GateSeverityBlocking
	default:
		return GateSeverityWarning
	}
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
		active := m.activeConnectionSessionCountLocked(artifact.PluginID)
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

func (m *Manager) supplyChainGovernanceIssues(ctx context.Context, artifact ArtifactRecord) ([]GovernanceIssue, error) {
	assessments, err := m.repo.ListSupplyChainAssessments(ctx, artifact.PluginID, artifact.ID)
	if err != nil {
		return nil, err
	}
	if len(assessments) == 0 {
		return nil, nil
	}
	latest := assessments[0]
	if latest.Status == SupplyChainStatusAllowed {
		return nil, nil
	}
	issues := append([]GovernanceIssue(nil), latest.Issues...)
	if latest.Status == SupplyChainStatusBlocked && len(issues) == 0 {
		issues = append(issues, issue("supply_chain_blocked", GateSeverityBlocking, "latest supply chain assessment blocks this artifact", artifact.PluginID, artifact.ID, nil))
	}
	return sortedIssues(issues), nil
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
		if review.ArtifactHash == fingerprint.ArtifactHash &&
			review.ConfigHash == fingerprint.ConfigHash &&
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
	ArtifactHash      string
	ConfigHash        string
	ScopeHash         string
	RolloutHash       string
	RuntimeLimitsHash string
	FeaturesHash      string
	PolicyHash        string
}

func governanceFingerprint(plugin PluginRecord, artifact ArtifactRecord, manifest Manifest, policyHash string) governanceFingerprintValue {
	return governanceFingerprintValue{
		ArtifactHash:      artifact.SHA256,
		ConfigHash:        stableHashJSONRaw(defaultJSONObject(plugin.ConfigJSON)),
		ScopeHash:         stableHash(manifestScope(manifest).Values),
		RolloutHash:       stableHash(manifestRollout(manifest)),
		RuntimeLimitsHash: stableHash(manifest.RuntimeLimits),
		FeaturesHash:      stableHash(requiredFeatureInputs(manifest)),
		PolicyHash:        policyHash,
	}
}

func (m *Manager) policySnapshot(profile string) PolicySnapshot {
	policy := policyForProfile(profile, m.repo.now())
	policy.RequireConformanceFixture = m.requireConformanceFixture
	return policy
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
	if artifact.RuntimeType == RuntimeSandbox && len(highRiskSandboxRequiredCapabilities(requiredRuntimeCapabilities(artifact))) > 0 {
		return RiskHigh
	}
	if len(manifest.Secrets) > 0 || len(externalDependencies(manifest)) > 0 {
		return RiskMedium
	}
	return RiskLow
}

func highRiskSandboxRequiredCapabilities(capabilities []string) []string {
	var highRisk []string
	for _, capability := range capabilities {
		switch strings.ToLower(strings.TrimSpace(capability)) {
		case "filesystem.write", "network.egress", "secret.handle", "process.restricted":
			highRisk = append(highRisk, capability)
		}
	}
	return uniqueSortedStrings(highRisk)
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

func manifestHasExtensionPoint(manifest Manifest, key string) bool {
	for _, point := range manifest.ExtensionPoints {
		if point.Key == key {
			return true
		}
	}
	return false
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

func normalizeExternalFailPolicy(spec ExternalSpec) string {
	policy := strings.TrimSpace(spec.FailPolicy)
	if policy != "" {
		return policy
	}
	switch strings.ToLower(strings.TrimSpace(spec.Purpose)) {
	case "auth", "authentication", "authorization", "entitlement", "security", "repository":
		return ExternalFailPolicyClosed
	case "route":
		return ExternalFailPolicyFallback
	case "audit", "log", "logging", "metric", "metrics", "observability":
		return ExternalFailPolicyOpen
	default:
		if spec.Required {
			return ExternalFailPolicyClosed
		}
		return ExternalFailPolicyOpen
	}
}

func validExternalFailPolicy(policy string) bool {
	switch policy {
	case ExternalFailPolicyOpen, ExternalFailPolicyClosed, ExternalFailPolicyDegraded, ExternalFailPolicyFallback:
		return true
	default:
		return false
	}
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

func vulnerabilityMatchesDependency(record VulnerabilityRecord, dep dependencySpec) bool {
	return record.PackageName == dep.Name && versionInRange(dep.Version, record.VersionRange)
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
	case "capability_enforcement_unavailable":
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
