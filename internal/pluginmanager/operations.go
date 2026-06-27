// internal/pluginmanager/operations.go 实现插件运行时运维能力，包括事件、指标、文件/数据存储、外部客户端、任务和诊断。

package pluginmanager

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/tursom/mc-gateway/plugin/api"
)

const (
	// 简单熔断状态使用字符串保存，便于直接落库和输出到诊断包。
	circuitClosed = "closed"
	circuitOpen   = "open"
)

var (
	// 插件上报的指标名和存储 key 需要收敛到可观测系统容易消费的字符集。
	metricNamePattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.:-]{0,127}$`)
	storeKeyPattern   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:/-]{0,255}$`)
)

type traceContextKey struct{}

type traceContext struct {
	PluginID     string
	TraceID      string
	ConnectionID string
	HandlerID    string
}

type Operations struct {
	repo Repository
	root string

	// plugins 保存每个插件的运行时运维上下文，按 manifest 重新配置但保留统计摘要。
	mu      sync.RWMutex
	plugins map[string]*PluginOperations

	// eventQueue 负责把插件事件异步落库，避免连接热路径被 SQLite 写入阻塞。
	eventQueue chan queuedEvent
	queued     atomic.Uint64
	dropped    atomic.Uint64
	deadLetter atomic.Uint64

	subscriberQueue      chan queuedEvent
	subscriberQueued     atomic.Uint64
	subscriberDropped    atomic.Uint64
	subscriberDeadLetter atomic.Uint64

	// subscribers 是当前启用插件注册的事件订阅者快照，由 Manager 发布。
	subscriberMu sync.RWMutex
	subscribers  []*subscriberHandler
}

type queuedEvent struct {
	pluginID     string
	name         string
	fields       map[string]string
	dropped      bool
	reason       string
	traceID      string
	connectionID string
}

type PluginOperations struct {
	parent     *Operations
	pluginID   string
	artifactID string
	manifest   Manifest

	// 下列 schema 来自 manifest，用于在插件运行时限制事件、指标、任务和外部依赖。
	mu              sync.Mutex
	eventSchemas    map[string]map[string]bool
	metricSchemas   map[string]MetricSpec
	taskSpecs       map[string]TaskSpec
	externalSpecs   map[string]ExternalSpec
	dataQuota       int64
	dataSpec        DataStoreSpec
	fileQuota       int64
	fileSpecs       map[string]FileStoreSpec
	eventValues     map[string]map[string]map[string]struct{}
	eventSummaries  map[string]*EventSummary
	metricSummaries map[string]*CustomMetricSummary
	externals       map[string]*externalRuntime
	tasks           map[string]*taskRuntime
}

type externalRuntime struct {
	spec ExternalSpec

	// 外部依赖统计用于诊断包和熔断策略，全部用原子值减少请求路径锁竞争。
	requests            atomic.Uint64
	errors              atomic.Uint64
	inflight            atomic.Int64
	durationCount       atomic.Uint64
	durationSumMS       atomic.Uint64
	consecutiveFailures atomic.Uint64

	mu           sync.Mutex
	circuitUntil time.Time
	recentError  string
	lastStatus   string
	lastSeenAt   int64
}

type taskRuntime struct {
	pluginID     string
	spec         TaskSpec
	task         api.BackgroundTask
	confirmToken string

	// 每个后台任务独立持有调度状态和 cancel 函数，插件停用时可逐个停止。
	mu                  sync.Mutex
	cancel              context.CancelFunc
	running             bool
	schedulerOn         bool
	lastRunAt           int64
	nextRunAt           int64
	lastDurationMS      int64
	lastError           string
	skipped             uint64
	consecutiveFailures uint64
}

// NewOperations 创建插件运维协调器，并启动事件落库和订阅投递两个后台消费者。
func NewOperations(repo Repository, root string) *Operations {
	ops := &Operations{
		repo:            repo,
		root:            root,
		plugins:         make(map[string]*PluginOperations),
		eventQueue:      make(chan queuedEvent, DefaultEventQueueLimit),
		subscriberQueue: make(chan queuedEvent, DefaultEventQueueLimit),
	}
	go ops.consumeEvents()
	go ops.consumeSubscriberEvents()
	return ops
}

// SetSubscribers 用新的订阅者快照替换旧快照。Manager 在插件启停后调用它，
// 事件消费者只读取快照副本，不直接依赖 Manager 锁。
func (o *Operations) SetSubscribers(subscribers []*subscriberHandler) {
	o.subscriberMu.Lock()
	defer o.subscriberMu.Unlock()
	o.subscribers = append([]*subscriberHandler(nil), subscribers...)
}

func (o *Operations) SubscriberDeadLetters() uint64 {
	return o.subscriberDeadLetter.Load()
}

func (o *Operations) DropSubscriberDeadLetters() uint64 {
	return o.subscriberDeadLetter.Swap(0)
}

func (o *Operations) ForPlugin(pluginID, artifactID string, manifest Manifest) *PluginOperations {
	o.mu.Lock()
	defer o.mu.Unlock()
	po := o.plugins[pluginID]
	if po == nil {
		// 首次看到插件时创建运维上下文；后续版本切换会复用它的近期统计。
		po = &PluginOperations{
			parent:          o,
			pluginID:        pluginID,
			eventValues:     make(map[string]map[string]map[string]struct{}),
			eventSummaries:  make(map[string]*EventSummary),
			metricSummaries: make(map[string]*CustomMetricSummary),
			externals:       make(map[string]*externalRuntime),
			tasks:           make(map[string]*taskRuntime),
		}
		o.plugins[pluginID] = po
	}
	po.configure(artifactID, manifest)
	return po
}

func (o *Operations) StopPlugin(pluginID string) {
	o.mu.RLock()
	po := o.plugins[pluginID]
	o.mu.RUnlock()
	if po != nil {
		po.stopTasks()
	}
}

func (o *Operations) StartTasks(pluginID string) {
	o.mu.RLock()
	po := o.plugins[pluginID]
	o.mu.RUnlock()
	if po != nil {
		po.StartTasks(pluginID)
	}
}

func (o *Operations) consumeEvents() {
	for event := range o.eventQueue {
		o.queued.Add(1)
		// 落库使用短超时，避免后台消费者在数据库异常时堆积过久。
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := o.repo.SaveEvent(ctx, EventSummary{
			PluginID: event.pluginID,
			Name:     event.name,
			Fields:   event.fields,
		}, event.dropped, event.reason, event.traceID, event.connectionID)
		cancel()
		if err != nil {
			o.deadLetter.Add(1)
		}
	}
}

func (o *Operations) queueEvent(event queuedEvent) {
	select {
	case o.eventQueue <- event:
	default:
		// 队列满时仍写一条 dropped 记录，保留“发生过丢弃”的审计线索。
		o.dropped.Add(1)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = o.repo.SaveEvent(ctx, EventSummary{
			PluginID: event.pluginID,
			Name:     event.name,
			Fields:   event.fields,
		}, true, "event_queue_full", event.traceID, event.connectionID)
		cancel()
	}
	o.queueSubscriberEvent(event)
}

func (o *Operations) queueSubscriberEvent(event queuedEvent) {
	// 已标记 dropped 的事件只进入持久化路径，不再交给订阅者重复处理。
	o.subscriberMu.RLock()
	hasSubscribers := len(o.subscribers) > 0
	o.subscriberMu.RUnlock()
	if !hasSubscribers || event.dropped {
		return
	}
	select {
	case o.subscriberQueue <- event:
		o.subscriberQueued.Add(1)
	default:
		o.subscriberDropped.Add(1)
	}
}

