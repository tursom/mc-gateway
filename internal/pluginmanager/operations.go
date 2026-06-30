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
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
	repo   Repository
	root   string
	nodeID string

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
	subscriberDeadMu     sync.Mutex
	subscriberDeadEvents []subscriberDeadLetterEntry

	// subscribers 是当前启用插件注册的事件订阅者快照，由 Manager 发布。
	subscriberMu sync.RWMutex
	subscribers  []*subscriberHandler

	exporterMu sync.RWMutex
	exporters  map[string]*operationsExporterRuntime
}

type operationsExporterRuntime struct {
	cfg  OperationsExporterConfig
	sink OperationsExporterSink

	mu             sync.Mutex
	lastError      string
	failureCount   uint64
	lastFailureAt  int64
	lastSuccessAt  int64
	rollbackCount  uint64
	lastRollbackAt int64
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

type subscriberDeadLetterEntry struct {
	id                   int64
	subscriberPluginID   string
	subscriberArtifactID string
	deliveryMode         string
	attempts             int
	nodeID               string
	event                queuedEvent
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
	nodeID       string
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
	lastAttempts        int
	skipped             uint64
	leaseOwner          string
	leaseExpiresAt      int64
	leaseAcquired       bool
	leaseSkipped        uint64
	consecutiveFailures uint64
}

// NewOperations 创建插件运维协调器，并启动事件落库和订阅投递两个后台消费者。
func NewOperations(repo Repository, root string) *Operations {
	return NewOperationsWithNodeID(repo, root, "")
}

func NewOperationsWithNodeID(repo Repository, root, nodeID string) *Operations {
	if strings.TrimSpace(nodeID) == "" {
		nodeID = defaultOperationsNodeID()
	}
	ops := &Operations{
		repo:            repo,
		root:            root,
		nodeID:          nodeID,
		plugins:         make(map[string]*PluginOperations),
		eventQueue:      make(chan queuedEvent, DefaultEventQueueLimit),
		subscriberQueue: make(chan queuedEvent, DefaultEventQueueLimit),
		exporters:       make(map[string]*operationsExporterRuntime),
	}
	go ops.consumeEvents()
	go ops.consumeSubscriberEvents()
	return ops
}

func defaultOperationsNodeID() string {
	if value := strings.TrimSpace(os.Getenv("MC_GATEWAY_NODE_ID")); value != "" {
		return value
	}
	if host, err := os.Hostname(); err == nil && strings.TrimSpace(host) != "" {
		return fmt.Sprintf("%s-%d", host, os.Getpid())
	}
	return fmt.Sprintf("local-%d", os.Getpid())
}

// SetSubscribers 用新的订阅者快照替换旧快照。Manager 在插件启停后调用它，
// 事件消费者只读取快照副本，不直接依赖 Manager 锁。
func (o *Operations) SetSubscribers(subscribers []*subscriberHandler) {
	o.subscriberMu.Lock()
	defer o.subscriberMu.Unlock()
	o.subscribers = append([]*subscriberHandler(nil), subscribers...)
}

func (o *Operations) SubscriberDeadLetters() uint64 {
	count, err := o.repo.CountPendingSubscriberDeadLetters(context.Background())
	if err == nil {
		return count
	}
	o.subscriberDeadMu.Lock()
	defer o.subscriberDeadMu.Unlock()
	return uint64(len(o.subscriberDeadEvents))
}

func (o *Operations) ReplaySubscriberDeadLetters() uint64 {
	entries := o.pendingSubscriberDeadLetters()
	if len(entries) == 0 {
		return 0
	}

	o.subscriberMu.RLock()
	subscribers := append([]*subscriberHandler(nil), o.subscribers...)
	o.subscriberMu.RUnlock()
	for _, entry := range entries {
		replayed := false
		for _, subscriber := range subscribers {
			if subscriber.pluginID != entry.subscriberPluginID {
				continue
			}
			replayed = true
			o.deliverSubscriberEvent(subscriber, entry.event)
		}
		if !replayed {
			o.restoreSubscriberDeadLetter(entry)
			continue
		}
		if entry.id > 0 {
			_ = o.repo.MarkSubscriberDeadLetter(context.Background(), entry.id, "replayed", "replay requested")
		}
	}
	return uint64(len(entries))
}

func (o *Operations) DropSubscriberDeadLetters() uint64 {
	persisted, err := o.repo.PendingSubscriberDeadLetters(context.Background(), DefaultSubscriberDeadLetterLimit)
	if err == nil && len(persisted) > 0 {
		for _, record := range persisted {
			_ = o.repo.MarkSubscriberDeadLetter(context.Background(), record.ID, "dropped", "drop requested")
		}
		o.subscriberDeadMu.Lock()
		o.subscriberDeadEvents = nil
		o.subscriberDeadMu.Unlock()
		return uint64(len(persisted))
	}
	o.subscriberDeadMu.Lock()
	defer o.subscriberDeadMu.Unlock()
	count := uint64(len(o.subscriberDeadEvents))
	o.subscriberDeadEvents = nil
	return count
}

func (o *Operations) ConfigureExporter(cfg OperationsExporterConfig, sink OperationsExporterSink) OperationsExporterStatus {
	typ := normalizeOperationsExporterType(cfg.Type)
	cfg.Type = typ
	o.exporterMu.Lock()
	runtime := o.exporters[typ]
	if runtime == nil {
		runtime = &operationsExporterRuntime{cfg: cfg}
		o.exporters[typ] = runtime
	}
	runtime.mu.Lock()
	runtime.cfg = cfg
	runtime.sink = sink
	runtime.mu.Unlock()
	o.exporterMu.Unlock()
	return runtime.status()
}

func (o *Operations) RollbackExporter(exporterType string) OperationsExporterStatus {
	typ := normalizeOperationsExporterType(exporterType)
	o.exporterMu.Lock()
	runtime := o.exporters[typ]
	if runtime == nil {
		runtime = &operationsExporterRuntime{cfg: OperationsExporterConfig{Type: typ}}
		o.exporters[typ] = runtime
	}
	runtime.mu.Lock()
	runtime.cfg.Enabled = false
	runtime.cfg.Endpoint = ""
	runtime.sink = nil
	runtime.rollbackCount++
	runtime.lastRollbackAt = time.Now().Unix()
	runtime.mu.Unlock()
	o.exporterMu.Unlock()
	return runtime.status()
}

func (o *Operations) ExporterStatuses() []OperationsExporterStatus {
	o.exporterMu.RLock()
	runtimes := make([]*operationsExporterRuntime, 0, len(o.exporters)+2)
	seen := make(map[string]bool, len(o.exporters)+2)
	for typ, runtime := range o.exporters {
		runtimes = append(runtimes, runtime)
		seen[typ] = true
	}
	for _, typ := range []string{OperationsExporterPrometheus, OperationsExporterOTel} {
		if !seen[typ] {
			runtimes = append(runtimes, &operationsExporterRuntime{cfg: OperationsExporterConfig{Type: typ}})
		}
	}
	o.exporterMu.RUnlock()

	statuses := make([]OperationsExporterStatus, 0, len(runtimes))
	for _, runtime := range runtimes {
		statuses = append(statuses, runtime.status())
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Type < statuses[j].Type })
	return statuses
}

func (o *Operations) ExportSnapshot(ctx context.Context, snapshot OperationsSnapshot) []OperationsExporterStatus {
	o.exporterMu.RLock()
	runtimes := make([]*operationsExporterRuntime, 0, len(o.exporters))
	for _, runtime := range o.exporters {
		runtimes = append(runtimes, runtime)
	}
	o.exporterMu.RUnlock()
	for _, runtime := range runtimes {
		runtime.export(ctx, snapshot)
	}
	return o.ExporterStatuses()
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
	o.storeSubscriberDeadLetter(subscriber, event, maxRetry)
	_ = o.repo.RecordOperation(context.Background(), subscriber.pluginID, subscriber.artifactID, "event_subscriber_delivery", "dead_letter", "system", "event subscriber delivery failed", map[string]any{
		"event_plugin_id": event.pluginID,
		"event_name":      event.name,
		"subscriber_mode": subscriber.mode,
	})
}

