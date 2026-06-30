package pluginmanager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"

	"github.com/tursom/mc-gateway/plugin/api"
)

const (
	wasmMinHandlerTimeoutMS = 10
	wasmMaxHandlerTimeoutMS = int(DefaultHandlerTimeout / time.Millisecond)
	wasmMinMemoryBytes      = 64 * 1024
	wasmMaxMemoryBytes      = 64 * 1024 * 1024
)

type WASMRunner struct {
	mu       sync.Mutex
	compiled map[string][]byte
}

type WASMInvocation struct {
	ArtifactID  string
	Module      []byte
	Manifest    Manifest
	Function    string
	Timeout     time.Duration
	MemoryBytes int
}

type WASMDispatchInvocation struct {
	ArtifactID     string
	PluginID       string
	Module         []byte
	Manifest       Manifest
	ExtensionPoint string
	Request        WASMABIRequest
	Timeout        time.Duration
	MemoryBytes    int
	MaxOutputBytes int
}

type WASMAdapter struct {
	Runner *WASMRunner
	Mode   string
}

type wasmHostedPlugin struct {
	runner             *WASMRunner
	manifest           Manifest
	artifact           ArtifactRecord
	pluginID           string
	module             []byte
	configJSON         json.RawMessage
	moduleHash         string
	moduleCacheStatus  string
	artifactGeneration int64
	state              string
	startedAt          int64
	drainingAt         int64
	stoppedAt          int64
	dispatchHook       func(context.Context, wasmRuntimeSnapshot, string, WASMABIRequest) (WASMABIResponse, error, bool)
	mu                 sync.Mutex
	activeCalls        int
	trapCount          int64
	timeoutCount       int64
	memoryErrorCount   int64
	lastError          string
	lastPoint          string
	lastAt             int64
	lastOK             bool
}

type wasmRuntimeSnapshot struct {
	runner             *WASMRunner
	manifest           Manifest
	artifact           ArtifactRecord
	pluginID           string
	module             []byte
	configJSON         json.RawMessage
	moduleHash         string
	moduleCacheStatus  string
	artifactGeneration int64
	dispatchHook       func(context.Context, wasmRuntimeSnapshot, string, WASMABIRequest) (WASMABIResponse, error, bool)
}

func (r *WASMRunner) Validate(ctx context.Context, manifest Manifest, behavior string) error {
	return r.Invoke(ctx, WASMInvocation{
		ArtifactID:  manifest.ID + ":" + behavior,
		Module:      wasmFixtureModule(behavior),
		Manifest:    manifest,
		Function:    wasmDefaultExport(manifest),
		Timeout:     wasmTimeout(manifest),
		MemoryBytes: manifest.RuntimeLimits.MemoryBytes,
	})
}

