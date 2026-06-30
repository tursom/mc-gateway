// cmd/gateway/admin_plugin_handlers.go 承载插件制品、期望状态、配置、密钥、运维操作和治理检查相关的 Admin API。

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tursom/mc-gateway/internal/adminhttp"
	"github.com/tursom/mc-gateway/internal/pluginmanager"
)

func handleAdminPluginArtifacts(w http.ResponseWriter, r *http.Request) {
	session, ok := requireRole(w, r, adminRoleMember)
	if !ok {
		return
	}
	if pluginsManager == nil {
		adminhttp.WriteAPIError(w, http.StatusServiceUnavailable, "plugin manager is not initialized")
		return
	}

	switch r.Method {
	case http.MethodGet:
		artifacts, err := pluginsManager.ListArtifacts(r.Context(), r.URL.Query().Get("plugin_id"))
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"artifacts": artifacts})
	case http.MethodPost:
		if session.Role != adminRoleAdmin {
			adminhttp.WriteAPIError(w, http.StatusForbidden, "forbidden")
			return
		}
		artifact, err := receivePluginArtifact(r, session.Username)
		if err != nil {
			recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_artifact_upload", "plugin_artifact", "", false, err.Error())
			adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_artifact_upload", "plugin_artifact", artifact.ID, true, "artifact uploaded", map[string]any{
			"plugin_id":      artifact.PluginID,
			"version":        artifact.Version,
			"sha256":         artifact.SHA256,
			"package_sha256": artifact.PackageSHA256,
		})
		adminhttp.WriteJSON(w, http.StatusCreated, map[string]any{"artifact": artifact})
	default:
		adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func handleAdminPluginSources(w http.ResponseWriter, r *http.Request) {
	session, ok := requireRole(w, r, adminRoleMember)
	if !ok {
		return
	}
	if pluginsManager == nil {
		adminhttp.WriteAPIError(w, http.StatusServiceUnavailable, "plugin manager is not initialized")
		return
	}
	switch r.Method {
	case http.MethodGet:
		artifacts, err := pluginsManager.ListArtifacts(r.Context(), r.URL.Query().Get("plugin_id"))
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		var sources []pluginmanager.ArtifactRecord
		for _, artifact := range artifacts {
			if artifact.ArtifactType == pluginmanager.ArtifactTypeSource {
				sources = append(sources, artifact)
			}
		}
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"sources": sources})
	case http.MethodPost:
		if session.Role != adminRoleAdmin {
			adminhttp.WriteAPIError(w, http.StatusForbidden, "forbidden")
			return
		}
		source, err := receivePluginSource(r, session.Username)
		if err != nil {
			recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_source_upload", "plugin_source", "", false, err.Error())
			adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_source_upload", "plugin_source", source.ID, true, "source package uploaded", map[string]any{
			"plugin_id":     source.PluginID,
			"version":       source.Version,
			"source_sha256": source.SHA256,
		})
		adminhttp.WriteJSON(w, http.StatusCreated, map[string]any{"source": source})
	default:
		adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func handleAdminPluginBuilds(w http.ResponseWriter, r *http.Request) {
	session, ok := requireRole(w, r, adminRoleMember)
	if !ok {
		return
	}
	if pluginsManager == nil {
		adminhttp.WriteAPIError(w, http.StatusServiceUnavailable, "plugin manager is not initialized")
		return
	}
	switch r.Method {
	case http.MethodGet:
		builds, err := pluginsManager.ListBuilds(r.Context(), r.URL.Query().Get("plugin_id"))
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"builds": publicPluginBuilds(builds)})
	case http.MethodPost:
		if session.Role != adminRoleAdmin {
			adminhttp.WriteAPIError(w, http.StatusForbidden, "forbidden")
			return
		}
		var req pluginmanager.BuildRequest
		if !adminhttp.DecodeJSONRequest(w, r, &req) {
			return
		}
		build, err := pluginsManager.CreateBuild(r.Context(), session.Username, req)
		if err != nil {
			recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_source_build", "plugin_build", req.SourceID, false, err.Error())
			adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_source_build_queue", "plugin_build", strconv.FormatInt(build.ID, 10), true, "build queued", map[string]any{
			"plugin_id":       build.PluginID,
			"source_sha256":   build.SourceSHA256,
			"artifact_sha256": build.ArtifactSHA256,
			"builder_type":    build.BuilderType,
			"go_version":      build.GoVersion,
		})
		adminhttp.WriteJSON(w, http.StatusCreated, map[string]any{"build": publicPluginBuild(build)})
	default:
		adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func handleAdminPluginBuild(w http.ResponseWriter, r *http.Request, rawSegment string) {
	session, ok := requireRole(w, r, adminRoleMember)
	if !ok {
		return
	}
	if pluginsManager == nil {
		adminhttp.WriteAPIError(w, http.StatusServiceUnavailable, "plugin manager is not initialized")
		return
	}
	parts := strings.Split(rawSegment, "/")
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		adminhttp.WriteAPIError(w, http.StatusBadRequest, "invalid build id")
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		build, err := pluginsManager.Build(r.Context(), id)
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusNotFound, err.Error())
			return
		}
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"build": publicPluginBuild(build)})
		return
	}
	if session.Role != adminRoleAdmin {
		adminhttp.WriteAPIError(w, http.StatusForbidden, "forbidden")
		return
	}
	if len(parts) != 2 || r.Method != http.MethodPost {
		adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	action, err := adminhttp.PathSegment(parts[1])
	if err != nil {
		adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	var build pluginmanager.BuildRecord
	switch action {
	case "run":
		build, err = pluginsManager.RunBuild(r.Context(), session.Username, id)
	case "cancel":
		build, err = pluginsManager.CancelBuild(r.Context(), session.Username, id)
	case "retry":
		build, err = pluginsManager.RetryBuild(r.Context(), session.Username, id)
	default:
		adminhttp.WriteAPIError(w, http.StatusBadRequest, "unknown build action")
		return
	}
	if err != nil {
		recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_build_"+action, "plugin_build", strconv.FormatInt(id, 10), false, err.Error())
		adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_build_"+action, "plugin_build", strconv.FormatInt(id, 10), true, "build "+action+" succeeded")
	adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"build": publicPluginBuild(build)})
}

