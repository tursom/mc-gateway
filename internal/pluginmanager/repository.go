// internal/pluginmanager/repository.go 持久化插件制品、插件记录、快照、构建、密钥、评审、公告和操作日志。

package pluginmanager

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Repository struct {
	db  *sql.DB
	now func() time.Time
}

type secretMaterial struct {
	SecretRecord
	CurrentValue  string
	PreviousValue string
}

func NewRepository(db *sql.DB) Repository {
	return Repository{
		db:  db,
		now: time.Now,
	}
}

func NewRepositoryWithClock(db *sql.DB, now func() time.Time) Repository {
	repo := NewRepository(db)
	if now != nil {
		repo.now = now
	}
	return repo
}

func (r Repository) SaveArtifact(ctx context.Context, artifact ArtifactRecord) error {
	_, err := r.db.ExecContext(ctx, `
INSERT INTO plugin_artifacts(
    id, plugin_id, version, file_name, file_path, sha256, package_sha256, size_bytes,
    artifact_type, runtime_type, runtime_entry, status, metadata_json,
    capabilities_summary_json, extension_points_json, api_version, go_version, go_os, go_arch,
    uploaded_by, error, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    file_name = excluded.file_name,
    file_path = excluded.file_path,
    package_sha256 = excluded.package_sha256,
    status = excluded.status,
    metadata_json = excluded.metadata_json,
    capabilities_summary_json = excluded.capabilities_summary_json,
    extension_points_json = excluded.extension_points_json,
    uploaded_by = excluded.uploaded_by,
    error = excluded.error,
    updated_at = excluded.updated_at`,
		artifact.ID, artifact.PluginID, artifact.Version, artifact.FileName, artifact.FilePath, artifact.SHA256, artifact.PackageSHA256, artifact.SizeBytes,
		artifact.ArtifactType, artifact.RuntimeType, artifact.RuntimeEntry, artifact.Status, artifact.MetadataJSON,
		artifact.CapabilitiesSummaryJSON, artifact.ExtensionPointsJSON, artifact.APIVersion, artifact.GoVersion, artifact.GOOS, artifact.GOARCH,
		artifact.UploadedBy, artifact.Error, artifact.CreatedAt, artifact.UpdatedAt)
	return err
}

func (r Repository) Artifact(ctx context.Context, id string) (ArtifactRecord, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT id, plugin_id, version, file_name, file_path, sha256, package_sha256, size_bytes,
       artifact_type, runtime_type, runtime_entry, status, metadata_json,
       capabilities_summary_json, extension_points_json, api_version, go_version, go_os, go_arch,
       uploaded_by, error, created_at, updated_at
FROM plugin_artifacts
WHERE id = ?`, id)
	return scanArtifact(row)
}

func (r Repository) ListArtifacts(ctx context.Context, pluginID string) ([]ArtifactRecord, error) {
	query := `
SELECT id, plugin_id, version, file_name, file_path, sha256, package_sha256, size_bytes,
       artifact_type, runtime_type, runtime_entry, status, metadata_json,
       capabilities_summary_json, extension_points_json, api_version, go_version, go_os, go_arch,
       uploaded_by, error, created_at, updated_at
FROM plugin_artifacts`
	var args []any
	if pluginID != "" {
		query += ` WHERE plugin_id = ?`
		args = append(args, pluginID)
	}
	query += ` ORDER BY created_at DESC, id DESC`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var artifacts []ArtifactRecord
	for rows.Next() {
		artifact, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, artifact)
	}
	return artifacts, rows.Err()
}

func (r Repository) UpsertDesired(ctx context.Context, actor, pluginID, artifactID, desiredState, configJSON string, priority int) (PluginRecord, error) {
	if desiredState == "" {
		desiredState = DesiredDisabled
	}
	if configJSON == "" {
		configJSON = "{}"
	}
	if !json.Valid([]byte(configJSON)) {
		return PluginRecord{}, errors.New("config_json must be valid JSON")
	}
	if priority == 0 {
		priority = DefaultPriority
	}
	switch desiredState {
	case DesiredEnabled, DesiredDisabled, DesiredDeleted:
	default:
		return PluginRecord{}, errors.New("invalid desired_state")
	}
	artifact, err := r.Artifact(ctx, artifactID)
	if err != nil {
		return PluginRecord{}, err
	}
	if artifact.PluginID != pluginID {
		return PluginRecord{}, errors.New("artifact plugin_id does not match")
	}
	if artifact.ArtifactType != ArtifactTypeBinary {
		return PluginRecord{}, errors.New("desired artifact must be a binary artifact")
	}

	now := r.now().Unix()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return PluginRecord{}, err
	}
	defer tx.Rollback()

	var existing PluginRecord
	row := tx.QueryRowContext(ctx, `
SELECT id, desired_artifact_id, active_artifact_id, loaded_artifact_id, desired_state, runtime_state,
       priority, config_json, desired_generation, applied_generation, last_error,
       runtime_summary_json, dispatch_summary_json, created_at, updated_at, updated_by
FROM plugins WHERE id = ?`, pluginID)
	err = scanPluginRow(row, &existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return PluginRecord{}, err
	}

	nextGeneration := int64(1)
	createdAt := now
	if err == nil {
		nextGeneration = existing.DesiredGeneration + 1
		createdAt = existing.CreatedAt
		if _, err := tx.ExecContext(ctx, `
INSERT INTO plugin_config_snapshots(plugin_id, artifact_id, config_json, desired_state, priority, desired_generation, created_by, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			existing.ID, existing.DesiredArtifactID, existing.ConfigJSON, existing.DesiredState, existing.Priority, existing.DesiredGeneration, actor, now); err != nil {
			return PluginRecord{}, err
		}
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO plugins(
    id, desired_artifact_id, active_artifact_id, loaded_artifact_id, desired_state, runtime_state,
    priority, config_json, desired_generation, applied_generation, last_error,
    runtime_summary_json, dispatch_summary_json, deleted_at, created_at, updated_at, updated_by
) VALUES (?, ?, '', '', ?, ?, ?, ?, ?, 0, '', '{}', '{}', 0, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    desired_artifact_id = excluded.desired_artifact_id,
    desired_state = excluded.desired_state,
    priority = excluded.priority,
    config_json = excluded.config_json,
    desired_generation = excluded.desired_generation,
    deleted_at = CASE WHEN excluded.desired_state = 'deleted' THEN excluded.updated_at ELSE 0 END,
    updated_at = excluded.updated_at,
    updated_by = excluded.updated_by`,
		pluginID, artifactID, desiredState, RuntimeDisabled, priority, configJSON, nextGeneration, createdAt, now, actor); err != nil {
		return PluginRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return PluginRecord{}, err
	}
	return r.Plugin(ctx, pluginID)
}

func (r Repository) RestoreSnapshot(ctx context.Context, actor string, snapshot ConfigSnapshotRecord) (PluginRecord, error) {
	if snapshot.PluginID == "" {
		return PluginRecord{}, errors.New("snapshot plugin_id is required")
	}
	return r.UpsertDesired(ctx, actor, snapshot.PluginID, snapshot.ArtifactID, snapshot.DesiredState, snapshot.ConfigJSON, snapshot.Priority)
}

func (r Repository) Plugin(ctx context.Context, id string) (PluginRecord, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT id, desired_artifact_id, active_artifact_id, loaded_artifact_id, desired_state, runtime_state,
       priority, config_json, desired_generation, applied_generation, last_error,
       runtime_summary_json, dispatch_summary_json, created_at, updated_at, updated_by
FROM plugins
WHERE id = ? AND desired_state <> 'deleted'`, id)
	var plugin PluginRecord
	if err := scanPluginRow(row, &plugin); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PluginRecord{}, ErrPluginNotFound
		}
		return PluginRecord{}, err
	}
	return plugin, nil
}

func (r Repository) ListPlugins(ctx context.Context) ([]PluginRecord, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT id, desired_artifact_id, active_artifact_id, loaded_artifact_id, desired_state, runtime_state,
       priority, config_json, desired_generation, applied_generation, last_error,
       runtime_summary_json, dispatch_summary_json, created_at, updated_at, updated_by
FROM plugins
WHERE desired_state <> 'deleted'
ORDER BY priority ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var plugins []PluginRecord
	for rows.Next() {
		var plugin PluginRecord
		if err := scanPluginRow(rows, &plugin); err != nil {
			return nil, err
		}
		plugins = append(plugins, plugin)
	}
	return plugins, rows.Err()
}

