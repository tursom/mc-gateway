package pluginmanager

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