func (o *Operations) consumeSubscriberEvents() {
	for event := range o.subscriberQueue {
		o.subscriberMu.RLock()
		subscribers := append([]*subscriberHandler(nil), o.subscribers...)
		o.subscriberMu.RUnlock()
		for _, subscriber := range subscribers {
			o.deliverSubscriberEvent(subscriber, event)
		}
	}
}

func (o *Operations) deliverSubscriberEvent(subscriber *subscriberHandler, event queuedEvent) {
	req := api.EventDeliveryRequest{
		PluginID:     event.pluginID,
		Name:         event.name,
		Fields:       copyStringMap(event.fields),
		TraceID:      event.traceID,
		ConnectionID: event.connectionID,
		Mode:         subscriber.mode,
	}
	accepted, err := subscriber.accepts(req)
	if err != nil || !accepted {
		return
	}
	maxRetry := subscriber.maxRetry
	if subscriber.mode == api.DeliveryBestEffort {
		maxRetry = 1
	}
	if maxRetry <= 0 {
		maxRetry = DefaultSubscriberMaxRetry
	}
	// best_effort 订阅最多投递一次；at_least_once 按订阅者配置进行有限重试。
	for attempt := 1; attempt <= maxRetry; attempt++ {
		req.Attempt = attempt
		result, err := subscriber.invoke(req)
		if err == nil && (result.OK || !result.Retry) {
			return
		}
		if attempt < maxRetry {
			time.Sleep(DefaultSubscriberRetryDelay)
		}
	}
	o.subscriberDeadLetter.Add(1)
	_ = o.repo.RecordOperation(context.Background(), subscriber.pluginID, subscriber.artifactID, "event_subscriber_delivery", "dead_letter", "system", "event subscriber delivery failed", map[string]any{
		"event_plugin_id": event.pluginID,
		"event_name":      event.name,
		"subscriber_mode": subscriber.mode,
	})
}

// configure 根据 manifest 重新构建插件运维能力边界。统计对象尽量复用，
// 但 schema、配额和外部依赖声明每次都以当前制品为准。
func (po *PluginOperations) configure(artifactID string, manifest Manifest) {
	po.mu.Lock()
	defer po.mu.Unlock()
	po.artifactID = artifactID
	po.manifest = manifest

	po.eventSchemas = make(map[string]map[string]bool)
	for _, spec := range manifest.Events {
		if spec.Name == "" {
			continue
		}
		fields := make(map[string]bool, len(spec.Fields))
		for _, field := range spec.Fields {
			fields[field] = true
		}
		po.eventSchemas[spec.Name] = fields
	}
	po.metricSchemas = make(map[string]MetricSpec)
	for _, spec := range manifest.CustomMetrics {
		if spec.Name != "" {
			po.metricSchemas[spec.Name] = spec
		}
	}
	po.taskSpecs = make(map[string]TaskSpec)
	for _, spec := range manifest.BackgroundTasks {
		if spec.ID != "" {
			po.taskSpecs[spec.ID] = spec
		}
	}
	po.externalSpecs = make(map[string]ExternalSpec)
	for _, spec := range manifest.ExternalDeps {
		if spec.Name != "" {
			po.externalSpecs[spec.Name] = spec
			if po.externals[spec.Name] == nil {
				po.externals[spec.Name] = &externalRuntime{spec: spec}
			} else {
				po.externals[spec.Name].spec = spec
			}
		}
	}
	po.dataQuota = DefaultPluginDataQuota
	po.dataSpec = DataStoreSpec{QuotaBytes: DefaultPluginDataQuota}
	if len(manifest.DataStores) > 0 {
		po.dataSpec = manifest.DataStores[0]
		if po.dataSpec.QuotaBytes > 0 {
			po.dataQuota = po.dataSpec.QuotaBytes
		}
	}
	po.fileQuota = DefaultPluginFileQuota
	po.fileSpecs = make(map[string]FileStoreSpec)
	for _, spec := range manifest.FileStores {
		if spec.Namespace != "" {
			po.fileSpecs[spec.Namespace] = spec
			if spec.QuotaBytes > 0 && spec.QuotaBytes < po.fileQuota {
				po.fileQuota = spec.QuotaBytes
			}
		}
	}
	if len(po.fileSpecs) == 0 {
		// 未声明文件存储时提供默认命名空间，方便简单插件直接使用常见分类。
		for _, namespace := range []string{"data", "cache", "tmp", "log", "diagnostic"} {
			po.fileSpecs[namespace] = FileStoreSpec{Namespace: namespace, QuotaBytes: DefaultPluginFileQuota}
		}
	}
}

// EmitEvent 校验并记录插件事件。即使字段非法，也会排队一条 dropped 事件，
// 便于诊断插件为什么没有产出预期事件。
func (po *PluginOperations) EmitEvent(ctx context.Context, name string, fields map[string]string) error {
	trace := traceFromContext(ctx)
	clean, dropReason, err := po.validateEvent(name, fields)
	if err != nil {
		po.parent.queueEvent(queuedEvent{
			pluginID: po.pluginID, name: name, fields: clean, dropped: true,
			reason: dropReason, traceID: trace.TraceID, connectionID: trace.ConnectionID,
		})
		return err
	}
	po.mu.Lock()
	summary := po.eventSummaries[name]
	if summary == nil {
		summary = &EventSummary{PluginID: po.pluginID, Name: name}
		po.eventSummaries[name] = summary
	}
	summary.Count++
	summary.Fields = clean
	summary.LastSeenAt = time.Now().Unix()
	po.mu.Unlock()
	po.parent.queueEvent(queuedEvent{
		pluginID: po.pluginID, name: name, fields: clean,
		traceID: trace.TraceID, connectionID: trace.ConnectionID,
	})
	return nil
}

// ObserveMetric 校验并更新插件自定义指标的最近摘要；这里不做时序存储，
// 只维护管理界面需要的当前观测值。
func (po *PluginOperations) ObserveMetric(ctx context.Context, name string, value float64, labels map[string]string) error {
	_ = ctx
	clean, metricType, err := po.validateMetric(name, labels)
	if err != nil {
		return err
	}
	po.mu.Lock()
	defer po.mu.Unlock()
	summary := po.metricSummaries[name]
	if summary == nil {
		summary = &CustomMetricSummary{PluginID: po.pluginID, Name: name, Type: metricType}
		po.metricSummaries[name] = summary
	}
	summary.Count++
	summary.LastValue = value
	summary.Labels = clean
	summary.LastSeenAt = time.Now().Unix()
	return nil
}

func (po *PluginOperations) Logger() api.Logger {
	return pluginLogger{ops: po}
}

func (po *PluginOperations) DataStore() api.DataStore {
	return pluginDataStore{ops: po}
}

func (po *PluginOperations) FileStore() api.FileStore {
	return pluginFileStore{ops: po}
}

func (po *PluginOperations) ExternalClient(name string) api.ExternalClient {
	po.mu.Lock()
	defer po.mu.Unlock()
	runtime := po.externals[name]
	if runtime == nil {
		spec := po.externalSpecs[name]
		runtime = &externalRuntime{spec: spec}
		po.externals[name] = runtime
	}
	return pluginExternalClient{ops: po, name: name, runtime: runtime}
}

