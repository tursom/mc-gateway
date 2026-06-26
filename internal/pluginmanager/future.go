package pluginmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type pluginHostProcess struct {
	PluginID    string
	ArtifactID  string
	State       string
	DrainMode   string
	CrashLoop   bool
	CrashCount  int
	LastError   string
	StartedAt   int64
	DrainingAt  int64
	ExitedAt    int64
	LastCrashAt int64
}

func (m *Manager) PluginServiceState(ctx context.Context) (PluginServiceState, error) {
	state, err := m.repo.PluginServiceState(ctx)
	if err != nil {
		return PluginServiceState{}, err
	}
	if m.serviceMode != "" {
		state.ActiveMode = m.serviceMode
		state.RestartRequired = state.DesiredMode != state.ActiveMode
	}
	return state, nil
}

func (m *Manager) PluginServiceStatus(ctx context.Context) (PluginServiceStatus, error) {
	state, err := m.PluginServiceState(ctx)
	if err != nil {
		return PluginServiceStatus{}, err
	}
	return PluginServiceStatus{Service: state, Hosts: m.PluginHostSummaries()}, nil
}

func (m *Manager) SetPluginServiceDesired(ctx context.Context, actor, mode string) (PluginServiceState, error) {
	mode = strings.TrimSpace(mode)
	if err := validatePluginServiceMode(mode); err != nil {
		return PluginServiceState{}, err
	}
	state, err := m.repo.SetPluginServiceDesired(ctx, actor, mode)
	if err != nil {
		return PluginServiceState{}, err
	}
	_ = m.repo.RecordOperation(ctx, "", "", "plugin_service_mode_desired", "succeeded", actor, "plugin service desired mode updated", map[string]any{
		"desired_mode":     state.DesiredMode,
		"active_mode":      state.ActiveMode,
		"restart_required": state.RestartRequired,
	})
	return state, nil
}

func (m *Manager) ApplyPluginServiceMode(ctx context.Context) error {
	state, err := m.repo.PluginServiceState(ctx)
	if err != nil {
		return err
	}
	if err := validatePluginServiceMode(state.DesiredMode); err != nil {
		_ = m.repo.SetPluginServiceError(ctx, err.Error())
		state.DesiredMode = PluginServiceModeInProcess
	}
	applied, err := m.repo.ApplyPluginServiceActive(ctx, state.DesiredMode)
	if err != nil {
		return err
	}
	m.serviceMode = applied.ActiveMode
	return nil
}

func validatePluginServiceMode(mode string) error {
	switch mode {
	case PluginServiceModeInProcess, PluginServiceModeGoPluginProcess, PluginServiceModeSandboxProcess:
		return nil
	default:
		return fmt.Errorf("invalid plugin service mode %q", mode)
	}
}

func (m *Manager) markHostStarted(pluginID, artifactID string) {
	if m.serviceMode != PluginServiceModeGoPluginProcess {
		return
	}
	now := time.Now().Unix()
	m.hostMu.Lock()
	defer m.hostMu.Unlock()
	host := m.hosts[pluginID]
	if host == nil {
		host = &pluginHostProcess{PluginID: pluginID}
		m.hosts[pluginID] = host
	}
	host.ArtifactID = artifactID
	host.State = RuntimeEnabled
	host.DrainMode = PluginMigrationDrainOnly
	host.StartedAt = now
	host.DrainingAt = 0
	host.ExitedAt = 0
	host.LastError = ""
}

func (m *Manager) markHostDraining(pluginID string) {
	if m.serviceMode != PluginServiceModeGoPluginProcess {
		return
	}
	now := time.Now().Unix()
	m.hostMu.Lock()
	defer m.hostMu.Unlock()
	host := m.hosts[pluginID]
	if host == nil {
		return
	}
	host.State = RuntimeDraining
	host.DrainMode = PluginMigrationDrainOnly
	host.DrainingAt = now
	host.ExitedAt = now
	host.State = RuntimeDisabled
}

