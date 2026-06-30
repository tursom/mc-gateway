// internal/pluginmanager/future.go 建模未来运行时和分发能力，但不把它们接入当前热路径。

package pluginmanager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	repositoryIndexMaxBytes       = 4 * 1024 * 1024
	defaultPluginHostCrashBackoff = time.Duration(DefaultPluginHostCrashBackoffSeconds) * time.Second
)

type pluginHostProcess struct {
	PluginID     string
	ArtifactID   string
	PID          int
	State        string
	DrainMode    string
	CrashLoop    bool
	CrashCount   int
	LastError    string
	StartedAt    int64
	DrainingAt   int64
	ExitedAt     int64
	LastCrashAt  int64
	BackoffUntil int64
	Isolated     bool
}

func (m *Manager) PluginServiceState(ctx context.Context) (PluginServiceState, error) {
	state, err := m.repo.PluginServiceState(ctx)
	if err != nil {
		return PluginServiceState{}, err
	}
	return m.enrichPluginServiceState(state), nil
}

func (m *Manager) PluginServiceStatus(ctx context.Context) (PluginServiceStatus, error) {
	state, err := m.PluginServiceState(ctx)
	if err != nil {
		return PluginServiceStatus{}, err
	}
	if err := m.recordPluginNodeState(ctx, state); err != nil {
		return PluginServiceStatus{}, err
	}
	nodes, err := m.repo.ListPluginNodes(ctx, DefaultPluginNodeStaleAfter)
	if err != nil {
		return PluginServiceStatus{}, err
	}
	return PluginServiceStatus{
		Service:         state,
		Modes:           PluginServiceModeFeaturesFor(m.RuntimeFeatureFactsOptions()),
		RuntimeTypes:    RuntimeTypeFeaturesFor(m.RuntimeFeatureFactsOptions()),
		RuntimeAdapters: RuntimeAdapterFactoryStatusesFor(m.RuntimeFeatureFactsOptions()),
		Hosts:           m.pluginHostSummaries(ctx),
		Nodes:           nodes,
	}, nil
}

func (m *Manager) recordPluginNodeState(ctx context.Context, state PluginServiceState) error {
	nodeID := strings.TrimSpace(m.nodeID)
	if nodeID == "" && m.operations != nil {
		nodeID = m.operations.nodeID
	}
	if nodeID == "" {
		nodeID = defaultOperationsNodeID()
	}
	host, _ := os.Hostname()
	startedAt := m.startedAt
	if startedAt == 0 {
		startedAt = time.Now().Unix()
	}
	return m.repo.UpsertPluginNode(ctx, PluginNodeState{
		NodeID:        nodeID,
		Hostname:      host,
		PID:           os.Getpid(),
		ServiceMode:   state.ActiveMode,
		DataPlaneMode: state.DataPlaneMode,
		Status:        PluginNodeStatusOnline,
		StartedAt:     startedAt,
	})
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
	state = m.enrichPluginServiceState(state)
	_ = m.repo.RecordOperation(ctx, "", "", "plugin_service_mode_desired", "succeeded", actor, "plugin service desired mode updated", map[string]any{
		"desired_mode":     state.DesiredMode,
		"active_mode":      state.ActiveMode,
		"data_plane_mode":  state.DataPlaneMode,
		"restart_required": state.RestartRequired,
		"maturity":         state.DesiredMaturity,
		"unsupported":      state.UnsupportedReason,
	})
	return state, nil
}