func (po *PluginOperations) RegisterBackgroundTask(task api.BackgroundTask) error {
	if task.ID == "" {
		return errors.New("background task id is required")
	}
	if !metricNamePattern.MatchString(task.ID) {
		return fmt.Errorf("invalid background task id %q", task.ID)
	}
	if task.Run == nil {
		return fmt.Errorf("background task %q run function is required", task.ID)
	}
	po.mu.Lock()
	defer po.mu.Unlock()
	spec := po.taskSpecs[task.ID]
	if len(po.taskSpecs) > 0 && spec.ID == "" {
		// manifest 声明了任务清单时，只允许注册清单中的任务，避免插件运行时
		// 动态创建管理端不可见的后台任务。
		return fmt.Errorf("background task %q is not declared by manifest", task.ID)
	}
	if spec.ID == "" {
		spec = TaskSpec{ID: task.ID, Name: task.Name}
	}
	if task.Name == "" {
		task.Name = spec.Name
	}
	if task.Interval == 0 && spec.Interval != "" {
		task.Interval, _ = time.ParseDuration(spec.Interval)
	}
	if task.Timeout == 0 && spec.Timeout != "" {
		task.Timeout, _ = time.ParseDuration(spec.Timeout)
	}
	if task.Jitter == 0 && spec.Jitter != "" {
		task.Jitter, _ = time.ParseDuration(spec.Jitter)
	}
	if !task.RunOnStart {
		task.RunOnStart = spec.RunOnStart
	}
	if !task.Manual {
		task.Manual = spec.Manual || spec.Mode == "manual"
	}
	if task.Timeout <= 0 {
		task.Timeout = DefaultHandlerTimeout
	}
	rt := po.tasks[task.ID]
	if rt == nil {
		// confirmToken 用于高风险手动任务的二次确认，避免误点直接执行。
		rt = &taskRuntime{
			pluginID:     po.pluginID,
			spec:         spec,
			confirmToken: randomToken(),
		}
		po.tasks[task.ID] = rt
	}
	rt.task = task
	rt.spec = spec
	return nil
}

// StartTasks 启动当前插件注册的后台任务调度器。只在插件 ID 匹配时执行，
// 防止调用方传错 ID 时启动其他插件的任务。
func (po *PluginOperations) StartTasks(pluginID string) {
	if pluginID != po.pluginID {
		return
	}
	po.mu.Lock()
	tasks := make([]*taskRuntime, 0, len(po.tasks))
	for _, task := range po.tasks {
		tasks = append(tasks, task)
	}
	po.mu.Unlock()
	for _, task := range tasks {
		po.startTask(task)
	}
}

// stopTasks 停止所有后台任务调度器。已经在执行的任务通过 cancel 感知停用。
func (po *PluginOperations) stopTasks() {
	po.mu.Lock()
	tasks := make([]*taskRuntime, 0, len(po.tasks))
	for _, task := range po.tasks {
		tasks = append(tasks, task)
	}
	po.mu.Unlock()
	for _, task := range tasks {
		task.mu.Lock()
		if task.cancel != nil {
			task.cancel()
			task.cancel = nil
		}
		task.schedulerOn = false
		task.nextRunAt = 0
		task.mu.Unlock()
	}
}

func (po *PluginOperations) startTask(task *taskRuntime) {
	task.mu.Lock()
	if task.schedulerOn {
		task.mu.Unlock()
		return
	}
	task.schedulerOn = true
	interval := task.task.Interval
	runOnStart := task.task.RunOnStart
	task.mu.Unlock()

	if runOnStart {
		go po.runTask(task)
	}
	if interval <= 0 || task.task.Manual {
		return
	}
	go func() {
		for {
			// 抖动值按任务 ID 确定，避免多个网关实例同一时间集中触发相同任务。
			delay := interval + deterministicJitter(task.task.Jitter, task.task.ID)
			task.mu.Lock()
			if !task.schedulerOn {
				task.mu.Unlock()
				return
			}
			task.nextRunAt = time.Now().Add(delay).Unix()
			task.mu.Unlock()
			timer := time.NewTimer(delay)
			<-timer.C
			task.mu.Lock()
			on := task.schedulerOn
			task.mu.Unlock()
			if !on {
				return
			}
			po.runTask(task)
		}
	}()
}

func (po *PluginOperations) runTask(task *taskRuntime) {
	task.mu.Lock()
	if task.running {
		// 同一任务不并发执行；调度周期追上时只记录跳过次数。
		task.skipped++
		task.mu.Unlock()
		return
	}
	task.running = true
	timeout := task.task.Timeout
	if timeout <= 0 {
		timeout = DefaultHandlerTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	task.cancel = cancel
	task.mu.Unlock()

	start := time.Now()
	err := task.task.Run(ctx)
	cancel()

	task.mu.Lock()
	task.running = false
	task.cancel = nil
	task.lastRunAt = start.Unix()
	task.lastDurationMS = time.Since(start).Milliseconds()
	if err != nil {
		task.consecutiveFailures++
		task.lastError = redactSensitive(err.Error())
	} else {
		task.consecutiveFailures = 0
		task.lastError = ""
	}
	task.mu.Unlock()
}

// TriggerTask 手动触发后台任务。confirmToken 来自任务摘要，调用方必须显式回传，
// 用来降低误触发有副作用任务的风险。
func (po *PluginOperations) TriggerTask(taskID, confirmToken string) (BackgroundTaskSummary, error) {
	po.mu.Lock()
	task := po.tasks[taskID]
	po.mu.Unlock()
	if task == nil {
		return BackgroundTaskSummary{}, fmt.Errorf("background task %q not found", taskID)
	}
	task.mu.Lock()
	expected := task.confirmToken
	task.mu.Unlock()
	if expected == "" || confirmToken != expected {
		return BackgroundTaskSummary{}, errors.New("confirm_token is required")
	}
	go po.runTask(task)
	return task.summary(), nil
}

// Snapshot 汇总插件运行态观测信息。内存中的近期摘要和数据库中的历史摘要会合并，
// 形成管理端和诊断包都能使用的一份视图。
func (po *PluginOperations) Snapshot(ctx context.Context, pluginID string, handlers []DispatchHandlerSummary, builds []BuildRecord, gc []GCCandidate) OperationsSnapshot {
	_ = ctx
	po.mu.Lock()
	events := make([]EventSummary, 0, len(po.eventSummaries))
	for _, event := range po.eventSummaries {
		events = append(events, *event)
	}
	metrics := make([]CustomMetricSummary, 0, len(po.metricSummaries))
	for _, metric := range po.metricSummaries {
		metrics = append(metrics, *metric)
	}
	externals := make([]ExternalDependencySummary, 0, len(po.externals))
	for name, ext := range po.externals {
		externals = append(externals, ext.summary(po.pluginID, name))
	}
	tasks := make([]BackgroundTaskSummary, 0, len(po.tasks))
	for _, task := range po.tasks {
		tasks = append(tasks, task.summary())
	}
	po.mu.Unlock()

	recentEvents, _ := po.parent.repo.RecentEvents(ctx, pluginID, DefaultEventRecentLimit)
	if len(recentEvents) > 0 {
		// 数据库中的事件能覆盖进程重启前的近期历史，内存摘要则提供当前进程最新值。
		events = mergeEventSummaries(events, recentEvents)
	}
	logs, _ := po.parent.repo.RecentLogs(ctx, pluginID, DefaultLogRecentLimit)
	traces, _ := po.parent.repo.RecentTraces(ctx, pluginID, 200)
	data, _ := po.parent.repo.ListPluginData(ctx, pluginID)
	files, _ := po.parent.repo.ListPluginFiles(ctx, pluginID)
	diagnostics, _ := po.parent.repo.ListDiagnostics(ctx, pluginID, 20)

	buildMetrics := make([]BuildMetricSummary, 0, len(builds))
	for _, build := range builds {
		buildMetrics = append(buildMetrics, BuildMetricSummary{
			PluginID:      build.PluginID,
			BuildID:       build.ID,
			Status:        build.Status,
			DurationMS:    build.DurationMS,
			Failed:        build.Status == BuildStatusFailed,
			ErrorRedacted: redactSensitive(build.Error),
			CreatedAt:     build.CreatedAt,
		})
	}
	sort.Slice(events, func(i, j int) bool { return events[i].LastSeenAt > events[j].LastSeenAt })
	sort.Slice(metrics, func(i, j int) bool { return metrics[i].LastSeenAt > metrics[j].LastSeenAt })
	sort.Slice(externals, func(i, j int) bool { return externals[i].Name < externals[j].Name })
	return OperationsSnapshot{
		PluginID:             pluginID,
		UpdatedAt:            time.Now().Unix(),
		Handlers:             handlers,
		Builds:               buildMetrics,
		Events:               events,
		CustomMetrics:        metrics,
		Logs:                 logs,
		Traces:               traces,
		BackgroundTasks:      tasks,
		PluginData:           data,
		PluginFiles:          files,
		ExternalDependencies: externals,
		GC:                   gc,
		EventQueue: EventQueueSummary{
			Limit:                 DefaultEventQueueLimit,
			Queued:                len(po.parent.eventQueue),
			Dropped:               po.parent.dropped.Load(),
			DeadLetters:           po.parent.deadLetter.Load(),
			SubscriberQueued:      po.parent.subscriberQueued.Load(),
			SubscriberDropped:     po.parent.subscriberDropped.Load(),
			SubscriberDeadLetters: po.parent.subscriberDeadLetter.Load(),
		},
		Diagnostics: diagnostics,
	}
}

func (o *Operations) Snapshot(ctx context.Context, pluginID string, handlers []DispatchHandlerSummary, builds []BuildRecord, gc []GCCandidate) OperationsSnapshot {
	o.mu.RLock()
	po := o.plugins[pluginID]
	o.mu.RUnlock()
	if po == nil {
		po = &PluginOperations{parent: o, pluginID: pluginID}
	}
	return po.Snapshot(ctx, pluginID, handlers, builds, gc)
}

func (o *Operations) TriggerTask(pluginID, taskID, confirmToken string) (BackgroundTaskSummary, error) {
	o.mu.RLock()
	po := o.plugins[pluginID]
	o.mu.RUnlock()
	if po == nil {
		return BackgroundTaskSummary{}, ErrPluginNotFound
	}
	return po.TriggerTask(taskID, confirmToken)
}

// DiagnosticPackage 生成可下载的插件诊断包。输出前会统一脱敏，避免把密钥、
// token 或完整协议载荷写入可共享文件。
func (o *Operations) DiagnosticPackage(ctx context.Context, plugin PluginRecord, manifest Manifest, handlers []DispatchHandlerSummary, builds []BuildRecord, gc []GCCandidate) ([]byte, DiagnosticPackageSummary, error) {
	snapshot := o.Snapshot(ctx, plugin.ID, handlers, builds, gc)
	operations, _ := o.repo.ListOperations(ctx, plugin.ID, 50)
	body := map[string]any{
		"created_at":        time.Now().Unix(),
		"plugin":            plugin,
		"manifest":          manifest,
		"operations":        snapshot,
		"recent_operations": operations,
		"redaction_policy": []string{
			"secret", "token", "password", "session response", "full packet payload",
		},
	}
	data, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return nil, DiagnosticPackageSummary{}, err
	}
	data = []byte(redactSensitive(string(data)))
	dir := filepath.Join(o.runtimeRoot(), "diagnostics", plugin.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, DiagnosticPackageSummary{}, err
	}
	file := filepath.Join(dir, fmt.Sprintf("%d.json", time.Now().UnixNano()))
	if err := os.WriteFile(file, data, 0600); err != nil {
		return nil, DiagnosticPackageSummary{}, err
	}
	summary, err := o.repo.SaveDiagnostic(ctx, plugin.ID, file, int64(len(data)), []string{"plugin", "manifest", "operations", "recent_operations"})
	if err != nil {
		return nil, DiagnosticPackageSummary{}, err
	}
	return data, summary, nil
}