func handleAdminPluginGC(w http.ResponseWriter, r *http.Request) {
	session, ok := requireRole(w, r, adminRoleMember)
	if !ok {
		return
	}
	if pluginsManager == nil {
		adminhttp.WriteAPIError(w, http.StatusServiceUnavailable, "plugin manager is not initialized")
		return
	}
	dryRun := r.Method == http.MethodGet
	if r.Method == http.MethodPost && session.Role != adminRoleAdmin {
		adminhttp.WriteAPIError(w, http.StatusForbidden, "forbidden")
		return
	}
	candidates, err := pluginsManager.RunGC(r.Context(), session.Username, dryRun)
	if err != nil {
		adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	adminhttp.WriteJSON(w, http.StatusOK, map[string]any{
		"dry_run":    dryRun,
		"candidates": candidates,
	})
}

func handleAdminPluginArtifact(w http.ResponseWriter, r *http.Request, rawArtifactID string) {
	if _, ok := requireRole(w, r, adminRoleMember); !ok {
		return
	}
	if pluginsManager == nil {
		adminhttp.WriteAPIError(w, http.StatusServiceUnavailable, "plugin manager is not initialized")
		return
	}
	parts := strings.Split(rawArtifactID, "/")
	artifactID, err := adminhttp.PathSegment(parts[0])
	if err != nil {
		adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if r.Method != http.MethodGet {
		adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if len(parts) == 2 && parts[1] == "package" {
		artifact, packagePath, err := pluginsManager.ArtifactDistributionPackage(r.Context(), artifactID)
		if err != nil {
			writePluginManagerError(w, err)
			return
		}
		fileName := filepath.Base(artifact.FileName)
		if fileName == "." || fileName == string(filepath.Separator) || strings.TrimSpace(fileName) == "" {
			fileName = artifact.PluginID + "-" + artifact.ID + ".mcgp"
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(fileName, `"`, "")+`"`)
		w.Header().Set("X-Plugin-ID", artifact.PluginID)
		w.Header().Set("X-Plugin-Artifact-ID", artifact.ID)
		w.Header().Set("X-Plugin-Package-SHA256", artifact.PackageSHA256)
		http.ServeFile(w, r, packagePath)
		return
	}
	if len(parts) != 1 {
		adminhttp.WriteAPIError(w, http.StatusNotFound, "plugin artifact route not found")
		return
	}
	artifact, err := pluginsManager.Artifact(r.Context(), artifactID)
	if err != nil {
		writePluginManagerError(w, err)
		return
	}
	adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"artifact": artifact})
}

func handleAdminPluginsList(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireRole(w, r, adminRoleMember); !ok {
		return
	}
	if pluginsManager == nil {
		adminhttp.WriteAPIError(w, http.StatusServiceUnavailable, "plugin manager is not initialized")
		return
	}
	plugins, err := pluginsManager.ListPlugins(r.Context())
	if err != nil {
		adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	views, err := pluginViews(r, plugins)
	if err != nil {
		adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"plugins": views})
}

func handleAdminPluginItem(w http.ResponseWriter, r *http.Request, rawPluginID string) {
	session, ok := requireRole(w, r, adminRoleMember)
	if !ok {
		return
	}
	if pluginsManager == nil {
		adminhttp.WriteAPIError(w, http.StatusServiceUnavailable, "plugin manager is not initialized")
		return
	}
	if strings.HasSuffix(rawPluginID, "/proxy-connections") {
		handleAdminPluginProxyConnections(w, r, strings.TrimSuffix(rawPluginID, "/proxy-connections"))
		return
	}

	pluginID, err := adminhttp.PathSegment(rawPluginID)
	if err != nil {
		adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	switch r.Method {
	case http.MethodGet:
		plugin, err := pluginsManager.Plugin(r.Context(), pluginID)
		if err != nil {
			writePluginManagerError(w, err)
			return
		}
		view, err := pluginView(r, plugin, true)
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"plugin": view})
	case http.MethodPut:
		if session.Role != adminRoleAdmin {
			adminhttp.WriteAPIError(w, http.StatusForbidden, "forbidden")
			return
		}
		var req adminhttp.PluginDesiredRequest
		if !adminhttp.DecodeJSONRequest(w, r, &req) {
			return
		}
		configJSON, err := pluginConfigJSON(req)
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		plugin, err := pluginsManager.SetDesired(r.Context(), session.Username, pluginID, req.ArtifactID, req.DesiredState, configJSON, req.Priority)
		if err != nil {
			recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_desired_update", "plugin", pluginID, false, err.Error())
			writePluginManagerError(w, err)
			return
		}
		recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_desired_update", "plugin", pluginID, true, "desired state updated", map[string]any{
			"artifact_id":        req.ArtifactID,
			"desired_state":      plugin.DesiredState,
			"desired_generation": plugin.DesiredGeneration,
			"priority":           plugin.Priority,
		})
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"plugin": plugin})
	default:
		adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func handleAdminPluginProxyConnections(w http.ResponseWriter, r *http.Request, rawPluginID string) {
	if r.Method != http.MethodGet {
		adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	pluginID, err := adminhttp.PathSegment(rawPluginID)
	if err != nil {
		adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	connections, err := pluginsManager.ActiveProxyConnections(r.Context(), pluginID)
	if err != nil {
		adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"proxy_connections": connections})
}

func handleAdminPluginConfig(w http.ResponseWriter, r *http.Request, rawSegment string) {
	session, ok := requireRole(w, r, adminRoleMember)
	if !ok {
		return
	}
	if pluginsManager == nil {
		adminhttp.WriteAPIError(w, http.StatusServiceUnavailable, "plugin manager is not initialized")
		return
	}
	pluginID, action, ok := splitPluginSubresource(w, rawSegment, "config")
	if !ok {
		return
	}
	switch {
	case r.Method == http.MethodGet && action == "snapshots":
		snapshots, err := pluginsManager.ListConfigSnapshots(r.Context(), pluginID)
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"snapshots": snapshots})
	case r.Method == http.MethodGet && strings.HasPrefix(action, "snapshots/") && strings.HasSuffix(action, "/diff"):
		parts := strings.Split(action, "/")
		if len(parts) != 3 {
			adminhttp.WriteAPIError(w, http.StatusBadRequest, "invalid snapshot diff path")
			return
		}
		snapshotID, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusBadRequest, "invalid snapshot id")
			return
		}
		diff, err := pluginsManager.ConfigSnapshotDiff(r.Context(), snapshotID)
		if err != nil {
			writePluginManagerError(w, err)
			return
		}
		if diff.PluginID != pluginID {
			adminhttp.WriteAPIError(w, http.StatusNotFound, "snapshot not found")
			return
		}
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"diff": diff})
	case r.Method == http.MethodPost && action == "dry-run":
		if session.Role != adminRoleAdmin {
			adminhttp.WriteAPIError(w, http.StatusForbidden, "forbidden")
			return
		}
		var req adminhttp.PluginConfigRequest
		if !adminhttp.DecodeJSONRequest(w, r, &req) {
			return
		}
		configJSON, err := pluginConfigRequestJSON(req)
		if err != nil {
			recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_config_dry_run", "plugin", pluginID, false, err.Error(), map[string]any{
				"artifact_id": req.ArtifactID,
			})
			adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		if req.ArtifactID == "" {
			plugin, err := pluginsManager.Plugin(r.Context(), pluginID)
			if err != nil {
				writePluginManagerError(w, err)
				return
			}
			req.ArtifactID = plugin.DesiredArtifactID
		}
		result, err := pluginsManager.DryRunConfig(r.Context(), pluginID, req.ArtifactID, configJSON)
		if err != nil {
			recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_config_dry_run", "plugin", pluginID, false, err.Error(), map[string]any{
				"artifact_id": req.ArtifactID,
			})
			writePluginManagerError(w, err)
			return
		}
		recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_config_dry_run", "plugin", pluginID, true, "config dry-run succeeded", map[string]any{
			"artifact_id":        req.ArtifactID,
			"restart_required":   result.RestartRequired,
			"sensitive_paths":    result.SensitivePaths,
			"redacted_diff_json": result.RedactedDiffJSON,
		})
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"result": result})
	case r.Method == http.MethodPut && action == "":
		if session.Role != adminRoleAdmin {
			adminhttp.WriteAPIError(w, http.StatusForbidden, "forbidden")
			return
		}
		var req adminhttp.PluginConfigRequest
		if !adminhttp.DecodeJSONRequest(w, r, &req) {
			return
		}
		configJSON, err := pluginConfigRequestJSON(req)
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		current, err := pluginsManager.Plugin(r.Context(), pluginID)
		if err != nil {
			writePluginManagerError(w, err)
			return
		}
		if req.ArtifactID == "" {
			req.ArtifactID = current.DesiredArtifactID
		}
		if req.DesiredState == "" {
			req.DesiredState = current.DesiredState
		}
		if req.Priority == 0 {
			req.Priority = current.Priority
		}
		plugin, err := pluginsManager.SetDesired(r.Context(), session.Username, pluginID, req.ArtifactID, req.DesiredState, configJSON, req.Priority)
		if err != nil {
			recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_config_update", "plugin", pluginID, false, err.Error())
			writePluginManagerError(w, err)
			return
		}
		recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_config_update", "plugin", pluginID, true, "config desired state updated", map[string]any{
			"artifact_id":        plugin.DesiredArtifactID,
			"desired_generation": plugin.DesiredGeneration,
			"desired_state":      plugin.DesiredState,
		})
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"plugin": plugin})
	default:
		adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func handleAdminPluginSecrets(w http.ResponseWriter, r *http.Request, rawSegment string) {
	session, ok := requireRole(w, r, adminRoleMember)
	if !ok {
		return
	}
	if pluginsManager == nil {
		adminhttp.WriteAPIError(w, http.StatusServiceUnavailable, "plugin manager is not initialized")
		return
	}
	pluginID, action, ok := splitPluginSubresource(w, rawSegment, "secrets")
	if !ok {
		return
	}
	switch {
	case r.Method == http.MethodGet && action == "":
		secrets, err := pluginsManager.ListSecrets(r.Context(), pluginID)
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"secrets": secrets})
	case r.Method == http.MethodPost && action == "":
		if session.Role != adminRoleAdmin {
			adminhttp.WriteAPIError(w, http.StatusForbidden, "forbidden")
			return
		}
		var req adminhttp.PluginSecretRequest
		if !adminhttp.DecodeJSONRequest(w, r, &req) {
			return
		}
		secret, err := pluginsManager.UpsertSecret(r.Context(), session.Username, pluginID, req.ArtifactID, req.Name, req.Value, req.ReloadRequired, req.HotReload)
		if err != nil {
			recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_secret_update", "plugin_secret", "plugin://"+pluginID+"/"+req.Name, false, "secret update failed", map[string]any{
				"error": err.Error(),
			})
			adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_secret_update", "plugin_secret", "plugin://"+pluginID+"/"+secret.Name, true, "secret updated", map[string]any{
			"current_version":  secret.CurrentVersion,
			"previous_version": secret.PreviousVersion,
			"reload_required":  secret.ReloadRequired,
			"hot_reload":       secret.HotReload,
		})
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"secret": secret})
	default:
		adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func handleAdminPluginRollback(w http.ResponseWriter, r *http.Request, rawSegment string) {
	session, ok := requireRole(w, r, adminRoleAdmin)
	if !ok {
		return
	}
	if pluginsManager == nil {
		adminhttp.WriteAPIError(w, http.StatusServiceUnavailable, "plugin manager is not initialized")
		return
	}
	if r.Method != http.MethodPost {
		adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	pluginID, action, ok := splitPluginSubresource(w, rawSegment, "rollback")
	if !ok {
		return
	}
	var req adminhttp.PluginRollbackRequest
	if !adminhttp.DecodeJSONRequest(w, r, &req) {
		return
	}
	var plugin pluginmanager.PluginRecord
	var err error
	switch action {
	case "artifact":
		plugin, err = pluginsManager.RollbackArtifact(r.Context(), session.Username, pluginID, req.ArtifactID)
	case "config":
		var snapshot pluginmanager.ConfigSnapshotRecord
		snapshot, err = pluginsManager.ConfigSnapshot(r.Context(), req.SnapshotID)
		if err == nil && snapshot.PluginID != pluginID {
			err = pluginmanager.ErrPluginNotFound
		}
		if err != nil {
			break
		}
		plugin, err = pluginsManager.RollbackConfigSnapshot(r.Context(), session.Username, req.SnapshotID, req.FullDesired)
	default:
		adminhttp.WriteAPIError(w, http.StatusBadRequest, "unknown rollback action")
		return
	}
	if err != nil {
		recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_rollback_"+action, "plugin", pluginID, false, err.Error())
		writePluginManagerError(w, err)
		return
	}
	recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_rollback_"+action, "plugin", pluginID, true, "rollback desired state updated", map[string]any{
		"desired_generation": plugin.DesiredGeneration,
		"active_changed":     false,
	})
	view, err := pluginView(r, plugin, true)
	if err != nil {
		adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	delete(view, "config_json")
	adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"plugin": view})
}