func (m *Manager) SimulatePluginHostCrash(ctx context.Context, pluginID, message string) error {
	plugin, err := m.repo.Plugin(ctx, pluginID)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	m.hostMu.Lock()
	host := m.hosts[pluginID]
	if host == nil {
		host = &pluginHostProcess{PluginID: pluginID, ArtifactID: plugin.ActiveArtifactID}
		m.hosts[pluginID] = host
	}
	host.State = RuntimeFailed
	host.CrashCount++
	host.CrashLoop = host.CrashCount >= 1
	host.LastError = message
	host.LastCrashAt = now
	m.hostMu.Unlock()
	_ = m.repo.MarkRuntime(ctx, pluginID, RuntimeFailed, plugin.ActiveArtifactID, plugin.LoadedArtifactID, plugin.AppliedGeneration, message, map[string]any{
		"plugin_host": m.hostSummary(pluginID),
	}, nil)
	_ = m.repo.RecordOperation(ctx, pluginID, plugin.ActiveArtifactID, "plugin_host_crash", "failed", "system", message, map[string]any{"crash_loop": true})
	return nil
}

func (m *Manager) PluginHostSummaries() []PluginHostRuntimeSummary {
	m.hostMu.Lock()
	defer m.hostMu.Unlock()
	out := make([]PluginHostRuntimeSummary, 0, len(m.hosts))
	for _, host := range m.hosts {
		out = append(out, host.summary())
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].PluginID < out[j].PluginID
	})
	return out
}

func (m *Manager) hostSummary(pluginID string) PluginHostRuntimeSummary {
	m.hostMu.Lock()
	defer m.hostMu.Unlock()
	if host := m.hosts[pluginID]; host != nil {
		return host.summary()
	}
	return PluginHostRuntimeSummary{PluginID: pluginID, State: RuntimeNotLoaded, DrainMode: PluginMigrationDrainOnly}
}

func (h *pluginHostProcess) summary() PluginHostRuntimeSummary {
	return PluginHostRuntimeSummary{
		PluginID: h.PluginID, ArtifactID: h.ArtifactID, State: h.State, DrainMode: h.DrainMode,
		CrashLoop: h.CrashLoop, CrashCount: h.CrashCount, LastError: h.LastError,
		StartedAt: h.StartedAt, DrainingAt: h.DrainingAt, ExitedAt: h.ExitedAt, LastCrashAt: h.LastCrashAt,
	}
}

type WASMRunner struct{}

func (WASMRunner) Validate(ctx context.Context, manifest Manifest, behavior string) error {
	timeout := DefaultHandlerTimeout
	if manifest.RuntimeLimits.HandlerTimeoutMS > 0 {
		timeout = time.Duration(manifest.RuntimeLimits.HandlerTimeoutMS) * time.Millisecond
	}
	if timeout <= 0 {
		timeout = DefaultHandlerTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				done <- fmt.Errorf("wasm plugin panic: %v", rec)
			}
		}()
		switch behavior {
		case "panic":
			panic("simulated wasm panic")
		case "timeout":
			<-callCtx.Done()
			done <- callCtx.Err()
		case "memory":
			done <- errors.New("wasm memory limit exceeded")
		default:
			done <- nil
		}
	}()
	select {
	case <-callCtx.Done():
		return callCtx.Err()
	case err := <-done:
		return err
	}
}

func (m *Manager) RunWASMValidation(ctx context.Context, pluginID, artifactID, behavior string) error {
	_, manifest, err := m.artifactManifest(ctx, pluginID, artifactID)
	if err != nil {
		return err
	}
	return (WASMRunner{}).Validate(ctx, manifest, behavior)
}