func (o *Operations) runtimeRoot() string {
	if o.root == "" {
		return os.TempDir()
	}
	return filepath.Join(o.root, "_runtime")
}

func (o *Operations) GCCandidates(ctx context.Context, pluginID string) ([]GCCandidate, error) {
	now := time.Now().Unix()
	var candidates []GCCandidate
	// 插件数据和文件只有在过期后才允许删除；未过期记录作为受保护候选项返回，
	// 方便 dry-run 解释为什么没有删除它们。
	data, err := o.repo.ListPluginData(ctx, pluginID)
	if err != nil {
		return nil, err
	}
	for _, record := range data {
		expired := record.ExpiresAt > 0 && record.ExpiresAt <= now
		reason := "plugin_data retained"
		if expired {
			reason = "plugin_data retention expired"
		}
		candidates = append(candidates, GCCandidate{
			Kind:       "plugin_data",
			ID:         record.Key,
			PluginID:   record.PluginID,
			Protected:  !expired,
			Reason:     reason,
			SizeBytes:  record.SizeBytes,
			CreatedAt:  record.UpdatedAt,
			Referenced: !expired,
		})
	}
	files, err := o.repo.ListPluginFiles(ctx, pluginID)
	if err != nil {
		return nil, err
	}
	seenFiles := make(map[string]bool)
	for _, record := range files {
		expired := record.ExpiresAt > 0 && record.ExpiresAt <= now
		filePath := filepath.Join(o.runtimeRoot(), record.PluginID, record.Namespace, filepath.FromSlash(record.Path))
		seenFiles[filePath] = true
		reason := "plugin file retained"
		if expired {
			reason = "plugin file retention expired"
		}
		candidates = append(candidates, GCCandidate{
			Kind:       "plugin_file",
			ID:         record.Namespace + "/" + record.Path,
			PluginID:   record.PluginID,
			Path:       filePath,
			Protected:  !expired,
			Reason:     reason,
			SizeBytes:  record.SizeBytes,
			CreatedAt:  record.UpdatedAt,
			Referenced: !expired,
		})
	}
	runtimeRoot := o.runtimeRoot()
	_ = filepath.WalkDir(runtimeRoot, func(filePath string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.Contains(filePath, string(filepath.Separator)+"_runtime"+string(filepath.Separator)) {
			return nil
		}
		if seenFiles[filePath] {
			return nil
		}
		// 文件系统里存在但仓库没有记录的文件视为孤儿文件，可以由 GC 清理。
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(runtimeRoot, filePath)
		parts := strings.Split(rel, string(filepath.Separator))
		id := rel
		pid := ""
		if len(parts) > 0 {
			pid = parts[0]
		}
		if pluginID != "" && pid != pluginID {
			return nil
		}
		candidates = append(candidates, GCCandidate{
			Kind:      "plugin_file_orphan",
			ID:        id,
			PluginID:  pid,
			Path:      filePath,
			Protected: false,
			Reason:    "orphaned runtime file",
			SizeBytes: info.Size(),
			CreatedAt: info.ModTime().Unix(),
		})
		return nil
	})
	logs, err := o.repo.RecentLogs(ctx, pluginID, DefaultLogRecentLimit+1)
	if err == nil && len(logs) > DefaultLogRecentLimit {
		for _, item := range logs[DefaultLogRecentLimit:] {
			candidates = append(candidates, GCCandidate{
				Kind:      "plugin_log",
				ID:        fmt.Sprintf("%s/%d", item.PluginID, item.CreatedAt),
				PluginID:  item.PluginID,
				Protected: false,
				Reason:    "log summary exceeds recent retention",
				SizeBytes: int64(len(item.Message)),
				CreatedAt: item.CreatedAt,
			})
		}
	}
	return candidates, nil
}

