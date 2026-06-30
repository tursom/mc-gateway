// internal/pluginmanager/manager.go 协调插件记录、制品加载、钩子分发快照和生命周期迁移。

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
	"github.com/tursom/mc-gateway/plugin/official/rulepolicy"
)

type RuntimeAdapter interface {
	Load(ctx context.Context, artifact ArtifactRecord, pluginRecord PluginRecord, gateway *Gateway) (api.Plugin, error)
}

// ConfigDryRunAdapter 是运行时适配器的可选能力。支持该能力时，配置保存前
// 可以真正实例化插件并调用 ReloadConfig，从而提前发现 schema 之外的错误。
type ConfigDryRunAdapter interface {
	DryRunConfig(ctx context.Context, artifact ArtifactRecord, pluginRecord PluginRecord) error
}

// GoPluginAdapter 加载 Go plugin 或内置插件，是当前 in-process 插件运行模式的默认实现。
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
	if artifact.RuntimeType == RuntimeBuiltin || artifact.PluginID == "official.rule-policy" {
		return instantiateBuiltinPlugin(artifact, pluginRecord, gateway, init)
	}
	// Go plugin 只能加载与当前进程 Go 版本、架构和 ABI 匹配的 .so 文件。
	// 这些兼容性检查在制品校验和构建阶段完成，这里只负责打开和实例化。
	opened, err := stdplugin.Open(artifact.FilePath)
	if err != nil {
		return nil, err
	}
	symbolName := "Plugin"
	var manifest Manifest
	if err := json.Unmarshal([]byte(artifact.MetadataJSON), &manifest); err == nil && manifest.Runtime.EntrySymbol != "" {
		symbolName = manifest.Runtime.EntrySymbol
	}
	// 默认入口符号是 Plugin，也允许 manifest 指定自定义入口，便于未来兼容
	// 不同构建工具生成的插件包。
	symbol, err := opened.Lookup(symbolName)
	if err != nil {
		return nil, err
	}
	factory, ok := symbol.(func() api.Plugin)
	if !ok {
		return nil, fmt.Errorf("plugin symbol %q has invalid signature", symbolName)
	}
	instance := factory()
	// 插件配置对象由插件自己声明；宿主只负责把持久化 JSON 解入该对象，
	// 再交给 ReloadConfig 做插件内部校验。
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

// instantiateBuiltinPlugin 让官方内置插件走同一套 Plugin 接口和配置流程，
// 避免在调用路径上区分内置插件与外部上传插件。
func instantiateBuiltinPlugin(artifact ArtifactRecord, pluginRecord PluginRecord, gateway *Gateway, init bool) (api.Plugin, error) {
	var instance api.Plugin
	switch artifact.PluginID {
	case "official.rule-policy":
		instance = rulepolicy.New()
	default:
		return nil, fmt.Errorf("unknown builtin plugin %q", artifact.PluginID)
	}
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
	repo                      Repository
	store                     ArtifactStore
	adapter                   RuntimeAdapter
	adapterManaged            bool
	builders                  map[string]SourceBuilder
	handleConn                func(net.Conn)
	wg                        *sync.WaitGroup
	policyProfile             string
	requireConformanceFixture bool
	nodeID                    string
	startedAt                 int64
	ingressReservedListeners  []IngressReservedListener
	futureGates               FutureRuntimeGates
	sandboxPolicy             SandboxPolicy
	sandboxSelfCheck          SandboxEnvironmentSelfCheck
	wasmRunner                *WASMRunner
	ingressLifecycle          *IngressLifecycleManager

	mu                sync.Mutex
	loaded            map[string]*loadedPlugin
	snapshot          atomic.Value
	extensionSnapshot atomic.Value
	routeCacheMu      sync.Mutex
	routeCache        map[string]routeCacheEntry

	// proxyConns 只跟踪由插件托管代理的连接，用于停用插件时的 drain 和强制关闭。
	proxyMu     sync.Mutex
	proxySeq    uint64
	proxyConns  map[uint64]*proxyConnection
	drainingIDs map[string]bool
	operations  *Operations

	// serviceMode/hosts 预留给插件运行时从进程内迁移到独立宿主的服务模式。
	serviceMode      string
	hostMu           sync.Mutex
	hosts            map[string]*pluginHostProcess
	pendingHostStops map[string]*PluginHostSupervisorProcess

	feedSchedulerMu     sync.Mutex
	feedSchedulerCancel context.CancelFunc
	feedSchedulerDone   chan struct{}
}

// loadedPlugin 是内存中的插件实例和它注册的扩展快照。数据库记录说明期望状态，
// loadedPlugin 说明当前进程实际已经加载了什么。
type loadedPlugin struct {
	record     PluginRecord
	artifact   ArtifactRecord
	instance   api.Plugin
	runtime    RuntimeInstance
	gateway    *Gateway
	handlers   []*upstreamHandler
	extensions pluginExtensions
}

// pluginExtensions 按扩展类型拆分注册结果，便于发布不可变快照给不同热路径使用。
type pluginExtensions struct {
	routes      []*routeHandler
	rules       []*ruleHandler
	statuses    []*statusHandler
	middleware  []*middlewareHandler
	subscribers []*subscriberHandler
	providers   []ProviderSummary
}

// upstreamHandler 包装一个上游连接钩子，并保存调用、错误、超时和代理流量指标。
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

	calls            atomic.Uint64
	errors           atomic.Uint64
	panics           atomic.Uint64
	timeouts         atomic.Uint64
	blocked          atomic.Uint64
	activeProxy      atomic.Int64
	drainingProxy    atomic.Int64
	proxyStarted     atomic.Uint64
	proxyCompleted   atomic.Uint64
	proxyErrors      atomic.Uint64
	proxyBytesIn     atomic.Uint64
	proxyBytesOut    atomic.Uint64
	proxyDuration    atomic.Uint64
	proxyForceClosed atomic.Uint64
	durationCount    atomic.Uint64
	durationSumMS    atomic.Uint64
	durationMaxMS    atomic.Uint64
	lastProxyError   atomic.Value
}

type proxyConnection struct {
	id                  uint64
	pluginID            string
	artifactID          string
	handlerID           string
	handler             *upstreamHandler
	client              net.Conn
	endpoint            net.Conn
	startedAt           time.Time
	draining            bool
	forceCloseRequested bool
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
	DB                        *sql.DB
	ArtifactRoot              string
	Now                       func() time.Time
	HandleConn                func(net.Conn)
	WaitGroup                 *sync.WaitGroup
	Adapter                   RuntimeAdapter
	Builders                  map[string]SourceBuilder
	PolicyProfile             string
	RequireConformanceFixture bool
	NodeID                    string
	IngressReservedListeners  []IngressReservedListener
	FutureRuntimeGates        FutureRuntimeGates
	SandboxPolicy             SandboxPolicy
	SandboxSelfCheck          SandboxEnvironmentSelfCheck
	AdvisoryFeeds             []ExternalFeedSchedule
	VulnerabilityFeeds        []ExternalFeedSchedule
}

// New 构造插件管理器并初始化内存快照。官方内置插件和插件服务模式会在这里
// 尽力注册/应用，失败不会阻止网关启动，后续 Admin API 仍可修复状态。
func New(options Options) *Manager {
	adapter := options.Adapter
	adapterManaged := adapter == nil
	if adapter == nil {
		adapter, _ = RuntimeAdapterFactory{}.AdapterFor(PluginServiceModeInProcess, RuntimeGoPlugin)
	}
	repo := NewRepository(options.DB)
	store := NewArtifactStore(options.ArtifactRoot)
	if options.Now != nil {
		repo.now = options.Now
		store.now = options.Now
	}
	manager := &Manager{
		repo:                      repo,
		store:                     store,
		adapter:                   adapter,
		adapterManaged:            adapterManaged,
		builders:                  options.Builders,
		handleConn:                options.HandleConn,
		wg:                        options.WaitGroup,
		policyProfile:             options.PolicyProfile,
		requireConformanceFixture: options.RequireConformanceFixture,
		loaded:                    make(map[string]*loadedPlugin),
		ingressReservedListeners:  append([]IngressReservedListener(nil), options.IngressReservedListeners...),
		futureGates:               normalizeFutureRuntimeGates(options.FutureRuntimeGates),
		sandboxPolicy:             normalizeSandboxPolicy(options.SandboxPolicy),
		sandboxSelfCheck:          options.SandboxSelfCheck,
		routeCache:                make(map[string]routeCacheEntry),
		proxyConns:                make(map[uint64]*proxyConnection),
		drainingIDs:               make(map[string]bool),
		hosts:                     make(map[string]*pluginHostProcess),
		pendingHostStops:          make(map[string]*PluginHostSupervisorProcess),
	}
	if manager.sandboxSelfCheck == nil {
		manager.sandboxSelfCheck = defaultSandboxEnvironmentSelfCheck
	}
	manager.wasmRunner = NewWASMRunner()
	manager.ingressLifecycle = NewIngressLifecycleManager(manager.repo, manager.ingressReservedListeners)
	manager.startedAt = time.Now().Unix()
	manager.operations = NewOperationsWithNodeID(manager.repo, options.ArtifactRoot, options.NodeID)
	manager.nodeID = manager.operations.nodeID
	if manager.builders == nil {
		// 默认同时提供本地进程构建和容器构建能力；部署方可在 Options 中收窄。
		manager.builders = map[string]SourceBuilder{
			BuilderTypeLocalProcess: LocalProcessBuilder{StoreRoot: options.ArtifactRoot},
			BuilderTypeContainer:    ContainerBuilder{},
		}
	}
	manager.publish(nil)
	manager.publishExtensionsLocked(nil)
	_ = manager.EnsureOfficialPlugins(context.Background(), "system")
	_ = manager.ApplyPluginServiceMode(context.Background())
	if state, err := manager.PluginServiceState(context.Background()); err == nil {
		_ = manager.recordPluginNodeState(context.Background(), state)
	}
	_ = manager.StartExternalFeedSchedulers(context.Background(), options.AdvisoryFeeds, options.VulnerabilityFeeds)
	return manager
}