func handleAdminPluginOperations(w http.ResponseWriter, r *http.Request, rawSegment string) {
	session, ok := requireRole(w, r, adminRoleMember)
	if !ok {
		return
	}
	if pluginsManager == nil {
		adminhttp.WriteAPIError(w, http.StatusServiceUnavailable, "plugin manager is not initialized")
		return
	}
	pluginID, action, ok := splitPluginSubresource(w, rawSegment, "operations")
	if !ok {
		return
	}
	switch {
	case r.Method == http.MethodGet && action == "":
		snapshot, err := pluginsManager.OperationsSnapshot(r.Context(), pluginID)
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"operations": snapshot})
	case r.Method == http.MethodPost && strings.HasPrefix(action, "tasks/") && strings.HasSuffix(action, "/trigger"):
		if session.Role != adminRoleAdmin {
			adminhttp.WriteAPIError(w, http.StatusForbidden, "forbidden")
			return
		}
		parts := strings.Split(action, "/")
		if len(parts) != 3 {
			adminhttp.WriteAPIError(w, http.StatusBadRequest, "invalid task trigger path")
			return
		}
		taskID, err := adminhttp.PathSegment(parts[1])
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		var req struct {
			ConfirmToken string `json:"confirm_token"`
		}
		if !adminhttp.DecodeJSONRequest(w, r, &req) {
			return
		}
		task, err := pluginsManager.TriggerBackgroundTask(r.Context(), session.Username, pluginID, taskID, req.ConfirmToken)
		if err != nil {
			recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_background_task_trigger", "plugin", pluginID, false, err.Error())
			adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_background_task_trigger", "plugin", pluginID, true, "background task triggered", map[string]any{"task_id": taskID})
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"task": task})
	case r.Method == http.MethodPost && strings.HasPrefix(action, "tasks/") && strings.HasSuffix(action, "/cancel"):
		if session.Role != adminRoleAdmin {
			adminhttp.WriteAPIError(w, http.StatusForbidden, "forbidden")
			return
		}
		parts := strings.Split(action, "/")
		if len(parts) != 3 {
			adminhttp.WriteAPIError(w, http.StatusBadRequest, "invalid task cancel path")
			return
		}
		taskID, err := adminhttp.PathSegment(parts[1])
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		task, err := pluginsManager.CancelBackgroundTask(r.Context(), session.Username, pluginID, taskID)
		if err != nil {
			recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_background_task_cancel", "plugin", pluginID, false, err.Error())
			adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_background_task_cancel", "plugin", pluginID, true, "background task canceled", map[string]any{"task_id": taskID})
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"task": task})
	case r.Method == http.MethodPost && strings.HasPrefix(action, "external/") && strings.HasSuffix(action, "/health-check"):
		if session.Role != adminRoleAdmin {
			adminhttp.WriteAPIError(w, http.StatusForbidden, "forbidden")
			return
		}
		parts := strings.Split(action, "/")
		if len(parts) != 3 {
			adminhttp.WriteAPIError(w, http.StatusBadRequest, "invalid external dependency health-check path")
			return
		}
		dependency, err := adminhttp.PathSegment(parts[1])
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		result, err := pluginsManager.HealthCheckExternalDependency(r.Context(), session.Username, pluginID, dependency)
		if err != nil {
			recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_external_dependency_health_check", "plugin", pluginID, false, err.Error())
			writePluginManagerError(w, err)
			return
		}
		recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_external_dependency_health_check", "plugin", pluginID, result.OK, "external dependency health check completed", map[string]any{
			"dependency":    dependency,
			"ok":            result.OK,
			"status":        result.Summary.LastStatus,
			"circuit_state": result.Summary.CircuitState,
		})
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"health_check": result})
	case r.Method == http.MethodGet && action == "diagnostic":
		data, summary, err := pluginsManager.DiagnosticPackage(r.Context(), session.Username, pluginID)
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		var body any
		if err := json.Unmarshal(data, &body); err != nil {
			body = map[string]any{"error": "diagnostic package could not be decoded"}
		}
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"diagnostic": body, "summary": summary})
	case (r.Method == http.MethodGet || r.Method == http.MethodPost) && action == "gc":
		if r.Method == http.MethodPost && session.Role != adminRoleAdmin {
			adminhttp.WriteAPIError(w, http.StatusForbidden, "forbidden")
			return
		}
		dryRun := r.Method == http.MethodGet
		candidates, err := pluginsManager.RunOperationsGC(r.Context(), session.Username, pluginID, dryRun)
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"dry_run": dryRun, "candidates": candidates})
	default:
		adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func handleAdminPluginOperationsGC(w http.ResponseWriter, r *http.Request) {
	session, ok := requireRole(w, r, adminRoleMember)
	if !ok {
		return
	}
	if pluginsManager == nil {
		adminhttp.WriteAPIError(w, http.StatusServiceUnavailable, "plugin manager is not initialized")
		return
	}
	if r.Method == http.MethodPost && session.Role != adminRoleAdmin {
		adminhttp.WriteAPIError(w, http.StatusForbidden, "forbidden")
		return
	}
	dryRun := r.Method == http.MethodGet
	candidates, err := pluginsManager.RunOperationsGC(r.Context(), session.Username, r.URL.Query().Get("plugin_id"), dryRun)
	if err != nil {
		adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"dry_run": dryRun, "candidates": candidates})
}

func handleAdminPluginDiagnostics(w http.ResponseWriter, r *http.Request, rawPluginID string) {
	session, ok := requireRole(w, r, adminRoleAdmin)
	if !ok {
		return
	}
	if pluginsManager == nil {
		adminhttp.WriteAPIError(w, http.StatusServiceUnavailable, "plugin manager is not initialized")
		return
	}
	if r.Method != http.MethodGet {
		adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	pluginID, err := adminhttp.PathSegment(rawPluginID)
	if err != nil {
		adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	data, summary, err := pluginsManager.DiagnosticPackage(r.Context(), session.Username, pluginID)
	if err != nil {
		writePluginManagerError(w, err)
		return
	}
	var body any
	if err := json.Unmarshal(data, &body); err != nil {
		body = map[string]any{"error": "diagnostic package could not be decoded"}
	}
	adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"diagnostic": body, "summary": summary})
}