// RunGC 执行插件运维数据清理。dryRun 只返回候选项并写操作日志，
// 真正删除时会跳过受保护项。
func (o *Operations) RunGC(ctx context.Context, actor, pluginID string, dryRun bool) ([]GCCandidate, error) {
	candidates, err := o.GCCandidates(ctx, pluginID)
	if err != nil {
		return nil, err
	}
	if dryRun {
		_ = o.repo.RecordOperation(ctx, pluginID, "", "plugin_operations_gc", "dry_run", actor, "plugin operations gc dry-run completed", map[string]any{"candidates": len(candidates)})
		return candidates, nil
	}
	var removed []GCCandidate
	for _, candidate := range candidates {
		if candidate.Protected {
			continue
		}
		switch candidate.Kind {
		case "plugin_data":
			_ = o.repo.DeletePluginData(ctx, candidate.PluginID, candidate.ID)
			removed = append(removed, candidate)
		case "plugin_file":
			namespace, name, _ := strings.Cut(candidate.ID, "/")
			_ = os.Remove(candidate.Path)
			_ = o.repo.DeletePluginFile(ctx, candidate.PluginID, namespace, name)
			removed = append(removed, candidate)
		case "plugin_file_orphan":
			_ = os.Remove(candidate.Path)
			removed = append(removed, candidate)
		}
	}
	_ = o.repo.RecordOperation(ctx, pluginID, "", "plugin_operations_gc", "succeeded", actor, "plugin operations gc completed", map[string]any{"removed": len(removed)})
	return removed, nil
}

// validateEvent 校验事件声明和字段集合，并限制字段值基数，避免插件事件把
// 管理端和后续指标系统拖入高基数数据。
func (po *PluginOperations) validateEvent(name string, fields map[string]string) (map[string]string, string, error) {
	if !metricNamePattern.MatchString(name) {
		return nil, "invalid_event_name", fmt.Errorf("invalid event name %q", name)
	}
	po.mu.Lock()
	defer po.mu.Unlock()
	allowed, declared := po.eventSchemas[name]
	if !declared {
		return nil, "undeclared_event", fmt.Errorf("event %q is not declared by manifest", name)
	}
	clean, err := sanitizeLabels(fields, allowed)
	if err != nil {
		return clean, "invalid_fields", err
	}
	values := po.eventValues[name]
	if values == nil {
		values = make(map[string]map[string]struct{})
		po.eventValues[name] = values
	}
	for field, value := range clean {
		set := values[field]
		if set == nil {
			set = make(map[string]struct{})
			values[field] = set
		}
		set[value] = struct{}{}
		if len(set) > 64 {
			return clean, "high_cardinality_field", fmt.Errorf("event %q field %q exceeded low-cardinality limit", name, field)
		}
	}
	return clean, "", nil
}

// validateMetric 校验自定义指标名和标签，确保插件只能上报 manifest 声明过的指标。
func (po *PluginOperations) validateMetric(name string, labels map[string]string) (map[string]string, string, error) {
	if !metricNamePattern.MatchString(name) {
		return nil, "", fmt.Errorf("invalid metric name %q", name)
	}
	po.mu.Lock()
	defer po.mu.Unlock()
	spec, declared := po.metricSchemas[name]
	if !declared {
		return nil, "", fmt.Errorf("custom metric %q is not declared by manifest", name)
	}
	allowed := make(map[string]bool, len(spec.Labels))
	for _, label := range spec.Labels {
		allowed[label] = true
	}
	clean, err := sanitizeLabels(labels, allowed)
	if err != nil {
		return clean, spec.Type, err
	}
	return clean, spec.Type, nil
}

type pluginLogger struct {
	ops *PluginOperations
}

func (l pluginLogger) Debug(ctx context.Context, message string, fields map[string]string) {
	l.write(ctx, "debug", message, fields)
}

func (l pluginLogger) Info(ctx context.Context, message string, fields map[string]string) {
	l.write(ctx, "info", message, fields)
}

func (l pluginLogger) Warn(ctx context.Context, message string, fields map[string]string) {
	l.write(ctx, "warn", message, fields)
}

func (l pluginLogger) Error(ctx context.Context, message string, fields map[string]string) {
	l.write(ctx, "error", message, fields)
}

func (l pluginLogger) write(ctx context.Context, level, message string, fields map[string]string) {
	if l.ops == nil {
		return
	}
	trace := traceFromContext(ctx)
	clean, _ := sanitizeLabels(fields, nil)
	// 插件日志只保存摘要并脱敏，避免把完整请求、密钥或 token 写入运行态数据库。
	item := LogSummary{
		PluginID:     l.ops.pluginID,
		Level:        level,
		Message:      redactSensitive(limitString(message, 512)),
		Fields:       clean,
		TraceID:      trace.TraceID,
		ConnectionID: trace.ConnectionID,
		CreatedAt:    time.Now().Unix(),
	}
	_ = l.ops.parent.repo.SaveLog(context.Background(), item)
	log.Info().
		Str("plugin", l.ops.pluginID).
		Str("level", level).
		Str("trace_id", trace.TraceID).
		Str("connection_id", trace.ConnectionID).
		Msg(item.Message)
}

type pluginDataStore struct {
	ops *PluginOperations
}

func (s pluginDataStore) Put(ctx context.Context, record api.DataRecord) error {
	if s.ops == nil {
		return errors.New("plugin data store is unavailable")
	}
	key, err := cleanStoreKey(record.Key)
	if err != nil {
		return err
	}
	if len(record.Value) > DefaultPluginDataKeyLimit {
		return fmt.Errorf("plugin_data key %q size %d exceeds limit %d", key, len(record.Value), DefaultPluginDataKeyLimit)
	}
	current, _ := s.ops.parent.repo.PluginDataUsage(ctx, s.ops.pluginID)
	old, _, _ := s.ops.parent.repo.GetPluginData(ctx, s.ops.pluginID, key)
	// 更新已有 key 时只计算净增长，避免重复写同一 key 被误判为超配额。
	nextUsage := current - old.SizeBytes + int64(len(record.Value))
	if nextUsage > s.ops.dataQuota {
		return fmt.Errorf("plugin_data quota exceeded: %d > %d", nextUsage, s.ops.dataQuota)
	}
	expiresAt := retentionDeadline(record.Retention)
	if record.DataClass == "" {
		record.DataClass = s.ops.dataSpec.DataClass
	}
	if record.SchemaVersion == 0 {
		record.SchemaVersion = s.ops.dataSpec.SchemaVersion
	}
	return s.ops.parent.repo.PutPluginData(ctx, PluginDataSummary{
		PluginID:      s.ops.pluginID,
		Key:           key,
		SchemaVersion: record.SchemaVersion,
		DataClass:     redactSensitive(record.DataClass),
		Exportable:    record.Exportable || s.ops.dataSpec.Exportable,
		SizeBytes:     int64(len(record.Value)),
		ExpiresAt:     expiresAt,
	}, append([]byte(nil), record.Value...))
}

func (s pluginDataStore) Get(ctx context.Context, key string) (api.DataRecord, error) {
	if s.ops == nil {
		return api.DataRecord{}, errors.New("plugin data store is unavailable")
	}
	clean, err := cleanStoreKey(key)
	if err != nil {
		return api.DataRecord{}, err
	}
	record, value, err := s.ops.parent.repo.GetPluginData(ctx, s.ops.pluginID, clean)
	if err != nil {
		return api.DataRecord{}, err
	}
	return api.DataRecord{
		Key: record.Key,
		// 返回副本，避免调用方修改仓库层读取出来的缓冲区。
		Value:         append([]byte(nil), value...),
		SchemaVersion: record.SchemaVersion,
		DataClass:     record.DataClass,
		Exportable:    record.Exportable,
	}, nil
}

func (s pluginDataStore) Delete(ctx context.Context, key string) error {
	if s.ops == nil {
		return errors.New("plugin data store is unavailable")
	}
	clean, err := cleanStoreKey(key)
	if err != nil {
		return err
	}
	return s.ops.parent.repo.DeletePluginData(ctx, s.ops.pluginID, clean)
}

