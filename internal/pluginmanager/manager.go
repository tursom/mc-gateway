package pluginmanager

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	stdplugin "plugin"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tursom/mc-gateway/plugin/api"
)

type RuntimeAdapter interface {
	Load(ctx context.Context, artifact ArtifactRecord, pluginRecord PluginRecord, gateway *Gateway) (api.Plugin, error)
}

type ConfigDryRunAdapter interface {
	DryRunConfig(ctx context.Context, artifact ArtifactRecord, pluginRecord PluginRecord) error
}

type GoPluginAdapter struct{}

func (a GoPluginAdapter) Load(ctx context.Context, artifact ArtifactRecord, pluginRecord PluginRecord, gateway *Gateway) (api.Plugin, error) {
	return a.instantiate(ctx, artifact, pluginRecord, gateway, true)
}

func (a GoPluginAdapter) DryRunConfig(ctx context.Context, artifact ArtifactRecord, pluginRecord PluginRecord) error {
	_ = ctx
	_, err := a.instantiate(ctx, artifact, pluginRecord, nil, false)
	return err
}

func (a GoPluginAdapter) RunPreflight(ctx context.Context, artifact ArtifactRecord, pluginRecord PluginRecord, profile, action string) (api.PreflightResult, error) {
	instance, err := a.instantiate(ctx, artifact, pluginRecord, nil, false)
	if err != nil {
		return api.PreflightResult{}, err
	}
	checker, ok := instance.(api.PreflightChecker)
	if !ok {
		return api.PreflightResult{}, nil
	}
	var config map[string]any
	_ = json.Unmarshal([]byte(defaultJSONObject(pluginRecord.ConfigJSON)), &config)
	return checker.Preflight(api.PreflightContext{
		PluginID:      pluginRecord.ID,
		ArtifactID:    artifact.ID,
		Profile:       profile,
		Action:        action,
		Config:        config,
		Scope:         jsonObjectFromRaw(manifestCapabilitiesRaw(artifact), "scope"),
		Rollout:       jsonObjectFromRaw(manifestCapabilitiesRaw(artifact), "rollout"),
		RuntimeLimits: jsonObjectFromRaw(artifact.MetadataJSON, "runtime_limits"),
		Features:      stringSlice(jsonObjectFromRaw(manifestCapabilitiesRaw(artifact), "required_features")),
	})
}

func (a GoPluginAdapter) RunSelfTest(ctx context.Context, artifact ArtifactRecord, pluginRecord PluginRecord, profile string) (api.SelfTestResult, error) {
	instance, err := a.instantiate(ctx, artifact, pluginRecord, nil, false)
	if err != nil {
		return api.SelfTestResult{}, err
	}
	tester, ok := instance.(api.SelfTester)
	if !ok {
		return api.SelfTestResult{}, nil
	}
	return tester.SelfTest(api.SelfTestProfile{Name: profile})
}

func (a GoPluginAdapter) instantiate(ctx context.Context, artifact ArtifactRecord, pluginRecord PluginRecord, gateway *Gateway, init bool) (api.Plugin, error) {
	_ = ctx
	opened, err := stdplugin.Open(artifact.FilePath)
	if err != nil {
		return nil, err
	}
	symbolName := "Plugin"
	var manifest Manifest
	if err := json.Unmarshal([]byte(artifact.MetadataJSON), &manifest); err == nil && manifest.Runtime.EntrySymbol != "" {
		symbolName = manifest.Runtime.EntrySymbol
	}
	symbol, err := opened.Lookup(symbolName)
	if err != nil {
		return nil, err
	}
	factory, ok := symbol.(func() api.Plugin)
	if !ok {
		return nil, fmt.Errorf("plugin symbol %q has invalid signature", symbolName)
	}
	instance := factory()
	cfg := instance.NewConfigObj()
	if cfg != nil && pluginRecord.ConfigJSON != "" && canUnmarshalInto(cfg) {
		if err := json.Unmarshal([]byte(pluginRecord.ConfigJSON), cfg); err != nil {
			return nil, fmt.Errorf("decode plugin config: %w", err)
		}
	}
	if err := instance.ReloadConfig(cfg); err != nil {
		return nil, err
	}
	if init {
		if err := instance.Init(gateway); err != nil {
			return nil, err
		}
	}
	return instance, nil
}

func canUnmarshalInto(value any) bool {
	if value == nil {
		return false
	}
	kind := reflect.TypeOf(value).Kind()
	return kind == reflect.Pointer || kind == reflect.Map || kind == reflect.Slice
}

type Manager struct {
	repo          Repository
	store         ArtifactStore
	adapter       RuntimeAdapter
	builders      map[string]SourceBuilder
	handleConn    func(net.Conn)
	wg            *sync.WaitGroup
	policyProfile string

	mu       sync.Mutex
	loaded   map[string]*loadedPlugin
	snapshot atomic.Value

	proxyMu     sync.Mutex
	proxySeq    uint64
	proxyConns  map[uint64]*proxyConnection
	drainingIDs map[string]bool
	operations  *Operations
}

type loadedPlugin struct {
	record   PluginRecord
	artifact ArtifactRecord
	instance api.Plugin
	gateway  *Gateway
	handlers []*upstreamHandler
}

type upstreamHandler struct {
	pluginID            string
	artifactID          string
	priority            int
	handlerID           string
	mode                string
	timeout             time.Duration
	initialWriteTimeout time.Duration
	accept              func(api.UpstreamConnectRequest) bool
	handle              func(api.UpstreamConnectRequest) (net.Conn, error)

	calls          atomic.Uint64
	errors         atomic.Uint64
	panics         atomic.Uint64
	timeouts       atomic.Uint64
	blocked        atomic.Uint64
	activeProxy    atomic.Int64
	proxyStarted   atomic.Uint64
	proxyCompleted atomic.Uint64
	proxyErrors    atomic.Uint64
	proxyBytesIn   atomic.Uint64
	proxyBytesOut  atomic.Uint64
	proxyDuration  atomic.Uint64
	durationCount  atomic.Uint64
	durationSumMS  atomic.Uint64
	durationMaxMS  atomic.Uint64
}

type proxyConnection struct {
	id         uint64
	pluginID   string
	artifactID string
	handlerID  string
	handler    *upstreamHandler
	client     net.Conn
	endpoint   net.Conn
	startedAt  time.Time
	draining   bool
}

type ProxyConnectionHandle struct {
	manager *Manager
	id      uint64
}

type ProxyConnectionStats struct {
	BytesToPlugin int64
	BytesToClient int64
	Duration      time.Duration
	Err           error
}

type Options struct {
	DB            *sql.DB
	ArtifactRoot  string
	HandleConn    func(net.Conn)
	WaitGroup     *sync.WaitGroup
	Adapter       RuntimeAdapter
	Builders      map[string]SourceBuilder
	PolicyProfile string
}

func New(options Options) *Manager {
	adapter := options.Adapter
	if adapter == nil {
		adapter = GoPluginAdapter{}
	}
	manager := &Manager{
		repo:          NewRepository(options.DB),
		store:         NewArtifactStore(options.ArtifactRoot),
		adapter:       adapter,
		builders:      options.Builders,
		handleConn:    options.HandleConn,
		wg:            options.WaitGroup,
		policyProfile: options.PolicyProfile,
		loaded:        make(map[string]*loadedPlugin),
		proxyConns:    make(map[uint64]*proxyConnection),
		drainingIDs:   make(map[string]bool),
	}
	manager.operations = NewOperations(manager.repo, options.ArtifactRoot)
	if manager.builders == nil {
		manager.builders = map[string]SourceBuilder{
			BuilderTypeLocalProcess: LocalProcessBuilder{StoreRoot: options.ArtifactRoot},
			BuilderTypeContainer:    ContainerBuilder{},
		}
	}
	manager.publish(nil)
	return manager
}

func (m *Manager) UploadArtifact(ctx context.Context, upload ArtifactUpload) (ArtifactRecord, error) {
	artifact, err := m.store.ValidateAndStore(upload)
	if err != nil {
		_ = m.repo.RecordOperation(ctx, "", "", "artifact_upload", "failed", upload.Actor, err.Error(), nil)
		return ArtifactRecord{}, err
	}
	if artifact.ArtifactType == ArtifactTypeSource {
		return m.saveSourceArtifact(ctx, upload.Actor, artifact, "artifact_upload")
	}
	if err := m.repo.SaveArtifact(ctx, artifact); err != nil {
		return ArtifactRecord{}, err
	}
	_ = m.repo.RecordOperation(ctx, artifact.PluginID, artifact.ID, "artifact_upload", "succeeded", upload.Actor, "artifact uploaded", map[string]any{
		"sha256":           artifact.SHA256,
		"package_sha256":   artifact.PackageSHA256,
		"api_version":      artifact.APIVersion,
		"extension_points": artifact.ExtensionPointsJSON,
	})
	return artifact, nil
}

func (m *Manager) UploadSource(ctx context.Context, upload ArtifactUpload) (ArtifactRecord, error) {
	artifact, err := m.store.ValidateAndStoreSource(upload)
	if err != nil {
		_ = m.repo.RecordOperation(ctx, "", "", "source_upload", "failed", upload.Actor, err.Error(), nil)
		return ArtifactRecord{}, err
	}
	return m.saveSourceArtifact(ctx, upload.Actor, artifact, "source_upload")
}

