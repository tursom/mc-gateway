package pluginmanager

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tursom/mc-gateway/plugin/api"
)

func TestManagerCloseDestroysPluginBeforeWaitingForExitWaitGroup(t *testing.T) {
	var exitWG sync.WaitGroup
	manager := New(Options{
		DB:           openPluginManagerTestDB(t),
		ArtifactRoot: t.TempDir(),
		Adapter:      &fakeAdapter{},
		WaitGroup:    &exitWG,
	})
	artifact := uploadTestArtifact(t, manager, "shutdown-order")
	if _, err := manager.SetDesired(context.Background(), "admin", "shutdown-order", artifact.ID, DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}

	plugin := &closeWaitGroupPlugin{stop: make(chan struct{})}
	exitWG.Add(1)
	go func() {
		defer exitWG.Done()
		<-plugin.stop
	}()

	manager.mu.Lock()
	manager.loaded["shutdown-order"] = &loadedPlugin{
		record:   PluginRecord{ID: "shutdown-order"},
		artifact: artifact,
		instance: plugin,
	}
	manager.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got := plugin.destroyCalls.Load(); got != 1 {
		t.Fatalf("Destroy() calls = %d, want 1", got)
	}
	if err := manager.Close(ctx); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if got := plugin.destroyCalls.Load(); got != 1 {
		t.Fatalf("Destroy() calls after second Close = %d, want 1", got)
	}
	record, err := manager.Plugin(context.Background(), "shutdown-order")
	if err != nil {
		t.Fatalf("Plugin(after Close) error = %v", err)
	}
	if record.DesiredState != DesiredEnabled {
		t.Fatalf("desired state after Close = %q, want %q", record.DesiredState, DesiredEnabled)
	}
}

func TestManagerCloseRunsLifecycleInOrderAndRejectsNewLoads(t *testing.T) {
	recorder := &closeLifecycleRecorder{}
	adapter := &closeLifecycleAdapter{recorder: recorder}
	manager := New(Options{
		DB:           openPluginManagerTestDB(t),
		ArtifactRoot: t.TempDir(),
		Adapter:      adapter,
	})
	plugin := &recordingClosePlugin{recorder: recorder}
	manager.mu.Lock()
	manager.loaded["lifecycle-order"] = &loadedPlugin{
		record:   PluginRecord{ID: "lifecycle-order"},
		artifact: ArtifactRecord{ID: "lifecycle-order-artifact", PluginID: "lifecycle-order"},
		instance: plugin,
	}
	manager.mu.Unlock()

	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got, want := recorder.snapshot(), []string{"drain", "destroy", "stop"}; !equalStrings(got, want) {
		t.Fatalf("close lifecycle = %v, want %v", got, want)
	}
	if _, err := manager.Load(context.Background(), "admin", "lifecycle-order"); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("Load(after Close) error = %v, want ErrManagerClosed", err)
	}
	if got := manager.DispatchPlan(context.Background()).Handlers; len(got) != 0 {
		t.Fatalf("dispatch handlers after Close = %+v, want empty", got)
	}
}

func TestManagerCloseHonorsContextWhenPluginDoesNotStop(t *testing.T) {
	var exitWG sync.WaitGroup
	manager := New(Options{
		DB:           openPluginManagerTestDB(t),
		ArtifactRoot: t.TempDir(),
		Adapter:      &fakeAdapter{},
		WaitGroup:    &exitWG,
	})
	destroyBlock := make(chan struct{})
	waitBlock := make(chan struct{})
	t.Cleanup(func() {
		close(destroyBlock)
		close(waitBlock)
	})
	plugin := &blockingClosePlugin{block: destroyBlock}
	exitWG.Add(1)
	go func() {
		defer exitWG.Done()
		<-waitBlock
	}()
	manager.mu.Lock()
	manager.loaded["blocked-close"] = &loadedPlugin{
		record:   PluginRecord{ID: "blocked-close"},
		artifact: ArtifactRecord{ID: "blocked-close-artifact", PluginID: "blocked-close"},
		instance: plugin,
	}
	manager.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := manager.Close(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close() error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("Close() elapsed = %v, want bounded by context", elapsed)
	}
}

func TestManagerCloseCancelsAndWaitsForBackgroundTasks(t *testing.T) {
	manager := New(Options{
		DB:           openPluginManagerTestDB(t),
		ArtifactRoot: t.TempDir(),
		Adapter:      &fakeAdapter{},
	})
	operations := manager.operations.ForPlugin("task-close", "artifact", Manifest{})
	started := make(chan struct{})
	finished := make(chan struct{})
	if err := operations.RegisterBackgroundTask(api.BackgroundTask{
		ID:         "shutdown-task",
		RunOnStart: true,
		Run: func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			close(finished)
			return ctx.Err()
		},
	}); err != nil {
		t.Fatalf("RegisterBackgroundTask() error = %v", err)
	}
	manager.operations.StartTasks("task-close")
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background task did not start")
	}

	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("Close() returned before background task finished")
	}
}

type closeWaitGroupPlugin struct {
	api.AbstractPlugin
	stop         chan struct{}
	stopOnce     sync.Once
	destroyCalls atomic.Int32
}

func (p *closeWaitGroupPlugin) Destroy() error {
	p.destroyCalls.Add(1)
	p.stopOnce.Do(func() { close(p.stop) })
	return nil
}

type closeLifecycleRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *closeLifecycleRecorder) add(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *closeLifecycleRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

type closeLifecycleAdapter struct {
	GoPluginAdapter
	recorder *closeLifecycleRecorder
}

func (a *closeLifecycleAdapter) Drain(context.Context, RuntimeInstance) error {
	a.recorder.add("drain")
	return nil
}

func (a *closeLifecycleAdapter) Stop(context.Context, RuntimeInstance) error {
	a.recorder.add("stop")
	return nil
}

type recordingClosePlugin struct {
	api.AbstractPlugin
	recorder *closeLifecycleRecorder
}

func (p *recordingClosePlugin) Destroy() error {
	p.recorder.add("destroy")
	return nil
}

type blockingClosePlugin struct {
	api.AbstractPlugin
	block <-chan struct{}
}

func (p *blockingClosePlugin) Destroy() error {
	<-p.block
	return nil
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