func (r *WASMRunner) Invoke(ctx context.Context, invocation WASMInvocation) error {
	if r == nil {
		r = NewWASMRunner()
	}
	if err := validateWASMExtensionPoints(invocation.Manifest); err != nil {
		return err
	}
	if err := validateWASMManifestABI(invocation.Manifest); err != nil {
		return err
	}
	if err := validateWASMRuntimeLimits(invocation.Manifest); err != nil {
		return err
	}
	if len(invocation.Module) == 0 {
		return errors.New("wasm module is empty")
	}
	timeout := invocation.Timeout
	if timeout <= 0 {
		timeout = wasmTimeout(invocation.Manifest)
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	runtimeConfig := wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(wasmMemoryLimitPages(invocation.MemoryBytes))
	runtime := wazero.NewRuntimeWithConfig(callCtx, runtimeConfig)
	defer runtime.Close(context.Background())

	compiled, err := r.compile(callCtx, runtime, invocation)
	if err != nil {
		return err
	}
	if err := validateWASMCompiledABI(compiled, invocation.Manifest); err != nil {
		return err
	}
	if err := instantiateWASMHostImports(callCtx, runtime); err != nil {
		return err
	}
	moduleConfig := wazero.NewModuleConfig().
		WithName("mc-gateway-plugin-wasm").
		WithStartFunctions()
	module, err := runtime.InstantiateModule(callCtx, compiled, moduleConfig)
	if err != nil {
		return err
	}
	defer module.Close(context.Background())
	fnName := invocation.Function
	if fnName == "" {
		fnName = wasmDefaultExport(invocation.Manifest)
	}
	fn := module.ExportedFunction(fnName)
	if fn == nil {
		return newWASMABIError(wasmABIErrorExportMissing, "", fmt.Sprintf("wasm export %q not found", fnName))
	}
	results, err := fn.Call(callCtx, 0, 0)
	if err != nil {
		return mapWASMInvocationError("", err)
	}
	if len(results) > 0 && results[0] != 0 {
		return newWASMABIError(wasmABIErrorBadOutput, "", fmt.Sprintf("wasm validation returned non-zero result %d", results[0]))
	}
	return nil
}

func (r *WASMRunner) Dispatch(ctx context.Context, invocation WASMDispatchInvocation) (WASMABIResponse, error) {
	if r == nil {
		r = NewWASMRunner()
	}
	point := invocation.ExtensionPoint
	if err := validateWASMExtensionPoints(invocation.Manifest); err != nil {
		return WASMABIResponse{}, err
	}
	if err := validateWASMManifestABI(invocation.Manifest); err != nil {
		return WASMABIResponse{}, err
	}
	if err := validateWASMRuntimeLimits(invocation.Manifest); err != nil {
		return WASMABIResponse{}, err
	}
	if len(invocation.Module) == 0 {
		return WASMABIResponse{}, errors.New("wasm module is empty")
	}
	fnName, ok := WASMABIExportForExtension(point)
	if !ok {
		return WASMABIResponse{}, newWASMABIError(wasmABIErrorSchemaInvalid, point, fmt.Sprintf("unsupported extension_point %q", point))
	}
	invocation.Request.PluginID = firstNonEmpty(invocation.Request.PluginID, invocation.PluginID, invocation.Manifest.ID)
	invocation.Request.ArtifactID = firstNonEmpty(invocation.Request.ArtifactID, invocation.ArtifactID)
	invocation.Request.ExtensionPoint = point
	reqBytes, err := EncodeWASMABIRequest(invocation.Request)
	if err != nil {
		return WASMABIResponse{}, err
	}
	timeout := invocation.Timeout
	if timeout <= 0 {
		timeout = wasmTimeout(invocation.Manifest)
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	runtimeConfig := wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(wasmMemoryLimitPages(invocation.MemoryBytes))
	runtime := wazero.NewRuntimeWithConfig(callCtx, runtimeConfig)
	defer runtime.Close(context.Background())

	compiled, err := r.compile(callCtx, runtime, WASMInvocation{
		ArtifactID:  invocation.ArtifactID,
		Module:      invocation.Module,
		Manifest:    invocation.Manifest,
		Function:    fnName,
		Timeout:     timeout,
		MemoryBytes: invocation.MemoryBytes,
	})
	if err != nil {
		return WASMABIResponse{}, err
	}
	if err := validateWASMCompiledABI(compiled, invocation.Manifest); err != nil {
		return WASMABIResponse{}, err
	}
	if err := instantiateWASMHostImports(callCtx, runtime); err != nil {
		return WASMABIResponse{}, err
	}
	moduleConfig := wazero.NewModuleConfig().
		WithName("mc-gateway-plugin-wasm").
		WithStartFunctions()
	module, err := runtime.InstantiateModule(callCtx, compiled, moduleConfig)
	if err != nil {
		return WASMABIResponse{}, err
	}
	defer module.Close(context.Background())
	memory := module.Memory()
	if memory == nil {
		return WASMABIResponse{}, newWASMABIError(wasmABIErrorABIMismatch, point, "wasm module must export memory for host ABI JSON exchange")
	}
	const requestOffset = uint32(0)
	if !memory.Write(requestOffset, reqBytes) {
		return WASMABIResponse{}, newWASMABIError(wasmABIErrorOversizedInput, point, "request does not fit wasm memory")
	}
	fn := module.ExportedFunction(fnName)
	if fn == nil {
		return WASMABIResponse{}, newWASMABIError(wasmABIErrorExportMissing, point, fmt.Sprintf("wasm export %q not found", fnName))
	}
	results, err := fn.Call(callCtx, uint64(requestOffset), uint64(len(reqBytes)))
	if err != nil {
		return WASMABIResponse{}, mapWASMInvocationError(point, err)
	}
	if len(results) != 1 {
		return WASMABIResponse{}, newWASMABIError(wasmABIErrorABIMismatch, point, fmt.Sprintf("wasm export %q returned %d results", fnName, len(results)))
	}
	respPtr, respLen := wasmUnpackPtrLen(results[0])
	maxOutput := invocation.MaxOutputBytes
	if maxOutput <= 0 {
		maxOutput = wasmABIMaxOutputBytes
	}
	if respLen == 0 || int(respLen) > maxOutput {
		return WASMABIResponse{}, newWASMABIError(wasmABIErrorOversizedOutput, point, fmt.Sprintf("response size %d exceeds limit %d", respLen, maxOutput))
	}
	respBytes, ok := memory.Read(respPtr, respLen)
	if !ok {
		return WASMABIResponse{}, newWASMABIError(wasmABIErrorBadOutput, point, fmt.Sprintf("response memory range ptr=%d len=%d is invalid", respPtr, respLen))
	}
	resp, _, err := DecodeWASMABIResponse(point, append([]byte(nil), respBytes...))
	if err != nil {
		return WASMABIResponse{}, err
	}
	if resp.Error != nil {
		return resp, newWASMABIError(resp.Error.Code, point, resp.Error.Message)
	}
	return resp, nil
}

func (r *WASMRunner) compile(ctx context.Context, runtime wazero.Runtime, invocation WASMInvocation) (wazero.CompiledModule, error) {
	compiled, _, err := r.compileWithCacheStatus(ctx, runtime, invocation)
	return compiled, err
}

func (r *WASMRunner) compileWithCacheStatus(ctx context.Context, runtime wazero.Runtime, invocation WASMInvocation) (wazero.CompiledModule, string, error) {
	key := invocation.ArtifactID
	if key == "" {
		sum := sha256.Sum256(invocation.Module)
		key = hex.EncodeToString(sum[:])
	}
	r.mu.Lock()
	if r.compiled == nil {
		r.compiled = make(map[string][]byte)
	}
	cached := append([]byte(nil), r.compiled[key]...)
	if len(cached) > 0 {
		r.mu.Unlock()
		compiled, err := runtime.CompileModule(ctx, cached)
		return compiled, "hit", err
	}
	r.mu.Unlock()
	compiled, err := runtime.CompileModule(ctx, invocation.Module)
	if err != nil {
		return nil, "miss", err
	}
	r.mu.Lock()
	r.compiled[key] = append([]byte(nil), invocation.Module...)
	r.mu.Unlock()
	return compiled, "miss", nil
}

func validateWASMExtensionPoints(manifest Manifest) error {
	for _, point := range manifest.ExtensionPoints {
		switch point.Key {
		case ExtensionRuleEvaluate, ExtensionRouteResolve, ExtensionConfigValidate:
		default:
			return fmt.Errorf("wasm extension point %q is not supported; allowed extension points are %s, %s, %s", point.Key, ExtensionConfigValidate, ExtensionRuleEvaluate, ExtensionRouteResolve)
		}
	}
	return nil
}

func wasmBlockedHostCapabilities(manifest Manifest, artifact ArtifactRecord) []string {
	blocked := map[string]bool{}
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name != "" {
			blocked[name] = true
		}
	}
	for _, capability := range requiredRuntimeCapabilities(artifact) {
		if wasmHostCapabilityNameBlocked(capability) {
			add("runtime.required_capabilities:" + capability)
		}
	}
	if len(manifest.Secrets) > 0 {
		add("secrets")
	}
	if len(manifest.ExternalDeps) > 0 {
		add("external_dependencies")
	}
	if len(manifest.FileStores) > 0 {
		add("file_stores")
	}
	var caps map[string]any
	if json.Unmarshal(manifest.Capabilities, &caps) == nil {
		for _, key := range []string{
			"env", "environment", "secret_env", "secret_envs", "secret", "secrets",
			"file", "files", "filesystem", "file_stores", "network", "net", "external_dependencies", "external_deps",
			"upstream_connect", "status", "providers", "provider", "event_subscriber", "ingress",
		} {
			if metadataFieldPresent(caps[key]) {
				add("capabilities." + key)
			}
		}
		if runtimeCaps := jsonMapFromAny(caps["runtime"]); runtimeCaps != nil {
			for _, key := range []string{"env", "environment", "secret_env", "secret", "file", "filesystem", "network"} {
				if metadataFieldPresent(runtimeCaps[key]) {
					add("capabilities.runtime." + key)
				}
			}
		}
	}
	return sortedStringKeys(blocked)
}

func wasmHostCapabilityNameBlocked(capability string) bool {
	normalized := strings.ToLower(strings.TrimSpace(capability))
	normalized = strings.ReplaceAll(normalized, "_", ".")
	switch {
	case normalized == "":
		return false
	case strings.Contains(normalized, "secret.env"):
		return true
	case strings.Contains(normalized, "secret"):
		return true
	case strings.Contains(normalized, "env"):
		return true
	case strings.Contains(normalized, "network") || strings.Contains(normalized, "net.") || strings.Contains(normalized, ".net"):
		return true
	case strings.Contains(normalized, "file") || strings.Contains(normalized, "filesystem") || strings.Contains(normalized, "fs."):
		return true
	default:
		return false
	}
}

func sortedStringKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func validateWASMRuntimeLimits(manifest Manifest) error {
	if manifest.Runtime.Type != RuntimeWASM {
		return nil
	}
	timeoutMS := manifest.RuntimeLimits.HandlerTimeoutMS
	if timeoutMS < wasmMinHandlerTimeoutMS || timeoutMS > wasmMaxHandlerTimeoutMS {
		return fmt.Errorf("wasm runtime limit handler_timeout_ms %d is outside supported range [%d,%d]", timeoutMS, wasmMinHandlerTimeoutMS, wasmMaxHandlerTimeoutMS)
	}
	memoryBytes := manifest.RuntimeLimits.MemoryBytes
	if memoryBytes < wasmMinMemoryBytes || memoryBytes > wasmMaxMemoryBytes {
		return fmt.Errorf("wasm runtime limit memory_bytes %d is outside supported range [%d,%d]", memoryBytes, wasmMinMemoryBytes, wasmMaxMemoryBytes)
	}
	return nil
}

func wasmModuleHash(module []byte) string {
	sum := sha256.Sum256(module)
	return hex.EncodeToString(sum[:])
}

func (r *WASMRunner) PrepareModule(ctx context.Context, invocation WASMInvocation) (string, string, error) {
	if r == nil {
		r = NewWASMRunner()
	}
	if err := validateWASMExtensionPoints(invocation.Manifest); err != nil {
		return "", "", err
	}
	if err := validateWASMManifestABI(invocation.Manifest); err != nil {
		return "", "", err
	}
	if err := validateWASMRuntimeLimits(invocation.Manifest); err != nil {
		return "", "", err
	}
	if len(invocation.Module) == 0 {
		return "", "", errors.New("wasm module is empty")
	}
	callCtx, cancel := context.WithTimeout(ctx, wasmTimeout(invocation.Manifest))
	defer cancel()
	runtimeConfig := wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(wasmMemoryLimitPages(invocation.MemoryBytes))
	runtime := wazero.NewRuntimeWithConfig(callCtx, runtimeConfig)
	defer runtime.Close(context.Background())
	compiled, cacheStatus, err := r.compileWithCacheStatus(callCtx, runtime, invocation)
	if err != nil {
		return "", cacheStatus, err
	}
	defer compiled.Close(context.Background())
	if err := validateWASMCompiledABI(compiled, invocation.Manifest); err != nil {
		return "", cacheStatus, err
	}
	return wasmModuleHash(invocation.Module), cacheStatus, nil
}

func validateWASMArtifactABI(ctx context.Context, artifact ArtifactRecord, manifest Manifest) error {
	if err := validateWASMManifestABI(manifest); err != nil {
		return err
	}
	if err := validateWASMRuntimeLimits(manifest); err != nil {
		return err
	}
	if artifact.FilePath == "" {
		return nil
	}
	module, err := os.ReadFile(artifact.FilePath)
	if err != nil {
		return err
	}
	return validateWASMModuleABI(ctx, module, manifest, artifactSizeMemoryLimit(artifact, manifest))
}

func artifactSizeMemoryLimit(_ ArtifactRecord, manifest Manifest) int {
	return manifest.RuntimeLimits.MemoryBytes
}

func wasmTimeout(manifest Manifest) time.Duration {
	timeout := DefaultHandlerTimeout
	if manifest.RuntimeLimits.HandlerTimeoutMS > 0 {
		timeout = time.Duration(manifest.RuntimeLimits.HandlerTimeoutMS) * time.Millisecond
	}
	if timeout <= 0 {
		return DefaultHandlerTimeout
	}
	return timeout
}

func wasmMemoryLimitPages(memoryBytes int) uint32 {
	if memoryBytes <= 0 {
		return 1
	}
	pages := (memoryBytes + 65535) / 65536
	if pages <= 0 {
		return 1
	}
	return uint32(pages)
}

func wasmArtifactModule(artifact ArtifactRecord) ([]byte, error) {
	if artifact.FilePath == "" {
		return nil, errors.New("wasm artifact path is empty")
	}
	return os.ReadFile(artifact.FilePath)
}

func wasmUnpackPtrLen(value uint64) (uint32, uint32) {
	return uint32(value >> 32), uint32(value)
}

func wasmManifestHasExtension(manifest Manifest, point string) bool {
	for _, extension := range manifest.ExtensionPoints {
		if extension.Key == point {
			return true
		}
	}
	return false
}

func wasmConfigJSON(config any) (json.RawMessage, error) {
	switch typed := config.(type) {
	case nil:
		return json.RawMessage(`{}`), nil
	case json.RawMessage:
		return json.RawMessage(defaultJSONObject(string(typed))), nil
	case []byte:
		return json.RawMessage(defaultJSONObject(string(typed))), nil
	case string:
		return json.RawMessage(defaultJSONObject(typed)), nil
	default:
		data, err := json.Marshal(typed)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(defaultJSONObject(string(data))), nil
	}
}

func mergeWASMRuntimeDetails(details map[string]any, plugin api.Plugin) map[string]any {
	wasmPlugin, ok := plugin.(*wasmHostedPlugin)
	if !ok || wasmPlugin == nil {
		return details
	}
	for key, value := range wasmPlugin.diagnosticsSummary() {
		details[key] = value
	}
	return details
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func (m *Manager) RunWASMValidation(ctx context.Context, pluginID, artifactID, behavior string) error {
	artifact, manifest, err := m.artifactManifest(ctx, pluginID, artifactID)
	if err != nil {
		return err
	}
	if m.wasmRunner == nil {
		m.wasmRunner = NewWASMRunner()
	}
	module := wasmFixtureModule(behavior)
	if behavior == "" || behavior == "artifact" {
		if artifact.FilePath == "" {
			return errors.New("wasm artifact path is empty")
		}
		module, err = os.ReadFile(artifact.FilePath)
		if err != nil {
			return err
		}
	}
	return m.wasmRunner.Invoke(ctx, WASMInvocation{
		ArtifactID:  artifact.ID + ":" + behavior,
		Module:      module,
		Manifest:    manifest,
		Function:    wasmDefaultExport(manifest),
		Timeout:     wasmTimeout(manifest),
		MemoryBytes: manifest.RuntimeLimits.MemoryBytes,
	})
}

func (a WASMAdapter) Load(ctx context.Context, artifact ArtifactRecord, pluginRecord PluginRecord, gateway *Gateway) (api.Plugin, error) {
	prepared, err := a.Prepare(ctx, artifact, pluginRecord)
	if err != nil {
		return nil, err
	}
	instance, err := a.Start(ctx, prepared, artifact, pluginRecord, gateway)
	if err != nil {
		return nil, err
	}
	return instance.Plugin, nil
}

func (a WASMAdapter) ValidateArtifact(ctx context.Context, artifact ArtifactRecord) error {
	if artifact.RuntimeType != RuntimeWASM {
		return fmt.Errorf("wasm adapter does not support runtime %q", artifact.RuntimeType)
	}
	var manifest Manifest
	if err := json.Unmarshal([]byte(artifact.MetadataJSON), &manifest); err != nil {
		return err
	}
	if err := validateWASMExtensionPoints(manifest); err != nil {
		return err
	}
	return validateWASMArtifactABI(ctx, artifact, manifest)
}

func (a WASMAdapter) Prepare(ctx context.Context, artifact ArtifactRecord, pluginRecord PluginRecord) (RuntimePrepared, error) {
	if err := a.ValidateArtifact(ctx, artifact); err != nil {
		return RuntimePrepared{}, err
	}
	mode := a.Mode
	if mode == "" {
		mode = PluginServiceModeInProcess
	}
	return RuntimePrepared{
		PluginID:   pluginRecord.ID,
		ArtifactID: artifact.ID,
		Runtime:    artifact.RuntimeType,
		Mode:       mode,
		PreparedAt: time.Now().Unix(),
	}, nil
}

func (a WASMAdapter) Start(ctx context.Context, prepared RuntimePrepared, artifact ArtifactRecord, pluginRecord PluginRecord, gateway *Gateway) (RuntimeInstance, error) {
	var manifest Manifest
	if err := json.Unmarshal([]byte(artifact.MetadataJSON), &manifest); err != nil {
		return RuntimeInstance{}, err
	}
	runner := a.Runner
	if runner == nil {
		runner = NewWASMRunner()
	}
	if err := validateWASMArtifactABI(ctx, artifact, manifest); err != nil {
		return RuntimeInstance{}, err
	}
	module, err := wasmArtifactModule(artifact)
	if err != nil {
		return RuntimeInstance{}, err
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
		return RuntimeInstance{}, err
	}
	configJSON := json.RawMessage(defaultJSONObject(pluginRecord.ConfigJSON))
	plugin := &wasmHostedPlugin{
		runner:             runner,
		manifest:           manifest,
		artifact:           artifact,
		pluginID:           pluginRecord.ID,
		module:             append([]byte(nil), module...),
		configJSON:         append(json.RawMessage(nil), configJSON...),
		moduleHash:         moduleHash,
		moduleCacheStatus:  cacheStatus,
		artifactGeneration: pluginRecord.DesiredGeneration,
		state:              RuntimeEnabled,
		startedAt:          time.Now().Unix(),
	}
	if err := plugin.validateConfig(ctx, configJSON); err != nil {
		return RuntimeInstance{}, err
	}
	if gateway != nil {
		if err := plugin.Init(gateway); err != nil {
			return RuntimeInstance{}, err
		}
	}
	return RuntimeInstance{
		RuntimePrepared: prepared,
		Plugin:          plugin,
		StartedAt:       time.Now().Unix(),
	}, nil
}

func (a WASMAdapter) HealthCheck(_ context.Context, instance RuntimeInstance) RuntimeHealth {
	state := RuntimeFailed
	ok := false
	errMessage := "plugin instance is nil"
	if plugin, okPlugin := instance.Plugin.(*wasmHostedPlugin); okPlugin && plugin != nil {
		state = plugin.lifecycleState()
		ok = state == RuntimeEnabled || state == RuntimeDraining
		errMessage = ""
	}
	return RuntimeHealth{
		OK:     ok,
		Status: state,
		Error:  errMessage,
		Details: mergeWASMRuntimeDetails(map[string]any{
			"host_abi":                wasmHostABIV1,
			"exports":                 wasmABIExportMap(),
			"imports":                 wasmABIImportMap(),
			"module_cache":            true,
			"default_filesystem":      "none",
			"default_network":         "none",
			"low_risk_extension_only": true,
		}, instance.Plugin),
		CheckedAt: time.Now().Unix(),
	}
}

func (a WASMAdapter) ReloadConfig(ctx context.Context, instance RuntimeInstance, configJSON string) error {
	plugin, ok := instance.Plugin.(*wasmHostedPlugin)
	if !ok || plugin == nil {
		return nil
	}
	next := json.RawMessage(defaultJSONObject(configJSON))
	if err := plugin.validateConfig(ctx, next); err != nil {
		return err
	}
	plugin.mu.Lock()
	plugin.configJSON = append(json.RawMessage(nil), next...)
	plugin.mu.Unlock()
	return nil
}

func (a WASMAdapter) Drain(ctx context.Context, instance RuntimeInstance) error {
	plugin, ok := instance.Plugin.(*wasmHostedPlugin)
	if !ok || plugin == nil {
		return nil
	}
	return plugin.drain(ctx)
}

func (a WASMAdapter) Stop(ctx context.Context, instance RuntimeInstance) error {
	plugin, ok := instance.Plugin.(*wasmHostedPlugin)
	if !ok || plugin == nil {
		return nil
	}
	return plugin.stop(ctx)
}

func (a WASMAdapter) Diagnostics(_ context.Context, instance RuntimeInstance) RuntimeAdapterDiagnostics {
	state := RuntimeFailed
	if plugin, ok := instance.Plugin.(*wasmHostedPlugin); ok && plugin != nil {
		state = plugin.lifecycleState()
	} else if instance.Plugin == nil && instance.RuntimePrepared.PluginID != "" {
		state = RuntimeNotLoaded
	}
	return RuntimeAdapterDiagnostics{
		PluginID:   instance.PluginID,
		ArtifactID: instance.ArtifactID,
		Runtime:    instance.Runtime,
		Mode:       instance.Mode,
		State:      state,
		Details: mergeWASMRuntimeDetails(map[string]any{
			"host_abi":                   wasmHostABIV1,
			"exports":                    wasmABIExportMap(),
			"imports":                    wasmABIImportMap(),
			"module_cache":               true,
			"fuel_equivalent_time_limit": true,
			"memory_limit":               true,
			"default_filesystem":         "none",
			"default_network":            "none",
		}, instance.Plugin),
		CreatedAt: time.Now().Unix(),
	}
}

func (a WASMAdapter) DryRunConfig(ctx context.Context, artifact ArtifactRecord, pluginRecord PluginRecord) error {
	var manifest Manifest
	if err := json.Unmarshal([]byte(artifact.MetadataJSON), &manifest); err != nil {
		return err
	}
	runner := a.Runner
	if runner == nil {
		runner = NewWASMRunner()
	}
	if err := validateWASMArtifactABI(ctx, artifact, manifest); err != nil {
		return err
	}
	module, err := wasmArtifactModule(artifact)
	if err != nil {
		return err
	}
	plugin := &wasmHostedPlugin{
		runner:   runner,
		manifest: manifest,
		artifact: artifact,
		pluginID: pluginRecord.ID,
		module:   module,
		state:    RuntimeEnabled,
	}
	return plugin.validateConfig(ctx, json.RawMessage(defaultJSONObject(pluginRecord.ConfigJSON)))
}

func (p *wasmHostedPlugin) Init(gateway api.Gateway) error {
	for _, point := range p.manifest.ExtensionPoints {
		switch point.Key {
		case ExtensionRuleEvaluate:
			if err := api.RegisterHookHandler(
				gateway,
				api.HookRuleEvaluate,
				func(api.RuleEvaluateRequest) bool { return true },
				p.evaluateRule,
			); err != nil {
				return err
			}
		case ExtensionRouteResolve:
			if err := api.RegisterHookHandler(
				gateway,
				api.HookRouteResolve,
				func(api.RouteResolveRequest) bool { return true },
				p.resolveRoute,
			); err != nil {
				return err
			}
		case ExtensionConfigValidate:
		default:
			return fmt.Errorf("wasm extension point %q is not supported by contained validation", point.Key)
		}
	}
	return nil
}

func (p *wasmHostedPlugin) Destroy() error { return nil }

func (p *wasmHostedPlugin) NewConfigObj() any { return json.RawMessage(`{}`) }

func (p *wasmHostedPlugin) ReloadConfig(config any) error {
	configJSON, err := wasmConfigJSON(config)
	if err != nil {
		return err
	}
	if err := p.validateConfig(context.Background(), configJSON); err != nil {
		return err
	}
	p.mu.Lock()
	p.configJSON = append(json.RawMessage(nil), configJSON...)
	p.mu.Unlock()
	return nil
}

func (p *wasmHostedPlugin) validateConfig(ctx context.Context, configJSON json.RawMessage) error {
	if p == nil || !wasmManifestHasExtension(p.manifest, ExtensionConfigValidate) {
		return nil
	}
	resp, err := p.dispatch(ctx, ExtensionConfigValidate, WASMABIRequest{
		Config: append(json.RawMessage(nil), configJSON...),
	})
	if err != nil {
		return err
	}
	if resp.Valid == nil || !*resp.Valid {
		reason := resp.Reason
		if reason == "" {
			reason = "wasm config validation failed"
		}
		err := fmt.Errorf("wasm config validation failed: %s", reason)
		p.recordDispatch(ExtensionConfigValidate, err)
		return err
	}
	return nil
}

func (p *wasmHostedPlugin) evaluateRule(req api.RuleEvaluateRequest) (api.RuleEvaluateDecision, error) {
	contextJSON, err := json.Marshal(req)
	if err != nil {
		return api.RuleEvaluateDecision{}, err
	}
	resp, err := p.dispatch(req.Context, ExtensionRuleEvaluate, WASMABIRequest{
		RuleContext: contextJSON,
	})
	if err != nil {
		return api.RuleEvaluateDecision{}, err
	}
	decision := api.RuleEvaluateDecision{
		Allow:      resp.Decision == "allow",
		Deny:       resp.Decision == "deny",
		Reject:     resp.Decision == "deny",
		Reason:     resp.Reason,
		ProviderID: p.pluginID,
	}
	if decision.Reason == "" {
		decision.Reason = "wasm rule decision"
	}
	return decision, nil
}

func (p *wasmHostedPlugin) resolveRoute(req api.RouteResolveRequest) (api.RouteDecision, error) {
	contextJSON, err := json.Marshal(req)
	if err != nil {
		return api.RouteDecision{}, err
	}
	resp, err := p.dispatch(req.Context, ExtensionRouteResolve, WASMABIRequest{
		RouteContext: contextJSON,
		Host:         req.Host,
		Source:       req.SourceAddr,
	})
	if err != nil {
		return api.RouteDecision{}, api.ErrPass
	}
	switch resp.Decision {
	case "use_default":
		return api.RouteDecision{Action: api.RouteDecisionPass, ProviderID: p.pluginID, Reason: firstNonEmpty(resp.Reason, "wasm route pass")}, nil
	case "override":
		return api.RouteDecision{Action: api.RouteDecisionOverride, Upstream: resp.Route, Host: req.Host, ProviderID: p.pluginID, Reason: firstNonEmpty(resp.Reason, "wasm route override")}, nil
	case "reject":
		return api.RouteDecision{Action: api.RouteDecisionReject, Host: req.Host, ProviderID: p.pluginID, Reason: firstNonEmpty(resp.Reason, "wasm route reject")}, nil
	default:
		return api.RouteDecision{}, api.ErrPass
	}
}

func (p *wasmHostedPlugin) dispatch(ctx context.Context, point string, req WASMABIRequest) (WASMABIResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	snapshot, finish, err := p.beginDispatch(point)
	if err != nil {
		p.recordDispatch(point, err)
		return WASMABIResponse{}, err
	}
	defer finish()
	if len(req.Config) == 0 {
		req.Config = append(json.RawMessage(nil), snapshot.configJSON...)
	}
	if snapshot.dispatchHook != nil {
		resp, hookErr, handled := snapshot.dispatchHook(ctx, snapshot, point, req)
		if handled {
			p.recordDispatch(point, hookErr)
			return resp, hookErr
		}
	}
	resp, err := snapshot.runner.Dispatch(ctx, WASMDispatchInvocation{
		ArtifactID:     snapshot.artifact.ID,
		PluginID:       snapshot.pluginID,
		Module:         snapshot.module,
		Manifest:       snapshot.manifest,
		ExtensionPoint: point,
		Request:        req,
		Timeout:        wasmTimeout(snapshot.manifest),
		MemoryBytes:    snapshot.manifest.RuntimeLimits.MemoryBytes,
		MaxOutputBytes: wasmABIMaxOutputBytes,
	})
	p.recordDispatch(point, err)
	return resp, err
}

func (p *wasmHostedPlugin) beginDispatch(point string) (wasmRuntimeSnapshot, func(), error) {
	if p == nil {
		return wasmRuntimeSnapshot{}, nil, errors.New("wasm plugin instance is nil")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	state := p.state
	if state == "" {
		state = RuntimeEnabled
		p.state = state
	}
	switch state {
	case RuntimeEnabled:
	case RuntimeDraining:
		return wasmRuntimeSnapshot{}, nil, fmt.Errorf("wasm plugin %q is draining; refusing new %s dispatch", p.pluginID, point)
	case RuntimeStopped:
		return wasmRuntimeSnapshot{}, nil, fmt.Errorf("wasm plugin %q is stopped; refusing new %s dispatch", p.pluginID, point)
	default:
		return wasmRuntimeSnapshot{}, nil, fmt.Errorf("wasm plugin %q is not running; state=%s", p.pluginID, state)
	}
	if p.runner == nil || len(p.module) == 0 {
		return wasmRuntimeSnapshot{}, nil, fmt.Errorf("wasm plugin %q runtime is released", p.pluginID)
	}
	p.activeCalls++
	snapshot := wasmRuntimeSnapshot{
		runner:             p.runner,
		manifest:           p.manifest,
		artifact:           p.artifact,
		pluginID:           p.pluginID,
		module:             append([]byte(nil), p.module...),
		configJSON:         append(json.RawMessage(nil), p.configJSON...),
		moduleHash:         p.moduleHash,
		moduleCacheStatus:  p.moduleCacheStatus,
		artifactGeneration: p.artifactGeneration,
		dispatchHook:       p.dispatchHook,
	}
	return snapshot, func() {
		p.mu.Lock()
		if p.activeCalls > 0 {
			p.activeCalls--
		}
		p.mu.Unlock()
	}, nil
}

func (p *wasmHostedPlugin) drain(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	waitCtx := ctx
	cancel := func() {}
	if _, ok := ctx.Deadline(); !ok {
		waitCtx, cancel = context.WithTimeout(ctx, wasmTimeout(p.manifest))
	}
	defer cancel()
	p.mu.Lock()
	if p.state == RuntimeStopped {
		p.mu.Unlock()
		return nil
	}
	p.state = RuntimeDraining
	if p.drainingAt == 0 {
		p.drainingAt = time.Now().Unix()
	}
	p.mu.Unlock()

	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		p.mu.Lock()
		active := p.activeCalls
		p.mu.Unlock()
		if active == 0 {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("wasm plugin %q drain timed out with %d active calls: %w", p.pluginID, active, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func (p *wasmHostedPlugin) stop(ctx context.Context) error {
	err := p.drain(ctx)
	p.mu.Lock()
	p.state = RuntimeStopped
	p.stoppedAt = time.Now().Unix()
	p.runner = nil
	p.module = nil
	p.configJSON = nil
	p.mu.Unlock()
	return err
}

func (p *wasmHostedPlugin) lifecycleState() string {
	if p == nil {
		return RuntimeFailed
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state == "" {
		return RuntimeEnabled
	}
	return p.state
}

func (p *wasmHostedPlugin) recordDispatch(point string, err error) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastPoint = point
	p.lastAt = time.Now().Unix()
	if err != nil {
		p.lastOK = false
		p.lastError = err.Error()
		switch wasmABIErrorCode(err) {
		case wasmABIErrorTrap:
			p.trapCount++
		case wasmABIErrorTimeout:
			p.timeoutCount++
		case wasmABIErrorMemoryExceeded:
			p.memoryErrorCount++
		}
		return
	}
	p.lastOK = true
	p.lastError = ""
}

func (p *wasmHostedPlugin) diagnosticsSummary() map[string]any {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return map[string]any{
		"state":                  firstNonEmpty(p.state, RuntimeEnabled),
		"host_abi":               wasmHostABIV1,
		"module_hash":            p.moduleHash,
		"module_cache_status":    p.moduleCacheStatus,
		"artifact_generation":    p.artifactGeneration,
		"handler_timeout_ms":     p.manifest.RuntimeLimits.HandlerTimeoutMS,
		"memory_bytes":           p.manifest.RuntimeLimits.MemoryBytes,
		"active_calls":           p.activeCalls,
		"trap_count":             p.trapCount,
		"timeout_count":          p.timeoutCount,
		"memory_error_count":     p.memoryErrorCount,
		"started_at":             p.startedAt,
		"draining_at":            p.drainingAt,
		"stopped_at":             p.stoppedAt,
		"last_extension_point":   p.lastPoint,
		"last_error":             p.lastError,
		"last_dispatch_at":       p.lastAt,
		"last_dispatch_ok":       p.lastOK,
		"runtime_module_loaded":  len(p.module) > 0,
		"runtime_runner_loaded":  p.runner != nil,
		"supported_extensions":   wasmManifestExtensionKeys(p.manifest),
		"low_risk_extension_set": true,
	}
}

func wasmManifestExtensionKeys(manifest Manifest) []string {
	keys := make([]string, 0, len(manifest.ExtensionPoints))
	for _, point := range manifest.ExtensionPoints {
		keys = append(keys, point.Key)
	}
	return keys
}

func wasmABIExportMap() map[string]string {
	return map[string]string{
		ExtensionConfigValidate: wasmExportConfigValidateV1,
		ExtensionRuleEvaluate:   wasmExportRuleEvaluateV1,
		ExtensionRouteResolve:   wasmExportRouteResolveV1,
	}
}

func wasmABIImportMap() map[string]string {
	return map[string]string{
		wasmHostImportModule + "." + wasmHostImportLog:    "log(level_i32, message_ptr_i32, message_len_i32) -> status_i32",
		wasmHostImportModule + "." + wasmHostImportMetric: "metric(name_ptr_i32, name_len_i32, labels_ptr_i32, labels_len_i32, value_f64) -> status_i32",
	}
}

func wasmFixtureModule(behavior string) []byte {
	switch behavior {
	case "timeout":
		return wasmLoopModule
	case "panic", "trap":
		return wasmTrapModule
	case "memory":
		return wasmMemoryGrowModule
	default:
		return wasmOKModule
	}
}

var (
	wasmOKModule         = wasmABIExportModule([]byte{0x00, 0x42, 0x00, 0x0b}, false)
	wasmTrapModule       = wasmABIExportModule([]byte{0x00, 0x00, 0x0b}, false)
	wasmLoopModule       = wasmABIExportModule([]byte{0x00, 0x03, 0x40, 0x0c, 0x00, 0x0b, 0x0b}, false)
	wasmMemoryGrowModule = wasmABIExportModule([]byte{0x00, 0x41, 0x02, 0x40, 0x00, 0xad, 0x0b}, true)
)

func wasmABIExportModule(functionBody []byte, memory bool) []byte {
	module := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	module = append(module, wasmSection(1, []byte{0x01, 0x60, 0x02, 0x7f, 0x7f, 0x01, 0x7e})...)
	module = append(module, wasmSection(3, []byte{0x01, 0x00})...)
	if memory {
		module = append(module, wasmSection(5, []byte{0x01, 0x01, 0x00, 0x01})...)
	}
	module = append(module, wasmSection(7, wasmABIExportSection())...)
	body := appendU32LEB(nil, uint32(len(functionBody)))
	body = append(body, functionBody...)
	code := appendU32LEB(nil, 1)
	code = append(code, body...)
	module = append(module, wasmSection(10, code)...)
	return module
}

func wasmABIExportSection() []byte {
	exports := []string{wasmExportConfigValidateV1, wasmExportRuleEvaluateV1, wasmExportRouteResolveV1}
	payload := appendU32LEB(nil, uint32(len(exports)))
	for _, name := range exports {
		payload = appendU32LEB(payload, uint32(len(name)))
		payload = append(payload, name...)
		payload = append(payload, 0x00, 0x00)
	}
	return payload
}

func wasmSection(id byte, payload []byte) []byte {
	section := []byte{id}
	section = appendU32LEB(section, uint32(len(payload)))
	section = append(section, payload...)
	return section
}

func appendU32LEB(out []byte, value uint32) []byte {
	for {
		b := byte(value & 0x7f)
		value >>= 7
		if value != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if value == 0 {
			return out
		}
	}
}