func (o *Operations) storeSubscriberDeadLetter(subscriber *subscriberHandler, event queuedEvent, attempts int) {
	entry := subscriberDeadLetterEntry{
		subscriberPluginID:   subscriber.pluginID,
		subscriberArtifactID: subscriber.artifactID,
		deliveryMode:         subscriber.mode,
		attempts:             attempts,
		nodeID:               o.nodeID,
		event:                copyQueuedEvent(event),
	}
	if id, err := o.repo.SaveSubscriberDeadLetter(context.Background(), subscriberDeadLetterRecordFromEntry(entry, "pending", "subscriber delivery failed")); err == nil {
		entry.id = id
	}
	o.restoreSubscriberDeadLetter(entry)
}

func (o *Operations) restoreSubscriberDeadLetter(entry subscriberDeadLetterEntry) {
	o.subscriberDeadMu.Lock()
	defer o.subscriberDeadMu.Unlock()
	if len(o.subscriberDeadEvents) >= DefaultSubscriberDeadLetterLimit {
		o.subscriberDeadEvents = append([]subscriberDeadLetterEntry(nil), o.subscriberDeadEvents[1:]...)
		o.subscriberDropped.Add(1)
	}
	o.subscriberDeadEvents = append(o.subscriberDeadEvents, entry)
}

func (o *Operations) pendingSubscriberDeadLetters() []subscriberDeadLetterEntry {
	records, err := o.repo.PendingSubscriberDeadLetters(context.Background(), DefaultSubscriberDeadLetterLimit)
	if err == nil && len(records) > 0 {
		entries := make([]subscriberDeadLetterEntry, 0, len(records))
		for _, record := range records {
			entries = append(entries, subscriberDeadLetterEntry{
				id:                   record.ID,
				subscriberPluginID:   record.SubscriberPluginID,
				subscriberArtifactID: record.SubscriberArtifactID,
				deliveryMode:         record.DeliveryMode,
				attempts:             record.Attempts,
				nodeID:               record.NodeID,
				event: queuedEvent{
					pluginID:     record.EventPluginID,
					name:         record.EventName,
					fields:       copyStringMap(record.Fields),
					traceID:      record.TraceID,
					connectionID: record.ConnectionID,
				},
			})
		}
		return entries
	}
	o.subscriberDeadMu.Lock()
	entries := append([]subscriberDeadLetterEntry(nil), o.subscriberDeadEvents...)
	o.subscriberDeadEvents = nil
	o.subscriberDeadMu.Unlock()
	return entries
}

func subscriberDeadLetterRecordFromEntry(entry subscriberDeadLetterEntry, status, reason string) SubscriberDeadLetterRecord {
	return SubscriberDeadLetterRecord{
		ID:                   entry.id,
		SubscriberPluginID:   entry.subscriberPluginID,
		SubscriberArtifactID: entry.subscriberArtifactID,
		EventPluginID:        entry.event.pluginID,
		EventName:            entry.event.name,
		Fields:               copyStringMap(entry.event.fields),
		TraceID:              entry.event.traceID,
		ConnectionID:         entry.event.connectionID,
		DeliveryMode:         entry.deliveryMode,
		Attempts:             entry.attempts,
		NodeID:               entry.nodeID,
		Status:               status,
		Reason:               reason,
	}
}

func copyQueuedEvent(event queuedEvent) queuedEvent {
	event.fields = copyStringMap(event.fields)
	return event
}

func normalizeOperationsExporterType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", OperationsExporterPrometheus:
		return OperationsExporterPrometheus
	case "opentelemetry", "open-telemetry", OperationsExporterOTel:
		return OperationsExporterOTel
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func (rt *operationsExporterRuntime) status() OperationsExporterStatus {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	status := "disabled"
	degraded := false
	unsupported := "external exporter integration is disabled by default; in-process summaries remain authoritative"
	if rt.cfg.Enabled {
		if rt.sink == nil {
			status = "degraded"
			degraded = true
			unsupported = "no external exporter sink is configured"
		} else if rt.lastError != "" {
			status = "degraded"
			degraded = true
			unsupported = "last export failed; connection path remains fail-open"
		} else {
			status = "enabled"
			unsupported = ""
		}
	}
	return OperationsExporterStatus{
		Type:               normalizeOperationsExporterType(rt.cfg.Type),
		Status:             status,
		Enabled:            rt.cfg.Enabled,
		Endpoint:           redactEndpoint(rt.cfg.Endpoint),
		Degraded:           degraded,
		FailOpen:           true,
		LowCardinalityGate: true,
		SensitiveFieldGate: true,
		LastError:          rt.lastError,
		FailureCount:       rt.failureCount,
		LastFailureAt:      rt.lastFailureAt,
		LastSuccessAt:      rt.lastSuccessAt,
		RollbackCount:      rt.rollbackCount,
		LastRollbackAt:     rt.lastRollbackAt,
		Boundary:           "exporter failures are recorded as degraded and never fail plugin connection handling",
		UnsupportedReason:  unsupported,
	}
}

func (rt *operationsExporterRuntime) export(ctx context.Context, snapshot OperationsSnapshot) {
	rt.mu.Lock()
	cfg := rt.cfg
	sink := rt.sink
	rt.mu.Unlock()
	if !cfg.Enabled {
		return
	}
	if sink == nil {
		rt.recordExporterFailure(errors.New("no external exporter sink is configured"))
		return
	}
	batch, err := operationsExportBatch(cfg.Type, snapshot)
	if err != nil {
		rt.recordExporterFailure(err)
		return
	}
	if err := sink.ExportOperations(ctx, batch); err != nil {
		rt.recordExporterFailure(err)
		return
	}
	rt.mu.Lock()
	rt.lastError = ""
	rt.lastSuccessAt = time.Now().Unix()
	rt.mu.Unlock()
}

