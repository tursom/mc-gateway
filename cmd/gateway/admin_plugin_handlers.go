package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
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

	return pluginsManager.UploadArtifact(r.Context(), pluginmanager.ArtifactUpload{
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