func (m *Manager) saveSourceArtifact(ctx context.Context, actor string, artifact ArtifactRecord, operation string) (ArtifactRecord, error) {
	if err := m.repo.SaveArtifact(ctx, artifact); err != nil {
		return ArtifactRecord{}, err
	}
	build, err := m.CreateBuild(ctx, actor, BuildRequest{SourceID: artifact.ID})
	if err != nil {
		_ = m.repo.RecordOperation(ctx, artifact.PluginID, artifact.ID, "source_build_queue", "failed", actor, err.Error(), nil)
		return ArtifactRecord{}, err
	}
	_ = m.repo.RecordOperation(ctx, artifact.PluginID, artifact.ID, operation, "succeeded", actor, "source package uploaded", map[string]any{
		"source_sha256": artifact.SHA256,
		"api_version":   artifact.APIVersion,
		"go_version":    artifact.GoVersion,
		"build_id":      build.ID,
	})
	return artifact, nil
}

func (m *Manager) BuildSource(ctx context.Context, actor string, req BuildRequest) (BuildRecord, error) {
	build, err := m.CreateBuild(ctx, actor, req)
	if err != nil {
		return BuildRecord{}, err
	}
	return m.RunBuild(ctx, actor, build.ID)
}

func (m *Manager) CreateBuild(ctx context.Context, actor string, req BuildRequest) (BuildRecord, error) {
	source, err := m.repo.Artifact(ctx, req.SourceID)
	if err != nil {
		return BuildRecord{}, err
	}
	if source.ArtifactType != ArtifactTypeSource {
		return BuildRecord{}, fmt.Errorf("artifact %s is %q, want source", source.ID, source.ArtifactType)
	}
	req = defaultBuildRequest(req, source)
	if req.GOOS != runtime.GOOS || req.GOARCH != runtime.GOARCH {
		return BuildRecord{}, fmt.Errorf("build target %s/%s does not match gateway %s/%s", req.GOOS, req.GOARCH, runtime.GOOS, runtime.GOARCH)
	}
	builder := m.builders[req.BuilderType]
	if builder == nil {
		return BuildRecord{}, fmt.Errorf("builder type %q is not available", req.BuilderType)
	}
	build, err := m.repo.CreateBuild(ctx, BuildRecord{
		PluginID:       source.PluginID,
		SourceID:       source.ID,
		Status:         BuildStatusQueued,
		BuilderType:    req.BuilderType,
		BuilderImage:   req.BuilderImage,
		BuilderVersion: req.BuilderVersion,
		GOOS:           req.GOOS,
		GOARCH:         req.GOARCH,
		GOAMD64:        req.GOAMD64,
		GOARM64:        req.GOARM64,
		CGOEnabled:     req.CGOEnabled,
		BuildTags:      req.BuildTags,
		SDKModule:      req.SDKModule,
		SDKVersion:     req.SDKVersion,
		GOPROXY:        req.GOPROXY,
		GONOSUMDB:      req.GONOSUMDB,
		GOPRIVATE:      req.GOPRIVATE,
		VendorRequired: req.VendorRequired,
		SourceSHA256:   source.SHA256,
		ModuleSummary:  "[]",
		GoVersionM:     "{}",
		MetadataJSON:   "{}",
		CreatedBy:      actor,
	})
	if err != nil {
		return BuildRecord{}, err
	}
	_ = m.repo.RecordOperation(ctx, source.PluginID, source.ID, "source_build_queue", "succeeded", actor, "source build queued", map[string]any{
		"build_id":      build.ID,
		"builder_type":  req.BuilderType,
		"source_sha256": source.SHA256,
	})
	return build, nil
}

func (m *Manager) RunBuild(ctx context.Context, actor string, buildID int64) (BuildRecord, error) {
	build, err := m.repo.Build(ctx, buildID)
	if err != nil {
		return BuildRecord{}, err
	}
	if build.Status != BuildStatusQueued {
		return BuildRecord{}, fmt.Errorf("build status %q cannot be run", build.Status)
	}
	source, err := m.repo.Artifact(ctx, build.SourceID)
	if err != nil {
		return BuildRecord{}, err
	}
	req := BuildRequest{
		SourceID:       build.SourceID,
		BuilderType:    build.BuilderType,
		BuilderImage:   build.BuilderImage,
		BuilderVersion: build.BuilderVersion,
		GOOS:           build.GOOS,
		GOARCH:         build.GOARCH,
		GOAMD64:        build.GOAMD64,
		GOARM64:        build.GOARM64,
		CGOEnabled:     build.CGOEnabled,
		BuildTags:      build.BuildTags,
		SDKModule:      build.SDKModule,
		SDKVersion:     build.SDKVersion,
		GOPROXY:        build.GOPROXY,
		GONOSUMDB:      build.GONOSUMDB,
		GOPRIVATE:      build.GOPRIVATE,
		VendorRequired: build.VendorRequired,
	}
	builder := m.builders[build.BuilderType]
	if builder == nil {
		return BuildRecord{}, fmt.Errorf("builder type %q is not available", build.BuilderType)
	}
	start := time.Now()
	if err := m.repo.MarkBuildRunning(ctx, build.ID); err != nil {
		return BuildRecord{}, err
	}
	build, _ = m.repo.Build(ctx, build.ID)
	result, buildErr := builder.Build(ctx, source, req, build)
	build.StartedAt = start.Unix()
	build.EndedAt = time.Now().Unix()
	build.DurationMS = buildDurationMS(start)
	build.GoVersion = result.GoVersion
	build.ModuleSummary = result.ModuleSummary
	build.GoVersionM = result.GoVersionM
	build.ABIFingerprint = result.ABIFingerprint
	build.LogSummary = result.LogSummary
	build.SourceSHA256 = source.SHA256
	if buildErr != nil {
		build.Status = BuildStatusFailed
		build.Error = buildErr.Error()
		_ = m.repo.FinishBuild(ctx, build)
		_ = m.repo.RecordOperation(ctx, source.PluginID, source.ID, "source_build", "failed", actor, buildErr.Error(), map[string]any{
			"build_id":       build.ID,
			"source_sha256":  source.SHA256,
			"builder_type":   req.BuilderType,
			"log_summary":    result.LogSummary,
			"active_changed": false,
		})
		return m.repo.Build(ctx, build.ID)
	}
	if result.Manifest.GoVersion != result.GoVersion {
		build.Status = BuildStatusFailed
		build.Error = fmt.Sprintf("manifest go_version %q does not match built Go version %q", result.Manifest.GoVersion, result.GoVersion)
		_ = m.repo.FinishBuild(ctx, build)
		return m.repo.Build(ctx, build.ID)
	}
	metadata := result.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["build_id"] = build.ID
	metadata["module_summary"] = json.RawMessage(defaultJSONArray(result.ModuleSummary))
	metadata["go_version_m"] = json.RawMessage(defaultJSONObject(result.GoVersionM))
	artifact, err := m.store.StoreBuiltBinary(ArtifactUpload{
		FileName: result.Manifest.ID + "-" + result.Manifest.Version + ".mcgp",
		Actor:    actor,
	}, result.Manifest, result.ArtifactBytes, source.PackageSHA256, metadata)
	if err != nil {
		build.Status = BuildStatusFailed
		build.Error = err.Error()
		_ = m.repo.FinishBuild(ctx, build)
		return m.repo.Build(ctx, build.ID)
	}
	if err := m.repo.SaveArtifact(ctx, artifact); err != nil {
		build.Status = BuildStatusFailed
		build.Error = err.Error()
		_ = m.repo.FinishBuild(ctx, build)
		return m.repo.Build(ctx, build.ID)
	}
	build.Status = BuildStatusSucceeded
	build.ArtifactID = artifact.ID
	build.ArtifactSHA256 = artifact.SHA256
	metadataBytes, _ := json.Marshal(metadata)
	build.MetadataJSON = string(metadataBytes)
	if err := m.repo.FinishBuild(ctx, build); err != nil {
		return BuildRecord{}, err
	}
	_ = m.repo.RecordOperation(ctx, source.PluginID, artifact.ID, "source_build", "succeeded", actor, "source build succeeded", map[string]any{
		"build_id":        build.ID,
		"source_id":       source.ID,
		"source_sha256":   source.SHA256,
		"artifact_sha256": artifact.SHA256,
		"builder_type":    req.BuilderType,
		"go_version":      result.GoVersion,
	})
	return m.repo.Build(ctx, build.ID)
}

func (m *Manager) SetDesired(ctx context.Context, actor, pluginID, artifactID, desiredState, configJSON string, priority int) (PluginRecord, error) {
	if desiredState == "" {
		desiredState = DesiredDisabled
	}
	if desiredState != DesiredDeleted {
		if _, err := m.DryRunConfig(ctx, pluginID, artifactID, configJSON); err != nil {
			_ = m.repo.RecordOperation(ctx, pluginID, artifactID, "config_dry_run", "failed", actor, err.Error(), map[string]any{
				"active_changed": false,
			})
			return PluginRecord{}, err
		}
	}
	pluginRecord, err := m.repo.UpsertDesired(ctx, actor, pluginID, artifactID, desiredState, configJSON, priority)
	if err != nil {
		_ = m.repo.RecordOperation(ctx, pluginID, artifactID, "desired_update", "failed", actor, err.Error(), nil)
		return PluginRecord{}, err
	}
	_ = m.repo.RecordOperation(ctx, pluginID, artifactID, "desired_update", "succeeded", actor, "desired state updated", map[string]any{
		"desired_state":      desiredState,
		"desired_generation": pluginRecord.DesiredGeneration,
		"priority":           pluginRecord.Priority,
	})
	return pluginRecord, nil
}