func (m *Manager) SetPluginServiceCrashPolicy(ctx context.Context, actor string, policy PluginHostCrashPolicy) (PluginServiceState, error) {
	policy = normalizePluginHostCrashPolicy(policy)
	if policy.BackoffSeconds > 3600 {
		return PluginServiceState{}, errors.New("plugin-host crash backoff_seconds must be <= 3600")
	}
	if policy.MaxCrashes > 100 {
		return PluginServiceState{}, errors.New("plugin-host crash max_crashes must be <= 100")
	}
	if policy.WindowSeconds > 86400 {
		return PluginServiceState{}, errors.New("plugin-host crash window_seconds must be <= 86400")
	}
	state, err := m.repo.SetPluginServiceCrashPolicy(ctx, actor, policy)
	if err != nil {
		return PluginServiceState{}, err
	}
	state = m.enrichPluginServiceState(state)
	_ = m.repo.RecordOperation(ctx, "", "", "plugin_service_crash_policy_update", "succeeded", actor, "plugin service crash policy updated", map[string]any{
		"backoff_seconds": state.CrashPolicy.BackoffSeconds,
		"max_crashes":     state.CrashPolicy.MaxCrashes,
		"window_seconds":  state.CrashPolicy.WindowSeconds,
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
	desiredFeature := PluginServiceModeFeatureForOptions(state.DesiredMode, m.RuntimeFeatureFactsOptions())
	if state.DesiredMode == PluginServiceModeSandboxProcess {
		if code, message := m.validateSandboxServiceModeApply(); code != "" {
			return m.recordPluginServiceModeApplyFailure(ctx, state, desiredFeature, code, message)
		}
	}
	if state.DesiredMode != PluginServiceModeInProcess && !m.adapterManaged {
		applied, applyErr := m.repo.ApplyPluginServiceActive(ctx, PluginServiceModeInProcess)
		if applyErr != nil {
			return applyErr
		}
		message := "plugin service mode requires the managed runtime adapter factory"
		_ = m.repo.SetPluginServiceError(ctx, message)
		m.serviceMode = PluginServiceModeInProcess
		_ = m.repo.RecordOperation(ctx, "", "", "plugin_service_mode_apply", "skipped", "system", message, map[string]any{
			"desired_mode":     state.DesiredMode,
			"active_mode":      applied.ActiveMode,
			"data_plane_mode":  PluginServiceModeInProcess,
			"restart_required": true,
			"maturity":         desiredFeature.Maturity,
		})
		return nil
	}
	if !desiredFeature.Implemented || !desiredFeature.DataPlane {
		applied, applyErr := m.repo.ApplyPluginServiceActive(ctx, PluginServiceModeInProcess)
		if applyErr != nil {
			return applyErr
		}
		message := desiredFeature.UnsupportedReason
		if message == "" {
			message = "plugin service mode is not implemented for the data plane"
		}
		_ = m.repo.SetPluginServiceError(ctx, message)
		m.serviceMode = PluginServiceModeInProcess
		_ = m.repo.RecordOperation(ctx, "", "", "plugin_service_mode_apply", "skipped", "system", message, map[string]any{
			"desired_mode":     state.DesiredMode,
			"active_mode":      applied.ActiveMode,
			"data_plane_mode":  PluginServiceModeInProcess,
			"restart_required": true,
			"maturity":         desiredFeature.Maturity,
		})
		return nil
	}
	if state.DesiredMode == PluginServiceModeSandboxProcess && !m.futureGates.SandboxEnabled() {
		applied, applyErr := m.repo.ApplyPluginServiceActive(ctx, PluginServiceModeInProcess)
		if applyErr != nil {
			return applyErr
		}
		message := "sandbox-process service mode is disabled by future runtime gate"
		_ = m.repo.SetPluginServiceError(ctx, message)
		m.serviceMode = PluginServiceModeInProcess
		_ = m.repo.RecordOperation(ctx, "", "", "plugin_service_mode_apply", "skipped", "system", message, map[string]any{
			"desired_mode":     state.DesiredMode,
			"active_mode":      applied.ActiveMode,
			"data_plane_mode":  PluginServiceModeInProcess,
			"restart_required": true,
			"maturity":         desiredFeature.Maturity,
		})
		return nil
	}
	applied, err := m.repo.ApplyPluginServiceActive(ctx, state.DesiredMode)
	if err != nil {
		return err
	}
	m.serviceMode = applied.ActiveMode
	if m.adapterManaged {
		runtimeType := RuntimeGoPlugin
		if m.serviceMode == PluginServiceModeSandboxProcess {
			runtimeType = RuntimeSandbox
		}
		m.adapter, _ = RuntimeAdapterFactory{Facts: m.RuntimeFeatureFactsOptions()}.AdapterFor(m.serviceMode, runtimeType)
	}
	return nil
}

func (m *Manager) validateSandboxServiceModeApply() (string, string) {
	if !m.futureGates.SandboxEnabled() {
		return "sandbox_future_gate_closed", "sandbox-process service mode is disabled by future runtime gate"
	}
	if !m.adapterManaged {
		return "sandbox_adapter_factory_unavailable", "sandbox-process service mode requires the managed runtime adapter factory"
	}
	switch m.currentPolicyProfile() {
	case PolicyProfileDev, PolicyProfileStaging, PolicyProfileProd:
	default:
		return "sandbox_policy_profile_invalid", fmt.Sprintf("sandbox-process service mode requires a supported policy profile, got %q", m.currentPolicyProfile())
	}
	selfCheck := m.sandboxSelfCheck
	if selfCheck == nil {
		selfCheck = defaultSandboxEnvironmentSelfCheck
	}
	if err := selfCheck(m.sandboxPolicy); err != nil {
		return "sandbox_environment_self_check_failed", "sandbox-process environment self-check failed: " + err.Error()
	}
	_, status := RuntimeAdapterFactory{Facts: m.RuntimeFeatureFactsOptions()}.AdapterFor(PluginServiceModeSandboxProcess, RuntimeSandbox)
	if !status.Implemented || !status.DataPlane || !status.Lifecycle {
		message := status.UnsupportedReason
		if message == "" {
			message = "sandbox-process runtime adapter is not available for the data plane"
		}
		return "sandbox_adapter_factory_unavailable", message
	}
	return "", ""
}

func (m *Manager) recordPluginServiceModeApplyFailure(ctx context.Context, state PluginServiceState, desiredFeature PluginServiceModeFeature, reasonCode, message string) error {
	if message == "" {
		message = "plugin service mode is not implemented for the data plane"
	}
	if err := m.repo.SetPluginServiceError(ctx, message); err != nil {
		return err
	}
	preservedMode := state.ActiveMode
	if preservedMode == "" {
		preservedMode = PluginServiceModeInProcess
	}
	m.serviceMode = preservedMode
	if m.adapterManaged {
		runtimeType := RuntimeGoPlugin
		if preservedMode == PluginServiceModeSandboxProcess {
			runtimeType = RuntimeSandbox
		}
		m.adapter, _ = RuntimeAdapterFactory{Facts: m.RuntimeFeatureFactsOptions()}.AdapterFor(preservedMode, runtimeType)
	}
	enriched := m.enrichPluginServiceState(state)
	_ = m.repo.RecordOperation(ctx, "", "", "plugin_service_mode_apply", "failed", "system", message, map[string]any{
		"reason_code":      reasonCode,
		"desired_mode":     state.DesiredMode,
		"active_mode":      enriched.ActiveMode,
		"data_plane_mode":  enriched.DataPlaneMode,
		"restart_required": enriched.RestartRequired,
		"maturity":         desiredFeature.Maturity,
	})
	return nil
}

func (m *Manager) enrichPluginServiceState(state PluginServiceState) PluginServiceState {
	state = normalizePluginServiceState(state)
	dataPlaneMode := m.serviceMode
	if dataPlaneMode == "" {
		dataPlaneMode = state.ActiveMode
	}
	dataPlaneFeature := PluginServiceModeFeatureForOptions(dataPlaneMode, m.RuntimeFeatureFactsOptions())
	if !dataPlaneFeature.Implemented || !dataPlaneFeature.DataPlane {
		dataPlaneMode = PluginServiceModeInProcess
		dataPlaneFeature = PluginServiceModeFeatureForOptions(dataPlaneMode, m.RuntimeFeatureFactsOptions())
	}
	desiredFeature := PluginServiceModeFeatureForOptions(state.DesiredMode, m.RuntimeFeatureFactsOptions())
	activeFeature := PluginServiceModeFeatureForOptions(state.ActiveMode, m.RuntimeFeatureFactsOptions())
	state.DataPlaneMode = dataPlaneMode
	state.ImplementedAdapter = activeFeature.Implemented && activeFeature.DataPlane && state.ActiveMode == dataPlaneMode
	state.DesiredMaturity = desiredFeature.Maturity
	state.ActiveMaturity = activeFeature.Maturity
	state.RestartRequired = state.DesiredMode != state.ActiveMode ||
		state.DesiredMode != state.DataPlaneMode ||
		state.ActiveMode != state.DataPlaneMode
	if state.UnsupportedReason == "" {
		switch {
		case !desiredFeature.Implemented || !desiredFeature.DataPlane:
			state.UnsupportedReason = desiredFeature.UnsupportedReason
		case !activeFeature.Implemented || !activeFeature.DataPlane:
			state.UnsupportedReason = activeFeature.UnsupportedReason
		case !dataPlaneFeature.Implemented || !dataPlaneFeature.DataPlane:
			state.UnsupportedReason = dataPlaneFeature.UnsupportedReason
		}
	}
	return state
}

func (m *Manager) pluginHostCrashPolicy(ctx context.Context) PluginHostCrashPolicy {
	state, err := m.repo.PluginServiceState(ctx)
	if err != nil {
		return normalizePluginHostCrashPolicy(PluginHostCrashPolicy{})
	}
	return normalizePluginHostCrashPolicy(state.CrashPolicy)
}

func validatePluginServiceMode(mode string) error {
	switch mode {
	case PluginServiceModeInProcess, PluginServiceModeGoPluginProcess, PluginServiceModeSandboxProcess:
		return nil
	default:
		return fmt.Errorf("invalid plugin service mode %q", mode)
	}
}

func (m *Manager) markHostStarted(pluginID, artifactID string, process *PluginHostSupervisorProcess) {
	if m.serviceMode != PluginServiceModeGoPluginProcess {
		return
	}
	now := time.Now().Unix()
	policy := m.pluginHostCrashPolicy(context.Background())
	m.hostMu.Lock()
	defer m.hostMu.Unlock()
	host := m.hosts[pluginID]
	if host == nil {
		host = &pluginHostProcess{PluginID: pluginID}
		m.hosts[pluginID] = host
	}
	if process != nil {
		applyPluginHostSummary(host, artifactID, process.Summary(), policy)
		return
	}
	host.ArtifactID = artifactID
	host.PID = os.Getpid()
	host.State = RuntimeEnabled
	host.DrainMode = PluginMigrationDrainOnly
	host.StartedAt = now
	host.DrainingAt = 0
	host.ExitedAt = 0
	host.LastError = ""
	host.LastCrashAt = 0
	host.BackoffUntil = 0
	host.Isolated = false
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
	host.ExitedAt = 0
}

func (m *Manager) markHostStopped(pluginID, artifactID string, process *PluginHostSupervisorProcess) {
	if m.serviceMode != PluginServiceModeGoPluginProcess {
		return
	}
	now := time.Now().Unix()
	policy := m.pluginHostCrashPolicy(context.Background())
	m.hostMu.Lock()
	defer m.hostMu.Unlock()
	host := m.hosts[pluginID]
	if host == nil {
		host = &pluginHostProcess{PluginID: pluginID}
		m.hosts[pluginID] = host
	}
	if process != nil && host.PID != 0 && process.PID != 0 && host.PID != process.PID && host.State == RuntimeEnabled {
		return
	}
	if process != nil {
		applyPluginHostSummary(host, artifactID, process.Summary(), policy)
		if host.ExitedAt == 0 {
			host.ExitedAt = now
		}
	}
	host.State = RuntimeDisabled
	host.DrainMode = PluginMigrationDrainOnly
	if artifactID != "" {
		host.ArtifactID = artifactID
	}
	if host.ExitedAt == 0 {
		host.ExitedAt = now
	}
}

func (m *Manager) SimulatePluginHostCrash(ctx context.Context, pluginID, message string) error {
	plugin, err := m.repo.Plugin(ctx, pluginID)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	policy := m.pluginHostCrashPolicy(ctx)
	m.hostMu.Lock()
	host := m.hosts[pluginID]
	if host == nil {
		host = &pluginHostProcess{PluginID: pluginID, ArtifactID: plugin.ActiveArtifactID}
		m.hosts[pluginID] = host
	}
	host.State = RuntimeFailed
	host.CrashCount = nextPluginHostCrashCount(host.CrashCount, host.LastCrashAt, now, policy)
	host.CrashLoop = host.CrashCount >= int(policy.MaxCrashes)
	host.LastError = message
	host.LastCrashAt = now
	host.BackoffUntil = now + policy.BackoffSeconds
	summary := host.summary()
	m.hostMu.Unlock()
	_ = m.repo.MarkRuntime(ctx, pluginID, RuntimeFailed, plugin.ActiveArtifactID, plugin.LoadedArtifactID, plugin.AppliedGeneration, message, map[string]any{
		"plugin_host": summary,
	}, nil)
	_ = m.repo.SetPluginServiceError(ctx, message)
	m.recordPluginNodeRuntimeState(ctx, pluginID)
	_ = m.repo.RecordOperation(ctx, pluginID, plugin.ActiveArtifactID, "plugin_host_crash", "failed", "system", message, map[string]any{
		"crash_loop":    summary.CrashLoop,
		"crash_count":   summary.CrashCount,
		"backoff_until": summary.BackoffUntil,
	})
	return nil
}

func (m *Manager) PluginHostSummaries() []PluginHostRuntimeSummary {
	return m.pluginHostSummaries(context.Background())
}

func (m *Manager) pluginHostSummaries(ctx context.Context) []PluginHostRuntimeSummary {
	m.refreshLoadedHostSummaries(ctx)
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

func (m *Manager) refreshLoadedHostSummaries(ctx context.Context) {
	if m.serviceMode != PluginServiceModeGoPluginProcess {
		return
	}
	type loadedHostProcess struct {
		pluginID   string
		artifactID string
		process    *PluginHostSupervisorProcess
	}
	m.mu.Lock()
	processes := make([]loadedHostProcess, 0, len(m.loaded))
	for pluginID, loaded := range m.loaded {
		if loaded == nil || loaded.runtime.HostProcess == nil {
			continue
		}
		processes = append(processes, loadedHostProcess{
			pluginID:   pluginID,
			artifactID: loaded.artifact.ID,
			process:    loaded.runtime.HostProcess,
		})
	}
	m.mu.Unlock()
	if len(processes) == 0 {
		return
	}
	policy := m.pluginHostCrashPolicy(ctx)
	m.hostMu.Lock()
	var crashed []PluginHostRuntimeSummary
	for _, item := range processes {
		host := m.hosts[item.pluginID]
		if host == nil {
			host = &pluginHostProcess{PluginID: item.pluginID}
			m.hosts[item.pluginID] = host
		}
		applyPluginHostSummary(host, item.artifactID, item.process.Summary(), policy)
		if host.State == RuntimeFailed && (!host.Isolated || !host.CrashLoop) {
			crashed = append(crashed, host.summary())
		}
	}
	m.hostMu.Unlock()
	for _, summary := range crashed {
		m.isolateCrashedPluginHost(ctx, summary)
	}
}

func (m *Manager) isolateCrashedPluginHost(ctx context.Context, summary PluginHostRuntimeSummary) {
	if summary.PluginID == "" {
		return
	}
	message := summary.LastError
	if message == "" {
		message = "plugin-host crash loop"
	}

	m.mu.Lock()
	loaded := m.loaded[summary.PluginID]
	activeArtifactID := summary.ArtifactID
	loadedArtifactID := summary.ArtifactID
	appliedGeneration := int64(0)
	if loaded != nil {
		if activeArtifactID == "" {
			activeArtifactID = loaded.artifact.ID
		}
		if loadedArtifactID == "" {
			loadedArtifactID = loaded.artifact.ID
		}
		appliedGeneration = loaded.record.DesiredGeneration
		delete(m.loaded, summary.PluginID)
	}
	m.removeFromDispatchLocked(summary.PluginID)
	m.removeExtensionsLocked(summary.PluginID)
	m.markDrainingLocked(summary.PluginID)
	m.operations.StopPlugin(summary.PluginID)
	m.mu.Unlock()

	m.hostMu.Lock()
	if host := m.hosts[summary.PluginID]; host != nil {
		if summary.CrashLoop {
			host.Isolated = true
		}
		host.State = RuntimeFailed
		if host.LastError == "" {
			host.LastError = message
		}
		summary = host.summary()
	}
	m.hostMu.Unlock()

	if appliedGeneration == 0 {
		if plugin, err := m.repo.Plugin(ctx, summary.PluginID); err == nil {
			if activeArtifactID == "" {
				activeArtifactID = plugin.ActiveArtifactID
			}
			if loadedArtifactID == "" {
				loadedArtifactID = plugin.LoadedArtifactID
			}
			appliedGeneration = plugin.AppliedGeneration
		}
	}
	_ = m.repo.MarkRuntime(ctx, summary.PluginID, RuntimeFailed, activeArtifactID, loadedArtifactID, appliedGeneration, message, map[string]any{
		"plugin_host": summary,
		"isolated":    true,
	}, nil)
	_ = m.repo.SetPluginServiceError(ctx, message)
	m.recordPluginNodeRuntimeState(ctx, summary.PluginID)
	operation := "plugin_host_crash_backoff"
	if summary.CrashLoop {
		operation = "plugin_host_crash_isolate"
	}
	_ = m.repo.RecordOperation(ctx, summary.PluginID, activeArtifactID, operation, "failed", "system", message, map[string]any{
		"crash_loop":    summary.CrashLoop,
		"crash_count":   summary.CrashCount,
		"backoff_until": summary.BackoffUntil,
		"isolated":      summary.Isolated,
	})
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
		PID: h.PID, CrashLoop: h.CrashLoop, CrashCount: h.CrashCount, LastError: h.LastError,
		StartedAt: h.StartedAt, DrainingAt: h.DrainingAt, ExitedAt: h.ExitedAt, LastCrashAt: h.LastCrashAt, BackoffUntil: h.BackoffUntil, Isolated: h.Isolated,
	}
}

func applyPluginHostSummary(host *pluginHostProcess, artifactID string, summary PluginHostRuntimeSummary, policy PluginHostCrashPolicy) {
	policy = normalizePluginHostCrashPolicy(policy)
	previousCrashAt := host.LastCrashAt
	previousCrashCount := host.CrashCount
	if host.PluginID == "" {
		host.PluginID = summary.PluginID
	}
	if summary.ArtifactID == "" {
		summary.ArtifactID = artifactID
	}
	if summary.CrashLoop && summary.LastCrashAt == 0 {
		summary.LastCrashAt = time.Now().Unix()
	}
	host.ArtifactID = summary.ArtifactID
	host.PID = summary.PID
	host.State = summary.State
	host.DrainMode = summary.DrainMode
	crashed := summary.CrashLoop || summary.LastCrashAt > 0 || summary.State == RuntimeFailed
	host.LastError = summary.LastError
	host.StartedAt = summary.StartedAt
	host.DrainingAt = summary.DrainingAt
	host.ExitedAt = summary.ExitedAt
	host.LastCrashAt = summary.LastCrashAt
	if crashed {
		if summary.LastCrashAt == previousCrashAt && previousCrashAt != 0 {
			host.CrashCount = previousCrashCount
		} else {
			host.CrashCount = nextPluginHostCrashCount(previousCrashCount, previousCrashAt, summary.LastCrashAt, policy)
		}
		host.CrashLoop = host.CrashCount >= int(policy.MaxCrashes)
		if summary.BackoffUntil > host.BackoffUntil {
			host.BackoffUntil = summary.BackoffUntil
		}
		if host.BackoffUntil == 0 || summary.LastCrashAt > previousCrashAt {
			host.BackoffUntil = summary.LastCrashAt + policy.BackoffSeconds
		}
	} else {
		host.CrashLoop = false
		host.CrashCount = 0
		host.BackoffUntil = 0
		host.Isolated = false
	}
}

func nextPluginHostCrashCount(previousCount int, previousCrashAt, crashAt int64, policy PluginHostCrashPolicy) int {
	policy = normalizePluginHostCrashPolicy(policy)
	if crashAt <= 0 {
		crashAt = time.Now().Unix()
	}
	if previousCrashAt > 0 && crashAt-previousCrashAt <= policy.WindowSeconds {
		return previousCount + 1
	}
	return 1
}

func (m *Manager) ImportRepositoryArtifact(ctx context.Context, actor string, req RepositoryImportRequest) (RepositoryImportRecord, ArtifactRecord, error) {
	if req.RepositoryType == "" {
		req.RepositoryType = RepositoryTypeFile
	}
	req.RepositoryType = normalizeRepositoryType(req.RepositoryType)
	if err := validateRepositoryType(req.RepositoryType); err != nil {
		return RepositoryImportRecord{}, ArtifactRecord{}, err
	}
	index, syncRecord, err := m.SyncRepositoryIndex(ctx, actor, req)
	if err != nil {
		return RepositoryImportRecord{}, ArtifactRecord{}, err
	}
	candidate, err := selectRepositoryCandidate(index, req)
	if err != nil {
		return RepositoryImportRecord{}, ArtifactRecord{}, err
	}
	artifactPath, fileName, cleanup, err := materializeRepositoryArtifact(ctx, req, candidate)
	if err != nil {
		return RepositoryImportRecord{}, ArtifactRecord{}, err
	}
	defer cleanup()
	if candidate.SHA256 != "" {
		actual, err := fileSHA256(artifactPath)
		if err != nil {
			return RepositoryImportRecord{}, ArtifactRecord{}, err
		}
		if !strings.EqualFold(actual, candidate.SHA256) {
			return RepositoryImportRecord{}, ArtifactRecord{}, fmt.Errorf("repository artifact sha256 mismatch: got %s want %s", actual, candidate.SHA256)
		}
	}
	artifact, err := m.UploadArtifact(ctx, ArtifactUpload{SourcePath: artifactPath, FileName: fileName, Actor: actor})
	if err != nil {
		return RepositoryImportRecord{}, ArtifactRecord{}, err
	}
	admission, _, err := m.evaluateGovernance(ctx, artifact.PluginID, artifact.ID, GovernanceActionPromotion, m.currentPolicyProfile(), "{}", true)
	if err != nil {
		profile := normalizeProfile(m.currentPolicyProfile())
		policy := m.policySnapshot(profile)
		var manifest Manifest
		_ = json.Unmarshal([]byte(artifact.MetadataJSON), &manifest)
		admission = GovernanceDecision{
			OK:         false,
			Action:     GovernanceActionPromotion,
			Profile:    profile,
			RiskLevel:  riskLevel(manifest, artifact),
			PolicyHash: policyHash(policy),
			CreatedAt:  m.repo.now().Unix(),
			Issues: []GovernanceIssue{issue("repository_import_admission_unavailable", GateSeverityBlocking, "repository import admission preview could not be evaluated", artifact.PluginID, artifact.ID, map[string]any{
				"error": err.Error(),
			})},
		}
	}
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
		"sync_status":      syncRecord.Status,
		"sync_id":          syncRecord.ID,
		"auto_enable":      false,
		"admission_result": admission.OK,
	})
	return record, artifact, nil
}