type pluginFileStore struct {
	ops *PluginOperations
}

func (s pluginFileStore) ResourcePath(name string) (string, error) {
	if s.ops == nil {
		return "", errors.New("plugin file store is unavailable")
	}
	clean, err := cleanStorePath(name)
	if err != nil {
		return "", err
	}
	artifactDir := filepath.Join(s.ops.parent.root, s.ops.pluginID, s.ops.artifactID)
	resource := filepath.Join(artifactDir, "resources", filepath.FromSlash(clean))
	if !isSubpath(filepath.Join(artifactDir, "resources"), resource) {
		return "", errors.New("unsafe resource path")
	}
	// ResourcePath 只返回随制品发布的只读资源路径，不写运行态文件记录。
	return resource, nil
}

func (s pluginFileStore) Write(ctx context.Context, namespace, name string, data []byte, dataClass string, retention time.Duration) error {
	if s.ops == nil {
		return errors.New("plugin file store is unavailable")
	}
	namespace, spec, err := s.namespaceSpec(namespace)
	if err != nil {
		return err
	}
	if spec.Readonly {
		return fmt.Errorf("file namespace %q is readonly", namespace)
	}
	clean, err := cleanStorePath(name)
	if err != nil {
		return err
	}
	quota := spec.QuotaBytes
	if quota <= 0 {
		quota = s.ops.fileQuota
	}
	usage, _ := s.ops.parent.repo.PluginFileUsage(ctx, s.ops.pluginID)
	if usage+int64(len(data)) > quota {
		return fmt.Errorf("plugin file quota exceeded: %d > %d", usage+int64(len(data)), quota)
	}
	root := s.runtimeRoot(namespace)
	target := filepath.Join(root, filepath.FromSlash(clean))
	if !isSubpath(root, target) {
		return errors.New("unsafe file path")
	}
	// 路径校验后再创建目录，防止插件通过 ../ 写出自己的命名空间。
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(target, data, 0644); err != nil {
		return err
	}
	if dataClass == "" {
		dataClass = spec.DataClass
	}
	return s.ops.parent.repo.UpsertPluginFile(ctx, PluginFileSummary{
		PluginID:  s.ops.pluginID,
		Namespace: namespace,
		Path:      clean,
		DataClass: redactSensitive(dataClass),
		SizeBytes: int64(len(data)),
		ExpiresAt: retentionDeadline(retention),
		Readonly:  spec.Readonly,
	}, target)
}

func (s pluginFileStore) Read(ctx context.Context, namespace, name string, maxBytes int64) ([]byte, error) {
	_ = ctx
	if s.ops == nil {
		return nil, errors.New("plugin file store is unavailable")
	}
	namespace, _, err := s.namespaceSpec(namespace)
	if err != nil {
		return nil, err
	}
	clean, err := cleanStorePath(name)
	if err != nil {
		return nil, err
	}
	root := s.runtimeRoot(namespace)
	target := filepath.Join(root, filepath.FromSlash(clean))
	if !isSubpath(root, target) {
		return nil, errors.New("unsafe file path")
	}
	if maxBytes <= 0 {
		maxBytes = DefaultPluginDataKeyLimit
	}
	file, err := os.Open(target)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var buf bytes.Buffer
	if _, err := io.CopyN(&buf, file, maxBytes+1); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if int64(buf.Len()) > maxBytes {
		return nil, fmt.Errorf("file %q exceeds max read size %d", clean, maxBytes)
	}
	return buf.Bytes(), nil
}

func (s pluginFileStore) Delete(ctx context.Context, namespace, name string) error {
	if s.ops == nil {
		return errors.New("plugin file store is unavailable")
	}
	namespace, spec, err := s.namespaceSpec(namespace)
	if err != nil {
		return err
	}
	if spec.Readonly {
		return fmt.Errorf("file namespace %q is readonly", namespace)
	}
	clean, err := cleanStorePath(name)
	if err != nil {
		return err
	}
	root := s.runtimeRoot(namespace)
	target := filepath.Join(root, filepath.FromSlash(clean))
	if !isSubpath(root, target) {
		return errors.New("unsafe file path")
	}
	_ = os.Remove(target)
	return s.ops.parent.repo.DeletePluginFile(ctx, s.ops.pluginID, namespace, clean)
}

func (s pluginFileStore) namespaceSpec(namespace string) (string, FileStoreSpec, error) {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		// 默认命名空间让简单插件不必显式声明每次读写的分类。
		namespace = "data"
	}
	if !metricNamePattern.MatchString(namespace) {
		return "", FileStoreSpec{}, fmt.Errorf("invalid file namespace %q", namespace)
	}
	spec := s.ops.fileSpecs[namespace]
	if spec.Namespace == "" {
		return "", FileStoreSpec{}, fmt.Errorf("file namespace %q is not declared", namespace)
	}
	return namespace, spec, nil
}

func (s pluginFileStore) runtimeRoot(namespace string) string {
	root := s.ops.parent.root
	if root == "" {
		root = os.TempDir()
	}
	return filepath.Join(root, "_runtime", s.ops.pluginID, namespace)
}

type pluginExternalClient struct {
	ops     *PluginOperations
	name    string
	runtime *externalRuntime
}

func (c pluginExternalClient) DoHTTP(ctx context.Context, req api.ExternalRequest) (api.ExternalResponse, error) {
	if c.ops == nil || c.runtime == nil {
		return api.ExternalResponse{}, errors.New("external client is unavailable")
	}
	spec, err := c.declaredSpec()
	if err != nil {
		return api.ExternalResponse{}, err
	}
	if req.URL == "" {
		req.URL = spec.Endpoint
	}
	if req.Method == "" {
		req.Method = http.MethodGet
	}
	if err := c.beforeRequest(); err != nil {
		return api.ExternalResponse{}, err
	}
	// beforeRequest 会增加 inflight，后续必须在 defer 中成对减少。
	start := time.Now()
	defer c.runtime.inflight.Add(-1)

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = parseDurationDefault(spec.Timeout, DefaultHandlerTimeout)
	}
	if timeout <= 0 {
		timeout = DefaultHandlerTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, req.Body)
	if err != nil {
		c.finish(start, "request_error", err)
		return api.ExternalResponse{}, err
	}
	httpReq.Header = req.Header.Clone()
	if spec.Traceparent {
		trace := traceFromContext(ctx)
		if trace.TraceID != "" {
			// 只透传 trace id，不暴露内部 connection id 或插件处理器 id。
			httpReq.Header.Set("traceparent", "00-"+limitHex(trace.TraceID, 32)+"-0000000000000000-01")
		}
	}

	attempts := spec.Retry + 1
	if attempts <= 0 {
		attempts = 1
	}
	var lastErr error
	var resp *http.Response
	for i := 0; i < attempts; i++ {
		resp, lastErr = http.DefaultClient.Do(httpReq)
		if lastErr == nil && resp.StatusCode < 500 {
			break
		}
		// 需要关闭失败响应体，避免重试时泄漏连接。
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}
	if lastErr != nil {
		c.finish(start, "error", lastErr)
		return api.ExternalResponse{}, lastErr
	}
	defer resp.Body.Close()
	body, err := readLimited(resp.Body, 1024*1024)
	if err != nil {
		c.finish(start, "read_error", err)
		return api.ExternalResponse{}, err
	}
	if resp.StatusCode >= 500 {
		err := fmt.Errorf("external dependency %s returned status %d", c.name, resp.StatusCode)
		c.finish(start, fmt.Sprintf("http_%d", resp.StatusCode), err)
		return api.ExternalResponse{}, err
	}
	c.finish(start, fmt.Sprintf("http_%d", resp.StatusCode), nil)
	c.recordTrace(ctx, start, "http", fmt.Sprintf("%d", resp.StatusCode))
	return api.ExternalResponse{StatusCode: resp.StatusCode, Header: resp.Header.Clone(), Body: body}, nil
}

