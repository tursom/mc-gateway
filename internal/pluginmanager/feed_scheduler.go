package pluginmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	ExternalFeedKindAdvisory      = "advisory"
	ExternalFeedKindVulnerability = "vulnerability"

	defaultExternalFeedInterval = 24 * time.Hour
	defaultExternalFeedTimeout  = 15 * time.Second
	defaultExternalFeedMaxBytes = 4 * 1024 * 1024
)

type ExternalFeedSchedule struct {
	Name       string
	URL        string
	Source     string
	Interval   time.Duration
	Timeout    time.Duration
	RunOnStart bool
}

func (m *Manager) StartExternalFeedSchedulers(ctx context.Context, advisoryFeeds, vulnerabilityFeeds []ExternalFeedSchedule) error {
	if m.closing.Load() {
		return ErrManagerClosed
	}
	if len(advisoryFeeds) == 0 && len(vulnerabilityFeeds) == 0 {
		return nil
	}
	for _, schedule := range advisoryFeeds {
		if err := validateExternalFeedSchedule(ExternalFeedKindAdvisory, schedule); err != nil {
			return err
		}
	}
	for _, schedule := range vulnerabilityFeeds {
		if err := validateExternalFeedSchedule(ExternalFeedKindVulnerability, schedule); err != nil {
			return err
		}
	}

	m.StopExternalFeedSchedulers()
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	m.feedSchedulerMu.Lock()
	m.feedSchedulerCancel = cancel
	m.feedSchedulerDone = done
	m.feedSchedulerMu.Unlock()

	advisoryFeeds = append([]ExternalFeedSchedule(nil), advisoryFeeds...)
	vulnerabilityFeeds = append([]ExternalFeedSchedule(nil), vulnerabilityFeeds...)
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for _, schedule := range advisoryFeeds {
			wg.Add(1)
			go func(schedule ExternalFeedSchedule) {
				defer wg.Done()
				m.runExternalFeedSchedule(runCtx, ExternalFeedKindAdvisory, schedule)
			}(schedule)
		}
		for _, schedule := range vulnerabilityFeeds {
			wg.Add(1)
			go func(schedule ExternalFeedSchedule) {
				defer wg.Done()
				m.runExternalFeedSchedule(runCtx, ExternalFeedKindVulnerability, schedule)
			}(schedule)
		}
		wg.Wait()
	}()
	return nil
}

func (m *Manager) StopExternalFeedSchedulers() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = m.stopExternalFeedSchedulers(ctx)
}

func (m *Manager) stopExternalFeedSchedulers(ctx context.Context) error {
	m.feedSchedulerMu.Lock()
	cancel := m.feedSchedulerCancel
	done := m.feedSchedulerDone
	m.feedSchedulerCancel = nil
	m.feedSchedulerDone = nil
	m.feedSchedulerMu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) SyncExternalAdvisoryFeed(ctx context.Context, actor string, schedule ExternalFeedSchedule) (AdvisoryFeedResult, error) {
	if err := validateExternalFeedSchedule(ExternalFeedKindAdvisory, schedule); err != nil {
		return AdvisoryFeedResult{}, err
	}
	data, source, err := fetchExternalFeed(ctx, schedule)
	if err != nil {
		return AdvisoryFeedResult{}, err
	}
	req, err := decodeExternalAdvisoryFeed(data)
	if err != nil {
		return AdvisoryFeedResult{}, err
	}
	if strings.TrimSpace(req.Source) == "" {
		req.Source = source
	}
	return m.SyncAdvisoryFeed(ctx, defaultExternalFeedActor(actor), req)
}

func (m *Manager) SyncExternalVulnerabilityFeed(ctx context.Context, actor string, schedule ExternalFeedSchedule) (VulnerabilityDBResult, error) {
	if err := validateExternalFeedSchedule(ExternalFeedKindVulnerability, schedule); err != nil {
		return VulnerabilityDBResult{}, err
	}
	data, source, err := fetchExternalFeed(ctx, schedule)
	if err != nil {
		return VulnerabilityDBResult{}, err
	}
	req, err := decodeExternalVulnerabilityFeed(data)
	if err != nil {
		return VulnerabilityDBResult{}, err
	}
	if strings.TrimSpace(req.Source) == "" {
		req.Source = source
	}
	return m.ImportVulnerabilityDB(ctx, defaultExternalFeedActor(actor), req)
}

func (m *Manager) runExternalFeedSchedule(ctx context.Context, kind string, schedule ExternalFeedSchedule) {
	schedule = defaultExternalFeedSchedule(schedule)
	if schedule.RunOnStart {
		m.runExternalFeedSync(ctx, kind, schedule)
	}
	ticker := time.NewTicker(schedule.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.runExternalFeedSync(ctx, kind, schedule)
		}
	}
}