func (m *Manager) DryRunConfig(ctx context.Context, pluginID, artifactID, configJSON string) (ConfigDryRunResult, error) {
	result := ConfigDryRunResult{
		OK:         false,
		PluginID:   pluginID,
		ArtifactID: artifactID,
	}
	if configJSON == "" {
		configJSON = "{}"
	}
	if !json.Valid([]byte(configJSON)) {
		err := errors.New("config_json must be valid JSON")
		result.Error = err.Error()
		return result, err
	}
	artifact, err := m.repo.Artifact(ctx, artifactID)
	if err != nil {
		result.Error = err.Error()
		return result, err
	}
	if artifact.PluginID != pluginID {
		err := errors.New("artifact plugin_id does not match")
		result.Error = err.Error()
		return result, err
	}
	if err := m.validateArtifactGate(artifact); err != nil {
		result.Error = err.Error()
		return result, err
	}
	var manifest Manifest
	if err := json.Unmarshal([]byte(artifact.MetadataJSON), &manifest); err != nil {
		result.Error = err.Error()
		return result, err
	}
	if err := validateConfigSchema(manifest.ConfigSchema, configJSON); err != nil {
		result.Error = err.Error()
		return result, err
	}
	if err := m.validateSecretRefs(ctx, manifest, configJSON); err != nil {
		result.Error = err.Error()
		return result, err
	}
	pluginRecord, err := m.pluginRecordForDryRun(ctx, pluginID, artifactID, configJSON)
	if err != nil {
		result.Error = err.Error()
		return result, err
	}
	if dryRunner, ok := m.adapter.(ConfigDryRunAdapter); ok {
		if err := dryRunner.DryRunConfig(ctx, artifact, pluginRecord); err != nil {
			result.Error = err.Error()
			return result, err
		}
	}
	currentConfig := "{}"
	if current, err := m.repo.Plugin(ctx, pluginID); err == nil {
		currentConfig = current.ConfigJSON
	}
	sensitivePaths := sensitiveConfigPaths(manifest.ConfigSchema, configJSON)
	redactedConfig, err := redactJSON(configJSON, sensitivePaths)
	if err != nil {
		result.Error = err.Error()
		return result, err
	}
	diff, err := redactedDiffJSON(currentConfig, configJSON, sensitivePaths)
	if err != nil {
		result.Error = err.Error()
		return result, err
	}
	result.OK = true
	result.RestartRequired = m.restartRequired(pluginID, artifactID)
	result.HotReload = !result.RestartRequired
	result.SensitivePaths = sensitivePaths
	result.RedactedConfigJSON = redactedConfig
	result.RedactedDiffJSON = diff
	return result, nil
}

func (m *Manager) RollbackArtifact(ctx context.Context, actor, pluginID, artifactID string) (PluginRecord, error) {
	current, err := m.repo.Plugin(ctx, pluginID)
	if err != nil {
		return PluginRecord{}, err
	}
	decision, err := m.EvaluateGovernance(ctx, pluginID, artifactID, GovernanceActionRollback, m.currentPolicyProfile(), current.ConfigJSON)
	if err == nil && !decision.OK {
		err = governanceBlockedError(decision)
	}
	if err != nil {
		_ = m.repo.RecordOperation(ctx, pluginID, artifactID, "artifact_rollback_gate", "failed", actor, err.Error(), map[string]any{
			"active_changed":      false,
			"governance_decision": decision,
		})
		return PluginRecord{}, err
	}
	if _, err := m.DryRunConfig(ctx, pluginID, artifactID, current.ConfigJSON); err != nil {
		_ = m.repo.RecordOperation(ctx, pluginID, artifactID, "artifact_rollback", "failed", actor, err.Error(), map[string]any{
			"active_changed": false,
		})
		return PluginRecord{}, err
	}
	plugin, err := m.repo.UpsertDesired(ctx, actor, pluginID, artifactID, current.DesiredState, current.ConfigJSON, current.Priority)
	if err != nil {
		_ = m.repo.RecordOperation(ctx, pluginID, artifactID, "artifact_rollback", "failed", actor, err.Error(), map[string]any{
			"active_changed": false,
		})
		return PluginRecord{}, err
	}
	_ = m.repo.RecordOperation(ctx, pluginID, artifactID, "artifact_rollback", "succeeded", actor, "artifact rollback desired state updated", map[string]any{
		"desired_generation":  plugin.DesiredGeneration,
		"active_changed":      false,
		"governance_decision": decision,
	})
	return plugin, nil
}

func (m *Manager) RollbackConfigSnapshot(ctx context.Context, actor string, snapshotID int64, fullDesired bool) (PluginRecord, error) {
	snapshot, err := m.repo.ConfigSnapshot(ctx, snapshotID)
	if err != nil {
		return PluginRecord{}, err
	}
	plugin, err := m.repo.Plugin(ctx, snapshot.PluginID)
	if err != nil {
		return PluginRecord{}, err
	}
	artifactID := plugin.DesiredArtifactID
	desiredState := plugin.DesiredState
	priority := plugin.Priority
	if fullDesired {
		artifactID = snapshot.ArtifactID
		desiredState = snapshot.DesiredState
		priority = snapshot.Priority
	}
	decision, err := m.EvaluateGovernance(ctx, snapshot.PluginID, artifactID, GovernanceActionRollback, m.currentPolicyProfile(), snapshot.ConfigJSON)
	if err == nil && !decision.OK {
		err = governanceBlockedError(decision)
	}
	if err != nil {
		_ = m.repo.RecordOperation(ctx, snapshot.PluginID, artifactID, "config_rollback_gate", "failed", actor, err.Error(), map[string]any{
			"snapshot_id":         snapshot.ID,
			"full_desired":        fullDesired,
			"active_changed":      false,
			"governance_decision": decision,
		})
		return PluginRecord{}, err
	}
	if _, err := m.DryRunConfig(ctx, snapshot.PluginID, artifactID, snapshot.ConfigJSON); err != nil {
		_ = m.repo.RecordOperation(ctx, snapshot.PluginID, artifactID, "config_rollback", "failed", actor, err.Error(), map[string]any{
			"snapshot_id":    snapshot.ID,
			"full_desired":   fullDesired,
			"active_changed": false,
		})
		return PluginRecord{}, err
	}
	next, err := m.repo.UpsertDesired(ctx, actor, snapshot.PluginID, artifactID, desiredState, snapshot.ConfigJSON, priority)
	if err != nil {
		_ = m.repo.RecordOperation(ctx, snapshot.PluginID, artifactID, "config_rollback", "failed", actor, err.Error(), map[string]any{
			"snapshot_id":    snapshot.ID,
			"full_desired":   fullDesired,
			"active_changed": false,
		})
		return PluginRecord{}, err
	}
	_ = m.repo.RecordOperation(ctx, snapshot.PluginID, artifactID, "config_rollback", "succeeded", actor, "config snapshot rollback desired state updated", map[string]any{
		"snapshot_id":         snapshot.ID,
		"full_desired":        fullDesired,
		"desired_generation":  next.DesiredGeneration,
		"active_changed":      false,
		"governance_decision": decision,
	})
	return next, nil
}

func (m *Manager) Load(ctx context.Context, actor, pluginID string) (PluginRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	pluginRecord, err := m.repo.Plugin(ctx, pluginID)
	if err != nil {
		return PluginRecord{}, err
	}
	loaded, err := m.loadLocked(ctx, pluginRecord)
	if err != nil {
		_ = m.repo.RecordOperation(ctx, pluginID, pluginRecord.DesiredArtifactID, "load", "failed", actor, err.Error(), nil)
		return PluginRecord{}, err
	}
	_ = m.repo.RecordOperation(ctx, pluginID, loaded.artifact.ID, "load", "succeeded", actor, "plugin loaded", nil)
	return m.repo.Plugin(ctx, pluginID)
}