func handleAdminPluginAction(w http.ResponseWriter, r *http.Request, rawSegment string) {
	session, ok := requireRole(w, r, adminRoleAdmin)
	if !ok {
		return
	}
	if pluginsManager == nil {
		adminhttp.WriteAPIError(w, http.StatusServiceUnavailable, "plugin manager is not initialized")
		return
	}
	if r.Method != http.MethodPost {
		adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	parts := strings.Split(rawSegment, "/")
	if len(parts) != 2 {
		adminhttp.WriteAPIError(w, http.StatusBadRequest, "invalid plugin action")
		return
	}
	pluginID, err := adminhttp.PathSegment(parts[0])
	if err != nil {
		adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	action, err := adminhttp.PathSegment(parts[1])
	if err != nil {
		adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	var plugin pluginmanager.PluginRecord
	switch action {
	case "load":
		plugin, err = pluginsManager.Load(r.Context(), session.Username, pluginID)
	case "enable":
		plugin, err = pluginsManager.Enable(r.Context(), session.Username, pluginID)
	case "disable":
		plugin, err = pluginsManager.Disable(r.Context(), session.Username, pluginID)
	case "delete":
		err = pluginsManager.Delete(r.Context(), session.Username, pluginID)
	default:
		adminhttp.WriteAPIError(w, http.StatusBadRequest, "unknown plugin action")
		return
	}
	if err != nil {
		recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_"+action, "plugin", pluginID, false, err.Error())
		writePluginManagerError(w, err)
		return
	}
	recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_"+action, "plugin", pluginID, true, "plugin "+action+" succeeded")
	if action == "delete" {
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"plugin": plugin})
}

func handleAdminPluginDraining(w http.ResponseWriter, r *http.Request, rawPluginID string) {
	session, ok := requireRole(w, r, adminRoleAdmin)
	if !ok {
		return
	}
	if pluginsManager == nil {
		adminhttp.WriteAPIError(w, http.StatusServiceUnavailable, "plugin manager is not initialized")
		return
	}
	if r.Method != http.MethodPost {
		adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	pluginID, err := adminhttp.PathSegment(rawPluginID)
	if err != nil {
		adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	closed, err := pluginsManager.ForceCloseDraining(r.Context(), session.Username, pluginID)
	if err != nil {
		recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_force_close_draining", "plugin", pluginID, false, err.Error())
		writePluginManagerError(w, err)
		return
	}
	recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_force_close_draining", "plugin", pluginID, true, "draining protocol-proxy connections force closed", map[string]any{
		"closed": closed,
	})
	adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"closed": closed})
}

func handleAdminPluginDispatchPlan(w http.ResponseWriter, r *http.Request) {
	session, ok := requireRole(w, r, adminRoleMember)
	if !ok {
		return
	}
	if pluginsManager == nil {
		adminhttp.WriteAPIError(w, http.StatusServiceUnavailable, "plugin manager is not initialized")
		return
	}
	switch r.Method {
	case http.MethodGet:
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"dispatch_plan": pluginsManager.DispatchPlan(r.Context())})
	case http.MethodPost:
		if session.Role != adminRoleAdmin {
			adminhttp.WriteAPIError(w, http.StatusForbidden, "forbidden")
			return
		}
		var req struct {
			Action string `json:"action"`
		}
		if !adminhttp.DecodeJSONRequest(w, r, &req) {
			return
		}
		switch req.Action {
		case "refresh-routes":
			cache := pluginsManager.RefreshRouteProviders(r.Context(), session.Username)
			recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_route_provider_refresh", "plugin_dispatch", "", true, "route provider cache refreshed", map[string]any{"cache_entries": len(cache)})
			adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"route_cache": cache})
		case "replay-subscribers":
			count := pluginsManager.ReplaySubscriberDeadLetters(r.Context(), session.Username)
			recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_event_subscriber_replay", "plugin_dispatch", "", true, "event subscriber dead letters replay requested", map[string]any{"replayed": count})
			adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"replayed": count})
		case "drop-subscriber-dead-letter":
			count := pluginsManager.DropSubscriberDeadLetters(r.Context(), session.Username)
			recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_event_subscriber_drop", "plugin_dispatch", "", true, "event subscriber dead letters dropped", map[string]any{"dropped": count})
			adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"dropped": count})
		default:
			adminhttp.WriteAPIError(w, http.StatusBadRequest, "unknown dispatch action")
		}
	default:
		adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func handleAdminPluginGovernance(w http.ResponseWriter, r *http.Request, rawSegment string) {
	session, ok := requireRole(w, r, adminRoleMember)
	if !ok {
		return
	}
	if pluginsManager == nil {
		adminhttp.WriteAPIError(w, http.StatusServiceUnavailable, "plugin manager is not initialized")
		return
	}
	pluginID, action, ok := splitPluginSubresource(w, rawSegment, "governance")
	if !ok {
		return
	}
	switch {
	case r.Method == http.MethodGet && action == "":
		status, err := pluginsManager.GovernanceStatus(r.Context(), pluginID, r.URL.Query().Get("artifact_id"), r.URL.Query().Get("profile"))
		if err != nil {
			writePluginManagerError(w, err)
			return
		}
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"governance": status})
	case r.Method == http.MethodPost && action == "review":
		if session.Role != adminRoleAdmin {
			adminhttp.WriteAPIError(w, http.StatusForbidden, "forbidden")
			return
		}
		var req pluginmanager.GovernanceReviewRequest
		if !adminhttp.DecodeJSONRequest(w, r, &req) {
			return
		}
		review, err := pluginsManager.CreateReview(r.Context(), session.Username, pluginID, req)
		if err != nil {
			recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_governance_review", "plugin", pluginID, false, err.Error())
			writePluginManagerError(w, err)
			return
		}
		recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_governance_review", "plugin", pluginID, true, "governance review recorded", map[string]any{
			"artifact_id": review.ArtifactID,
			"profile":     review.Profile,
			"policy_hash": review.PolicyHash,
			"decision":    review.Decision,
		})
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"review": review})
	case r.Method == http.MethodPost && action == "override":
		if session.Role != adminRoleAdmin {
			adminhttp.WriteAPIError(w, http.StatusForbidden, "forbidden")
			return
		}
		var req pluginmanager.WarningOverrideRequest
		if !adminhttp.DecodeJSONRequest(w, r, &req) {
			return
		}
		override, err := pluginsManager.CreateWarningOverride(r.Context(), session.Username, pluginID, req)
		if err != nil {
			recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_governance_override", "plugin", pluginID, false, err.Error())
			writePluginManagerError(w, err)
			return
		}
		recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_governance_override", "plugin", pluginID, true, "governance warning override recorded", map[string]any{
			"artifact_id": override.ArtifactID,
			"profile":     override.Profile,
			"action":      override.Action,
			"expires_at":  override.ExpiresAt,
		})
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"override": override})
	case r.Method == http.MethodPost && action == "preflight":
		if session.Role != adminRoleAdmin {
			adminhttp.WriteAPIError(w, http.StatusForbidden, "forbidden")
			return
		}
		var req pluginmanager.PreflightRequest
		if !adminhttp.DecodeJSONRequest(w, r, &req) {
			return
		}
		result, err := pluginsManager.RunPreflight(r.Context(), session.Username, pluginID, req)
		if err != nil {
			recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_governance_preflight", "plugin", pluginID, false, err.Error())
			writePluginManagerError(w, err)
			return
		}
		recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_governance_preflight", "plugin", pluginID, result.OK, "governance preflight completed", map[string]any{"profile": result.Profile})
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"preflight": result})
	case r.Method == http.MethodPost && action == "self-test":
		if session.Role != adminRoleAdmin {
			adminhttp.WriteAPIError(w, http.StatusForbidden, "forbidden")
			return
		}
		var req pluginmanager.SelfTestRequest
		if !adminhttp.DecodeJSONRequest(w, r, &req) {
			return
		}
		result, err := pluginsManager.RunSelfTest(r.Context(), session.Username, pluginID, req)
		if err != nil {
			recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_governance_self_test", "plugin", pluginID, false, err.Error())
			writePluginManagerError(w, err)
			return
		}
		recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_governance_self_test", "plugin", pluginID, result.OK, "governance self-test completed", map[string]any{"profile": result.Profile})
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"self_test": result})
	case r.Method == http.MethodPost && action == "benchmark":
		if session.Role != adminRoleAdmin {
			adminhttp.WriteAPIError(w, http.StatusForbidden, "forbidden")
			return
		}
		var req pluginmanager.BenchmarkRequest
		if !adminhttp.DecodeJSONRequest(w, r, &req) {
			return
		}
		benchmark, err := pluginsManager.SaveBenchmark(r.Context(), session.Username, req)
		if err != nil {
			recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_governance_benchmark", "plugin", pluginID, false, err.Error())
			writePluginManagerError(w, err)
			return
		}
		recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_governance_benchmark", "plugin", pluginID, true, "governance benchmark recorded", map[string]any{
			"artifact_id":   benchmark.ArtifactID,
			"profile":       benchmark.Profile,
			"baseline_diff": benchmark.BaselineDiff,
		})
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"benchmark": benchmark})
	default:
		adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func handleAdminPluginAdvisories(w http.ResponseWriter, r *http.Request) {
	session, ok := requireRole(w, r, adminRoleMember)
	if !ok {
		return
	}
	if pluginsManager == nil {
		adminhttp.WriteAPIError(w, http.StatusServiceUnavailable, "plugin manager is not initialized")
		return
	}
	switch r.Method {
	case http.MethodGet:
		advisories, err := pluginsManager.ListAdvisories(r.Context(), r.URL.Query().Get("plugin_id"))
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"advisories": advisories})
	case http.MethodPost:
		if session.Role != adminRoleAdmin {
			adminhttp.WriteAPIError(w, http.StatusForbidden, "forbidden")
			return
		}
		var req adminPluginAdvisoryPostRequest
		if !adminhttp.DecodeJSONRequest(w, r, &req) {
			return
		}
		if req.Rescan {
			report, err := pluginsManager.RescanAdvisories(r.Context(), session.Username, req.PluginID, req.ArtifactID)
			if err != nil {
				recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_governance_advisory_rescan", "plugin_advisory", req.PluginID, false, err.Error())
				writePluginManagerError(w, err)
				return
			}
			recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_governance_advisory_rescan", "plugin_advisory", req.PluginID, true, "plugin advisories rescanned", map[string]any{
				"plugin_id":       report.PluginID,
				"artifact_id":     report.ArtifactID,
				"scanned":         report.Scanned,
				"matches":         len(report.Matches),
				"blocking":        report.Blocking,
				"warnings":        report.Warnings,
				"quarantine_runs": report.QuarantineRuns,
			})
			adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"rescan": report})
			return
		}
		if req.FeedURL != "" {
			result, err := pluginsManager.SyncExternalAdvisoryFeed(r.Context(), session.Username, pluginmanager.ExternalFeedSchedule{
				URL:    req.FeedURL,
				Source: req.Source,
			})
			if err != nil {
				recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_governance_advisory_feed_sync", "plugin_advisory", req.Source, false, err.Error())
				writePluginManagerError(w, err)
				return
			}
			recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_governance_advisory_feed_sync", "plugin_advisory", result.Source, true, "plugin advisory feed synced from external source", map[string]any{
				"source":          result.Source,
				"imported":        result.Imported,
				"matches":         len(result.Rescan.Matches),
				"blocking":        result.Rescan.Blocking,
				"warnings":        result.Rescan.Warnings,
				"quarantine_runs": result.Rescan.QuarantineRuns,
			})
			adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"feed": result})
			return
		}
		if len(req.Advisories) > 0 {
			result, err := pluginsManager.SyncAdvisoryFeed(r.Context(), session.Username, pluginmanager.AdvisoryFeedRequest{
				Source:     req.Source,
				Advisories: req.Advisories,
			})
			if err != nil {
				recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_governance_advisory_feed", "plugin_advisory", req.Source, false, err.Error())
				writePluginManagerError(w, err)
				return
			}
			recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_governance_advisory_feed", "plugin_advisory", result.Source, true, "plugin advisory feed synced", map[string]any{
				"source":          result.Source,
				"imported":        result.Imported,
				"matches":         len(result.Rescan.Matches),
				"blocking":        result.Rescan.Blocking,
				"warnings":        result.Rescan.Warnings,
				"quarantine_runs": result.Rescan.QuarantineRuns,
			})
			adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"feed": result})
			return
		}
		advisory, err := pluginsManager.UpsertAdvisory(r.Context(), session.Username, req.AdvisoryRequest)
		if err != nil {
			recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_governance_advisory", "plugin_advisory", req.AdvisoryID, false, err.Error())
			writePluginManagerError(w, err)
			return
		}
		recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_governance_advisory", "plugin_advisory", advisory.AdvisoryID, true, "plugin advisory upserted", map[string]any{
			"action":          advisory.Action,
			"status":          advisory.Status,
			"artifact_sha256": advisory.ArtifactSHA256,
			"plugin_id":       advisory.PluginID,
		})
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"advisory": advisory})
	default:
		adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func handleAdminPluginVulnerabilities(w http.ResponseWriter, r *http.Request) {
	session, ok := requireRole(w, r, adminRoleMember)
	if !ok {
		return
	}
	if pluginsManager == nil {
		adminhttp.WriteAPIError(w, http.StatusServiceUnavailable, "plugin manager is not initialized")
		return
	}
	switch r.Method {
	case http.MethodGet:
		vulnerabilities, err := pluginsManager.ListVulnerabilities(r.Context(), r.URL.Query().Get("package_name"))
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"vulnerabilities": vulnerabilities})
	case http.MethodPost:
		if session.Role != adminRoleAdmin {
			adminhttp.WriteAPIError(w, http.StatusForbidden, "forbidden")
			return
		}
		var req adminPluginVulnerabilityPostRequest
		if !adminhttp.DecodeJSONRequest(w, r, &req) {
			return
		}
		if req.Rescan {
			report, err := pluginsManager.ScanVulnerabilities(r.Context(), session.Username, req.PluginID, req.ArtifactID)
			if err != nil {
				recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_governance_vulnerability_scan", "plugin_vulnerability", req.PluginID, false, err.Error())
				writePluginManagerError(w, err)
				return
			}
			recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_governance_vulnerability_scan", "plugin_vulnerability", req.PluginID, true, "plugin vulnerability database scanned", map[string]any{
				"plugin_id":       report.PluginID,
				"artifact_id":     report.ArtifactID,
				"scanned":         report.Scanned,
				"matches":         len(report.Matches),
				"blocking":        report.Blocking,
				"warnings":        report.Warnings,
				"quarantine_runs": report.QuarantineRuns,
			})
			adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"scan": report})
			return
		}
		if req.FeedURL != "" {
			result, err := pluginsManager.SyncExternalVulnerabilityFeed(r.Context(), session.Username, pluginmanager.ExternalFeedSchedule{
				URL:    req.FeedURL,
				Source: req.Source,
			})
			if err != nil {
				recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_governance_vulnerability_feed_sync", "plugin_vulnerability", req.Source, false, err.Error())
				writePluginManagerError(w, err)
				return
			}
			recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_governance_vulnerability_feed_sync", "plugin_vulnerability", result.Source, true, "plugin vulnerability feed synced from external source", map[string]any{
				"source":          result.Source,
				"imported":        result.Imported,
				"matches":         len(result.Scan.Matches),
				"blocking":        result.Scan.Blocking,
				"warnings":        result.Scan.Warnings,
				"quarantine_runs": result.Scan.QuarantineRuns,
			})
			adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"database": result})
			return
		}
		if len(req.Vulnerabilities) > 0 {
			result, err := pluginsManager.ImportVulnerabilityDB(r.Context(), session.Username, pluginmanager.VulnerabilityDBRequest{
				Source:          req.Source,
				Vulnerabilities: req.Vulnerabilities,
			})
			if err != nil {
				recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_governance_vulnerability_db", "plugin_vulnerability", req.Source, false, err.Error())
				writePluginManagerError(w, err)
				return
			}
			recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_governance_vulnerability_db", "plugin_vulnerability", result.Source, true, "plugin vulnerability database imported", map[string]any{
				"source":          result.Source,
				"imported":        result.Imported,
				"matches":         len(result.Scan.Matches),
				"blocking":        result.Scan.Blocking,
				"warnings":        result.Scan.Warnings,
				"quarantine_runs": result.Scan.QuarantineRuns,
			})
			adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"database": result})
			return
		}
		vulnerability, err := pluginsManager.UpsertVulnerability(r.Context(), session.Username, req.VulnerabilityRequest)
		if err != nil {
			recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_governance_vulnerability", "plugin_vulnerability", req.VulnerabilityID, false, err.Error())
			writePluginManagerError(w, err)
			return
		}
		recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_governance_vulnerability", "plugin_vulnerability", vulnerability.VulnerabilityID, true, "plugin vulnerability upserted", map[string]any{
			"action":       vulnerability.Action,
			"status":       vulnerability.Status,
			"package_name": vulnerability.PackageName,
			"severity":     vulnerability.Severity,
		})
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"vulnerability": vulnerability})
	default:
		adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