func (m *Manager) runExternalFeedSync(ctx context.Context, kind string, schedule ExternalFeedSchedule) {
	start := time.Now()
	actor := "system"
	var imported int
	var matches int
	var blocking int
	var warnings int
	var err error
	switch kind {
	case ExternalFeedKindAdvisory:
		var result AdvisoryFeedResult
		result, err = m.SyncExternalAdvisoryFeed(ctx, actor, schedule)
		imported = result.Imported
		matches = len(result.Rescan.Matches)
		blocking = result.Rescan.Blocking
		warnings = result.Rescan.Warnings
	case ExternalFeedKindVulnerability:
		var result VulnerabilityDBResult
		result, err = m.SyncExternalVulnerabilityFeed(ctx, actor, schedule)
		imported = result.Imported
		matches = len(result.Scan.Matches)
		blocking = result.Scan.Blocking
		warnings = result.Scan.Warnings
	default:
		err = fmt.Errorf("unknown external feed kind %q", kind)
	}
	status := "succeeded"
	message := "external plugin feed synced"
	if err != nil {
		status = "failed"
		message = err.Error()
	}
	_ = m.repo.RecordOperation(context.Background(), "", "", "external_"+kind+"_feed_sync", status, actor, message, map[string]any{
		"name":        schedule.Name,
		"source":      externalFeedSource(schedule),
		"url":         redactSensitive(redactEndpoint(schedule.URL)),
		"imported":    imported,
		"matches":     matches,
		"blocking":    blocking,
		"warnings":    warnings,
		"duration_ms": time.Since(start).Milliseconds(),
	})
}

func validateExternalFeedSchedule(kind string, schedule ExternalFeedSchedule) error {
	switch kind {
	case ExternalFeedKindAdvisory, ExternalFeedKindVulnerability:
	default:
		return fmt.Errorf("unknown external feed kind %q", kind)
	}
	if strings.TrimSpace(schedule.URL) == "" {
		return fmt.Errorf("%s feed url is required", kind)
	}
	if schedule.Interval < 0 {
		return fmt.Errorf("%s feed interval must be non-negative", kind)
	}
	if schedule.Timeout < 0 {
		return fmt.Errorf("%s feed timeout must be non-negative", kind)
	}
	return nil
}

func defaultExternalFeedSchedule(schedule ExternalFeedSchedule) ExternalFeedSchedule {
	if schedule.Interval <= 0 {
		schedule.Interval = defaultExternalFeedInterval
	}
	if schedule.Timeout <= 0 {
		schedule.Timeout = defaultExternalFeedTimeout
	}
	return schedule
}

func defaultExternalFeedActor(actor string) string {
	if strings.TrimSpace(actor) == "" {
		return "system"
	}
	return actor
}

func externalFeedSource(schedule ExternalFeedSchedule) string {
	if strings.TrimSpace(schedule.Source) != "" {
		return redactSensitive(strings.TrimSpace(schedule.Source))
	}
	if strings.TrimSpace(schedule.Name) != "" {
		return redactSensitive(strings.TrimSpace(schedule.Name))
	}
	return redactSensitive(redactEndpoint(strings.TrimSpace(schedule.URL)))
}

func fetchExternalFeed(ctx context.Context, schedule ExternalFeedSchedule) ([]byte, string, error) {
	schedule = defaultExternalFeedSchedule(schedule)
	feedURL := strings.TrimSpace(schedule.URL)
	fetchCtx, cancel := context.WithTimeout(ctx, schedule.Timeout)
	defer cancel()
	parsed, err := url.Parse(feedURL)
	if err != nil {
		return nil, "", err
	}
	var reader io.ReadCloser
	switch parsed.Scheme {
	case "http", "https":
		req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, feedURL, nil)
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("Accept", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, "", err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			_ = resp.Body.Close()
			return nil, "", fmt.Errorf("external feed returned %s", resp.Status)
		}
		reader = resp.Body
	case "file":
		file, err := os.Open(parsed.Path)
		if err != nil {
			return nil, "", err
		}
		reader = file
	case "":
		file, err := os.Open(feedURL)
		if err != nil {
			return nil, "", err
		}
		reader = file
	default:
		return nil, "", fmt.Errorf("unsupported external feed scheme %q", parsed.Scheme)
	}
	defer reader.Close()
	data, err := readExternalFeedData(reader)
	if err != nil {
		return nil, "", err
	}
	return data, externalFeedSource(schedule), nil
}

func readExternalFeedData(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, defaultExternalFeedMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > defaultExternalFeedMaxBytes {
		return nil, errors.New("external feed is too large")
	}
	return data, nil
}

func decodeExternalAdvisoryFeed(data []byte) (AdvisoryFeedRequest, error) {
	var req AdvisoryFeedRequest
	if err := json.Unmarshal(data, &req); err == nil && len(req.Advisories) > 0 {
		return req, nil
	}
	var advisories []AdvisoryRequest
	if err := json.Unmarshal(data, &advisories); err != nil {
		return AdvisoryFeedRequest{}, fmt.Errorf("external advisory feed must be an object with advisories or an advisory array: %w", err)
	}
	return AdvisoryFeedRequest{Advisories: advisories}, nil
}

func decodeExternalVulnerabilityFeed(data []byte) (VulnerabilityDBRequest, error) {
	var req VulnerabilityDBRequest
	if err := json.Unmarshal(data, &req); err == nil && len(req.Vulnerabilities) > 0 {
		return req, nil
	}
	var vulnerabilities []VulnerabilityRequest
	if err := json.Unmarshal(data, &vulnerabilities); err != nil {
		return VulnerabilityDBRequest{}, fmt.Errorf("external vulnerability feed must be an object with vulnerabilities or a vulnerability array: %w", err)
	}
	return VulnerabilityDBRequest{Vulnerabilities: vulnerabilities}, nil
}
