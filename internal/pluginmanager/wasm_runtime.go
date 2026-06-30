package pluginmanager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"

	"github.com/tursom/mc-gateway/plugin/api"
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

type WASMAdapter struct {
	Runner *WASMRunner
	Mode   string
}

type wasmHostedPlugin struct {
	runner   *WASMRunner
	manifest Manifest
	artifact ArtifactRecord
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

func (r *WASMRunner) compile(ctx context.Context, runtime wazero.Runtime, invocation WASMInvocation) (wazero.CompiledModule, error) {
	key := invocation.ArtifactID
	if key == "" {
		sum := sha256.Sum256(invocation.Module)
		key = hex.EncodeToString(sum[:])
	}
	r.mu.Lock()
	cached := append([]byte(nil), r.compiled[key]...)
	if len(cached) > 0 {
		r.mu.Unlock()
		return runtime.CompileModule(ctx, cached)
	}
	r.mu.Unlock()
	compiled, err := runtime.CompileModule(ctx, invocation.Module)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.compiled[key] = append([]byte(nil), invocation.Module...)
	r.mu.Unlock()
	return compiled, nil
}

func validateWASMExtensionPoints(manifest Manifest) error {
	for _, point := range manifest.ExtensionPoints {
		switch point.Key {
		case ExtensionRuleEvaluate, ExtensionRouteResolve, ExtensionConfigValidate:
		default:
			return fmt.Errorf("wasm extension point %q is not supported by contained validation", point.Key)
		}
	}
	return nil
}

func validateWASMArtifactABI(ctx context.Context, artifact ArtifactRecord, manifest Manifest) error {
	if err := validateWASMManifestABI(manifest); err != nil {
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

func (a WASMAdapter) Start(ctx context.Context, prepared RuntimePrepared, artifact ArtifactRecord, _ PluginRecord, _ *Gateway) (RuntimeInstance, error) {
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
	return RuntimeInstance{
		RuntimePrepared: prepared,
		Plugin:          wasmHostedPlugin{runner: runner, manifest: manifest, artifact: artifact},
		StartedAt:       time.Now().Unix(),
	}, nil
}

func (a WASMAdapter) HealthCheck(_ context.Context, _ RuntimeInstance) RuntimeHealth {
	return RuntimeHealth{
		OK:     true,
		Status: RuntimeEnabled,
		Details: map[string]any{
			"host_abi":                wasmHostABIV1,
			"exports":                 wasmABIExportMap(),
			"imports":                 wasmABIImportMap(),
			"module_cache":            true,
			"default_filesystem":      "none",
			"default_network":         "none",
			"low_risk_extension_only": true,
		},
		CheckedAt: time.Now().Unix(),
	}
}

func (a WASMAdapter) ReloadConfig(context.Context, RuntimeInstance, string) error { return nil }

func (a WASMAdapter) Drain(context.Context, RuntimeInstance) error { return nil }

func (a WASMAdapter) Stop(context.Context, RuntimeInstance) error { return nil }

func (a WASMAdapter) Diagnostics(_ context.Context, instance RuntimeInstance) RuntimeAdapterDiagnostics {
	return RuntimeAdapterDiagnostics{
		PluginID:   instance.PluginID,
		ArtifactID: instance.ArtifactID,
		Runtime:    instance.Runtime,
		Mode:       instance.Mode,
		State:      RuntimeEnabled,
		Details: map[string]any{
			"host_abi":                   wasmHostABIV1,
			"exports":                    wasmABIExportMap(),
			"imports":                    wasmABIImportMap(),
			"module_cache":               true,
			"fuel_equivalent_time_limit": true,
			"memory_limit":               true,
			"default_filesystem":         "none",
			"default_network":            "none",
		},
		CreatedAt: time.Now().Unix(),
	}
}

func (p wasmHostedPlugin) Init(api.Gateway) error { return nil }

func (p wasmHostedPlugin) Destroy() error { return nil }

func (p wasmHostedPlugin) NewConfigObj() any { return struct{}{} }

func (p wasmHostedPlugin) ReloadConfig(any) error { return nil }

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
