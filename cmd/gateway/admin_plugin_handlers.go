package main

import (
	"encoding/json"
	"errors"
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
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"builds": builds})
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
		adminhttp.WriteJSON(w, http.StatusCreated, map[string]any{"build": build})
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
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"build": build})
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
	adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"build": build})
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
	artifactID, err := adminhttp.PathSegment(rawArtifactID)
	if err != nil {
		adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if r.Method != http.MethodGet {
		adminhttp.WriteAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
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
	adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"plugins": plugins})
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
		adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"plugin": plugin})
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
	if _, ok := requireRole(w, r, adminRoleMember); !ok {
		return
	}
	if pluginsManager == nil {
		adminhttp.WriteAPIError(w, http.StatusServiceUnavailable, "plugin manager is not initialized")
		return
	}
	adminhttp.WriteJSON(w, http.StatusOK, map[string]any{"dispatch_plan": pluginsManager.DispatchPlan(r.Context())})
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

func writePluginManagerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, pluginmanager.ErrArtifactNotFound), errors.Is(err, pluginmanager.ErrPluginNotFound):
		adminhttp.WriteAPIError(w, http.StatusNotFound, err.Error())
	default:
		adminhttp.WriteAPIError(w, http.StatusBadRequest, err.Error())
	}
}
