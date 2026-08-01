package pluginmanager

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Close removes every plugin from the data plane before stopping its owned
// resources. Desired state remains untouched so the next process can reconcile it.
func (m *Manager) Close(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.closeOnce.Do(func() {
		m.closing.Store(true)
		m.closeDone = make(chan struct{})
		go func() {
			defer close(m.closeDone)
			m.closeErr = m.close(ctx)
		}()
	})

	select {
	case <-m.closeDone:
		return m.closeErr
	case <-ctx.Done():
		m.forceCloseProxyConnections()
		return ctx.Err()
	}
}

func (m *Manager) close(ctx context.Context) error {
	var errs []error
	m.mu.Lock()
	m.publish(nil)
	m.publishExtensionsLocked(nil)
	if m.ingressLifecycle != nil {
		m.ingressLifecycle.Close()
	}
	loaded := make([]*loadedPlugin, 0, len(m.loaded))
	for pluginID, plugin := range m.loaded {
		m.markDrainingLocked(pluginID)
		m.markHostDraining(pluginID)
		loaded = append(loaded, plugin)
		delete(m.loaded, pluginID)
	}
	m.mu.Unlock()

	if err := m.stopExternalFeedSchedulers(ctx); err != nil {
		errs = append(errs, fmt.Errorf("stop external feed schedulers: %w", err))
	}
	errs = append(errs, runManagerClosePhase(ctx, "drain", loaded, func(plugin *loadedPlugin) error {
		if lifecycle, ok := m.runtimeAdapterForArtifact(plugin.artifact).(RuntimeAdapterLifecycle); ok {
			return lifecycle.Drain(ctx, plugin.runtime)
		}
		return nil
	})...)
	for _, plugin := range loaded {
		m.operations.StopPlugin(plugin.record.ID)
	}
	if err := m.operations.Close(ctx); err != nil {
		errs = append(errs, fmt.Errorf("stop plugin operations: %w", err))
	}
	errs = append(errs, runManagerClosePhase(ctx, "destroy", loaded, func(plugin *loadedPlugin) error {
		if plugin.instance == nil {
			return nil
		}
		return plugin.instance.Destroy()
	})...)
	errs = append(errs, runManagerClosePhase(ctx, "stop runtime", loaded, func(plugin *loadedPlugin) error {
		if lifecycle, ok := m.runtimeAdapterForArtifact(plugin.artifact).(RuntimeAdapterLifecycle); ok {
			if err := lifecycle.Stop(ctx, plugin.runtime); err != nil {
				return err
			}
		}
		if plugin.runtime.HostProcess != nil {
			m.markHostStopped(plugin.record.ID, plugin.artifact.ID, plugin.runtime.HostProcess)
		}
		return nil
	})...)
	errs = append(errs, m.stopPendingRuntimeProcesses(ctx)...)

	if err := waitGroupContext(ctx, m.wg); err != nil {
		m.forceCloseProxyConnections()
		errs = append(errs, fmt.Errorf("wait for plugin goroutines: %w", err))
	}
	return errors.Join(errs...)
}

func (m *Manager) stopPendingRuntimeProcesses(ctx context.Context) []error {
	m.hostMu.Lock()
	pendingHosts := m.pendingHostStops
	pendingSandboxes := m.pendingSandboxStops
	m.pendingHostStops = make(map[string]*PluginHostSupervisorProcess)
	m.pendingSandboxStops = make(map[string]*SandboxProcess)
	m.hostMu.Unlock()

	type pendingStop struct {
		name string
		stop func() error
	}
	stops := make([]pendingStop, 0, len(pendingHosts)+len(pendingSandboxes))
	for pluginID, process := range pendingHosts {
		pluginID, process := pluginID, process
		stops = append(stops, pendingStop{
			name: pluginID,
			stop: func() error {
				err := process.Stop(ctx)
				m.markHostStopped(pluginID, process.ArtifactID, process)
				return err
			},
		})
	}
	for pluginID, process := range pendingSandboxes {
		process := process
		stops = append(stops, pendingStop{name: pluginID, stop: func() error { return process.Stop(ctx) }})
	}
	if len(stops) == 0 {
		return nil
	}

	errCh := make(chan error, len(stops))
	var wg sync.WaitGroup
	for _, pending := range stops {
		pending := pending
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := pending.stop(); err != nil {
				errCh <- fmt.Errorf("stop pending runtime for plugin %q: %w", pending.name, err)
			}
		}()
	}
	if err := waitGroupContext(ctx, &wg); err != nil {
		return []error{fmt.Errorf("stop pending plugin runtimes: %w", err)}
	}
	close(errCh)
	errs := make([]error, 0, len(errCh))
	for err := range errCh {
		errs = append(errs, err)
	}
	return errs
}

func runManagerClosePhase(ctx context.Context, phase string, plugins []*loadedPlugin, run func(*loadedPlugin) error) []error {
	if len(plugins) == 0 {
		return nil
	}
	errCh := make(chan error, len(plugins))
	var wg sync.WaitGroup
	for _, plugin := range plugins {
		plugin := plugin
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := run(plugin); err != nil {
				errCh <- fmt.Errorf("%s plugin %q: %w", phase, plugin.record.ID, err)
			}
		}()
	}
	waitErr := waitGroupContext(ctx, &wg)
	errs := make([]error, 0, len(plugins)+1)
	if waitErr == nil {
		close(errCh)
		for err := range errCh {
			errs = append(errs, err)
		}
		return errs
	}
	errs = append(errs, fmt.Errorf("%s plugins: %w", phase, waitErr))
	for {
		select {
		case err := <-errCh:
			errs = append(errs, err)
		default:
			return errs
		}
	}
}

func waitGroupContext(ctx context.Context, wg *sync.WaitGroup) error {
	if wg == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) forceCloseProxyConnections() {
	if m == nil {
		return
	}
	m.proxyMu.Lock()
	connections := make([]*proxyConnection, 0, len(m.proxyConns))
	for _, connection := range m.proxyConns {
		connection.forceCloseRequested = true
		connections = append(connections, connection)
	}
	m.proxyMu.Unlock()
	for _, connection := range connections {
		_ = connection.client.Close()
		_ = connection.endpoint.Close()
	}
}