func (r Repository) ListConfigSnapshots(ctx context.Context, pluginID string) ([]ConfigSnapshotRecord, error) {
	query := `
SELECT id, plugin_id, artifact_id, config_json, desired_state, priority, desired_generation, created_by, created_at
FROM plugin_config_snapshots`
	var args []any
	if pluginID != "" {
		query += ` WHERE plugin_id = ?`
		args = append(args, pluginID)
	}
	query += ` ORDER BY created_at DESC, id DESC`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var snapshots []ConfigSnapshotRecord
	for rows.Next() {
		snapshot, err := scanConfigSnapshot(rows)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, rows.Err()
}

func (r Repository) ConfigSnapshot(ctx context.Context, id int64) (ConfigSnapshotRecord, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT id, plugin_id, artifact_id, config_json, desired_state, priority, desired_generation, created_by, created_at
FROM plugin_config_snapshots
WHERE id = ?`, id)
	return scanConfigSnapshot(row)
}

func (r Repository) DesiredEnabled(ctx context.Context) ([]PluginRecord, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT id, desired_artifact_id, active_artifact_id, loaded_artifact_id, desired_state, runtime_state,
       priority, config_json, desired_generation, applied_generation, last_error,
       runtime_summary_json, dispatch_summary_json, created_at, updated_at, updated_by
FROM plugins
WHERE desired_state = 'enabled'
ORDER BY priority ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var plugins []PluginRecord
	for rows.Next() {
		var plugin PluginRecord
		if err := scanPluginRow(rows, &plugin); err != nil {
			return nil, err
		}
		plugins = append(plugins, plugin)
	}
	return plugins, rows.Err()
}

func (r Repository) MarkRuntime(ctx context.Context, pluginID, runtimeState, activeArtifactID, loadedArtifactID string, appliedGeneration int64, lastError string, runtimeSummary, dispatchSummary any) error {
	now := r.now().Unix()
	runtimeJSON, err := marshalDefaultObject(runtimeSummary)
	if err != nil {
		return err
	}
	dispatchJSON, err := marshalDefaultObject(dispatchSummary)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `
UPDATE plugins
SET runtime_state = ?, active_artifact_id = ?, loaded_artifact_id = ?, applied_generation = ?,
    last_error = ?, runtime_summary_json = ?, dispatch_summary_json = ?, updated_at = ?
WHERE id = ?`,
		runtimeState, activeArtifactID, loadedArtifactID, appliedGeneration, lastError, runtimeJSON, dispatchJSON, now, pluginID)
	return err
}

func (r Repository) PluginServiceState(ctx context.Context) (PluginServiceState, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT desired_mode, active_mode, applied_at, live_migration, crash_backoff_seconds, crash_max_count, crash_window_seconds, last_error, updated_by, updated_at
FROM plugin_service_state WHERE id = 1`)
	state, err := scanPluginServiceState(row)
	if errors.Is(err, sql.ErrNoRows) {
		now := r.now().Unix()
		_, err = r.db.ExecContext(ctx, `
INSERT OR IGNORE INTO plugin_service_state(id, desired_mode, active_mode, applied_at, live_migration, crash_backoff_seconds, crash_max_count, crash_window_seconds, updated_by, updated_at)
VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			PluginServiceModeInProcess, PluginServiceModeInProcess, now, PluginMigrationDrainOnly, DefaultPluginHostCrashBackoffSeconds, DefaultPluginHostCrashMaxCrashes, DefaultPluginHostCrashWindowSeconds, "system", now)
		if err != nil {
			return PluginServiceState{}, err
		}
		return PluginServiceState{
			DesiredMode:   PluginServiceModeInProcess,
			ActiveMode:    PluginServiceModeInProcess,
			AppliedAt:     now,
			LiveMigration: PluginMigrationDrainOnly,
			CrashPolicy:   normalizePluginHostCrashPolicy(PluginHostCrashPolicy{}),
			UpdatedBy:     "system",
			UpdatedAt:     now,
		}, nil
	}
	state = normalizePluginServiceState(state)
	return state, err
}

func (r Repository) SetPluginServiceDesired(ctx context.Context, actor, mode string) (PluginServiceState, error) {
	now := r.now().Unix()
	if _, err := r.db.ExecContext(ctx, `
INSERT INTO plugin_service_state(id, desired_mode, active_mode, applied_at, live_migration, crash_backoff_seconds, crash_max_count, crash_window_seconds, updated_by, updated_at)
VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET desired_mode = excluded.desired_mode, updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
		mode, PluginServiceModeInProcess, 0, PluginMigrationDrainOnly, DefaultPluginHostCrashBackoffSeconds, DefaultPluginHostCrashMaxCrashes, DefaultPluginHostCrashWindowSeconds, actor, now); err != nil {
		return PluginServiceState{}, err
	}
	return r.PluginServiceState(ctx)
}

func (r Repository) ApplyPluginServiceActive(ctx context.Context, mode string) (PluginServiceState, error) {
	now := r.now().Unix()
	if _, err := r.db.ExecContext(ctx, `
INSERT INTO plugin_service_state(id, desired_mode, active_mode, applied_at, live_migration, crash_backoff_seconds, crash_max_count, crash_window_seconds, last_error, updated_by, updated_at)
VALUES (1, ?, ?, ?, ?, ?, ?, ?, '', 'system', ?)
ON CONFLICT(id) DO UPDATE SET active_mode = excluded.active_mode, applied_at = excluded.applied_at, last_error = '', updated_at = excluded.updated_at`,
		mode, mode, now, PluginMigrationDrainOnly, DefaultPluginHostCrashBackoffSeconds, DefaultPluginHostCrashMaxCrashes, DefaultPluginHostCrashWindowSeconds, now); err != nil {
		return PluginServiceState{}, err
	}
	return r.PluginServiceState(ctx)
}

func (r Repository) SetPluginServiceError(ctx context.Context, message string) error {
	now := r.now().Unix()
	_, err := r.db.ExecContext(ctx, `
INSERT INTO plugin_service_state(id, desired_mode, active_mode, applied_at, live_migration, crash_backoff_seconds, crash_max_count, crash_window_seconds, last_error, updated_by, updated_at)
VALUES (1, ?, ?, 0, ?, ?, ?, ?, ?, 'system', ?)
ON CONFLICT(id) DO UPDATE SET last_error = excluded.last_error, updated_at = excluded.updated_at`,
		PluginServiceModeInProcess, PluginServiceModeInProcess, PluginMigrationDrainOnly, DefaultPluginHostCrashBackoffSeconds, DefaultPluginHostCrashMaxCrashes, DefaultPluginHostCrashWindowSeconds, message, now)
	return err
}

func (r Repository) SetPluginServiceCrashPolicy(ctx context.Context, actor string, policy PluginHostCrashPolicy) (PluginServiceState, error) {
	policy = normalizePluginHostCrashPolicy(policy)
	now := r.now().Unix()
	if _, err := r.db.ExecContext(ctx, `
INSERT INTO plugin_service_state(id, desired_mode, active_mode, applied_at, live_migration, crash_backoff_seconds, crash_max_count, crash_window_seconds, updated_by, updated_at)
VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET crash_backoff_seconds = excluded.crash_backoff_seconds, crash_max_count = excluded.crash_max_count, crash_window_seconds = excluded.crash_window_seconds, updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
		PluginServiceModeInProcess, PluginServiceModeInProcess, 0, PluginMigrationDrainOnly, policy.BackoffSeconds, policy.MaxCrashes, policy.WindowSeconds, actor, now); err != nil {
		return PluginServiceState{}, err
	}
	return r.PluginServiceState(ctx)
}

func (r Repository) UpsertPluginNode(ctx context.Context, node PluginNodeState) error {
	if strings.TrimSpace(node.NodeID) == "" {
		return errors.New("node_id is required")
	}
	now := r.now().Unix()
	if node.StartedAt == 0 {
		node.StartedAt = now
	}
	if node.HeartbeatAt == 0 {
		node.HeartbeatAt = now
	}
	if node.ServiceMode == "" {
		node.ServiceMode = PluginServiceModeInProcess
	}
	if node.DataPlaneMode == "" {
		node.DataPlaneMode = PluginServiceModeInProcess
	}
	if node.Status == "" {
		node.Status = PluginNodeStatusOnline
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO plugin_nodes(node_id, hostname, pid, service_mode, data_plane_mode, status, started_at, heartbeat_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(node_id) DO UPDATE SET
    hostname = excluded.hostname,
    pid = excluded.pid,
    service_mode = excluded.service_mode,
    data_plane_mode = excluded.data_plane_mode,
    status = excluded.status,
    started_at = excluded.started_at,
    heartbeat_at = excluded.heartbeat_at`,
		node.NodeID, node.Hostname, node.PID, node.ServiceMode, node.DataPlaneMode, node.Status, node.StartedAt, node.HeartbeatAt)
	return err
}

func (r Repository) ListPluginNodes(ctx context.Context, staleAfter time.Duration) ([]PluginNodeState, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT node_id, hostname, pid, service_mode, data_plane_mode, status, started_at, heartbeat_at
FROM plugin_nodes
ORDER BY heartbeat_at DESC, node_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := r.now().Unix()
	var staleBefore int64
	if staleAfter > 0 {
		staleBefore = now - int64(staleAfter/time.Second)
	}
	var nodes []PluginNodeState
	for rows.Next() {
		var node PluginNodeState
		if err := rows.Scan(&node.NodeID, &node.Hostname, &node.PID, &node.ServiceMode, &node.DataPlaneMode, &node.Status, &node.StartedAt, &node.HeartbeatAt); err != nil {
			return nil, err
		}
		if staleBefore > 0 && node.HeartbeatAt < staleBefore {
			node.Stale = true
			node.Status = PluginNodeStatusStale
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

func (r Repository) UpsertPluginNodeRuntime(ctx context.Context, state PluginNodeRuntimeState) error {
	if strings.TrimSpace(state.NodeID) == "" || strings.TrimSpace(state.PluginID) == "" {
		return errors.New("node_id and plugin_id are required")
	}
	now := r.now().Unix()
	if state.UpdatedAt == 0 {
		state.UpdatedAt = now
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO plugin_node_states(
    node_id, plugin_id, artifact_id, desired_state, runtime_state, desired_generation,
    applied_generation, loaded, enabled, health, error, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(node_id, plugin_id) DO UPDATE SET
    artifact_id = excluded.artifact_id,
    desired_state = excluded.desired_state,
    runtime_state = excluded.runtime_state,
    desired_generation = excluded.desired_generation,
    applied_generation = excluded.applied_generation,
    loaded = excluded.loaded,
    enabled = excluded.enabled,
    health = excluded.health,
    error = excluded.error,
    updated_at = excluded.updated_at`,
		state.NodeID, state.PluginID, state.ArtifactID, state.DesiredState, state.RuntimeState, state.DesiredGeneration,
		state.AppliedGeneration, boolInt(state.Loaded), boolInt(state.Enabled), state.Health, state.Error, state.UpdatedAt)
	return err
}

func (r Repository) DeletePluginNodeRuntime(ctx context.Context, nodeID, pluginID string) error {
	if strings.TrimSpace(nodeID) == "" || strings.TrimSpace(pluginID) == "" {
		return errors.New("node_id and plugin_id are required")
	}
	_, err := r.db.ExecContext(ctx, `DELETE FROM plugin_node_states WHERE node_id = ? AND plugin_id = ?`, nodeID, pluginID)
	return err
}

func (r Repository) ListPluginNodeRuntime(ctx context.Context, pluginID string, staleAfter time.Duration) ([]PluginNodeRuntimeState, error) {
	query := `
SELECT s.node_id, s.plugin_id, s.artifact_id, s.desired_state, s.runtime_state,
       s.desired_generation, s.applied_generation, s.loaded, s.enabled, s.health,
       s.error, s.updated_at, COALESCE(n.heartbeat_at, 0)
FROM plugin_node_states s
LEFT JOIN plugin_nodes n ON n.node_id = s.node_id`
	var args []any
	if pluginID != "" {
		query += ` WHERE s.plugin_id = ?`
		args = append(args, pluginID)
	}
	query += ` ORDER BY s.plugin_id ASC, s.node_id ASC`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := r.now().Unix()
	var staleBefore int64
	if staleAfter > 0 {
		staleBefore = now - int64(staleAfter/time.Second)
	}
	var states []PluginNodeRuntimeState
	for rows.Next() {
		var state PluginNodeRuntimeState
		var loaded, enabled int
		if err := rows.Scan(
			&state.NodeID, &state.PluginID, &state.ArtifactID, &state.DesiredState, &state.RuntimeState,
			&state.DesiredGeneration, &state.AppliedGeneration, &loaded, &enabled, &state.Health,
			&state.Error, &state.UpdatedAt, &state.NodeHeartbeatAt,
		); err != nil {
			return nil, err
		}
		state.Loaded = loaded != 0
		state.Enabled = enabled != 0
		if state.NodeHeartbeatAt == 0 || (staleBefore > 0 && state.NodeHeartbeatAt < staleBefore) {
			state.Stale = true
		}
		states = append(states, state)
	}
	return states, rows.Err()
}

func (r Repository) UpdateArtifactStatus(ctx context.Context, artifactID, status, message string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE plugin_artifacts SET status = ?, error = ?, updated_at = ? WHERE id = ?`,
		status, message, r.now().Unix(), artifactID)
	return err
}

func (r Repository) CreateBuild(ctx context.Context, build BuildRecord) (BuildRecord, error) {
	now := r.now().Unix()
	build.CreatedAt = now
	build.UpdatedAt = now
	if build.Status == "" {
		build.Status = BuildStatusQueued
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO plugin_builds(
    plugin_id, source_id, artifact_id, status, builder_type, builder_image, builder_version,
    go_version, go_os, go_arch, go_amd64, go_arm64, cgo_enabled, build_tags,
    sdk_module, sdk_version, go_proxy, go_no_sumdb, go_private, vendor_required,
    source_sha256, artifact_sha256, module_summary_json, go_version_m_json, abi_fingerprint,
    log_summary, metadata_json, error, started_at, ended_at, duration_ms, created_by, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		build.PluginID, build.SourceID, build.ArtifactID, build.Status, build.BuilderType, build.BuilderImage, build.BuilderVersion,
		build.GoVersion, build.GOOS, build.GOARCH, build.GOAMD64, build.GOARM64, build.CGOEnabled, build.BuildTags,
		build.SDKModule, build.SDKVersion, build.GOPROXY, build.GONOSUMDB, build.GOPRIVATE, boolInt(build.VendorRequired),
		build.SourceSHA256, build.ArtifactSHA256, defaultJSONArray(build.ModuleSummary), defaultJSONObject(build.GoVersionM), build.ABIFingerprint,
		build.LogSummary, defaultJSONObject(build.MetadataJSON), build.Error, build.StartedAt, build.EndedAt, build.DurationMS, build.CreatedBy, build.CreatedAt, build.UpdatedAt)
	if err != nil {
		return BuildRecord{}, err
	}
	id, err := lastInsertID(ctx, r.db)
	if err != nil {
		return BuildRecord{}, err
	}
	return r.Build(ctx, id)
}

func (r Repository) Build(ctx context.Context, id int64) (BuildRecord, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT id, plugin_id, source_id, artifact_id, status, builder_type, builder_image, builder_version,
       go_version, go_os, go_arch, go_amd64, go_arm64, cgo_enabled, build_tags,
       sdk_module, sdk_version, go_proxy, go_no_sumdb, go_private, vendor_required,
       source_sha256, artifact_sha256, module_summary_json, go_version_m_json, abi_fingerprint,
       log_summary, metadata_json, error, started_at, ended_at, duration_ms, created_by, created_at, updated_at
FROM plugin_builds WHERE id = ?`, id)
	return scanBuild(row)
}

func (r Repository) ListBuilds(ctx context.Context, pluginID string) ([]BuildRecord, error) {
	query := `
SELECT id, plugin_id, source_id, artifact_id, status, builder_type, builder_image, builder_version,
       go_version, go_os, go_arch, go_amd64, go_arm64, cgo_enabled, build_tags,
       sdk_module, sdk_version, go_proxy, go_no_sumdb, go_private, vendor_required,
       source_sha256, artifact_sha256, module_summary_json, go_version_m_json, abi_fingerprint,
       log_summary, metadata_json, error, started_at, ended_at, duration_ms, created_by, created_at, updated_at
FROM plugin_builds`
	var args []any
	if pluginID != "" {
		query += ` WHERE plugin_id = ?`
		args = append(args, pluginID)
	}
	query += ` ORDER BY created_at DESC, id DESC`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var builds []BuildRecord
	for rows.Next() {
		build, err := scanBuild(rows)
		if err != nil {
			return nil, err
		}
		builds = append(builds, build)
	}
	return builds, rows.Err()
}

func (r Repository) MarkBuildRunning(ctx context.Context, id int64) error {
	now := r.now().Unix()
	_, err := r.db.ExecContext(ctx, `UPDATE plugin_builds SET status = ?, started_at = ?, updated_at = ? WHERE id = ? AND status = ?`,
		BuildStatusRunning, now, now, id, BuildStatusQueued)
	return err
}

func (r Repository) FinishBuild(ctx context.Context, build BuildRecord) error {
	now := r.now().Unix()
	if build.EndedAt == 0 {
		build.EndedAt = now
	}
	if build.StartedAt > 0 && build.DurationMS == 0 {
		build.DurationMS = (build.EndedAt - build.StartedAt) * 1000
	}
	_, err := r.db.ExecContext(ctx, `
UPDATE plugin_builds SET
    artifact_id = ?, status = ?, builder_image = ?, builder_version = ?, go_version = ?,
    go_os = ?, go_arch = ?, go_amd64 = ?, go_arm64 = ?, cgo_enabled = ?, build_tags = ?,
    sdk_module = ?, sdk_version = ?, go_proxy = ?, go_no_sumdb = ?, go_private = ?, vendor_required = ?,
    source_sha256 = ?, artifact_sha256 = ?, module_summary_json = ?, go_version_m_json = ?,
    abi_fingerprint = ?, log_summary = ?, metadata_json = ?, error = ?, ended_at = ?, duration_ms = ?, updated_at = ?
WHERE id = ?`,
		build.ArtifactID, build.Status, build.BuilderImage, build.BuilderVersion, build.GoVersion,
		build.GOOS, build.GOARCH, build.GOAMD64, build.GOARM64, build.CGOEnabled, build.BuildTags,
		build.SDKModule, build.SDKVersion, build.GOPROXY, build.GONOSUMDB, build.GOPRIVATE, boolInt(build.VendorRequired),
		build.SourceSHA256, build.ArtifactSHA256, defaultJSONArray(build.ModuleSummary), defaultJSONObject(build.GoVersionM),
		build.ABIFingerprint, build.LogSummary, defaultJSONObject(build.MetadataJSON), build.Error, build.EndedAt, build.DurationMS, now,
		build.ID)
	return err
}

func (r Repository) CancelBuild(ctx context.Context, id int64, actor string) (BuildRecord, error) {
	now := r.now().Unix()
	res, err := r.db.ExecContext(ctx, `
UPDATE plugin_builds
SET status = ?, error = ?, ended_at = ?, updated_at = ?
WHERE id = ? AND status IN (?, ?)`,
		BuildStatusCanceled, "build canceled by "+actor, now, now, id, BuildStatusQueued, BuildStatusRunning)
	if err != nil {
		return BuildRecord{}, err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return r.Build(ctx, id)
	}
	return r.Build(ctx, id)
}

func (r Repository) RetryBuild(ctx context.Context, id int64, actor string) (BuildRecord, error) {
	previous, err := r.Build(ctx, id)
	if err != nil {
		return BuildRecord{}, err
	}
	if previous.Status != BuildStatusFailed && previous.Status != BuildStatusCanceled {
		return BuildRecord{}, fmt.Errorf("build status %q cannot be retried", previous.Status)
	}
	previous.ID = 0
	previous.ArtifactID = ""
	previous.ArtifactSHA256 = ""
	previous.Status = BuildStatusQueued
	previous.LogSummary = ""
	previous.Error = ""
	previous.StartedAt = 0
	previous.EndedAt = 0
	previous.DurationMS = 0
	previous.CreatedBy = actor
	return r.CreateBuild(ctx, previous)
}

func (r Repository) ClearBuildLog(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `
UPDATE plugin_builds
SET log_summary = '', updated_at = ?
WHERE id = ? AND status NOT IN (?, ?)`,
		r.now().Unix(), id, BuildStatusQueued, BuildStatusRunning)
	return err
}

func (r Repository) ReferencedArtifactIDs(ctx context.Context) (map[string]bool, error) {
	refs := make(map[string]bool)
	rows, err := r.db.QueryContext(ctx, `
SELECT desired_artifact_id, active_artifact_id, loaded_artifact_id
FROM plugins
WHERE desired_state <> 'deleted'`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var desired, active, loaded string
		if err := rows.Scan(&desired, &active, &loaded); err != nil {
			rows.Close()
			return nil, err
		}
		for _, id := range []string{desired, active, loaded} {
			if id != "" {
				refs[id] = true
			}
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	rows, err = r.db.QueryContext(ctx, `SELECT artifact_id FROM plugin_config_snapshots WHERE artifact_id <> ''`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		if id != "" {
			refs[id] = true
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	rows, err = r.db.QueryContext(ctx, `SELECT source_id FROM plugin_builds WHERE source_id <> '' AND status IN (?, ?)`, BuildStatusQueued, BuildStatusRunning)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		if id != "" {
			refs[id] = true
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	rows, err = r.db.QueryContext(ctx, `SELECT artifact_id FROM plugin_builds WHERE artifact_id <> '' AND status = ?`, BuildStatusSucceeded)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		if id != "" {
			refs[id] = true
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return refs, nil
}

func (r Repository) UpsertSecret(ctx context.Context, actor, pluginID, name, value string, reloadRequired, hotReload bool) (SecretRecord, error) {
	if pluginID == "" {
		return SecretRecord{}, errors.New("plugin_id is required")
	}
	if !pluginIDPattern.MatchString(pluginID) {
		return SecretRecord{}, fmt.Errorf("invalid plugin id %q", pluginID)
	}
	if !secretNamePattern.MatchString(name) {
		return SecretRecord{}, fmt.Errorf("invalid secret name %q", name)
	}
	if value == "" {
		return SecretRecord{}, errors.New("secret value is required")
	}
	now := r.now().Unix()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return SecretRecord{}, err
	}
	defer tx.Rollback()
	var currentVersion int64
	var currentValue string
	err = tx.QueryRowContext(ctx, `SELECT current_version, current_value FROM plugin_secrets WHERE plugin_id = ? AND name = ?`, pluginID, name).Scan(&currentVersion, &currentValue)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return SecretRecord{}, err
	}
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `
INSERT INTO plugin_secrets(plugin_id, name, current_version, previous_version, current_value, previous_value, reload_required, hot_reload, updated_by, created_at, updated_at)
VALUES (?, ?, 1, 0, ?, '', ?, ?, ?, ?, ?)`,
			pluginID, name, value, boolInt(reloadRequired), boolInt(hotReload), actor, now, now)
	} else {
		_, err = tx.ExecContext(ctx, `
UPDATE plugin_secrets
SET previous_version = current_version,
    previous_value = current_value,
    current_version = current_version + 1,
    current_value = ?,
    reload_required = ?,
    hot_reload = ?,
    updated_by = ?,
    updated_at = ?
WHERE plugin_id = ? AND name = ?`,
			value, boolInt(reloadRequired), boolInt(hotReload), actor, now, pluginID, name)
	}
	if err != nil {
		return SecretRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return SecretRecord{}, err
	}
	return r.Secret(ctx, pluginID, name)
}

func (r Repository) Secret(ctx context.Context, pluginID, name string) (SecretRecord, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT plugin_id, name, current_version, previous_version, reload_required, hot_reload, updated_by, created_at, updated_at
FROM plugin_secrets
WHERE plugin_id = ? AND name = ?`, pluginID, name)
	return scanSecret(row)
}

func (r Repository) SecretMaterial(ctx context.Context, pluginID, name string) (secretMaterial, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT plugin_id, name, current_version, previous_version, current_value, previous_value, reload_required, hot_reload, updated_by, created_at, updated_at
FROM plugin_secrets
WHERE plugin_id = ? AND name = ?`, pluginID, name)
	var material secretMaterial
	var reloadRequired int
	var hotReload int
	err := row.Scan(
		&material.PluginID, &material.Name, &material.CurrentVersion, &material.PreviousVersion,
		&material.CurrentValue, &material.PreviousValue, &reloadRequired, &hotReload,
		&material.UpdatedBy, &material.CreatedAt, &material.UpdatedAt,
	)
	material.ReloadRequired = reloadRequired != 0
	material.HotReload = hotReload != 0
	return material, err
}

func (r Repository) ListSecrets(ctx context.Context, pluginID string) ([]SecretRecord, error) {
	query := `
SELECT plugin_id, name, current_version, previous_version, reload_required, hot_reload, updated_by, created_at, updated_at
FROM plugin_secrets`
	var args []any
	if pluginID != "" {
		query += ` WHERE plugin_id = ?`
		args = append(args, pluginID)
	}
	query += ` ORDER BY plugin_id ASC, name ASC`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var secrets []SecretRecord
	for rows.Next() {
		secret, err := scanSecret(rows)
		if err != nil {
			return nil, err
		}
		secrets = append(secrets, secret)
	}
	return secrets, rows.Err()
}

func (r Repository) SaveReview(ctx context.Context, review ReviewRecord) (ReviewRecord, error) {
	now := r.now().Unix()
	if review.CreatedAt == 0 {
		review.CreatedAt = now
	}
	if review.Profile == "" {
		review.Profile = PolicyProfileDev
	}
	if review.RiskLevel == "" {
		review.RiskLevel = RiskLow
	}
	if review.Decision == "" {
		review.Decision = ReviewDecisionApproved
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO plugin_reviews(
    plugin_id, artifact_id, artifact_hash, profile, risk_level, config_hash, scope_hash, rollout_hash,
    runtime_limits_hash, features_hash, policy_hash, decision, notes, reviewed_by, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		review.PluginID, review.ArtifactID, review.ArtifactHash, review.Profile, review.RiskLevel, review.ConfigHash, review.ScopeHash, review.RolloutHash,
		review.RuntimeLimitsHash, review.FeaturesHash, review.PolicyHash, review.Decision, review.Notes, review.ReviewedBy, review.CreatedAt)
	if err != nil {
		return ReviewRecord{}, err
	}
	id, err := lastInsertID(ctx, r.db)
	if err != nil {
		return ReviewRecord{}, err
	}
	review.ID = id
	return review, nil
}

func (r Repository) ListReviews(ctx context.Context, pluginID string) ([]ReviewRecord, error) {
	query := `
SELECT id, plugin_id, artifact_id, profile, risk_level, config_hash, scope_hash, rollout_hash,
       artifact_hash, runtime_limits_hash, features_hash, policy_hash, decision, notes, reviewed_by, created_at
FROM plugin_reviews`
	var args []any
	if pluginID != "" {
		query += ` WHERE plugin_id = ?`
		args = append(args, pluginID)
	}
	query += ` ORDER BY created_at DESC, id DESC`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var reviews []ReviewRecord
	for rows.Next() {
		review, err := scanReview(rows)
		if err != nil {
			return nil, err
		}
		reviews = append(reviews, review)
	}
	return reviews, rows.Err()
}

func (r Repository) ApprovedReview(ctx context.Context, pluginID, artifactID, profile, policyHash, configHash, scopeHash, rolloutHash, runtimeHash, featuresHash string) (ReviewRecord, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT id, plugin_id, artifact_id, profile, risk_level, config_hash, scope_hash, rollout_hash,
       artifact_hash, runtime_limits_hash, features_hash, policy_hash, decision, notes, reviewed_by, created_at
FROM plugin_reviews
WHERE plugin_id = ? AND artifact_id = ? AND profile = ? AND policy_hash = ?
  AND config_hash = ? AND scope_hash = ? AND rollout_hash = ? AND runtime_limits_hash = ? AND features_hash = ?
  AND decision = ?
ORDER BY created_at DESC, id DESC
LIMIT 1`, pluginID, artifactID, profile, policyHash, configHash, scopeHash, rolloutHash, runtimeHash, featuresHash, ReviewDecisionApproved)
	review, err := scanReview(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewRecord{}, nil
	}
	return review, err
}

func (r Repository) SaveWarningOverride(ctx context.Context, override WarningOverrideRecord) (WarningOverrideRecord, error) {
	now := r.now().Unix()
	if override.CreatedAt == 0 {
		override.CreatedAt = now
	}
	if override.Profile == "" {
		override.Profile = PolicyProfileDev
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO plugin_warning_overrides(plugin_id, artifact_id, profile, action, policy_hash, reason, created_by, expires_at, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		override.PluginID, override.ArtifactID, override.Profile, override.Action, override.PolicyHash, override.Reason, override.CreatedBy, override.ExpiresAt, override.CreatedAt)
	if err != nil {
		return WarningOverrideRecord{}, err
	}
	id, err := lastInsertID(ctx, r.db)
	if err != nil {
		return WarningOverrideRecord{}, err
	}
	override.ID = id
	return override, nil
}

func (r Repository) ActiveWarningOverride(ctx context.Context, pluginID, artifactID, profile, action, policyHash string) (WarningOverrideRecord, bool, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT id, plugin_id, artifact_id, profile, action, policy_hash, reason, created_by, expires_at, created_at
FROM plugin_warning_overrides
WHERE plugin_id = ? AND artifact_id = ? AND profile = ? AND action = ? AND policy_hash = ? AND expires_at > ?
ORDER BY expires_at DESC, id DESC
LIMIT 1`, pluginID, artifactID, profile, action, policyHash, r.now().Unix())
	override, err := scanWarningOverride(row)
	if errors.Is(err, sql.ErrNoRows) {
		return WarningOverrideRecord{}, false, nil
	}
	if err != nil {
		return WarningOverrideRecord{}, false, err
	}
	return override, true, nil
}

func (r Repository) ListWarningOverrides(ctx context.Context, pluginID string) ([]WarningOverrideRecord, error) {
	query := `
SELECT id, plugin_id, artifact_id, profile, action, policy_hash, reason, created_by, expires_at, created_at
FROM plugin_warning_overrides`
	var args []any
	if pluginID != "" {
		query += ` WHERE plugin_id = ?`
		args = append(args, pluginID)
	}
	query += ` ORDER BY created_at DESC, id DESC`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var overrides []WarningOverrideRecord
	for rows.Next() {
		override, err := scanWarningOverride(rows)
		if err != nil {
			return nil, err
		}
		overrides = append(overrides, override)
	}
	return overrides, rows.Err()
}

func (r Repository) UpsertAdvisory(ctx context.Context, actor string, req AdvisoryRequest) (AdvisoryRecord, error) {
	now := r.now().Unix()
	if req.Status == "" {
		req.Status = AdvisoryStatusActive
	}
	if req.Action == "" {
		req.Action = AdvisoryActionDenylist
	}
	if strings.TrimSpace(req.AdvisoryID) == "" {
		return AdvisoryRecord{}, errors.New("advisory_id is required")
	}
	res, err := r.db.ExecContext(ctx, `
UPDATE plugin_advisories
SET status = ?, action = ?, artifact_sha256 = ?, plugin_id = ?, version_range = ?,
    dependency_name = ?, dependency_range = ?, recommended_action = ?, fixed_version = ?,
    mitigation = ?, created_by = ?, updated_at = ?
WHERE advisory_id = ?`,
		req.Status, req.Action, req.ArtifactSHA256, req.PluginID, req.VersionRange,
		req.DependencyName, req.DependencyRange, req.RecommendedAction, req.FixedVersion,
		req.Mitigation, actor, now, req.AdvisoryID)
	if err != nil {
		return AdvisoryRecord{}, err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		_, err = r.db.ExecContext(ctx, `
INSERT INTO plugin_advisories(
    advisory_id, status, action, artifact_sha256, plugin_id, version_range, dependency_name,
    dependency_range, recommended_action, fixed_version, mitigation, created_by, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			req.AdvisoryID, req.Status, req.Action, req.ArtifactSHA256, req.PluginID, req.VersionRange, req.DependencyName,
			req.DependencyRange, req.RecommendedAction, req.FixedVersion, req.Mitigation, actor, now, now)
		if err != nil {
			return AdvisoryRecord{}, err
		}
	}
	return r.AdvisoryByID(ctx, req.AdvisoryID)
}

func (r Repository) AdvisoryByID(ctx context.Context, advisoryID string) (AdvisoryRecord, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT id, advisory_id, status, action, artifact_sha256, plugin_id, version_range, dependency_name,
       dependency_range, recommended_action, fixed_version, mitigation, created_by, created_at, updated_at
FROM plugin_advisories
WHERE advisory_id = ?
ORDER BY id DESC
LIMIT 1`, advisoryID)
	return scanAdvisory(row)
}

func (r Repository) ListAdvisories(ctx context.Context, pluginID string) ([]AdvisoryRecord, error) {
	query := `
SELECT id, advisory_id, status, action, artifact_sha256, plugin_id, version_range, dependency_name,
       dependency_range, recommended_action, fixed_version, mitigation, created_by, created_at, updated_at
FROM plugin_advisories`
	var args []any
	if pluginID != "" {
		query += ` WHERE plugin_id = ? OR plugin_id = ''`
		args = append(args, pluginID)
	}
	query += ` ORDER BY updated_at DESC, id DESC`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var advisories []AdvisoryRecord
	for rows.Next() {
		advisory, err := scanAdvisory(rows)
		if err != nil {
			return nil, err
		}
		advisories = append(advisories, advisory)
	}
	return advisories, rows.Err()
}

func (r Repository) UpsertVulnerability(ctx context.Context, actor string, req VulnerabilityRequest) (VulnerabilityRecord, error) {
	now := r.now().Unix()
	req.VulnerabilityID = strings.TrimSpace(req.VulnerabilityID)
	req.PackageName = strings.TrimSpace(req.PackageName)
	if req.VulnerabilityID == "" {
		return VulnerabilityRecord{}, errors.New("vulnerability_id is required")
	}
	if req.PackageName == "" {
		return VulnerabilityRecord{}, errors.New("package_name is required")
	}
	if req.Status == "" {
		req.Status = AdvisoryStatusActive
	}
	if req.Action == "" {
		req.Action = AdvisoryActionDenylist
	}
	references, err := json.Marshal(req.References)
	if err != nil {
		return VulnerabilityRecord{}, err
	}
	result, err := r.db.ExecContext(ctx, `
UPDATE plugin_vulnerabilities
SET source = ?, status = ?, version_range = ?, severity = ?, action = ?, fixed_version = ?,
    summary = ?, references_json = ?, created_by = ?, updated_at = ?
WHERE vulnerability_id = ? AND package_name = ?`,
		req.Source, req.Status, req.VersionRange, strings.ToLower(req.Severity), req.Action, req.FixedVersion,
		req.Summary, string(references), actor, now, req.VulnerabilityID, req.PackageName)
	if err != nil {
		return VulnerabilityRecord{}, err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		_, err = r.db.ExecContext(ctx, `
INSERT INTO plugin_vulnerabilities(
    vulnerability_id, source, status, package_name, version_range, severity, action,
    fixed_version, summary, references_json, created_by, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			req.VulnerabilityID, req.Source, req.Status, req.PackageName, req.VersionRange, strings.ToLower(req.Severity), req.Action,
			req.FixedVersion, req.Summary, string(references), actor, now, now)
		if err != nil {
			return VulnerabilityRecord{}, err
		}
	}
	return r.VulnerabilityByKey(ctx, req.VulnerabilityID, req.PackageName)
}

func (r Repository) VulnerabilityByKey(ctx context.Context, vulnerabilityID, packageName string) (VulnerabilityRecord, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT id, vulnerability_id, source, status, package_name, version_range, severity, action,
       fixed_version, summary, references_json, created_by, created_at, updated_at
FROM plugin_vulnerabilities
WHERE vulnerability_id = ? AND package_name = ?
ORDER BY id DESC
LIMIT 1`, vulnerabilityID, packageName)
	return scanVulnerability(row)
}

func (r Repository) ListVulnerabilities(ctx context.Context, packageName string) ([]VulnerabilityRecord, error) {
	query := `
SELECT id, vulnerability_id, source, status, package_name, version_range, severity, action,
       fixed_version, summary, references_json, created_by, created_at, updated_at
FROM plugin_vulnerabilities`
	var args []any
	if strings.TrimSpace(packageName) != "" {
		query += ` WHERE package_name = ?`
		args = append(args, strings.TrimSpace(packageName))
	}
	query += ` ORDER BY updated_at DESC, id DESC`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []VulnerabilityRecord
	for rows.Next() {
		record, err := scanVulnerability(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (r Repository) SaveRepositoryImport(ctx context.Context, record RepositoryImportRecord) (RepositoryImportRecord, error) {
	now := r.now().Unix()
	if record.CreatedAt == 0 {
		record.CreatedAt = now
	}
	if record.AdmissionJSON == "" || !json.Valid([]byte(record.AdmissionJSON)) {
		record.AdmissionJSON = "{}"
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO plugin_repository_imports(
    repository_type, index_path, repository_name, candidate_id, plugin_id, version,
    artifact_id, package_sha256, trust_policy, admission_json, imported_by, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.RepositoryType, record.IndexPath, record.RepositoryName, record.CandidateID, record.PluginID, record.Version,
		record.ArtifactID, record.PackageSHA256, record.TrustPolicy, record.AdmissionJSON, record.ImportedBy, record.CreatedAt)
	if err != nil {
		return RepositoryImportRecord{}, err
	}
	id, err := lastInsertID(ctx, r.db)
	if err != nil {
		return RepositoryImportRecord{}, err
	}
	return r.RepositoryImport(ctx, id)
}

func (r Repository) RepositoryImport(ctx context.Context, id int64) (RepositoryImportRecord, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT id, repository_type, index_path, repository_name, candidate_id, plugin_id, version,
       artifact_id, package_sha256, trust_policy, admission_json, imported_by, created_at
FROM plugin_repository_imports WHERE id = ?`, id)
	return scanRepositoryImport(row)
}

func (r Repository) ListRepositoryImports(ctx context.Context) ([]RepositoryImportRecord, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT id, repository_type, index_path, repository_name, candidate_id, plugin_id, version,
       artifact_id, package_sha256, trust_policy, admission_json, imported_by, created_at
FROM plugin_repository_imports ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var imports []RepositoryImportRecord
	for rows.Next() {
		record, err := scanRepositoryImport(rows)
		if err != nil {
			return nil, err
		}
		imports = append(imports, record)
	}
	return imports, rows.Err()
}

func (r Repository) SaveRepositoryIndexSync(ctx context.Context, record RepositoryIndexSyncRecord) (RepositoryIndexSyncRecord, error) {
	now := r.now().Unix()
	if record.CreatedAt == 0 {
		record.CreatedAt = now
	}
	if record.Status == "" {
		record.Status = RepositorySyncStatusSucceeded
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO plugin_repository_index_syncs(
    repository_type, index_path, repository_name, status, cache_key, candidate_count, error, synced_by, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.RepositoryType, record.IndexPath, record.RepositoryName, record.Status, record.CacheKey,
		record.CandidateCount, record.Error, record.SyncedBy, record.CreatedAt)
	if err != nil {
		return RepositoryIndexSyncRecord{}, err
	}
	id, err := lastInsertID(ctx, r.db)
	if err != nil {
		return RepositoryIndexSyncRecord{}, err
	}
	return r.RepositoryIndexSync(ctx, id)
}

func (r Repository) RepositoryIndexSync(ctx context.Context, id int64) (RepositoryIndexSyncRecord, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT id, repository_type, index_path, repository_name, status, cache_key, candidate_count, error, synced_by, created_at
FROM plugin_repository_index_syncs WHERE id = ?`, id)
	return scanRepositoryIndexSync(row)
}

func (r Repository) LatestRepositoryIndexSync(ctx context.Context, repositoryType, indexPath string) (RepositoryIndexSyncRecord, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT id, repository_type, index_path, repository_name, status, cache_key, candidate_count, error, synced_by, created_at
FROM plugin_repository_index_syncs
WHERE repository_type = ? AND index_path = ? AND status IN (?, ?)
ORDER BY created_at DESC, id DESC
LIMIT 1`, repositoryType, indexPath, RepositorySyncStatusSucceeded, RepositorySyncStatusDegraded)
	return scanRepositoryIndexSync(row)
}

func (r Repository) ListRepositoryIndexSyncs(ctx context.Context, repositoryType, indexPath string, limit int) ([]RepositoryIndexSyncRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `
SELECT id, repository_type, index_path, repository_name, status, cache_key, candidate_count, error, synced_by, created_at
FROM plugin_repository_index_syncs`
	var args []any
	var clauses []string
	if repositoryType != "" {
		clauses = append(clauses, "repository_type = ?")
		args = append(args, repositoryType)
	}
	if indexPath != "" {
		clauses = append(clauses, "index_path = ?")
		args = append(args, indexPath)
	}
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []RepositoryIndexSyncRecord
	for rows.Next() {
		record, err := scanRepositoryIndexSync(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (r Repository) SaveSupplyChainAssessment(ctx context.Context, assessment SupplyChainAssessment) (SupplyChainAssessment, error) {
	now := r.now().Unix()
	if assessment.CreatedAt == 0 {
		assessment.CreatedAt = now
	}
	issues, err := json.Marshal(assessment.Issues)
	if err != nil {
		return SupplyChainAssessment{}, err
	}
	signature, err := marshalDefaultObject(assessment.Signature)
	if err != nil {
		return SupplyChainAssessment{}, err
	}
	sbom, err := marshalDefaultObject(assessment.SBOM)
	if err != nil {
		return SupplyChainAssessment{}, err
	}
	license, err := marshalDefaultObject(assessment.License)
	if err != nil {
		return SupplyChainAssessment{}, err
	}
	advisory, err := marshalDefaultObject(assessment.Advisory)
	if err != nil {
		return SupplyChainAssessment{}, err
	}
	metadata, err := marshalDefaultObject(assessment.Metadata)
	if err != nil {
		return SupplyChainAssessment{}, err
	}
	if assessment.Status == "" {
		assessment.Status = SupplyChainStatusAllowed
	}
	_, err = r.db.ExecContext(ctx, `
INSERT INTO plugin_supply_chain_assessments(
    plugin_id, artifact_id, status, issues_json, signature_json, sbom_json,
    license_json, advisory_json, metadata_json, created_by, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		assessment.PluginID, assessment.ArtifactID, assessment.Status, string(issues), signature, sbom,
		license, advisory, metadata, assessment.CreatedBy, assessment.CreatedAt)
	if err != nil {
		return SupplyChainAssessment{}, err
	}
	id, err := lastInsertID(ctx, r.db)
	if err != nil {
		return SupplyChainAssessment{}, err
	}
	return r.SupplyChainAssessment(ctx, id)
}

func (r Repository) SupplyChainAssessment(ctx context.Context, id int64) (SupplyChainAssessment, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT id, plugin_id, artifact_id, status, issues_json, signature_json, sbom_json,
       license_json, advisory_json, metadata_json, created_by, created_at
FROM plugin_supply_chain_assessments WHERE id = ?`, id)
	return scanSupplyChainAssessment(row)
}

func (r Repository) ListSupplyChainAssessments(ctx context.Context, pluginID, artifactID string) ([]SupplyChainAssessment, error) {
	query := `
SELECT id, plugin_id, artifact_id, status, issues_json, signature_json, sbom_json,
       license_json, advisory_json, metadata_json, created_by, created_at
FROM plugin_supply_chain_assessments`
	var args []any
	var clauses []string
	if pluginID != "" {
		clauses = append(clauses, "plugin_id = ?")
		args = append(args, pluginID)
	}
	if artifactID != "" {
		clauses = append(clauses, "artifact_id = ?")
		args = append(args, artifactID)
	}
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += ` ORDER BY created_at DESC, id DESC`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var assessments []SupplyChainAssessment
	for rows.Next() {
		assessment, err := scanSupplyChainAssessment(rows)
		if err != nil {
			return nil, err
		}
		assessments = append(assessments, assessment)
	}
	return assessments, rows.Err()
}

func (r Repository) SaveTrustRoot(ctx context.Context, actor string, record TrustRootRecord) (TrustRootRecord, error) {
	now := r.now().Unix()
	if record.Algorithm == "" {
		record.Algorithm = SignatureAlgorithmEd25519
	}
	if record.Status == "" {
		record.Status = TrustRootStatusTrusted
	}
	if record.PolicyJSON == "" || !json.Valid([]byte(record.PolicyJSON)) {
		record.PolicyJSON = "{}"
	}
	if record.RotatedAt == 0 {
		record.RotatedAt = now
	}
	record.UpdatedAt = now
	record.CreatedBy = actor
	_, err := r.db.ExecContext(ctx, `
INSERT INTO plugin_trust_roots(
    root_id, key_id, algorithm, public_key, public_key_sha256, status, policy_json,
    created_by, rotated_at, revoked_at, revocation_reason, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(root_id, key_id) DO UPDATE SET
    algorithm = excluded.algorithm,
    public_key = excluded.public_key,
    public_key_sha256 = excluded.public_key_sha256,
    status = excluded.status,
    policy_json = excluded.policy_json,
    created_by = excluded.created_by,
    rotated_at = excluded.rotated_at,
    revoked_at = CASE WHEN plugin_trust_roots.status = 'revoked' THEN plugin_trust_roots.revoked_at ELSE excluded.revoked_at END,
    revocation_reason = CASE WHEN plugin_trust_roots.status = 'revoked' THEN plugin_trust_roots.revocation_reason ELSE excluded.revocation_reason END,
    updated_at = excluded.updated_at`,
		record.RootID, record.KeyID, record.Algorithm, record.PublicKey, record.PublicKeySHA256, record.Status,
		record.PolicyJSON, record.CreatedBy, record.RotatedAt, record.RevokedAt, record.RevocationReason, record.UpdatedAt)
	if err != nil {
		return TrustRootRecord{}, err
	}
	return r.TrustRoot(ctx, record.RootID, record.KeyID)
}

func (r Repository) RevokeTrustRoot(ctx context.Context, actor, rootID, keyID, reason string) (TrustRootRecord, error) {
	now := r.now().Unix()
	res, err := r.db.ExecContext(ctx, `
UPDATE plugin_trust_roots
SET status = 'revoked', revoked_at = ?, revocation_reason = ?, created_by = ?, updated_at = ?
WHERE root_id = ? AND key_id = ?`,
		now, reason, actor, now, rootID, keyID)
	if err != nil {
		return TrustRootRecord{}, err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return TrustRootRecord{}, sql.ErrNoRows
	}
	return r.TrustRoot(ctx, rootID, keyID)
}

func (r Repository) TrustRoot(ctx context.Context, rootID, keyID string) (TrustRootRecord, error) {
	query := `
SELECT id, root_id, key_id, algorithm, public_key, public_key_sha256, status, policy_json,
       created_by, rotated_at, revoked_at, revocation_reason, updated_at
FROM plugin_trust_roots
WHERE root_id = ?`
	args := []any{rootID}
	if keyID != "" {
		query += ` AND key_id = ?`
		args = append(args, keyID)
	}
	query += ` ORDER BY status = 'trusted' DESC, rotated_at DESC, id DESC LIMIT 1`
	row := r.db.QueryRowContext(ctx, query, args...)
	return scanTrustRoot(row)
}

func (r Repository) ListTrustRoots(ctx context.Context, rootID string) ([]TrustRootRecord, error) {
	query := `
SELECT id, root_id, key_id, algorithm, public_key, public_key_sha256, status, policy_json,
       created_by, rotated_at, revoked_at, revocation_reason, updated_at
FROM plugin_trust_roots`
	var args []any
	if rootID != "" {
		query += ` WHERE root_id = ?`
		args = append(args, rootID)
	}
	query += ` ORDER BY root_id ASC, status = 'trusted' DESC, rotated_at DESC, id DESC`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []TrustRootRecord
	for rows.Next() {
		record, err := scanTrustRoot(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (r Repository) SavePreflight(ctx context.Context, record PreflightRecord) (PreflightRecord, error) {
	now := r.now().Unix()
	if record.CreatedAt == 0 {
		record.CreatedAt = now
	}
	if record.Profile == "" {
		record.Profile = PolicyProfileDev
	}
	if record.ResultJSON == "" || !json.Valid([]byte(record.ResultJSON)) {
		record.ResultJSON = "{}"
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO plugin_preflight_results(plugin_id, artifact_id, profile, status, result_json, created_by, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		record.PluginID, record.ArtifactID, record.Profile, record.Status, record.ResultJSON, record.CreatedBy, record.CreatedAt)
	if err != nil {
		return PreflightRecord{}, err
	}
	id, err := lastInsertID(ctx, r.db)
	if err != nil {
		return PreflightRecord{}, err
	}
	record.ID = id
	return record, nil
}

func (r Repository) ListPreflights(ctx context.Context, pluginID string) ([]PreflightRecord, error) {
	query := `
SELECT id, plugin_id, artifact_id, profile, status, result_json, created_by, created_at
FROM plugin_preflight_results`
	var args []any
	if pluginID != "" {
		query += ` WHERE plugin_id = ?`
		args = append(args, pluginID)
	}
	query += ` ORDER BY created_at DESC, id DESC`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []PreflightRecord
	for rows.Next() {
		record, err := scanPreflight(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (r Repository) SaveBenchmark(ctx context.Context, actor string, req BenchmarkRequest) (BenchmarkRecord, error) {
	now := r.now().Unix()
	if req.Profile == "" {
		req.Profile = PolicyProfileDev
	}
	record := BenchmarkRecord{
		PluginID:            "",
		ArtifactID:          req.ArtifactID,
		Profile:             req.Profile,
		BenchmarkProfile:    req.BenchmarkProfile,
		P95MS:               req.P95MS,
		P99MS:               req.P99MS,
		ErrorRate:           req.ErrorRate,
		ActiveProxyCapacity: req.ActiveProxyCapacity,
		BaselineDiff:        req.BaselineDiff,
		CreatedBy:           actor,
		CreatedAt:           now,
	}
	artifact, err := r.Artifact(ctx, req.ArtifactID)
	if err != nil {
		return BenchmarkRecord{}, err
	}
	record.PluginID = artifact.PluginID
	_, err = r.db.ExecContext(ctx, `
INSERT INTO plugin_benchmarks(
    plugin_id, artifact_id, profile, benchmark_profile, p95_ms, p99_ms, error_rate,
    active_proxy_capacity, baseline_diff, created_by, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.PluginID, record.ArtifactID, record.Profile, record.BenchmarkProfile, record.P95MS, record.P99MS, record.ErrorRate,
		record.ActiveProxyCapacity, record.BaselineDiff, record.CreatedBy, record.CreatedAt)
	if err != nil {
		return BenchmarkRecord{}, err
	}
	id, err := lastInsertID(ctx, r.db)
	if err != nil {
		return BenchmarkRecord{}, err
	}
	record.ID = id
	return record, nil
}

func (r Repository) ListBenchmarks(ctx context.Context, pluginID string) ([]BenchmarkRecord, error) {
	query := `
SELECT id, plugin_id, artifact_id, profile, benchmark_profile, p95_ms, p99_ms, error_rate,
       active_proxy_capacity, baseline_diff, created_by, created_at
FROM plugin_benchmarks`
	var args []any
	if pluginID != "" {
		query += ` WHERE plugin_id = ?`
		args = append(args, pluginID)
	}
	query += ` ORDER BY created_at DESC, id DESC`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []BenchmarkRecord
	for rows.Next() {
		record, err := scanBenchmark(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (r Repository) LatestBenchmark(ctx context.Context, pluginID, artifactID, profile string) (BenchmarkRecord, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT id, plugin_id, artifact_id, profile, benchmark_profile, p95_ms, p99_ms, error_rate,
       active_proxy_capacity, baseline_diff, created_by, created_at
FROM plugin_benchmarks
WHERE plugin_id = ? AND artifact_id = ? AND profile = ?
ORDER BY created_at DESC, id DESC
LIMIT 1`, pluginID, artifactID, profile)
	benchmark, err := scanBenchmark(row)
	if errors.Is(err, sql.ErrNoRows) {
		return BenchmarkRecord{}, nil
	}
	return benchmark, err
}

func (r Repository) SaveInstrumentation(ctx context.Context, actor string, req InstrumentationRequest) (InstrumentationRecord, error) {
	now := r.now().Unix()
	if req.Status == "" {
		req.Status = InstrumentationStatusAvailable
	}
	provenance, err := marshalDefaultObject(req.Provenance)
	if err != nil {
		return InstrumentationRecord{}, err
	}
	conformance, err := marshalDefaultObject(req.Conformance)
	if err != nil {
		return InstrumentationRecord{}, err
	}
	benchmark, err := marshalDefaultObject(req.Benchmark)
	if err != nil {
		return InstrumentationRecord{}, err
	}
	smoke, err := marshalDefaultObject(req.Smoke)
	if err != nil {
		return InstrumentationRecord{}, err
	}
	_, err = r.db.ExecContext(ctx, `
INSERT INTO plugin_instrumentation(
    name, version, profile, generated_diff_hash, gateway_binary_sha256, ci_artifact_sha256,
    provenance_json, conformance_json, benchmark_json, smoke_json, runbook_rollback, status,
    created_by, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.Name, req.Version, req.Profile, req.GeneratedDiffHash, req.GatewayBinarySHA256, req.CIArtifactSHA256,
		provenance, conformance, benchmark, smoke, req.RunbookRollback, req.Status, actor, now)
	if err != nil {
		return InstrumentationRecord{}, err
	}
	id, err := lastInsertID(ctx, r.db)
	if err != nil {
		return InstrumentationRecord{}, err
	}
	return r.Instrumentation(ctx, id)
}

func (r Repository) Instrumentation(ctx context.Context, id int64) (InstrumentationRecord, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT id, name, version, profile, generated_diff_hash, gateway_binary_sha256,
       ci_artifact_sha256, provenance_json, conformance_json, benchmark_json,
       smoke_json, runbook_rollback, status, created_by, created_at
FROM plugin_instrumentation WHERE id = ?`, id)
	return scanInstrumentation(row)
}

func (r Repository) ListInstrumentation(ctx context.Context) ([]InstrumentationRecord, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT id, name, version, profile, generated_diff_hash, gateway_binary_sha256,
       ci_artifact_sha256, provenance_json, conformance_json, benchmark_json,
       smoke_json, runbook_rollback, status, created_by, created_at
FROM plugin_instrumentation ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []InstrumentationRecord
	for rows.Next() {
		record, err := scanInstrumentation(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (r Repository) RecordOperation(ctx context.Context, pluginID, artifactID, operation, status, actor, message string, metadata any) error {
	metadataJSON, err := marshalDefaultObject(diagnosticSafeValue(metadata))
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `
INSERT INTO plugin_operations(plugin_id, artifact_id, operation, status, actor, message, metadata_json, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		pluginID, artifactID, operation, status, actor, redactSensitive(message), metadataJSON, r.now().Unix())
	return err
}

func (r Repository) ListOperations(ctx context.Context, pluginID string, limit int) ([]OperationRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `
SELECT id, plugin_id, artifact_id, operation, status, actor, message, metadata_json, created_at
FROM plugin_operations`
	var args []any
	if pluginID != "" {
		query += ` WHERE plugin_id = ?`
		args = append(args, pluginID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []OperationRecord
	for rows.Next() {
		var record OperationRecord
		if err := rows.Scan(&record.ID, &record.PluginID, &record.ArtifactID, &record.Operation, &record.Status, &record.Actor, &record.Message, &record.MetadataJSON, &record.CreatedAt); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (r Repository) SaveEvent(ctx context.Context, event EventSummary, dropped bool, reason, traceID, connectionID string) error {
	fields, err := marshalDefaultObject(event.Fields)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `
INSERT INTO plugin_events(plugin_id, name, fields_json, dropped, reason, trace_id, connection_id, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		event.PluginID, event.Name, fields, boolInt(dropped), reason, traceID, connectionID, r.now().Unix())
	return err
}

func (r Repository) RecentEvents(ctx context.Context, pluginID string, limit int) ([]EventSummary, error) {
	if limit <= 0 {
		limit = DefaultEventRecentLimit
	}
	query := `SELECT plugin_id, name, fields_json, dropped, reason, created_at FROM plugin_events`
	var args []any
	if pluginID != "" {
		query += ` WHERE plugin_id = ?`
		args = append(args, pluginID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []EventSummary
	for rows.Next() {
		var event EventSummary
		var fieldsJSON string
		var dropped int
		var reason string
		if err := rows.Scan(&event.PluginID, &event.Name, &fieldsJSON, &dropped, &reason, &event.LastSeenAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(defaultJSONObject(fieldsJSON)), &event.Fields)
		if dropped != 0 {
			event.Dropped = 1
			if reason != "" {
				if event.Fields == nil {
					event.Fields = make(map[string]string)
				}
				event.Fields["drop_reason"] = reason
			}
		} else {
			event.Count = 1
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (r Repository) SaveSubscriberDeadLetter(ctx context.Context, record SubscriberDeadLetterRecord) (int64, error) {
	fields, err := marshalDefaultObject(record.Fields)
	if err != nil {
		return 0, err
	}
	now := r.now().Unix()
	if record.CreatedAt == 0 {
		record.CreatedAt = now
	}
	if record.UpdatedAt == 0 {
		record.UpdatedAt = record.CreatedAt
	}
	if record.Status == "" {
		record.Status = "pending"
	}
	result, err := r.db.ExecContext(ctx, `
INSERT INTO plugin_subscriber_dead_letters(
    subscriber_plugin_id, subscriber_artifact_id, event_plugin_id, event_name, fields_json,
    trace_id, connection_id, delivery_mode, attempts, node_id, status, reason, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.SubscriberPluginID, record.SubscriberArtifactID, record.EventPluginID, record.EventName, fields,
		record.TraceID, record.ConnectionID, record.DeliveryMode, record.Attempts, record.NodeID, record.Status, record.Reason,
		record.CreatedAt, record.UpdatedAt)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (r Repository) PendingSubscriberDeadLetters(ctx context.Context, limit int) ([]SubscriberDeadLetterRecord, error) {
	if limit <= 0 {
		limit = DefaultSubscriberDeadLetterLimit
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT id, subscriber_plugin_id, subscriber_artifact_id, event_plugin_id, event_name, fields_json,
       trace_id, connection_id, delivery_mode, attempts, node_id, status, reason, created_at, updated_at
FROM plugin_subscriber_dead_letters
WHERE status = 'pending'
ORDER BY created_at ASC, id ASC
LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []SubscriberDeadLetterRecord
	for rows.Next() {
		record, err := scanSubscriberDeadLetter(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (r Repository) CountPendingSubscriberDeadLetters(ctx context.Context) (uint64, error) {
	row := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_subscriber_dead_letters WHERE status = 'pending'`)
	var count uint64
	if err := row.Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (r Repository) MarkSubscriberDeadLetter(ctx context.Context, id int64, status, reason string) error {
	if id <= 0 {
		return errors.New("dead letter id is required")
	}
	switch status {
	case "pending", "replayed", "dropped":
	default:
		return fmt.Errorf("invalid subscriber dead letter status %q", status)
	}
	_, err := r.db.ExecContext(ctx, `
UPDATE plugin_subscriber_dead_letters
SET status = ?, reason = ?, updated_at = ?
WHERE id = ?`,
		status, reason, r.now().Unix(), id)
	return err
}

func scanSubscriberDeadLetter(row rowScanner) (SubscriberDeadLetterRecord, error) {
	var record SubscriberDeadLetterRecord
	var fieldsJSON string
	err := row.Scan(
		&record.ID, &record.SubscriberPluginID, &record.SubscriberArtifactID, &record.EventPluginID, &record.EventName,
		&fieldsJSON, &record.TraceID, &record.ConnectionID, &record.DeliveryMode, &record.Attempts, &record.NodeID,
		&record.Status, &record.Reason, &record.CreatedAt, &record.UpdatedAt,
	)
	if err != nil {
		return SubscriberDeadLetterRecord{}, err
	}
	_ = json.Unmarshal([]byte(defaultJSONObject(fieldsJSON)), &record.Fields)
	return record, nil
}

func (r Repository) EventRetentionOverflow(ctx context.Context, pluginID string, keep int) (GCCandidate, bool, error) {
	return r.recentTableOverflow(ctx, "plugin_events", pluginID, keep, "event", "event", "plugin event recent retention exceeded")
}

func (r Repository) DeleteEventRetentionOverflow(ctx context.Context, pluginID string, keep int) (int64, error) {
	return r.deleteRecentTableOverflow(ctx, "plugin_events", pluginID, keep)
}

func (r Repository) SaveLog(ctx context.Context, log LogSummary) error {
	fields, err := marshalDefaultObject(log.Fields)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `
INSERT INTO plugin_logs(plugin_id, level, message, fields_json, trace_id, connection_id, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		log.PluginID, log.Level, log.Message, fields, log.TraceID, log.ConnectionID, r.now().Unix())
	return err
}

func (r Repository) RecentLogs(ctx context.Context, pluginID string, limit int) ([]LogSummary, error) {
	if limit <= 0 {
		limit = DefaultLogRecentLimit
	}
	query := `SELECT plugin_id, level, message, fields_json, trace_id, connection_id, created_at FROM plugin_logs`
	var args []any
	if pluginID != "" {
		query += ` WHERE plugin_id = ?`
		args = append(args, pluginID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var logs []LogSummary
	for rows.Next() {
		var item LogSummary
		var fieldsJSON string
		if err := rows.Scan(&item.PluginID, &item.Level, &item.Message, &fieldsJSON, &item.TraceID, &item.ConnectionID, &item.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(defaultJSONObject(fieldsJSON)), &item.Fields)
		logs = append(logs, item)
	}
	return logs, rows.Err()
}

func (r Repository) LogRetentionOverflow(ctx context.Context, pluginID string, keep int) (GCCandidate, bool, error) {
	return r.recentTableOverflow(ctx, "plugin_logs", pluginID, keep, "plugin_log", "log", "log summary exceeds recent retention")
}

func (r Repository) DeleteLogRetentionOverflow(ctx context.Context, pluginID string, keep int) (int64, error) {
	return r.deleteRecentTableOverflow(ctx, "plugin_logs", pluginID, keep)
}

func (r Repository) SaveTrace(ctx context.Context, trace TraceSummary, fields map[string]string) error {
	fieldsJSON, err := marshalDefaultObject(fields)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `
INSERT INTO plugin_traces(plugin_id, trace_id, connection_id, handler_id, operation, status, duration_ms, fields_json, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		trace.PluginID, trace.TraceID, trace.ConnectionID, trace.HandlerID, trace.Operation, trace.Status, trace.DurationMS, fieldsJSON, r.now().Unix())
	return err
}

func (r Repository) RecentTraces(ctx context.Context, pluginID string, limit int) ([]TraceSummary, error) {
	if limit <= 0 {
		limit = 200
	}
	query := `SELECT plugin_id, trace_id, connection_id, handler_id, operation, status, duration_ms, created_at FROM plugin_traces`
	var args []any
	if pluginID != "" {
		query += ` WHERE plugin_id = ?`
		args = append(args, pluginID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var traces []TraceSummary
	for rows.Next() {
		var trace TraceSummary
		if err := rows.Scan(&trace.PluginID, &trace.TraceID, &trace.ConnectionID, &trace.HandlerID, &trace.Operation, &trace.Status, &trace.DurationMS, &trace.CreatedAt); err != nil {
			return nil, err
		}
		traces = append(traces, trace)
	}
	return traces, rows.Err()
}

func (r Repository) TraceRetentionOverflow(ctx context.Context, pluginID string, keep int) (GCCandidate, bool, error) {
	return r.recentTableOverflow(ctx, "plugin_traces", pluginID, keep, "trace", "trace", "trace summary recent retention exceeded")
}

func (r Repository) DeleteTraceRetentionOverflow(ctx context.Context, pluginID string, keep int) (int64, error) {
	return r.deleteRecentTableOverflow(ctx, "plugin_traces", pluginID, keep)
}

func (r Repository) recentTableOverflow(ctx context.Context, table, pluginID string, keep int, kind, category, reason string) (GCCandidate, bool, error) {
	if keep <= 0 {
		keep = 1
	}
	if table != "plugin_events" && table != "plugin_logs" && table != "plugin_traces" {
		return GCCandidate{}, false, fmt.Errorf("unsupported retention table %q", table)
	}
	query := fmt.Sprintf(`
SELECT COUNT(*), COALESCE(SUM(size_bytes), 0), COALESCE(MIN(created_at), 0)
FROM (
  SELECT id, created_at, LENGTH(plugin_id) + LENGTH(%s) AS size_bytes
  FROM %s
  WHERE plugin_id = ? AND id NOT IN (
    SELECT id FROM %s WHERE plugin_id = ? ORDER BY created_at DESC, id DESC LIMIT ?
  )
)`, recentTableSizeExpression(table), table, table)
	var count, size, oldest int64
	if err := r.db.QueryRowContext(ctx, query, pluginID, pluginID, keep).Scan(&count, &size, &oldest); err != nil {
		return GCCandidate{}, false, err
	}
	if count == 0 {
		return GCCandidate{}, false, nil
	}
	return GCCandidate{
		Kind:             kind,
		Category:         category,
		ID:               fmt.Sprintf("%s/overflow:%d", pluginID, keep),
		PluginID:         pluginID,
		Protected:        false,
		Reason:           reason,
		RetentionRule:    fmt.Sprintf("keep latest %d %s records per plugin", keep, category),
		RetentionSeconds: 0,
		SizeBytes:        size,
		CreatedAt:        oldest,
	}, true, nil
}

func (r Repository) deleteRecentTableOverflow(ctx context.Context, table, pluginID string, keep int) (int64, error) {
	if keep <= 0 {
		keep = 1
	}
	if table != "plugin_events" && table != "plugin_logs" && table != "plugin_traces" {
		return 0, fmt.Errorf("unsupported retention table %q", table)
	}
	result, err := r.db.ExecContext(ctx, fmt.Sprintf(`
DELETE FROM %s
WHERE plugin_id = ? AND id NOT IN (
  SELECT id FROM %s WHERE plugin_id = ? ORDER BY created_at DESC, id DESC LIMIT ?
)`, table, table), pluginID, pluginID, keep)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func recentTableSizeExpression(table string) string {
	switch table {
	case "plugin_events":
		return "name) + LENGTH(fields_json) + LENGTH(reason) + LENGTH(trace_id) + LENGTH(connection_id"
	case "plugin_logs":
		return "level) + LENGTH(message) + LENGTH(fields_json) + LENGTH(trace_id) + LENGTH(connection_id"
	case "plugin_traces":
		return "trace_id) + LENGTH(connection_id) + LENGTH(handler_id) + LENGTH(operation) + LENGTH(status) + LENGTH(fields_json"
	default:
		return "''"
	}
}

func (r Repository) AcquireTaskLease(ctx context.Context, pluginID, taskID, shardKey, owner string, ttl time.Duration) (TaskLeaseRecord, bool, error) {
	if pluginID == "" || taskID == "" || owner == "" {
		return TaskLeaseRecord{}, false, errors.New("plugin_id, task_id and owner_node_id are required")
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	now := r.now()
	nowUnix := now.Unix()
	expiresAt := now.Add(ttl).Unix()
	if expiresAt <= nowUnix {
		expiresAt = nowUnix + 1
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return TaskLeaseRecord{}, false, err
	}
	defer tx.Rollback()

	row := tx.QueryRowContext(ctx, `
SELECT plugin_id, task_id, shard_key, owner_node_id, expires_at, acquired_at, updated_at
FROM plugin_task_leases
WHERE plugin_id = ? AND task_id = ? AND shard_key = ?`, pluginID, taskID, shardKey)
	current, err := scanTaskLease(row)
	if errors.Is(err, sql.ErrNoRows) {
		record := TaskLeaseRecord{
			PluginID:    pluginID,
			TaskID:      taskID,
			ShardKey:    shardKey,
			OwnerNodeID: owner,
			ExpiresAt:   expiresAt,
			AcquiredAt:  nowUnix,
			UpdatedAt:   nowUnix,
		}
		result, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO plugin_task_leases(plugin_id, task_id, shard_key, owner_node_id, expires_at, acquired_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
			record.PluginID, record.TaskID, record.ShardKey, record.OwnerNodeID, record.ExpiresAt, record.AcquiredAt, record.UpdatedAt)
		if err != nil {
			return TaskLeaseRecord{}, false, err
		}
		affected, _ := result.RowsAffected()
		if affected == 1 {
			if err := tx.Commit(); err != nil {
				return TaskLeaseRecord{}, false, err
			}
			return record, true, nil
		}
		current, err = scanTaskLease(tx.QueryRowContext(ctx, `
SELECT plugin_id, task_id, shard_key, owner_node_id, expires_at, acquired_at, updated_at
FROM plugin_task_leases
WHERE plugin_id = ? AND task_id = ? AND shard_key = ?`, pluginID, taskID, shardKey))
		if err != nil {
			return TaskLeaseRecord{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return TaskLeaseRecord{}, false, err
		}
		return current, false, nil
	}
	if err != nil {
		return TaskLeaseRecord{}, false, err
	}
	if current.OwnerNodeID != owner && current.ExpiresAt > nowUnix {
		if err := tx.Commit(); err != nil {
			return TaskLeaseRecord{}, false, err
		}
		return current, false, nil
	}

	acquiredAt := current.AcquiredAt
	if current.OwnerNodeID != owner {
		acquiredAt = nowUnix
	}
	record := TaskLeaseRecord{
		PluginID:    pluginID,
		TaskID:      taskID,
		ShardKey:    shardKey,
		OwnerNodeID: owner,
		ExpiresAt:   expiresAt,
		AcquiredAt:  acquiredAt,
		UpdatedAt:   nowUnix,
	}
	result, err := tx.ExecContext(ctx, `
UPDATE plugin_task_leases
SET owner_node_id = ?, expires_at = ?, acquired_at = ?, updated_at = ?
WHERE plugin_id = ? AND task_id = ? AND shard_key = ? AND (owner_node_id = ? OR expires_at <= ?)`,
		record.OwnerNodeID, record.ExpiresAt, record.AcquiredAt, record.UpdatedAt,
		pluginID, taskID, shardKey, owner, nowUnix)
	if err != nil {
		return TaskLeaseRecord{}, false, err
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		current, err = scanTaskLease(tx.QueryRowContext(ctx, `
SELECT plugin_id, task_id, shard_key, owner_node_id, expires_at, acquired_at, updated_at
FROM plugin_task_leases
WHERE plugin_id = ? AND task_id = ? AND shard_key = ?`, pluginID, taskID, shardKey))
		if err != nil {
			return TaskLeaseRecord{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return TaskLeaseRecord{}, false, err
		}
		return current, false, nil
	}
	if err := tx.Commit(); err != nil {
		return TaskLeaseRecord{}, false, err
	}
	return record, true, nil
}

func (r Repository) RenewTaskLease(ctx context.Context, pluginID, taskID, shardKey, owner string, ttl time.Duration) (TaskLeaseRecord, bool, error) {
	if pluginID == "" || taskID == "" || owner == "" {
		return TaskLeaseRecord{}, false, errors.New("plugin_id, task_id and owner_node_id are required")
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	now := r.now()
	nowUnix := now.Unix()
	expiresAt := now.Add(ttl).Unix()
	if expiresAt <= nowUnix {
		expiresAt = nowUnix + 1
	}
	result, err := r.db.ExecContext(ctx, `
UPDATE plugin_task_leases
SET expires_at = ?, updated_at = ?
WHERE plugin_id = ? AND task_id = ? AND shard_key = ? AND owner_node_id = ? AND expires_at > ?`,
		expiresAt, nowUnix, pluginID, taskID, shardKey, owner, nowUnix)
	if err != nil {
		return TaskLeaseRecord{}, false, err
	}
	affected, _ := result.RowsAffected()
	row := r.db.QueryRowContext(ctx, `
SELECT plugin_id, task_id, shard_key, owner_node_id, expires_at, acquired_at, updated_at
FROM plugin_task_leases
WHERE plugin_id = ? AND task_id = ? AND shard_key = ?`, pluginID, taskID, shardKey)
	record, scanErr := scanTaskLease(row)
	if errors.Is(scanErr, sql.ErrNoRows) {
		return TaskLeaseRecord{}, false, nil
	}
	if scanErr != nil {
		return TaskLeaseRecord{}, false, scanErr
	}
	return record, affected == 1, nil
}

func (r Repository) ReleaseTaskLease(ctx context.Context, pluginID, taskID, shardKey, owner string) error {
	if pluginID == "" || taskID == "" || owner == "" {
		return errors.New("plugin_id, task_id and owner_node_id are required")
	}
	_, err := r.db.ExecContext(ctx, `
DELETE FROM plugin_task_leases
WHERE plugin_id = ? AND task_id = ? AND shard_key = ? AND owner_node_id = ?`,
		pluginID, taskID, shardKey, owner)
	return err
}

func (r Repository) ListTaskLeases(ctx context.Context, pluginID string) ([]TaskLeaseRecord, error) {
	query := `
SELECT plugin_id, task_id, shard_key, owner_node_id, expires_at, acquired_at, updated_at
FROM plugin_task_leases`
	var args []any
	if pluginID != "" {
		query += ` WHERE plugin_id = ?`
		args = append(args, pluginID)
	}
	query += ` ORDER BY expires_at ASC, plugin_id ASC, task_id ASC, shard_key ASC`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []TaskLeaseRecord
	for rows.Next() {
		record, err := scanTaskLease(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (r Repository) DeleteTaskLease(ctx context.Context, pluginID, taskID, shardKey string) error {
	_, err := r.db.ExecContext(ctx, `
DELETE FROM plugin_task_leases
WHERE plugin_id = ? AND task_id = ? AND shard_key = ?`, pluginID, taskID, shardKey)
	return err
}

func (r Repository) PutPluginData(ctx context.Context, record PluginDataSummary, value []byte) error {
	now := r.now().Unix()
	_, err := r.db.ExecContext(ctx, `
INSERT INTO plugin_data(plugin_id, key, value, schema_version, data_class, exportable, size_bytes, expires_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(plugin_id, key) DO UPDATE SET
    value = excluded.value,
    schema_version = excluded.schema_version,
    data_class = excluded.data_class,
    exportable = excluded.exportable,
    size_bytes = excluded.size_bytes,
    expires_at = excluded.expires_at,
    updated_at = excluded.updated_at`,
		record.PluginID, record.Key, value, record.SchemaVersion, record.DataClass, boolInt(record.Exportable), int64(len(value)), record.ExpiresAt, now, now)
	return err
}

func (r Repository) GetPluginData(ctx context.Context, pluginID, key string) (PluginDataSummary, []byte, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT plugin_id, key, value, schema_version, data_class, exportable, size_bytes, expires_at, updated_at
FROM plugin_data WHERE plugin_id = ? AND key = ?`, pluginID, key)
	var record PluginDataSummary
	var exportable int
	var value []byte
	err := row.Scan(&record.PluginID, &record.Key, &value, &record.SchemaVersion, &record.DataClass, &exportable, &record.SizeBytes, &record.ExpiresAt, &record.UpdatedAt)
	record.Exportable = exportable != 0
	return record, value, err
}

func (r Repository) DeletePluginData(ctx context.Context, pluginID, key string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM plugin_data WHERE plugin_id = ? AND key = ?`, pluginID, key)
	return err
}

func (r Repository) ListPluginData(ctx context.Context, pluginID string) ([]PluginDataSummary, error) {
	query := `SELECT plugin_id, key, schema_version, data_class, exportable, size_bytes, expires_at, updated_at FROM plugin_data`
	var args []any
	if pluginID != "" {
		query += ` WHERE plugin_id = ?`
		args = append(args, pluginID)
	}
	query += ` ORDER BY updated_at DESC, key ASC`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []PluginDataSummary
	for rows.Next() {
		var record PluginDataSummary
		var exportable int
		if err := rows.Scan(&record.PluginID, &record.Key, &record.SchemaVersion, &record.DataClass, &exportable, &record.SizeBytes, &record.ExpiresAt, &record.UpdatedAt); err != nil {
			return nil, err
		}
		record.Exportable = exportable != 0
		records = append(records, record)
	}
	return records, rows.Err()
}

func (r Repository) PluginDataUsage(ctx context.Context, pluginID string) (int64, error) {
	row := r.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes), 0) FROM plugin_data WHERE plugin_id = ?`, pluginID)
	var total int64
	return total, row.Scan(&total)
}

func (r Repository) UpsertPluginFile(ctx context.Context, record PluginFileSummary, diskPath string) error {
	now := r.now().Unix()
	_, err := r.db.ExecContext(ctx, `
INSERT INTO plugin_files(plugin_id, namespace, path, disk_path, data_class, exportable, readonly, size_bytes, expires_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(plugin_id, namespace, path) DO UPDATE SET
    disk_path = excluded.disk_path,
    data_class = excluded.data_class,
    exportable = excluded.exportable,
    readonly = excluded.readonly,
    size_bytes = excluded.size_bytes,
    expires_at = excluded.expires_at,
    updated_at = excluded.updated_at`,
		record.PluginID, record.Namespace, record.Path, diskPath, record.DataClass, boolInt(record.Readonly), boolInt(record.Readonly), record.SizeBytes, record.ExpiresAt, now, now)
	return err
}

func (r Repository) DeletePluginFile(ctx context.Context, pluginID, namespace, name string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM plugin_files WHERE plugin_id = ? AND namespace = ? AND path = ?`, pluginID, namespace, name)
	return err
}

func (r Repository) ListPluginFiles(ctx context.Context, pluginID string) ([]PluginFileSummary, error) {
	query := `SELECT plugin_id, namespace, path, data_class, size_bytes, expires_at, updated_at, readonly FROM plugin_files`
	var args []any
	if pluginID != "" {
		query += ` WHERE plugin_id = ?`
		args = append(args, pluginID)
	}
	query += ` ORDER BY updated_at DESC, namespace ASC, path ASC`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []PluginFileSummary
	for rows.Next() {
		var record PluginFileSummary
		var readonly int
		if err := rows.Scan(&record.PluginID, &record.Namespace, &record.Path, &record.DataClass, &record.SizeBytes, &record.ExpiresAt, &record.UpdatedAt, &readonly); err != nil {
			return nil, err
		}
		record.Readonly = readonly != 0
		records = append(records, record)
	}
	return records, rows.Err()
}

func (r Repository) PluginFileUsage(ctx context.Context, pluginID string) (int64, error) {
	row := r.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes), 0) FROM plugin_files WHERE plugin_id = ?`, pluginID)
	var total int64
	return total, row.Scan(&total)
}

func (r Repository) SaveDiagnostic(ctx context.Context, pluginID, path string, size int64, sections []string) (DiagnosticPackageSummary, error) {
	sectionsJSON, err := marshalDefaultObject(sections)
	if err != nil {
		return DiagnosticPackageSummary{}, err
	}
	now := r.now().Unix()
	result, err := r.db.ExecContext(ctx, `
INSERT INTO plugin_diagnostics(plugin_id, path, size_bytes, sections_json, created_at)
VALUES (?, ?, ?, ?, ?)`, pluginID, path, size, sectionsJSON, now)
	if err != nil {
		return DiagnosticPackageSummary{}, err
	}
	_, _ = result.LastInsertId()
	return DiagnosticPackageSummary{PluginID: pluginID, CreatedAt: now, SizeBytes: size, Sections: sections}, nil
}

type diagnosticPackageRecord struct {
	ID        int64
	PluginID  string
	Path      string
	SizeBytes int64
	Sections  []string
	CreatedAt int64
}

func (r Repository) ListDiagnosticRecords(ctx context.Context, pluginID string) ([]diagnosticPackageRecord, error) {
	query := `SELECT id, plugin_id, path, size_bytes, sections_json, created_at FROM plugin_diagnostics`
	var args []any
	if pluginID != "" {
		query += ` WHERE plugin_id = ?`
		args = append(args, pluginID)
	}
	query += ` ORDER BY created_at DESC, id DESC`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []diagnosticPackageRecord
	for rows.Next() {
		var record diagnosticPackageRecord
		var sectionsJSON string
		if err := rows.Scan(&record.ID, &record.PluginID, &record.Path, &record.SizeBytes, &sectionsJSON, &record.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(defaultJSONArray(sectionsJSON)), &record.Sections)
		records = append(records, record)
	}
	return records, rows.Err()
}

func (r Repository) DeleteDiagnostic(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM plugin_diagnostics WHERE id = ?`, id)
	return err
}

func (r Repository) ListDiagnostics(ctx context.Context, pluginID string, limit int) ([]DiagnosticPackageSummary, error) {
	if limit <= 0 {
		limit = 20
	}
	query := `SELECT plugin_id, size_bytes, sections_json, created_at FROM plugin_diagnostics`
	var args []any
	if pluginID != "" {
		query += ` WHERE plugin_id = ?`
		args = append(args, pluginID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []DiagnosticPackageSummary
	for rows.Next() {
		var record DiagnosticPackageSummary
		var sectionsJSON string
		if err := rows.Scan(&record.PluginID, &record.SizeBytes, &sectionsJSON, &record.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(defaultJSONArray(sectionsJSON)), &record.Sections)
		records = append(records, record)
	}
	return records, rows.Err()
}

func (r Repository) DispatchPlan(ctx context.Context) (DispatchPlan, error) {
	plugins, err := r.ListPlugins(ctx)
	if err != nil {
		return DispatchPlan{}, err
	}
	plan := DispatchPlan{UpdatedAt: r.now().Unix()}
	for _, plugin := range plugins {
		if plugin.DispatchSummaryJSON == "" || plugin.RuntimeState != RuntimeEnabled {
			continue
		}
		var summaries []DispatchHandlerSummary
		if err := json.Unmarshal([]byte(plugin.DispatchSummaryJSON), &summaries); err == nil {
			plan.Handlers = append(plan.Handlers, summaries...)
		}
	}
	return plan, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanPluginServiceState(row rowScanner) (PluginServiceState, error) {
	var state PluginServiceState
	err := row.Scan(
		&state.DesiredMode, &state.ActiveMode, &state.AppliedAt, &state.LiveMigration,
		&state.CrashPolicy.BackoffSeconds, &state.CrashPolicy.MaxCrashes, &state.CrashPolicy.WindowSeconds,
		&state.LastError, &state.UpdatedBy, &state.UpdatedAt,
	)
	state = normalizePluginServiceState(state)
	return state, err
}

func scanArtifact(row rowScanner) (ArtifactRecord, error) {
	var artifact ArtifactRecord
	err := row.Scan(
		&artifact.ID, &artifact.PluginID, &artifact.Version, &artifact.FileName, &artifact.FilePath, &artifact.SHA256, &artifact.PackageSHA256, &artifact.SizeBytes,
		&artifact.ArtifactType, &artifact.RuntimeType, &artifact.RuntimeEntry, &artifact.Status, &artifact.MetadataJSON,
		&artifact.CapabilitiesSummaryJSON, &artifact.ExtensionPointsJSON, &artifact.APIVersion, &artifact.GoVersion, &artifact.GOOS, &artifact.GOARCH,
		&artifact.UploadedBy, &artifact.Error, &artifact.CreatedAt, &artifact.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ArtifactRecord{}, ErrArtifactNotFound
	}
	return artifact, err
}

func scanPluginRow(row rowScanner, plugin *PluginRecord) error {
	return row.Scan(
		&plugin.ID, &plugin.DesiredArtifactID, &plugin.ActiveArtifactID, &plugin.LoadedArtifactID, &plugin.DesiredState, &plugin.RuntimeState,
		&plugin.Priority, &plugin.ConfigJSON, &plugin.DesiredGeneration, &plugin.AppliedGeneration, &plugin.LastError,
		&plugin.RuntimeSummaryJSON, &plugin.DispatchSummaryJSON, &plugin.CreatedAt, &plugin.UpdatedAt, &plugin.UpdatedBy,
	)
}

func scanConfigSnapshot(row rowScanner) (ConfigSnapshotRecord, error) {
	var snapshot ConfigSnapshotRecord
	err := row.Scan(
		&snapshot.ID, &snapshot.PluginID, &snapshot.ArtifactID, &snapshot.ConfigJSON, &snapshot.DesiredState,
		&snapshot.Priority, &snapshot.DesiredGeneration, &snapshot.CreatedBy, &snapshot.CreatedAt,
	)
	return snapshot, err
}

func scanSecret(row rowScanner) (SecretRecord, error) {
	var secret SecretRecord
	var reloadRequired int
	var hotReload int
	err := row.Scan(
		&secret.PluginID, &secret.Name, &secret.CurrentVersion, &secret.PreviousVersion,
		&reloadRequired, &hotReload, &secret.UpdatedBy, &secret.CreatedAt, &secret.UpdatedAt,
	)
	secret.ReloadRequired = reloadRequired != 0
	secret.HotReload = hotReload != 0
	return secret, err
}

func scanBuild(row rowScanner) (BuildRecord, error) {
	var build BuildRecord
	var vendorRequired int
	err := row.Scan(
		&build.ID, &build.PluginID, &build.SourceID, &build.ArtifactID, &build.Status, &build.BuilderType, &build.BuilderImage, &build.BuilderVersion,
		&build.GoVersion, &build.GOOS, &build.GOARCH, &build.GOAMD64, &build.GOARM64, &build.CGOEnabled, &build.BuildTags,
		&build.SDKModule, &build.SDKVersion, &build.GOPROXY, &build.GONOSUMDB, &build.GOPRIVATE, &vendorRequired,
		&build.SourceSHA256, &build.ArtifactSHA256, &build.ModuleSummary, &build.GoVersionM, &build.ABIFingerprint,
		&build.LogSummary, &build.MetadataJSON, &build.Error, &build.StartedAt, &build.EndedAt, &build.DurationMS, &build.CreatedBy, &build.CreatedAt, &build.UpdatedAt,
	)
	build.VendorRequired = vendorRequired != 0
	return build, err
}

func scanReview(row rowScanner) (ReviewRecord, error) {
	var review ReviewRecord
	err := row.Scan(
		&review.ID, &review.PluginID, &review.ArtifactID, &review.Profile, &review.RiskLevel,
		&review.ConfigHash, &review.ScopeHash, &review.RolloutHash, &review.ArtifactHash, &review.RuntimeLimitsHash,
		&review.FeaturesHash, &review.PolicyHash, &review.Decision, &review.Notes, &review.ReviewedBy, &review.CreatedAt,
	)
	return review, err
}

func scanWarningOverride(row rowScanner) (WarningOverrideRecord, error) {
	var override WarningOverrideRecord
	err := row.Scan(
		&override.ID, &override.PluginID, &override.ArtifactID, &override.Profile, &override.Action,
		&override.PolicyHash, &override.Reason, &override.CreatedBy, &override.ExpiresAt, &override.CreatedAt,
	)
	return override, err
}

func scanAdvisory(row rowScanner) (AdvisoryRecord, error) {
	var advisory AdvisoryRecord
	err := row.Scan(
		&advisory.ID, &advisory.AdvisoryID, &advisory.Status, &advisory.Action, &advisory.ArtifactSHA256,
		&advisory.PluginID, &advisory.VersionRange, &advisory.DependencyName, &advisory.DependencyRange,
		&advisory.RecommendedAction, &advisory.FixedVersion, &advisory.Mitigation, &advisory.CreatedBy,
		&advisory.CreatedAt, &advisory.UpdatedAt,
	)
	return advisory, err
}

func scanVulnerability(row rowScanner) (VulnerabilityRecord, error) {
	var record VulnerabilityRecord
	var referencesJSON string
	err := row.Scan(
		&record.ID, &record.VulnerabilityID, &record.Source, &record.Status, &record.PackageName,
		&record.VersionRange, &record.Severity, &record.Action, &record.FixedVersion,
		&record.Summary, &referencesJSON, &record.CreatedBy, &record.CreatedAt, &record.UpdatedAt,
	)
	if err != nil {
		return record, err
	}
	_ = json.Unmarshal([]byte(defaultJSONArray(referencesJSON)), &record.References)
	return record, nil
}

func scanPreflight(row rowScanner) (PreflightRecord, error) {
	var record PreflightRecord
	err := row.Scan(
		&record.ID, &record.PluginID, &record.ArtifactID, &record.Profile, &record.Status,
		&record.ResultJSON, &record.CreatedBy, &record.CreatedAt,
	)
	return record, err
}

func scanBenchmark(row rowScanner) (BenchmarkRecord, error) {
	var record BenchmarkRecord
	err := row.Scan(
		&record.ID, &record.PluginID, &record.ArtifactID, &record.Profile, &record.BenchmarkProfile,
		&record.P95MS, &record.P99MS, &record.ErrorRate, &record.ActiveProxyCapacity,
		&record.BaselineDiff, &record.CreatedBy, &record.CreatedAt,
	)
	return record, err
}

func scanTaskLease(row rowScanner) (TaskLeaseRecord, error) {
	var record TaskLeaseRecord
	err := row.Scan(
		&record.PluginID, &record.TaskID, &record.ShardKey, &record.OwnerNodeID,
		&record.ExpiresAt, &record.AcquiredAt, &record.UpdatedAt,
	)
	return record, err
}

func scanRepositoryImport(row rowScanner) (RepositoryImportRecord, error) {
	var record RepositoryImportRecord
	err := row.Scan(
		&record.ID, &record.RepositoryType, &record.IndexPath, &record.RepositoryName,
		&record.CandidateID, &record.PluginID, &record.Version, &record.ArtifactID,
		&record.PackageSHA256, &record.TrustPolicy, &record.AdmissionJSON,
		&record.ImportedBy, &record.CreatedAt,
	)
	return record, err
}

func scanRepositoryIndexSync(row rowScanner) (RepositoryIndexSyncRecord, error) {
	var record RepositoryIndexSyncRecord
	err := row.Scan(
		&record.ID, &record.RepositoryType, &record.IndexPath, &record.RepositoryName,
		&record.Status, &record.CacheKey, &record.CandidateCount, &record.Error,
		&record.SyncedBy, &record.CreatedAt,
	)
	return record, err
}

func scanSupplyChainAssessment(row rowScanner) (SupplyChainAssessment, error) {
	var assessment SupplyChainAssessment
	var issuesJSON, signatureJSON, sbomJSON, licenseJSON, advisoryJSON, metadataJSON string
	err := row.Scan(
		&assessment.ID, &assessment.PluginID, &assessment.ArtifactID, &assessment.Status,
		&issuesJSON, &signatureJSON, &sbomJSON, &licenseJSON, &advisoryJSON,
		&metadataJSON, &assessment.CreatedBy, &assessment.CreatedAt,
	)
	if err != nil {
		return assessment, err
	}
	_ = json.Unmarshal([]byte(defaultJSONArray(issuesJSON)), &assessment.Issues)
	assessment.Signature = jsonMap(signatureJSON)
	assessment.SBOM = jsonMap(sbomJSON)
	assessment.License = jsonMap(licenseJSON)
	assessment.Advisory = jsonMap(advisoryJSON)
	assessment.Metadata = jsonMap(metadataJSON)
	return assessment, nil
}

func scanTrustRoot(row rowScanner) (TrustRootRecord, error) {
	var record TrustRootRecord
	err := row.Scan(
		&record.ID, &record.RootID, &record.KeyID, &record.Algorithm,
		&record.PublicKey, &record.PublicKeySHA256, &record.Status, &record.PolicyJSON,
		&record.CreatedBy, &record.RotatedAt, &record.RevokedAt, &record.RevocationReason,
		&record.UpdatedAt,
	)
	return record, err
}

func scanInstrumentation(row rowScanner) (InstrumentationRecord, error) {
	var record InstrumentationRecord
	err := row.Scan(
		&record.ID, &record.Name, &record.Version, &record.Profile, &record.GeneratedDiffHash,
		&record.GatewayBinarySHA256, &record.CIArtifactSHA256, &record.ProvenanceJSON,
		&record.ConformanceJSON, &record.BenchmarkJSON, &record.SmokeJSON,
		&record.RunbookRollback, &record.Status, &record.CreatedBy, &record.CreatedAt,
	)
	return record, err
}

func marshalDefaultObject(value any) (string, error) {
	if value == nil {
		return "{}", nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	if len(data) == 0 || string(data) == "null" {
		return "{}", nil
	}
	return string(data), nil
}

func defaultJSONObject(value string) string {
	if value == "" || !json.Valid([]byte(value)) {
		return "{}"
	}
	return value
}

func defaultJSONArray(value string) string {
	if value == "" || !json.Valid([]byte(value)) {
		return "[]"
	}
	return value
}

func jsonMap(value string) map[string]any {
	var out map[string]any
	if json.Unmarshal([]byte(defaultJSONObject(value)), &out) != nil {
		return nil
	}
	return out
}

func normalizePluginServiceState(state PluginServiceState) PluginServiceState {
	if state.DesiredMode == "" {
		state.DesiredMode = PluginServiceModeInProcess
	}
	if state.ActiveMode == "" {
		state.ActiveMode = PluginServiceModeInProcess
	}
	if state.LiveMigration == "" {
		state.LiveMigration = PluginMigrationDrainOnly
	}
	state.CrashPolicy = normalizePluginHostCrashPolicy(state.CrashPolicy)
	state.RestartRequired = state.DesiredMode != state.ActiveMode
	return state
}

func normalizePluginHostCrashPolicy(policy PluginHostCrashPolicy) PluginHostCrashPolicy {
	if policy.BackoffSeconds <= 0 {
		policy.BackoffSeconds = DefaultPluginHostCrashBackoffSeconds
	}
	if policy.MaxCrashes <= 0 {
		policy.MaxCrashes = DefaultPluginHostCrashMaxCrashes
	}
	if policy.WindowSeconds <= 0 {
		policy.WindowSeconds = DefaultPluginHostCrashWindowSeconds
	}
	return policy
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func lastInsertID(ctx context.Context, db *sql.DB) (int64, error) {
	row := db.QueryRowContext(ctx, `SELECT last_insert_rowid()`)
	var id int64
	if err := row.Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}