func (m *Manager) Enable(ctx context.Context, actor, pluginID string) (PluginRecord, error) {
	pluginRecord, err := m.repo.Plugin(ctx, pluginID)
	if err != nil {
		return PluginRecord{}, err
	}
	if pluginRecord.DesiredState != DesiredEnabled {
		pluginRecord, err = m.SetDesired(ctx, actor, pluginRecord.ID, pluginRecord.DesiredArtifactID, DesiredEnabled, pluginRecord.ConfigJSON, pluginRecord.Priority)
		if err != nil {
			return PluginRecord{}, err
		}
	}
	decision, err := m.EvaluateGovernance(ctx, pluginID, pluginRecord.DesiredArtifactID, GovernanceActionEnable, m.currentPolicyProfile(), pluginRecord.ConfigJSON)
	if err == nil && !decision.OK {
		err = governanceBlockedError(decision)
	}
	if err != nil {
		_ = m.repo.MarkRuntime(ctx, pluginID, RuntimeFailed, "", "", pluginRecord.AppliedGeneration, err.Error(), map[string]any{"governance": decision}, nil)
		_ = m.repo.RecordOperation(ctx, pluginID, pluginRecord.DesiredArtifactID, "enable_gate", "failed", actor, err.Error(), map[string]any{
			"decision": decision,
		})
		return PluginRecord{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	loaded, err := m.loadLocked(ctx, pluginRecord)
	if err != nil {
		_ = m.repo.RecordOperation(ctx, pluginID, pluginRecord.DesiredArtifactID, "enable", "failed", actor, err.Error(), nil)
		return PluginRecord{}, err
	}
	if len(loaded.handlers) == 0 {
		err := fmt.Errorf("plugin %q did not register %s", pluginID, ExtensionUpstreamConnect)
		_ = m.repo.MarkRuntime(ctx, pluginID, RuntimeFailed, "", loaded.artifact.ID, pluginRecord.AppliedGeneration, err.Error(), map[string]any{"error": err.Error()}, nil)
		_ = m.repo.RecordOperation(ctx, pluginID, pluginRecord.DesiredArtifactID, "enable", "failed", actor, err.Error(), nil)
		return PluginRecord{}, err
	}

	current := m.currentHandlersLocked()
	current[pluginID] = loaded.handlers
	next := flattenHandlers(current)
	if err := m.markEnabled(ctx, loaded); err != nil {
		return PluginRecord{}, err
	}
	m.clearDrainingLocked(pluginID)
	m.publish(next)
	_ = m.repo.UpdateArtifactStatus(ctx, loaded.artifact.ID, ArtifactStatusLoaded, "")
	_ = m.repo.RecordOperation(ctx, pluginID, loaded.artifact.ID, "enable", "succeeded", actor, "plugin enabled", map[string]any{
		"desired_generation":  loaded.record.DesiredGeneration,
		"handler_count":       len(loaded.handlers),
		"governance_decision": decision,
	})
	return m.repo.Plugin(ctx, pluginID)
}

func (m *Manager) Disable(ctx context.Context, actor, pluginID string) (PluginRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	pluginRecord, err := m.repo.Plugin(ctx, pluginID)
	if err != nil {
		return PluginRecord{}, err
	}
	pluginRecord, err = m.repo.UpsertDesired(ctx, actor, pluginRecord.ID, pluginRecord.DesiredArtifactID, DesiredDisabled, pluginRecord.ConfigJSON, pluginRecord.Priority)
	if err != nil {
		return PluginRecord{}, err
	}
	m.removeFromDispatchLocked(pluginID)
	m.markDrainingLocked(pluginID)
	m.operations.StopPlugin(pluginID)
	if loaded := m.loaded[pluginID]; loaded != nil && loaded.instance != nil {
		if err := loaded.instance.Destroy(); err != nil {
			_ = m.repo.RecordOperation(ctx, pluginID, loaded.artifact.ID, "disable", "warning", actor, err.Error(), nil)
		}
	}
	runtimeState := RuntimeDisabled
	if m.activeProxyCountLocked(pluginID) > 0 {
		runtimeState = RuntimeDraining
	}
	delete(m.loaded, pluginID)
	if err := m.repo.MarkRuntime(ctx, pluginID, runtimeState, "", "", pluginRecord.DesiredGeneration, "", map[string]any{
		"active_proxy_connections": m.activeProxyCountLocked(pluginID),
	}, nil); err != nil {
		return PluginRecord{}, err
	}
	_ = m.repo.RecordOperation(ctx, pluginID, pluginRecord.DesiredArtifactID, "disable", "succeeded", actor, "plugin disabled", nil)
	return m.repo.Plugin(ctx, pluginID)
}

func (m *Manager) Delete(ctx context.Context, actor, pluginID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	pluginRecord, err := m.repo.Plugin(ctx, pluginID)
	if err != nil {
		return err
	}
	m.removeFromDispatchLocked(pluginID)
	m.markDrainingLocked(pluginID)
	m.operations.StopPlugin(pluginID)
	if loaded := m.loaded[pluginID]; loaded != nil && loaded.instance != nil {
		_ = loaded.instance.Destroy()
	}
	delete(m.loaded, pluginID)
	if _, err := m.repo.UpsertDesired(ctx, actor, pluginRecord.ID, pluginRecord.DesiredArtifactID, DesiredDeleted, pluginRecord.ConfigJSON, pluginRecord.Priority); err != nil {
		return err
	}
	_ = m.repo.RecordOperation(ctx, pluginID, pluginRecord.DesiredArtifactID, "delete", "succeeded", actor, "plugin deleted", map[string]any{
		"cleanup": "pending_restart_for_loaded_go_plugin",
	})
	return nil
}

func (m *Manager) Reconcile(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	desired, err := m.repo.DesiredEnabled(ctx)
	if err != nil {
		return err
	}
	nextByPlugin := make(map[string][]*upstreamHandler)
	for _, pluginRecord := range desired {
		decision, err := m.EvaluateGovernance(ctx, pluginRecord.ID, pluginRecord.DesiredArtifactID, GovernanceActionEnable, m.currentPolicyProfile(), pluginRecord.ConfigJSON)
		if err == nil && !decision.OK {
			err = governanceBlockedError(decision)
		}
		if err != nil {
			_ = m.repo.MarkRuntime(ctx, pluginRecord.ID, RuntimeFailed, "", "", pluginRecord.AppliedGeneration, err.Error(), map[string]any{"governance": decision}, nil)
			_ = m.repo.RecordOperation(ctx, pluginRecord.ID, pluginRecord.DesiredArtifactID, "reconcile_gate", "failed", "system", err.Error(), map[string]any{"decision": decision})
			continue
		}
		loaded, err := m.loadLocked(ctx, pluginRecord)
		if err != nil {
			_ = m.repo.MarkRuntime(ctx, pluginRecord.ID, RuntimeFailed, "", "", pluginRecord.AppliedGeneration, err.Error(), map[string]any{"error": err.Error()}, nil)
			_ = m.repo.RecordOperation(ctx, pluginRecord.ID, pluginRecord.DesiredArtifactID, "reconcile", "failed", "system", err.Error(), nil)
			continue
		}
		if len(loaded.handlers) == 0 {
			err := fmt.Errorf("plugin %q did not register %s", pluginRecord.ID, ExtensionUpstreamConnect)
			_ = m.repo.MarkRuntime(ctx, pluginRecord.ID, RuntimeFailed, "", loaded.artifact.ID, pluginRecord.AppliedGeneration, err.Error(), map[string]any{"error": err.Error()}, nil)
			_ = m.repo.RecordOperation(ctx, pluginRecord.ID, pluginRecord.DesiredArtifactID, "reconcile", "failed", "system", err.Error(), nil)
			continue
		}
		nextByPlugin[pluginRecord.ID] = loaded.handlers
		_ = m.markEnabled(ctx, loaded)
		m.clearDrainingLocked(pluginRecord.ID)
	}
	m.publish(flattenHandlers(nextByPlugin))
	return nil
}

func (m *Manager) ConnectUpstream(ctx context.Context, req api.UpstreamConnectRequest) (UpstreamResult, error) {
	value := m.snapshot.Load()
	if value == nil {
		return UpstreamResult{}, nil
	}
	handlers, ok := value.([]*upstreamHandler)
	if !ok {
		return UpstreamResult{}, nil
	}
	if req.Context == nil {
		req.Context = ctx
	}
	req.InitialData = append([]byte(nil), req.InitialData...)
	for _, handler := range handlers {
		accepted, err := handler.accepts(req)
		if err != nil {
			return UpstreamResult{Handled: true}, err
		}
		if !accepted {
			continue
		}
		req.Context = WithTraceContext(req.Context, handler.pluginID, req.TraceID, req.ConnectionID, handler.handlerID)
		start := time.Now()
		conn, err := handler.invoke(req)
		status := "ok"
		if err != nil {
			status = "error"
		}
		_ = m.repo.SaveTrace(context.Background(), TraceSummary{
			PluginID:     handler.pluginID,
			TraceID:      req.TraceID,
			ConnectionID: req.ConnectionID,
			HandlerID:    handler.handlerID,
			Operation:    "plugin.handler." + handler.handlerID,
			Status:       status,
			DurationMS:   time.Since(start).Milliseconds(),
		}, map[string]string{
			"host":     req.ServerHost,
			"upstream": req.UpstreamAddress,
			"mode":     handler.mode,
		})
		if errors.Is(err, api.ErrPass) {
			continue
		}
		if err != nil {
			return UpstreamResult{Handled: true}, err
		}
		if conn != nil {
			if handler.mode == UpstreamModeDialer {
				_ = m.repo.SaveTrace(context.Background(), TraceSummary{
					PluginID:     handler.pluginID,
					TraceID:      req.TraceID,
					ConnectionID: req.ConnectionID,
					HandlerID:    handler.handlerID,
					Operation:    "backend.dial",
					Status:       "plugin_supplied",
					DurationMS:   0,
				}, map[string]string{"upstream": req.UpstreamAddress})
			}
			result := UpstreamResult{
				Conn:      conn,
				Handled:   true,
				Mode:      handler.mode,
				PluginID:  handler.pluginID,
				HandlerID: handler.handlerID,
			}
			if handler.mode == UpstreamModeProtocolProxy {
				return m.startProtocolProxy(ctx, handler, result, req)
			}
			return result, nil
		}
	}
	return UpstreamResult{}, nil
}

func (m *Manager) startProtocolProxy(ctx context.Context, handler *upstreamHandler, result UpstreamResult, req api.UpstreamConnectRequest) (UpstreamResult, error) {
	endpoint := result.Conn
	initial := append([]byte(nil), req.InitialData...)
	if len(initial) > 0 {
		if handler.initialWriteTimeout > 0 {
			_ = endpoint.SetWriteDeadline(time.Now().Add(handler.initialWriteTimeout))
			defer endpoint.SetWriteDeadline(time.Time{})
		}
		if err := writeAll(endpoint, initial); err != nil {
			handler.proxyErrors.Add(1)
			_ = endpoint.Close()
			return UpstreamResult{Handled: true, Mode: handler.mode, PluginID: handler.pluginID, HandlerID: handler.handlerID}, fmt.Errorf("plugin %s protocol-proxy initial replay failed: %w", handler.pluginID, err)
		}
	}

	handle := m.TrackProxyConnection(result, req.Source, endpoint)
	if handle == nil {
		_ = endpoint.Close()
		return UpstreamResult{Handled: true, Mode: handler.mode, PluginID: handler.pluginID, HandlerID: handler.handlerID}, fmt.Errorf("plugin %s protocol-proxy tracking failed", handler.pluginID)
	}
	runProtocolProxy(ctx, handle, req.Source, endpoint)

	return UpstreamResult{
		Handled:         true,
		Mode:            handler.mode,
		PluginID:        handler.pluginID,
		HandlerID:       handler.handlerID,
		InitialDataSent: len(initial) > 0,
		Proxied:         true,
	}, nil
}

func (h *upstreamHandler) accepts(req api.UpstreamConnectRequest) (accepted bool, err error) {
	if h.accept == nil {
		return true, nil
	}
	defer func() {
		if rec := recover(); rec != nil {
			h.panics.Add(1)
			accepted = false
			err = fmt.Errorf("plugin %s acceptor panic: %v", h.pluginID, rec)
		}
	}()
	return h.accept(req), nil
}

func (m *Manager) ListArtifacts(ctx context.Context, pluginID string) ([]ArtifactRecord, error) {
	return m.repo.ListArtifacts(ctx, pluginID)
}

func (m *Manager) ListBuilds(ctx context.Context, pluginID string) ([]BuildRecord, error) {
	return m.repo.ListBuilds(ctx, pluginID)
}

func (m *Manager) Build(ctx context.Context, id int64) (BuildRecord, error) {
	return m.repo.Build(ctx, id)
}

func (m *Manager) ListConfigSnapshots(ctx context.Context, pluginID string) ([]ConfigSnapshotRecord, error) {
	return m.repo.ListConfigSnapshots(ctx, pluginID)
}

func (m *Manager) ConfigSnapshot(ctx context.Context, id int64) (ConfigSnapshotRecord, error) {
	return m.repo.ConfigSnapshot(ctx, id)
}

func (m *Manager) ConfigSnapshotDiff(ctx context.Context, snapshotID int64) (ConfigSnapshotDiff, error) {
	snapshot, err := m.repo.ConfigSnapshot(ctx, snapshotID)
	if err != nil {
		return ConfigSnapshotDiff{}, err
	}
	plugin, err := m.repo.Plugin(ctx, snapshot.PluginID)
	if err != nil {
		return ConfigSnapshotDiff{}, err
	}
	artifactID := snapshot.ArtifactID
	if artifactID == "" {
		artifactID = plugin.DesiredArtifactID
	}
	artifact, err := m.repo.Artifact(ctx, artifactID)
	if err != nil {
		return ConfigSnapshotDiff{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal([]byte(artifact.MetadataJSON), &manifest); err != nil {
		return ConfigSnapshotDiff{}, err
	}
	paths := sensitiveConfigPaths(manifest.ConfigSchema, snapshot.ConfigJSON)
	diff, err := redactedDiffJSON(plugin.ConfigJSON, snapshot.ConfigJSON, paths)
	if err != nil {
		return ConfigSnapshotDiff{}, err
	}
	return ConfigSnapshotDiff{
		SnapshotID:         snapshot.ID,
		PluginID:           snapshot.PluginID,
		ArtifactID:         artifactID,
		SensitivePaths:     paths,
		RedactedDiffJSON:   diff,
		RestartRequired:    m.restartRequired(snapshot.PluginID, artifactID),
		CurrentGeneration:  plugin.DesiredGeneration,
		SnapshotGeneration: snapshot.DesiredGeneration,
	}, nil
}

func (m *Manager) ListSecrets(ctx context.Context, pluginID string) ([]SecretRecord, error) {
	return m.repo.ListSecrets(ctx, pluginID)
}

func (m *Manager) quarantineAffected(ctx context.Context, advisory AdvisoryRecord) {
	plugins, err := m.repo.ListPlugins(ctx)
	if err != nil {
		return
	}
	var manifests = make(map[string]Manifest)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, plugin := range plugins {
		if plugin.RuntimeState != RuntimeEnabled || plugin.ActiveArtifactID == "" {
			continue
		}
		artifact, err := m.repo.Artifact(ctx, plugin.ActiveArtifactID)
		if err != nil {
			continue
		}
		manifest := manifests[artifact.ID]
		if manifest.ID == "" {
			_ = json.Unmarshal([]byte(artifact.MetadataJSON), &manifest)
			manifests[artifact.ID] = manifest
		}
		if advisoryMatches(advisory, artifact, manifest) {
			m.removeFromDispatchLocked(plugin.ID)
			m.markDrainingLocked(plugin.ID)
			_ = m.repo.MarkRuntime(ctx, plugin.ID, RuntimeDraining, artifact.ID, artifact.ID, plugin.AppliedGeneration, "plugin quarantined by advisory "+advisory.AdvisoryID, map[string]any{
				"quarantine":  true,
				"advisory_id": advisory.AdvisoryID,
			}, nil)
		}
	}
}

func (m *Manager) UpsertSecret(ctx context.Context, actor, pluginID, artifactID, name, value string, reloadRequired, hotReload bool) (SecretRecord, error) {
	if artifactID == "" {
		plugin, err := m.repo.Plugin(ctx, pluginID)
		if err != nil {
			return SecretRecord{}, err
		}
		artifactID = plugin.DesiredArtifactID
	}
	artifact, err := m.repo.Artifact(ctx, artifactID)
	if err != nil {
		return SecretRecord{}, err
	}
	if artifact.PluginID != pluginID {
		return SecretRecord{}, errors.New("artifact plugin_id does not match")
	}
	var manifest Manifest
	if err := json.Unmarshal([]byte(artifact.MetadataJSON), &manifest); err != nil {
		return SecretRecord{}, err
	}
	if len(manifest.Secrets) > 0 {
		declared := false
		for _, spec := range manifest.Secrets {
			if spec.Name == name {
				declared = true
				if !reloadRequired && !hotReload {
					switch spec.Rotation.Reload {
					case "hot":
						hotReload = true
					case "reload_required", "restart_required", "manual":
						reloadRequired = true
					}
				}
				break
			}
		}
		if !declared {
			return SecretRecord{}, fmt.Errorf("secret %q is not declared by manifest", name)
		}
	}
	secret, err := m.repo.UpsertSecret(ctx, actor, pluginID, name, value, reloadRequired, hotReload)
	if err != nil {
		_ = m.repo.RecordOperation(ctx, pluginID, "", "secret_update", "failed", actor, "secret update failed", map[string]any{
			"secret_ref": "plugin://" + pluginID + "/" + name,
			"error":      redactSecretText(err.Error()),
		})
		return SecretRecord{}, err
	}
	_ = m.repo.RecordOperation(ctx, pluginID, "", "secret_update", "succeeded", actor, "secret updated", map[string]any{
		"secret_ref":       "plugin://" + pluginID + "/" + name,
		"current_version":  secret.CurrentVersion,
		"previous_version": secret.PreviousVersion,
		"reload_required":  secret.ReloadRequired,
		"hot_reload":       secret.HotReload,
	})
	return secret, nil
}

func (m *Manager) ActiveProxyConnections(ctx context.Context, pluginID string) ([]ProxyConnectionSummary, error) {
	_ = ctx
	now := time.Now()
	var summaries []ProxyConnectionSummary
	m.proxyMu.Lock()
	defer m.proxyMu.Unlock()
	for _, conn := range m.proxyConns {
		if pluginID != "" && conn.pluginID != pluginID {
			continue
		}
		summaries = append(summaries, ProxyConnectionSummary{
			ID:         conn.id,
			PluginID:   conn.pluginID,
			ArtifactID: conn.artifactID,
			HandlerID:  conn.handlerID,
			StartedAt:  conn.startedAt.Unix(),
			DurationMS: now.Sub(conn.startedAt).Milliseconds(),
			Draining:   conn.draining,
		})
	}
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].StartedAt < summaries[j].StartedAt
	})
	return summaries, nil
}

func (m *Manager) CancelBuild(ctx context.Context, actor string, id int64) (BuildRecord, error) {
	build, err := m.repo.CancelBuild(ctx, id, actor)
	if err != nil {
		return BuildRecord{}, err
	}
	_ = m.repo.RecordOperation(ctx, build.PluginID, build.SourceID, "source_build_cancel", "succeeded", actor, "build canceled", map[string]any{"build_id": id})
	return build, nil
}

func (m *Manager) RetryBuild(ctx context.Context, actor string, id int64) (BuildRecord, error) {
	build, err := m.repo.Build(ctx, id)
	if err != nil {
		return BuildRecord{}, err
	}
	if build.Status != BuildStatusFailed && build.Status != BuildStatusCanceled {
		return BuildRecord{}, fmt.Errorf("build status %q cannot be retried", build.Status)
	}
	next, err := m.CreateBuild(ctx, actor, BuildRequest{
		SourceID:       build.SourceID,
		BuilderType:    build.BuilderType,
		BuilderImage:   build.BuilderImage,
		BuilderVersion: build.BuilderVersion,
		GOOS:           build.GOOS,
		GOARCH:         build.GOARCH,
		GOAMD64:        build.GOAMD64,
		GOARM64:        build.GOARM64,
		CGOEnabled:     build.CGOEnabled,
		BuildTags:      build.BuildTags,
		SDKModule:      build.SDKModule,
		SDKVersion:     build.SDKVersion,
		GOPROXY:        build.GOPROXY,
		GONOSUMDB:      build.GONOSUMDB,
		GOPRIVATE:      build.GOPRIVATE,
		VendorRequired: build.VendorRequired,
	})
	if err != nil {
		return BuildRecord{}, err
	}
	return m.RunBuild(ctx, actor, next.ID)
}

func (m *Manager) Artifact(ctx context.Context, id string) (ArtifactRecord, error) {
	return m.repo.Artifact(ctx, id)
}

func (m *Manager) ListPlugins(ctx context.Context) ([]PluginRecord, error) {
	return m.repo.ListPlugins(ctx)
}

func (m *Manager) Plugin(ctx context.Context, id string) (PluginRecord, error) {
	return m.repo.Plugin(ctx, id)
}

func (m *Manager) DispatchPlan(ctx context.Context) DispatchPlan {
	value := m.snapshot.Load()
	plan := DispatchPlan{UpdatedAt: time.Now().Unix()}
	if handlers, ok := value.([]*upstreamHandler); ok {
		plan.Handlers = handlerSummaries(handlers)
	}
	return plan
}

func (m *Manager) OperationsSnapshot(ctx context.Context, pluginID string) (OperationsSnapshot, error) {
	if pluginID != "" {
		if _, err := m.repo.Plugin(ctx, pluginID); err != nil {
			return OperationsSnapshot{}, err
		}
	}
	plan := m.DispatchPlan(ctx)
	var handlers []DispatchHandlerSummary
	for _, handler := range plan.Handlers {
		if pluginID == "" || handler.PluginID == pluginID {
			handlers = append(handlers, handler)
		}
	}
	builds, err := m.repo.ListBuilds(ctx, pluginID)
	if err != nil {
		return OperationsSnapshot{}, err
	}
	gc, _ := m.operations.GCCandidates(ctx, pluginID)
	return m.operations.Snapshot(ctx, pluginID, handlers, builds, gc), nil
}

func (m *Manager) TriggerBackgroundTask(ctx context.Context, actor, pluginID, taskID, confirmToken string) (BackgroundTaskSummary, error) {
	summary, err := m.operations.TriggerTask(pluginID, taskID, confirmToken)
	if err != nil {
		_ = m.repo.RecordOperation(ctx, pluginID, "", "background_task_trigger", "failed", actor, err.Error(), map[string]any{"task_id": taskID})
		return BackgroundTaskSummary{}, err
	}
	_ = m.repo.RecordOperation(ctx, pluginID, "", "background_task_trigger", "succeeded", actor, "background task triggered", map[string]any{"task_id": taskID})
	return summary, nil
}

func (m *Manager) DiagnosticPackage(ctx context.Context, actor, pluginID string) ([]byte, DiagnosticPackageSummary, error) {
	plugin, err := m.repo.Plugin(ctx, pluginID)
	if err != nil {
		return nil, DiagnosticPackageSummary{}, err
	}
	artifact, err := m.repo.Artifact(ctx, plugin.DesiredArtifactID)
	if err != nil {
		return nil, DiagnosticPackageSummary{}, err
	}
	var manifest Manifest
	_ = json.Unmarshal([]byte(artifact.MetadataJSON), &manifest)
	plan := m.DispatchPlan(ctx)
	var handlers []DispatchHandlerSummary
	for _, handler := range plan.Handlers {
		if handler.PluginID == pluginID {
			handlers = append(handlers, handler)
		}
	}
	builds, _ := m.repo.ListBuilds(ctx, pluginID)
	gc, _ := m.operations.GCCandidates(ctx, pluginID)
	data, summary, err := m.operations.DiagnosticPackage(ctx, plugin, manifest, handlers, builds, gc)
	if err != nil {
		_ = m.repo.RecordOperation(ctx, pluginID, artifact.ID, "diagnostic_package", "failed", actor, err.Error(), nil)
		return nil, DiagnosticPackageSummary{}, err
	}
	_ = m.repo.RecordOperation(ctx, pluginID, artifact.ID, "diagnostic_package", "succeeded", actor, "diagnostic package generated", map[string]any{
		"size_bytes": summary.SizeBytes,
		"sections":   summary.Sections,
	})
	return data, summary, nil
}

func (m *Manager) RunOperationsGC(ctx context.Context, actor, pluginID string, dryRun bool) ([]GCCandidate, error) {
	return m.operations.RunGC(ctx, actor, pluginID, dryRun)
}

func (m *Manager) TrackProxyConnection(result UpstreamResult, client, endpoint net.Conn) *ProxyConnectionHandle {
	if result.Mode != UpstreamModeProtocolProxy || client == nil || endpoint == nil {
		return nil
	}
	handler := m.findHandler(result.PluginID, result.HandlerID)
	if handler == nil {
		return nil
	}
	id := atomic.AddUint64(&m.proxySeq, 1)
	proxyConn := &proxyConnection{
		id:         id,
		pluginID:   result.PluginID,
		artifactID: handler.artifactID,
		handlerID:  result.HandlerID,
		handler:    handler,
		client:     client,
		endpoint:   endpoint,
		startedAt:  time.Now(),
	}
	handler.activeProxy.Add(1)
	handler.proxyStarted.Add(1)
	m.proxyMu.Lock()
	proxyConn.draining = m.drainingIDs[result.PluginID]
	m.proxyConns[id] = proxyConn
	m.proxyMu.Unlock()
	return &ProxyConnectionHandle{manager: m, id: id}
}

func (h *ProxyConnectionHandle) Finish(stats ProxyConnectionStats) {
	if h == nil || h.manager == nil {
		return
	}
	h.manager.finishProxyConnection(h.id, stats)
}

func (m *Manager) ForceCloseDraining(ctx context.Context, actor, pluginID string) (int, error) {
	_ = ctx
	var conns []*proxyConnection
	m.proxyMu.Lock()
	for _, conn := range m.proxyConns {
		if conn.pluginID == pluginID && conn.draining {
			conns = append(conns, conn)
		}
	}
	m.proxyMu.Unlock()
	for _, conn := range conns {
		_ = conn.client.Close()
		_ = conn.endpoint.Close()
	}
	_ = m.repo.RecordOperation(ctx, pluginID, "", "force_close_draining", "succeeded", actor, "draining protocol-proxy connections force closed", map[string]any{
		"closed": len(conns),
	})
	return len(conns), nil
}

func (m *Manager) findHandler(pluginID, handlerID string) *upstreamHandler {
	value := m.snapshot.Load()
	if handlers, ok := value.([]*upstreamHandler); ok {
		for _, handler := range handlers {
			if handler.pluginID == pluginID && handler.handlerID == handlerID {
				return handler
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if loaded := m.loaded[pluginID]; loaded != nil {
		for _, handler := range loaded.handlers {
			if handler.handlerID == handlerID {
				return handler
			}
		}
	}
	return nil
}

func (m *Manager) finishProxyConnection(id uint64, stats ProxyConnectionStats) {
	m.proxyMu.Lock()
	proxyConn := m.proxyConns[id]
	delete(m.proxyConns, id)
	m.proxyMu.Unlock()
	if proxyConn == nil || proxyConn.handler == nil {
		return
	}
	proxyConn.handler.activeProxy.Add(-1)
	proxyConn.handler.proxyCompleted.Add(1)
	if stats.Err != nil {
		proxyConn.handler.proxyErrors.Add(1)
	}
	if stats.BytesToPlugin > 0 {
		proxyConn.handler.proxyBytesIn.Add(uint64(stats.BytesToPlugin))
	}
	if stats.BytesToClient > 0 {
		proxyConn.handler.proxyBytesOut.Add(uint64(stats.BytesToClient))
	}
	if stats.Duration > 0 {
		proxyConn.handler.proxyDuration.Add(uint64(stats.Duration.Milliseconds()))
	}
}

func (m *Manager) loadLocked(ctx context.Context, pluginRecord PluginRecord) (*loadedPlugin, error) {
	if loaded := m.loaded[pluginRecord.ID]; loaded != nil &&
		loaded.artifact.ID == pluginRecord.DesiredArtifactID &&
		loaded.record.DesiredGeneration == pluginRecord.DesiredGeneration {
		return loaded, nil
	}
	artifact, err := m.repo.Artifact(ctx, pluginRecord.DesiredArtifactID)
	if err != nil {
		return nil, err
	}
	if err := m.validateArtifactGate(artifact); err != nil {
		return nil, err
	}

	var manifest Manifest
	if err := json.Unmarshal([]byte(artifact.MetadataJSON), &manifest); err != nil {
		return nil, err
	}
	gateway := NewGateway(pluginRecord.ID, m.handleConn, m.wg, m.operations.ForPlugin(pluginRecord.ID, artifact.ID, manifest))
	instance, err := m.adapter.Load(ctx, artifact, pluginRecord, gateway)
	if err != nil {
		_ = m.repo.MarkRuntime(ctx, pluginRecord.ID, RuntimeFailed, "", "", pluginRecord.AppliedGeneration, err.Error(), map[string]any{"error": err.Error()}, nil)
		return nil, err
	}
	handlers := buildHandlers(pluginRecord, artifact, gateway)
	loaded := &loadedPlugin{
		record:   pluginRecord,
		artifact: artifact,
		instance: instance,
		gateway:  gateway,
		handlers: handlers,
	}
	m.loaded[pluginRecord.ID] = loaded
	if err := m.repo.MarkRuntime(ctx, pluginRecord.ID, RuntimeLoaded, "", artifact.ID, pluginRecord.AppliedGeneration, "", map[string]any{
		"handler_count": len(handlers),
	}, handlerSummaries(handlers)); err != nil {
		return nil, err
	}
	m.operations.StartTasks(pluginRecord.ID)
	return loaded, nil
}

func (m *Manager) validateArtifactGate(artifact ArtifactRecord) error {
	if artifact.Status == ArtifactStatusDeleted || artifact.Status == ArtifactStatusRejected {
		return fmt.Errorf("artifact status %q is not loadable", artifact.Status)
	}
	if artifact.ArtifactType != ArtifactTypeBinary {
		return errors.New("desired artifact must be a binary artifact")
	}
	if artifact.GoVersion != runtime.Version() {
		return fmt.Errorf("artifact go_version %q does not match gateway %q", artifact.GoVersion, runtime.Version())
	}
	if artifact.GOOS != runtime.GOOS || artifact.GOARCH != runtime.GOARCH {
		return fmt.Errorf("artifact target %s/%s does not match gateway %s/%s", artifact.GOOS, artifact.GOARCH, runtime.GOOS, runtime.GOARCH)
	}
	return nil
}

func (m *Manager) pluginRecordForDryRun(ctx context.Context, pluginID, artifactID, configJSON string) (PluginRecord, error) {
	pluginRecord, err := m.repo.Plugin(ctx, pluginID)
	if err != nil {
		if !errors.Is(err, ErrPluginNotFound) {
			return PluginRecord{}, err
		}
		return PluginRecord{
			ID:                pluginID,
			DesiredArtifactID: artifactID,
			DesiredState:      DesiredDisabled,
			RuntimeState:      RuntimeDisabled,
			Priority:          DefaultPriority,
			ConfigJSON:        configJSON,
			DesiredGeneration: 1,
		}, nil
	}
	pluginRecord.DesiredArtifactID = artifactID
	pluginRecord.ConfigJSON = configJSON
	return pluginRecord, nil
}

func (m *Manager) restartRequired(pluginID, artifactID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	loaded := m.loaded[pluginID]
	return loaded != nil && loaded.artifact.ID != artifactID
}

func (m *Manager) markEnabled(ctx context.Context, loaded *loadedPlugin) error {
	m.operations.StartTasks(loaded.record.ID)
	return m.repo.MarkRuntime(ctx, loaded.record.ID, RuntimeEnabled, loaded.artifact.ID, loaded.artifact.ID, loaded.record.DesiredGeneration, "", map[string]any{
		"handler_count": len(loaded.handlers),
	}, handlerSummaries(loaded.handlers))
}

func (m *Manager) currentHandlersLocked() map[string][]*upstreamHandler {
	current := make(map[string][]*upstreamHandler)
	value := m.snapshot.Load()
	if handlers, ok := value.([]*upstreamHandler); ok {
		for _, handler := range handlers {
			current[handler.pluginID] = append(current[handler.pluginID], handler)
		}
	}
	return current
}

func (m *Manager) removeFromDispatchLocked(pluginID string) {
	current := m.currentHandlersLocked()
	delete(current, pluginID)
	m.publish(flattenHandlers(current))
}

func (m *Manager) markDrainingLocked(pluginID string) {
	m.proxyMu.Lock()
	defer m.proxyMu.Unlock()
	m.drainingIDs[pluginID] = true
	for _, conn := range m.proxyConns {
		if conn.pluginID == pluginID {
			conn.draining = true
		}
	}
}

func (m *Manager) clearDrainingLocked(pluginID string) {
	m.proxyMu.Lock()
	delete(m.drainingIDs, pluginID)
	m.proxyMu.Unlock()
}

func (m *Manager) activeProxyCountLocked(pluginID string) int {
	m.proxyMu.Lock()
	defer m.proxyMu.Unlock()
	count := 0
	for _, conn := range m.proxyConns {
		if conn.pluginID == pluginID {
			count++
		}
	}
	return count
}

func (m *Manager) publish(handlers []*upstreamHandler) {
	sort.SliceStable(handlers, func(i, j int) bool {
		if handlers[i].priority != handlers[j].priority {
			return handlers[i].priority < handlers[j].priority
		}
		if handlers[i].pluginID != handlers[j].pluginID {
			return handlers[i].pluginID < handlers[j].pluginID
		}
		return handlers[i].handlerID < handlers[j].handlerID
	})
	m.snapshot.Store(handlers)
}

func buildHandlers(pluginRecord PluginRecord, artifact ArtifactRecord, gateway *Gateway) []*upstreamHandler {
	timeout := DefaultHandlerTimeout
	initialWriteTimeout := DefaultInitialWriteTimeout
	var manifest Manifest
	if err := json.Unmarshal([]byte(artifact.MetadataJSON), &manifest); err == nil {
		if manifest.RuntimeLimits.HandlerTimeoutMS > 0 {
			timeout = time.Duration(manifest.RuntimeLimits.HandlerTimeoutMS) * time.Millisecond
		}
		if manifest.RuntimeLimits.InitialWriteTimeoutMS > 0 {
			initialWriteTimeout = time.Duration(manifest.RuntimeLimits.InitialWriteTimeoutMS) * time.Millisecond
		}
	}
	var handlers []*upstreamHandler
	mode := upstreamModeFromArtifact(artifact)
	if hook, ok := gateway.UpstreamConnectHandler(); ok {
		handlers = append(handlers, &upstreamHandler{
			pluginID:            pluginRecord.ID,
			artifactID:          artifact.ID,
			priority:            pluginRecord.Priority,
			handlerID:           "upstream.connect/v1",
			mode:                mode,
			timeout:             timeout,
			initialWriteTimeout: initialWriteTimeout,
			accept:              hook.Acceptor(),
			handle:              hook.Handler(),
		})
	}
	if hook, ok := gateway.LegacyUpstreamHandler(); ok {
		acceptor := hook.Acceptor()
		handler := hook.Handler()
		handlers = append(handlers, &upstreamHandler{
			pluginID:            pluginRecord.ID,
			artifactID:          artifact.ID,
			priority:            pluginRecord.Priority,
			handlerID:           "legacy-upstream",
			mode:                UpstreamModeDialer,
			timeout:             timeout,
			initialWriteTimeout: initialWriteTimeout,
			accept: func(req api.UpstreamConnectRequest) bool {
				return acceptor(req.Source, req.Upstream)
			},
			handle: func(req api.UpstreamConnectRequest) (net.Conn, error) {
				return handler(req.Source, req.Upstream)
			},
		})
	}
	return handlers
}

func upstreamModeFromArtifact(artifact ArtifactRecord) string {
	var summary CapabilitySummary
	if err := json.Unmarshal([]byte(artifact.CapabilitiesSummaryJSON), &summary); err == nil {
		switch summary.UpstreamConnect.Mode {
		case UpstreamModeProtocolProxy:
			return UpstreamModeProtocolProxy
		case UpstreamModeDialer:
			return UpstreamModeDialer
		}
	}
	return UpstreamModeDialer
}

func (h *upstreamHandler) invoke(req api.UpstreamConnectRequest) (conn net.Conn, err error) {
	h.calls.Add(1)
	start := time.Now()
	defer func() {
		durationMS := uint64(time.Since(start).Milliseconds())
		h.durationCount.Add(1)
		h.durationSumMS.Add(durationMS)
		for {
			current := h.durationMaxMS.Load()
			if durationMS <= current || h.durationMaxMS.CompareAndSwap(current, durationMS) {
				break
			}
		}
	}()
	ctx := req.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if h.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.timeout)
		defer cancel()
	}
	req.Context = ctx

	done := make(chan result, 1)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				h.panics.Add(1)
				done <- result{err: fmt.Errorf("plugin %s panic: %v", h.pluginID, rec)}
			}
		}()
		conn, err := h.handle(req)
		done <- result{conn: conn, err: err}
	}()

	select {
	case <-ctx.Done():
		h.timeouts.Add(1)
		go closeLateConn(done)
		return nil, ctx.Err()
	case result := <-done:
		if errors.Is(result.err, api.ErrBlocked) {
			h.blocked.Add(1)
		}
		if result.err != nil && !errors.Is(result.err, api.ErrPass) {
			h.errors.Add(1)
		}
		return result.conn, result.err
	}
}