type adminPluginAdvisoryPostRequest struct {
	pluginmanager.AdvisoryRequest
	Source     string                          `json:"source"`
	Advisories []pluginmanager.AdvisoryRequest `json:"advisories"`
	Rescan     bool                            `json:"rescan"`
	ArtifactID string                          `json:"artifact_id"`
	FeedURL    string                          `json:"feed_url"`
}

type adminPluginVulnerabilityPostRequest struct {
	pluginmanager.VulnerabilityRequest
	Vulnerabilities []pluginmanager.VulnerabilityRequest `json:"vulnerabilities"`
	Rescan          bool                                 `json:"rescan"`
	PluginID        string                               `json:"plugin_id"`
	ArtifactID      string                               `json:"artifact_id"`
	FeedURL         string                               `json:"feed_url"`
}

func receivePluginArtifact(r *http.Request, actor string) (pluginmanager.ArtifactRecord, error) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	file, header, err := r.FormFile("artifact")
	if err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	defer file.Close()

	tmp, err := os.CreateTemp("", "mc-gateway-plugin-*.mcgp")
	if err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	defer tmp.Close()

	if _, err := tmp.ReadFrom(file); err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	if err := tmp.Close(); err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}

	upload := pluginmanager.ArtifactUpload{
		SourcePath: tmpPath,
		FileName:   filepath.Base(header.Filename),
		Actor:      actor,
	}
	manifest, err := readPackageManifest(tmpPath)
	if err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	if manifest.ArtifactType == pluginmanager.ArtifactTypeSource {
		return pluginsManager.UploadSource(r.Context(), upload)
	}
	return pluginsManager.UploadArtifact(r.Context(), upload)
}

