package pluginmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"time"

	"github.com/tursom/mc-gateway/plugin/api"
)

type RuntimePrepared struct {
	PluginID   string
	ArtifactID string
	Runtime    string
	Mode       string
	PreparedAt int64
}

type RuntimeInstance struct {
	RuntimePrepared
	Plugin      api.Plugin
	HostProcess *PluginHostSupervisorProcess
	StartedAt   int64
}

type RuntimeHealth struct {
	OK        bool           `json:"ok"`
	Status    string         `json:"status"`
	Error     string         `json:"error,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
	CheckedAt int64          `json:"checked_at"`
}

type RuntimeAdapterDiagnostics struct {
	PluginID   string         `json:"plugin_id"`
	ArtifactID string         `json:"artifact_id"`
	Runtime    string         `json:"runtime"`
	Mode       string         `json:"mode"`
	State      string         `json:"state"`
	Details    map[string]any `json:"details,omitempty"`
	CreatedAt  int64          `json:"created_at"`
}

type RuntimeAdapterLifecycle interface {
	ValidateArtifact(ctx context.Context, artifact ArtifactRecord) error
	Prepare(ctx context.Context, artifact ArtifactRecord, pluginRecord PluginRecord) (RuntimePrepared, error)
	Start(ctx context.Context, prepared RuntimePrepared, artifact ArtifactRecord, pluginRecord PluginRecord, gateway *Gateway) (RuntimeInstance, error)
	HealthCheck(ctx context.Context, instance RuntimeInstance) RuntimeHealth
	ReloadConfig(ctx context.Context, instance RuntimeInstance, configJSON string) error
	Drain(ctx context.Context, instance RuntimeInstance) error
	Stop(ctx context.Context, instance RuntimeInstance) error
	Diagnostics(ctx context.Context, instance RuntimeInstance) RuntimeAdapterDiagnostics
}

type RuntimeAdapterFactoryStatus struct {
	ServiceMode       string `json:"service_mode"`
	RuntimeType       string `json:"runtime_type"`
	Adapter           string `json:"adapter"`
	Implemented       bool   `json:"implemented"`
	Maturity          string `json:"maturity"`
	DataPlane         bool   `json:"data_plane"`
	Lifecycle         bool   `json:"lifecycle"`
	RequiresRestart   bool   `json:"requires_restart"`
	HostProtocol      string `json:"host_protocol,omitempty"`
	ControlChannel    string `json:"control_channel,omitempty"`
	UnsupportedReason string `json:"unsupported_reason,omitempty"`
}

type RuntimeAdapterFactory struct {
	Facts RuntimeFeatureFactsOptions
}

func RuntimeAdapterFactoryStatuses() []RuntimeAdapterFactoryStatus {
	return RuntimeAdapterFactoryStatusesFor(RuntimeFeatureFactsOptions{})
}

func RuntimeAdapterFactoryStatusesFor(options RuntimeFeatureFactsOptions) []RuntimeAdapterFactoryStatus {
	factory := RuntimeAdapterFactory{Facts: options}
	pairs := []struct {
		mode        string
		runtimeType string
	}{
		{PluginServiceModeInProcess, RuntimeGoPlugin},
		{PluginServiceModeInProcess, RuntimeBuiltin},
		{PluginServiceModeInProcess, RuntimeWASM},
		{PluginServiceModeGoPluginProcess, RuntimeGoPlugin},
		{PluginServiceModeSandboxProcess, RuntimeSandbox},
		{PluginServiceModeSandboxProcess, RuntimeWASM},
	}
	statuses := make([]RuntimeAdapterFactoryStatus, 0, len(pairs))
	for _, pair := range pairs {
		_, status := factory.AdapterFor(pair.mode, pair.runtimeType)
		statuses = append(statuses, status)
	}
	return statuses
}

func (factory RuntimeAdapterFactory) AdapterFor(serviceMode, runtimeType string) (RuntimeAdapter, RuntimeAdapterFactoryStatus) {
	if serviceMode == "" {
		serviceMode = PluginServiceModeInProcess
	}
	facts := normalizeRuntimeFeatureFactsOptions(factory.Facts)
	status := RuntimeAdapterFactoryStatus{
		ServiceMode: serviceMode,
		RuntimeType: runtimeType,
	}
	completeStatus := func() RuntimeAdapterFactoryStatus {
		if status.RuntimeType == "" {
			status.RuntimeType = RuntimeGoPlugin
		}
		if status.Maturity != "" {
			return status
		}
		modeMaturity := PluginServiceModeFeatureForOptions(status.ServiceMode, facts).Maturity
		runtimeMaturity := RuntimeTypeFeatureFor(status.RuntimeType, facts).Maturity
		status.Maturity = FeatureMaturityImplemented
		switch {
		case modeMaturity == FeatureMaturityStub || runtimeMaturity == FeatureMaturityStub:
			status.Maturity = FeatureMaturityStub
		case modeMaturity == FeatureMaturityReserved || runtimeMaturity == FeatureMaturityReserved:
			status.Maturity = FeatureMaturityReserved
		case modeMaturity == FeatureMaturityPartial || runtimeMaturity == FeatureMaturityPartial:
			status.Maturity = FeatureMaturityPartial
		}
		return status
	}
	switch serviceMode {
	case PluginServiceModeInProcess:
		switch runtimeType {
		case "", RuntimeGoPlugin, RuntimeBuiltin:
			status.RuntimeType = runtimeType
			if status.RuntimeType == "" {
				status.RuntimeType = RuntimeGoPlugin
			}
			status.Adapter = "go-plugin-in-process"
			status.Implemented = true
			status.DataPlane = true
			status.Lifecycle = true
			return GoPluginAdapter{}, completeStatus()
		case RuntimeSandbox:
			status.Adapter = "sandbox-process"
			status.RequiresRestart = true
			status.UnsupportedReason = "sandbox-process runtime requires sandbox-process service mode"
		case RuntimeWASM:
			status.Adapter = "wasm"
			status.Implemented = true
			status.DataPlane = true
			status.Lifecycle = true
			status.ControlChannel = "wazero-host-abi"
			status.UnsupportedReason = "wasm runtime only supports low-risk extension points; protocol-proxy, network, file, and high-risk extension points are not supported"
			return WASMAdapter{Mode: PluginServiceModeInProcess}, completeStatus()
		default:
			status.Adapter = "unknown"
			status.RequiresRestart = true
			status.UnsupportedReason = "runtime type is not recognized by this gateway"
		}
	case PluginServiceModeGoPluginProcess:
		status.Adapter = "go-plugin-process-host"
		status.Implemented = true
		status.DataPlane = true
		status.Lifecycle = true
		status.RequiresRestart = true
		status.HostProtocol = PluginHostProtocol
		status.ControlChannel = PluginHostControlChannelUnix
		status.UnsupportedReason = GoPluginProcessPartialUnsupportedReason
		return GoPluginProcessAdapter{}, completeStatus()
	case PluginServiceModeSandboxProcess:
		status.RequiresRestart = true
		switch runtimeType {
		case RuntimeSandbox:
			modeFeature := PluginServiceModeFeatureForOptions(PluginServiceModeSandboxProcess, facts)
			runtimeFeature := RuntimeTypeFeatureFor(RuntimeSandbox, facts)
			status.Adapter = "sandbox-process"
			status.ControlChannel = "sandbox-control-rpc"
			status.Implemented = modeFeature.Implemented && runtimeFeature.Implemented
			status.DataPlane = modeFeature.DataPlane && runtimeFeature.DataPlane
			status.Lifecycle = status.DataPlane
			status.UnsupportedReason = firstRuntimeUnsupportedReason(modeFeature.UnsupportedReason, runtimeFeature.UnsupportedReason)
			return SandboxProcessAdapter{Policy: facts.SandboxPolicy, SelfCheck: facts.SandboxSelfCheck}, completeStatus()
		case RuntimeWASM:
			status.Adapter = "wazero"
			status.ControlChannel = "wazero-host-abi"
			status.Maturity = FeatureMaturityReserved
			status.UnsupportedReason = "sandbox-process wasm adapter is reserved; use in-process wasm for the current low-risk WASM data-plane"
			return WASMAdapter{Mode: PluginServiceModeSandboxProcess}, completeStatus()
		default:
			status.Adapter = "sandbox-process"
			status.UnsupportedReason = "sandbox-process service mode supports sandbox-process and wasm runtimes"
		}
	default:
		status.Adapter = "unknown"
		status.RequiresRestart = true
		status.UnsupportedReason = "plugin service mode is not recognized by this gateway"
	}
	status = completeStatus()
	return UnsupportedRuntimeAdapter{Status: status}, status
}

func firstRuntimeUnsupportedReason(reasons ...string) string {
	for _, reason := range reasons {
		if reason != "" {
			return reason
		}
	}
	return ""
}

type UnsupportedRuntimeAdapter struct {
	Status RuntimeAdapterFactoryStatus
}

func (a UnsupportedRuntimeAdapter) Load(context.Context, ArtifactRecord, PluginRecord, *Gateway) (api.Plugin, error) {
	return nil, a.err()
}

func (a UnsupportedRuntimeAdapter) ValidateArtifact(context.Context, ArtifactRecord) error {
	return a.err()
}

func (a UnsupportedRuntimeAdapter) Prepare(context.Context, ArtifactRecord, PluginRecord) (RuntimePrepared, error) {
	return RuntimePrepared{}, a.err()
}

func (a UnsupportedRuntimeAdapter) Start(context.Context, RuntimePrepared, ArtifactRecord, PluginRecord, *Gateway) (RuntimeInstance, error) {
	return RuntimeInstance{}, a.err()
}

func (a UnsupportedRuntimeAdapter) HealthCheck(context.Context, RuntimeInstance) RuntimeHealth {
	now := time.Now().Unix()
	return RuntimeHealth{OK: false, Status: RuntimeFailed, Error: a.err().Error(), CheckedAt: now}
}

func (a UnsupportedRuntimeAdapter) ReloadConfig(context.Context, RuntimeInstance, string) error {
	return a.err()
}

func (a UnsupportedRuntimeAdapter) Drain(context.Context, RuntimeInstance) error {
	return a.err()
}

func (a UnsupportedRuntimeAdapter) Stop(context.Context, RuntimeInstance) error {
	return nil
}

func (a UnsupportedRuntimeAdapter) Diagnostics(context.Context, RuntimeInstance) RuntimeAdapterDiagnostics {
	return RuntimeAdapterDiagnostics{
		Runtime:   a.Status.RuntimeType,
		Mode:      a.Status.ServiceMode,
		State:     RuntimeFailed,
		CreatedAt: time.Now().Unix(),
		Details: map[string]any{
			"adapter":     a.Status.Adapter,
			"implemented": a.Status.Implemented,
			"data_plane":  a.Status.DataPlane,
			"unsupported": a.Status.UnsupportedReason,
		},
	}
}

func (a UnsupportedRuntimeAdapter) err() error {
	if a.Status.UnsupportedReason != "" {
		return errors.New(a.Status.UnsupportedReason)
	}
	return errors.New("runtime adapter is not implemented")
}

func (a GoPluginAdapter) ValidateArtifact(_ context.Context, artifact ArtifactRecord) error {
	if artifact.RuntimeType == RuntimeBuiltin || artifact.PluginID == "official.rule-policy" {
		return nil
	}
	if artifact.RuntimeType != "" && artifact.RuntimeType != RuntimeGoPlugin {
		return fmt.Errorf("go-plugin adapter does not support runtime %q", artifact.RuntimeType)
	}
	if artifact.ArtifactType != "" && artifact.ArtifactType != ArtifactTypeBinary {
		return fmt.Errorf("go-plugin adapter requires binary artifact, got %q", artifact.ArtifactType)
	}
	if artifact.GoVersion != "" && artifact.GoVersion != runtime.Version() {
		return fmt.Errorf("artifact go_version %q does not match gateway %q", artifact.GoVersion, runtime.Version())
	}
	if (artifact.GOOS != "" && artifact.GOOS != runtime.GOOS) || (artifact.GOARCH != "" && artifact.GOARCH != runtime.GOARCH) {
		return fmt.Errorf("artifact target %s/%s does not match gateway %s/%s", artifact.GOOS, artifact.GOARCH, runtime.GOOS, runtime.GOARCH)
	}
	return nil
}

func (a GoPluginAdapter) Prepare(ctx context.Context, artifact ArtifactRecord, pluginRecord PluginRecord) (RuntimePrepared, error) {
	if err := a.ValidateArtifact(ctx, artifact); err != nil {
		return RuntimePrepared{}, err
	}
	return RuntimePrepared{
		PluginID:   pluginRecord.ID,
		ArtifactID: artifact.ID,
		Runtime:    artifact.RuntimeType,
		Mode:       PluginServiceModeInProcess,
		PreparedAt: time.Now().Unix(),
	}, nil
}

func (a GoPluginAdapter) Start(ctx context.Context, prepared RuntimePrepared, artifact ArtifactRecord, pluginRecord PluginRecord, gateway *Gateway) (RuntimeInstance, error) {
	instance, err := a.instantiate(ctx, artifact, pluginRecord, gateway, true)
	if err != nil {
		return RuntimeInstance{}, err
	}
	return RuntimeInstance{RuntimePrepared: prepared, Plugin: instance, StartedAt: time.Now().Unix()}, nil
}

func (GoPluginAdapter) HealthCheck(_ context.Context, instance RuntimeInstance) RuntimeHealth {
	now := time.Now().Unix()
	if instance.Plugin == nil {
		return RuntimeHealth{OK: false, Status: RuntimeFailed, Error: "plugin instance is nil", CheckedAt: now}
	}
	return RuntimeHealth{OK: true, Status: RuntimeEnabled, CheckedAt: now}
}

func (GoPluginAdapter) ReloadConfig(_ context.Context, instance RuntimeInstance, configJSON string) error {
	if instance.Plugin == nil {
		return errors.New("plugin instance is nil")
	}
	cfg := instance.Plugin.NewConfigObj()
	if cfg != nil && configJSON != "" && canUnmarshalInto(cfg) {
		if err := json.Unmarshal([]byte(configJSON), cfg); err != nil {
			return err
		}
	}
	return instance.Plugin.ReloadConfig(cfg)
}

func (GoPluginAdapter) Drain(context.Context, RuntimeInstance) error {
	return nil
}

func (GoPluginAdapter) Stop(context.Context, RuntimeInstance) error {
	return nil
}

func (GoPluginAdapter) Diagnostics(_ context.Context, instance RuntimeInstance) RuntimeAdapterDiagnostics {
	return RuntimeAdapterDiagnostics{
		PluginID:   instance.PluginID,
		ArtifactID: instance.ArtifactID,
		Runtime:    instance.Runtime,
		Mode:       instance.Mode,
		State:      RuntimeEnabled,
		CreatedAt:  time.Now().Unix(),
		Details: map[string]any{
			"adapter": "go-plugin-in-process",
		},
	}
}
