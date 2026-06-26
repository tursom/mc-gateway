package pluginmanager

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type Repository struct {
	db  *sql.DB
	now func() time.Time
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

func (r Repository) RecordOperation(ctx context.Context, pluginID, artifactID, operation, status, actor, message string, metadata any) error {
	metadataJSON, err := marshalDefaultObject(metadata)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `
INSERT INTO plugin_operations(plugin_id, artifact_id, operation, status, actor, message, metadata_json, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		pluginID, artifactID, operation, status, actor, message, metadataJSON, r.now().Unix())
	return err
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