func receivePluginSource(r *http.Request, actor string) (pluginmanager.ArtifactRecord, error) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	file, header, err := r.FormFile("artifact")
	if err != nil {
		file, header, err = r.FormFile("source")
	}
	if err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	defer file.Close()

	tmp, err := os.CreateTemp("", "mc-gateway-plugin-source-*.mcgp")
	if err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	defer tmp.Close()

	if _, err := tmp.ReadFrom(file); err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}
	if err := tmp.Close(); err != nil {
		return pluginmanager.ArtifactRecord{}, err
	}

	return pluginsManager.UploadSource(r.Context(), pluginmanager.ArtifactUpload{
		SourcePath: tmpPath,
		FileName:   filepath.Base(header.Filename),
		Actor:      actor,
	})
}

func pluginConfigJSON(req adminhttp.PluginDesiredRequest) (string, error) {
	if strings.TrimSpace(req.ConfigJSON) != "" {
		if !json.Valid([]byte(req.ConfigJSON)) {
			return "", errors.New("config_json must be valid JSON")
		}
		return req.ConfigJSON, nil
	}
	if req.Config == nil {
		return "{}", nil
	}
	data, err := json.Marshal(req.Config)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func pluginConfigRequestJSON(req adminhttp.PluginConfigRequest) (string, error) {
	if strings.TrimSpace(req.ConfigJSON) != "" {
		if !json.Valid([]byte(req.ConfigJSON)) {
			return "", errors.New("config_json must be valid JSON")
		}
		return req.ConfigJSON, nil
	}
	if req.Config == nil {
		return "{}", nil
	}
	data, err := json.Marshal(req.Config)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func splitPluginSubresource(w http.ResponseWriter, rawSegment, resource string) (pluginID, action string, ok bool) {
	parts := strings.Split(rawSegment, "/")
	if len(parts) < 2 || parts[1] != resource {
		adminhttp.WriteAPIError(w, http.StatusBadRequest, "invalid plugin "+resource+" path")
		return "", "", false
	}
	id, err := adminhttp.PathSegment(parts[0])
	if err != nil {
		adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
		return "", "", false
	}
	if len(parts) > 2 {
		action = strings.Join(parts[2:], "/")
	}
	return id, action, true
}

func pluginViews(r *http.Request, plugins []pluginmanager.PluginRecord) ([]map[string]any, error) {
	views := make([]map[string]any, 0, len(plugins))
	for _, plugin := range plugins {
		view, err := pluginView(r, plugin, false)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

func publicPluginBuilds(builds []pluginmanager.BuildRecord) []pluginmanager.BuildRecord {
	public := make([]pluginmanager.BuildRecord, 0, len(builds))
	for _, build := range builds {
		public = append(public, publicPluginBuild(build))
	}
	return public
}

func publicPluginBuild(build pluginmanager.BuildRecord) pluginmanager.BuildRecord {
	build.GOPROXY = buildPolicySummary(build.GOPROXY)
	build.GONOSUMDB = buildPolicySummary(build.GONOSUMDB)
	build.GOPRIVATE = buildPolicySummary(build.GOPRIVATE)
	return build
}

func buildPolicySummary(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return "configured"
}

func pluginView(r *http.Request, plugin pluginmanager.PluginRecord, detail bool) (map[string]any, error) {
	artifacts, err := pluginsManager.ListArtifacts(r.Context(), plugin.ID)
	if err != nil {
		return nil, err
	}
	builds, err := pluginsManager.ListBuilds(r.Context(), plugin.ID)
	if err != nil {
		return nil, err
	}
	secrets, err := pluginsManager.ListSecrets(r.Context(), plugin.ID)
	if err != nil {
		return nil, err
	}
	snapshots, err := pluginsManager.ListConfigSnapshots(r.Context(), plugin.ID)
	if err != nil {
		return nil, err
	}
	artifactByID := make(map[string]pluginmanager.ArtifactRecord, len(artifacts))
	for _, artifact := range artifacts {
		artifactByID[artifact.ID] = artifact
	}
	desiredArtifact := artifactByID[plugin.DesiredArtifactID]
	activeArtifact := artifactByID[plugin.ActiveArtifactID]
	loadedArtifact := artifactByID[plugin.LoadedArtifactID]
	manifest := pluginManifest(desiredArtifact)
	view := map[string]any{
		"id":                   plugin.ID,
		"name":                 manifest.Name,
		"version":              desiredArtifact.Version,
		"artifact_type":        desiredArtifact.ArtifactType,
		"runtime_type":         desiredArtifact.RuntimeType,
		"desired_state":        plugin.DesiredState,
		"runtime_state":        plugin.RuntimeState,
		"desired_artifact_id":  plugin.DesiredArtifactID,
		"active_artifact_id":   plugin.ActiveArtifactID,
		"loaded_artifact_id":   plugin.LoadedArtifactID,
		"desired_artifact":     desiredArtifact,
		"active_artifact":      activeArtifact,
		"loaded_artifact":      loadedArtifact,
		"extension_points":     jsonArrayString(desiredArtifact.ExtensionPointsJSON),
		"priority":             plugin.Priority,
		"scope":                pluginScope(manifest),
		"rollout":              pluginRollout(manifest),
		"restart_required":     pluginRestartRequired(plugin),
		"health":               pluginHealth(plugin),
		"last_error":           plugin.LastError,
		"runtime_summary":      jsonObjectString(plugin.RuntimeSummaryJSON),
		"dispatch_summary":     jsonArrayString(plugin.DispatchSummaryJSON),
		"capabilities_summary": jsonObjectString(desiredArtifact.CapabilitiesSummaryJSON),
		"extension_status":     pluginExtensionStatus(r.Context(), plugin.ID),
		"minecraft":            pluginMinecraftSummary(desiredArtifact),
		"config_json":          plugin.ConfigJSON,
		"config_schema":        jsonObjectString(manifestConfigSchemaString(manifest)),
		"secrets":              secrets,
		"snapshots":            snapshots,
		"builds":               publicPluginBuilds(builds),
		"artifacts":            artifacts,
		"updated_at":           plugin.UpdatedAt,
	}
	if detail {
		view["manifest"] = manifest
		view["operations_path"] = "/plugin-artifacts?plugin_id=" + plugin.ID
		governance, err := pluginsManager.GovernanceStatus(r.Context(), plugin.ID, plugin.DesiredArtifactID, pluginsManager.PolicyProfile())
		if err != nil {
			view["governance_error"] = err.Error()
		} else {
			view["governance"] = governance
		}
		rollout, err := pluginsManager.PluginRolloutStatus(r.Context(), plugin.ID)
		if err != nil {
			view["rollout_error"] = err.Error()
		} else {
			view["rollout_status"] = rollout
			view["node_runtime_states"] = rollout.NodeRuntimeStates
		}
		view["active_proxy_connections"] = activeProxyConnections(plugin)
		connections, err := pluginsManager.ActiveProxyConnections(r.Context(), plugin.ID)
		if err != nil {
			return nil, err
		}
		view["proxy_connections"] = connections
	}
	return view, nil
}

func pluginExtensionStatus(ctx context.Context, pluginID string) map[string]any {
	plan := pluginsManager.DispatchPlan(ctx)
	filter := func(items []pluginmanager.DispatchHandlerSummary) []pluginmanager.DispatchHandlerSummary {
		var out []pluginmanager.DispatchHandlerSummary
		for _, item := range items {
			if item.PluginID == pluginID {
				out = append(out, item)
			}
		}
		return out
	}
	var providers []pluginmanager.ProviderSummary
	for _, provider := range plan.Providers {
		if provider.PluginID == pluginID {
			providers = append(providers, provider)
		}
	}
	return map[string]any{
		"routes":      filter(plan.Routes),
		"rules":       filter(plan.Rules),
		"statuses":    filter(plan.Statuses),
		"middleware":  filter(plan.Middleware),
		"subscribers": filter(plan.Subscribers),
		"providers":   providers,
		"route_cache": plan.RouteCache,
	}
}

func pluginManifest(artifact pluginmanager.ArtifactRecord) pluginmanager.Manifest {
	var manifest pluginmanager.Manifest
	_ = json.Unmarshal([]byte(artifact.MetadataJSON), &manifest)
	return manifest
}

func manifestConfigSchemaString(manifest pluginmanager.Manifest) string {
	if len(manifest.ConfigSchema) == 0 {
		return "{}"
	}
	return string(manifest.ConfigSchema)
}

func pluginScope(manifest pluginmanager.Manifest) any {
	var caps map[string]any
	if len(manifest.Capabilities) == 0 || json.Unmarshal(manifest.Capabilities, &caps) != nil {
		return map[string]any{"type": "global"}
	}
	if scope, ok := caps["scope"]; ok {
		return scope
	}
	return map[string]any{"type": "global"}
}

func pluginRollout(manifest pluginmanager.Manifest) any {
	var caps map[string]any
	if len(manifest.Capabilities) == 0 || json.Unmarshal(manifest.Capabilities, &caps) != nil {
		return map[string]any{"mode": "all"}
	}
	if rollout, ok := caps["rollout"]; ok {
		return rollout
	}
	return map[string]any{"mode": "all"}
}

func pluginHealth(plugin pluginmanager.PluginRecord) string {
	if plugin.LastError != "" || plugin.RuntimeState == pluginmanager.RuntimeFailed {
		return "error"
	}
	if plugin.RuntimeState == pluginmanager.RuntimeEnabled {
		return "healthy"
	}
	if plugin.RuntimeState == pluginmanager.RuntimeDraining {
		return "draining"
	}
	return "inactive"
}

func pluginRestartRequired(plugin pluginmanager.PluginRecord) bool {
	return plugin.LoadedArtifactID != "" && plugin.DesiredArtifactID != "" && plugin.LoadedArtifactID != plugin.DesiredArtifactID
}

func pluginMinecraftSummary(artifact pluginmanager.ArtifactRecord) any {
	var summary pluginmanager.CapabilitySummary
	if json.Unmarshal([]byte(artifact.CapabilitiesSummaryJSON), &summary) != nil || summary.Minecraft == nil {
		return nil
	}
	return summary.Minecraft
}

func activeProxyConnections(plugin pluginmanager.PluginRecord) int64 {
	var summary map[string]any
	if json.Unmarshal([]byte(plugin.RuntimeSummaryJSON), &summary) != nil {
		return 0
	}
	switch value := summary["active_proxy_connections"].(type) {
	case float64:
		return int64(value)
	case int64:
		return value
	default:
		return 0
	}
}

func jsonObjectString(raw string) any {
	var value any
	if raw == "" || json.Unmarshal([]byte(raw), &value) != nil {
		return map[string]any{}
	}
	return value
}

func jsonArrayString(raw string) any {
	var value any
	if raw == "" || json.Unmarshal([]byte(raw), &value) != nil {
		return []any{}
	}
	return value
}

func writePluginManagerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, pluginmanager.ErrArtifactNotFound), errors.Is(err, pluginmanager.ErrPluginNotFound):
		adminhttp.WriteAPIError(w, http.StatusNotFound, err.Error())
	default:
		adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
	}
}

func handleAdminPluginFeatures(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireRole(w, r, adminRoleMember); !ok {
		return
	}
	if r.Method != http.MethodGet {
		adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	facts := pluginFeatureFacts()
	if pluginsManager != nil {
		facts = pluginFeatureFactsFor(pluginsManager.RuntimeFeatureFactsOptions())
	}
	adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"plugin_features": facts})
}