func (rt *operationsExporterRuntime) recordExporterFailure(err error) {
	if err == nil {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.failureCount++
	rt.lastFailureAt = time.Now().Unix()
	rt.lastError = redactSensitive(err.Error())
}

func operationsExportBatch(exporterType string, snapshot OperationsSnapshot) (OperationsExportBatch, error) {
	batch := OperationsExportBatch{ExporterType: normalizeOperationsExporterType(exporterType)}
	for _, event := range snapshot.Events {
		labels, err := validateExporterLabels(event.Fields)
		if err != nil {
			return batch, fmt.Errorf("event %s labels rejected by exporter boundary: %w", event.Name, err)
		}
		labels["plugin_id"] = event.PluginID
		if err := validateExporterLabelSet(labels); err != nil {
			return batch, fmt.Errorf("event %s labels rejected by exporter boundary: %w", event.Name, err)
		}
		batch.Samples = append(batch.Samples, OperationsExportSample{
			PluginID:  event.PluginID,
			Kind:      "event",
			Name:      event.Name,
			Labels:    labels,
			Value:     float64(event.Count),
			CreatedAt: event.LastSeenAt,
		})
	}
	for _, metric := range snapshot.CustomMetrics {
		labels, err := validateExporterLabels(metric.Labels)
		if err != nil {
			return batch, fmt.Errorf("metric %s labels rejected by exporter boundary: %w", metric.Name, err)
		}
		labels["plugin_id"] = metric.PluginID
		if err := validateExporterLabelSet(labels); err != nil {
			return batch, fmt.Errorf("metric %s labels rejected by exporter boundary: %w", metric.Name, err)
		}
		batch.Samples = append(batch.Samples, OperationsExportSample{
			PluginID:  metric.PluginID,
			Kind:      "metric",
			Name:      metric.Name,
			Labels:    labels,
			Value:     metric.LastValue,
			CreatedAt: metric.LastSeenAt,
		})
	}
	return batch, nil
}

func validateExporterLabels(labels map[string]string) (map[string]string, error) {
	return validateExporterLabelsWithLimit(labels, 12)
}

func validateExporterLabelsWithLimit(labels map[string]string, maxLabels int) (map[string]string, error) {
	if maxLabels <= 0 {
		maxLabels = 12
	}
	if len(labels) > maxLabels {
		return nil, errors.New("too many exporter labels")
	}
	clean := make(map[string]string, len(labels))
	for key, value := range labels {
		if !metricNamePattern.MatchString(key) {
			return clean, fmt.Errorf("invalid exporter label %q", key)
		}
		if isSensitiveName(key) {
			return clean, fmt.Errorf("exporter label %q is sensitive", key)
		}
		if len(value) > DefaultLabelValueMaxBytes {
			return clean, fmt.Errorf("exporter label %q value exceeds low-cardinality size limit", key)
		}
		value = redactSensitive(value)
		if value == "[REDACTED]" {
			return clean, fmt.Errorf("exporter label %q contains sensitive value", key)
		}
		if value == "" {
			continue
		}
		clean[key] = value
	}
	return clean, nil
}

func validateExporterLabelSet(labels map[string]string) error {
	_, err := validateExporterLabelsWithLimit(labels, 13)
	return err
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

func (po *PluginOperations) ExternalDependencySummary(name string) (ExternalDependencySummary, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return ExternalDependencySummary{}, errors.New("external dependency name is required")
	}
	po.mu.Lock()
	runtime := po.externals[name]
	if runtime == nil {
		spec := po.externalSpecs[name]
		if spec.Name == "" {
			po.mu.Unlock()
			return ExternalDependencySummary{}, fmt.Errorf("external dependency %q is not declared by manifest", name)
		}
		runtime = &externalRuntime{spec: spec}
		po.externals[name] = runtime
	}
	po.mu.Unlock()
	return runtime.summary(po.pluginID, name, po.manifest.Runtime.Type), nil
}

func (po *PluginOperations) HealthCheckExternalDependency(ctx context.Context, name string) (ExternalDependencySummary, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return ExternalDependencySummary{}, errors.New("external dependency name is required")
	}
	po.mu.Lock()
	spec := po.externalSpecs[name]
	po.mu.Unlock()
	if spec.Name == "" {
		return ExternalDependencySummary{}, fmt.Errorf("external dependency %q is not declared by manifest", name)
	}
	err := po.ExternalClient(name).HealthCheck(ctx)
	summary, summaryErr := po.ExternalDependencySummary(name)
	if summaryErr != nil {
		return ExternalDependencySummary{}, summaryErr
	}
	return summary, err
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
			nodeID:       po.parent.nodeID,
			spec:         spec,
			confirmToken: randomToken(),
		}
		po.tasks[task.ID] = rt
	}
	rt.nodeID = po.parent.nodeID
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
	policy := normalizeTaskRunPolicy(task.spec.RunPolicy)
	shardKey := taskLeaseShardKey(policy, task.spec)
	leaseRequired := taskLeaseRequired(policy)
	if !leaseRequired {
		task.leaseOwner = ""
		task.leaseExpiresAt = 0
		task.leaseAcquired = false
	}
	task.mu.Unlock()

	maxAttempts := taskMaxAttempts(task.spec)
	leaseTTL := taskLeaseTTL(task.spec, timeout*time.Duration(maxAttempts))
	leaseOwned := false
	if leaseRequired {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		record, acquired, err := po.parent.repo.AcquireTaskLease(ctx, po.pluginID, task.task.ID, shardKey, po.parent.nodeID, leaseTTL)
		cancel()
		task.mu.Lock()
		task.leaseOwner = record.OwnerNodeID
		task.leaseExpiresAt = record.ExpiresAt
		task.leaseAcquired = acquired
		if err != nil {
			task.running = false
			task.consecutiveFailures++
			task.lastError = redactSensitive(err.Error())
			task.mu.Unlock()
			return
		}
		if !acquired {
			task.running = false
			task.skipped++
			task.leaseSkipped++
			task.lastError = fmt.Sprintf("task lease held by node %s until %d", record.OwnerNodeID, record.ExpiresAt)
			task.mu.Unlock()
			return
		}
		task.mu.Unlock()
		leaseOwned = true
	}

	runCtx, cancel := context.WithCancel(context.Background())
	task.mu.Lock()
	task.cancel = cancel
	task.mu.Unlock()

	var stopRenew chan struct{}
	var renewDone chan struct{}
	leaseLost := make(chan string, 1)
	if leaseOwned {
		stopRenew = make(chan struct{})
		renewDone = make(chan struct{})
		go po.renewTaskLease(runCtx, task, shardKey, leaseTTL, cancel, stopRenew, renewDone, leaseLost)
	}

	start := time.Now()
	attempts := 0
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		attempts = attempt
		attemptCtx, attemptCancel := context.WithTimeout(runCtx, timeout)
		err = task.task.Run(attemptCtx)
		attemptCancel()
		if err == nil || runCtx.Err() != nil {
			break
		}
	}
	cancel()
	if stopRenew != nil {
		close(stopRenew)
		<-renewDone
	}
	if leaseOwned {
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), time.Second)
		_ = po.parent.repo.ReleaseTaskLease(releaseCtx, po.pluginID, task.task.ID, shardKey, po.parent.nodeID)
		releaseCancel()
	}

	task.mu.Lock()
	task.running = false
	task.cancel = nil
	if leaseOwned {
		task.leaseAcquired = false
		task.leaseOwner = ""
		task.leaseExpiresAt = 0
	}
	task.lastRunAt = start.Unix()
	task.lastDurationMS = time.Since(start).Milliseconds()
	task.lastAttempts = attempts
	if err != nil {
		task.consecutiveFailures++
		select {
		case leaseErr := <-leaseLost:
			task.lastError = redactSensitive(leaseErr)
		default:
			task.lastError = redactSensitive(err.Error())
		}
	} else {
		task.consecutiveFailures = 0
		task.lastError = ""
	}
	task.mu.Unlock()
}

func (po *PluginOperations) renewTaskLease(ctx context.Context, task *taskRuntime, shardKey string, ttl time.Duration, cancel context.CancelFunc, stop <-chan struct{}, done chan<- struct{}, leaseLost chan<- string) {
	defer close(done)
	interval := ttl / 2
	if interval <= 0 {
		interval = time.Second
	}
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	if interval >= ttl && ttl > 0 {
		interval = ttl / 2
		if interval <= 0 {
			interval = ttl
		}
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-timer.C:
			renewCtx, renewCancel := context.WithTimeout(context.Background(), time.Second)
			record, renewed, err := po.parent.repo.RenewTaskLease(renewCtx, po.pluginID, task.task.ID, shardKey, po.parent.nodeID, ttl)
			renewCancel()
			task.mu.Lock()
			task.leaseOwner = record.OwnerNodeID
			task.leaseExpiresAt = record.ExpiresAt
			task.leaseAcquired = renewed && record.OwnerNodeID == po.parent.nodeID
			task.mu.Unlock()
			if err != nil {
				select {
				case leaseLost <- "task lease renew failed: " + err.Error():
				default:
				}
				cancel()
				return
			}
			if !renewed {
				message := "task lease lost"
				if record.OwnerNodeID != "" {
					message = fmt.Sprintf("task lease held by node %s until %d", record.OwnerNodeID, record.ExpiresAt)
				}
				select {
				case leaseLost <- message:
				default:
				}
				cancel()
				return
			}
			timer.Reset(interval)
		}
	}
}

func normalizeTaskRunPolicy(policy string) string {
	switch strings.ReplaceAll(strings.ToLower(strings.TrimSpace(policy)), "-", "_") {
	case "", TaskRunPolicyPerNode:
		return TaskRunPolicyPerNode
	case TaskRunPolicySingleton:
		return TaskRunPolicySingleton
	case TaskRunPolicySharded:
		return TaskRunPolicySharded
	default:
		return policy
	}
}

func taskLeaseRequired(policy string) bool {
	return policy == TaskRunPolicySingleton || policy == TaskRunPolicySharded
}