func (c pluginExternalClient) DialTCP(ctx context.Context, address string, timeout time.Duration) (net.Conn, error) {
	if c.ops == nil || c.runtime == nil {
		return nil, errors.New("external client is unavailable")
	}
	spec, err := c.declaredSpec()
	if err != nil {
		return nil, err
	}
	if address == "" {
		address = strings.TrimPrefix(spec.Endpoint, "tcp://")
	}
	if timeout <= 0 {
		timeout = parseDurationDefault(spec.Timeout, DefaultExternalTimeout)
	}
	if err := c.beforeRequest(); err != nil {
		return nil, err
	}
	start := time.Now()
	defer c.runtime.inflight.Add(-1)
	// 外部 TCP 连接返回给插件后由插件负责关闭；这里仅记录拨号阶段的观测信息。
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		c.finish(start, "dial_error", err)
		return nil, err
	}
	c.finish(start, "ok", nil)
	c.recordTrace(ctx, start, "tcp", "ok")
	return conn, nil
}

// HealthCheck 使用 manifest 声明的 endpoint 做轻量检查。TCP 依赖只建立后关闭，
// HTTP 依赖优先使用 HEAD，避免拉取大响应体。
func (c pluginExternalClient) HealthCheck(ctx context.Context) error {
	spec, err := c.declaredSpec()
	if err != nil {
		return err
	}
	if strings.HasPrefix(spec.Endpoint, "tcp://") {
		conn, err := c.DialTCP(ctx, strings.TrimPrefix(spec.Endpoint, "tcp://"), parseDurationDefault(spec.Timeout, DefaultExternalTimeout))
		if conn != nil {
			_ = conn.Close()
		}
		return err
	}
	resp, err := c.DoHTTP(ctx, api.ExternalRequest{Method: http.MethodHead, URL: spec.Endpoint})
	if err == nil && resp.StatusCode >= 400 {
		err = fmt.Errorf("health check status %d", resp.StatusCode)
	}
	if err != nil {
		c.runtime.mu.Lock()
		c.runtime.lastStatus = "health_failed"
		c.runtime.recentError = redactSensitive(err.Error())
		c.runtime.mu.Unlock()
	}
	return err
}

func (c pluginExternalClient) declaredSpec() (ExternalSpec, error) {
	c.ops.mu.Lock()
	defer c.ops.mu.Unlock()
	spec := c.ops.externalSpecs[c.name]
	if spec.Name == "" {
		return ExternalSpec{}, fmt.Errorf("external dependency %q is not declared by manifest", c.name)
	}
	return spec, nil
}

// beforeRequest 检查熔断窗口并记录并发请求数。成功进入请求路径后，
// 调用方必须在结束时减少 inflight。
func (c pluginExternalClient) beforeRequest() error {
	c.runtime.mu.Lock()
	defer c.runtime.mu.Unlock()
	if time.Now().Before(c.runtime.circuitUntil) {
		return fmt.Errorf("external dependency %s circuit is open", c.name)
	}
	c.runtime.requests.Add(1)
	c.runtime.inflight.Add(1)
	return nil
}

// finish 更新外部依赖统计并维护一个简单熔断器。连续三次失败会短暂打开熔断，
// 防止插件把故障依赖打爆。
func (c pluginExternalClient) finish(start time.Time, status string, err error) {
	duration := time.Since(start)
	c.runtime.durationCount.Add(1)
	c.runtime.durationSumMS.Add(uint64(duration.Milliseconds()))
	c.runtime.mu.Lock()
	defer c.runtime.mu.Unlock()
	c.runtime.lastStatus = status
	c.runtime.lastSeenAt = time.Now().Unix()
	if err != nil {
		c.runtime.errors.Add(1)
		failures := c.runtime.consecutiveFailures.Add(1)
		c.runtime.recentError = redactSensitive(err.Error())
		if failures >= 3 {
			c.runtime.circuitUntil = time.Now().Add(30 * time.Second)
		}
		return
	}
	c.runtime.consecutiveFailures.Store(0)
	c.runtime.recentError = ""
	c.runtime.circuitUntil = time.Time{}
}

// recordTrace 把外部依赖调用写入 trace 摘要，endpoint 和 purpose 会先脱敏。
func (c pluginExternalClient) recordTrace(ctx context.Context, start time.Time, kind, status string) {
	trace := traceFromContext(ctx)
	_ = c.ops.parent.repo.SaveTrace(context.Background(), TraceSummary{
		PluginID:     c.ops.pluginID,
		TraceID:      trace.TraceID,
		ConnectionID: trace.ConnectionID,
		HandlerID:    trace.HandlerID,
		Operation:    "external." + kind + "." + c.name,
		Status:       status,
		DurationMS:   time.Since(start).Milliseconds(),
	}, map[string]string{
		"endpoint": redactEndpoint(c.runtime.spec.Endpoint),
		"purpose":  redactSensitive(c.runtime.spec.Purpose),
	})
}

// summary 返回外部依赖的可展示状态，并隐藏 endpoint 中可能带账号的信息。
func (rt *externalRuntime) summary(pluginID, name string) ExternalDependencySummary {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	state := circuitClosed
	if time.Now().Before(rt.circuitUntil) {
		state = circuitOpen
	}
	return ExternalDependencySummary{
		PluginID:            pluginID,
		Name:                name,
		Endpoint:            redactEndpoint(rt.spec.Endpoint),
		Purpose:             redactSensitive(rt.spec.Purpose),
		Required:            rt.spec.Required,
		FailPolicy:          rt.spec.FailPolicy,
		DataClasses:         append([]string(nil), rt.spec.DataClasses...),
		Requests:            rt.requests.Load(),
		Errors:              rt.errors.Load(),
		Inflight:            rt.inflight.Load(),
		DurationCount:       rt.durationCount.Load(),
		DurationSumMS:       rt.durationSumMS.Load(),
		CircuitState:        state,
		ConsecutiveFailures: rt.consecutiveFailures.Load(),
		RecentError:         rt.recentError,
		LastStatus:          rt.lastStatus,
		LastSeenAt:          rt.lastSeenAt,
	}
}

// summary 返回后台任务的当前调度状态，供运维快照和管理端展示。
func (rt *taskRuntime) summary() BackgroundTaskSummary {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	mode := rt.spec.Mode
	if mode == "" {
		if rt.task.Manual {
			mode = "manual"
		} else {
			mode = "interval"
		}
	}
	return BackgroundTaskSummary{
		PluginID:            rt.pluginID,
		TaskID:              rt.task.ID,
		Name:                rt.task.Name,
		Mode:                mode,
		IntervalMS:          rt.task.Interval.Milliseconds(),
		RunOnStart:          rt.task.RunOnStart,
		TimeoutMS:           rt.task.Timeout.Milliseconds(),
		Manual:              rt.task.Manual,
		Running:             rt.running,
		LastRunAt:           rt.lastRunAt,
		NextRunAt:           rt.nextRunAt,
		LastDurationMS:      rt.lastDurationMS,
		LastError:           rt.lastError,
		Skipped:             rt.skipped,
		ConsecutiveFailures: rt.consecutiveFailures,
	}
}

// WithTraceContext 把插件处理链路信息塞入 context，供日志、事件和外部依赖
// 记录复用同一个 trace/connection 标识。
func WithTraceContext(ctx context.Context, pluginID, traceID, connectionID, handlerID string) context.Context {
	return context.WithValue(ctx, traceContextKey{}, traceContext{
		PluginID:     pluginID,
		TraceID:      traceID,
		ConnectionID: connectionID,
		HandlerID:    handlerID,
	})
}