func (m *Manager) ImportRepositoryArtifact(ctx context.Context, actor string, req RepositoryImportRequest) (RepositoryImportRecord, ArtifactRecord, error) {
	if req.RepositoryType == "" {
		req.RepositoryType = RepositoryTypeFile
	}
	if req.RepositoryType != RepositoryTypeFile {
		return RepositoryImportRecord{}, ArtifactRecord{}, fmt.Errorf("repository type %q is reserved; only file is enabled", req.RepositoryType)
	}
	index, err := readRepositoryIndex(req.IndexPath)
	if err != nil {
		return RepositoryImportRecord{}, ArtifactRecord{}, err
	}
	candidate, err := selectRepositoryCandidate(index, req)
	if err != nil {
		return RepositoryImportRecord{}, ArtifactRecord{}, err
	}
	artifactPath := candidate.ArtifactPath
	if !filepath.IsAbs(artifactPath) {
		artifactPath = filepath.Join(filepath.Dir(req.IndexPath), artifactPath)
	}
	artifact, err := m.UploadArtifact(ctx, ArtifactUpload{SourcePath: artifactPath, FileName: filepath.Base(artifactPath), Actor: actor})
	if err != nil {
		return RepositoryImportRecord{}, ArtifactRecord{}, err
	}
	admission, _ := m.EvaluateGovernance(ctx, artifact.PluginID, artifact.ID, GovernanceActionPromotion, m.currentPolicyProfile(), "{}")
	admissionJSON, _ := json.Marshal(admission)
	record, err := m.repo.SaveRepositoryImport(ctx, RepositoryImportRecord{
		RepositoryType: req.RepositoryType,
		IndexPath:      req.IndexPath,
		RepositoryName: index.Name,
		CandidateID:    candidate.ID,
		PluginID:       artifact.PluginID,
		Version:        artifact.Version,
		ArtifactID:     artifact.ID,
		PackageSHA256:  artifact.PackageSHA256,
		TrustPolicy:    req.TrustPolicy,
		AdmissionJSON:  string(admissionJSON),
		ImportedBy:     actor,
	})
	if err != nil {
		return RepositoryImportRecord{}, ArtifactRecord{}, err
	}
	_ = m.repo.RecordOperation(ctx, artifact.PluginID, artifact.ID, "repository_import", "succeeded", actor, "repository artifact imported to local store", map[string]any{
		"repository":       index.Name,
		"candidate_id":     candidate.ID,
		"auto_enable":      false,
		"admission_result": admission.OK,
	})
	return record, artifact, nil
}

func (m *Manager) ListRepositoryImports(ctx context.Context) ([]RepositoryImportRecord, error) {
	return m.repo.ListRepositoryImports(ctx)
}

func (m *Manager) AssessSupplyChain(ctx context.Context, actor, pluginID, artifactID string, metadata map[string]any) (SupplyChainAssessment, error) {
	artifact, manifest, err := m.artifactManifest(ctx, pluginID, artifactID)
	if err != nil {
		return SupplyChainAssessment{}, err
	}
	issues := supplyChainIssues(artifact, manifest, metadata)
	status := SupplyChainStatusAllowed
	if hasBlockingIssue(issues) {
		status = SupplyChainStatusBlocked
	} else if hasWarningIssue(issues) {
		status = SupplyChainStatusWarning
	}
	assessment, err := m.repo.SaveSupplyChainAssessment(ctx, SupplyChainAssessment{
		PluginID: artifact.PluginID, ArtifactID: artifact.ID, Status: status, Issues: issues,
		Signature: jsonMapFromAny(metadata["signature"]), SBOM: jsonMapFromAny(metadata["sbom"]),
		License: jsonMapFromAny(metadata["license"]), Advisory: jsonMapFromAny(metadata["advisory"]),
		Metadata: metadata, CreatedBy: actor,
	})
	if err != nil {
		return SupplyChainAssessment{}, err
	}
	if status == SupplyChainStatusBlocked {
		_ = m.repo.UpdateArtifactStatus(ctx, artifact.ID, ArtifactStatusRejected, "supply chain assessment blocked artifact")
	}
	return assessment, nil
}

func (m *Manager) ListSupplyChainAssessments(ctx context.Context, pluginID, artifactID string) ([]SupplyChainAssessment, error) {
	return m.repo.ListSupplyChainAssessments(ctx, pluginID, artifactID)
}

func (m *Manager) SaveInstrumentation(ctx context.Context, actor string, req InstrumentationRequest) (InstrumentationRecord, error) {
	if strings.TrimSpace(req.Name) == "" {
		return InstrumentationRecord{}, errors.New("instrumentation name is required")
	}
	if req.RunbookRollback == "" {
		req.RunbookRollback = "Rollback by deploying the previous gateway binary."
	}
	record, err := m.repo.SaveInstrumentation(ctx, actor, req)
	if err != nil {
		return InstrumentationRecord{}, err
	}
	_ = m.repo.RecordOperation(ctx, "", "", "instrumentation_register", "succeeded", actor, "build-time instrumentation metadata registered", map[string]any{
		"name": record.Name, "version": record.Version, "runtime_plugin": false,
	})
	return record, nil
}