func handleAdminPluginService(w http.ResponseWriter, r *http.Request) {
	session, ok := requireRole(w, r, adminRoleMember)
	if !ok {
		return
	}
	if pluginsManager == nil {
		adminhttp.WriteAPIError(w, http.StatusServiceUnavailable, "plugin manager is not initialized")
		return
	}
	switch r.Method {
	case http.MethodGet:
		status, err := pluginsManager.PluginServiceStatus(r.Context())
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"plugin_service": status})
	case http.MethodPut:
		if session.Role != adminRoleAdmin {
			adminhttp.WriteAPIError(w, http.StatusForbidden, "forbidden")
			return
		}
		var req adminhttp.PluginServiceRequest
		if !adminhttp.DecodeJSONRequest(w, r, &req) {
			return
		}
		var state pluginmanager.PluginServiceState
		var err error
		if req.DesiredMode != "" {
			state, err = pluginsManager.SetPluginServiceDesired(r.Context(), session.Username, req.DesiredMode)
			if err != nil {
				recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_service_mode_update", "plugin_service", "", false, err.Error())
				adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
				return
			}
		} else {
			state, err = pluginsManager.PluginServiceState(r.Context())
			if err != nil {
				adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		if req.CrashPolicy != nil {
			policy := state.CrashPolicy
			if req.CrashPolicy.BackoffSeconds != nil {
				policy.BackoffSeconds = *req.CrashPolicy.BackoffSeconds
			}
			if req.CrashPolicy.MaxCrashes != nil {
				policy.MaxCrashes = *req.CrashPolicy.MaxCrashes
			}
			if req.CrashPolicy.WindowSeconds != nil {
				policy.WindowSeconds = *req.CrashPolicy.WindowSeconds
			}
			state, err = pluginsManager.SetPluginServiceCrashPolicy(r.Context(), session.Username, policy)
			if err != nil {
				recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_service_mode_update", "plugin_service", "", false, err.Error())
				adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_service_mode_update", "plugin_service", "", true, "plugin service mode desired state updated", map[string]any{
			"desired_mode":     state.DesiredMode,
			"active_mode":      state.ActiveMode,
			"data_plane_mode":  state.DataPlaneMode,
			"restart_required": state.RestartRequired,
			"maturity":         state.DesiredMaturity,
			"unsupported":      state.UnsupportedReason,
			"crash_policy":     state.CrashPolicy,
		})
		status, err := pluginsManager.PluginServiceStatus(r.Context())
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"plugin_service": status})
	case http.MethodPost:
		if session.Role != adminRoleAdmin {
			adminhttp.WriteAPIError(w, http.StatusForbidden, "forbidden")
			return
		}
		if err := pluginsManager.ApplyPluginServiceMode(r.Context()); err != nil {
			adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		status, err := pluginsManager.PluginServiceStatus(r.Context())
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"plugin_service": status})
	default:
		adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func handleAdminPluginRepositories(w http.ResponseWriter, r *http.Request) {
	session, ok := requireRole(w, r, adminRoleMember)
	if !ok {
		return
	}
	if pluginsManager == nil {
		adminhttp.WriteAPIError(w, http.StatusServiceUnavailable, "plugin manager is not initialized")
		return
	}
	switch r.Method {
	case http.MethodGet:
		imports, err := pluginsManager.ListRepositoryImports(r.Context())
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"imports": imports})
	case http.MethodPost:
		var req adminhttp.PluginRepositoryImportRequest
		if !adminhttp.DecodeJSONRequest(w, r, &req) {
			return
		}
		switch strings.TrimSpace(req.Action) {
		case "", "import":
		case "updates":
			report, err := pluginsManager.RepositoryUpdateAvailability(r.Context(), pluginmanager.RepositoryImportRequest{
				RepositoryType: req.RepositoryType,
				IndexPath:      req.IndexPath,
				ArtifactID:     req.ArtifactID,
				PluginID:       req.PluginID,
				Version:        req.Version,
				TrustPolicy:    req.TrustPolicy,
			})
			if err != nil {
				adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
				return
			}
			adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"updates": report})
			return
		case "apply":
			if session.Role != adminRoleAdmin {
				adminhttp.WriteAPIError(w, http.StatusForbidden, "forbidden")
				return
			}
			configJSON, err := adminRepositoryImportConfigJSON(req)
			if err != nil {
				adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
				return
			}
			result, err := pluginsManager.ApplyRepositoryImport(r.Context(), session.Username, req.ImportID, configJSON, req.DesiredState, req.Priority, req.DryRun)
			if err != nil {
				recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_repository_import_apply", "plugin_repository_import", strconv.FormatInt(req.ImportID, 10), false, err.Error())
				adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
				return
			}
			recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_repository_import_apply", "plugin_repository_import", strconv.FormatInt(req.ImportID, 10), result.OK, "repository import apply evaluated", map[string]any{
				"import_id": result.Import.ID,
				"plugin_id": result.Import.PluginID,
				"artifact":  result.Import.ArtifactID,
				"dry_run":   result.DryRun,
				"ok":        result.OK,
			})
			adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"result": result, "dry_run": req.DryRun})
			return
		default:
			adminhttp.WriteAPIError(w, http.StatusBadRequest, "unknown repository action")
			return
		}
		if session.Role != adminRoleAdmin {
			adminhttp.WriteAPIError(w, http.StatusForbidden, "forbidden")
			return
		}
		record, artifact, err := pluginsManager.ImportRepositoryArtifact(r.Context(), session.Username, pluginmanager.RepositoryImportRequest{
			RepositoryType: req.RepositoryType,
			IndexPath:      req.IndexPath,
			ArtifactID:     req.ArtifactID,
			PluginID:       req.PluginID,
			Version:        req.Version,
			TrustPolicy:    req.TrustPolicy,
		})
		if err != nil {
			recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_repository_import", "plugin_repository", req.IndexPath, false, err.Error())
			adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_repository_import", "plugin_artifact", artifact.ID, true, "repository artifact imported locally", map[string]any{
			"plugin_id":   artifact.PluginID,
			"version":     artifact.Version,
			"auto_enable": false,
			"import_id":   record.ID,
		})
		adminhttp.WriteJSON(w, http.StatusCreated, map[string]any{"import": record, "artifact": artifact})
	default:
		adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func adminRepositoryImportConfigJSON(req adminhttp.PluginRepositoryImportRequest) (string, error) {
	if req.ConfigJSON != "" && req.Config != nil {
		return "", errors.New("config and config_json are mutually exclusive")
	}
	if req.ConfigJSON != "" {
		if !json.Valid([]byte(req.ConfigJSON)) {
			return "", errors.New("config_json must be valid JSON")
		}
		return req.ConfigJSON, nil
	}
	if req.Config != nil {
		data, err := json.Marshal(req.Config)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	return "", nil
}

func handleAdminPluginSupplyChain(w http.ResponseWriter, r *http.Request) {
	session, ok := requireRole(w, r, adminRoleMember)
	if !ok {
		return
	}
	if pluginsManager == nil {
		adminhttp.WriteAPIError(w, http.StatusServiceUnavailable, "plugin manager is not initialized")
		return
	}
	switch r.Method {
	case http.MethodGet:
		assessments, err := pluginsManager.ListSupplyChainAssessments(r.Context(), r.URL.Query().Get("plugin_id"), r.URL.Query().Get("artifact_id"))
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"assessments": assessments})
	case http.MethodPost:
		if session.Role != adminRoleAdmin {
			adminhttp.WriteAPIError(w, http.StatusForbidden, "forbidden")
			return
		}
		var req adminhttp.PluginSupplyChainRequest
		if !adminhttp.DecodeJSONRequest(w, r, &req) {
			return
		}
		assessment, err := pluginsManager.AssessSupplyChain(r.Context(), session.Username, req.PluginID, req.ArtifactID, req.Metadata)
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		adminhttp.WriteJSON(w, http.StatusCreated, map[string]any{"assessment": assessment})
	default:
		adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

type adminPluginPromotionRequest struct {
	Action             string                        `json:"action"`
	PluginID           string                        `json:"plugin_id"`
	ArtifactID         string                        `json:"artifact_id"`
	Profile            string                        `json:"profile"`
	Config             map[string]any                `json:"config"`
	ConfigJSON         string                        `json:"config_json"`
	ConfigByPlugin     map[string]map[string]any     `json:"config_by_plugin"`
	ConfigJSONByPlugin map[string]string             `json:"config_json_by_plugin"`
	DryRun             bool                          `json:"dry_run"`
	Bundle             pluginmanager.PromotionBundle `json:"bundle"`
	Baseline           pluginmanager.PromotionBundle `json:"baseline"`
	Target             pluginmanager.PromotionBundle `json:"target"`
	Current            pluginmanager.PromotionBundle `json:"current"`
}

func handleAdminPluginPromotions(w http.ResponseWriter, r *http.Request) {
	session, ok := requireRole(w, r, adminRoleAdmin)
	if !ok {
		return
	}
	if pluginsManager == nil {
		adminhttp.WriteAPIError(w, http.StatusServiceUnavailable, "plugin manager is not initialized")
		return
	}
	if r.Method != http.MethodPost {
		adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req adminPluginPromotionRequest
	if !adminhttp.DecodeJSONRequest(w, r, &req) {
		return
	}
	switch strings.TrimSpace(req.Action) {
	case "export":
		configJSON, err := adminPromotionConfigJSON(req)
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		bundle, err := pluginsManager.ExportPromotionBundle(r.Context(), "admin-api", req.Profile, req.PluginID, req.ArtifactID, configJSON)
		if err != nil {
			recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_promotion_export", "plugin", req.PluginID, false, err.Error())
			adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		report := pluginmanager.EvaluatePromotionBundle(bundle)
		recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_promotion_export", "plugin", req.PluginID, true, "promotion bundle exported", map[string]any{
			"artifact_id": req.ArtifactID,
			"bundle_id":   bundle.BundleID,
			"dry_run":     true,
		})
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"bundle": bundle, "report": report, "dry_run": true})
	case "import":
		report := pluginmanager.EvaluatePromotionBundle(req.Bundle)
		recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_promotion_import", "promotion", req.Bundle.BundleID, report.OK, "promotion bundle import dry-run evaluated", map[string]any{
			"bundle_id": req.Bundle.BundleID,
			"dry_run":   true,
			"status":    report.Status,
		})
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"report": report, "dry_run": true})
	case "apply":
		configByPlugin, err := adminPromotionConfigByPlugin(req)
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		result, err := pluginsManager.ApplyPromotionBundle(r.Context(), session.Username, req.Bundle, configByPlugin, req.DryRun)
		if err != nil {
			recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_promotion_apply", "promotion", req.Bundle.BundleID, false, err.Error())
			adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_promotion_apply", "promotion", req.Bundle.BundleID, result.OK, "promotion bundle apply evaluated", map[string]any{
			"bundle_id": req.Bundle.BundleID,
			"dry_run":   req.DryRun,
			"status":    result.Status,
			"applied":   len(result.Applied),
		})
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"result": result, "dry_run": req.DryRun})
	case "diff":
		diff := pluginmanager.DiffPromotionBundles(req.Baseline, req.Target)
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"diff": diff, "ok": len(diff) == 0})
	case "drift":
		report := pluginmanager.PromotionDrift(req.Current, req.Baseline)
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"report": report})
	case "dr-drill":
		configByPlugin, err := adminPromotionConfigByPlugin(req)
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		report, err := pluginsManager.RunPromotionDRDrill(r.Context(), req.Bundle, configByPlugin)
		if err != nil {
			recordAudit(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_promotion_dr_drill", "promotion", req.Bundle.BundleID, false, err.Error())
			adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		recordAuditMetadata(r.Context(), session.Username, adminhttp.RequestSourceIP(r), "plugin_promotion_dr_drill", "promotion", req.Bundle.BundleID, report.OK, "promotion DR drill evaluated", map[string]any{
			"bundle_id": req.Bundle.BundleID,
			"dry_run":   true,
			"status":    report.Status,
		})
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"report": report, "dry_run": true})
	default:
		adminhttp.WriteAPIError(w, http.StatusBadRequest, "unknown promotion action")
	}
}