type result struct {
	conn net.Conn
	err  error
}

func closeLateConn(done <-chan result) {
	result := <-done
	if result.conn != nil {
		_ = result.conn.Close()
	}
}

type proxyCopyResult struct {
	toPlugin bool
	bytes    int64
	err      error
}

type closeWriter interface {
	CloseWrite() error
}

type closeReader interface {
	CloseRead() error
}

func runProtocolProxy(ctx context.Context, handle *ProxyConnectionHandle, client, endpoint net.Conn) {
	start := time.Now()
	defer client.Close()
	defer endpoint.Close()
	done := make(chan proxyCopyResult, 2)
	stopContext := make(chan struct{})
	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				_ = client.Close()
				_ = endpoint.Close()
			case <-stopContext:
			}
		}()
	}

	go copyProtocolProxy(endpoint, client, true, done)
	go copyProtocolProxy(client, endpoint, false, done)

	var stats ProxyConnectionStats
	for i := 0; i < 2; i++ {
		result := <-done
		if result.toPlugin {
			stats.BytesToPlugin += result.bytes
		} else {
			stats.BytesToClient += result.bytes
		}
		if result.err != nil && !errors.Is(result.err, io.EOF) && stats.Err == nil {
			stats.Err = result.err
		}
	}
	close(stopContext)
	stats.Duration = time.Since(start)
	handle.Finish(stats)
}

