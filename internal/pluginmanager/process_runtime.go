package pluginmanager

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/tursom/mc-gateway/plugin/api"
)

type GoPluginProcessAdapter struct {
	Supervisor PluginHostSupervisor
}

type pluginHostCallerContextKey struct{}

type processHostedPlugin struct {
	process *PluginHostSupervisorProcess
}

func (a GoPluginProcessAdapter) Load(ctx context.Context, artifact ArtifactRecord, pluginRecord PluginRecord, gateway *Gateway) (api.Plugin, error) {
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

func (a GoPluginProcessAdapter) ValidateArtifact(ctx context.Context, artifact ArtifactRecord) error {
	if err := (GoPluginAdapter{}).ValidateArtifact(ctx, artifact); err != nil {
		return err
	}
	return nil
}

func (a GoPluginProcessAdapter) Prepare(ctx context.Context, artifact ArtifactRecord, pluginRecord PluginRecord) (RuntimePrepared, error) {
	if err := a.ValidateArtifact(ctx, artifact); err != nil {
		return RuntimePrepared{}, err
	}
	return runtimePreparedFor(artifact, pluginRecord, PluginServiceModeGoPluginProcess), nil
}

func (a GoPluginProcessAdapter) Start(ctx context.Context, prepared RuntimePrepared, artifact ArtifactRecord, pluginRecord PluginRecord, gateway *Gateway) (RuntimeInstance, error) {
	supervisor := a.Supervisor
	process, _, err := supervisor.Start(ctx, pluginRecord.ID, artifact.ID)
	if err != nil {
		return RuntimeInstance{}, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = process.Stop(stopCtx)
		}
	}()
	payload, err := json.Marshal(PluginHostInitRequest{
		PluginID:     pluginRecord.ID,
		ArtifactID:   artifact.ID,
		ArtifactPath: artifact.FilePath,
		EntrySymbol:  pluginHostEntrySymbol(artifact),
		ConfigJSON:   pluginRecord.ConfigJSON,
	})
	if err != nil {
		return RuntimeInstance{}, err
	}
	resp, err := SendPluginHostControlRequest(ctx, process.SocketPath, PluginHostControlRequest{
		Command:  PluginHostCommandInit,
		Protocol: process.Protocol,
		Payload:  payload,
	})
	if err != nil {
		return RuntimeInstance{}, err
	}
	if !resp.OK || resp.Lifecycle == nil {
		if resp.Error != "" {
			return RuntimeInstance{}, errors.New(resp.Error)
		}
		return RuntimeInstance{}, errors.New("plugin-host init failed")
	}
	if pluginHostHasHook(resp.Lifecycle.RegisteredHooks, api.HookUpstreamConnectV2.Key()) {
		if err := api.RegisterUpstreamConnectHandlerV2(gateway, func(req api.UpstreamConnectRequestV2) error {
			return RunPluginHostTakeover(pluginHostCallerContext(req.Context), process.SocketPath, req)
		}); err != nil {
			return RuntimeInstance{}, err
		}
	}
	cleanup = false
	return RuntimeInstance{
		RuntimePrepared: prepared,
		Plugin:          processHostedPlugin{process: process},
		HostProcess:     process,
		StartedAt:       time.Now().Unix(),
	}, nil
}

func (a GoPluginProcessAdapter) HealthCheck(_ context.Context, instance RuntimeInstance) RuntimeHealth {
	now := time.Now().Unix()
	if instance.HostProcess == nil {
		return RuntimeHealth{OK: false, Status: RuntimeFailed, Error: "plugin-host process is nil", CheckedAt: now}
	}
	summary := instance.HostProcess.Summary()
	return RuntimeHealth{
		OK:        !summary.CrashLoop,
		Status:    summary.State,
		Error:     summary.LastError,
		Details:   pluginHostRuntimeSummaryDetails(summary),
		CheckedAt: now,
	}
}

func (a GoPluginProcessAdapter) ReloadConfig(ctx context.Context, instance RuntimeInstance, configJSON string) error {
	if instance.HostProcess == nil {
		return errors.New("plugin-host process is nil")
	}
	payload, err := json.Marshal(PluginHostReloadConfigRequest{ConfigJSON: configJSON})
	if err != nil {
		return err
	}
	resp, err := SendPluginHostControlRequest(ctx, instance.HostProcess.SocketPath, PluginHostControlRequest{
		Command:  PluginHostCommandReloadConfig,
		Protocol: instance.HostProcess.Protocol,
		Payload:  payload,
	})
	if err != nil {
		return err
	}
	if !resp.OK {
		if resp.Error != "" {
			return errors.New(resp.Error)
		}
		return errors.New("plugin-host reload_config failed")
	}
	return nil
}