func (m *Manager) ListInstrumentation(ctx context.Context) ([]InstrumentationRecord, error) {
	return m.repo.ListInstrumentation(ctx)
}

type repositoryIndex struct {
	Name       string                `json:"name"`
	Candidates []repositoryCandidate `json:"artifacts"`
}

type repositoryCandidate struct {
	ID           string `json:"id"`
	PluginID     string `json:"plugin_id"`
	Version      string `json:"version"`
	ArtifactPath string `json:"artifact_path"`
}

func readRepositoryIndex(path string) (repositoryIndex, error) {
	var index repositoryIndex
	data, err := os.ReadFile(path)
	if err != nil {
		return index, err
	}
	if err := json.Unmarshal(data, &index); err != nil {
		return index, err
	}
	if index.Name == "" {
		index.Name = "local"
	}
	return index, nil
}

func selectRepositoryCandidate(index repositoryIndex, req RepositoryImportRequest) (repositoryCandidate, error) {
	for _, candidate := range index.Candidates {
		if req.ArtifactID != "" && candidate.ID != req.ArtifactID {
			continue
		}
		if req.PluginID != "" && candidate.PluginID != req.PluginID {
			continue
		}
		if req.Version != "" && candidate.Version != req.Version {
			continue
		}
		if candidate.ArtifactPath == "" {
			return repositoryCandidate{}, errors.New("repository candidate artifact_path is required")
		}
		return candidate, nil
	}
	return repositoryCandidate{}, errors.New("repository candidate not found")
}

func supplyChainIssues(artifact ArtifactRecord, manifest Manifest, metadata map[string]any) []GovernanceIssue {
	var issues []GovernanceIssue
	signature := jsonMapFromAny(metadata["signature"])
	if requiredBool(signature, "required") && !requiredBool(signature, "verified") {
		issues = append(issues, issue("signature_unverified", GateSeverityBlocking, "required artifact signature is not verified", artifact.PluginID, artifact.ID, nil))
	}
	sbom := jsonMapFromAny(metadata["sbom"])
	if requiredBool(sbom, "required") && !requiredBool(sbom, "scan_ok") {
		issues = append(issues, issue("sbom_scan_blocked", GateSeverityBlocking, "SBOM vulnerability scan failed", artifact.PluginID, artifact.ID, nil))
	}
	license := jsonMapFromAny(metadata["license"])
	if denied := stringSlice(license["denylist_matches"]); len(denied) > 0 {
		issues = append(issues, issue("license_denylist", GateSeverityBlocking, "artifact matches denied license policy", artifact.PluginID, artifact.ID, map[string]any{"licenses": denied}))
	}
	if allowed := stringSlice(license["allowlist_missing"]); len(allowed) > 0 {
		issues = append(issues, issue("license_allowlist_missing", GateSeverityBlocking, "artifact license is not in allowlist", artifact.PluginID, artifact.ID, map[string]any{"licenses": allowed}))
	}
	advisory := jsonMapFromAny(metadata["advisory"])
	if requiredBool(advisory, "blocked") {
		issues = append(issues, issue("advisory_feed_blocked", GateSeverityBlocking, "advisory feed marks artifact as blocked", artifact.PluginID, artifact.ID, nil))
	}
	if len(sbomDependencies(manifest)) == 0 && requiredBool(sbom, "required") {
		issues = append(issues, issue("sbom_missing_dependencies", GateSeverityBlocking, "manifest supply_chain does not include SBOM dependencies", artifact.PluginID, artifact.ID, nil))
	}
	return sortedIssues(issues)
}

func jsonMapFromAny(value any) map[string]any {
	if value == nil {
		return nil
	}
	if out, ok := value.(map[string]any); ok {
		return out
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var out map[string]any
	if json.Unmarshal(data, &out) != nil {
		return nil
	}
	return out
}

func requiredBool(values map[string]any, key string) bool {
	value, _ := values[key].(bool)
	return value
}
