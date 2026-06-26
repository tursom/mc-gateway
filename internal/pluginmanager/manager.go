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
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tursom/mc-gateway/plugin/api"
)

type RuntimeAdapter interface {
	Load(ctx context.Context, artifact ArtifactRecord, pluginRecord PluginRecord, gateway *Gateway) (api.Plugin, error)
}

type GoPluginAdapter struct{}

func (a GoPluginAdapter) Load(ctx context.Context, artifact ArtifactRecord, pluginRecord PluginRecord, gateway *Gateway) (api.Plugin, error) {
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
	if err := instance.Init(gateway); err != nil {
		return nil, err
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
	repo       Repository
	store      ArtifactStore
	adapter    RuntimeAdapter
	handleConn func(net.Conn)
	wg         *sync.WaitGroup

	mu       sync.Mutex
	loaded   map[string]*loadedPlugin
	snapshot atomic.Value
}

type loadedPlugin struct {
	record   PluginRecord
	artifact ArtifactRecord
	instance api.Plugin
	gateway  *Gateway
	handlers []*upstreamHandler
}

type upstreamHandler struct {
	pluginID   string
	artifactID string
	priority   int
	handlerID  string
	timeout    time.Duration
	accept     func(api.UpstreamConnectRequest) bool
	handle     func(api.UpstreamConnectRequest) (net.Conn, error)

	calls    atomic.Uint64
	errors   atomic.Uint64
	panics   atomic.Uint64
	timeouts atomic.Uint64
}

type Options struct {
	DB           *sql.DB
	ArtifactRoot string
	HandleConn   func(net.Conn)
	WaitGroup    *sync.WaitGroup
	Adapter      RuntimeAdapter
}

func New(options Options) *Manager {
	adapter := options.Adapter
	if adapter == nil {
		adapter = GoPluginAdapter{}
	}
	manager := &Manager{
		repo:       NewRepository(options.DB),
		store:      NewArtifactStore(options.ArtifactRoot),
		adapter:    adapter,
		handleConn: options.HandleConn,
		wg:         options.WaitGroup,
		loaded:     make(map[string]*loadedPlugin),
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

func (m *Manager) SetDesired(ctx context.Context, actor, pluginID, artifactID, desiredState, configJSON string, priority int) (PluginRecord, error) {
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
	m.mu.Lock()
	defer m.mu.Unlock()

	pluginRecord, err := m.repo.Plugin(ctx, pluginID)
	if err != nil {
		return PluginRecord{}, err
	}
	if pluginRecord.DesiredState != DesiredEnabled {
		pluginRecord, err = m.repo.UpsertDesired(ctx, actor, pluginRecord.ID, pluginRecord.DesiredArtifactID, DesiredEnabled, pluginRecord.ConfigJSON, pluginRecord.Priority)
		if err != nil {
			return PluginRecord{}, err
		}
	}
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
	m.publish(next)
	_ = m.repo.UpdateArtifactStatus(ctx, loaded.artifact.ID, ArtifactStatusLoaded, "")
	_ = m.repo.RecordOperation(ctx, pluginID, loaded.artifact.ID, "enable", "succeeded", actor, "plugin enabled", map[string]any{
		"desired_generation": loaded.record.DesiredGeneration,
		"handler_count":      len(loaded.handlers),
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
	if loaded := m.loaded[pluginID]; loaded != nil && loaded.instance != nil {
		if err := loaded.instance.Destroy(); err != nil {
			_ = m.repo.RecordOperation(ctx, pluginID, loaded.artifact.ID, "disable", "warning", actor, err.Error(), nil)
		}
	}
	delete(m.loaded, pluginID)
	if err := m.repo.MarkRuntime(ctx, pluginID, RuntimeDisabled, "", "", pluginRecord.DesiredGeneration, "", nil, nil); err != nil {
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
	for _, handler := range handlers {
		accepted, err := handler.accepts(req)
		if err != nil {
			return UpstreamResult{Handled: true}, err
		}
		if !accepted {
			continue
		}
		conn, err := handler.invoke(req)
		if errors.Is(err, api.ErrPass) {
			continue
		}
		if err != nil {
			return UpstreamResult{Handled: true}, err
		}
		if conn != nil {
			return UpstreamResult{Conn: conn, Handled: true}, nil
		}
	}
	return UpstreamResult{}, nil
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
	if artifact.Status == ArtifactStatusDeleted || artifact.Status == ArtifactStatusRejected {
		return nil, fmt.Errorf("artifact status %q is not loadable", artifact.Status)
	}

	gateway := NewGateway(pluginRecord.ID, m.handleConn, m.wg)
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
	return loaded, nil
}

func (m *Manager) markEnabled(ctx context.Context, loaded *loadedPlugin) error {
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
	var manifest Manifest
	if err := json.Unmarshal([]byte(artifact.MetadataJSON), &manifest); err == nil && manifest.RuntimeLimits.HandlerTimeoutMS > 0 {
		timeout = time.Duration(manifest.RuntimeLimits.HandlerTimeoutMS) * time.Millisecond
	}
	var handlers []*upstreamHandler
	if hook, ok := gateway.UpstreamConnectHandler(); ok {
		handlers = append(handlers, &upstreamHandler{
			pluginID:   pluginRecord.ID,
			artifactID: artifact.ID,
			priority:   pluginRecord.Priority,
			handlerID:  "upstream.connect/v1",
			timeout:    timeout,
			accept:     hook.Acceptor(),
			handle:     hook.Handler(),
		})
	}
	if hook, ok := gateway.LegacyUpstreamHandler(); ok {
		acceptor := hook.Acceptor()
		handler := hook.Handler()
		handlers = append(handlers, &upstreamHandler{
			pluginID:   pluginRecord.ID,
			artifactID: artifact.ID,
			priority:   pluginRecord.Priority,
			handlerID:  "legacy-upstream",
			timeout:    timeout,
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

func (h *upstreamHandler) invoke(req api.UpstreamConnectRequest) (conn net.Conn, err error) {
	h.calls.Add(1)
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
			PluginID:       handler.pluginID,
			ArtifactID:     handler.artifactID,
			Priority:       handler.priority,
			HandlerID:      handler.handlerID,
			ExtensionPoint: ExtensionUpstreamConnect,
			TimeoutMS:      handler.timeout.Milliseconds(),
			Calls:          handler.calls.Load(),
			Errors:         handler.errors.Load(),
			Panics:         handler.panics.Load(),
			Timeouts:       handler.timeouts.Load(),
		})
	}
	return summaries
}