func taskLeaseShardKey(policy string, spec TaskSpec) string {
	if policy == TaskRunPolicySingleton {
		return "global"
	}
	if policy == TaskRunPolicySharded {
		if key := strings.TrimSpace(spec.ShardKey); key != "" {
			return key
		}
		return "default"
	}
	return ""
}

func taskLeaseTTL(spec TaskSpec, timeout time.Duration) time.Duration {
	ttl := DefaultTaskLeaseTTL
	if timeout > 0 && timeout*2 > ttl {
		ttl = timeout * 2
	}
	if spec.LeaseTTL != "" {
		if parsed, err := time.ParseDuration(spec.LeaseTTL); err == nil && parsed > 0 {
			ttl = parsed
		}
	}
	return ttl
}

func taskMaxAttempts(spec TaskSpec) int {
	return taskRetryLimit(spec) + 1
}

func taskRetryLimit(spec TaskSpec) int {
	if spec.Retry <= 0 {
		return 0
	}
	if spec.Retry > 10 {
		return 10
	}
	return spec.Retry
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

func (po *PluginOperations) CancelTask(taskID string) (BackgroundTaskSummary, error) {
	po.mu.Lock()
	task := po.tasks[taskID]
	po.mu.Unlock()
	if task == nil {
		return BackgroundTaskSummary{}, fmt.Errorf("background task %q not found", taskID)
	}
	task.mu.Lock()
	cancel := task.cancel
	running := task.running
	if cancel != nil {
		cancel()
		task.lastError = "cancellation requested"
	}
	task.mu.Unlock()
	if !running || cancel == nil {
		return task.summary(), errors.New("background task is not running")
	}
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
		externals = append(externals, ext.summary(po.pluginID, name, po.manifest.Runtime.Type))
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
		Exporters:            po.parent.ExporterStatuses(),
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
			SubscriberDeadLetters: po.parent.SubscriberDeadLetters(),
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

func (o *Operations) CancelTask(pluginID, taskID string) (BackgroundTaskSummary, error) {
	o.mu.RLock()
	po := o.plugins[pluginID]
	o.mu.RUnlock()
	if po == nil {
		return BackgroundTaskSummary{}, ErrPluginNotFound
	}
	return po.CancelTask(taskID)
}

// DiagnosticPackage 生成可下载的插件诊断包。输出前会统一脱敏，避免把密钥、
// token 或完整协议载荷写入可共享文件。
func (o *Operations) DiagnosticPackage(ctx context.Context, plugin PluginRecord, artifact ArtifactRecord, manifest Manifest, handlers []DispatchHandlerSummary, builds []BuildRecord, gc []GCCandidate) ([]byte, DiagnosticPackageSummary, error) {
	snapshot := o.Snapshot(ctx, plugin.ID, handlers, builds, gc)
	operations, _ := o.repo.ListOperations(ctx, plugin.ID, 50)
	body := map[string]any{
		"created_at":        time.Now().Unix(),
		"plugin":            diagnosticPluginRecord(plugin, manifest),
		"plugin_state":      diagnosticPluginState(plugin),
		"manifest":          diagnosticSafeValue(manifest),
		"dispatch_summary":  diagnosticSafeValue(snapshot.Handlers),
		"recent_errors":     diagnosticRecentErrors(plugin, snapshot, operations),
		"trace_summary":     diagnosticSafeValue(snapshot.Traces),
		"event_summary":     diagnosticSafeValue(snapshot.Events),
		"metric_summary":    diagnosticSafeValue(snapshot.CustomMetrics),
		"operations":        diagnosticSafeValue(snapshot),
		"recent_operations": diagnosticOperationRecords(operations),
		"runbook":           diagnosticRunbook(plugin, snapshot, gc),
		"redaction_policy": []string{
			"secret", "token", "password", "session response", "full packet payload",
		},
	}
	sections := []string{
		"plugin", "plugin_state", "manifest", "dispatch_summary", "recent_errors",
		"trace_summary", "event_summary", "metric_summary", "operations", "recent_operations", "runbook",
	}
	if wasmSummary := diagnosticWASMSummary(plugin, artifact, manifest); wasmSummary != nil {
		body["wasm_runtime"] = diagnosticSafeValue(wasmSummary)
		sections = append(sections, "wasm_runtime")
	}
	if sandboxSummary := diagnosticSandboxSummary(plugin, artifact, manifest); sandboxSummary != nil {
		body["sandbox_runtime"] = diagnosticSafeValue(sandboxSummary)
		sections = append(sections, "sandbox_runtime")
	}
	data, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return nil, DiagnosticPackageSummary{}, err
	}
	dir := filepath.Join(o.runtimeRoot(), "diagnostics", plugin.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, DiagnosticPackageSummary{}, err
	}
	file := filepath.Join(dir, fmt.Sprintf("%d.json", time.Now().UnixNano()))
	if err := os.WriteFile(file, data, 0600); err != nil {
		return nil, DiagnosticPackageSummary{}, err
	}
	summary, err := o.repo.SaveDiagnostic(ctx, plugin.ID, file, int64(len(data)), sections)
	if err != nil {
		return nil, DiagnosticPackageSummary{}, err
	}
	return data, summary, nil
}

func diagnosticSandboxSummary(plugin PluginRecord, artifact ArtifactRecord, manifest Manifest) map[string]any {
	if artifact.RuntimeType != RuntimeSandbox && manifest.Runtime.Type != RuntimeSandbox {
		return nil
	}
	runtimeSummary := jsonMapFromJSONString(plugin.RuntimeSummaryJSON)
	return map[string]any{
		"manifest_summary": map[string]any{
			"id":                         manifest.ID,
			"name":                       manifest.Name,
			"version":                    manifest.Version,
			"runtime_type":               firstNonEmpty(manifest.Runtime.Type, artifact.RuntimeType),
			"runtime_entry":              manifest.Runtime.Entry,
			"protocol":                   manifest.Runtime.Protocol,
			"abi_version":                manifest.Runtime.ABIVersion,
			"os":                         manifest.Runtime.OS,
			"arch":                       manifest.Runtime.Arch,
			"supported_extension_points": wasmManifestExtensionKeys(manifest),
			"limits": map[string]any{
				"handler_timeout_ms": manifest.RuntimeLimits.HandlerTimeoutMS,
				"memory_bytes":       manifest.RuntimeLimits.MemoryBytes,
			},
		},
		"runtime_status": map[string]any{
			"runtime_instance_id": runtimeSummary["runtime_instance_id"],
			"state":               runtimeSummary["runtime_state"],
			"reason_code":         runtimeSummary["reason_code"],
			"pid":                 runtimeSummary["pid"],
			"control_socket":      runtimeSummary["control_socket"],
			"cgroup":              runtimeSummary["cgroup"],
			"network_namespace":   runtimeSummary["network_namespace"],
			"active_calls":        runtimeSummary["active_calls"],
			"active_streams":      runtimeSummary["active_streams"],
			"last_error":          runtimeSummary["last_error"],
		},
		"enforcement_status": map[string]any{
			"namespace_enforced":  runtimeSummary["namespace_enforced"],
			"filesystem_enforced": runtimeSummary["filesystem_enforced"],
			"network_enforced":    runtimeSummary["network_enforced"],
			"env_enforced":        runtimeSummary["env_enforced"],
			"cpu_memory_enforced": runtimeSummary["cpu_memory_enforced"],
			"process_enforced":    runtimeSummary["process_enforced"],
			"cleanup_enforced":    runtimeSummary["cleanup_enforced"],
			"secret_rpc":          runtimeSummary["secret_rpc"],
			"facts":               runtimeSummary["enforcement_facts"],
			"attributes":          runtimeSummary["enforcement_attributes"],
		},
		"stdout_stderr": map[string]any{
			"stdout_summary": jsonMapFromAny(runtimeSummary["enforcement_attributes"])["stdout_summary"],
			"stderr_summary": jsonMapFromAny(runtimeSummary["enforcement_attributes"])["stderr_summary"],
			"redacted":       true,
		},
	}
}

func diagnosticWASMSummary(plugin PluginRecord, artifact ArtifactRecord, manifest Manifest) map[string]any {
	if artifact.RuntimeType != RuntimeWASM && manifest.Runtime.Type != RuntimeWASM {
		return nil
	}
	runtimeSummary := jsonMapFromJSONString(plugin.RuntimeSummaryJSON)
	artifactMetadata := jsonMapFromJSONString(artifact.MetadataJSON)
	wasmMetadata := jsonMapFromAny(artifactMetadata["wasm"])
	requiredExports, _ := wasmRequiredExports(manifest)
	moduleHash := diagnosticStringFromAny(runtimeSummary["module_hash"])
	if moduleHash == "" {
		moduleHash = diagnosticStringFromAny(wasmMetadata["module_sha256"])
	}
	if moduleHash == "" {
		moduleHash = artifact.SHA256
	}
	return map[string]any{
		"manifest_summary": map[string]any{
			"id":                         manifest.ID,
			"name":                       manifest.Name,
			"version":                    manifest.Version,
			"runtime_type":               firstNonEmpty(manifest.Runtime.Type, artifact.RuntimeType),
			"runtime_entry":              manifest.Runtime.Entry,
			"supported_extension_points": wasmManifestExtensionKeys(manifest),
			"limits": map[string]any{
				"handler_timeout_ms": manifest.RuntimeLimits.HandlerTimeoutMS,
				"memory_bytes":       manifest.RuntimeLimits.MemoryBytes,
				"max_input_bytes":    wasmABIMaxInputBytes,
				"max_output_bytes":   wasmABIMaxOutputBytes,
			},
		},
		"abi_summary": map[string]any{
			"host_abi":                   wasmHostABIV1,
			"manifest_abi":               manifest.Runtime.ABI,
			"required_exports":           requiredExports,
			"host_imports":               wasmABIImportMap(),
			"low_risk_extension_only":    true,
			"supported_extension_points": []string{ExtensionConfigValidate, ExtensionRouteResolve, ExtensionRuleEvaluate},
		},
		"module_hash":         moduleHash,
		"module_cache_status": runtimeSummary["module_cache_status"],
		"artifact_id":         artifact.ID,
		"package_sha256":      artifact.PackageSHA256,
		"metrics": map[string]any{
			"call_count":            runtimeSummary["call_count"],
			"duration_count":        runtimeSummary["duration_count"],
			"duration_sum_ms":       runtimeSummary["duration_sum_ms"],
			"duration_max_ms":       runtimeSummary["duration_max_ms"],
			"duration_histogram_ms": runtimeSummary["duration_histogram_ms"],
			"active_calls":          runtimeSummary["active_calls"],
			"trap_count":            runtimeSummary["trap_count"],
			"timeout_count":         runtimeSummary["timeout_count"],
			"memory_error_count":    runtimeSummary["memory_error_count"],
		},
		"quarantine": map[string]any{
			"quarantined":       runtimeSummary["quarantined"],
			"quarantine_at":     runtimeSummary["quarantine_at"],
			"quarantine_reason": runtimeSummary["quarantine_reason"],
			"trap_threshold":    wasmTrapQuarantineThreshold,
		},
		"recent_failures": runtimeSummary["recent_failures"],
	}
}

func jsonMapFromJSONString(raw string) map[string]any {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out map[string]any
	if json.Unmarshal([]byte(defaultJSONObject(raw)), &out) != nil {
		return nil
	}
	return out
}

func diagnosticStringFromAny(value any) string {
	text, _ := value.(string)
	return text
}

func diagnosticPluginState(plugin PluginRecord) map[string]any {
	return map[string]any{
		"plugin_id":             plugin.ID,
		"desired_state":         plugin.DesiredState,
		"runtime_state":         plugin.RuntimeState,
		"desired_artifact_id":   plugin.DesiredArtifactID,
		"active_artifact_id":    plugin.ActiveArtifactID,
		"loaded_artifact_id":    plugin.LoadedArtifactID,
		"desired_generation":    plugin.DesiredGeneration,
		"applied_generation":    plugin.AppliedGeneration,
		"last_error":            redactSensitive(plugin.LastError),
		"runtime_summary_json":  diagnosticRedactJSONString(plugin.RuntimeSummaryJSON),
		"dispatch_summary_json": diagnosticRedactJSONString(plugin.DispatchSummaryJSON),
	}
}

func diagnosticRecentErrors(plugin PluginRecord, snapshot OperationsSnapshot, operations []OperationRecord) []map[string]any {
	var out []map[string]any
	appendError := func(source, message string, fields map[string]any) {
		message = redactSensitive(strings.TrimSpace(message))
		if message == "" {
			return
		}
		item := map[string]any{
			"source":  source,
			"message": message,
		}
		for key, value := range fields {
			item[key] = diagnosticSafeValue(value)
		}
		out = append(out, item)
	}
	appendError("plugin", plugin.LastError, map[string]any{"plugin_id": plugin.ID})
	for _, operation := range operations {
		if operation.Status != "failed" && operation.Status != "warning" {
			continue
		}
		appendError("operation", operation.Message, map[string]any{
			"operation":  operation.Operation,
			"status":     operation.Status,
			"created_at": operation.CreatedAt,
		})
		if len(out) >= 20 {
			return out
		}
	}
	for _, logItem := range snapshot.Logs {
		if logItem.Level != "error" && logItem.Level != "warn" {
			continue
		}
		appendError("log", logItem.Message, map[string]any{
			"level":      logItem.Level,
			"trace_id":   logItem.TraceID,
			"created_at": logItem.CreatedAt,
		})
		if len(out) >= 20 {
			return out
		}
	}
	for _, dep := range snapshot.ExternalDependencies {
		appendError("external_dependency", dep.RecentError, map[string]any{
			"name":          dep.Name,
			"last_status":   dep.LastStatus,
			"circuit_state": dep.CircuitState,
		})
		if len(out) >= 20 {
			return out
		}
	}
	for _, task := range snapshot.BackgroundTasks {
		appendError("background_task", task.LastError, map[string]any{
			"task_id":    task.TaskID,
			"run_policy": task.RunPolicy,
			"node_id":    task.NodeID,
		})
		if len(out) >= 20 {
			return out
		}
	}
	return out
}

func diagnosticRunbook(plugin PluginRecord, snapshot OperationsSnapshot, gc []GCCandidate) map[string]any {
	actions := []map[string]string{
		{
			"id":      "review_release_gates",
			"command": fmt.Sprintf("gateway plugin preflight %s --profile prod", plugin.ID),
			"reason":  "Re-run config, secret, governance, conformance, advisory, and conflict gates before changing production state.",
		},
		{
			"id":      "collect_diagnostics",
			"command": fmt.Sprintf("gateway plugin diagnose %s", plugin.ID),
			"reason":  "Capture a fresh redacted package after reproducing the incident.",
		},
	}
	if plugin.RuntimeState == RuntimeEnabled || plugin.DesiredState == DesiredEnabled {
		actions = append(actions, map[string]string{
			"id":      "stop_new_dispatch",
			"command": fmt.Sprintf("gateway plugin disable %s", plugin.ID),
			"reason":  "Stop new plugin dispatch before rollback or deep investigation.",
		})
	}
	if plugin.ActiveArtifactID != "" {
		actions = append(actions, map[string]string{
			"id":      "rollback_artifact",
			"command": fmt.Sprintf("gateway plugin rollback %s --artifact <approved-artifact-id>", plugin.ID),
			"reason":  "Rollback only to an artifact that still passes current governance and advisory gates.",
		})
	}
	for _, dep := range snapshot.ExternalDependencies {
		if dep.Name == "" {
			continue
		}
		actions = append(actions, map[string]string{
			"id":      "check_external_dependency",
			"command": fmt.Sprintf("gateway plugin external health-check %s %s", plugin.ID, dep.Name),
			"reason":  fmt.Sprintf("Verify external dependency %s before blaming plugin code.", dep.Name),
		})
	}
	if len(gc) > 0 {
		actions = append(actions, map[string]string{
			"id":      "review_gc",
			"command": fmt.Sprintf("gateway plugin gc --gateway <admin-url> --token <token>"),
			"reason":  "Review protected and expired plugin data/files before applying cleanup.",
		})
	}
	return map[string]any{
		"summary": map[string]any{
			"plugin_id":           plugin.ID,
			"desired_state":       plugin.DesiredState,
			"runtime_state":       plugin.RuntimeState,
			"desired_artifact_id": plugin.DesiredArtifactID,
			"active_artifact_id":  plugin.ActiveArtifactID,
			"handler_count":       len(snapshot.Handlers),
			"external_count":      len(snapshot.ExternalDependencies),
			"gc_candidate_count":  len(gc),
		},
		"actions": actions,
		"notes": []string{
			"Diagnostic packages are redacted but should still be treated as internal operational data.",
			"Do not bypass governance, conformance, advisory, or secret checks during rollback.",
		},
	}
}

func diagnosticPluginRecord(plugin PluginRecord, manifest Manifest) PluginRecord {
	plugin.ConfigJSON = diagnosticRedactedConfig(plugin.ConfigJSON, manifest)
	plugin.LastError = redactSensitive(plugin.LastError)
	plugin.RuntimeSummaryJSON = diagnosticRedactJSONString(plugin.RuntimeSummaryJSON)
	plugin.DispatchSummaryJSON = diagnosticRedactJSONString(plugin.DispatchSummaryJSON)
	return plugin
}

func diagnosticRedactedConfig(configJSON string, manifest Manifest) string {
	configJSON = defaultJSONObject(configJSON)
	paths := sensitiveConfigPaths(manifest.ConfigSchema, configJSON)
	redacted, err := redactJSON(configJSON, paths)
	if err != nil {
		return `"[REDACTED]"`
	}
	return redacted
}

func diagnosticOperationRecords(records []OperationRecord) []OperationRecord {
	out := make([]OperationRecord, 0, len(records))
	for _, record := range records {
		record.Actor = redactSensitive(record.Actor)
		record.Message = redactSensitive(record.Message)
		record.MetadataJSON = diagnosticRedactJSONString(record.MetadataJSON)
		out = append(out, record)
	}
	return out
}

func diagnosticRedactJSONString(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return redactSensitive(limitString(raw, 512))
	}
	data, err := json.Marshal(diagnosticRedactValue(value, ""))
	if err != nil {
		return "[REDACTED]"
	}
	return string(data)
}