func (m *Manager) ListRepositoryImports(ctx context.Context) ([]RepositoryImportRecord, error) {
	return m.repo.ListRepositoryImports(ctx)
}

func (m *Manager) RepositoryUpdateAvailability(ctx context.Context, req RepositoryImportRequest) (RepositoryUpdateReport, error) {
	if req.RepositoryType == "" {
		req.RepositoryType = RepositoryTypeFile
	}
	req.RepositoryType = normalizeRepositoryType(req.RepositoryType)
	if err := validateRepositoryType(req.RepositoryType); err != nil {
		return RepositoryUpdateReport{}, err
	}
	index, syncRecord, err := m.SyncRepositoryIndex(ctx, "system", req)
	if err != nil {
		return RepositoryUpdateReport{}, err
	}
	report := RepositoryUpdateReport{
		RepositoryType: req.RepositoryType,
		IndexPath:      req.IndexPath,
		RepositoryName: index.Name,
		CheckedAt:      m.repo.now().Unix(),
		Sync:           syncRecord,
		Candidates:     []RepositoryUpdateCandidate{},
		Updates:        []RepositoryUpdateCandidate{},
	}
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
		item, err := m.repositoryUpdateCandidate(ctx, candidate)
		if err != nil {
			return RepositoryUpdateReport{}, err
		}
		report.Candidates = append(report.Candidates, item)
		if item.UpdateAvailable {
			report.Updates = append(report.Updates, item)
		}
	}
	_ = m.repo.RecordOperation(ctx, "", "", "repository_update_availability", "succeeded", "system", "repository update availability checked without desired-state mutation", map[string]any{
		"repository":    report.RepositoryName,
		"sync_status":   syncRecord.Status,
		"candidates":    len(report.Candidates),
		"updates":       len(report.Updates),
		"mutates_state": false,
	})
	return report, nil
}

func (m *Manager) repositoryUpdateCandidate(ctx context.Context, candidate repositoryCandidate) (RepositoryUpdateCandidate, error) {
	item := RepositoryUpdateCandidate{
		CandidateID:      candidate.ID,
		PluginID:         candidate.PluginID,
		AvailableVersion: candidate.Version,
		CandidateSHA256:  candidate.SHA256,
		Reason:           "not_installed",
	}
	plugin, err := m.repo.Plugin(ctx, candidate.PluginID)
	if errors.Is(err, ErrPluginNotFound) {
		return item, nil
	}
	if err != nil {
		return RepositoryUpdateCandidate{}, err
	}
	item.CurrentArtifactID = plugin.DesiredArtifactID
	if item.CurrentArtifactID == "" {
		item.CurrentArtifactID = plugin.ActiveArtifactID
	}
	if item.CurrentArtifactID != "" {
		if artifact, err := m.repo.Artifact(ctx, item.CurrentArtifactID); err == nil {
			item.CurrentPackageSHA256 = artifact.PackageSHA256
			item.CurrentVersion = artifact.Version
		} else if !errors.Is(err, ErrArtifactNotFound) {
			return RepositoryUpdateCandidate{}, err
		}
	}
	comparison, stable := compareRepositoryVersions(candidate.Version, item.CurrentVersion)
	item.VersionComparison = comparison
	item.VersionComparisonStable = stable
	switch {
	case candidate.Version == "" && candidate.SHA256 == "":
		item.Reason = "candidate_metadata_incomplete"
	case stable && comparison > 0:
		item.UpdateAvailable = true
		item.Reason = "newer_version"
	case stable && comparison < 0:
		item.Reason = "candidate_not_newer"
	case !stable && candidate.Version != "" && item.CurrentVersion != "" && candidate.Version != item.CurrentVersion:
		item.UpdateAvailable = true
		item.Reason = "version_changed"
	case candidate.SHA256 != "" && item.CurrentPackageSHA256 != "" && !strings.EqualFold(candidate.SHA256, item.CurrentPackageSHA256):
		item.UpdateAvailable = true
		item.Reason = "package_changed"
	default:
		item.Reason = "current"
	}
	return item, nil
}