// EnsureOfficialPlugins 将内置官方插件登记为普通制品记录。这样 UI、治理、
// 配置和启停流程都可以复用同一套插件管理模型。
func (m *Manager) EnsureOfficialPlugins(ctx context.Context, actor string) error {
	now := time.Now().Unix()
	manifest := Manifest{
		SchemaVersion: SchemaVersion,
		ID:            "official.rule-policy",
		Name:          "Official Rule Policy",
		Version:       "0.1.0",
		Description:   "Built-in official rule/policy extension for host rewrite, CIDR policy, rate limit, maintenance mode and upstream rewrite.",
		ArtifactType:  ArtifactTypeBinary,
		Runtime: RuntimeManifest{
			Type: RuntimeBuiltin,
		},
		APIVersion: APIVersion,
		ExtensionPoints: []ExtensionPoint{
			{Type: "middleware", Key: ExtensionConnectionFilter},
			{Type: "middleware", Key: ExtensionHandshakeFilter},
			{Type: "provider", Key: ExtensionRouteResolve},
			{Type: "rule", Key: ExtensionRuleEvaluate},
			{Type: "hook", Key: ExtensionStatusPing},
		},
		Capabilities:  json.RawMessage(`{"extension_points":["connection.filter/v1","handshake.filter/v1","route.resolve/v1","rule.evaluate/v1","status.ping/v1"],"middleware":{"fail_policy":"fail_open"},"route":{"cache_ttl_ms":60000},"status":{"hosts":["*"]}}`),
		RuntimeLimits: RuntimeLimits{HandlerTimeoutMS: int(DefaultHandlerTimeout / time.Millisecond)},
		ConfigSchema:  json.RawMessage(`{"type":"object","properties":{"host_rewrite":{"type":"object"},"upstream_rewrite":{"type":"object"},"source_allow_cidr":{"type":"array"},"source_deny_cidr":{"type":"array"},"rate_limit":{"type":"object"},"maintenance":{"type":"object","properties":{"enabled":{"type":"boolean"},"hosts":{"type":"array"},"motd":{"type":"string"},"favicon":{"type":"string"},"online_players":{"type":"integer"},"max_players":{"type":"integer"},"version":{"type":"string"},"window":{"type":"string"},"status_by_host":{"type":"object"}}}}}`),
	}
	metadata, _ := artifactMetadataJSON(manifest, ConformanceSummary{
		Source:  "builtin:official.rule-policy",
		OK:      true,
		Total:   5,
		Passed:  5,
		Skipped: 0,
		Failed:  0,
	}, true, nil)
	extensionPoints, _ := json.Marshal(manifest.ExtensionPoints)
	summaryJSON, _ := manifestCapabilitiesSummaryJSON(manifest)
	artifact := ArtifactRecord{
		ID:                      "builtin-official-rule-policy-0.1.0",
		PluginID:                manifest.ID,
		Version:                 manifest.Version,
		FileName:                "builtin:official.rule-policy",
		FilePath:                "",
		SHA256:                  "builtin:official.rule-policy:0.1.0",
		PackageSHA256:           "builtin:official.rule-policy:0.1.0",
		ArtifactType:            ArtifactTypeBinary,
		RuntimeType:             RuntimeBuiltin,
		Status:                  ArtifactStatusLoadable,
		MetadataJSON:            string(metadata),
		CapabilitiesSummaryJSON: string(summaryJSON),
		ExtensionPointsJSON:     string(extensionPoints),
		APIVersion:              APIVersion,
		UploadedBy:              actor,
		CreatedAt:               now,
		UpdatedAt:               now,
	}
	if err := m.repo.SaveArtifact(ctx, artifact); err != nil {
		return err
	}
	_ = m.repo.RecordOperation(ctx, manifest.ID, artifact.ID, "official_plugin_register", "succeeded", actor, "official rule/policy plugin registered", nil)
	return nil
}