func diagnosticSafeValue(value any) any {
	data, err := json.Marshal(value)
	if err != nil {
		return "[REDACTED]"
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return "[REDACTED]"
	}
	return diagnosticRedactValue(decoded, "")
}

func diagnosticRedactValue(value any, key string) any {
	if isDiagnosticSensitiveName(key) {
		return "[REDACTED]"
	}
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for childKey, child := range typed {
			out[childKey] = diagnosticRedactValue(child, childKey)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, child := range typed {
			out[i] = diagnosticRedactValue(child, key)
		}
		return out
	case string:
		if isDiagnosticMachineCodeName(key) {
			return limitString(typed, 128)
		}
		if isEndpointName(key) {
			return redactSensitive(redactEndpoint(typed))
		}
		return redactSensitive(limitString(typed, 512))
	default:
		return value
	}
}

func isEndpointName(name string) bool {
	lower := strings.ToLower(name)
	return lower == "endpoint" || lower == "url" || strings.Contains(lower, "uri")
}

func isDiagnosticMachineCodeName(name string) bool {
	lower := strings.ToLower(name)
	return lower == "reason_code" || lower == "error_code"
}

func isDiagnosticSensitiveName(name string) bool {
	lower := strings.ToLower(name)
	for _, marker := range []string{"secret", "password", "token", "credential", "authorization"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func (o *Operations) runtimeRoot() string {
	if o.root == "" {
		return os.TempDir()
	}
	return filepath.Join(o.root, "_runtime")
}

func (o *Operations) GCCandidates(ctx context.Context, pluginID string) ([]GCCandidate, error) {
	now := time.Now().Unix()
	candidates := o.retentionRuleCandidates(pluginID)
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
			Kind:          "plugin_data",
			Category:      "data",
			ID:            record.Key,
			PluginID:      record.PluginID,
			Protected:     !expired,
			Reason:        reason,
			RetentionRule: dataRetentionRule(record.ExpiresAt),
			SizeBytes:     record.SizeBytes,
			CreatedAt:     record.UpdatedAt,
			ExpiresAt:     record.ExpiresAt,
			Referenced:    !expired,
		})
	}
	files, err := o.repo.ListPluginFiles(ctx, pluginID)
	if err != nil {
		return nil, err
	}
	seenRuntimeFiles := make(map[string]bool)
	for _, record := range files {
		expired := record.ExpiresAt > 0 && record.ExpiresAt <= now
		filePath := filepath.Join(o.runtimeRoot(), record.PluginID, record.Namespace, filepath.FromSlash(record.Path))
		seenRuntimeFiles[filePath] = true
		reason := "plugin file retained"
		if expired {
			reason = "plugin file retention expired"
		}
		candidates = append(candidates, GCCandidate{
			Kind:          "plugin_file",
			Category:      "file",
			ID:            record.Namespace + "/" + record.Path,
			PluginID:      record.PluginID,
			Path:          filePath,
			Protected:     !expired,
			Reason:        reason,
			RetentionRule: dataRetentionRule(record.ExpiresAt),
			SizeBytes:     record.SizeBytes,
			CreatedAt:     record.UpdatedAt,
			ExpiresAt:     record.ExpiresAt,
			Referenced:    !expired,
		})
	}
	diagnostics, err := o.repo.ListDiagnosticRecords(ctx, pluginID)
	if err != nil {
		return nil, err
	}
	retentionSeconds := int64(DefaultDiagnosticRetention.Seconds())
	for _, record := range diagnostics {
		if record.Path != "" {
			seenRuntimeFiles[record.Path] = true
		}
		expired := record.CreatedAt > 0 && record.CreatedAt+retentionSeconds <= now
		missing := false
		if record.Path != "" {
			if _, err := os.Stat(record.Path); errors.Is(err, os.ErrNotExist) {
				missing = true
			}
		}
		reason := "diagnostic package retained"
		if expired {
			reason = "diagnostic package retention expired"
		}
		if missing {
			reason = "diagnostic package file missing"
		}
		candidates = append(candidates, GCCandidate{
			Kind:             "diagnostic_package",
			Category:         "diagnostic",
			ID:               fmt.Sprintf("%d", record.ID),
			PluginID:         record.PluginID,
			Path:             record.Path,
			Protected:        !expired && !missing,
			Reason:           reason,
			RetentionRule:    "diagnostic packages retained for 7d by default",
			RetentionSeconds: retentionSeconds,
			SizeBytes:        record.SizeBytes,
			CreatedAt:        record.CreatedAt,
			ExpiresAt:        record.CreatedAt + retentionSeconds,
			Referenced:       !expired && !missing,
		})
	}
	leases, err := o.repo.ListTaskLeases(ctx, pluginID)
	if err != nil {
		return nil, err
	}
	for _, lease := range leases {
		expired := lease.ExpiresAt > 0 && lease.ExpiresAt <= now
		reason := "background task lease retained"
		if expired {
			reason = "background task lease expired"
		}
		candidates = append(candidates, GCCandidate{
			Kind:          "background_task_lease",
			Category:      "background_task",
			ID:            strings.Join([]string{lease.TaskID, lease.ShardKey}, "/"),
			PluginID:      lease.PluginID,
			Protected:     !expired,
			Reason:        reason,
			RetentionRule: "singleton/sharded task leases are retained until expires_at to protect cross-node ownership",
			SizeBytes:     int64(len(lease.PluginID) + len(lease.TaskID) + len(lease.ShardKey) + len(lease.OwnerNodeID)),
			CreatedAt:     lease.AcquiredAt,
			ExpiresAt:     lease.ExpiresAt,
			Referenced:    !expired,
		})
	}
	runtimeRoot := o.runtimeRoot()
	_ = filepath.WalkDir(runtimeRoot, func(filePath string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.Contains(filePath, string(filepath.Separator)+"_runtime"+string(filepath.Separator)) {
			return nil
		}
		if seenRuntimeFiles[filePath] {
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
			Kind:          "plugin_file_orphan",
			Category:      "file",
			ID:            id,
			PluginID:      pid,
			Path:          filePath,
			Protected:     false,
			Reason:        "orphaned runtime file",
			RetentionRule: "runtime files without PluginFileStore or diagnostic records are removable",
			SizeBytes:     info.Size(),
			CreatedAt:     info.ModTime().Unix(),
		})
		return nil
	})
	candidates = append(candidates, o.sandboxRuntimeGCCandidates(pluginID)...)
	if pluginID != "" {
		if candidate, ok, err := o.repo.EventRetentionOverflow(ctx, pluginID, DefaultEventRecentLimit); err != nil {
			return nil, err
		} else if ok {
			candidates = append(candidates, candidate)
		}
		if candidate, ok, err := o.repo.LogRetentionOverflow(ctx, pluginID, DefaultLogRecentLimit); err != nil {
			return nil, err
		} else if ok {
			candidates = append(candidates, candidate)
		}
		if candidate, ok, err := o.repo.TraceRetentionOverflow(ctx, pluginID, DefaultEventRecentLimit); err != nil {
			return nil, err
		} else if ok {
			candidates = append(candidates, candidate)
		}
	}
	return candidates, nil
}

