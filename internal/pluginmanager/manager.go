// internal/pluginmanager/manager.go 协调插件记录、制品加载、钩子分发快照和生命周期迁移。

package pluginmanager

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/tursom/mc-gateway/plugin/official/trustedrealip"
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
	if artifact.RuntimeType == RuntimeBuiltin {
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
	descriptor, ok := builtinPluginDescriptorByID(artifact.PluginID)
	if !ok {
		return nil, fmt.Errorf("unknown builtin plugin %q", artifact.PluginID)
	}
	instance := descriptor.factory()
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

type builtinPluginDescriptor struct {
	manifest          Manifest
	conformancePassed int
	factory           func() api.Plugin
}

func builtinPluginDescriptors() []builtinPluginDescriptor {
	return []builtinPluginDescriptor{
		{
			manifest: Manifest{
				SchemaVersion: SchemaVersion,
				ID:            "official.rule-policy",
				Name:          "Official Rule Policy",
				Version:       "0.1.0",
				Description:   "Built-in official rule/policy extension for host rewrite, CIDR policy, rate limit, maintenance mode and upstream rewrite.",
				ArtifactType:  ArtifactTypeBinary,
				Runtime:       RuntimeManifest{Type: RuntimeBuiltin},
				APIVersion:    APIVersion,
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
			},
			conformancePassed: 5,
			factory:           func() api.Plugin { return rulepolicy.New() },
		},
		{
			manifest: Manifest{
				SchemaVersion: SchemaVersion,
				ID:            "official.trusted-real-ip",
				Name:          "Official Trusted Real IP",
				Version:       "0.1.0",
				Description:   "Uses a trusted WebSocket proxy header as the effective Minecraft client address.",
				ArtifactType:  ArtifactTypeBinary,
				Runtime:       RuntimeManifest{Type: RuntimeBuiltin},
				APIVersion:    APIVersion,
				ExtensionPoints: []ExtensionPoint{
					{Type: "hook", Key: ExtensionUpstreamConnect},
				},
				Capabilities:  json.RawMessage(`{"extension_points":["upstream.connect/v2"]}`),
				RuntimeLimits: RuntimeLimits{HandlerTimeoutMS: int(DefaultHandlerTimeout / time.Millisecond)},
				ConfigSchema:  json.RawMessage(`{"type":"object","properties":{"header":{"type":"string","minLength":1,"default":"X-Real-IP"},"trusted_peers":{"type":"array","minItems":1,"items":{"type":"string"},"default":["127.0.0.1/32"]}}}`),
			},
			conformancePassed: 1,
			factory:           func() api.Plugin { return trustedrealip.New() },
		},
	}
}

func builtinPluginDescriptorByID(pluginID string) (builtinPluginDescriptor, bool) {
	for _, descriptor := range builtinPluginDescriptors() {
		if descriptor.manifest.ID == pluginID {
			return descriptor, true
		}
	}
	return builtinPluginDescriptor{}, false
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
	displaced         map[runtimeIdentity]*loadedPlugin
	snapshot          atomic.Value
	extensionSnapshot atomic.Value
	routeCacheMu      sync.Mutex
	routeCache        map[string]routeCacheEntry

	// connectionSessions 跟踪接管链的根连接及全部参与插件，用于 drain 和强制关闭。
	sessionMu          sync.Mutex
	sessionSeq         uint64
	connectionSessions map[uint64]*connectionSession
	drainingIDs        map[string]bool
	operations         *Operations

	// serviceMode/hosts 预留给插件运行时从进程内迁移到独立宿主的服务模式。
	serviceMode         string
	hostMu              sync.Mutex
	hosts               map[string]*pluginHostProcess
	pendingRuntimeStops map[runtimeIdentity]*loadedPlugin
	runtimeCleanupMu    sync.Mutex
	runtimeCleanupWG    sync.WaitGroup

	feedSchedulerMu     sync.Mutex
	feedSchedulerCancel context.CancelFunc
	feedSchedulerDone   chan struct{}

	closing   atomic.Bool
	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

// loadedPlugin 是内存中的插件实例和它注册的扩展快照。数据库记录说明期望状态，
// loadedPlugin 说明当前进程实际已经加载了什么。
type loadedPlugin struct {
	record       PluginRecord
	artifact     ArtifactRecord
	instance     api.Plugin
	runtime      RuntimeInstance
	gateway      *Gateway
	operations   *PluginOperations
	handlers     []*upstreamHandler
	extensions   pluginExtensions
	taskStopOnce sync.Once
	taskStopErr  error
}

type runtimeIdentity struct {
	pluginID          string
	artifactID        string
	desiredGeneration int64
	runtimeInstanceID string
}

func loadedRuntimeIdentity(loaded *loadedPlugin) runtimeIdentity {
	if loaded == nil {
		return runtimeIdentity{}
	}
	return runtimeIdentity{
		pluginID:          loaded.record.ID,
		artifactID:        loaded.artifact.ID,
		desiredGeneration: loaded.record.DesiredGeneration,
		runtimeInstanceID: loaded.runtime.RuntimeInstanceID,
	}
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
	pluginID   string
	artifactID string
	runtime    runtimeIdentity
	priority   int
	handlerID  string
	handle     api.UpstreamConnectHandlerV2

	calls               atomic.Uint64
	errors              atomic.Uint64
	panics              atomic.Uint64
	timeouts            atomic.Uint64
	blocked             atomic.Uint64
	activeSessions      atomic.Int64
	drainingSessions    atomic.Int64
	sessionsStarted     atomic.Uint64
	sessionsCompleted   atomic.Uint64
	sessionErrors       atomic.Uint64
	sessionDuration     atomic.Uint64
	sessionsForceClosed atomic.Uint64
	durationCount       atomic.Uint64
	durationSumMS       atomic.Uint64
	durationMaxMS       atomic.Uint64
	lastSessionError    atomic.Value
	draining            atomic.Bool
	runtimeRefs         atomic.Int64
}

type connectionSession struct {
	id                  uint64
	ctx                 context.Context
	cancel              context.CancelFunc
	streams             []net.Conn
	startedAt           time.Time
	participants        map[*upstreamHandler]bool
	draining            map[string]bool
	forceCloseRequested bool
}

type Options struct {
	DB                        *sql.DB
	ArtifactRoot              string
	Now                       func() time.Time
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
		wg:                        options.WaitGroup,
		policyProfile:             options.PolicyProfile,
		requireConformanceFixture: options.RequireConformanceFixture,
		loaded:                    make(map[string]*loadedPlugin),
		displaced:                 make(map[runtimeIdentity]*loadedPlugin),
		ingressReservedListeners:  append([]IngressReservedListener(nil), options.IngressReservedListeners...),
		futureGates:               normalizeFutureRuntimeGates(options.FutureRuntimeGates),
		sandboxPolicy:             normalizeSandboxPolicy(options.SandboxPolicy),
		sandboxSelfCheck:          options.SandboxSelfCheck,
		routeCache:                make(map[string]routeCacheEntry),
		connectionSessions:        make(map[uint64]*connectionSession),
		drainingIDs:               make(map[string]bool),
		hosts:                     make(map[string]*pluginHostProcess),
		pendingRuntimeStops:       make(map[runtimeIdentity]*loadedPlugin),
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
	for _, descriptor := range builtinPluginDescriptors() {
		manifest := descriptor.manifest
		source := "builtin:" + manifest.ID
		metadata, err := artifactMetadataJSON(manifest, ConformanceSummary{
			Source: source, OK: true, Total: descriptor.conformancePassed, Passed: descriptor.conformancePassed,
		}, true, nil)
		if err != nil {
			return err
		}
		extensionPoints, err := json.Marshal(manifest.ExtensionPoints)
		if err != nil {
			return err
		}
		summaryJSON, err := manifestCapabilitiesSummaryJSON(manifest)
		if err != nil {
			return err
		}
		artifact := ArtifactRecord{
			ID: "builtin-" + strings.ReplaceAll(manifest.ID, ".", "-") + "-" + manifest.Version, PluginID: manifest.ID, Version: manifest.Version,
			FileName: source, SHA256: source + ":" + manifest.Version, PackageSHA256: source + ":" + manifest.Version,
			ArtifactType: ArtifactTypeBinary, RuntimeType: RuntimeBuiltin, Status: ArtifactStatusLoadable,
			MetadataJSON: string(metadata), CapabilitiesSummaryJSON: string(summaryJSON), ExtensionPointsJSON: string(extensionPoints),
			APIVersion: APIVersion, UploadedBy: actor, CreatedAt: now, UpdatedAt: now,
		}
		if err := m.repo.SaveArtifact(ctx, artifact); err != nil {
			return err
		}
		_ = m.repo.RecordOperation(ctx, manifest.ID, artifact.ID, "official_plugin_register", "succeeded", actor, "official builtin plugin registered", nil)
	}
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
	if m.closing.Load() {
		return PluginRecord{}, ErrManagerClosed
	}
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
		dryRunAdapter = m.runtimeAdapterForArtifact(artifact)
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
	if m.closing.Load() {
		return PluginRecord{}, ErrManagerClosed
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	// Load 只把插件实例化到内存并登记为 loaded，不发布到热路径。
	// 管理端可用它验证制品和配置，而不立即影响在线连接。
	pluginRecord, err := m.repo.Plugin(ctx, pluginID)
	if err != nil {
		return PluginRecord{}, err
	}
	loaded, replaced, err := m.loadLocked(ctx, pluginRecord)
	if err != nil {
		_ = m.repo.RecordOperation(ctx, pluginID, pluginRecord.DesiredArtifactID, "load", "failed", actor, err.Error(), map[string]any{"reason_code": reasonCodeFromError(err)})
		return PluginRecord{}, err
	}
	if replaced != nil {
		if m.pluginPublishedLocked(pluginID) && len(m.displacedPluginsLocked(pluginID)) == 0 {
			m.displaced[loadedRuntimeIdentity(replaced)] = replaced
		} else {
			for _, cleanupErr := range m.retireLoadedPluginLocked(ctx, replaced) {
				_ = m.repo.RecordOperation(ctx, pluginID, replaced.artifact.ID, "load_cleanup", "warning", actor, cleanupErr.Error(), nil)
			}
		}
	}
	_ = m.repo.RecordOperation(ctx, pluginID, loaded.artifact.ID, "load", "succeeded", actor, "plugin loaded", nil)
	return m.repo.Plugin(ctx, pluginID)
}

// Enable 将期望状态推进为启用，并把插件处理器发布到连接热路径。
// 发布前会先通过治理门禁，避免高风险制品绕过评审直接生效。
func (m *Manager) Enable(ctx context.Context, actor, pluginID string) (PluginRecord, error) {
	if m.closing.Load() {
		return PluginRecord{}, ErrManagerClosed
	}
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
		artifact, _ := m.repo.Artifact(ctx, pluginRecord.DesiredArtifactID)
		summary := runtimeFailureSummary(artifact, err)
		summary["governance"] = decision
		_, _ = m.repo.MarkRuntimeIfDesiredGeneration(ctx, pluginID, pluginRecord.DesiredGeneration, RuntimeFailed, "", "", pluginRecord.AppliedGeneration, err.Error(), summary, nil)
		m.recordPluginNodeRuntimeState(ctx, pluginID)
		_ = m.repo.RecordOperation(ctx, pluginID, pluginRecord.DesiredArtifactID, "enable_gate", "failed", actor, err.Error(), map[string]any{
			"decision":    decision,
			"reason_code": reasonCodeFromError(err),
		})
		return PluginRecord{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// 真正加载与发布都在同一把锁内完成，保证 snapshot、extensions 和 loaded
	// 三类内存状态不会被并发读到半更新结果。
	previous := m.loaded[pluginID]
	loaded, replaced, err := m.loadLocked(ctx, pluginRecord)
	if err != nil {
		_ = m.repo.RecordOperation(ctx, pluginID, pluginRecord.DesiredArtifactID, "enable", "failed", actor, err.Error(), map[string]any{"reason_code": reasonCodeFromError(err)})
		return PluginRecord{}, err
	}
	if len(loaded.handlers) == 0 && loaded.extensions.empty() {
		err := fmt.Errorf("plugin %q did not register any supported extension point", pluginID)
		_, _ = m.repo.MarkRuntimeIfDesiredGeneration(ctx, pluginID, pluginRecord.DesiredGeneration, RuntimeFailed, "", loaded.artifact.ID, pluginRecord.AppliedGeneration, err.Error(), map[string]any{"error": err.Error()}, nil)
		m.recordPluginNodeRuntimeStateLocked(ctx, pluginID)
		_ = m.repo.RecordOperation(ctx, pluginID, pluginRecord.DesiredArtifactID, "enable", "failed", actor, err.Error(), nil)
		m.rollbackLoadedCandidateLocked(ctx, pluginID, loaded, previous, replaced)
		return PluginRecord{}, err
	}

	current := m.currentHandlersLocked()
	current[pluginID] = loaded.handlers
	next := flattenHandlers(current)
	extensions := m.currentExtensionsLocked()
	extensions[pluginID] = loaded.extensions
	if err := m.markEnabled(ctx, loaded); err != nil {
		m.rollbackLoadedCandidateLocked(ctx, pluginID, loaded, previous, replaced)
		return PluginRecord{}, err
	}
	// 数据库运行态先写成功，再发布内存快照；这样 UI 看到 enabled 时，
	// 连接热路径也已经具备对应处理器。
	m.clearDrainingLocked(pluginID)
	retiring := m.takeDisplacedPluginsLocked(pluginID)
	retiring = appendUniqueLoadedPlugin(retiring, replaced)
	for _, old := range retiring {
		m.markLoadedRuntimeDrainingLocked(old)
	}
	m.publish(next)
	m.publishExtensionsLocked(extensions)
	for _, old := range retiring {
		_ = m.stopLoadedPluginTasks(old)
	}
	m.operations.ActivateRuntime(loaded.operations)
	for _, old := range retiring {
		for _, cleanupErr := range m.retireLoadedPluginLocked(ctx, old) {
			_ = m.repo.RecordOperation(ctx, pluginID, old.artifact.ID, "enable_cleanup", "warning", actor, cleanupErr.Error(), nil)
		}
	}
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
// 因此这里停止任务、移除分发入口，并等待已有连接接管 session 结束。
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
	retiring := m.takeDisplacedPluginsLocked(pluginID)
	retiring = appendUniqueLoadedPlugin(retiring, m.loaded[pluginID])
	disableMetadata := map[string]any{}
	for _, loaded := range retiring {
		for key, value := range wasmLifecycleMetadata(loaded) {
			disableMetadata[key] = value
		}
		for _, err := range m.retireLoadedPluginLocked(ctx, loaded) {
			_ = m.repo.RecordOperation(ctx, pluginID, loaded.artifact.ID, "disable", "warning", actor, err.Error(), nil)
		}
	}
	runtimeState := RuntimeDisabled
	if m.activeConnectionSessionCountLocked(pluginID) > 0 {
		runtimeState = RuntimeDraining
	}
	delete(m.loaded, pluginID)
	if _, err := m.repo.MarkRuntimeIfDesiredGeneration(ctx, pluginID, pluginRecord.DesiredGeneration, runtimeState, "", "", pluginRecord.DesiredGeneration, "", map[string]any{
		"active_connection_sessions": m.activeConnectionSessionCountLocked(pluginID),
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
	retiring := m.takeDisplacedPluginsLocked(pluginID)
	retiring = appendUniqueLoadedPlugin(retiring, m.loaded[pluginID])
	for _, loaded := range retiring {
		_ = m.retireLoadedPluginLocked(ctx, loaded)
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
	errs := m.drainAndDestroyRuntimeInstance(ctx, loaded)
	if err := m.stopRuntimeAdapter(ctx, loaded); err != nil {
		errs = append(errs, err)
	}
	return errs
}

func (m *Manager) drainAndDestroyRuntimeInstance(ctx context.Context, loaded *loadedPlugin) []error {
	var errs []error
	errs = append(errs, m.drainRuntimeInstance(ctx, loaded)...)
	if loaded.instance != nil {
		if err := loaded.instance.Destroy(); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

func (m *Manager) stopRuntimeAdapter(ctx context.Context, loaded *loadedPlugin) error {
	if lifecycle, ok := m.runtimeAdapterForArtifact(loaded.artifact).(RuntimeAdapterLifecycle); ok {
		if err := lifecycle.Stop(ctx, loaded.runtime); err != nil {
			return err
		}
	}
	if loaded.runtime.HostProcess != nil {
		m.markHostStopped(loaded.record.ID, loaded.artifact.ID, loaded.runtime.HostProcess)
	}
	return nil
}

func (m *Manager) drainRuntimeInstance(ctx context.Context, loaded *loadedPlugin) []error {
	if lifecycle, ok := m.runtimeAdapterForArtifact(loaded.artifact).(RuntimeAdapterLifecycle); ok {
		if err := lifecycle.Drain(ctx, loaded.runtime); err != nil {
			return []error{err}
		}
	}
	return nil
}

// Reconcile 根据数据库中的期望启用列表重建内存分发快照，主要用于进程启动
// 或运行态状态漂移后的自愈。
func (m *Manager) Reconcile(ctx context.Context) error {
	if m.closing.Load() {
		return ErrManagerClosed
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	desired, err := m.repo.DesiredEnabled(ctx)
	if err != nil {
		return err
	}
	nextByPlugin := make(map[string][]*upstreamHandler)
	extensionsByPlugin := make(map[string]pluginExtensions)
	var replacedPlugins []*loadedPlugin
	var enabledPlugins []*loadedPlugin
	for _, pluginRecord := range desired {
		// 单个插件失败不阻断其他插件收敛；失败会记录到 runtime_state 和操作日志。
		decision, err := m.EvaluateReleaseGate(ctx, pluginRecord.ID, pluginRecord.DesiredArtifactID, GovernanceActionEnable, m.currentPolicyProfile(), pluginRecord.ConfigJSON)
		if err != nil {
			artifact, _ := m.repo.Artifact(ctx, pluginRecord.DesiredArtifactID)
			summary := runtimeFailureSummary(artifact, err)
			summary["governance"] = decision
			_, _ = m.repo.MarkRuntimeIfDesiredGeneration(ctx, pluginRecord.ID, pluginRecord.DesiredGeneration, RuntimeFailed, "", "", pluginRecord.AppliedGeneration, err.Error(), summary, nil)
			m.recordPluginNodeRuntimeStateLocked(ctx, pluginRecord.ID)
			_ = m.repo.RecordOperation(ctx, pluginRecord.ID, pluginRecord.DesiredArtifactID, "reconcile_gate", "failed", "system", err.Error(), map[string]any{"decision": decision, "reason_code": reasonCodeFromError(err)})
			continue
		}
		loaded, replaced, err := m.loadLocked(ctx, pluginRecord)
		if err != nil {
			artifact, _ := m.repo.Artifact(ctx, pluginRecord.DesiredArtifactID)
			_, _ = m.repo.MarkRuntimeIfDesiredGeneration(ctx, pluginRecord.ID, pluginRecord.DesiredGeneration, RuntimeFailed, "", "", pluginRecord.AppliedGeneration, err.Error(), runtimeFailureSummary(artifact, err), nil)
			m.recordPluginNodeRuntimeStateLocked(ctx, pluginRecord.ID)
			_ = m.repo.RecordOperation(ctx, pluginRecord.ID, pluginRecord.DesiredArtifactID, "reconcile", "failed", "system", err.Error(), map[string]any{"reason_code": reasonCodeFromError(err)})
			continue
		}
		if replaced != nil {
			replacedPlugins = appendUniqueLoadedPlugin(replacedPlugins, replaced)
		}
		if len(loaded.handlers) == 0 && loaded.extensions.empty() {
			err := fmt.Errorf("plugin %q did not register any supported extension point", pluginRecord.ID)
			_, _ = m.repo.MarkRuntimeIfDesiredGeneration(ctx, pluginRecord.ID, pluginRecord.DesiredGeneration, RuntimeFailed, "", loaded.artifact.ID, pluginRecord.AppliedGeneration, err.Error(), map[string]any{"error": err.Error()}, nil)
			m.recordPluginNodeRuntimeStateLocked(ctx, pluginRecord.ID)
			_ = m.repo.RecordOperation(ctx, pluginRecord.ID, pluginRecord.DesiredArtifactID, "reconcile", "failed", "system", err.Error(), nil)
			continue
		}
		if err := m.markEnabled(ctx, loaded); err != nil {
			_ = m.repo.RecordOperation(ctx, pluginRecord.ID, pluginRecord.DesiredArtifactID, "reconcile", "failed", "system", err.Error(), map[string]any{"reason_code": reasonCodeFromError(err)})
			continue
		}
		nextByPlugin[pluginRecord.ID] = loaded.handlers
		extensionsByPlugin[pluginRecord.ID] = loaded.extensions
		enabledPlugins = append(enabledPlugins, loaded)
		m.clearDrainingLocked(pluginRecord.ID)
	}
	for _, displaced := range m.takeAllDisplacedPluginsLocked() {
		replacedPlugins = appendUniqueLoadedPlugin(replacedPlugins, displaced)
	}
	for _, replaced := range replacedPlugins {
		m.markLoadedRuntimeDrainingLocked(replaced)
	}
	var removedPlugins []*loadedPlugin
	for pluginID, loaded := range m.loaded {
		if _, retained := nextByPlugin[pluginID]; retained {
			continue
		}
		m.markDrainingLocked(pluginID)
		m.markHostDraining(pluginID)
		removedPlugins = append(removedPlugins, loaded)
	}
	// 所有插件都处理完后一次性发布快照，避免热路径在收敛过程中看到部分插件。
	m.publish(flattenHandlers(nextByPlugin))
	m.publishExtensionsLocked(extensionsByPlugin)
	for _, replaced := range replacedPlugins {
		_ = m.stopLoadedPluginTasks(replaced)
	}
	for _, removed := range removedPlugins {
		_ = m.stopLoadedPluginTasks(removed)
	}
	for _, loaded := range enabledPlugins {
		m.operations.ActivateRuntime(loaded.operations)
	}
	for _, replaced := range replacedPlugins {
		for _, cleanupErr := range m.retireLoadedPluginLocked(ctx, replaced) {
			_ = m.repo.RecordOperation(ctx, replaced.record.ID, replaced.artifact.ID, "reconcile_cleanup", "warning", "system", cleanupErr.Error(), nil)
		}
	}
	for _, loaded := range removedPlugins {
		pluginID := loaded.record.ID
		for _, cleanupErr := range m.retireLoadedPluginLocked(ctx, loaded) {
			_ = m.repo.RecordOperation(ctx, pluginID, loaded.artifact.ID, "reconcile_cleanup", "warning", "system", cleanupErr.Error(), nil)
		}
		if m.loaded[pluginID] == loaded {
			delete(m.loaded, pluginID)
		}
	}
	return nil
}

func (m *Manager) retireLoadedPluginLocked(ctx context.Context, loaded *loadedPlugin) []error {
	if loaded == nil {
		return nil
	}
	var errs []error
	if err := m.stopLoadedPluginTasks(loaded); err != nil {
		errs = append(errs, err)
	}
	if m.deferRuntimeRetirementIfActive(loaded) {
		return errs
	}
	errs = append(errs, m.drainAndDestroyRuntimeInstance(ctx, loaded)...)
	if err := m.stopRuntimeAdapter(ctx, loaded); err != nil {
		errs = append(errs, err)
	}
	return errs
}

func (m *Manager) stopLoadedPluginTasks(loaded *loadedPlugin) error {
	if loaded == nil {
		return nil
	}
	loaded.taskStopOnce.Do(func() {
		loaded.taskStopErr = m.operations.StopRuntime(loaded.operations)
	})
	return loaded.taskStopErr
}

func (m *Manager) pluginPublishedLocked(pluginID string) bool {
	if _, ok := m.currentHandlersLocked()[pluginID]; ok {
		return true
	}
	_, ok := m.currentExtensionsLocked()[pluginID]
	return ok
}

func (m *Manager) displacedPluginsLocked(pluginID string) []*loadedPlugin {
	plugins := make([]*loadedPlugin, 0, 1)
	for _, loaded := range m.displaced {
		if loaded.record.ID == pluginID {
			plugins = append(plugins, loaded)
		}
	}
	return plugins
}

func (m *Manager) takeDisplacedPluginsLocked(pluginID string) []*loadedPlugin {
	plugins := make([]*loadedPlugin, 0, 1)
	for key, loaded := range m.displaced {
		if loaded.record.ID == pluginID {
			plugins = append(plugins, loaded)
			delete(m.displaced, key)
		}
	}
	return plugins
}

func (m *Manager) takeAllDisplacedPluginsLocked() []*loadedPlugin {
	plugins := make([]*loadedPlugin, 0, len(m.displaced))
	for key, loaded := range m.displaced {
		plugins = appendUniqueLoadedPlugin(plugins, loaded)
		delete(m.displaced, key)
	}
	return plugins
}

func appendUniqueLoadedPlugin(plugins []*loadedPlugin, candidate *loadedPlugin) []*loadedPlugin {
	if candidate == nil {
		return plugins
	}
	for _, loaded := range plugins {
		if loaded == candidate {
			return plugins
		}
	}
	return append(plugins, candidate)
}

func (m *Manager) rollbackLoadedCandidateLocked(ctx context.Context, pluginID string, candidate, previous, replaced *loadedPlugin) {
	displaced := m.takeDisplacedPluginsLocked(pluginID)
	if len(displaced) == 0 && candidate == previous && m.pluginPublishedLocked(pluginID) {
		return
	}
	restore := replaced
	if len(displaced) > 0 {
		restore = displaced[0]
	}
	if restore != nil {
		m.loaded[pluginID] = restore
	} else {
		delete(m.loaded, pluginID)
	}
	var stale []*loadedPlugin
	if len(displaced) > 1 {
		stale = append(stale, displaced[1:]...)
	}
	stale = appendUniqueLoadedPlugin(stale, replaced)
	for _, loaded := range stale {
		if loaded != restore {
			_ = m.retireLoadedPluginLocked(ctx, loaded)
		}
	}
	if candidate != restore {
		_ = m.retireLoadedPluginLocked(ctx, candidate)
	}
}

func (m *Manager) deferRuntimeRetirementIfActive(loaded *loadedPlugin) bool {
	if loaded == nil {
		return false
	}
	key := loadedRuntimeIdentity(loaded)
	m.sessionMu.Lock()
	defer m.sessionMu.Unlock()
	if m.activeRuntimeReferencesWithoutLock(loaded, key) == 0 {
		return false
	}
	m.hostMu.Lock()
	if m.pendingRuntimeStops == nil {
		m.pendingRuntimeStops = make(map[runtimeIdentity]*loadedPlugin)
	}
	m.pendingRuntimeStops[key] = loaded
	m.hostMu.Unlock()
	return true
}

func (m *Manager) reserveUpstreamHandler(handler *upstreamHandler) bool {
	if handler == nil {
		return false
	}
	m.sessionMu.Lock()
	defer m.sessionMu.Unlock()
	if m.drainingIDs[handler.pluginID] || handler.draining.Load() {
		return false
	}
	handler.runtimeRefs.Add(1)
	return true
}

func (m *Manager) releaseUpstreamHandler(handler *upstreamHandler) {
	if handler == nil {
		return
	}
	m.sessionMu.Lock()
	handler.runtimeRefs.Add(-1)
	m.sessionMu.Unlock()
	m.queuePendingRuntimeStopIfDrained(handler.runtime)
}

// HandleConnection 在 core 读取任何 Minecraft 字节前把连接交给 v2 插件链。
// 每个处理器可以处理到底、进入下一插件，或直接进入 core。
func (m *Manager) HandleConnection(ctx context.Context, req api.UpstreamConnectRequestV2, core func(context.Context, api.ConnectionState) error) error {
	value := m.snapshot.Load()
	if req.Context == nil {
		req.Context = ctx
	}
	handlers, _ := value.([]*upstreamHandler)
	if core == nil {
		return errors.New("upstream core continuation is nil")
	}
	if req.Connection.Stream == nil {
		return errors.New("upstream connection stream is nil")
	}
	session := m.startConnectionSession(req.Context, req.Connection.Stream)
	defer m.finishConnectionSession(session.id)
	req.Context = session.ctx
	dispatch := connectionDispatch{
		manager:  m,
		handlers: handlers,
		base:     cloneTakeoverRequest(req),
		core:     core,
		session:  session,
	}
	return dispatch.run(0, req.Connection)
}

type connectionDispatch struct {
	manager  *Manager
	handlers []*upstreamHandler
	base     api.UpstreamConnectRequestV2
	core     func(context.Context, api.ConnectionState) error
	session  *connectionSession
}

type connectionFlow struct {
	dispatch *connectionDispatch
	next     int
	mu       sync.Mutex
	state    connectionFlowState
	done     chan struct{}
}

type connectionFlowState uint8

const (
	connectionFlowAvailable connectionFlowState = iota
	connectionFlowRunning
	connectionFlowRunningAfterHandlerReturn
	connectionFlowUsed
	connectionFlowExpired
)

func (d *connectionDispatch) run(index int, state api.ConnectionState) error {
	if state.Stream == nil {
		return errors.New("upstream connection stream is nil")
	}
	d.manager.trackConnectionSessionStream(d.session, state.Stream)
	for index < len(d.handlers) {
		handler := d.handlers[index]
		index++
		if !d.manager.reserveUpstreamHandler(handler) {
			continue
		}
		d.manager.addConnectionParticipant(d.session, handler)
		req := cloneTakeoverRequest(d.base)
		req.Connection = cloneConnectionState(state)
		req.Context = WithTraceContext(req.Context, handler.pluginID, req.TraceID, req.ConnectionID, handler.handlerID)
		flow := &connectionFlow{dispatch: d, next: index, done: make(chan struct{})}
		req.Flow = flow
		start := time.Now()
		err := handler.invoke(req)
		if flow.handlerReturned() {
			d.manager.closeConnectionSessionStreams(d.session)
			flow.wait()
			if err == nil {
				err = errors.New("upstream continuation outlived its handler invocation")
			}
		}
		d.manager.releaseUpstreamHandler(handler)
		status := "ok"
		if err != nil {
			status = "error"
			handler.sessionErrors.Add(1)
			handler.lastSessionError.Store(redactSensitive(sanitizeLog(err.Error())))
		}
		_ = d.manager.repo.SaveTrace(context.Background(), TraceSummary{
			PluginID: handler.pluginID, TraceID: req.TraceID, ConnectionID: req.ConnectionID,
			HandlerID: handler.handlerID, Operation: "connection.takeover", Status: status,
			DurationMS: time.Since(start).Milliseconds(),
		}, map[string]string{"transport": req.Ingress.Transport})
		return err
	}
	return d.core(d.base.Context, cloneConnectionState(state))
}

func (f *connectionFlow) Next(state api.ConnectionState) error {
	if err := f.claim(); err != nil {
		return err
	}
	defer f.complete()
	return f.dispatch.run(f.next, state)
}

func (f *connectionFlow) Core(state api.ConnectionState) error {
	if err := f.claim(); err != nil {
		return err
	}
	defer f.complete()
	if state.Stream == nil {
		return errors.New("upstream connection stream is nil")
	}
	f.dispatch.manager.trackConnectionSessionStream(f.dispatch.session, state.Stream)
	return f.dispatch.core(f.dispatch.base.Context, cloneConnectionState(state))
}

func (f *connectionFlow) claim() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch f.state {
	case connectionFlowRunning, connectionFlowRunningAfterHandlerReturn, connectionFlowUsed:
		return api.ErrContinuationUsed
	case connectionFlowExpired:
		return errors.New("upstream continuation is no longer available")
	case connectionFlowAvailable:
		f.state = connectionFlowRunning
		return nil
	default:
		return errors.New("upstream continuation has invalid state")
	}
}

func (f *connectionFlow) complete() {
	f.mu.Lock()
	if f.state == connectionFlowRunning || f.state == connectionFlowRunningAfterHandlerReturn {
		f.state = connectionFlowUsed
		close(f.done)
	}
	f.mu.Unlock()
}

func (f *connectionFlow) handlerReturned() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch f.state {
	case connectionFlowAvailable:
		f.state = connectionFlowExpired
		return false
	case connectionFlowRunning:
		f.state = connectionFlowRunningAfterHandlerReturn
		return true
	default:
		return false
	}
}

func (f *connectionFlow) wait() {
	<-f.done
}

func cloneConnectionState(state api.ConnectionState) api.ConnectionState {
	if state.Metadata != nil {
		metadata := make(map[string]string, len(state.Metadata))
		for key, value := range state.Metadata {
			metadata[key] = value
		}
		state.Metadata = metadata
	}
	return state
}

func cloneTakeoverRequest(req api.UpstreamConnectRequestV2) api.UpstreamConnectRequestV2 {
	req.Connection = cloneConnectionState(req.Connection)
	req.Ingress = req.Ingress.Clone()
	return req
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

func (m *Manager) ActiveConnectionSessions(ctx context.Context, pluginID string) ([]ConnectionSessionSummary, error) {
	_ = ctx
	now := time.Now()
	var summaries []ConnectionSessionSummary
	m.sessionMu.Lock()
	defer m.sessionMu.Unlock()
	for _, session := range m.connectionSessions {
		for handler := range session.participants {
			if pluginID != "" && handler.pluginID != pluginID {
				continue
			}
			lastError, _ := handler.lastSessionError.Load().(string)
			summaries = append(summaries, ConnectionSessionSummary{
				ID: session.id, PluginID: handler.pluginID, ArtifactID: handler.artifactID,
				HandlerID: handler.handlerID, StartedAt: session.startedAt.Unix(),
				DurationMS: now.Sub(session.startedAt).Milliseconds(), Draining: session.draining[handler.pluginID],
				ForceCloseRequested: session.forceCloseRequested, LastSessionError: lastError,
			})
		}
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
	for _, summary := range m.liveSandboxRuntimeSummaries(pluginID) {
		snapshot.SandboxRuntimes = append(snapshot.SandboxRuntimes, summary)
	}
	sort.Slice(snapshot.SandboxRuntimes, func(i, j int) bool {
		return snapshot.SandboxRuntimes[i].PluginID < snapshot.SandboxRuntimes[j].PluginID
	})
	snapshot.Exporters = m.operations.ExportSnapshot(ctx, snapshot)
	return snapshot, nil
}

func (m *Manager) liveSandboxRuntimeSummaries(pluginID string) map[string]SandboxDiagnosticSummary {
	m.mu.Lock()
	out := make(map[string]SandboxDiagnosticSummary)
	var crashed []SandboxDiagnosticSummary
	for id, loaded := range m.loaded {
		if pluginID != "" && id != pluginID {
			continue
		}
		if loaded == nil || loaded.artifact.RuntimeType != RuntimeSandbox {
			continue
		}
		process := sandboxHostedPluginFromInstance(loaded.instance).process
		if process == nil {
			continue
		}
		summary := process.Diagnostics()
		out[id] = summary
		if summary.CrashLoop || summary.State == RuntimeFailed {
			crashed = append(crashed, summary)
		}
	}
	m.mu.Unlock()
	for _, summary := range crashed {
		m.quarantineSandboxProcess(context.Background(), summary)
	}
	return out
}

func (m *Manager) quarantineSandboxProcess(ctx context.Context, summary SandboxDiagnosticSummary) {
	if summary.PluginID == "" {
		return
	}
	message := strings.TrimSpace(summary.LastError)
	if message == "" {
		message = "sandbox-process crash loop"
	}
	reasonCode := summary.ReasonCode
	if reasonCode == "" {
		reasonCode = ReasonSandboxCrashLoop
	}

	m.mu.Lock()
	loaded := m.loaded[summary.PluginID]
	if loaded == nil || loaded.artifact.RuntimeType != RuntimeSandbox {
		m.mu.Unlock()
		return
	}
	activeArtifactID := loaded.artifact.ID
	loadedArtifactID := loaded.artifact.ID
	appliedGeneration := loaded.record.DesiredGeneration
	m.removeFromDispatchLocked(summary.PluginID)
	m.removeExtensionsLocked(summary.PluginID)
	m.markDrainingLocked(summary.PluginID)
	m.operations.StopPlugin(summary.PluginID)
	delete(m.loaded, summary.PluginID)
	m.mu.Unlock()

	runtimeSummary := sandboxDiagnosticSummaryMap(summary)
	runtimeSummary["quarantine"] = true
	runtimeSummary["reason_code"] = reasonCode
	_ = m.repo.MarkRuntime(ctx, summary.PluginID, RuntimeFailed, activeArtifactID, loadedArtifactID, appliedGeneration, message, runtimeSummary, nil)
	_ = m.repo.SetPluginServiceError(ctx, message)
	m.recordPluginNodeRuntimeState(ctx, summary.PluginID)
	_ = m.repo.RecordOperation(ctx, summary.PluginID, activeArtifactID, "sandbox_crash_loop_quarantine", "failed", "system", message, map[string]any{
		"reason_code":         reasonCode,
		"runtime_instance_id": summary.RuntimeInstanceID,
		"pid":                 summary.PID,
		"crash_loop":          summary.CrashLoop,
		"crash_count":         summary.CrashCount,
		"active_streams":      summary.ActiveStreams,
		"active_calls":        summary.ActiveCalls,
		"quarantine":          true,
	})
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
		PluginID:          plugin.ID,
		ArtifactID:        artifact.ID,
		Name:              dependency,
		OK:                healthErr == nil,
		Summary:           summary,
		RuntimeStatus:     plugin.RuntimeState,
		RuntimeGeneration: plugin.AppliedGeneration,
		CheckedBy:         actor,
		CheckedAt:         m.repo.now().Unix(),
	}
	status := "succeeded"
	message := "external dependency health check succeeded"
	if healthErr != nil {
		status = "failed"
		message = "external dependency health check failed"
		result.Error = redactSensitive(healthErr.Error())
	}
	_ = m.repo.RecordOperation(ctx, plugin.ID, artifact.ID, "external_dependency_health_check", status, actor, message, map[string]any{
		"dependency":         dependency,
		"ok":                 result.OK,
		"status":             summary.LastStatus,
		"circuit_state":      summary.CircuitState,
		"runtime_status":     result.RuntimeStatus,
		"runtime_generation": result.RuntimeGeneration,
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
	if liveSummary := m.liveSandboxRuntimeSummaries(pluginID)[pluginID]; liveSummary.PluginID != "" {
		if data, err := json.Marshal(sandboxDiagnosticSummaryMap(liveSummary)); err == nil {
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

func (m *Manager) startConnectionSession(ctx context.Context, root net.Conn) *connectionSession {
	if ctx == nil {
		ctx = context.Background()
	}
	sessionCtx, cancel := context.WithCancel(ctx)
	session := &connectionSession{
		id: atomic.AddUint64(&m.sessionSeq, 1), ctx: sessionCtx, cancel: cancel, startedAt: time.Now(),
		streams: []net.Conn{root}, participants: make(map[*upstreamHandler]bool), draining: make(map[string]bool),
	}
	m.sessionMu.Lock()
	m.connectionSessions[session.id] = session
	m.sessionMu.Unlock()
	return session
}

func (m *Manager) trackConnectionSessionStream(session *connectionSession, stream net.Conn) {
	if session == nil || stream == nil {
		return
	}
	m.sessionMu.Lock()
	session.streams = append(session.streams, stream)
	forceClose := session.forceCloseRequested
	m.sessionMu.Unlock()
	if forceClose {
		_ = stream.Close()
	}
}

func (m *Manager) closeConnectionSessionStreams(session *connectionSession) {
	if session == nil {
		return
	}
	session.cancel()
	m.sessionMu.Lock()
	streams := append([]net.Conn(nil), session.streams...)
	m.sessionMu.Unlock()
	for _, stream := range streams {
		_ = stream.Close()
	}
}

func (m *Manager) addConnectionParticipant(session *connectionSession, handler *upstreamHandler) {
	if session == nil || handler == nil {
		return
	}
	m.sessionMu.Lock()
	if !session.participants[handler] {
		session.participants[handler] = true
		handler.activeSessions.Add(1)
		handler.sessionsStarted.Add(1)
		if m.drainingIDs[handler.pluginID] || handler.draining.Load() {
			session.draining[handler.pluginID] = true
			handler.drainingSessions.Add(1)
		}
	}
	m.sessionMu.Unlock()
}

func (m *Manager) ForceCloseDraining(ctx context.Context, actor, pluginID string) (int, error) {
	var sessions []*connectionSession
	m.sessionMu.Lock()
	for _, session := range m.connectionSessions {
		if session.draining[pluginID] {
			if !session.forceCloseRequested {
				for handler := range session.participants {
					if handler.pluginID == pluginID {
						handler.sessionsForceClosed.Add(1)
					}
				}
			}
			session.forceCloseRequested = true
			sessions = append(sessions, session)
		}
	}
	m.sessionMu.Unlock()
	for _, session := range sessions {
		m.closeConnectionSessionStreams(session)
	}
	_ = m.repo.RecordOperation(ctx, pluginID, "", "force_close_draining", "succeeded", actor, "draining connection sessions force closed", map[string]any{
		"closed": len(sessions),
	})
	return len(sessions), nil
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

func (m *Manager) finishConnectionSession(id uint64) {
	m.sessionMu.Lock()
	session := m.connectionSessions[id]
	delete(m.connectionSessions, id)
	var participants []*upstreamHandler
	var draining map[string]bool
	if session != nil {
		participants = make([]*upstreamHandler, 0, len(session.participants))
		for handler := range session.participants {
			participants = append(participants, handler)
		}
		draining = make(map[string]bool, len(session.draining))
		for pluginID, active := range session.draining {
			draining[pluginID] = active
		}
	}
	m.sessionMu.Unlock()
	if session == nil {
		return
	}
	session.cancel()
	for _, handler := range participants {
		handler.activeSessions.Add(-1)
		if draining[handler.pluginID] {
			handler.drainingSessions.Add(-1)
		}
		handler.sessionsCompleted.Add(1)
		handler.sessionDuration.Add(uint64(time.Since(session.startedAt).Milliseconds()))
		m.queuePendingRuntimeStopIfDrained(handler.runtime)
	}
}

func (m *Manager) queuePendingRuntimeStopIfDrained(key runtimeIdentity) {
	m.runtimeCleanupMu.Lock()
	loaded := m.takePendingRuntimeStopIfDrained(key)
	if loaded != nil {
		m.runtimeCleanupWG.Add(1)
	}
	m.runtimeCleanupMu.Unlock()
	if loaded == nil {
		return
	}
	go func() {
		defer m.runtimeCleanupWG.Done()
		m.stopPendingRuntime(loaded)
	}()
}

func (m *Manager) takePendingRuntimeStopIfDrained(key runtimeIdentity) *loadedPlugin {
	m.sessionMu.Lock()
	defer m.sessionMu.Unlock()
	m.hostMu.Lock()
	defer m.hostMu.Unlock()
	loaded := m.pendingRuntimeStops[key]
	if loaded == nil || m.activeRuntimeReferencesWithoutLock(loaded, key) > 0 {
		return nil
	}
	delete(m.pendingRuntimeStops, key)
	return loaded
}

func (m *Manager) stopPendingRuntime(loaded *loadedPlugin) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := errors.Join(m.stopRuntimeInstance(ctx, loaded)...); err != nil {
		_ = m.repo.RecordOperation(ctx, loaded.record.ID, loaded.artifact.ID, "runtime_cleanup", "warning", "system", err.Error(), map[string]any{
			"runtime_instance_id": loaded.runtime.RuntimeInstanceID,
		})
	}
}

// loadLocked 加载或复用插件实例。调用方必须持有 m.mu，确保 loaded 缓存和
// 运行态标记不会与 Enable/Disable/Reconcile 并发冲突。
func (m *Manager) loadLocked(ctx context.Context, pluginRecord PluginRecord) (*loadedPlugin, *loadedPlugin, error) {
	if m.closing.Load() {
		return nil, nil, ErrManagerClosed
	}
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
				return loaded, nil, nil
			}
		} else {
			// 同一制品、同一期望代数已经加载时直接复用，避免重复 Init 和重复注册任务。
			return loaded, nil, nil
		}
	}
	artifact, err := m.repo.Artifact(ctx, pluginRecord.DesiredArtifactID)
	if err != nil {
		return nil, nil, err
	}
	if err := m.validateArtifactGate(artifact); err != nil {
		_, _ = m.repo.MarkRuntimeIfDesiredGeneration(ctx, pluginRecord.ID, pluginRecord.DesiredGeneration, RuntimeFailed, "", "", pluginRecord.AppliedGeneration, err.Error(), runtimeFailureSummary(artifact, err), nil)
		m.recordPluginNodeRuntimeStateLocked(ctx, pluginRecord.ID)
		return nil, nil, err
	}

	var manifest Manifest
	if err := json.Unmarshal([]byte(artifact.MetadataJSON), &manifest); err != nil {
		_, _ = m.repo.MarkRuntimeIfDesiredGeneration(ctx, pluginRecord.ID, pluginRecord.DesiredGeneration, RuntimeFailed, "", "", pluginRecord.AppliedGeneration, err.Error(), runtimeFailureSummary(artifact, err), nil)
		m.recordPluginNodeRuntimeStateLocked(ctx, pluginRecord.ID)
		return nil, nil, err
	}
	pluginOperations := m.operations.ForRuntime(pluginRecord.ID, artifact.ID, manifest)
	gateway := NewGateway(pluginRecord.ID, m.wg, pluginOperations)
	runtimeInstance, err := m.startRuntimeInstance(ctx, artifact, pluginRecord, gateway)
	if err != nil {
		_, _ = m.repo.MarkRuntimeIfDesiredGeneration(ctx, pluginRecord.ID, pluginRecord.DesiredGeneration, RuntimeFailed, "", "", pluginRecord.AppliedGeneration, err.Error(), runtimeFailureSummary(artifact, err), nil)
		m.recordPluginNodeRuntimeStateLocked(ctx, pluginRecord.ID)
		return nil, nil, err
	}
	instance := runtimeInstance.Plugin
	handlers := buildHandlers(pluginRecord, artifact, gateway)
	runtimeKey := runtimeIdentity{
		pluginID:          pluginRecord.ID,
		artifactID:        artifact.ID,
		desiredGeneration: pluginRecord.DesiredGeneration,
		runtimeInstanceID: runtimeInstance.RuntimeInstanceID,
	}
	for _, handler := range handlers {
		handler.runtime = runtimeKey
	}
	extensions := buildExtensions(pluginRecord, artifact, gateway)
	// 钩子和扩展是从 gateway 注册记录中构建出来的；插件 Init 期间完成注册。
	loaded := &loadedPlugin{
		record:     pluginRecord,
		artifact:   artifact,
		instance:   instance,
		runtime:    runtimeInstance,
		gateway:    gateway,
		operations: pluginOperations,
		handlers:   handlers,
		extensions: extensions,
	}
	if wasmPlugin, ok := instance.(*wasmHostedPlugin); ok && wasmPlugin != nil {
		wasmPlugin.onRepeatedTrapQuarantine = m.handleWASMRepeatedTrapQuarantine
	}
	previous := m.loaded[pluginRecord.ID]
	m.loaded[pluginRecord.ID] = loaded
	updated, err := m.repo.MarkRuntimeIfDesiredGeneration(ctx, pluginRecord.ID, pluginRecord.DesiredGeneration, RuntimeLoaded, "", artifact.ID, pluginRecord.AppliedGeneration, "", m.loadedRuntimeSummary(loaded), loaded.dispatchSummaries())
	if err != nil {
		if previous != nil {
			m.loaded[pluginRecord.ID] = previous
		} else {
			delete(m.loaded, pluginRecord.ID)
		}
		_ = m.stopRuntimeInstance(ctx, loaded)
		return nil, nil, err
	}
	if !updated {
		if previous != nil {
			m.loaded[pluginRecord.ID] = previous
		} else {
			delete(m.loaded, pluginRecord.ID)
		}
		_ = m.stopRuntimeInstance(ctx, loaded)
		return nil, nil, errors.New("runtime desired generation changed before load completed")
	}
	m.recordPluginNodeRuntimeStateLocked(ctx, pluginRecord.ID)
	return loaded, previous, nil
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
		RuntimePrepared: runtimePreparedFor(artifact, pluginRecord, m.serviceMode),
		Plugin:          instance,
		StartedAt:       time.Now().Unix(),
	}, nil
}

func runtimeFailureSummary(artifact ArtifactRecord, err error) map[string]any {
	message := ""
	if err != nil {
		message = err.Error()
	}
	return map[string]any{
		"runtime_type": artifact.RuntimeType,
		"artifact_id":  artifact.ID,
		"error":        redactSensitive(message),
		"reason_code":  reasonCodeFromError(err),
	}
}

func runtimePreparedFor(artifact ArtifactRecord, pluginRecord PluginRecord, serviceMode string) RuntimePrepared {
	prepared := RuntimePrepared{
		PluginID:          pluginRecord.ID,
		ArtifactID:        artifact.ID,
		DesiredGeneration: pluginRecord.DesiredGeneration,
		ConfigHash:        stableHashJSONRaw(defaultJSONObject(pluginRecord.ConfigJSON)),
		Runtime:           artifact.RuntimeType,
		Mode:              serviceMode,
		PreparedAt:        time.Now().Unix(),
	}
	var manifest Manifest
	if json.Unmarshal([]byte(artifact.MetadataJSON), &manifest) == nil {
		prepared.RuntimeLimitsHash = stableHash(manifest.RuntimeLimits)
		prepared.CapabilityHash = stableHashJSONRaw(defaultJSONObject(string(manifest.Capabilities)))
	}
	return prepared
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
			switch configured := m.adapter.(type) {
			case SandboxProcessAdapter:
				typed.Supervisor = configured.Supervisor
				typed.startProcess = configured.startProcess
			case *SandboxProcessAdapter:
				if configured != nil {
					typed.Supervisor = configured.Supervisor
					typed.startProcess = configured.startProcess
				}
			}
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
		var manifest Manifest
		if err := json.Unmarshal([]byte(artifact.MetadataJSON), &manifest); err != nil {
			return fmt.Errorf("decode sandbox manifest: %w", err)
		}
		if err := validateSandboxArtifactMetadata(artifact, manifest); err != nil {
			return err
		}
		if m.serviceMode != PluginServiceModeSandboxProcess {
			return errors.New("sandbox-process runtime is disabled by plugin service mode")
		}
		if _, message := m.validateSandboxServiceModeApply(); message != "" {
			return errors.New(message)
		}
		caps := requiredRuntimeCapabilities(artifact)
		if missing := sandboxExternalDependencyCapabilityMissing(manifest, caps); len(missing) > 0 {
			return fmt.Errorf("sandbox-process external dependencies require runtime capability network.egress: %s", strings.Join(missing, ","))
		}
		if unsupported := unsupportedSandboxRequiredCapabilities(m.sandboxPolicy, caps); len(unsupported) > 0 {
			return fmt.Errorf("sandbox-process cannot enforce required capabilities: %s", strings.Join(unsupported, ","))
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
	current, currentErr := m.repo.Plugin(ctx, plugin.ID)
	if currentErr == nil && (current.DesiredGeneration != plugin.DesiredGeneration || current.DesiredArtifactID != plugin.DesiredArtifactID) {
		metadata["stale_runtime_generation"] = true
		metadata["current_generation"] = current.DesiredGeneration
		m.mu.Unlock()
		_ = m.repo.RecordOperation(ctx, plugin.ID, plugin.DesiredArtifactID, "reload", "skipped", actor, "stale runtime generation skipped", metadata)
		return nil
	}
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
	updated, err := m.repo.MarkRuntimeIfDesiredGeneration(ctx, plugin.ID, plugin.DesiredGeneration, RuntimeEnabled, loaded.artifact.ID, loaded.artifact.ID, plugin.DesiredGeneration, "", summary, loaded.dispatchSummaries())
	if err == nil && updated {
		m.recordPluginNodeRuntimeStateLocked(ctx, plugin.ID)
	}
	m.mu.Unlock()
	if err != nil {
		metadata["last_error"] = redactSensitive(err.Error())
		_ = m.repo.RecordOperation(ctx, plugin.ID, loaded.artifact.ID, "reload", "failed", actor, "runtime reload state update failed", metadata)
		return err
	}
	if !updated {
		metadata["stale_runtime_generation"] = true
		_ = m.repo.RecordOperation(ctx, plugin.ID, loaded.artifact.ID, "reload", "skipped", actor, "stale runtime generation skipped", metadata)
		return nil
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
	m.markHostStarted(loaded.record.ID, loaded.artifact.ID, loaded.runtime.HostProcess)
	summary := m.loadedRuntimeSummary(loaded)
	summary["plugin_host"] = m.hostSummary(loaded.record.ID)
	updated, err := m.repo.MarkRuntimeIfDesiredGeneration(ctx, loaded.record.ID, loaded.record.DesiredGeneration, RuntimeEnabled, loaded.artifact.ID, loaded.artifact.ID, loaded.record.DesiredGeneration, "", summary, loaded.dispatchSummaries())
	if err != nil {
		return err
	}
	if !updated {
		return errors.New("runtime desired generation changed before enable completed")
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
	if loaded.runtime.PluginID != "" {
		summary["runtime_plugin_id"] = loaded.runtime.PluginID
	}
	if loaded.runtime.ArtifactID != "" {
		summary["runtime_artifact_id"] = loaded.runtime.ArtifactID
	}
	if loaded.runtime.RuntimeInstanceID != "" {
		summary["runtime_instance_id"] = loaded.runtime.RuntimeInstanceID
	}
	if loaded.runtime.DesiredGeneration != 0 {
		summary["desired_generation"] = loaded.runtime.DesiredGeneration
	}
	if loaded.runtime.ConfigHash != "" {
		summary["config_hash"] = loaded.runtime.ConfigHash
	}
	if loaded.runtime.RuntimeLimitsHash != "" {
		summary["runtime_limits_hash"] = loaded.runtime.RuntimeLimitsHash
	}
	if loaded.runtime.CapabilityHash != "" {
		summary["capability_hash"] = loaded.runtime.CapabilityHash
	}
	if wasmPlugin, ok := loaded.instance.(*wasmHostedPlugin); ok && wasmPlugin != nil {
		for key, value := range wasmPlugin.diagnosticsSummary() {
			summary[key] = value
		}
	}
	if loaded.artifact.RuntimeType == RuntimeSandbox {
		process := sandboxHostedPluginFromInstance(loaded.instance).process
		if process != nil {
			for key, value := range sandboxDiagnosticSummaryMap(process.Diagnostics()) {
				summary[key] = value
			}
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
	m.sessionMu.Lock()
	defer m.sessionMu.Unlock()
	m.drainingIDs[pluginID] = true
	for _, session := range m.connectionSessions {
		if session.draining[pluginID] {
			continue
		}
		for handler := range session.participants {
			if handler.pluginID == pluginID {
				session.draining[pluginID] = true
				handler.drainingSessions.Add(1)
			}
		}
	}
}

func (m *Manager) markLoadedRuntimeDrainingLocked(loaded *loadedPlugin) {
	if loaded == nil {
		return
	}
	key := loadedRuntimeIdentity(loaded)
	for _, handler := range loaded.handlers {
		handler.draining.Store(true)
	}
	m.sessionMu.Lock()
	defer m.sessionMu.Unlock()
	for _, session := range m.connectionSessions {
		for handler := range session.participants {
			if handler.runtime == key && !session.draining[handler.pluginID] {
				session.draining[handler.pluginID] = true
				handler.drainingSessions.Add(1)
			}
		}
	}
}

func (m *Manager) clearDrainingLocked(pluginID string) {
	m.sessionMu.Lock()
	delete(m.drainingIDs, pluginID)
	m.sessionMu.Unlock()
}

func (m *Manager) activeConnectionSessionCountLocked(pluginID string) int {
	m.sessionMu.Lock()
	defer m.sessionMu.Unlock()
	count := 0
	for _, session := range m.connectionSessions {
		for handler := range session.participants {
			if handler.pluginID == pluginID {
				count++
				break
			}
		}
	}
	return count
}

func (m *Manager) activeConnectionSessionCountForRuntimeLocked(key runtimeIdentity) int {
	m.sessionMu.Lock()
	defer m.sessionMu.Unlock()
	return m.activeConnectionSessionCountForRuntimeWithoutLock(key)
}

func (m *Manager) activeConnectionSessionCountForRuntimeWithoutLock(key runtimeIdentity) int {
	count := 0
	for _, session := range m.connectionSessions {
		for handler := range session.participants {
			if handler.runtime == key {
				count++
				break
			}
		}
	}
	return count
}

func (m *Manager) activeRuntimeReferencesWithoutLock(loaded *loadedPlugin, key runtimeIdentity) int64 {
	count := int64(m.activeConnectionSessionCountForRuntimeWithoutLock(key))
	for _, handler := range loaded.handlers {
		count += handler.runtimeRefs.Load()
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
	var handlers []*upstreamHandler
	if hook, ok := gateway.UpstreamConnectHandlerV2(); ok {
		handlers = append(handlers, &upstreamHandler{
			pluginID: pluginRecord.ID, artifactID: artifact.ID, priority: pluginRecord.Priority,
			handlerID: "upstream.connect/v2", handle: hook,
		})
	}
	return handlers
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
	case RuntimeWASM:
		return true
	default:
		return false
	}
}

func unsupportedSandboxRequiredCapabilities(policy SandboxPolicy, capabilities []string) []string {
	policy = normalizeSandboxPolicy(policy)
	var unsupported []string
	for _, capability := range uniqueSortedStrings(capabilities) {
		switch strings.ToLower(strings.TrimSpace(capability)) {
		case "", "filesystem.read", "filesystem.write", "network.none", "env", "secret.handle", "cpu.memory":
			continue
		case "network.egress":
			if sandboxFactsAllRequiredEnforced(sandboxEnforcementFacts(policy), "network", "egress_policy") {
				continue
			}
		case "process.restricted":
			if sandboxFactsAllRequiredEnforced(
				sandboxEnforcementFacts(policy),
				"process",
				"no_new_privs",
				"capabilities_dropped",
				"seccomp",
				"fork_exec_policy",
			) {
				continue
			}
		}
		unsupported = append(unsupported, capability)
	}
	return uniqueSortedStrings(unsupported)
}

func sandboxExternalDependencyCapabilityMissing(manifest Manifest, capabilities []string) []string {
	if len(manifest.ExternalDeps) == 0 || stringSliceContainsValue(capabilities, "network.egress") {
		return nil
	}
	missing := make([]string, 0, len(manifest.ExternalDeps))
	for _, dep := range manifest.ExternalDeps {
		name := strings.TrimSpace(dep.Name)
		if name == "" {
			name = dep.Endpoint
		}
		if name != "" {
			missing = append(missing, name)
		}
	}
	return uniqueSortedStrings(missing)
}

func stringSliceContainsValue(values []string, want string) bool {
	want = strings.ToLower(strings.TrimSpace(want))
	for _, value := range values {
		if strings.ToLower(strings.TrimSpace(value)) == want {
			return true
		}
	}
	return false
}

func (h *upstreamHandler) invoke(req api.UpstreamConnectRequestV2) (err error) {
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
	defer func() {
		if rec := recover(); rec != nil {
			h.panics.Add(1)
			err = fmt.Errorf("plugin %s panic: %v", h.pluginID, rec)
		}
		if err != nil {
			h.errors.Add(1)
		}
	}()
	return h.handle(req)
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
		lastSessionError, _ := handler.lastSessionError.Load().(string)
		summaries = append(summaries, DispatchHandlerSummary{
			PluginID:                      handler.pluginID,
			ArtifactID:                    handler.artifactID,
			Priority:                      handler.priority,
			HandlerID:                     handler.handlerID,
			ExtensionPoint:                ExtensionUpstreamConnect,
			Mode:                          "connection-takeover",
			TimeoutMS:                     0,
			Calls:                         handler.calls.Load(),
			Errors:                        handler.errors.Load(),
			Panics:                        handler.panics.Load(),
			Timeouts:                      handler.timeouts.Load(),
			Blocked:                       handler.blocked.Load(),
			ActiveSessions:                handler.activeSessions.Load(),
			DrainingSessions:              handler.drainingSessions.Load(),
			ConnectionSessionsStarted:     handler.sessionsStarted.Load(),
			ConnectionSessionsCompleted:   handler.sessionsCompleted.Load(),
			ConnectionSessionsForceClosed: handler.sessionsForceClosed.Load(),
			ConnectionSessionErrors:       handler.sessionErrors.Load(),
			LastSessionError:              lastSessionError,
			ConnectionSessionDurationMS:   handler.sessionDuration.Load(),
			DurationCount:                 handler.durationCount.Load(),
			DurationSumMS:                 handler.durationSumMS.Load(),
			DurationMaxMS:                 handler.durationMaxMS.Load(),
		})
	}
	return summaries
}