// UploadArtifact 校验并保存二进制插件制品；源码包会转交给源码保存流程，
// 因为源码上传后还需要自动排队构建。
func (m *Manager) UploadArtifact(ctx context.Context, upload ArtifactUpload) (ArtifactRecord, error) {
	artifact, err := m.store.ValidateAndStore(upload)
	if err != nil {
		audit := m.store.uploadFailureAuditDetails(upload, err)
		_ = m.repo.RecordOperation(ctx, audit.pluginID, audit.artifactID, "artifact_upload", "failed", upload.Actor, err.Error(), audit.metadata)
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

// UploadSource 保存源码插件包并创建构建记录。真正构建可以立即运行，也可以
// 由管理端稍后触发 RunBuild。
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
	req = defaultBuildRequestForProfile(req, source, m.currentPolicyProfile())
	// Go plugin 与宿主进程存在 ABI 约束，目前只允许构建当前网关所在平台的目标。
	if req.GOOS != runtime.GOOS || req.GOARCH != runtime.GOARCH {
		return BuildRecord{}, fmt.Errorf("build target %s/%s does not match gateway %s/%s", req.GOOS, req.GOARCH, runtime.GOOS, runtime.GOARCH)
	}
	if err := m.validateBuildPolicy(req); err != nil {
		return BuildRecord{}, err
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

// RunBuild 执行已排队的源码构建，并把产出的二进制制品重新写入制品仓库。
// 构建记录始终会落库，失败时也会保存日志摘要，便于管理端诊断。
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
	if err := m.validateBuildPolicy(req); err != nil {
		build.StartedAt = time.Now().Unix()
		build.EndedAt = build.StartedAt
		build.DurationMS = 0
		build.Status = BuildStatusFailed
		build.Error = err.Error()
		build.LogSummary = sanitizeLog(err.Error())
		build.SourceSHA256 = source.SHA256
		_ = m.repo.FinishBuild(ctx, build)
		_ = m.repo.RecordOperation(ctx, source.PluginID, source.ID, "source_build", "failed", actor, err.Error(), map[string]any{
			"build_id":       build.ID,
			"source_sha256":  source.SHA256,
			"builder_type":   req.BuilderType,
			"active_changed": false,
		})
		return m.repo.Build(ctx, build.ID)
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

func (m *Manager) validateBuildPolicy(req BuildRequest) error {
	if normalizeProfile(m.currentPolicyProfile()) == PolicyProfileProd && req.BuilderType == BuilderTypeLocalProcess {
		return errors.New("local-process source builder is disabled in prod profile; use container builder or upload an external CI binary artifact")
	}
	if req.BuilderType == BuilderTypeContainer && strings.TrimSpace(req.BuilderImage) == "" {
		return errors.New("container source builder requires builder_image")
	}
	return nil
}

// SetDesired 只修改插件的期望状态，不直接改变当前进程已加载的插件。
// 调用方需要再执行 Enable/Disable/Reconcile 才会推动运行态收敛。
func (m *Manager) SetDesired(ctx context.Context, actor, pluginID, artifactID, desiredState, configJSON string, priority int) (PluginRecord, error) {
	if desiredState == "" {
		desiredState = DesiredDisabled
	}
	if desiredState != DesiredDeleted {
		// 任何非删除状态都先做配置 dry-run，避免把无法加载的配置写成新的期望状态。
		if _, err := m.DryRunConfig(ctx, pluginID, artifactID, configJSON); err != nil {
			metadata := map[string]any{
				"active_changed": false,
			}
			for key, value := range m.wasmValidationFailureMetadata(ctx, artifactID, err) {
				metadata[key] = value
			}
			_ = m.repo.RecordOperation(ctx, pluginID, artifactID, "config_dry_run", "failed", actor, err.Error(), metadata)
			if metadata["runtime_type"] == RuntimeWASM {
				_ = m.repo.RecordOperation(ctx, pluginID, artifactID, "wasm_validation", "failed", actor, err.Error(), metadata)
			}
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
	m.recordPluginNodeRuntimeState(ctx, pluginID)
	rollout, rolloutErr := m.PluginRolloutStatus(ctx, pluginID)
	metadata := map[string]any{
		"desired_state":       desiredState,
		"desired_generation":  pluginRecord.DesiredGeneration,
		"artifact_available":  false,
		"artifact_dist_mode":  "",
		"artifact_dist_state": "",
		"nodes_total":         0,
		"nodes_ready":         0,
		"nodes_failed":        0,
		"partial_failure":     false,
		"retry_policy":        "node heartbeat reconcile retries desired generation until ready",
	}
	if rolloutErr == nil {
		metadata["artifact_available"] = rollout.ArtifactDistribution
		metadata["artifact_dist_mode"] = rollout.ArtifactDistributionMode
		metadata["artifact_dist_state"] = rollout.ArtifactDistributionStatus
		metadata["nodes_total"] = rollout.NodesTotal
		metadata["nodes_ready"] = rollout.NodesReady
		metadata["nodes_failed"] = rollout.NodesFailed
		metadata["partial_failure"] = rollout.PartialFailure
	}
	_ = m.repo.RecordOperation(ctx, pluginID, artifactID, "cluster_desired_apply", "succeeded", actor, "desired generation queued for automatic cluster apply", metadata)
	if desiredState == DesiredEnabled {
		if err := m.reloadLoadedRuntime(ctx, actor, pluginRecord, "config_update"); err != nil {
			return PluginRecord{}, err
		}
		if refreshed, err := m.repo.Plugin(ctx, pluginID); err == nil {
			pluginRecord = refreshed
		}
	}
	return pluginRecord, nil
}

// DryRunConfig 执行保存配置前的完整预检：JSON 合法性、制品归属、治理门禁、
// schema、密钥引用以及运行时 ReloadConfig 都会在这里验证。
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
	sensitivePaths := sensitiveConfigPaths(manifest.ConfigSchema, configJSON)
	pluginRecord, err := m.pluginRecordForDryRun(ctx, pluginID, artifactID, configJSON)
	if err != nil {
		result.Error = err.Error()
		return result, err
	}
	dryRunAdapter := m.adapter
	if m.adapterManaged {
		dryRunAdapter, _ = RuntimeAdapterFactory{}.AdapterFor(m.serviceMode, artifact.RuntimeType)
		if typed, ok := dryRunAdapter.(WASMAdapter); ok {
			typed.Runner = m.wasmRunner
			dryRunAdapter = typed
		}
	}
	if dryRunner, ok := dryRunAdapter.(ConfigDryRunAdapter); ok {
		// 运行时 dry-run 会实例化插件但不调用 Init，避免注册钩子或启动后台任务。
		if err := dryRunner.DryRunConfig(ctx, artifact, pluginRecord); err != nil {
			message := redactSensitiveConfigText(err.Error(), configJSON, sensitivePaths)
			result.Error = message
			return result, errors.New(message)
		}
	}
	currentConfig := "{}"
	if current, err := m.repo.Plugin(ctx, pluginID); err == nil {
		currentConfig = current.ConfigJSON
	}
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
	decision, err := m.EvaluateReleaseGate(ctx, pluginID, artifactID, GovernanceActionRollback, m.currentPolicyProfile(), current.ConfigJSON)
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
	rollbackMetadata := map[string]any{
		"desired_generation":  plugin.DesiredGeneration,
		"active_changed":      false,
		"governance_decision": decision,
	}
	for key, value := range m.wasmArtifactMetadata(ctx, artifactID) {
		rollbackMetadata[key] = value
	}
	_ = m.repo.RecordOperation(ctx, pluginID, artifactID, "artifact_rollback", "succeeded", actor, "artifact rollback desired state updated", rollbackMetadata)
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
	decision, err := m.EvaluateReleaseGate(ctx, snapshot.PluginID, artifactID, GovernanceActionRollback, m.currentPolicyProfile(), snapshot.ConfigJSON)
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
	rollbackMetadata := map[string]any{
		"snapshot_id":         snapshot.ID,
		"full_desired":        fullDesired,
		"desired_generation":  next.DesiredGeneration,
		"active_changed":      false,
		"governance_decision": decision,
	}
	for key, value := range m.wasmArtifactMetadata(ctx, artifactID) {
		rollbackMetadata[key] = value
	}
	_ = m.repo.RecordOperation(ctx, snapshot.PluginID, artifactID, "config_rollback", "succeeded", actor, "config snapshot rollback desired state updated", rollbackMetadata)
	return next, nil
}

func (m *Manager) Load(ctx context.Context, actor, pluginID string) (PluginRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Load 只把插件实例化到内存并登记为 loaded，不发布到热路径。
	// 管理端可用它验证制品和配置，而不立即影响在线连接。
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

// Enable 将期望状态推进为启用，并把插件处理器发布到连接热路径。
// 发布前会先通过治理门禁，避免高风险制品绕过评审直接生效。
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
	decision, err := m.EvaluateReleaseGate(ctx, pluginID, pluginRecord.DesiredArtifactID, GovernanceActionEnable, m.currentPolicyProfile(), pluginRecord.ConfigJSON)
	if err != nil {
		_ = m.repo.MarkRuntime(ctx, pluginID, RuntimeFailed, "", "", pluginRecord.AppliedGeneration, err.Error(), map[string]any{"governance": decision}, nil)
		m.recordPluginNodeRuntimeState(ctx, pluginID)
		_ = m.repo.RecordOperation(ctx, pluginID, pluginRecord.DesiredArtifactID, "enable_gate", "failed", actor, err.Error(), map[string]any{
			"decision": decision,
		})
		return PluginRecord{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// 真正加载与发布都在同一把锁内完成，保证 snapshot、extensions 和 loaded
	// 三类内存状态不会被并发读到半更新结果。
	loaded, err := m.loadLocked(ctx, pluginRecord)
	if err != nil {
		_ = m.repo.RecordOperation(ctx, pluginID, pluginRecord.DesiredArtifactID, "enable", "failed", actor, err.Error(), nil)
		return PluginRecord{}, err
	}
	if len(loaded.handlers) == 0 && loaded.extensions.empty() {
		err := fmt.Errorf("plugin %q did not register any supported extension point", pluginID)
		_ = m.repo.MarkRuntime(ctx, pluginID, RuntimeFailed, "", loaded.artifact.ID, pluginRecord.AppliedGeneration, err.Error(), map[string]any{"error": err.Error()}, nil)
		m.recordPluginNodeRuntimeStateLocked(ctx, pluginID)
		_ = m.repo.RecordOperation(ctx, pluginID, pluginRecord.DesiredArtifactID, "enable", "failed", actor, err.Error(), nil)
		return PluginRecord{}, err
	}

	current := m.currentHandlersLocked()
	current[pluginID] = loaded.handlers
	next := flattenHandlers(current)
	extensions := m.currentExtensionsLocked()
	extensions[pluginID] = loaded.extensions
	if err := m.markEnabled(ctx, loaded); err != nil {
		return PluginRecord{}, err
	}
	// 数据库运行态先写成功，再发布内存快照；这样 UI 看到 enabled 时，
	// 连接热路径也已经具备对应处理器。
	m.clearDrainingLocked(pluginID)
	m.publish(next)
	m.publishExtensionsLocked(extensions)
	_ = m.repo.UpdateArtifactStatus(ctx, loaded.artifact.ID, ArtifactStatusLoaded, "")
	enableMetadata := map[string]any{
		"desired_generation":  loaded.record.DesiredGeneration,
		"handler_count":       len(loaded.handlers),
		"governance_decision": decision,
	}
	for key, value := range wasmLifecycleMetadata(loaded) {
		enableMetadata[key] = value
	}
	_ = m.repo.RecordOperation(ctx, pluginID, loaded.artifact.ID, "enable", "succeeded", actor, "plugin enabled", enableMetadata)
	return m.repo.Plugin(ctx, pluginID)
}

// Disable 从热路径移除插件并进入 drain。Go plugin 不能从进程卸载，
// 因此这里停止任务、移除分发入口，并等待已有 protocol-proxy 连接结束。
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
	m.removeExtensionsLocked(pluginID)
	// 先标记 draining，再 Destroy 插件实例，确保后续管理操作能看到仍在
	// 转发中的插件代理连接。
	m.markDrainingLocked(pluginID)
	m.markHostDraining(pluginID)
	m.operations.StopPlugin(pluginID)
	disableMetadata := map[string]any{}
	if loaded := m.loaded[pluginID]; loaded != nil {
		for key, value := range wasmLifecycleMetadata(loaded) {
			disableMetadata[key] = value
		}
		var errs []error
		if m.shouldDeferProcessHostStop(pluginID, loaded) {
			errs = m.drainRuntimeInstance(ctx, loaded)
			m.deferProcessHostStop(pluginID, loaded.runtime.HostProcess)
		} else {
			errs = m.stopRuntimeInstance(ctx, loaded)
		}
		for _, err := range errs {
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
	m.recordPluginNodeRuntimeStateLocked(ctx, pluginID)
	if len(disableMetadata) == 0 {
		disableMetadata = nil
	}
	_ = m.repo.RecordOperation(ctx, pluginID, pluginRecord.DesiredArtifactID, "disable", "succeeded", actor, "plugin disabled", disableMetadata)
	return m.repo.Plugin(ctx, pluginID)
}

// Delete 与 Disable 类似，但把期望状态写为 deleted。实际制品清理仍由 GC
// 根据引用关系判断，避免删除仍被快照或历史操作引用的文件。
func (m *Manager) Delete(ctx context.Context, actor, pluginID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	pluginRecord, err := m.repo.Plugin(ctx, pluginID)
	if err != nil {
		return err
	}
	m.removeFromDispatchLocked(pluginID)
	m.removeExtensionsLocked(pluginID)
	m.markDrainingLocked(pluginID)
	m.markHostDraining(pluginID)
	m.operations.StopPlugin(pluginID)
	if loaded := m.loaded[pluginID]; loaded != nil {
		_ = m.stopRuntimeInstance(ctx, loaded)
	}
	delete(m.loaded, pluginID)
	if _, err := m.repo.UpsertDesired(ctx, actor, pluginRecord.ID, pluginRecord.DesiredArtifactID, DesiredDeleted, pluginRecord.ConfigJSON, pluginRecord.Priority); err != nil && !errors.Is(err, ErrPluginNotFound) {
		return err
	}
	m.deletePluginNodeRuntimeState(ctx, pluginID)
	_ = m.repo.RecordOperation(ctx, pluginID, pluginRecord.DesiredArtifactID, "delete", "succeeded", actor, "plugin deleted", map[string]any{
		"cleanup": "pending_restart_for_loaded_go_plugin",
	})
	return nil
}

func (m *Manager) stopRuntimeInstance(ctx context.Context, loaded *loadedPlugin) []error {
	var errs []error
	errs = append(errs, m.drainRuntimeInstance(ctx, loaded)...)
	if loaded.instance != nil {
		if err := loaded.instance.Destroy(); err != nil {
			errs = append(errs, err)
		}
	}
	if lifecycle, ok := m.adapter.(RuntimeAdapterLifecycle); ok {
		if err := lifecycle.Stop(ctx, loaded.runtime); err != nil {
			errs = append(errs, err)
		}
	}
	if loaded.runtime.HostProcess != nil {
		m.markHostStopped(loaded.record.ID, loaded.artifact.ID, loaded.runtime.HostProcess)
	}
	return errs
}

func (m *Manager) drainRuntimeInstance(ctx context.Context, loaded *loadedPlugin) []error {
	if lifecycle, ok := m.adapter.(RuntimeAdapterLifecycle); ok {
		if err := lifecycle.Drain(ctx, loaded.runtime); err != nil {
			return []error{err}
		}
	}
	return nil
}

func (m *Manager) shouldDeferProcessHostStop(pluginID string, loaded *loadedPlugin) bool {
	return m.serviceMode == PluginServiceModeGoPluginProcess &&
		loaded != nil &&
		loaded.runtime.HostProcess != nil &&
		m.activeProxyCountLocked(pluginID) > 0
}

func (m *Manager) deferProcessHostStop(pluginID string, process *PluginHostSupervisorProcess) {
	if process == nil {
		return
	}
	m.hostMu.Lock()
	if m.pendingHostStops == nil {
		m.pendingHostStops = make(map[string]*PluginHostSupervisorProcess)
	}
	m.pendingHostStops[pluginID] = process
	m.hostMu.Unlock()
}

// Reconcile 根据数据库中的期望启用列表重建内存分发快照，主要用于进程启动
// 或运行态状态漂移后的自愈。
func (m *Manager) Reconcile(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	desired, err := m.repo.DesiredEnabled(ctx)
	if err != nil {
		return err
	}
	nextByPlugin := make(map[string][]*upstreamHandler)
	extensionsByPlugin := make(map[string]pluginExtensions)
	for _, pluginRecord := range desired {
		// 单个插件失败不阻断其他插件收敛；失败会记录到 runtime_state 和操作日志。
		decision, err := m.EvaluateReleaseGate(ctx, pluginRecord.ID, pluginRecord.DesiredArtifactID, GovernanceActionEnable, m.currentPolicyProfile(), pluginRecord.ConfigJSON)
		if err != nil {
			_ = m.repo.MarkRuntime(ctx, pluginRecord.ID, RuntimeFailed, "", "", pluginRecord.AppliedGeneration, err.Error(), map[string]any{"governance": decision}, nil)
			m.recordPluginNodeRuntimeStateLocked(ctx, pluginRecord.ID)
			_ = m.repo.RecordOperation(ctx, pluginRecord.ID, pluginRecord.DesiredArtifactID, "reconcile_gate", "failed", "system", err.Error(), map[string]any{"decision": decision})
			continue
		}
		loaded, err := m.loadLocked(ctx, pluginRecord)
		if err != nil {
			_ = m.repo.MarkRuntime(ctx, pluginRecord.ID, RuntimeFailed, "", "", pluginRecord.AppliedGeneration, err.Error(), map[string]any{"error": err.Error()}, nil)
			m.recordPluginNodeRuntimeStateLocked(ctx, pluginRecord.ID)
			_ = m.repo.RecordOperation(ctx, pluginRecord.ID, pluginRecord.DesiredArtifactID, "reconcile", "failed", "system", err.Error(), nil)
			continue
		}
		if len(loaded.handlers) == 0 && loaded.extensions.empty() {
			err := fmt.Errorf("plugin %q did not register any supported extension point", pluginRecord.ID)
			_ = m.repo.MarkRuntime(ctx, pluginRecord.ID, RuntimeFailed, "", loaded.artifact.ID, pluginRecord.AppliedGeneration, err.Error(), map[string]any{"error": err.Error()}, nil)
			m.recordPluginNodeRuntimeStateLocked(ctx, pluginRecord.ID)
			_ = m.repo.RecordOperation(ctx, pluginRecord.ID, pluginRecord.DesiredArtifactID, "reconcile", "failed", "system", err.Error(), nil)
			continue
		}
		nextByPlugin[pluginRecord.ID] = loaded.handlers
		extensionsByPlugin[pluginRecord.ID] = loaded.extensions
		_ = m.markEnabled(ctx, loaded)
		m.clearDrainingLocked(pluginRecord.ID)
	}
	// 所有插件都处理完后一次性发布快照，避免热路径在收敛过程中看到部分插件。
	m.publish(flattenHandlers(nextByPlugin))
	m.publishExtensionsLocked(extensionsByPlugin)
	return nil
}

// ConnectUpstream 依次调用当前快照中的上游连接处理器。处理器返回 ErrPass
// 表示让下一个插件继续尝试，返回连接则由网关使用插件提供的上游。
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
	req.Context = context.WithValue(req.Context, pluginHostCallerContextKey{}, req.Context)
	req.InitialData = append([]byte(nil), req.InitialData...)
	for _, handler := range handlers {
		// accept 阶段应尽量轻量，用于快速过滤不关心的主机或上游。
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
			// protocol-proxy 模式由插件代理完整协议流；普通 dialer 模式只提供
			// 已连接的上游 net.Conn，后续转发仍由网关主流程完成。
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

// startProtocolProxy 把客户端连接交给插件提供的协议代理端点。网关仍跟踪连接，
// 以便停用插件时可以 drain 或强制关闭。
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
	runProtocolProxy(ctx, handle, req.Source, endpoint, int64(len(initial)))

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
			m.quarantinePluginLocked(plugin.ID)
			_ = m.repo.MarkRuntime(ctx, plugin.ID, RuntimeDraining, artifact.ID, artifact.ID, plugin.AppliedGeneration, "plugin quarantined by advisory "+advisory.AdvisoryID, map[string]any{
				"quarantine":  true,
				"advisory_id": advisory.AdvisoryID,
			}, nil)
		}
	}
}

func (m *Manager) quarantineAffectedByVulnerability(ctx context.Context, vulnerability VulnerabilityRecord) {
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
		for _, dep := range sbomDependencies(manifest) {
			if vulnerabilityMatchesDependency(vulnerability, dep) {
				m.quarantinePluginLocked(plugin.ID)
				_ = m.repo.MarkRuntime(ctx, plugin.ID, RuntimeDraining, artifact.ID, artifact.ID, plugin.AppliedGeneration, "plugin quarantined by vulnerability "+vulnerability.VulnerabilityID, map[string]any{
					"quarantine":       true,
					"vulnerability_id": vulnerability.VulnerabilityID,
					"package_name":     vulnerability.PackageName,
				}, nil)
				break
			}
		}
	}
}

func (m *Manager) quarantinePluginLocked(pluginID string) {
	m.removeFromDispatchLocked(pluginID)
	m.removeExtensionsLocked(pluginID)
	m.routeCacheMu.Lock()
	m.routeCache = make(map[string]routeCacheEntry)
	m.routeCacheMu.Unlock()
	m.markDrainingLocked(pluginID)
	m.markHostDraining(pluginID)
	m.operations.StopPlugin(pluginID)
}

func (m *Manager) handleWASMRepeatedTrapQuarantine(ctx context.Context, event wasmRuntimeQuarantine) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	loaded := m.loaded[event.PluginID]
	if loaded == nil || loaded.artifact.ID != event.ArtifactID || loaded.artifact.RuntimeType != RuntimeWASM {
		_ = m.repo.RecordOperation(ctx, event.PluginID, event.ArtifactID, "wasm_repeated_trap_quarantine", "warning", "system", event.Reason, map[string]any{
			"runtime_type":    RuntimeWASM,
			"trap_count":      event.TrapCount,
			"trap_threshold":  event.Threshold,
			"extension_point": event.ExtensionPoint,
			"stale_runtime":   true,
		})
		return
	}
	m.quarantinePluginLocked(event.PluginID)
	plugin, err := m.repo.Plugin(ctx, event.PluginID)
	if err != nil {
		return
	}
	summary := event.RuntimeSummary
	if summary == nil {
		summary = m.loadedRuntimeSummary(loaded)
	}
	summary["quarantine"] = true
	summary["quarantine_reason"] = event.Reason
	summary["trap_threshold"] = event.Threshold
	summary["last_error"] = event.LastError
	_ = m.repo.MarkRuntime(ctx, event.PluginID, RuntimeDraining, loaded.artifact.ID, loaded.artifact.ID, plugin.AppliedGeneration, event.Reason, summary, loaded.dispatchSummaries())
	m.recordPluginNodeRuntimeStateLocked(ctx, event.PluginID)
	_ = m.repo.RecordOperation(ctx, event.PluginID, event.ArtifactID, "wasm_repeated_trap_quarantine", "warning", "system", event.Reason, map[string]any{
		"runtime_type":    RuntimeWASM,
		"runtime_abi":     summary["runtime_abi"],
		"module_hash":     summary["module_hash"],
		"trap_count":      event.TrapCount,
		"trap_threshold":  event.Threshold,
		"extension_point": event.ExtensionPoint,
		"last_error":      event.LastError,
		"quarantine":      true,
	})
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
	if secret.HotReload {
		plugin, err := m.repo.Plugin(ctx, pluginID)
		if err != nil {
			if errors.Is(err, ErrPluginNotFound) {
				_ = m.repo.RecordOperation(ctx, pluginID, artifactID, "reload", "skipped", actor, "secret hot reload skipped because plugin desired state is not configured", map[string]any{
					"reload_reason":   "secret_rotation",
					"secret_ref":      "plugin://" + pluginID + "/" + name,
					"active_changed":  false,
					"current_version": secret.CurrentVersion,
				})
				return secret, nil
			}
			_ = m.repo.RecordOperation(ctx, pluginID, artifactID, "reload", "failed", actor, err.Error(), map[string]any{
				"reload_reason":   "secret_rotation",
				"secret_ref":      "plugin://" + pluginID + "/" + name,
				"active_changed":  false,
				"current_version": secret.CurrentVersion,
			})
			return SecretRecord{}, err
		}
		if err := m.reloadLoadedRuntime(ctx, actor, plugin, "secret_rotation"); err != nil {
			return SecretRecord{}, err
		}
	}
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
		lastProxyError := ""
		if conn.handler != nil {
			lastProxyError, _ = conn.handler.lastProxyError.Load().(string)
		}
		summaries = append(summaries, ProxyConnectionSummary{
			ID:                  conn.id,
			PluginID:            conn.pluginID,
			ArtifactID:          conn.artifactID,
			HandlerID:           conn.handlerID,
			StartedAt:           conn.startedAt.Unix(),
			DurationMS:          now.Sub(conn.startedAt).Milliseconds(),
			Draining:            conn.draining,
			ForceCloseRequested: conn.forceCloseRequested,
			LastProxyError:      lastProxyError,
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

func (m *Manager) ArtifactDistributionPackage(ctx context.Context, id string) (ArtifactRecord, string, error) {
	artifact, err := m.repo.Artifact(ctx, id)
	if err != nil {
		return ArtifactRecord{}, "", err
	}
	if artifact.ArtifactType != ArtifactTypeBinary {
		return ArtifactRecord{}, "", fmt.Errorf("artifact %s is %s; only binary distribution packages are transferable", id, artifact.ArtifactType)
	}
	if !m.store.HasDistributionPackage(artifact) {
		return ArtifactRecord{}, "", fmt.Errorf("artifact %s distribution package is missing", id)
	}
	return artifact, m.store.DistributionPackagePath(artifact), nil
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
	state := m.extensionState()
	plan.Routes = routeHandlerSummaries(state.routes)
	plan.Rules = ruleHandlerSummaries(state.rules)
	plan.Statuses = statusHandlerSummaries(state.statuses)
	plan.Middleware = middlewareHandlerSummaries(state.middleware)
	plan.Subscribers = subscriberHandlerSummaries(state.subscribers)
	plan.Providers = append([]ProviderSummary(nil), state.providers...)
	plan.RouteCache = m.RouteCacheSnapshot()
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
	for _, group := range [][]DispatchHandlerSummary{plan.Routes, plan.Rules, plan.Statuses, plan.Middleware, plan.Subscribers} {
		for _, handler := range group {
			if pluginID == "" || handler.PluginID == pluginID {
				handlers = append(handlers, handler)
			}
		}
	}
	builds, err := m.repo.ListBuilds(ctx, pluginID)
	if err != nil {
		return OperationsSnapshot{}, err
	}
	gc, _ := m.operations.GCCandidates(ctx, pluginID)
	snapshot := m.operations.Snapshot(ctx, pluginID, handlers, builds, gc)
	snapshot.CustomMetrics = append(snapshot.CustomMetrics, m.wasmRuntimeMetricSummaries(ctx, pluginID)...)
	snapshot.Exporters = m.operations.ExportSnapshot(ctx, snapshot)
	return snapshot, nil
}

func (m *Manager) wasmRuntimeMetricSummaries(ctx context.Context, pluginID string) []CustomMetricSummary {
	summaries := m.liveWASMRuntimeSummaries(pluginID)
	if pluginID != "" {
		if _, ok := summaries[pluginID]; !ok {
			if plugin, err := m.repo.Plugin(ctx, pluginID); err == nil {
				if summary := jsonMapFromJSONString(plugin.RuntimeSummaryJSON); summary != nil && summary["runtime_type"] == RuntimeWASM {
					summaries[pluginID] = summary
				}
			}
		}
	}
	var metrics []CustomMetricSummary
	for id, summary := range summaries {
		metrics = append(metrics,
			wasmRuntimeMetricSummary(id, "wasm.call_count", summary["call_count"]),
			wasmRuntimeMetricSummary(id, "wasm.duration_count", summary["duration_count"]),
			wasmRuntimeMetricSummary(id, "wasm.duration_sum_ms", summary["duration_sum_ms"]),
			wasmRuntimeMetricSummary(id, "wasm.duration_max_ms", summary["duration_max_ms"]),
			wasmRuntimeMetricSummary(id, "wasm.timeout_count", summary["timeout_count"]),
			wasmRuntimeMetricSummary(id, "wasm.trap_count", summary["trap_count"]),
			wasmRuntimeMetricSummary(id, "wasm.memory_exceeded_count", summary["memory_error_count"]),
			wasmRuntimeMetricSummary(id, "wasm.active_calls", summary["active_calls"]),
		)
	}
	sort.Slice(metrics, func(i, j int) bool {
		if metrics[i].PluginID == metrics[j].PluginID {
			return metrics[i].Name < metrics[j].Name
		}
		return metrics[i].PluginID < metrics[j].PluginID
	})
	return metrics
}

func (m *Manager) liveWASMRuntimeSummaries(pluginID string) map[string]map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]map[string]any)
	for id, loaded := range m.loaded {
		if pluginID != "" && id != pluginID {
			continue
		}
		if loaded == nil || loaded.artifact.RuntimeType != RuntimeWASM {
			continue
		}
		wasmPlugin, ok := loaded.instance.(*wasmHostedPlugin)
		if !ok || wasmPlugin == nil {
			continue
		}
		out[id] = wasmPlugin.diagnosticsSummary()
	}
	return out
}

func wasmRuntimeMetricSummary(pluginID, name string, value any) CustomMetricSummary {
	n := numericMetricValue(value)
	return CustomMetricSummary{
		PluginID:   pluginID,
		Name:       name,
		Type:       "gauge",
		Count:      uint64(n),
		LastValue:  float64(n),
		Labels:     map[string]string{"runtime": RuntimeWASM},
		LastSeenAt: time.Now().Unix(),
	}
}

func numericMetricValue(value any) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case uint64:
		const maxInt64 = uint64(1<<63 - 1)
		if typed > maxInt64 {
			return int64(maxInt64)
		}
		return int64(typed)
	case float64:
		return int64(typed)
	case json.Number:
		n, _ := typed.Int64()
		return n
	default:
		return 0
	}
}

func (m *Manager) HealthCheckExternalDependency(ctx context.Context, actor, pluginID, dependency string) (ExternalDependencyHealthCheck, error) {
	pluginID = strings.TrimSpace(pluginID)
	dependency = strings.TrimSpace(dependency)
	if pluginID == "" {
		return ExternalDependencyHealthCheck{}, errors.New("plugin_id is required")
	}
	if dependency == "" {
		return ExternalDependencyHealthCheck{}, errors.New("external dependency name is required")
	}
	plugin, err := m.repo.Plugin(ctx, pluginID)
	if err != nil {
		return ExternalDependencyHealthCheck{}, err
	}
	artifactID := plugin.ActiveArtifactID
	if artifactID == "" {
		artifactID = plugin.LoadedArtifactID
	}
	if artifactID == "" {
		artifactID = plugin.DesiredArtifactID
	}
	if artifactID == "" {
		return ExternalDependencyHealthCheck{}, errors.New("plugin has no artifact to health-check")
	}
	artifact, err := m.repo.Artifact(ctx, artifactID)
	if err != nil {
		return ExternalDependencyHealthCheck{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal([]byte(artifact.MetadataJSON), &manifest); err != nil {
		return ExternalDependencyHealthCheck{}, err
	}
	declared := false
	for _, spec := range manifest.ExternalDeps {
		if spec.Name == dependency {
			declared = true
			break
		}
	}
	if !declared {
		return ExternalDependencyHealthCheck{}, fmt.Errorf("external dependency %q is not declared by manifest", dependency)
	}
	summary, healthErr := m.operations.ForPlugin(plugin.ID, artifact.ID, manifest).HealthCheckExternalDependency(ctx, dependency)
	result := ExternalDependencyHealthCheck{
		PluginID:  plugin.ID,
		Name:      dependency,
		OK:        healthErr == nil,
		Summary:   summary,
		CheckedBy: actor,
		CheckedAt: m.repo.now().Unix(),
	}
	status := "succeeded"
	message := "external dependency health check succeeded"
	if healthErr != nil {
		status = "failed"
		message = "external dependency health check failed"
		result.Error = redactSensitive(healthErr.Error())
	}
	_ = m.repo.RecordOperation(ctx, plugin.ID, artifact.ID, "external_dependency_health_check", status, actor, message, map[string]any{
		"dependency":    dependency,
		"ok":            result.OK,
		"status":        summary.LastStatus,
		"circuit_state": summary.CircuitState,
	})
	return result, nil
}

func (m *Manager) PluginRolloutStatus(ctx context.Context, pluginID string) (PluginRolloutStatus, error) {
	plugin, err := m.repo.Plugin(ctx, pluginID)
	if err != nil {
		return PluginRolloutStatus{}, err
	}
	states, err := m.repo.ListPluginNodeRuntime(ctx, pluginID, DefaultPluginNodeStaleAfter)
	if err != nil {
		return PluginRolloutStatus{}, err
	}
	status := PluginRolloutStatus{
		PluginID:                   plugin.ID,
		DesiredState:               plugin.DesiredState,
		DesiredArtifactID:          plugin.DesiredArtifactID,
		DesiredGeneration:          plugin.DesiredGeneration,
		OK:                         plugin.DesiredState != DesiredEnabled || len(states) > 0,
		NodeRuntimeStates:          states,
		ArtifactDistributionMode:   "local-content-store",
		ArtifactDistributionStatus: "not_required",
		CrossNodeApply:             false,
	}
	if plugin.DesiredArtifactID != "" {
		artifact, err := m.repo.Artifact(ctx, plugin.DesiredArtifactID)
		if err != nil {
			status.ArtifactDistributionStatus = "error"
			status.ArtifactDistributionError = err.Error()
			status.OK = false
		} else if artifact.RuntimeType == RuntimeBuiltin {
			status.ArtifactDistributionStatus = "not_required"
		} else if artifact.ArtifactType == ArtifactTypeBinary && m.store.HasDistributionPackage(artifact) {
			status.ArtifactDistribution = true
			status.ArtifactDistributionStatus = "available"
			status.ArtifactPackageSHA256 = artifact.PackageSHA256
		} else if artifact.ArtifactType == ArtifactTypeBinary {
			status.ArtifactDistributionStatus = "missing"
			status.ArtifactDistributionError = "local distribution package is missing"
			status.OK = false
		}
	}
	for _, state := range states {
		status.NodesTotal++
		ready := pluginNodeRuntimeReady(plugin, state)
		if ready {
			status.NodesReady++
		}
		if state.Stale {
			status.NodesStale++
		}
		failed := state.RuntimeState == RuntimeFailed || state.Error != "" || !ready
		if failed {
			status.NodesFailed++
			status.OK = false
			status.PartialFailure = true
		}
	}
	status.CrossNodeApply = status.NodesTotal > 1
	return status, nil
}

func pluginNodeRuntimeReady(plugin PluginRecord, state PluginNodeRuntimeState) bool {
	if state.Stale {
		return false
	}
	switch plugin.DesiredState {
	case DesiredEnabled:
		return state.Enabled &&
			state.RuntimeState == RuntimeEnabled &&
			state.ArtifactID == plugin.DesiredArtifactID &&
			state.AppliedGeneration == plugin.DesiredGeneration
	case DesiredDisabled:
		return state.RuntimeState == RuntimeDisabled && !state.Enabled
	case DesiredDeleted:
		return false
	default:
		return false
	}
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

func (m *Manager) CancelBackgroundTask(ctx context.Context, actor, pluginID, taskID string) (BackgroundTaskSummary, error) {
	summary, err := m.operations.CancelTask(pluginID, taskID)
	if err != nil {
		_ = m.repo.RecordOperation(ctx, pluginID, "", "background_task_cancel", "failed", actor, err.Error(), map[string]any{"task_id": taskID})
		return BackgroundTaskSummary{}, err
	}
	_ = m.repo.RecordOperation(ctx, pluginID, "", "background_task_cancel", "succeeded", actor, "background task canceled", map[string]any{"task_id": taskID})
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
	if liveSummary := m.liveWASMRuntimeSummaries(pluginID)[pluginID]; liveSummary != nil {
		if data, err := json.Marshal(liveSummary); err == nil {
			plugin.RuntimeSummaryJSON = string(data)
		}
	}
	plan := m.DispatchPlan(ctx)
	var handlers []DispatchHandlerSummary
	for _, handler := range plan.Handlers {
		if handler.PluginID == pluginID {
			handlers = append(handlers, handler)
		}
	}
	builds, _ := m.repo.ListBuilds(ctx, pluginID)
	gc, _ := m.operations.GCCandidates(ctx, pluginID)
	data, summary, err := m.operations.DiagnosticPackage(ctx, plugin, artifact, manifest, handlers, builds, gc)
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
	if proxyConn.draining {
		handler.drainingProxy.Add(1)
	}
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
	var conns []*proxyConnection
	m.proxyMu.Lock()
	for _, conn := range m.proxyConns {
		if conn.pluginID == pluginID && conn.draining {
			if conn.handler != nil && !conn.forceCloseRequested {
				conn.handler.proxyForceClosed.Add(1)
			}
			conn.forceCloseRequested = true
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
	// 代理连接结束时汇总字节数和耗时，供 Admin UI 展示插件代理健康情况。
	m.proxyMu.Lock()
	proxyConn := m.proxyConns[id]
	delete(m.proxyConns, id)
	m.proxyMu.Unlock()
	if proxyConn == nil || proxyConn.handler == nil {
		return
	}
	proxyConn.handler.activeProxy.Add(-1)
	if proxyConn.draining {
		proxyConn.handler.drainingProxy.Add(-1)
	}
	proxyConn.handler.proxyCompleted.Add(1)
	if stats.Err != nil {
		proxyConn.handler.proxyErrors.Add(1)
		proxyConn.handler.lastProxyError.Store(stats.Err.Error())
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
	if process := m.takePendingHostStopIfDrained(proxyConn.pluginID); process != nil {
		go m.stopPendingProcessHost(proxyConn.pluginID, process)
	}
}

func (m *Manager) takePendingHostStopIfDrained(pluginID string) *PluginHostSupervisorProcess {
	m.proxyMu.Lock()
	active := 0
	for _, conn := range m.proxyConns {
		if conn.pluginID == pluginID {
			active++
		}
	}
	m.proxyMu.Unlock()
	if active > 0 {
		return nil
	}
	m.hostMu.Lock()
	defer m.hostMu.Unlock()
	process := m.pendingHostStops[pluginID]
	delete(m.pendingHostStops, pluginID)
	return process
}

func (m *Manager) stopPendingProcessHost(pluginID string, process *PluginHostSupervisorProcess) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = process.Stop(ctx)
	m.markHostStopped(pluginID, process.ArtifactID, process)
}

// loadLocked 加载或复用插件实例。调用方必须持有 m.mu，确保 loaded 缓存和
// 运行态标记不会与 Enable/Disable/Reconcile 并发冲突。
func (m *Manager) loadLocked(ctx context.Context, pluginRecord PluginRecord) (*loadedPlugin, error) {
	if loaded := m.loaded[pluginRecord.ID]; loaded != nil &&
		loaded.artifact.ID == pluginRecord.DesiredArtifactID &&
		loaded.record.DesiredGeneration == pluginRecord.DesiredGeneration {
		if m.serviceMode == PluginServiceModeGoPluginProcess && loaded.runtime.HostProcess != nil {
			if summary := loaded.runtime.HostProcess.Summary(); summary.CrashLoop {
				m.markHostStarted(pluginRecord.ID, loaded.artifact.ID, loaded.runtime.HostProcess)
				delete(m.loaded, pluginRecord.ID)
			} else {
				// 同一制品、同一期望代数已经加载且 host 仍健康时直接复用，
				// 避免重复 Init 和重复注册任务。
				return loaded, nil
			}
		} else {
			// 同一制品、同一期望代数已经加载时直接复用，避免重复 Init 和重复注册任务。
			return loaded, nil
		}
	}
	artifact, err := m.repo.Artifact(ctx, pluginRecord.DesiredArtifactID)
	if err != nil {
		return nil, err
	}
	if err := m.validateArtifactGate(artifact); err != nil {
		_ = m.repo.MarkRuntime(ctx, pluginRecord.ID, RuntimeFailed, "", "", pluginRecord.AppliedGeneration, err.Error(), map[string]any{"error": err.Error()}, nil)
		m.recordPluginNodeRuntimeStateLocked(ctx, pluginRecord.ID)
		return nil, err
	}

	var manifest Manifest
	if err := json.Unmarshal([]byte(artifact.MetadataJSON), &manifest); err != nil {
		_ = m.repo.MarkRuntime(ctx, pluginRecord.ID, RuntimeFailed, "", "", pluginRecord.AppliedGeneration, err.Error(), map[string]any{"error": err.Error()}, nil)
		m.recordPluginNodeRuntimeStateLocked(ctx, pluginRecord.ID)
		return nil, err
	}
	gateway := NewGateway(pluginRecord.ID, m.handleConn, m.wg, m.operations.ForPlugin(pluginRecord.ID, artifact.ID, manifest))
	runtimeInstance, err := m.startRuntimeInstance(ctx, artifact, pluginRecord, gateway)
	if err != nil {
		_ = m.repo.MarkRuntime(ctx, pluginRecord.ID, RuntimeFailed, "", "", pluginRecord.AppliedGeneration, err.Error(), map[string]any{"error": err.Error()}, nil)
		m.recordPluginNodeRuntimeStateLocked(ctx, pluginRecord.ID)
		return nil, err
	}
	instance := runtimeInstance.Plugin
	handlers := buildHandlers(pluginRecord, artifact, gateway)
	extensions := buildExtensions(pluginRecord, artifact, gateway)
	// 钩子和扩展是从 gateway 注册记录中构建出来的；插件 Init 期间完成注册。
	loaded := &loadedPlugin{
		record:     pluginRecord,
		artifact:   artifact,
		instance:   instance,
		runtime:    runtimeInstance,
		gateway:    gateway,
		handlers:   handlers,
		extensions: extensions,
	}
	if wasmPlugin, ok := instance.(*wasmHostedPlugin); ok && wasmPlugin != nil {
		wasmPlugin.onRepeatedTrapQuarantine = m.handleWASMRepeatedTrapQuarantine
	}
	m.loaded[pluginRecord.ID] = loaded
	if err := m.repo.MarkRuntime(ctx, pluginRecord.ID, RuntimeLoaded, "", artifact.ID, pluginRecord.AppliedGeneration, "", m.loadedRuntimeSummary(loaded), loaded.dispatchSummaries()); err != nil {
		return nil, err
	}
	m.recordPluginNodeRuntimeStateLocked(ctx, pluginRecord.ID)
	m.operations.StartTasks(pluginRecord.ID)
	return loaded, nil
}

func (m *Manager) startRuntimeInstance(ctx context.Context, artifact ArtifactRecord, pluginRecord PluginRecord, gateway *Gateway) (RuntimeInstance, error) {
	if m.serviceMode == PluginServiceModeGoPluginProcess {
		now := time.Now().Unix()
		m.hostMu.Lock()
		host := m.hosts[pluginRecord.ID]
		if host != nil && host.BackoffUntil > now && (host.ArtifactID == "" || host.ArtifactID == artifact.ID) {
			backoffUntil := host.BackoffUntil
			lastError := host.LastError
			m.hostMu.Unlock()
			return RuntimeInstance{}, fmt.Errorf("plugin-host crash loop backoff active for plugin %q until %s (%ds remaining): %s", pluginRecord.ID, time.Unix(backoffUntil, 0).UTC().Format(time.RFC3339), backoffUntil-now, lastError)
		}
		m.hostMu.Unlock()
	}
	adapter := m.runtimeAdapterForArtifact(artifact)
	if lifecycle, ok := adapter.(RuntimeAdapterLifecycle); ok {
		prepared, err := lifecycle.Prepare(ctx, artifact, pluginRecord)
		if err != nil {
			return RuntimeInstance{}, err
		}
		instance, err := lifecycle.Start(ctx, prepared, artifact, pluginRecord, gateway)
		if err != nil {
			return RuntimeInstance{}, err
		}
		if instance.Plugin == nil {
			return RuntimeInstance{}, errors.New("runtime adapter returned nil plugin instance")
		}
		return instance, nil
	}
	instance, err := adapter.Load(ctx, artifact, pluginRecord, gateway)
	if err != nil {
		return RuntimeInstance{}, err
	}
	if instance == nil {
		return RuntimeInstance{}, errors.New("runtime adapter returned nil plugin instance")
	}
	return RuntimeInstance{
		RuntimePrepared: RuntimePrepared{
			PluginID:   pluginRecord.ID,
			ArtifactID: artifact.ID,
			Runtime:    artifact.RuntimeType,
			Mode:       m.serviceMode,
			PreparedAt: time.Now().Unix(),
		},
		Plugin:    instance,
		StartedAt: time.Now().Unix(),
	}, nil
}

func (m *Manager) runtimeAdapterForArtifact(artifact ArtifactRecord) RuntimeAdapter {
	adapter := m.adapter
	if m.adapterManaged {
		adapter, _ = RuntimeAdapterFactory{Facts: m.RuntimeFeatureFactsOptions()}.AdapterFor(m.serviceMode, artifact.RuntimeType)
		switch typed := adapter.(type) {
		case GoPluginProcessAdapter:
			switch configured := m.adapter.(type) {
			case GoPluginProcessAdapter:
				typed.Supervisor = configured.Supervisor
			case *GoPluginProcessAdapter:
				if configured != nil {
					typed.Supervisor = configured.Supervisor
				}
			}
			adapter = typed
		case SandboxProcessAdapter:
			typed.Policy = m.sandboxPolicy
			typed.Secrets = m
			adapter = typed
		case WASMAdapter:
			typed.Runner = m.wasmRunner
			adapter = typed
		}
	}
	return adapter
}

// validateArtifactGate 确认制品能被当前网关进程加载。Go plugin 对 Go 版本和
// 目标平台敏感；沙箱保留在未来服务模式下，WASM 只允许低风险扩展点。
func (m *Manager) validateArtifactGate(artifact ArtifactRecord) error {
	if artifact.Status == ArtifactStatusDeleted || artifact.Status == ArtifactStatusRejected {
		return fmt.Errorf("artifact status %q is not loadable", artifact.Status)
	}
	if artifact.ArtifactType != ArtifactTypeBinary {
		return errors.New("desired artifact must be a binary artifact")
	}
	if artifact.RuntimeType == RuntimeBuiltin {
		return nil
	}
	if artifact.RuntimeType == RuntimeSandbox {
		if m.serviceMode != PluginServiceModeSandboxProcess {
			return errors.New("sandbox-process runtime is disabled by plugin service mode")
		}
		if _, message := m.validateSandboxServiceModeApply(); message != "" {
			return errors.New(message)
		}
		if caps := requiredRuntimeCapabilities(artifact); len(caps) > 0 {
			return fmt.Errorf("sandbox-process cannot enforce required capabilities: %s", strings.Join(caps, ","))
		}
		return nil
	}
	if artifact.RuntimeType == RuntimeWASM {
		if caps := requiredRuntimeCapabilities(artifact); len(caps) > 0 {
			return fmt.Errorf("wasm runtime cannot enforce required capabilities: %s", strings.Join(caps, ","))
		}
		var manifest Manifest
		if err := json.Unmarshal([]byte(artifact.MetadataJSON), &manifest); err != nil {
			return fmt.Errorf("decode wasm manifest: %w", err)
		}
		if blocked := wasmBlockedHostCapabilities(manifest, artifact); len(blocked) > 0 {
			return fmt.Errorf("wasm runtime does not support host capabilities: %s", strings.Join(blocked, ","))
		}
		if err := validateWASMExtensionPoints(manifest); err != nil {
			return err
		}
		if err := validateWASMArtifactABI(context.Background(), artifact, manifest); err != nil {
			return err
		}
		return nil
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

func (m *Manager) reloadLoadedRuntime(ctx context.Context, actor string, plugin PluginRecord, reason string) error {
	if plugin.ID == "" || plugin.DesiredArtifactID == "" {
		return nil
	}
	m.mu.Lock()
	loaded := m.loaded[plugin.ID]
	if loaded == nil || loaded.artifact.ID != plugin.DesiredArtifactID {
		metadata := map[string]any{
			"reload_reason":      reason,
			"runtime_loaded":     loaded != nil,
			"desired_generation": plugin.DesiredGeneration,
			"active_changed":     false,
		}
		if loaded != nil {
			metadata["loaded_artifact_id"] = loaded.artifact.ID
		}
		m.mu.Unlock()
		for key, value := range m.wasmArtifactMetadata(ctx, plugin.DesiredArtifactID) {
			metadata[key] = value
		}
		_ = m.repo.RecordOperation(ctx, plugin.ID, plugin.DesiredArtifactID, "reload", "skipped", actor, "runtime reload skipped", metadata)
		return nil
	}
	adapter := m.runtimeAdapterForArtifact(loaded.artifact)
	lifecycle, ok := adapter.(RuntimeAdapterLifecycle)
	metadata := runtimeReloadMetadata(loaded, plugin, reason)
	if !ok {
		m.mu.Unlock()
		_ = m.repo.RecordOperation(ctx, plugin.ID, loaded.artifact.ID, "reload", "skipped", actor, "runtime adapter does not support reload", metadata)
		return nil
	}
	if err := lifecycle.ReloadConfig(ctx, loaded.runtime, plugin.ConfigJSON); err != nil {
		message := reloadErrorMessage(loaded.artifact, plugin.ConfigJSON, err)
		metadata["last_error"] = message
		m.mu.Unlock()
		_ = m.repo.RecordOperation(ctx, plugin.ID, loaded.artifact.ID, "reload", "failed", actor, message, metadata)
		return err
	}
	loaded.record = plugin
	if wasmPlugin, ok := loaded.instance.(*wasmHostedPlugin); ok && wasmPlugin != nil {
		wasmPlugin.mu.Lock()
		wasmPlugin.artifactGeneration = plugin.DesiredGeneration
		wasmPlugin.mu.Unlock()
	}
	summary := m.loadedRuntimeSummary(loaded)
	if loaded.runtime.HostProcess != nil {
		summary["plugin_host"] = m.hostSummary(loaded.record.ID)
	}
	err := m.repo.MarkRuntime(ctx, plugin.ID, RuntimeEnabled, loaded.artifact.ID, loaded.artifact.ID, plugin.DesiredGeneration, "", summary, loaded.dispatchSummaries())
	if err == nil {
		m.recordPluginNodeRuntimeStateLocked(ctx, plugin.ID)
	}
	m.mu.Unlock()
	if err != nil {
		metadata["last_error"] = redactSensitive(err.Error())
		_ = m.repo.RecordOperation(ctx, plugin.ID, loaded.artifact.ID, "reload", "failed", actor, "runtime reload state update failed", metadata)
		return err
	}
	metadata["applied_generation"] = plugin.DesiredGeneration
	_ = m.repo.RecordOperation(ctx, plugin.ID, loaded.artifact.ID, "reload", "succeeded", actor, "runtime config reloaded", metadata)
	return nil
}

func runtimeReloadMetadata(loaded *loadedPlugin, plugin PluginRecord, reason string) map[string]any {
	metadata := map[string]any{
		"reload_reason":      reason,
		"desired_generation": plugin.DesiredGeneration,
		"active_changed":     true,
		"handler_count":      len(loaded.handlers),
		"extension_count":    loaded.extensions.count(),
	}
	for key, value := range wasmLifecycleMetadata(loaded) {
		metadata[key] = value
	}
	return metadata
}

func reloadErrorMessage(artifact ArtifactRecord, configJSON string, err error) string {
	message := err.Error()
	var manifest Manifest
	if json.Unmarshal([]byte(artifact.MetadataJSON), &manifest) == nil {
		message = redactSensitiveConfigText(message, configJSON, sensitiveConfigPaths(manifest.ConfigSchema, configJSON))
	}
	return redactSensitive(message)
}

func (m *Manager) markEnabled(ctx context.Context, loaded *loadedPlugin) error {
	m.operations.StartTasks(loaded.record.ID)
	m.markHostStarted(loaded.record.ID, loaded.artifact.ID, loaded.runtime.HostProcess)
	summary := m.loadedRuntimeSummary(loaded)
	summary["plugin_host"] = m.hostSummary(loaded.record.ID)
	if err := m.repo.MarkRuntime(ctx, loaded.record.ID, RuntimeEnabled, loaded.artifact.ID, loaded.artifact.ID, loaded.record.DesiredGeneration, "", summary, loaded.dispatchSummaries()); err != nil {
		return err
	}
	m.recordPluginNodeRuntimeStateLocked(ctx, loaded.record.ID)
	return nil
}

func (m *Manager) loadedRuntimeSummary(loaded *loadedPlugin) map[string]any {
	summary := map[string]any{
		"handler_count":   len(loaded.handlers),
		"extension_count": loaded.extensions.count(),
		"service_mode":    m.serviceMode,
		"runtime_type":    loaded.artifact.RuntimeType,
	}
	if wasmPlugin, ok := loaded.instance.(*wasmHostedPlugin); ok && wasmPlugin != nil {
		for key, value := range wasmPlugin.diagnosticsSummary() {
			summary[key] = value
		}
	}
	return summary
}

func wasmLifecycleMetadata(loaded *loadedPlugin) map[string]any {
	if loaded == nil || loaded.artifact.RuntimeType != RuntimeWASM {
		return nil
	}
	metadata := map[string]any{
		"runtime_type": RuntimeWASM,
	}
	if wasmPlugin, ok := loaded.instance.(*wasmHostedPlugin); ok && wasmPlugin != nil {
		summary := wasmPlugin.diagnosticsSummary()
		for _, key := range []string{
			"runtime_abi", "host_abi", "module_hash", "module_cache_status",
			"handler_timeout_ms", "memory_bytes", "supported_extensions",
			"call_count", "trap_count", "timeout_count", "memory_error_count",
			"active_calls", "quarantined", "quarantine_reason",
			"last_extension_point", "last_error", "limits",
		} {
			if value, ok := summary[key]; ok {
				metadata[key] = value
			}
		}
	}
	return metadata
}

func (m *Manager) wasmArtifactMetadata(ctx context.Context, artifactID string) map[string]any {
	artifact, err := m.repo.Artifact(ctx, artifactID)
	if err != nil || artifact.RuntimeType != RuntimeWASM {
		return nil
	}
	var manifest Manifest
	_ = json.Unmarshal([]byte(artifact.MetadataJSON), &manifest)
	metadata := map[string]any{
		"runtime_type": RuntimeWASM,
		"runtime_abi":  manifest.Runtime.ABI,
		"module_hash":  artifact.SHA256,
	}
	if artifactMetadata := jsonMapFromJSONString(artifact.MetadataJSON); artifactMetadata != nil {
		if wasmMetadata := jsonMapFromAny(artifactMetadata["wasm"]); wasmMetadata != nil {
			if moduleHash := diagnosticStringFromAny(wasmMetadata["module_sha256"]); moduleHash != "" {
				metadata["module_hash"] = moduleHash
			}
		}
	}
	if len(manifest.ExtensionPoints) > 0 {
		metadata["supported_extensions"] = wasmManifestExtensionKeys(manifest)
	}
	return metadata
}

func (m *Manager) wasmValidationFailureMetadata(ctx context.Context, artifactID string, err error) map[string]any {
	metadata := m.wasmArtifactMetadata(ctx, artifactID)
	if len(metadata) == 0 {
		return nil
	}
	code := wasmABIErrorCode(err)
	metadata["validation_failure"] = true
	if code != "" {
		metadata["wasm_error_code"] = code
	}
	if code == wasmABIErrorABIMismatch || strings.Contains(strings.ToLower(err.Error()), "abi") {
		metadata["abi_mismatch"] = true
	}
	return metadata
}

func (m *Manager) recordPluginNodeRuntimeState(ctx context.Context, pluginID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recordPluginNodeRuntimeStateLocked(ctx, pluginID)
}

// recordPluginNodeRuntimeStateLocked records this manager node's runtime truth.
// Callers must hold m.mu so a local crash-loop cannot overwrite another healthy
// node's desired/active expression through the shared plugin row.
func (m *Manager) recordPluginNodeRuntimeStateLocked(ctx context.Context, pluginID string) {
	plugin, err := m.repo.Plugin(ctx, pluginID)
	if err != nil {
		return
	}
	artifactID := plugin.ActiveArtifactID
	if artifactID == "" {
		artifactID = plugin.LoadedArtifactID
	}
	nodeID := m.nodeID
	if nodeID == "" && m.operations != nil {
		nodeID = m.operations.nodeID
	}
	if nodeID == "" {
		nodeID = defaultOperationsNodeID()
	}
	state := PluginNodeRuntimeState{
		NodeID:            nodeID,
		PluginID:          plugin.ID,
		ArtifactID:        artifactID,
		DesiredState:      plugin.DesiredState,
		RuntimeState:      plugin.RuntimeState,
		DesiredGeneration: plugin.DesiredGeneration,
		AppliedGeneration: plugin.AppliedGeneration,
		Loaded:            plugin.LoadedArtifactID != "",
		Enabled:           plugin.RuntimeState == RuntimeEnabled,
		Health:            pluginNodeHealth(plugin),
		Error:             plugin.LastError,
	}
	if loaded := m.loaded[pluginID]; loaded != nil {
		state.ArtifactID = loaded.artifact.ID
		state.DesiredState = loaded.record.DesiredState
		state.DesiredGeneration = loaded.record.DesiredGeneration
		state.AppliedGeneration = loaded.record.DesiredGeneration
		state.Loaded = true
		state.Enabled = true
		state.RuntimeState = RuntimeEnabled
		state.Health = RuntimeEnabled
		state.Error = ""
		if loaded.runtime.HostProcess != nil {
			summary := loaded.runtime.HostProcess.Summary()
			if summary.State != "" {
				state.RuntimeState = summary.State
			}
			if summary.State == RuntimeFailed || summary.CrashLoop || summary.LastError != "" {
				state.Enabled = false
				state.Health = RuntimeFailed
				state.Error = summary.LastError
			}
		}
	}
	m.hostMu.Lock()
	host := m.hosts[pluginID]
	if host != nil && host.State != "" && host.State != RuntimeNotLoaded {
		state.ArtifactID = host.ArtifactID
		state.RuntimeState = host.State
		state.Loaded = host.State != RuntimeDisabled
		state.Enabled = host.State == RuntimeEnabled
		state.Health = host.State
		state.Error = host.LastError
	}
	m.hostMu.Unlock()
	_ = m.repo.UpsertPluginNodeRuntime(ctx, PluginNodeRuntimeState{
		NodeID:            state.NodeID,
		PluginID:          state.PluginID,
		ArtifactID:        state.ArtifactID,
		DesiredState:      state.DesiredState,
		RuntimeState:      state.RuntimeState,
		DesiredGeneration: state.DesiredGeneration,
		AppliedGeneration: state.AppliedGeneration,
		Loaded:            state.Loaded,
		Enabled:           state.Enabled,
		Health:            state.Health,
		Error:             state.Error,
	})
}

func (m *Manager) deletePluginNodeRuntimeState(ctx context.Context, pluginID string) {
	nodeID := m.nodeID
	if nodeID == "" && m.operations != nil {
		nodeID = m.operations.nodeID
	}
	if nodeID == "" {
		return
	}
	_ = m.repo.DeletePluginNodeRuntime(ctx, nodeID, pluginID)
}

func pluginNodeHealth(plugin PluginRecord) string {
	if plugin.LastError != "" || plugin.RuntimeState == RuntimeFailed {
		return RuntimeFailed
	}
	switch plugin.RuntimeState {
	case RuntimeEnabled:
		return "healthy"
	case RuntimeLoaded:
		return RuntimeLoaded
	case RuntimeDraining:
		return RuntimeDraining
	case RuntimeDisabled:
		return RuntimeDisabled
	default:
		return plugin.RuntimeState
	}
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
		if conn.pluginID == pluginID && !conn.draining {
			conn.draining = true
			if conn.handler != nil {
				conn.handler.drainingProxy.Add(1)
			}
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

func requiredRuntimeCapabilities(artifact ArtifactRecord) []string {
	var summary CapabilitySummary
	if json.Unmarshal([]byte(artifact.CapabilitiesSummaryJSON), &summary) != nil {
		return nil
	}
	return uniqueSortedStrings(summary.Runtime.RequiredCapabilities)
}

func runtimeRequiredCapabilitiesUnsupported(runtimeType string) bool {
	switch runtimeType {
	case RuntimeSandbox, RuntimeWASM:
		return true
	default:
		return false
	}
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

func runProtocolProxy(ctx context.Context, handle *ProxyConnectionHandle, client, endpoint net.Conn, initialBytesToPlugin int64) {
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
	stats.BytesToPlugin = initialBytesToPlugin
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

// ValidateConfigSchema 复用管理端 dry-run 的 JSON schema 子集校验，供 CLI
// conformance 和其他离线工具保持同一语义。
func ValidateConfigSchema(schema json.RawMessage, configJSON string) error {
	return validateConfigSchema(schema, configJSON)
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

func redactSensitiveConfigText(text, configJSON string, sensitivePaths []string) string {
	if text == "" || len(sensitivePaths) == 0 {
		return text
	}
	for _, value := range sensitiveConfigTextValues(configJSON, sensitivePaths) {
		if value != "" {
			text = strings.ReplaceAll(text, value, "[REDACTED]")
		}
	}
	return text
}

func sensitiveConfigTextValues(configJSON string, sensitivePaths []string) []string {
	var value any
	if err := json.Unmarshal([]byte(defaultJSONObject(configJSON)), &value); err != nil {
		return nil
	}
	pathSet := make(map[string]bool, len(sensitivePaths))
	for _, path := range sensitivePaths {
		pathSet[path] = true
	}
	redacted := redactValue(value, "$", pathSet)
	seen := make(map[string]bool)
	collectRedactedTextValues(value, redacted, seen)
	values := make([]string, 0, len(seen))
	for value := range seen {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

func collectRedactedTextValues(original, redacted any, values map[string]bool) {
	switch typed := redacted.(type) {
	case string:
		if typed != "[REDACTED]" {
			return
		}
		switch raw := original.(type) {
		case string:
			if raw != "" {
				values[raw] = true
			}
		default:
			data, err := json.Marshal(raw)
			if err == nil && len(data) > 0 {
				values[string(data)] = true
			}
		}
	case map[string]any:
		rawMap, _ := original.(map[string]any)
		for key, child := range typed {
			collectRedactedTextValues(rawMap[key], child, values)
		}
	case []any:
		rawList, _ := original.([]any)
		for idx, child := range typed {
			var raw any
			if idx < len(rawList) {
				raw = rawList[idx]
			}
			collectRedactedTextValues(raw, child, values)
		}
	}
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
		lastProxyError, _ := handler.lastProxyError.Load().(string)
		summaries = append(summaries, DispatchHandlerSummary{
			PluginID:         handler.pluginID,
			ArtifactID:       handler.artifactID,
			Priority:         handler.priority,
			HandlerID:        handler.handlerID,
			ExtensionPoint:   ExtensionUpstreamConnect,
			Mode:             handler.mode,
			TimeoutMS:        handler.timeout.Milliseconds(),
			Calls:            handler.calls.Load(),
			Errors:           handler.errors.Load(),
			Panics:           handler.panics.Load(),
			Timeouts:         handler.timeouts.Load(),
			Blocked:          handler.blocked.Load(),
			ActiveProxy:      handler.activeProxy.Load(),
			DrainingProxy:    handler.drainingProxy.Load(),
			ProxyStarted:     handler.proxyStarted.Load(),
			ProxyCompleted:   handler.proxyCompleted.Load(),
			ProxyForceClosed: handler.proxyForceClosed.Load(),
			ProxyErrors:      handler.proxyErrors.Load(),
			LastProxyError:   lastProxyError,
			ProxyBytesIn:     handler.proxyBytesIn.Load(),
			ProxyBytesOut:    handler.proxyBytesOut.Load(),
			ProxyDurationMS:  handler.proxyDuration.Load(),
			DurationCount:    handler.durationCount.Load(),
			DurationSumMS:    handler.durationSumMS.Load(),
			DurationMaxMS:    handler.durationMaxMS.Load(),
		})
	}
	return summaries
}