func (o *Operations) sandboxRuntimeGCCandidates(pluginID string) []GCCandidate {
	runtimeRoot := o.runtimeRoot()
	var candidates []GCCandidate
	_ = filepath.WalkDir(runtimeRoot, func(filePath string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if candidate, ok := sandboxRuntimeGCCandidate(filePath, d, runtimeRoot, pluginID); ok {
			candidates = append(candidates, candidate)
			if d.IsDir() {
				return filepath.SkipDir
			}
		}
		return nil
	})
	for _, entry := range readDirEntries(os.TempDir()) {
		filePath := filepath.Join(os.TempDir(), entry.Name())
		if candidate, ok := sandboxRuntimeGCCandidate(filePath, entry, runtimeRoot, pluginID); ok {
			candidates = append(candidates, candidate)
		}
	}
	return candidates
}

func readDirEntries(root string) []os.DirEntry {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	return entries
}

func sandboxRuntimeGCCandidate(filePath string, d os.DirEntry, runtimeRoot, pluginID string) (GCCandidate, bool) {
	name := d.Name()
	if !strings.HasPrefix(name, "mc-gateway-sandbox-") && !strings.HasPrefix(name, "sandbox-cgroup-") {
		return GCCandidate{}, false
	}
	if !d.IsDir() && !strings.HasSuffix(name, ".sock") && !strings.HasSuffix(name, ".cgroup") {
		return GCCandidate{}, false
	}
	info, err := d.Info()
	if err != nil {
		return GCCandidate{}, false
	}
	pid := sandboxGCPluginID(filePath, runtimeRoot)
	if pluginID != "" && pid != pluginID {
		return GCCandidate{}, false
	}
	kind := "sandbox_runtime_temp_dir"
	category := "runtime"
	reason := "stale sandbox runtime temp dir"
	if strings.HasSuffix(name, ".sock") {
		kind = "sandbox_stale_socket"
		category = "socket"
		reason = "stale sandbox control socket"
	} else if strings.HasPrefix(name, "sandbox-cgroup-") || strings.HasSuffix(name, ".cgroup") {
		kind = "sandbox_stale_cgroup"
		category = "cgroup"
		reason = "stale sandbox cgroup marker"
	}
	size := info.Size()
	if d.IsDir() {
		size = dirSize(filePath)
	}
	return GCCandidate{
		Kind:          kind,
		Category:      category,
		ID:            filePath,
		PluginID:      pid,
		Path:          filePath,
		Protected:     false,
		Reason:        reason,
		RetentionRule: "sandbox runtime leftovers are removable after process exit or failed startup",
		SizeBytes:     size,
		CreatedAt:     info.ModTime().Unix(),
	}, true
}