func (m *Manager) ApplyRepositoryImport(ctx context.Context, actor string, importID int64, configJSON, desiredState string, priority int, dryRun bool) (RepositoryImportApplyResult, error) {
	record, err := m.repo.RepositoryImport(ctx, importID)
	if err != nil {
		return RepositoryImportApplyResult{}, err
	}
	result := RepositoryImportApplyResult{Status: PromotionStatusReady, OK: true, DryRun: dryRun, Import: record}
	artifact, err := m.repo.Artifact(ctx, record.ArtifactID)
	if err != nil {
		result.Checks = append(result.Checks, PromotionCheck{Code: "artifact_lookup_failed", Severity: GateSeverityBlocking, Message: err.Error(), PluginID: record.PluginID})
		result.Status = PromotionStatusBlocked
		result.OK = false
		return result, nil
	}
	if artifact.PluginID != record.PluginID || (record.Version != "" && artifact.Version != record.Version) {
		result.Checks = append(result.Checks, PromotionCheck{
			Code:     "repository_import_artifact_mismatch",
			Severity: GateSeverityBlocking,
			Message:  "repository import record does not match stored artifact",
			PluginID: record.PluginID,
			Details: map[string]any{
				"import_plugin_id":   record.PluginID,
				"artifact_plugin_id": artifact.PluginID,
				"import_version":     record.Version,
				"artifact_version":   artifact.Version,
			},
		})
	}
	if record.PackageSHA256 != "" && artifact.PackageSHA256 != "" && !strings.EqualFold(record.PackageSHA256, artifact.PackageSHA256) {
		result.Checks = append(result.Checks, PromotionCheck{
			Code:     "repository_import_package_hash_mismatch",
			Severity: GateSeverityBlocking,
			Message:  "repository import package hash does not match stored artifact package",
			PluginID: record.PluginID,
			Details: map[string]any{
				"import_package_sha256":   record.PackageSHA256,
				"artifact_package_sha256": artifact.PackageSHA256,
			},
		})
	}
	if desiredState == "" {
		desiredState = DesiredDisabled
	}
	if desiredState != DesiredDisabled {
		result.Checks = append(result.Checks, PromotionCheck{
			Code:     "repository_import_auto_enable",
			Severity: GateSeverityBlocking,
			Message:  "repository import apply cannot automatically enable production traffic",
			PluginID: record.PluginID,
			Details:  map[string]any{"desired_state": desiredState},
		})
	}
	if configJSON == "" {
		configJSON = "{}"
		if current, err := m.repo.Plugin(ctx, record.PluginID); err == nil && current.ConfigJSON != "" {
			configJSON = current.ConfigJSON
		}
	}
	if _, err := m.DryRunConfig(ctx, record.PluginID, artifact.ID, configJSON); err != nil {
		result.Checks = append(result.Checks, PromotionCheck{Code: "config_dry_run_failed", Severity: GateSeverityBlocking, Message: err.Error(), PluginID: record.PluginID})
	}
	decision, err := m.EvaluateReleaseGate(ctx, record.PluginID, artifact.ID, GovernanceActionPromotion, m.currentPolicyProfile(), configJSON)
	if err != nil {
		if decision.PolicyHash == "" {
			result.Checks = append(result.Checks, PromotionCheck{Code: "governance_unavailable", Severity: GateSeverityBlocking, Message: err.Error(), PluginID: record.PluginID})
		} else {
			result.Checks = append(result.Checks, PromotionCheck{
				Code:     "governance_gate",
				Severity: GateSeverityBlocking,
				Message:  "repository import apply is blocked by governance",
				PluginID: record.PluginID,
				Details:  map[string]any{"decision": decision},
			})
		}
	} else if !decision.OK || hasWarningIssue(decision.Issues) {
		result.Checks = append(result.Checks, PromotionCheck{
			Code:     "governance_gate",
			Severity: GateSeverityBlocking,
			Message:  "repository import apply is blocked by governance",
			PluginID: record.PluginID,
			Details:  map[string]any{"decision": decision},
		})
	}
	if promotionHasBlocking(result.Checks) {
		result.Status = PromotionStatusBlocked
		result.OK = false
		return result, nil
	}
	if dryRun {
		return result, nil
	}
	if priority == 0 {
		priority = DefaultPriority
		if current, err := m.repo.Plugin(ctx, record.PluginID); err == nil && current.Priority != 0 {
			priority = current.Priority
		}
	}
	plugin, err := m.SetDesired(ctx, actor, record.PluginID, artifact.ID, DesiredDisabled, configJSON, priority)
	if err != nil {
		result.Status = PromotionStatusBlocked
		result.OK = false
		result.Checks = append(result.Checks, PromotionCheck{Code: "apply_failed", Severity: GateSeverityBlocking, Message: err.Error(), PluginID: record.PluginID})
		return result, err
	}
	result.Applied = &PromotionAppliedPlugin{
		PluginID:          plugin.ID,
		ArtifactID:        plugin.DesiredArtifactID,
		DesiredState:      plugin.DesiredState,
		DesiredGeneration: plugin.DesiredGeneration,
		Priority:          plugin.Priority,
	}
	if rollout, err := m.PluginRolloutStatus(ctx, plugin.ID); err == nil {
		result.Rollout = &rollout
	}
	_ = m.repo.RecordOperation(ctx, record.PluginID, artifact.ID, "repository_import_apply", "succeeded", actor, "repository import applied to desired state", map[string]any{
		"import_id":           record.ID,
		"dry_run":             false,
		"desired_generation":  plugin.DesiredGeneration,
		"control_plane":       "shared_plugin_desired_state",
		"cross_node_apply":    result.Rollout != nil && result.Rollout.CrossNodeApply,
		"nodes_total":         rolloutNodesTotal(result.Rollout),
		"nodes_ready":         rolloutNodesReady(result.Rollout),
		"artifact_available":  result.Rollout != nil && result.Rollout.ArtifactDistribution,
		"artifact_dist_mode":  rolloutArtifactDistributionMode(result.Rollout),
		"artifact_dist_state": rolloutArtifactDistributionStatus(result.Rollout),
	})
	return result, nil
}