func (a GoPluginProcessAdapter) Drain(ctx context.Context, instance RuntimeInstance) error {
	if instance.HostProcess == nil {
		return nil
	}
	resp, err := SendPluginHostControlRequest(ctx, instance.HostProcess.SocketPath, PluginHostControlRequest{
		Command:  PluginHostCommandDrain,
		Protocol: instance.HostProcess.Protocol,
	})
	if err != nil {
		return err
	}
	if !resp.OK && resp.Error != "" {
		return errors.New(resp.Error)
	}
	return nil
}

func (a GoPluginProcessAdapter) Stop(ctx context.Context, instance RuntimeInstance) error {
	if instance.HostProcess == nil {
		return nil
	}
	return instance.HostProcess.Stop(ctx)
}

func (a GoPluginProcessAdapter) Diagnostics(_ context.Context, instance RuntimeInstance) RuntimeAdapterDiagnostics {
	details := map[string]any{"adapter": "go-plugin-process-host"}
	state := RuntimeNotLoaded
	if instance.HostProcess != nil {
		summary := instance.HostProcess.Summary()
		state = summary.State
		for key, value := range pluginHostRuntimeSummaryDetails(summary) {
			details[key] = value
		}
	}
	return RuntimeAdapterDiagnostics{
		PluginID:   instance.PluginID,
		ArtifactID: instance.ArtifactID,
		Runtime:    instance.Runtime,
		Mode:       instance.Mode,
		State:      state,
		Details:    details,
		CreatedAt:  time.Now().Unix(),
	}
}

func pluginHostRuntimeSummaryDetails(summary PluginHostRuntimeSummary) map[string]any {
	return map[string]any{
		"pid":           summary.PID,
		"started_at":    summary.StartedAt,
		"draining_at":   summary.DrainingAt,
		"exited_at":     summary.ExitedAt,
		"drain_mode":    summary.DrainMode,
		"crash_loop":    summary.CrashLoop,
		"crash_count":   summary.CrashCount,
		"last_error":    summary.LastError,
		"last_crash_at": summary.LastCrashAt,
		"backoff_until": summary.BackoffUntil,
		"isolated":      summary.Isolated,
	}
}

func (p processHostedPlugin) Init(api.Gateway) error { return nil }

func (p processHostedPlugin) Destroy() error { return nil }

func (p processHostedPlugin) NewConfigObj() any { return struct{}{} }

func (p processHostedPlugin) ReloadConfig(any) error { return nil }

func pluginHostEntrySymbol(artifact ArtifactRecord) string {
	var manifest Manifest
	if err := json.Unmarshal([]byte(artifact.MetadataJSON), &manifest); err == nil {
		return manifest.Runtime.EntrySymbol
	}
	return ""
}

func pluginHostHasHook(hooks []string, want string) bool {
	for _, hook := range hooks {
		if hook == want {
			return true
		}
	}
	return false
}

func pluginHostTakeoverRequest(req api.UpstreamConnectRequestV2) PluginHostTakeoverRequest {
	out := PluginHostTakeoverRequest{
		SessionID:           newSandboxStreamID(),
		ConnectionID:        req.ConnectionID,
		TraceID:             req.TraceID,
		PeerAddr:            req.PeerAddr,
		LocalAddr:           req.LocalAddr,
		EffectiveSourceAddr: req.Connection.EffectiveSourceAddr,
		Metadata:            copyStringMap(req.Connection.Metadata),
		Ingress:             req.Ingress,
	}
	if ctx := pluginHostCallerContext(req.Context); ctx != nil {
		if deadline, ok := ctx.Deadline(); ok {
			out.DeadlineUnixMS = deadline.UnixMilli()
		}
	}
	return out
}

func pluginHostCallerContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	if caller, ok := ctx.Value(pluginHostCallerContextKey{}).(context.Context); ok && caller != nil {
		return caller
	}
	return ctx
}