func sandboxGCPluginID(filePath, runtimeRoot string) string {
	if rel, err := filepath.Rel(runtimeRoot, filePath); err == nil && !strings.HasPrefix(rel, "..") {
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) > 1 {
			return parts[0]
		}
	}
	return ""
}

func (o *Operations) retentionRuleCandidates(pluginID string) []GCCandidate {
	pluginLabel := pluginID
	if pluginLabel == "" {
		pluginLabel = "*"
	}
	rules := []struct {
		category string
		rule     string
		reason   string
	}{
		{"diagnostic", "diagnostic packages retained for 7d by default", "diagnostic package retention rule"},
		{"event", fmt.Sprintf("keep latest %d event records per plugin", DefaultEventRecentLimit), "event retention rule"},
		{"metric", "custom metrics keep in-memory current summary only; no long-term gateway metric series are retained", "metric retention rule"},
		{"trace", fmt.Sprintf("keep latest %d trace summary records per plugin", DefaultEventRecentLimit), "trace retention rule"},
		{"background_task", "task run summaries stay in memory; singleton/sharded leases are retained until expires_at", "background task retention rule"},
		{"data", "PluginDataStore records without expires_at are protected; records with expired expires_at are removable", "plugin data retention rule"},
		{"file", "PluginFileStore records without expires_at are protected; expired records and orphan runtime files are removable", "plugin file retention rule"},
	}
	out := make([]GCCandidate, 0, len(rules))
	for _, rule := range rules {
		out = append(out, GCCandidate{
			Kind:          "retention_rule",
			Category:      rule.category,
			ID:            pluginLabel + "/" + rule.category,
			PluginID:      pluginID,
			Protected:     true,
			Reason:        rule.reason,
			RetentionRule: rule.rule,
			Referenced:    true,
		})
	}
	return out
}

func dataRetentionRule(expiresAt int64) string {
	if expiresAt <= 0 {
		return "no expires_at set; protected until plugin or admin deletes it"
	}
	return "expires_at controls retention; removable after deadline"
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
		case "sandbox_runtime_temp_dir", "sandbox_stale_socket", "sandbox_stale_cgroup":
			_ = os.RemoveAll(candidate.Path)
			removed = append(removed, candidate)
		case "diagnostic_package":
			id, err := strconv.ParseInt(candidate.ID, 10, 64)
			if err != nil {
				continue
			}
			if candidate.Path != "" {
				_ = os.Remove(candidate.Path)
			}
			_ = o.repo.DeleteDiagnostic(ctx, id)
			removed = append(removed, candidate)
		case "background_task_lease":
			taskID, shardKey, _ := strings.Cut(candidate.ID, "/")
			_ = o.repo.DeleteTaskLease(ctx, candidate.PluginID, taskID, shardKey)
			removed = append(removed, candidate)
		case "event":
			if count, err := o.repo.DeleteEventRetentionOverflow(ctx, candidate.PluginID, DefaultEventRecentLimit); err == nil && count > 0 {
				removed = append(removed, candidate)
			}
		case "plugin_log":
			if count, err := o.repo.DeleteLogRetentionOverflow(ctx, candidate.PluginID, DefaultLogRecentLimit); err == nil && count > 0 {
				removed = append(removed, candidate)
			}
		case "trace":
			if count, err := o.repo.DeleteTraceRetentionOverflow(ctx, candidate.PluginID, DefaultEventRecentLimit); err == nil && count > 0 {
				removed = append(removed, candidate)
			}
		}
	}
	_ = o.repo.RecordOperation(ctx, pluginID, "", "plugin_operations_gc", "succeeded", actor, "plugin operations gc completed", map[string]any{
		"removed":       len(removed),
		"removed_kinds": gcCandidateKindCounts(removed),
	})
	return removed, nil
}