func copyProtocolProxy(dst io.Writer, src io.Reader, toPlugin bool, done chan<- proxyCopyResult) {
	result := proxyCopyResult{toPlugin: toPlugin}
	defer func() {
		if rec := recover(); rec != nil {
			result.err = fmt.Errorf("protocol-proxy copy panic: %v", rec)
		}
		closeRead(src)
		if toPlugin {
			closeWriteOnly(dst)
		} else {
			closeWrite(dst)
		}
		done <- result
	}()
	result.bytes, result.err = copyForward(dst, src)
}

func copyForward(dst io.Writer, src io.Reader) (int64, error) {
	return io.Copy(dst, src)
}

func writeAll(w io.Writer, buf []byte) error {
	for len(buf) > 0 {
		n, err := w.Write(buf)
		if n > 0 {
			buf = buf[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func closeWrite(conn any) {
	if closer, ok := conn.(closeWriter); ok {
		_ = closer.CloseWrite()
		return
	}
	if closer, ok := conn.(io.Closer); ok {
		_ = closer.Close()
	}
}

func closeWriteOnly(conn any) {
	if closer, ok := conn.(closeWriter); ok {
		_ = closer.CloseWrite()
	}
}

func closeRead(conn any) {
	if closer, ok := conn.(closeReader); ok {
		_ = closer.CloseRead()
	}
}

func validateConfigSchema(schema json.RawMessage, configJSON string) error {
	if len(strings.TrimSpace(string(schema))) == 0 || string(schema) == "null" {
		return nil
	}
	var root map[string]any
	if err := json.Unmarshal(schema, &root); err != nil {
		return fmt.Errorf("invalid config_schema: %w", err)
	}
	var config any
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		return err
	}
	return validateSchemaValue(root, config, "$")
}

func validateSchemaValue(schema map[string]any, value any, path string) error {
	if typ, _ := schema["type"].(string); typ != "" {
		if !jsonTypeMatches(typ, value) {
			return fmt.Errorf("%s must be %s", path, typ)
		}
	}
	if enumValues, ok := schema["enum"].([]any); ok && len(enumValues) > 0 {
		found := false
		for _, allowed := range enumValues {
			if reflect.DeepEqual(allowed, value) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%s must match enum", path)
		}
	}
	props, _ := schema["properties"].(map[string]any)
	obj, _ := value.(map[string]any)
	if required, ok := schema["required"].([]any); ok {
		for _, raw := range required {
			name, _ := raw.(string)
			if name == "" {
				continue
			}
			if obj == nil {
				return fmt.Errorf("%s must be object for required %q", path, name)
			}
			if _, exists := obj[name]; !exists {
				return fmt.Errorf("%s.%s is required", path, name)
			}
		}
	}
	if obj == nil || len(props) == 0 {
		return nil
	}
	for name, propSchema := range props {
		childSchema, ok := propSchema.(map[string]any)
		if !ok {
			continue
		}
		childValue, exists := obj[name]
		if !exists {
			continue
		}
		if err := validateSchemaValue(childSchema, childValue, path+"."+name); err != nil {
			return err
		}
	}
	return nil
}

func jsonTypeMatches(typ string, value any) bool {
	switch typ {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		_, ok := value.(float64)
		return ok
	case "integer":
		n, ok := value.(float64)
		return ok && n == float64(int64(n))
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	default:
		return true
	}
}

func (m *Manager) validateSecretRefs(ctx context.Context, manifest Manifest, configJSON string) error {
	declared := make(map[string]SecretSpec, len(manifest.Secrets))
	for _, spec := range manifest.Secrets {
		if spec.Name != "" {
			declared[spec.Name] = spec
		}
	}
	configRefs, err := collectConfigSecretRefs(configJSON)
	if err != nil {
		return err
	}
	for ref := range configRefs {
		if len(declared) > 0 {
			if _, ok := declared[ref]; !ok {
				return fmt.Errorf("secret ref %q is not declared by manifest", ref)
			}
		}
	}
	for name, spec := range declared {
		if spec.Required {
			configRefs[name] = true
		}
	}
	if len(configRefs) == 0 {
		return nil
	}
	secrets, err := m.repo.ListSecrets(ctx, manifest.ID)
	if err != nil {
		return err
	}
	configured := make(map[string]bool, len(secrets))
	for _, secret := range secrets {
		configured[secret.Name] = secret.CurrentVersion > 0
	}
	var missing []string
	for ref := range configRefs {
		if !configured[ref] {
			missing = append(missing, ref)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("missing configured secret ref(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

func collectConfigSecretRefs(configJSON string) (map[string]bool, error) {
	var value any
	if err := json.Unmarshal([]byte(defaultJSONObject(configJSON)), &value); err != nil {
		return nil, err
	}
	refs := make(map[string]bool)
	collectSecretRefs(value, refs)
	return refs, nil
}

func collectSecretRefs(value any, refs map[string]bool) {
	switch typed := value.(type) {
	case map[string]any:
		for name, child := range typed {
			if strings.HasSuffix(strings.ToLower(name), "_secret_ref") {
				if ref, ok := child.(string); ok && ref != "" {
					refs[ref] = true
				}
			}
			collectSecretRefs(child, refs)
		}
	case []any:
		for _, child := range typed {
			collectSecretRefs(child, refs)
		}
	}
}

func sensitiveConfigPaths(schema json.RawMessage, configJSON string) []string {
	paths := map[string]bool{}
	var root map[string]any
	if len(schema) > 0 {
		_ = json.Unmarshal(schema, &root)
	}
	collectSensitiveSchemaPaths(root, "$", paths)
	var config any
	if err := json.Unmarshal([]byte(configJSON), &config); err == nil {
		collectSensitiveNamePaths(config, "$", paths)
	}
	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

func collectSensitiveSchemaPaths(schema map[string]any, path string, paths map[string]bool) {
	if len(schema) == 0 {
		return
	}
	if isSensitiveSchema(schema) {
		paths[path] = true
	}
	props, _ := schema["properties"].(map[string]any)
	for name, raw := range props {
		child, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		collectSensitiveSchemaPaths(child, path+"."+name, paths)
	}
}

func isSensitiveSchema(schema map[string]any) bool {
	for _, key := range []string{"sensitive", "secret", "writeOnly"} {
		if value, ok := schema[key].(bool); ok && value {
			return true
		}
	}
	if format, _ := schema["format"].(string); isSensitiveName(format) {
		return true
	}
	return false
}

func collectSensitiveNamePaths(value any, path string, paths map[string]bool) {
	switch typed := value.(type) {
	case map[string]any:
		for name, child := range typed {
			childPath := path + "." + name
			if isSensitiveName(name) {
				paths[childPath] = true
			}
			collectSensitiveNamePaths(child, childPath, paths)
		}
	case []any:
		for idx, child := range typed {
			collectSensitiveNamePaths(child, fmt.Sprintf("%s[%d]", path, idx), paths)
		}
	}
}

func isSensitiveName(name string) bool {
	lower := strings.ToLower(name)
	for _, marker := range []string{"secret", "password", "token", "key", "credential"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func redactJSON(configJSON string, sensitivePaths []string) (string, error) {
	var value any
	if err := json.Unmarshal([]byte(configJSON), &value); err != nil {
		return "", err
	}
	pathSet := make(map[string]bool, len(sensitivePaths))
	for _, path := range sensitivePaths {
		pathSet[path] = true
	}
	value = redactValue(value, "$", pathSet)
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func redactValue(value any, path string, sensitive map[string]bool) any {
	if sensitive[path] {
		return "[REDACTED]"
	}
	switch typed := value.(type) {
	case map[string]any:
		next := make(map[string]any, len(typed))
		for name, child := range typed {
			next[name] = redactValue(child, path+"."+name, sensitive)
		}
		return next
	case []any:
		next := make([]any, len(typed))
		for idx, child := range typed {
			next[idx] = redactValue(child, fmt.Sprintf("%s[%d]", path, idx), sensitive)
		}
		return next
	default:
		return value
	}
}

func redactedDiffJSON(oldConfig, newConfig string, sensitivePaths []string) (string, error) {
	oldRedacted, err := redactJSON(defaultJSONObject(oldConfig), sensitivePaths)
	if err != nil {
		return "", err
	}
	newRedacted, err := redactJSON(defaultJSONObject(newConfig), sensitivePaths)
	if err != nil {
		return "", err
	}
	var oldValue any
	var newValue any
	if err := json.Unmarshal([]byte(oldRedacted), &oldValue); err != nil {
		return "", err
	}
	if err := json.Unmarshal([]byte(newRedacted), &newValue); err != nil {
		return "", err
	}
	diff := map[string]any{
		"changed": !reflect.DeepEqual(oldValue, newValue),
		"before":  oldValue,
		"after":   newValue,
	}
	data, err := json.Marshal(diff)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func redactSecretText(text string) string {
	if text == "" {
		return ""
	}
	return "[REDACTED]"
}

func flattenHandlers(byPlugin map[string][]*upstreamHandler) []*upstreamHandler {
	var handlers []*upstreamHandler
	for _, pluginHandlers := range byPlugin {
		handlers = append(handlers, pluginHandlers...)
	}
	return handlers
}

func handlerSummaries(handlers []*upstreamHandler) []DispatchHandlerSummary {
	summaries := make([]DispatchHandlerSummary, 0, len(handlers))
	for _, handler := range handlers {
		summaries = append(summaries, DispatchHandlerSummary{
			PluginID:        handler.pluginID,
			ArtifactID:      handler.artifactID,
			Priority:        handler.priority,
			HandlerID:       handler.handlerID,
			ExtensionPoint:  ExtensionUpstreamConnect,
			Mode:            handler.mode,
			TimeoutMS:       handler.timeout.Milliseconds(),
			Calls:           handler.calls.Load(),
			Errors:          handler.errors.Load(),
			Panics:          handler.panics.Load(),
			Timeouts:        handler.timeouts.Load(),
			Blocked:         handler.blocked.Load(),
			ActiveProxy:     handler.activeProxy.Load(),
			ProxyStarted:    handler.proxyStarted.Load(),
			ProxyCompleted:  handler.proxyCompleted.Load(),
			ProxyErrors:     handler.proxyErrors.Load(),
			ProxyBytesIn:    handler.proxyBytesIn.Load(),
			ProxyBytesOut:   handler.proxyBytesOut.Load(),
			ProxyDurationMS: handler.proxyDuration.Load(),
			DurationCount:   handler.durationCount.Load(),
			DurationSumMS:   handler.durationSumMS.Load(),
			DurationMaxMS:   handler.durationMaxMS.Load(),
		})
	}
	return summaries
}