func (m *Manager) AssessSupplyChain(ctx context.Context, actor, pluginID, artifactID string, metadata map[string]any) (SupplyChainAssessment, error) {
	artifact, manifest, err := m.artifactManifest(ctx, pluginID, artifactID)
	if err != nil {
		return SupplyChainAssessment{}, err
	}
	metadata = normalizeSupplyChainAssessmentMetadata(manifest, metadata)
	metadata, err = m.attachSourceBuildAssessmentMetadata(ctx, artifact, metadata)
	if err != nil {
		return SupplyChainAssessment{}, err
	}
	metadata = normalizeExternalCIArtifactMetadata(artifact, metadata)
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
	if err := validateInstrumentationReleaseEvidence(req); err != nil {
		return InstrumentationRecord{}, err
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

func validateInstrumentationReleaseEvidence(req InstrumentationRequest) error {
	status := req.Status
	if status == "" {
		status = InstrumentationStatusAvailable
	}
	if status != InstrumentationStatusAvailable {
		return nil
	}
	profile := normalizeProfile(req.Profile)
	if profile == "" {
		profile = PolicyProfileProd
	}
	if strings.TrimSpace(metadataString(req.Provenance["policy_hash"])) == "" {
		return errors.New("available instrumentation requires policy_hash governance binding")
	}
	if governance := jsonMapFromAny(req.Provenance["governance"]); governance == nil {
		return errors.New("available instrumentation requires governance metadata")
	} else {
		if ok, _ := governance["ok"].(bool); !ok {
			return errors.New("available instrumentation requires passing governance metadata")
		}
		if action := metadataString(governance["action"]); action != "" && action != GovernanceActionPromotion {
			return fmt.Errorf("available instrumentation governance action must be %s", GovernanceActionPromotion)
		}
		if reqProfile := metadataString(governance["profile"]); reqProfile != "" && normalizeProfile(reqProfile) != profile {
			return errors.New("available instrumentation governance profile does not match request profile")
		}
		if policyHash := metadataString(governance["policy_hash"]); policyHash != "" && policyHash != metadataString(req.Provenance["policy_hash"]) {
			return errors.New("available instrumentation governance policy hash does not match provenance")
		}
	}
	if strings.TrimSpace(req.GeneratedDiffHash) == "" {
		return errors.New("available instrumentation requires generated_diff_hash evidence")
	}
	if strings.TrimSpace(req.GatewayBinarySHA256) == "" {
		return errors.New("available instrumentation requires gateway_binary_sha256 binding")
	}
	if strings.TrimSpace(req.CIArtifactSHA256) == "" {
		return errors.New("available instrumentation requires ci_artifact_sha256 binding")
	}
	for _, item := range []struct {
		name     string
		evidence map[string]any
	}{
		{name: "conformance", evidence: req.Conformance},
		{name: "benchmark", evidence: req.Benchmark},
		{name: "smoke", evidence: req.Smoke},
	} {
		if ok, _ := item.evidence["ok"].(bool); !ok {
			return fmt.Errorf("available instrumentation requires passing %s evidence", item.name)
		}
	}
	return nil
}

func (m *Manager) ExportPromotionBundle(ctx context.Context, source, profile, pluginID, artifactID, configJSON string) (PromotionBundle, error) {
	artifact, manifest, err := m.artifactManifest(ctx, pluginID, artifactID)
	if err != nil {
		return PromotionBundle{}, err
	}
	if configJSON == "" {
		configJSON = "{}"
		if plugin, err := m.repo.Plugin(ctx, pluginID); err == nil && plugin.ConfigJSON != "" {
			configJSON = plugin.ConfigJSON
		}
	}
	if !json.Valid([]byte(configJSON)) {
		return PromotionBundle{}, errors.New("promotion config must be valid JSON")
	}
	if source == "" {
		source = "admin-api"
	}
	return NewPromotionBundle(source, profile, artifact, manifest, configJSON), nil
}

func NewPromotionBundle(source string, profile string, artifact ArtifactRecord, manifest Manifest, configJSON string) PromotionBundle {
	if profile == "" {
		profile = PolicyProfileDev
	}
	if configJSON == "" {
		configJSON = "{}"
	}
	plugin := PromotionPlugin{
		PluginID:          manifest.ID,
		Version:           manifest.Version,
		ArtifactType:      artifact.ArtifactType,
		RuntimeType:       manifest.Runtime.Type,
		APIVersion:        manifest.APIVersion,
		ArtifactSHA256:    artifact.SHA256,
		PackageSHA256:     artifact.PackageSHA256,
		DesiredState:      DesiredDisabled,
		ConfigHash:        stableHashJSONRaw(defaultJSONObject(configJSON)),
		ScopeHash:         stableHash(manifestScope(manifest).Values),
		RolloutHash:       stableHash(manifestRollout(manifest)),
		RuntimeLimitsHash: stableHash(manifest.RuntimeLimits),
		FeaturesHash:      stableHash(requiredFeatureInputs(manifest)),
		SecretRefs:        manifestSecretRefs(manifest),
		Environment:       normalizeProfile(profile),
		Provenance: map[string]any{
			"artifact_type": artifact.ArtifactType,
			"runtime_type":  manifest.Runtime.Type,
			"go_version":    artifact.GoVersion,
			"go_os":         artifact.GOOS,
			"go_arch":       artifact.GOARCH,
			"package_name":  artifact.FileName,
		},
	}
	if manifest.Runtime.Type == RuntimeSandbox {
		plugin.Provenance["runtime_entry"] = manifest.Runtime.Entry
		plugin.Provenance["runtime_protocol"] = manifest.Runtime.Protocol
		plugin.Provenance["runtime_os"] = manifest.Runtime.OS
		plugin.Provenance["runtime_arch"] = manifest.Runtime.Arch
		plugin.Provenance["runtime_abi_version"] = manifest.Runtime.ABIVersion
		if metadata := jsonMapFromJSONString(artifact.MetadataJSON); metadata != nil {
			if sandbox := jsonMapFromAny(metadata["sandbox"]); sandbox != nil {
				plugin.Provenance["runtime_entry_sha256"] = metadataString(sandbox["entry_sha256"])
			}
		}
	}
	bundle := PromotionBundle{
		SchemaVersion: SchemaVersion,
		APIVersion:    APIVersion,
		Profile:       normalizeProfile(profile),
		Source:        source,
		Plugins:       []PromotionPlugin{plugin},
		CreatedAt:     time.Now().Unix(),
	}
	bundle.BundleID = promotionBundleID(bundle)
	return bundle
}

func EvaluatePromotionBundle(bundle PromotionBundle) PromotionReport {
	checks := promotionBundleChecks(bundle)
	status := PromotionStatusReady
	if promotionHasBlocking(checks) {
		status = PromotionStatusBlocked
	}
	return PromotionReport{
		Status: status,
		OK:     status == PromotionStatusReady,
		Checks: checks,
		Bundle: bundle,
	}
}

func (m *Manager) ApplyPromotionBundle(ctx context.Context, actor string, bundle PromotionBundle, configByPlugin map[string]string, dryRun bool) (PromotionApplyResult, error) {
	checks := promotionBundleChecks(bundle)
	candidates := make([]promotionApplyCandidate, 0, len(bundle.Plugins))
	for _, plugin := range bundle.Plugins {
		artifact, ok, err := m.promotionApplyArtifact(ctx, plugin)
		if err != nil {
			checks = append(checks, PromotionCheck{Code: "artifact_lookup_failed", Severity: GateSeverityBlocking, Message: err.Error(), PluginID: plugin.PluginID})
			continue
		}
		if !ok {
			checks = append(checks, PromotionCheck{
				Code:     "missing_artifact",
				Severity: GateSeverityBlocking,
				Message:  "target gateway does not have the promotion artifact",
				PluginID: plugin.PluginID,
				Details:  map[string]any{"artifact_sha256": plugin.ArtifactSHA256},
			})
			continue
		}
		checks = append(checks, promotionApplyArtifactChecks(plugin, artifact)...)
		configJSON, err := m.promotionApplyConfigJSON(ctx, plugin, configByPlugin)
		if err != nil {
			checks = append(checks, PromotionCheck{Code: "environment_override_invalid", Severity: GateSeverityBlocking, Message: err.Error(), PluginID: plugin.PluginID})
			continue
		}
		if plugin.ConfigHash != "" && stableHashJSONRaw(defaultJSONObject(configJSON)) != plugin.ConfigHash {
			checks = append(checks, PromotionCheck{
				Code:     "config_hash_mismatch",
				Severity: GateSeverityBlocking,
				Message:  "target promotion config does not match bundle config hash",
				PluginID: plugin.PluginID,
				Details: map[string]any{
					"expected": plugin.ConfigHash,
					"actual":   stableHashJSONRaw(defaultJSONObject(configJSON)),
				},
			})
			continue
		}
		if _, err := m.DryRunConfig(ctx, plugin.PluginID, artifact.ID, configJSON); err != nil {
			checks = append(checks, PromotionCheck{Code: "config_dry_run_failed", Severity: GateSeverityBlocking, Message: err.Error(), PluginID: plugin.PluginID})
			continue
		}
		decision, err := m.EvaluateReleaseGate(ctx, plugin.PluginID, artifact.ID, GovernanceActionPromotion, bundle.Profile, configJSON)
		if err != nil {
			if decision.PolicyHash == "" {
				checks = append(checks, PromotionCheck{Code: "governance_unavailable", Severity: GateSeverityBlocking, Message: err.Error(), PluginID: plugin.PluginID})
			} else {
				checks = append(checks, PromotionCheck{
					Code:     "governance_gate",
					Severity: GateSeverityBlocking,
					Message:  "promotion apply is blocked by governance",
					PluginID: plugin.PluginID,
					Details:  map[string]any{"decision": decision},
				})
			}
			continue
		}
		if !decision.OK || hasWarningIssue(decision.Issues) {
			checks = append(checks, PromotionCheck{
				Code:     "governance_gate",
				Severity: GateSeverityBlocking,
				Message:  "promotion apply is blocked by governance",
				PluginID: plugin.PluginID,
				Details:  map[string]any{"decision": decision},
			})
			continue
		}
		priority := DefaultPriority
		if current, err := m.repo.Plugin(ctx, plugin.PluginID); err == nil && current.Priority != 0 {
			priority = current.Priority
		}
		desiredState := plugin.DesiredState
		if desiredState == "" {
			desiredState = DesiredDisabled
		}
		candidates = append(candidates, promotionApplyCandidate{Plugin: plugin, Artifact: artifact, ConfigJSON: configJSON, DesiredState: desiredState, Priority: priority})
	}
	result := PromotionApplyResult{Status: PromotionStatusReady, OK: true, DryRun: dryRun, Checks: checks, Bundle: bundle}
	if promotionHasBlocking(checks) {
		result.Status = PromotionStatusBlocked
		result.OK = false
		return result, nil
	}
	if dryRun {
		return result, nil
	}
	for _, candidate := range candidates {
		plugin, err := m.SetDesired(ctx, actor, candidate.Plugin.PluginID, candidate.Artifact.ID, candidate.DesiredState, candidate.ConfigJSON, candidate.Priority)
		if err != nil {
			result.Status = PromotionStatusBlocked
			result.OK = false
			result.Checks = append(result.Checks, PromotionCheck{Code: "apply_failed", Severity: GateSeverityBlocking, Message: err.Error(), PluginID: candidate.Plugin.PluginID})
			return result, err
		}
		result.Applied = append(result.Applied, PromotionAppliedPlugin{
			PluginID:          plugin.ID,
			ArtifactID:        plugin.DesiredArtifactID,
			DesiredState:      plugin.DesiredState,
			DesiredGeneration: plugin.DesiredGeneration,
			Priority:          plugin.Priority,
		})
		if rollout, err := m.PluginRolloutStatus(ctx, plugin.ID); err == nil {
			result.Rollout = append(result.Rollout, rollout)
		}
	}
	_ = m.repo.RecordOperation(ctx, "", "", "promotion_apply", "succeeded", actor, "promotion bundle applied to desired state", map[string]any{
		"bundle_id":         bundle.BundleID,
		"plugins":           len(result.Applied),
		"dry_run":           false,
		"control_plane":     "shared_plugin_desired_state",
		"rollout_nodes":     promotionRolloutNodes(result.Rollout),
		"cross_node_apply":  promotionRolloutCrossNode(result.Rollout),
		"artifact_transfer": "local_content_store_or_cli_admin_to_admin",
	})
	return result, nil
}

type promotionApplyCandidate struct {
	Plugin       PromotionPlugin
	Artifact     ArtifactRecord
	ConfigJSON   string
	DesiredState string
	Priority     int
}

func DiffPromotionBundles(current, target PromotionBundle) []PromotionDiff {
	currentByID := promotionPluginsByID(current)
	targetByID := promotionPluginsByID(target)
	var diffs []PromotionDiff
	seen := map[string]bool{}
	for pluginID, targetPlugin := range targetByID {
		seen[pluginID] = true
		currentPlugin, ok := currentByID[pluginID]
		if !ok {
			diffs = append(diffs, PromotionDiff{PluginID: pluginID, Field: "plugin", Current: "missing", Target: "present"})
			continue
		}
		diffs = append(diffs, promotionPluginDiffs(currentPlugin, targetPlugin)...)
	}
	for pluginID := range currentByID {
		if !seen[pluginID] {
			diffs = append(diffs, PromotionDiff{PluginID: pluginID, Field: "plugin", Current: "present", Target: "missing"})
		}
	}
	sort.Slice(diffs, func(i, j int) bool {
		if diffs[i].PluginID == diffs[j].PluginID {
			return diffs[i].Field < diffs[j].Field
		}
		return diffs[i].PluginID < diffs[j].PluginID
	})
	return diffs
}

func PromotionDrift(current, baseline PromotionBundle) PromotionDriftReport {
	diff := DiffPromotionBundles(baseline, current)
	status := PromotionStatusReady
	if len(diff) > 0 {
		status = PromotionStatusDrift
	}
	return PromotionDriftReport{Status: status, OK: len(diff) == 0, Diff: diff}
}

func RunPromotionDRDrill(bundle PromotionBundle) PromotionDRDrillReport {
	checks := promotionBundleChecks(bundle)
	for _, plugin := range bundle.Plugins {
		if plugin.DesiredState == DesiredEnabled {
			checks = append(checks, PromotionCheck{
				Code:     "drill_no_traffic",
				Severity: GateSeverityBlocking,
				Message:  "DR drill bundle must not enable production traffic",
				PluginID: plugin.PluginID,
			})
		}
		if plugin.ConfigHash == "" || plugin.ArtifactSHA256 == "" {
			checks = append(checks, PromotionCheck{
				Code:     "drill_incomplete_fingerprint",
				Severity: GateSeverityBlocking,
				Message:  "DR drill requires artifact and config fingerprints",
				PluginID: plugin.PluginID,
			})
		}
	}
	status := PromotionStatusReady
	if promotionHasBlocking(checks) {
		status = PromotionStatusBlocked
	}
	return PromotionDRDrillReport{Status: status, OK: status == PromotionStatusReady, Checks: checks}
}

func (m *Manager) RunPromotionDRDrill(ctx context.Context, bundle PromotionBundle, configByPlugin map[string]string) (PromotionDRDrillReport, error) {
	report := RunPromotionDRDrill(bundle)
	checks := append([]PromotionCheck(nil), report.Checks...)
	for _, plugin := range bundle.Plugins {
		if plugin.PluginID == "" || plugin.ArtifactSHA256 == "" {
			continue
		}
		artifact, ok, err := m.promotionApplyArtifact(ctx, plugin)
		if err != nil {
			checks = append(checks, PromotionCheck{Code: "artifact_lookup_failed", Severity: GateSeverityBlocking, Message: err.Error(), PluginID: plugin.PluginID})
			continue
		}
		if !ok {
			checks = append(checks, PromotionCheck{
				Code:     "missing_artifact",
				Severity: GateSeverityBlocking,
				Message:  "target gateway does not have the promotion artifact",
				PluginID: plugin.PluginID,
				Details:  map[string]any{"artifact_sha256": plugin.ArtifactSHA256},
			})
			continue
		}
		checks = append(checks, promotionApplyArtifactChecks(plugin, artifact)...)
		configJSON, err := m.promotionApplyConfigJSON(ctx, plugin, configByPlugin)
		if err != nil {
			checks = append(checks, PromotionCheck{Code: "environment_override_invalid", Severity: GateSeverityBlocking, Message: err.Error(), PluginID: plugin.PluginID})
			continue
		}
		if plugin.ConfigHash != "" && stableHashJSONRaw(defaultJSONObject(configJSON)) != plugin.ConfigHash {
			checks = append(checks, PromotionCheck{
				Code:     "config_hash_mismatch",
				Severity: GateSeverityBlocking,
				Message:  "target promotion config does not match bundle config hash",
				PluginID: plugin.PluginID,
				Details: map[string]any{
					"expected": plugin.ConfigHash,
					"actual":   stableHashJSONRaw(defaultJSONObject(configJSON)),
				},
			})
			continue
		}
		if _, err := m.DryRunConfig(ctx, plugin.PluginID, artifact.ID, configJSON); err != nil {
			checks = append(checks, PromotionCheck{Code: "config_dry_run_failed", Severity: GateSeverityBlocking, Message: err.Error(), PluginID: plugin.PluginID})
			continue
		}
		decision, err := m.EvaluateReleaseGate(ctx, plugin.PluginID, artifact.ID, GovernanceActionPromotion, bundle.Profile, configJSON)
		if err != nil {
			if decision.PolicyHash == "" {
				checks = append(checks, PromotionCheck{Code: "governance_unavailable", Severity: GateSeverityBlocking, Message: err.Error(), PluginID: plugin.PluginID})
			} else {
				checks = append(checks, PromotionCheck{
					Code:     "governance_gate",
					Severity: GateSeverityBlocking,
					Message:  "promotion DR drill is blocked by governance",
					PluginID: plugin.PluginID,
					Details:  map[string]any{"decision": decision},
				})
			}
			continue
		}
		if !decision.OK || hasWarningIssue(decision.Issues) {
			checks = append(checks, PromotionCheck{
				Code:     "governance_gate",
				Severity: GateSeverityBlocking,
				Message:  "promotion DR drill is blocked by governance",
				PluginID: plugin.PluginID,
				Details:  map[string]any{"decision": decision},
			})
		}
	}
	status := PromotionStatusReady
	if promotionHasBlocking(checks) {
		status = PromotionStatusBlocked
	}
	return PromotionDRDrillReport{Status: status, OK: status == PromotionStatusReady, Checks: checks}, nil
}

func promotionBundleID(bundle PromotionBundle) string {
	bundle.BundleID = ""
	bundle.CreatedAt = 0
	return stableHash(bundle)
}

func promotionBundleChecks(bundle PromotionBundle) []PromotionCheck {
	var checks []PromotionCheck
	if bundle.SchemaVersion != SchemaVersion {
		checks = append(checks, PromotionCheck{Code: "schema_version", Severity: GateSeverityBlocking, Message: "unsupported promotion schema version", Details: map[string]any{"schema_version": bundle.SchemaVersion}})
	}
	if bundle.APIVersion != APIVersion {
		checks = append(checks, PromotionCheck{Code: "api_version", Severity: GateSeverityBlocking, Message: "unsupported plugin API version", Details: map[string]any{"api_version": bundle.APIVersion}})
	}
	switch bundle.Profile {
	case PolicyProfileDev, PolicyProfileStaging, PolicyProfileProd:
	case "":
		checks = append(checks, PromotionCheck{Code: "policy_profile_missing", Severity: GateSeverityBlocking, Message: "promotion bundle policy profile is required"})
	default:
		checks = append(checks, PromotionCheck{Code: "policy_profile_unsupported", Severity: GateSeverityBlocking, Message: "target gateway does not support promotion policy profile", Details: map[string]any{"profile": bundle.Profile}})
	}
	if len(bundle.Plugins) == 0 {
		checks = append(checks, PromotionCheck{Code: "empty_bundle", Severity: GateSeverityBlocking, Message: "promotion bundle contains no plugins"})
	}
	for _, plugin := range bundle.Plugins {
		if plugin.PluginID == "" || plugin.ArtifactSHA256 == "" {
			checks = append(checks, PromotionCheck{Code: "plugin_identity", Severity: GateSeverityBlocking, Message: "promotion plugin identity is incomplete", PluginID: plugin.PluginID})
		}
		desiredState := plugin.DesiredState
		if desiredState == "" {
			desiredState = DesiredDisabled
		}
		if desiredState != DesiredDisabled && desiredState != DesiredDeleted && desiredState != DesiredEnabled {
			checks = append(checks, PromotionCheck{
				Code:     "desired_state_invalid",
				Severity: GateSeverityBlocking,
				Message:  "promotion desired_state is invalid",
				PluginID: plugin.PluginID,
				Details:  map[string]any{"desired_state": plugin.DesiredState},
			})
		}
		feature := RuntimeTypeFeature(plugin.RuntimeType)
		if !feature.Implemented || !feature.DataPlane {
			checks = append(checks, PromotionCheck{
				Code:     "runtime_unsupported",
				Severity: GateSeverityBlocking,
				Message:  "target gateway cannot enable requested runtime",
				PluginID: plugin.PluginID,
				Details:  map[string]any{"runtime_type": plugin.RuntimeType, "reason": feature.UnsupportedReason},
			})
		}
		if plugin.DesiredState == DesiredEnabled {
			checks = append(checks, PromotionCheck{
				Code:     "auto_enable_disabled",
				Severity: GateSeverityBlocking,
				Message:  "promotion import cannot automatically enable production traffic",
				PluginID: plugin.PluginID,
			})
		}
		if len(plugin.SecretRefs) > 0 {
			missing := missingPromotionSecretMappings(plugin)
			checks = append(checks, PromotionCheck{
				Code:     "secret_mapping_required",
				Severity: GateSeverityWarning,
				Message:  "promotion import requires target-environment secret mapping",
				PluginID: plugin.PluginID,
				Details:  map[string]any{"secret_refs": plugin.SecretRefs, "missing": missing},
			})
			if len(missing) > 0 {
				checks = append(checks, PromotionCheck{
					Code:     "secret_mapping_missing",
					Severity: GateSeverityBlocking,
					Message:  "target promotion environment does not map all required secret refs",
					PluginID: plugin.PluginID,
					Details:  map[string]any{"missing": missing},
				})
			}
		}
	}
	return checks
}

func missingPromotionSecretMappings(plugin PromotionPlugin) []string {
	var missing []string
	for _, ref := range plugin.SecretRefs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		if strings.TrimSpace(plugin.SecretMapping[ref]) == "" {
			missing = append(missing, ref)
		}
	}
	return uniqueSortedStrings(missing)
}

func (m *Manager) promotionApplyArtifact(ctx context.Context, plugin PromotionPlugin) (ArtifactRecord, bool, error) {
	artifacts, err := m.repo.ListArtifacts(ctx, plugin.PluginID)
	if err != nil {
		return ArtifactRecord{}, false, err
	}
	for _, artifact := range artifacts {
		if artifact.SHA256 == plugin.ArtifactSHA256 || artifact.ID == plugin.ArtifactSHA256 {
			return artifact, true, nil
		}
	}
	return ArtifactRecord{}, false, nil
}

func promotionApplyArtifactChecks(plugin PromotionPlugin, artifact ArtifactRecord) []PromotionCheck {
	var checks []PromotionCheck
	for _, field := range []struct {
		name string
		want string
		got  string
	}{
		{"version", plugin.Version, artifact.Version},
		{"runtime_type", plugin.RuntimeType, artifact.RuntimeType},
		{"api_version", plugin.APIVersion, artifact.APIVersion},
		{"package_sha256", plugin.PackageSHA256, artifact.PackageSHA256},
	} {
		if field.want == "" || field.got == "" || field.want == field.got {
			continue
		}
		checks = append(checks, PromotionCheck{
			Code:     field.name + "_mismatch",
			Severity: GateSeverityBlocking,
			Message:  "target artifact does not match promotion bundle",
			PluginID: plugin.PluginID,
			Details:  map[string]any{"field": field.name, "expected": field.want, "actual": field.got},
		})
	}
	return checks
}

func (m *Manager) promotionApplyConfigJSON(ctx context.Context, plugin PromotionPlugin, configByPlugin map[string]string) (string, error) {
	if configByPlugin != nil {
		if configJSON, ok := configByPlugin[plugin.PluginID]; ok {
			return applyPromotionEnvironmentOverrides(configJSON, plugin.Overrides)
		}
	}
	if current, err := m.repo.Plugin(ctx, plugin.PluginID); err == nil && current.ConfigJSON != "" {
		return applyPromotionEnvironmentOverrides(current.ConfigJSON, plugin.Overrides)
	}
	return applyPromotionEnvironmentOverrides("{}", plugin.Overrides)
}

func promotionHasBlocking(checks []PromotionCheck) bool {
	for _, check := range checks {
		if check.Severity == GateSeverityBlocking {
			return true
		}
	}
	return false
}

func promotionPluginsByID(bundle PromotionBundle) map[string]PromotionPlugin {
	out := make(map[string]PromotionPlugin, len(bundle.Plugins))
	for _, plugin := range bundle.Plugins {
		out[plugin.PluginID] = plugin
	}
	return out
}

func promotionPluginDiffs(current, target PromotionPlugin) []PromotionDiff {
	fields := []struct {
		name    string
		current string
		target  string
	}{
		{"version", current.Version, target.Version},
		{"artifact_sha256", current.ArtifactSHA256, target.ArtifactSHA256},
		{"package_sha256", current.PackageSHA256, target.PackageSHA256},
		{"runtime_type", current.RuntimeType, target.RuntimeType},
		{"api_version", current.APIVersion, target.APIVersion},
		{"desired_state", current.DesiredState, target.DesiredState},
		{"config_hash", current.ConfigHash, target.ConfigHash},
		{"scope_hash", current.ScopeHash, target.ScopeHash},
		{"rollout_hash", current.RolloutHash, target.RolloutHash},
		{"runtime_limits_hash", current.RuntimeLimitsHash, target.RuntimeLimitsHash},
		{"features_hash", current.FeaturesHash, target.FeaturesHash},
	}
	var diffs []PromotionDiff
	for _, field := range fields {
		if field.current != field.target {
			diffs = append(diffs, PromotionDiff{PluginID: target.PluginID, Field: field.name, Current: field.current, Target: field.target, Reason: promotionDriftReason(field.name, field.current, field.target)})
		}
	}
	return diffs
}

func promotionDriftReason(field, current, target string) string {
	switch field {
	case "desired_state":
		return "desired state differs from target bundle"
	case "artifact_sha256", "package_sha256", "version":
		return "actual artifact differs from desired promotion artifact"
	case "config_hash":
		return "target config hash differs from desired environment config"
	case "runtime_type":
		return "target runtime differs from desired runtime support"
	case "api_version":
		return "target plugin API differs from desired gateway API"
	case "scope_hash", "rollout_hash", "runtime_limits_hash", "features_hash":
		return "target governance fingerprint differs from desired bundle"
	default:
		return "current value differs from target value"
	}
}

func applyPromotionEnvironmentOverrides(configJSON string, overrides map[string]any) (string, error) {
	if configJSON == "" {
		configJSON = "{}"
	}
	if !json.Valid([]byte(configJSON)) {
		return "", errors.New("promotion config must be valid JSON")
	}
	if len(overrides) == 0 {
		return configJSON, nil
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(defaultJSONObject(configJSON)), &config); err != nil {
		return "", err
	}
	mergePromotionOverrideMap(config, overrides)
	data, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func mergePromotionOverrideMap(dst map[string]any, src map[string]any) {
	for key, value := range src {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if nestedSrc := jsonMapFromAny(value); nestedSrc != nil {
			if nestedDst := jsonMapFromAny(dst[key]); nestedDst != nil {
				mergePromotionOverrideMap(nestedDst, nestedSrc)
				dst[key] = nestedDst
				continue
			}
		}
		dst[key] = value
	}
}

func manifestSecretRefs(manifest Manifest) []string {
	refs := make([]string, 0, len(manifest.Secrets))
	for _, secret := range manifest.Secrets {
		if strings.TrimSpace(secret.Name) != "" {
			refs = append(refs, secret.Name)
		}
	}
	sort.Strings(refs)
	return refs
}

func requiredFeatureInputs(manifest Manifest) []string {
	var caps map[string]any
	_ = json.Unmarshal(manifest.Capabilities, &caps)
	var features []string
	for _, rawKey := range []string{"required_features", "features"} {
		features = append(features, stringSlice(caps[rawKey])...)
	}
	return uniqueSortedStrings(features)
}

func (m *Manager) SyncRepositoryIndex(ctx context.Context, actor string, req RepositoryImportRequest) (repositoryIndex, RepositoryIndexSyncRecord, error) {
	if req.RepositoryType == "" {
		req.RepositoryType = RepositoryTypeFile
	}
	req.RepositoryType = normalizeRepositoryType(req.RepositoryType)
	if err := validateRepositoryType(req.RepositoryType); err != nil {
		return repositoryIndex{}, RepositoryIndexSyncRecord{}, err
	}
	if strings.TrimSpace(actor) == "" {
		actor = "system"
	}
	index, data, err := loadRepositoryIndexData(ctx, req)
	status := RepositorySyncStatusSucceeded
	message := "repository index synced"
	record := RepositoryIndexSyncRecord{
		RepositoryType: req.RepositoryType,
		IndexPath:      req.IndexPath,
		Status:         status,
		SyncedBy:       actor,
	}
	if err == nil {
		record.RepositoryName = index.Name
		record.CandidateCount = len(index.Candidates)
		if cacheKey, cacheErr := m.storeRepositoryIndexCache(req, data); cacheErr == nil {
			record.CacheKey = cacheKey
		} else {
			record.Status = RepositorySyncStatusDegraded
			record.Error = cacheErr.Error()
			message = "repository index synced but cache write failed"
		}
	} else {
		cachedIndex, cacheKey, cacheErr := m.loadCachedRepositoryIndex(ctx, req)
		if cacheErr != nil {
			record.Status = RepositorySyncStatusFailed
			record.Error = err.Error()
			saved, saveErr := m.repo.SaveRepositoryIndexSync(ctx, record)
			if saveErr != nil {
				return repositoryIndex{}, RepositoryIndexSyncRecord{}, saveErr
			}
			_ = m.repo.RecordOperation(ctx, "", "", "repository_index_sync", "failed", actor, err.Error(), map[string]any{
				"repository_type": req.RepositoryType,
				"index_path":      redactEndpoint(req.IndexPath),
				"cache_error":     cacheErr.Error(),
			})
			return repositoryIndex{}, saved, err
		}
		index = cachedIndex
		record.Status = RepositorySyncStatusDegraded
		record.RepositoryName = index.Name
		record.CacheKey = cacheKey
		record.CandidateCount = len(index.Candidates)
		record.Error = err.Error()
		message = "repository index sync degraded to cached index"
	}
	saved, err := m.repo.SaveRepositoryIndexSync(ctx, record)
	if err != nil {
		return repositoryIndex{}, RepositoryIndexSyncRecord{}, err
	}
	_ = m.repo.RecordOperation(ctx, "", "", "repository_index_sync", saved.Status, actor, message, map[string]any{
		"repository_type": saved.RepositoryType,
		"repository":      saved.RepositoryName,
		"index_path":      redactEndpoint(saved.IndexPath),
		"cache_key":       saved.CacheKey,
		"candidates":      saved.CandidateCount,
		"degraded":        saved.Status == RepositorySyncStatusDegraded,
	})
	return index, saved, nil
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
	SHA256       string `json:"sha256,omitempty"`
}

func normalizeRepositoryType(repositoryType string) string {
	return strings.ToLower(strings.TrimSpace(repositoryType))
}

func validateRepositoryType(repositoryType string) error {
	switch repositoryType {
	case RepositoryTypeFile, RepositoryTypeURL, RepositoryTypeOfficial, RepositoryTypeInternal:
		return nil
	default:
		return fmt.Errorf("repository type %q is not supported", repositoryType)
	}
}

func loadRepositoryIndexData(ctx context.Context, req RepositoryImportRequest) (repositoryIndex, []byte, error) {
	switch {
	case isRepositoryURL(req.IndexPath):
		return readRepositoryIndexURL(ctx, req.IndexPath)
	case req.RepositoryType == RepositoryTypeURL:
		return repositoryIndex{}, nil, fmt.Errorf("url repository requires an http or https index_path")
	default:
		return readRepositoryIndex(req.IndexPath)
	}
}

func readRepositoryIndex(path string) (repositoryIndex, []byte, error) {
	var index repositoryIndex
	data, err := os.ReadFile(path)
	if err != nil {
		return index, nil, err
	}
	if err := json.Unmarshal(data, &index); err != nil {
		return index, nil, err
	}
	if index.Name == "" {
		index.Name = "local"
	}
	normalized, err := json.Marshal(index)
	if err != nil {
		return index, nil, err
	}
	return index, normalized, nil
}

func readRepositoryIndexURL(ctx context.Context, indexURL string) (repositoryIndex, []byte, error) {
	var index repositoryIndex
	data, err := readRepositoryURL(ctx, indexURL, repositoryIndexMaxBytes)
	if err != nil {
		return index, nil, err
	}
	if err := json.Unmarshal(data, &index); err != nil {
		return index, nil, err
	}
	if index.Name == "" {
		index.Name = "url"
	}
	normalized, err := json.Marshal(index)
	if err != nil {
		return index, nil, err
	}
	return index, normalized, nil
}

func (m *Manager) storeRepositoryIndexCache(req RepositoryImportRequest, data []byte) (string, error) {
	if len(data) == 0 {
		return "", errors.New("repository index cache data is empty")
	}
	key := repositoryIndexCacheKey(req)
	path := m.repositoryIndexCachePath(key)
	if path == "" {
		return "", errors.New("repository index cache root is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return "", err
	}
	return key, nil
}

func (m *Manager) loadCachedRepositoryIndex(ctx context.Context, req RepositoryImportRequest) (repositoryIndex, string, error) {
	record, err := m.repo.LatestRepositoryIndexSync(ctx, req.RepositoryType, req.IndexPath)
	if err != nil {
		return repositoryIndex{}, "", err
	}
	if record.CacheKey == "" {
		return repositoryIndex{}, "", errors.New("repository index cache key is empty")
	}
	path := m.repositoryIndexCachePath(record.CacheKey)
	if path == "" {
		return repositoryIndex{}, "", errors.New("repository index cache root is empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return repositoryIndex{}, "", err
	}
	var index repositoryIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return repositoryIndex{}, "", err
	}
	if index.Name == "" {
		index.Name = record.RepositoryName
	}
	return index, record.CacheKey, nil
}

func (m *Manager) repositoryIndexCachePath(key string) string {
	if m.store.Root == "" || key == "" {
		return ""
	}
	return filepath.Join(m.store.Root, "_repository_cache", key+".json")
}

func repositoryIndexCacheKey(req RepositoryImportRequest) string {
	sum := sha256.Sum256([]byte(req.RepositoryType + "\n" + req.IndexPath))
	return hex.EncodeToString(sum[:])
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

func materializeRepositoryArtifact(ctx context.Context, req RepositoryImportRequest, candidate repositoryCandidate) (string, string, func(), error) {
	artifactPath := candidate.ArtifactPath
	if artifactPath == "" {
		return "", "", func() {}, errors.New("repository candidate artifact_path is required")
	}
	if isRepositoryURL(artifactPath) || isRepositoryURL(req.IndexPath) {
		artifactURL, err := resolveRepositoryArtifactURL(req.IndexPath, artifactPath)
		if err != nil {
			return "", "", func() {}, err
		}
		localPath, fileName, cleanup, err := downloadRepositoryArtifact(ctx, artifactURL)
		return localPath, fileName, cleanup, err
	}
	if !filepath.IsAbs(artifactPath) {
		artifactPath = filepath.Join(filepath.Dir(req.IndexPath), artifactPath)
	}
	return artifactPath, filepath.Base(artifactPath), func() {}, nil
}

func resolveRepositoryArtifactURL(indexPath, artifactPath string) (string, error) {
	if isRepositoryURL(artifactPath) {
		return artifactPath, nil
	}
	base, err := url.Parse(indexPath)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(artifactPath)
	if err != nil {
		return "", err
	}
	return base.ResolveReference(ref).String(), nil
}

func downloadRepositoryArtifact(ctx context.Context, artifactURL string) (string, string, func(), error) {
	data, err := readRepositoryURL(ctx, artifactURL, DefaultPackageMaxBytes)
	if err != nil {
		return "", "", func() {}, err
	}
	fileName := repositoryURLFileName(artifactURL)
	tmp, err := os.CreateTemp("", "mcgp-repository-*-"+fileName)
	if err != nil {
		return "", "", func() {}, err
	}
	cleanup := func() { _ = os.Remove(tmp.Name()) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", "", func() {}, err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", "", func() {}, err
	}
	return tmp.Name(), fileName, cleanup, nil
}

func readRepositoryURL(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported repository URL scheme %q", parsed.Scheme)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("repository URL %s returned status %d", rawURL, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("repository URL %s exceeds limit %d", rawURL, maxBytes)
	}
	return data, nil
}

func isRepositoryURL(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

func repositoryURLFileName(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "artifact.mcgp"
	}
	base := path.Base(parsed.Path)
	if base == "." || base == "/" || strings.TrimSpace(base) == "" {
		return "artifact.mcgp"
	}
	return base
}

func compareRepositoryVersions(available, current string) (int, bool) {
	availableParts, okAvailable := parseRepositoryVersion(available)
	currentParts, okCurrent := parseRepositoryVersion(current)
	if !okAvailable || !okCurrent {
		return 0, false
	}
	maxLen := len(availableParts)
	if len(currentParts) > maxLen {
		maxLen = len(currentParts)
	}
	for i := 0; i < maxLen; i++ {
		a, c := 0, 0
		if i < len(availableParts) {
			a = availableParts[i]
		}
		if i < len(currentParts) {
			c = currentParts[i]
		}
		switch {
		case a > c:
			return 1, true
		case a < c:
			return -1, true
		}
	}
	return 0, true
}

func parseRepositoryVersion(version string) ([]int, bool) {
	version = strings.TrimSpace(strings.TrimPrefix(version, "v"))
	if version == "" {
		return nil, false
	}
	if i := strings.IndexAny(version, "+-"); i >= 0 {
		version = version[:i]
	}
	parts := strings.Split(version, ".")
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, false
		}
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 {
			return nil, false
		}
		out = append(out, value)
	}
	return out, true
}

func supplyChainIssues(artifact ArtifactRecord, manifest Manifest, metadata map[string]any) []GovernanceIssue {
	var issues []GovernanceIssue
	signature := jsonMapFromAny(metadata["signature"])
	if requiredBool(signature, "required") && !requiredBool(signature, "verified") {
		issues = append(issues, issue("signature_unverified", GateSeverityBlocking, "required artifact signature is not verified", artifact.PluginID, artifact.ID, nil))
	}
	sbom := jsonMapFromAny(metadata["sbom"])
	issues = append(issues, externalCITrustIssues(artifact, signature, sbom, jsonMapFromAny(metadata["external_ci"]))...)
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
	if unknown := stringSlice(license["unknown"]); len(unknown) > 0 {
		issues = append(issues, issue("license_unknown", GateSeverityBlocking, "artifact license is unknown and policy disallows unknown licenses", artifact.PluginID, artifact.ID, map[string]any{"licenses": unknown}))
	}
	if review := stringSlice(license["review_required_matches"]); len(review) > 0 {
		issues = append(issues, issue("license_review_required", GateSeverityWarning, "artifact license requires review", artifact.PluginID, artifact.ID, map[string]any{"licenses": review}))
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

func normalizeExternalCIArtifactMetadata(artifact ArtifactRecord, metadata map[string]any) map[string]any {
	externalCI := jsonMapFromAny(metadata["external_ci"])
	if externalCI == nil {
		return metadata
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	if artifactSHA := metadataString(externalCI["artifact_sha256"]); artifactSHA != "" && artifact.SHA256 != "" {
		externalCI["artifact_sha256_matches"] = strings.EqualFold(artifactSHA, artifact.SHA256)
	}
	if packageSHA := metadataString(externalCI["package_sha256"]); packageSHA != "" && artifact.PackageSHA256 != "" {
		externalCI["package_sha256_matches"] = strings.EqualFold(packageSHA, artifact.PackageSHA256)
	}
	if artifact.RuntimeType == RuntimeWASM {
		if moduleSHA := wasmExternalCIModuleSHA(externalCI); moduleSHA != "" && artifact.SHA256 != "" {
			externalCI["wasm_module_sha256_matches"] = strings.EqualFold(moduleSHA, artifact.SHA256)
		}
	}
	if externalCIRequired(externalCI) {
		missing := missingExternalCIProvenanceMetadataFieldsForArtifact(artifact, metadata, externalCI)
		externalCI["provenance_complete"] = len(missing) == 0
		if len(missing) > 0 {
			externalCI["missing_fields"] = missing
		}
	}
	metadata["external_ci"] = externalCI
	return metadata
}

func externalCITrustIssues(artifact ArtifactRecord, signature, sbom, externalCI map[string]any) []GovernanceIssue {
	if externalCI == nil {
		return nil
	}
	required := externalCIRequired(externalCI)
	var issues []GovernanceIssue
	if artifactSHA := metadataString(externalCI["artifact_sha256"]); artifactSHA != "" && artifact.SHA256 != "" && !strings.EqualFold(artifactSHA, artifact.SHA256) {
		issues = append(issues, issue("external_ci_artifact_hash_mismatch", GateSeverityBlocking, "external CI provenance artifact hash does not match stored artifact", artifact.PluginID, artifact.ID, map[string]any{
			"expected": artifact.SHA256,
			"actual":   artifactSHA,
		}))
	}
	if packageSHA := metadataString(externalCI["package_sha256"]); packageSHA != "" && artifact.PackageSHA256 != "" && !strings.EqualFold(packageSHA, artifact.PackageSHA256) {
		issues = append(issues, issue("external_ci_package_hash_mismatch", GateSeverityBlocking, "external CI provenance package hash does not match stored package", artifact.PluginID, artifact.ID, map[string]any{
			"expected": artifact.PackageSHA256,
			"actual":   packageSHA,
		}))
	}
	if !required {
		return issues
	}
	if missing := missingExternalCIProvenanceMetadataFieldsForArtifact(artifact, map[string]any{"signature": signature, "sbom": sbom}, externalCI); len(missing) > 0 {
		issues = append(issues, issue("external_ci_provenance_incomplete", GateSeverityBlocking, "external CI provenance is missing required fields", artifact.PluginID, artifact.ID, map[string]any{"missing": missing}))
	}
	if artifact.RuntimeType == RuntimeWASM {
		if moduleSHA := wasmExternalCIModuleSHA(externalCI); moduleSHA != "" && artifact.SHA256 != "" && !strings.EqualFold(moduleSHA, artifact.SHA256) {
			issues = append(issues, issue("external_ci_wasm_module_hash_mismatch", GateSeverityBlocking, "external CI WASM module hash does not match stored artifact", artifact.PluginID, artifact.ID, map[string]any{
				"expected": artifact.SHA256,
				"actual":   moduleSHA,
			}))
		}
	}
	if !requiredBool(signature, "verified") {
		issues = append(issues, issue("external_ci_signature_unverified", GateSeverityBlocking, "external CI artifact signature is not verified", artifact.PluginID, artifact.ID, nil))
	}
	if !requiredBool(externalCI, "trusted") {
		issues = append(issues, issue("external_ci_untrusted", GateSeverityBlocking, "external CI provenance is not trusted by the target policy", artifact.PluginID, artifact.ID, nil))
	}
	return issues
}

func missingExternalCIProvenanceMetadataFields(metadata map[string]any, externalCI map[string]any) []string {
	return missingExternalCIProvenanceMetadataFieldsForArtifact(ArtifactRecord{}, metadata, externalCI)
}

func missingExternalCIProvenanceMetadataFieldsForArtifact(artifact ArtifactRecord, metadata map[string]any, externalCI map[string]any) []string {
	missing := missingExternalCIProvenanceFields(externalCI)
	if artifact.RuntimeType == RuntimeWASM {
		if wasmExternalCIToolchain(externalCI) == "" {
			missing = append(missing, "wasm_toolchain")
		}
		if wasmExternalCITarget(externalCI) == "" {
			missing = append(missing, "wasm_target")
		}
		if wasmExternalCIModuleSHA(externalCI) == "" {
			missing = append(missing, "wasm_module_sha256")
		}
	}
	if !metadataFieldPresent(metadata["signature"]) {
		missing = append(missing, "signature")
	}
	if !metadataFieldPresent(metadata["sbom"]) {
		missing = append(missing, "sbom_metadata")
	}
	return missing
}

func wasmExternalCIToolchain(externalCI map[string]any) string {
	return firstMetadataString(externalCI, "wasm_toolchain", "toolchain", "runtime_toolchain")
}

func wasmExternalCITarget(externalCI map[string]any) string {
	return firstMetadataString(externalCI, "wasm_target", "target", "runtime_target")
}

func wasmExternalCIModuleSHA(externalCI map[string]any) string {
	return firstMetadataString(externalCI, "wasm_module_sha256", "module_sha256")
}

func externalCIRequired(externalCI map[string]any) bool {
	if externalCI == nil {
		return false
	}
	if _, ok := externalCI["required"]; !ok {
		return true
	}
	return requiredBool(externalCI, "required")
}

func missingExternalCIProvenanceFields(externalCI map[string]any) []string {
	required := []string{
		"source_sha256",
		"artifact_sha256",
		"run_id",
		"builder_id",
		"attestation",
		"sbom",
		"release_provenance",
	}
	var missing []string
	for _, field := range required {
		if !metadataFieldPresent(externalCI[field]) {
			missing = append(missing, field)
		}
	}
	return missing
}

func metadataFieldPresent(value any) bool {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed) != ""
	case map[string]any:
		return len(typed) > 0
	case []any:
		return len(typed) > 0
	default:
		return value != nil
	}
}

func metadataString(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

type supplyChainLicensePolicy struct {
	Allowed           []string
	Denied            []string
	ReviewRequired    []string
	AllowUnknown      bool
	AllowUnknownSet   bool
	ApplyToTransitive bool
}

func normalizeSupplyChainAssessmentMetadata(manifest Manifest, metadata map[string]any) map[string]any {
	if metadata == nil {
		metadata = map[string]any{}
	} else {
		copy := make(map[string]any, len(metadata))
		for key, value := range metadata {
			copy[key] = value
		}
		metadata = copy
	}
	license := jsonMapFromAny(metadata["license"])
	policy := licensePolicyFromAssessmentMetadata(metadata, license)
	if license == nil {
		license = map[string]any{}
	}
	declared := collectSupplyChainLicenseIDs(manifest, license, policy.ApplyToTransitive)
	if len(declared) > 0 {
		license["declared"] = declared
	}
	denied := intersectLicensePolicy(declared, policy.Denied)
	if len(denied) > 0 {
		license["denylist_matches"] = uniqueSortedStrings(append(stringSlice(license["denylist_matches"]), denied...))
	}
	if len(policy.Allowed) > 0 {
		if len(declared) == 0 {
			if policy.AllowUnknownSet && !policy.AllowUnknown {
				license["unknown"] = uniqueSortedStrings(append(stringSlice(license["unknown"]), "NOASSERTION"))
			}
		} else if missing := missingFromLicenseAllowlist(declared, policy.Allowed); len(missing) > 0 {
			license["allowlist_missing"] = uniqueSortedStrings(append(stringSlice(license["allowlist_missing"]), missing...))
		}
	} else if len(declared) == 0 && policy.AllowUnknownSet && !policy.AllowUnknown {
		license["unknown"] = uniqueSortedStrings(append(stringSlice(license["unknown"]), "NOASSERTION"))
	}
	if review := intersectLicensePolicy(declared, policy.ReviewRequired); len(review) > 0 {
		license["review_required_matches"] = uniqueSortedStrings(append(stringSlice(license["review_required_matches"]), review...))
	}
	if len(license) > 0 {
		metadata["license"] = license
	}
	return metadata
}

func licensePolicyFromAssessmentMetadata(metadata map[string]any, license map[string]any) supplyChainLicensePolicy {
	policyMap := jsonMapFromAny(metadata["license_policy"])
	if policyMap == nil {
		policyMap = jsonMapFromAny(license["policy"])
	}
	policy := supplyChainLicensePolicy{AllowUnknown: true}
	if policyMap == nil {
		return policy
	}
	policy.Allowed = normalizeLicenseIDs(append(append(stringSlice(policyMap["allowed"]), stringSlice(policyMap["allowlist"])...), stringSlice(policyMap["allowed_licenses"])...))
	policy.Denied = normalizeLicenseIDs(append(append(stringSlice(policyMap["denied"]), stringSlice(policyMap["denylist"])...), stringSlice(policyMap["denied_licenses"])...))
	policy.ReviewRequired = normalizeLicenseIDs(append(stringSlice(policyMap["review_required"]), stringSlice(policyMap["review_required_licenses"])...))
	if value, ok := policyMap["allow_unknown"].(bool); ok {
		policy.AllowUnknown = value
		policy.AllowUnknownSet = true
	}
	if value, ok := policyMap["apply_to_transitive"].(bool); ok {
		policy.ApplyToTransitive = value
	}
	return policy
}

func collectSupplyChainLicenseIDs(manifest Manifest, license map[string]any, includeTransitive bool) []string {
	var values []string
	for _, key := range []string{"id", "ids", "license", "licenses", "expression", "spdx", "declared", "declared_license", "declared_licenses", "detected", "detected_licenses"} {
		values = append(values, licenseValuesFromAny(license[key])...)
	}
	if len(manifest.SupplyChain) > 0 {
		var supply map[string]any
		if json.Unmarshal(manifest.SupplyChain, &supply) == nil {
			for _, key := range []string{"license", "licenses", "license_expression", "declared_license", "declared_licenses"} {
				values = append(values, licenseValuesFromAny(supply[key])...)
			}
			if includeTransitive {
				for _, dep := range anySlice(supply["dependencies"]) {
					if depMap := jsonMapFromAny(dep); depMap != nil {
						for _, key := range []string{"license", "licenses", "license_expression"} {
							values = append(values, licenseValuesFromAny(depMap[key])...)
						}
					}
				}
			}
		}
	}
	return normalizeLicenseIDs(values)
}

func licenseValuesFromAny(value any) []string {
	switch typed := value.(type) {
	case nil:
		return nil
	case string:
		return []string{typed}
	case []string:
		return typed
	case []any:
		var values []string
		for _, item := range typed {
			values = append(values, licenseValuesFromAny(item)...)
		}
		return values
	case map[string]any:
		var values []string
		for _, key := range []string{"id", "license", "expression", "spdx"} {
			values = append(values, licenseValuesFromAny(typed[key])...)
		}
		return values
	default:
		return nil
	}
}

func normalizeLicenseIDs(values []string) []string {
	var out []string
	for _, value := range values {
		for _, token := range licenseExpressionTokens(value) {
			out = append(out, strings.ToUpper(token))
		}
	}
	return uniqueSortedStrings(out)
}

func licenseExpressionTokens(value string) []string {
	replacer := strings.NewReplacer("(", " ", ")", " ", "[", " ", "]", " ", "{", " ", "}", " ", ",", " ", ";", " ", "\n", " ", "\t", " ")
	value = replacer.Replace(value)
	var out []string
	for _, token := range strings.Fields(value) {
		token = strings.Trim(token, `"'`)
		upper := strings.ToUpper(token)
		switch upper {
		case "", "AND", "OR", "WITH":
			continue
		default:
			out = append(out, token)
		}
	}
	return out
}

func intersectLicensePolicy(declared, policy []string) []string {
	if len(declared) == 0 || len(policy) == 0 {
		return nil
	}
	allowed := make(map[string]bool, len(policy))
	for _, item := range policy {
		allowed[item] = true
	}
	var matches []string
	for _, item := range declared {
		if allowed[item] {
			matches = append(matches, item)
		}
	}
	return uniqueSortedStrings(matches)
}

func missingFromLicenseAllowlist(declared, allowed []string) []string {
	if len(declared) == 0 || len(allowed) == 0 {
		return nil
	}
	allowedSet := make(map[string]bool, len(allowed))
	for _, item := range allowed {
		allowedSet[item] = true
	}
	var missing []string
	for _, item := range declared {
		if !allowedSet[item] {
			missing = append(missing, item)
		}
	}
	return uniqueSortedStrings(missing)
}

func anySlice(value any) []any {
	switch typed := value.(type) {
	case []any:
		return typed
	case []map[string]any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, item)
		}
		return out
	default:
		return nil
	}
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