func gcCandidateKindCounts(candidates []GCCandidate) map[string]int {
	counts := make(map[string]int)
	for _, candidate := range candidates {
		counts[candidate.Kind]++
	}
	return counts
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
	var oldSize int64
	if files, err := s.ops.parent.repo.ListPluginFiles(ctx, s.ops.pluginID); err == nil {
		for _, existing := range files {
			if existing.Namespace == namespace && existing.Path == clean {
				oldSize = existing.SizeBytes
				break
			}
		}
	}
	nextUsage := usage - oldSize + int64(len(data))
	if nextUsage > quota {
		return fmt.Errorf("plugin file quota exceeded: %d > %d", nextUsage, quota)
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
	start := time.Now()
	req.URL, err = externalHTTPURLForSpec(spec, req.URL)
	if err != nil {
		c.finish(start, "policy_denied", err)
		c.recordTrace(ctx, start, "http", "policy_denied")
		return api.ExternalResponse{}, err
	}
	if req.Method == "" {
		req.Method = http.MethodGet
	}
	if err := c.beforeRequest(); err != nil {
		c.finish(start, "circuit_open", err)
		c.recordTrace(ctx, start, "http", "circuit_open")
		return api.ExternalResponse{}, err
	}
	// beforeRequest 会增加 inflight，后续必须在 defer 中成对减少。
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
		c.recordTrace(ctx, start, "http", "request_error")
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
		c.recordTrace(ctx, start, "http", "error")
		return api.ExternalResponse{}, lastErr
	}
	defer resp.Body.Close()
	body, err := readLimited(resp.Body, 1024*1024)
	if err != nil {
		c.finish(start, "read_error", err)
		c.recordTrace(ctx, start, "http", "read_error")
		return api.ExternalResponse{}, err
	}
	if resp.StatusCode >= 500 {
		err := fmt.Errorf("external dependency %s returned status %d", c.name, resp.StatusCode)
		c.finish(start, fmt.Sprintf("http_%d", resp.StatusCode), err)
		c.recordTrace(ctx, start, "http", fmt.Sprintf("http_%d", resp.StatusCode))
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
	start := time.Now()
	address, err = externalTCPAddressForSpec(spec, address)
	if err != nil {
		c.finish(start, "policy_denied", err)
		c.recordTrace(ctx, start, "tcp", "policy_denied")
		return nil, err
	}
	if timeout <= 0 {
		timeout = parseDurationDefault(spec.Timeout, DefaultExternalTimeout)
	}
	if err := c.beforeRequest(); err != nil {
		c.finish(start, "circuit_open", err)
		c.recordTrace(ctx, start, "tcp", "circuit_open")
		return nil, err
	}
	defer c.runtime.inflight.Add(-1)
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// 外部 TCP 连接返回给插件后由插件负责关闭；这里仅记录拨号阶段的观测信息。
	var dialer net.Dialer
	conn, err := dialer.DialContext(dialCtx, "tcp", address)
	if err != nil {
		c.finish(start, "dial_error", err)
		c.recordTrace(ctx, start, "tcp", "dial_error")
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

func externalHTTPURLForSpec(spec ExternalSpec, raw string) (string, error) {
	endpoint, err := url.Parse(strings.TrimSpace(spec.Endpoint))
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return "", fmt.Errorf("external dependency %q endpoint is invalid", spec.Name)
	}
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return "", fmt.Errorf("external dependency %q does not allow HTTP access", spec.Name)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return endpoint.String(), nil
	}
	target, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("external dependency %q request URL is invalid", spec.Name)
	}
	if !target.IsAbs() {
		target = endpoint.ResolveReference(target)
	}
	if target.Scheme != endpoint.Scheme || !strings.EqualFold(target.Host, endpoint.Host) {
		return "", fmt.Errorf("external dependency %q request URL is outside declared endpoint", spec.Name)
	}
	if !externalHTTPPathInScope(endpoint.Path, target.Path) {
		return "", fmt.Errorf("external dependency %q request URL is outside declared endpoint path", spec.Name)
	}
	return target.String(), nil
}

func externalHTTPPathInScope(scopePath, targetPath string) bool {
	scopePath = cleanExternalHTTPPath(scopePath)
	targetPath = cleanExternalHTTPPath(targetPath)
	if scopePath == "/" {
		return true
	}
	return targetPath == scopePath || strings.HasPrefix(targetPath, scopePath+"/")
}

func cleanExternalHTTPPath(value string) string {
	if value == "" {
		return "/"
	}
	clean := path.Clean(value)
	if clean == "." {
		return "/"
	}
	if !strings.HasPrefix(clean, "/") {
		clean = "/" + clean
	}
	return clean
}

func externalTCPAddressForSpec(spec ExternalSpec, address string) (string, error) {
	endpoint := strings.TrimSpace(spec.Endpoint)
	if !strings.HasPrefix(endpoint, "tcp://") {
		return "", fmt.Errorf("external dependency %q does not allow TCP access", spec.Name)
	}
	declared := strings.TrimPrefix(endpoint, "tcp://")
	address = strings.TrimSpace(address)
	if address == "" {
		return declared, nil
	}
	if !strings.EqualFold(address, declared) {
		return "", fmt.Errorf("external dependency %q request address is outside declared endpoint", spec.Name)
	}
	return address, nil
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
		"endpoint":    redactEndpoint(c.runtime.spec.Endpoint),
		"purpose":     redactSensitive(c.runtime.spec.Purpose),
		"timeout_ms":  fmt.Sprintf("%d", parseDurationDefault(c.runtime.spec.Timeout, DefaultExternalTimeout).Milliseconds()),
		"fail_policy": normalizeExternalFailPolicy(c.runtime.spec),
	})
}

// summary 返回外部依赖的可展示状态，并隐藏 endpoint 中可能带账号的信息。
func (rt *externalRuntime) summary(pluginID, name, runtimeType string) ExternalDependencySummary {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	state := circuitClosed
	if time.Now().Before(rt.circuitUntil) {
		state = circuitOpen
	}
	networkBoundary := "native go plugins can bypass ExternalClient; gateway observes declared clients but does not provide strong network isolation"
	networkEnforced := false
	if runtimeType == RuntimeSandbox {
		networkBoundary = "sandbox-process raw network is disabled; declared external dependencies use host-mediated ExternalClient policy"
		networkEnforced = true
	}
	return ExternalDependencySummary{
		PluginID:            pluginID,
		Name:                name,
		Endpoint:            redactEndpoint(rt.spec.Endpoint),
		Purpose:             redactSensitive(rt.spec.Purpose),
		Required:            rt.spec.Required,
		FailPolicy:          normalizeExternalFailPolicy(rt.spec),
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
		NetworkBoundary:     networkBoundary,
		NetworkEnforced:     networkEnforced,
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
	policy := normalizeTaskRunPolicy(rt.spec.RunPolicy)
	shardKey := taskLeaseShardKey(policy, rt.spec)
	return BackgroundTaskSummary{
		PluginID:            rt.pluginID,
		TaskID:              rt.task.ID,
		Name:                rt.task.Name,
		Mode:                mode,
		RunPolicy:           policy,
		NodeID:              rt.nodeID,
		ShardKey:            shardKey,
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
		Retry:               taskRetryLimit(rt.spec),
		LastAttempts:        rt.lastAttempts,
		LeaseRequired:       taskLeaseRequired(policy),
		LeaseAcquired:       rt.leaseAcquired,
		LeaseOwner:          rt.leaseOwner,
		LeaseExpiresAt:      rt.leaseExpiresAt,
		LeaseSkipped:        rt.leaseSkipped,
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
	parsed, err := url.Parse(endpoint)
	if err == nil && parsed.User != nil {
		parsed.User = url.User("[REDACTED]")
	}
	if err == nil {
		query := parsed.Query()
		for key := range query {
			lower := strings.ToLower(key)
			for _, marker := range []string{"secret", "token", "password", "session", "credential", "authorization"} {
				if strings.Contains(lower, marker) {
					query.Set(key, "[REDACTED]")
					break
				}
			}
		}
		parsed.RawQuery = query.Encode()
		endpoint = parsed.String()
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