func traceFromContext(ctx context.Context) traceContext {
	if ctx == nil {
		return traceContext{}
	}
	trace, _ := ctx.Value(traceContextKey{}).(traceContext)
	return trace
}

// sanitizeLabels 过滤插件上报字段：数量、名称、声明范围、敏感字段和单值长度
// 都会被限制，避免低成本插件事件变成高基数或敏感数据出口。
func sanitizeLabels(fields map[string]string, allowed map[string]bool) (map[string]string, error) {
	if len(fields) == 0 {
		return map[string]string{}, nil
	}
	if len(fields) > 12 {
		return nil, errors.New("too many fields")
	}
	clean := make(map[string]string, len(fields))
	for key, value := range fields {
		if !metricNamePattern.MatchString(key) {
			return clean, fmt.Errorf("invalid field %q", key)
		}
		if len(allowed) > 0 && !allowed[key] {
			return clean, fmt.Errorf("field %q is not declared", key)
		}
		if isSensitiveName(key) {
			return clean, fmt.Errorf("field %q is sensitive", key)
		}
		if len(value) > DefaultLabelValueMaxBytes {
			return clean, fmt.Errorf("field %q value exceeds low-cardinality size limit", key)
		}
		value = redactSensitive(value)
		if value == "" {
			continue
		}
		clean[key] = value
	}
	return clean, nil
}

// cleanStoreKey 校验插件数据存储 key，禁止绝对路径、反斜杠和上级目录片段。
func cleanStoreKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" || !storeKeyPattern.MatchString(key) {
		return "", fmt.Errorf("invalid plugin_data key %q", key)
	}
	if strings.Contains(key, "..") || strings.HasPrefix(key, "/") || strings.Contains(key, `\`) {
		return "", fmt.Errorf("unsafe plugin_data key %q", key)
	}
	return key, nil
}

// cleanStorePath 校验插件文件路径，要求传入值已经是规范相对路径。
func cleanStorePath(name string) (string, error) {
	if name == "" || strings.Contains(name, `\`) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("unsafe file path %q", name)
	}
	clean := path.Clean(name)
	if clean == "." || clean != name || strings.HasPrefix(clean, "../") || clean == ".." || path.IsAbs(clean) {
		return "", fmt.Errorf("unsafe file path %q", name)
	}
	return clean, nil
}

// isSubpath 判断 target 是否仍在 root 内，作为最终路径穿越保护。
func isSubpath(root, target string) bool {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// retentionDeadline 将保留时间转换为 Unix 时间戳；0 表示不过期。
func retentionDeadline(retention time.Duration) int64 {
	if retention <= 0 {
		return 0
	}
	return time.Now().Add(retention).Unix()
}

// parseDurationDefault 在 manifest 配置缺失或非法时返回默认时长。
func parseDurationDefault(value string, fallback time.Duration) time.Duration {
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

// deterministicJitter 基于任务 key 生成稳定抖动，避免每次重启后调度时间完全随机。
func deterministicJitter(jitter time.Duration, key string) time.Duration {
	if jitter <= 0 || key == "" {
		return 0
	}
	var sum int64
	for _, ch := range key {
		sum += int64(ch)
	}
	return time.Duration(sum % int64(jitter))
}

func limitString(value string, max int) string {
	if max <= 0 || len(value) <= max {
		return value
	}
	return value[:max]
}

// redactSensitive 对明显敏感的文本做粗粒度脱敏。它不替代结构化密钥管理，
// 只作为日志、诊断和摘要输出前的最后防线。
func redactSensitive(value string) string {
	if value == "" {
		return ""
	}
	lower := strings.ToLower(value)
	for _, marker := range []string{"secret", "token", "password", "session", "credential", "authorization", "packet"} {
		if strings.Contains(lower, marker) {
			return "[REDACTED]"
		}
	}
	return value
}

// redactEndpoint 隐藏包含账号信息的 endpoint，并限制展示长度。
func redactEndpoint(endpoint string) string {
	if endpoint == "" {
		return ""
	}
	if strings.Contains(endpoint, "@") {
		return "[REDACTED]"
	}
	return limitString(endpoint, 256)
}

// randomToken 生成确认令牌；随机源失败时退化为时间戳，保证调用方仍能完成流程。
func randomToken() string {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(data[:])
}

// limitHex 将 trace id 规范为指定长度的十六进制字符串，用于 traceparent 头。
func limitHex(value string, max int) string {
	value = strings.ToLower(value)
	var out strings.Builder
	for _, ch := range value {
		if (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') {
			out.WriteRune(ch)
		}
	}
	for out.Len() < max {
		out.WriteByte('0')
	}
	return out.String()[:max]
}

// readLimited 读取外部响应时设置硬上限，避免插件依赖返回超大 body 占满内存。
func readLimited(reader io.Reader, max int64) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := io.CopyN(&buf, reader, max+1); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if int64(buf.Len()) > max {
		return nil, fmt.Errorf("response exceeds limit %d", max)
	}
	return buf.Bytes(), nil
}

// mergeEventSummaries 合并内存事件摘要和数据库近期事件，按插件和事件名聚合计数。
func mergeEventSummaries(current, recent []EventSummary) []EventSummary {
	byKey := make(map[string]EventSummary)
	for _, event := range current {
		byKey[event.PluginID+"\x00"+event.Name] = event
	}
	for _, event := range recent {
		key := event.PluginID + "\x00" + event.Name
		existing := byKey[key]
		existing.PluginID = event.PluginID
		existing.Name = event.Name
		existing.Count += event.Count
		existing.Dropped += event.Dropped
		if event.LastSeenAt > existing.LastSeenAt {
			existing.LastSeenAt = event.LastSeenAt
			existing.Fields = event.Fields
		}
		byKey[key] = existing
	}
	out := make([]EventSummary, 0, len(byKey))
	for _, event := range byKey {
		out = append(out, event)
	}
	return out
}

// noop* 类型用于在插件未启用完整 Operations 时仍返回满足接口的安全空实现。
type noopOperationsLogger struct{}

func (noopOperationsLogger) Debug(context.Context, string, map[string]string) {}
func (noopOperationsLogger) Info(context.Context, string, map[string]string)  {}
func (noopOperationsLogger) Warn(context.Context, string, map[string]string)  {}
func (noopOperationsLogger) Error(context.Context, string, map[string]string) {}

type noopOperationsDataStore struct{}

func (noopOperationsDataStore) Put(context.Context, api.DataRecord) error { return nil }
func (noopOperationsDataStore) Get(context.Context, string) (api.DataRecord, error) {
	return api.DataRecord{}, errors.New("plugin data store is unavailable")
}
func (noopOperationsDataStore) Delete(context.Context, string) error { return nil }

type noopOperationsFileStore struct{}

func (noopOperationsFileStore) ResourcePath(string) (string, error) {
	return "", errors.New("plugin file store is unavailable")
}
func (noopOperationsFileStore) Write(context.Context, string, string, []byte, string, time.Duration) error {
	return nil
}
func (noopOperationsFileStore) Read(context.Context, string, string, int64) ([]byte, error) {
	return nil, errors.New("plugin file store is unavailable")
}
func (noopOperationsFileStore) Delete(context.Context, string, string) error { return nil }

type noopOperationsExternalClient struct{}

func (noopOperationsExternalClient) DoHTTP(context.Context, api.ExternalRequest) (api.ExternalResponse, error) {
	return api.ExternalResponse{}, errors.New("external client is unavailable")
}
func (noopOperationsExternalClient) DialTCP(context.Context, string, time.Duration) (net.Conn, error) {
	return nil, errors.New("external client is unavailable")
}
func (noopOperationsExternalClient) HealthCheck(context.Context) error { return nil }