func adminPromotionConfigJSON(req adminPluginPromotionRequest) (string, error) {
	if req.ConfigJSON != "" && req.Config != nil {
		return "", errors.New("config and config_json are mutually exclusive")
	}
	if req.ConfigJSON != "" {
		if !json.Valid([]byte(req.ConfigJSON)) {
			return "", errors.New("config_json must be valid JSON")
		}
		return req.ConfigJSON, nil
	}
	if req.Config != nil {
		data, err := json.Marshal(req.Config)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	return "", nil
}

func adminPromotionConfigByPlugin(req adminPluginPromotionRequest) (map[string]string, error) {
	out := map[string]string{}
	for pluginID, configJSON := range req.ConfigJSONByPlugin {
		if !json.Valid([]byte(configJSON)) {
			return nil, fmt.Errorf("config_json_by_plugin[%s] must be valid JSON", pluginID)
		}
		out[pluginID] = configJSON
	}
	for pluginID, config := range req.ConfigByPlugin {
		if _, exists := out[pluginID]; exists {
			return nil, fmt.Errorf("config_by_plugin[%s] conflicts with config_json_by_plugin", pluginID)
		}
		data, err := json.Marshal(config)
		if err != nil {
			return nil, err
		}
		out[pluginID] = string(data)
	}
	if req.ConfigJSON != "" || req.Config != nil {
		configJSON, err := adminPromotionConfigJSON(req)
		if err != nil {
			return nil, err
		}
		pluginID := req.PluginID
		if pluginID == "" && len(req.Bundle.Plugins) == 1 {
			pluginID = req.Bundle.Plugins[0].PluginID
		}
		if pluginID == "" {
			return nil, errors.New("plugin_id is required when applying a shared promotion config to a multi-plugin bundle")
		}
		if _, exists := out[pluginID]; exists {
			return nil, fmt.Errorf("config for plugin %s is specified more than once", pluginID)
		}
		out[pluginID] = configJSON
	}
	return out, nil
}

func handleAdminPluginInstrumentation(w http.ResponseWriter, r *http.Request) {
	session, ok := requireRole(w, r, adminRoleMember)
	if !ok {
		return
	}
	if pluginsManager == nil {
		adminhttp.WriteAPIError(w, http.StatusServiceUnavailable, "plugin manager is not initialized")
		return
	}
	switch r.Method {
	case http.MethodGet:
		records, err := pluginsManager.ListInstrumentation(r.Context())
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"instrumentation": records})
	case http.MethodPost:
		if session.Role != adminRoleAdmin {
			adminhttp.WriteAPIError(w, http.StatusForbidden, "forbidden")
			return
		}
		var req pluginmanager.InstrumentationRequest
		if !adminhttp.DecodeJSONRequest(w, r, &req) {
			return
		}
		record, err := pluginsManager.SaveInstrumentation(r.Context(), session.Username, req)
		if err != nil {
			adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		adminhttp.WriteJSON(w, http.StatusCreated, map[string]any{"instrumentation": record})
	default:
		adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
