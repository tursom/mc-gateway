// cmd/gateway/admin_api_test.go 包含用于约束 admin api 行为的测试。

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/tursom/mc-gateway/internal/pluginmanager"
)

func TestAdminSetupLoginAndPermissions(t *testing.T) {
	handler := newAdminTestHandler(t)

	resp := adminTestRequest(t, handler, http.MethodGet, "/admin/api/setup", "", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET setup status = %d, want %d", resp.Code, http.StatusOK)
	}
	if got := adminTestJSON(t, resp)["required"]; got != true {
		t.Fatalf("setup required = %v, want true", got)
	}

	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/setup", "", map[string]any{
		"username": "admin",
		"password": "secret",
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("POST setup status = %d, body=%s", resp.Code, resp.Body.String())
	}

	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/setup", "", map[string]any{
		"username": "admin2",
		"password": "secret",
	})
	if resp.Code != http.StatusConflict {
		t.Fatalf("repeat setup status = %d, want %d", resp.Code, http.StatusConflict)
	}

	adminToken := adminTestLogin(t, handler, "admin", "secret")
	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/status", adminToken, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("admin status = %d, body=%s", resp.Code, resp.Body.String())
	}

	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/users", adminToken, map[string]any{
		"username": "guest",
		"role":     "guest",
		"password": "guest-secret",
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("create guest status = %d, body=%s", resp.Code, resp.Body.String())
	}

	guestToken := adminTestLogin(t, handler, "guest", "guest-secret")
	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/routes", guestToken, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("guest routes status = %d, body=%s", resp.Code, resp.Body.String())
	}

	resp = adminTestRequest(t, handler, http.MethodPut, "/admin/api/routes/play.example", guestToken, map[string]any{
		"upstream": "127.0.0.1:25565",
		"enabled":  true,
	})
	if resp.Code != http.StatusForbidden {
		t.Fatalf("guest route write status = %d, want %d", resp.Code, http.StatusForbidden)
	}

	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/users", guestToken, nil)
	if resp.Code != http.StatusForbidden {
		t.Fatalf("guest users status = %d, want %d", resp.Code, http.StatusForbidden)
	}
}

func TestAdminRoutesRefreshSnapshot(t *testing.T) {
	handler := newAdminTestHandlerWithAdmin(t)
	token := adminTestLogin(t, handler, "admin", "secret")

	resp := adminTestRequest(t, handler, http.MethodPut, "/admin/api/routes/play.example", token, map[string]any{
		"upstream": "127.0.0.1:25565",
		"enabled":  true,
		"note":     "primary",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("route upsert status = %d, body=%s", resp.Code, resp.Body.String())
	}

	upstream, ok := lookupRoute("play.example")
	if !ok || upstream != "127.0.0.1:25565" {
		t.Fatalf("lookupRoute() = %q, %v; want route", upstream, ok)
	}

	resp = adminTestRequest(t, handler, http.MethodDelete, "/admin/api/routes/play.example", token, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("route delete status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if upstream, ok := lookupRoute("play.example"); ok || upstream != "" {
		t.Fatalf("lookupRoute() after delete = %q, %v; want miss", upstream, ok)
	}
}

func TestAdminCustomPathAndAPIPrefixFromEnv(t *testing.T) {
	t.Cleanup(saveGatewayState(t))

	t.Setenv(adminEnvDB, filepath.Join(t.TempDir(), "gateway.sqlite3"))
	t.Setenv(adminEnvPath, "/ops")
	t.Setenv(adminEnvAPIPrefix, "/ops/api")
	if err := loadConfig(); err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	handler := newGatewayHTTPHandler()

	resp := adminTestRequest(t, handler, http.MethodGet, "/ops", "", nil)
	if resp.Code != http.StatusMovedPermanently {
		t.Fatalf("admin path redirect status = %d, want %d", resp.Code, http.StatusMovedPermanently)
	}
	if got := resp.Header().Get("Location"); got != "/ops/" {
		t.Fatalf("admin path redirect location = %q, want /ops/", got)
	}

	resp = adminTestRequest(t, handler, http.MethodGet, "/ops/", "", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("custom admin page status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), `script src="config.js"`) {
		t.Fatalf("custom admin page does not reference runtime config: %s", resp.Body.String())
	}

	resp = adminTestRequest(t, handler, http.MethodGet, "/ops/config.js", "", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("custom admin config status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if strings.TrimSpace(resp.Body.String()) != `window.MCGatewayAdmin={"apiPrefix":"/ops/api"};` {
		t.Fatalf("custom admin config body = %q", resp.Body.String())
	}

	resp = adminTestRequest(t, handler, http.MethodGet, "/ops/api/setup", "", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("custom setup status = %d, body=%s", resp.Code, resp.Body.String())
	}

	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/setup", "", nil)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("default setup status = %d, want %d", resp.Code, http.StatusNotFound)
	}
}

func TestAdminServiceUpdateMarksRestartRequired(t *testing.T) {
	handler := newAdminTestHandlerWithAdmin(t)
	token := adminTestLogin(t, handler, "admin", "secret")

	resp := adminTestRequest(t, handler, http.MethodPut, "/admin/api/services/kcp", token, map[string]any{
		"enabled": true,
		"port":    25570,
		"options": map[string]any{
			"data_shards":   12,
			"parity_shards": 4,
		},
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("service update status = %d, body=%s", resp.Code, resp.Body.String())
	}

	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/services", token, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("services status = %d, body=%s", resp.Code, resp.Body.String())
	}
	body := adminTestJSON(t, resp)
	services := body["services"].([]any)
	var found map[string]any
	for _, item := range services {
		service := item.(map[string]any)
		if service["name"] == "kcp" {
			found = service
			break
		}
	}
	if found == nil {
		t.Fatal("kcp service not found")
	}
	if found["enabled"] != true || found["restart_required"] != true || found["running"] != false {
		t.Fatalf("kcp service = %#v, want enabled restart_required and not running", found)
	}
}

func TestAdminPluginServiceStatusReportsReservedModes(t *testing.T) {
	handler := newAdminTestHandlerWithAdmin(t)
	token := adminTestLogin(t, handler, "admin", "secret")

	resp := adminTestRequest(t, handler, http.MethodGet, "/admin/api/plugin-service", token, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("plugin service status = %d, body=%s", resp.Code, resp.Body.String())
	}
	status := adminTestJSON(t, resp)["plugin_service"].(map[string]any)
	service := status["service"].(map[string]any)
	if service["desired_mode"] != pluginmanager.PluginServiceModeInProcess ||
		service["active_mode"] != pluginmanager.PluginServiceModeInProcess ||
		service["data_plane_mode"] != pluginmanager.PluginServiceModeInProcess ||
		service["implemented_adapter"] != true ||
		service["restart_required"] != false {
		t.Fatalf("default plugin service = %#v, want implemented in-process data plane", service)
	}
	crashPolicy := service["crash_policy"].(map[string]any)
	if int(crashPolicy["backoff_seconds"].(float64)) != int(pluginmanager.DefaultPluginHostCrashBackoffSeconds) ||
		int(crashPolicy["max_crashes"].(float64)) != int(pluginmanager.DefaultPluginHostCrashMaxCrashes) ||
		int(crashPolicy["window_seconds"].(float64)) != int(pluginmanager.DefaultPluginHostCrashWindowSeconds) {
		t.Fatalf("default crash policy = %#v, want default policy", crashPolicy)
	}
	processMode := findAdminServiceModeFeature(t, status["service_modes"].([]any), pluginmanager.PluginServiceModeGoPluginProcess)
	if processMode["implemented"] != true ||
		processMode["data_plane"] != true ||
		processMode["maturity"] != pluginmanager.FeatureMaturityPartial ||
		!strings.Contains(processMode["unsupported_reason"].(string), "protocol-proxy drain-only") ||
		!strings.Contains(processMode["unsupported_reason"].(string), "per-node crash isolation") ||
		strings.Contains(processMode["unsupported_reason"].(string), "cross-node crash policy coordination are not implemented") {
		t.Fatalf("go-plugin-process mode = %#v, want partial process data plane", processMode)
	}
	processAdapter := findAdminRuntimeAdapterStatus(t, status["runtime_adapters"].([]any), pluginmanager.PluginServiceModeGoPluginProcess, pluginmanager.RuntimeGoPlugin)
	if processAdapter["implemented"] != true ||
		processAdapter["maturity"] != pluginmanager.FeatureMaturityPartial ||
		processAdapter["data_plane"] != true ||
		processAdapter["lifecycle"] != true ||
		!strings.Contains(processAdapter["unsupported_reason"].(string), "fd-live") {
		t.Fatalf("go-plugin-process adapter = %#v, want partial process data plane", processAdapter)
	}
	wasmSandboxAdapter := findAdminRuntimeAdapterStatus(t, status["runtime_adapters"].([]any), pluginmanager.PluginServiceModeSandboxProcess, pluginmanager.RuntimeWASM)
	expectedWASMAdapter := findPluginRuntimeAdapterStatus(pluginmanager.RuntimeAdapterFactoryStatuses(), pluginmanager.PluginServiceModeSandboxProcess, pluginmanager.RuntimeWASM)
	if wasmSandboxAdapter["implemented"] != expectedWASMAdapter.Implemented ||
		wasmSandboxAdapter["maturity"] != expectedWASMAdapter.Maturity ||
		wasmSandboxAdapter["data_plane"] != expectedWASMAdapter.DataPlane ||
		wasmSandboxAdapter["lifecycle"] != expectedWASMAdapter.Lifecycle ||
		wasmSandboxAdapter["requires_restart"] != expectedWASMAdapter.RequiresRestart ||
		wasmSandboxAdapter["adapter"] != expectedWASMAdapter.Adapter ||
		wasmSandboxAdapter["control_channel"] != expectedWASMAdapter.ControlChannel ||
		wasmSandboxAdapter["unsupported_reason"] != expectedWASMAdapter.UnsupportedReason {
		t.Fatalf("wasm sandbox adapter = %#v, want shared adapter fact source %+v", wasmSandboxAdapter, expectedWASMAdapter)
	}
	nodes := status["nodes"].([]any)
	if len(nodes) != 1 {
		t.Fatalf("plugin service nodes = %#v, want one local node", nodes)
	}
	node := nodes[0].(map[string]any)
	if node["node_id"] == "" ||
		node["status"] != pluginmanager.PluginNodeStatusOnline ||
		node["data_plane_mode"] != pluginmanager.PluginServiceModeInProcess ||
		node["stale"] != false {
		t.Fatalf("plugin service node = %#v, want fresh in-process node", node)
	}

	resp = adminTestRequest(t, handler, http.MethodPut, "/admin/api/plugin-service", token, map[string]any{
		"desired_mode": pluginmanager.PluginServiceModeGoPluginProcess,
		"crash_policy": map[string]any{"backoff_seconds": 7, "max_crashes": 3, "window_seconds": 120},
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("plugin service desired update = %d, body=%s", resp.Code, resp.Body.String())
	}
	status = adminTestJSON(t, resp)["plugin_service"].(map[string]any)
	service = status["service"].(map[string]any)
	if service["desired_mode"] != pluginmanager.PluginServiceModeGoPluginProcess ||
		service["active_mode"] != pluginmanager.PluginServiceModeInProcess ||
		service["data_plane_mode"] != pluginmanager.PluginServiceModeInProcess ||
		service["restart_required"] != true {
		t.Fatalf("plugin service after desired update = %#v, want pending process desired mode", service)
	}
	crashPolicy = service["crash_policy"].(map[string]any)
	if int(crashPolicy["backoff_seconds"].(float64)) != 7 ||
		int(crashPolicy["max_crashes"].(float64)) != 3 ||
		int(crashPolicy["window_seconds"].(float64)) != 120 {
		t.Fatalf("plugin service crash policy = %#v, want updated policy", crashPolicy)
	}

	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugin-service", token, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("plugin service apply = %d, body=%s", resp.Code, resp.Body.String())
	}
	status = adminTestJSON(t, resp)["plugin_service"].(map[string]any)
	service = status["service"].(map[string]any)
	if service["desired_mode"] != pluginmanager.PluginServiceModeGoPluginProcess ||
		service["active_mode"] != pluginmanager.PluginServiceModeGoPluginProcess ||
		service["data_plane_mode"] != pluginmanager.PluginServiceModeGoPluginProcess ||
		service["implemented_adapter"] != true ||
		service["restart_required"] != false {
		t.Fatalf("plugin service after apply = %#v, want process data plane", service)
	}
}

func TestAdminPluginFeaturesExposeSharedFactSource(t *testing.T) {
	handler := newAdminTestHandlerWithAdmin(t)
	token := adminTestLogin(t, handler, "admin", "secret")

	resp := adminTestRequest(t, handler, http.MethodGet, "/admin/api/plugin-features", token, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("plugin features status = %d, body=%s", resp.Code, resp.Body.String())
	}
	features := adminTestJSON(t, resp)["plugin_features"].(map[string]any)
	if features["schema_version"] != pluginmanager.SchemaVersion ||
		features["api_version"] != pluginmanager.APIVersion {
		t.Fatalf("plugin feature versions = %#v, want current schema/api", features)
	}
	runtimeTypes := features["runtime_types"].([]any)
	if len(runtimeTypes) != len(pluginmanager.RuntimeTypeFeatures()) {
		t.Fatalf("runtime_types = %#v, want shared runtime feature matrix", runtimeTypes)
	}
	wasm := findAdminRuntimeFeature(t, runtimeTypes, pluginmanager.RuntimeWASM)
	expectedWASM := pluginmanager.RuntimeTypeFeature(pluginmanager.RuntimeWASM)
	if wasm["implemented"] != expectedWASM.Implemented ||
		wasm["maturity"] != expectedWASM.Maturity ||
		wasm["data_plane"] != expectedWASM.DataPlane ||
		wasm["requires_restart"] != expectedWASM.RequiresRestart ||
		wasm["unsupported_reason"] != expectedWASM.UnsupportedReason ||
		wasm["entry"] != expectedWASM.Entry {
		t.Fatalf("wasm feature = %#v, want shared runtime fact source %+v", wasm, expectedWASM)
	}
	extensionPoints := features["extension_points"].([]any)
	if len(extensionPoints) != len(pluginmanager.ExtensionPointFeatures()) {
		t.Fatalf("extension_points = %#v, want shared extension feature matrix", extensionPoints)
	}
	adminAuth := findAdminExtensionPointFeature(t, extensionPoints, pluginmanager.ExtensionAdminAuthProvider)
	if adminAuth["implemented"] != false ||
		adminAuth["maturity"] != pluginmanager.FeatureMaturityReserved ||
		adminAuth["data_plane"] != false ||
		!strings.Contains(adminAuth["unsupported_reason"].(string), "break-glass") {
		t.Fatalf("admin auth provider = %#v, want reserved local break-glass boundary", adminAuth)
	}
	ingress := findAdminExtensionPointFeature(t, extensionPoints, pluginmanager.ExtensionIngressService)
	expectedIngress := findPluginExtensionPointFeature(pluginmanager.ExtensionPointFeatures(), pluginmanager.ExtensionIngressService)
	if ingress["key"] != expectedIngress.Key ||
		ingress["type"] != expectedIngress.Type ||
		ingress["implemented"] != expectedIngress.Implemented ||
		ingress["maturity"] != expectedIngress.Maturity ||
		ingress["data_plane"] != expectedIngress.DataPlane ||
		ingress["requires_restart"] != expectedIngress.RequiresRestart ||
		ingress["unsupported_reason"] != expectedIngress.UnsupportedReason {
		t.Fatalf("ingress service = %#v, want shared extension fact source %+v", ingress, expectedIngress)
	}
}

func TestAdminPluginRuntimePanelStaticContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("admin_static", "js", "views", "plugins.js"))
	if err != nil {
		t.Fatalf("read plugins.js error = %v", err)
	}
	content := string(data)
	for _, want := range []string{
		`api("/plugin-features")`,
		"Desired mode",
		"Active mode",
		"Effective data plane",
		"Adapter",
		"Crash policy",
		"Restart",
		"desired pending",
		"Current data plane remains",
		"admin.auth.provider/v1",
		"ingress.service/v1",
		"Extension",
		"Data plane",
		"no data plane",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("admin plugin runtime panel static JS missing %q", want)
		}
	}
}

func TestAdminUserPatchInvalidatesExistingSession(t *testing.T) {
	handler := newAdminTestHandlerWithAdmin(t)
	adminToken := adminTestLogin(t, handler, "admin", "secret")

	resp := adminTestRequest(t, handler, http.MethodPost, "/admin/api/users", adminToken, map[string]any{
		"username": "member",
		"role":     "member",
		"password": "member-secret",
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("create member status = %d, body=%s", resp.Code, resp.Body.String())
	}

	memberToken := adminTestLogin(t, handler, "member", "member-secret")
	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/status", memberToken, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("member status before patch = %d, body=%s", resp.Code, resp.Body.String())
	}

	resp = adminTestRequest(t, handler, http.MethodPatch, "/admin/api/users/member", adminToken, map[string]any{
		"role": "guest",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("patch member status = %d, body=%s", resp.Code, resp.Body.String())
	}

	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/status", memberToken, nil)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("old member token status = %d, want %d", resp.Code, http.StatusUnauthorized)
	}
}

func TestAdminPluginPhase4API(t *testing.T) {
	handler := newAdminTestHandlerWithAdmin(t)
	pluginsManager = pluginmanager.New(pluginmanager.Options{
		DB:           adminDB,
		ArtifactRoot: filepath.Join(filepath.Dir(adminDBPath), "plugins", "artifacts"),
		Adapter:      gatewayTestPluginAdapter{},
	})
	adminToken := adminTestLogin(t, handler, "admin", "secret")
	resp := adminTestRequest(t, handler, http.MethodPost, "/admin/api/users", adminToken, map[string]any{
		"username": "member",
		"role":     "member",
		"password": "member-secret",
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("create member status = %d, body=%s", resp.Code, resp.Body.String())
	}
	memberToken := adminTestLogin(t, handler, "member", "member-secret")

	artifact := uploadGatewayPhase4Artifact(t, "phase4-plugin")
	if _, err := pluginsManager.SetDesired(context.Background(), "admin", "phase4-plugin", artifact.ID, pluginmanager.DesiredEnabled, `{"token":"old","host":"a"}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := pluginsManager.Enable(context.Background(), "admin", "phase4-plugin"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}

	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/plugins", memberToken, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("member plugins list status = %d, body=%s", resp.Code, resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/plugins/phase4-plugin", memberToken, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("member plugin detail status = %d, body=%s", resp.Code, resp.Body.String())
	}
	detail := adminTestJSON(t, resp)["plugin"].(map[string]any)
	rollout := detail["rollout_status"].(map[string]any)
	if rollout["ok"] != true ||
		rollout["partial_failure"] != false ||
		int(rollout["nodes_ready"].(float64)) != 1 ||
		len(detail["node_runtime_states"].([]any)) != 1 {
		t.Fatalf("plugin rollout detail = %#v node_states=%#v, want one ready node", rollout, detail["node_runtime_states"])
	}

	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugins/phase4-plugin/secrets", memberToken, map[string]any{
		"name":  "api_token",
		"value": "member-secret-value",
	})
	if resp.Code != http.StatusForbidden {
		t.Fatalf("member secret write status = %d, want forbidden; body=%s", resp.Code, resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugins/phase4-plugin/config/dry-run", memberToken, map[string]any{
		"artifact_id": artifact.ID,
		"config_json": `{"token":"member-dry-run-secret","host":"member"}`,
	})
	if resp.Code != http.StatusForbidden {
		t.Fatalf("member config dry-run status = %d, want forbidden; body=%s", resp.Code, resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodPut, "/admin/api/plugins/phase4-plugin/config", memberToken, map[string]any{
		"artifact_id": artifact.ID,
		"config_json": `{"token":"member-config-secret","host":"member"}`,
	})
	if resp.Code != http.StatusForbidden {
		t.Fatalf("member config write status = %d, want forbidden; body=%s", resp.Code, resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugins/phase4-plugin/rollback/config", memberToken, map[string]any{
		"snapshot_id": 1,
	})
	if resp.Code != http.StatusForbidden {
		t.Fatalf("member rollback status = %d, want forbidden; body=%s", resp.Code, resp.Body.String())
	}

	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugins/phase4-plugin/secrets", adminToken, map[string]any{
		"artifact_id":     artifact.ID,
		"name":            "api_token",
		"value":           "super-secret-value",
		"reload_required": true,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("secret write status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "super-secret-value") {
		t.Fatalf("secret response leaked value: %s", resp.Body.String())
	}
	secretBody := adminTestJSON(t, resp)["secret"].(map[string]any)
	if int(secretBody["current_version"].(float64)) != 1 || int(secretBody["previous_version"].(float64)) != 0 {
		t.Fatalf("secret response = %#v, want current 1 previous 0", secretBody)
	}
	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugins/phase4-plugin/secrets", adminToken, map[string]any{
		"artifact_id": artifact.ID,
		"name":        "api_token",
		"value":       "rotated-secret-value",
		"hot_reload":  true,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("secret rotation status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "rotated-secret-value") || strings.Contains(resp.Body.String(), "super-secret-value") {
		t.Fatalf("secret rotation response leaked value: %s", resp.Body.String())
	}
	secretBody = adminTestJSON(t, resp)["secret"].(map[string]any)
	if int(secretBody["current_version"].(float64)) != 2 || int(secretBody["previous_version"].(float64)) != 1 {
		t.Fatalf("rotated secret response = %#v, want current 2 previous 1", secretBody)
	}

	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugins/phase4-plugin/config/dry-run", adminToken, map[string]any{
		"artifact_id": artifact.ID,
		"config_json": `{"token":"new","host":"b"}`,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("config dry-run status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "new") || strings.Contains(resp.Body.String(), "old") {
		t.Fatalf("dry-run leaked sensitive value: %s", resp.Body.String())
	}

	resp = adminTestRequest(t, handler, http.MethodPut, "/admin/api/plugins/phase4-plugin/config", adminToken, map[string]any{
		"artifact_id": artifact.ID,
		"config_json": `{"token":"new","host":"b"}`,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("config update status = %d, body=%s", resp.Code, resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/plugins/phase4-plugin/config/snapshots", adminToken, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("snapshots status = %d, body=%s", resp.Code, resp.Body.String())
	}
	snapshotID := firstSnapshotID(t, resp)
	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/plugins/phase4-plugin/config/snapshots/"+strconv.FormatInt(snapshotID, 10)+"/diff", adminToken, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("snapshot diff status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "new") || strings.Contains(resp.Body.String(), "old") {
		t.Fatalf("snapshot diff leaked sensitive value: %s", resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugins/phase4-plugin/rollback/config", adminToken, map[string]any{
		"snapshot_id": snapshotID,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("config rollback status = %d, body=%s", resp.Code, resp.Body.String())
	}
	for _, forbidden := range []string{"super-secret-value", "rotated-secret-value", `"token":"new"`, `"token": "new"`, `"token":"old"`, `"token": "old"`} {
		if strings.Contains(resp.Body.String(), forbidden) {
			t.Fatalf("config rollback response leaked %q: %s", forbidden, resp.Body.String())
		}
	}

	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/audit-logs", adminToken, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("audit status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "plugin_secret_update") ||
		!strings.Contains(resp.Body.String(), "plugin_config_update") ||
		!strings.Contains(resp.Body.String(), "plugin_rollback_config") ||
		!strings.Contains(resp.Body.String(), "desired_generation") ||
		!strings.Contains(resp.Body.String(), "active_changed") {
		t.Fatalf("audit body missing write-operation explanation fields: %s", resp.Body.String())
	}
	for _, forbidden := range []string{"super-secret-value", "rotated-secret-value", "member-secret-value", "member-dry-run-secret", "member-config-secret"} {
		if strings.Contains(resp.Body.String(), forbidden) {
			t.Fatalf("audit leaked %q: %s", forbidden, resp.Body.String())
		}
	}
}

func TestAdminPluginConfigDryRunFailuresPreserveStateAndAuditRedacts(t *testing.T) {
	handler := newAdminTestHandlerWithAdmin(t)
	adapter := &adminDryRunFailureAdapter{}
	pluginsManager = pluginmanager.New(pluginmanager.Options{
		DB:           adminDB,
		ArtifactRoot: filepath.Join(filepath.Dir(adminDBPath), "plugins", "artifacts"),
		Adapter:      adapter,
	})
	adminToken := adminTestLogin(t, handler, "admin", "secret")

	artifact := uploadGatewayPhase4Artifact(t, "dry-run-failure-plugin")
	if _, err := pluginsManager.SetDesired(context.Background(), "admin", "dry-run-failure-plugin", artifact.ID, pluginmanager.DesiredEnabled, `{"token":"old-secret","host":"a"}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := pluginsManager.Enable(context.Background(), "admin", "dry-run-failure-plugin"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	before, err := pluginsManager.Plugin(context.Background(), "dry-run-failure-plugin")
	if err != nil {
		t.Fatalf("Plugin(before) error = %v", err)
	}
	if before.ActiveArtifactID != artifact.ID || before.DesiredArtifactID != artifact.ID {
		t.Fatalf("plugin before dry-run failures = %+v, want active desired artifact %s", before, artifact.ID)
	}
	assertUnchanged := func(label string) {
		t.Helper()
		after, err := pluginsManager.Plugin(context.Background(), "dry-run-failure-plugin")
		if err != nil {
			t.Fatalf("Plugin(after %s) error = %v", label, err)
		}
		if after.ActiveArtifactID != before.ActiveArtifactID ||
			after.DesiredArtifactID != before.DesiredArtifactID ||
			after.DesiredGeneration != before.DesiredGeneration ||
			after.ConfigJSON != before.ConfigJSON {
			t.Fatalf("plugin after %s = %+v, want state unchanged from %+v", label, after, before)
		}
	}

	for _, tc := range []struct {
		name string
		body map[string]any
		want string
	}{
		{
			name: "invalid-json",
			body: map[string]any{
				"artifact_id": artifact.ID,
				"config_json": `{"token":"bad-json-secret"`,
			},
			want: "valid JSON",
		},
		{
			name: "schema-mismatch",
			body: map[string]any{
				"artifact_id": artifact.ID,
				"config_json": `{"token":"bad-schema-secret","host":7}`,
			},
			want: "$.host must be string",
		},
		{
			name: "missing-secret-ref",
			body: map[string]any{
				"artifact_id": artifact.ID,
				"config_json": `{"token":"missing-secret-config","host":"b","api_secret_ref":"api_token"}`,
			},
			want: "missing configured secret",
		},
	} {
		resp := adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugins/dry-run-failure-plugin/config/dry-run", adminToken, tc.body)
		if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), tc.want) {
			t.Fatalf("%s dry-run status = %d, body=%s; want bad request containing %q", tc.name, resp.Code, resp.Body.String(), tc.want)
		}
		assertUnchanged(tc.name)
	}

	if _, err := pluginsManager.UpsertSecret(context.Background(), "admin", "dry-run-failure-plugin", artifact.ID, "api_token", "runtime-secret-value", true, false); err != nil {
		t.Fatalf("UpsertSecret() error = %v", err)
	}
	adapter.err = errors.New("reload rejected for runtime config")
	resp := adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugins/dry-run-failure-plugin/config/dry-run", adminToken, map[string]any{
		"artifact_id": artifact.ID,
		"config_json": `{"token":"runtime-secret-config","host":"b","api_secret_ref":"api_token"}`,
	})
	if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), "reload rejected") {
		t.Fatalf("runtime dry-run status = %d, body=%s; want reload rejection", resp.Code, resp.Body.String())
	}
	assertUnchanged("runtime-dry-run")

	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/audit-logs", adminToken, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("audit status = %d, body=%s", resp.Code, resp.Body.String())
	}
	failedDryRuns := 0
	for _, raw := range adminTestJSON(t, resp)["audit_logs"].([]any) {
		record := raw.(map[string]any)
		if record["action"] == "plugin_config_dry_run" &&
			record["target_id"] == "dry-run-failure-plugin" &&
			record["success"] == false {
			failedDryRuns++
		}
	}
	if failedDryRuns != 4 {
		t.Fatalf("failed dry-run audit records = %d, want 4; body=%s", failedDryRuns, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "plugin_config_dry_run") {
		t.Fatalf("audit body missing plugin_config_dry_run: %s", resp.Body.String())
	}
	for _, forbidden := range []string{"old-secret", "bad-json-secret", "bad-schema-secret", "missing-secret-config", "runtime-secret-config", "runtime-secret-value"} {
		if strings.Contains(resp.Body.String(), forbidden) {
			t.Fatalf("audit leaked %q: %s", forbidden, resp.Body.String())
		}
	}
}

func TestAdminPluginPhase5GovernanceAPI(t *testing.T) {
	handler := newAdminTestHandlerWithAdmin(t)
	pluginsManager = pluginmanager.New(pluginmanager.Options{
		DB:           adminDB,
		ArtifactRoot: filepath.Join(filepath.Dir(adminDBPath), "plugins", "artifacts"),
		Adapter:      gatewayTestPluginAdapter{},
	})
	adminToken := adminTestLogin(t, handler, "admin", "secret")
	resp := adminTestRequest(t, handler, http.MethodPost, "/admin/api/users", adminToken, map[string]any{
		"username": "member",
		"role":     "member",
		"password": "member-secret",
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("create member status = %d, body=%s", resp.Code, resp.Body.String())
	}
	memberToken := adminTestLogin(t, handler, "member", "member-secret")

	artifact := uploadGatewayPhase5ProtocolProxyArtifact(t, "phase5-proxy")
	if _, err := pluginsManager.SetDesired(context.Background(), "admin", "phase5-proxy", artifact.ID, pluginmanager.DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/plugins/phase5-proxy/governance?profile=prod", memberToken, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("member governance status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "review_required") {
		t.Fatalf("governance status body = %s, want review_required", resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugins/phase5-proxy/governance/review", memberToken, map[string]any{
		"artifact_id": artifact.ID,
		"profile":     pluginmanager.PolicyProfileProd,
	})
	if resp.Code != http.StatusForbidden {
		t.Fatalf("member review write status = %d, want forbidden; body=%s", resp.Code, resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugins/phase5-proxy/governance/review", adminToken, map[string]any{
		"artifact_id": artifact.ID,
		"profile":     pluginmanager.PolicyProfileProd,
		"decision":    pluginmanager.ReviewDecisionApproved,
		"notes":       "phase 5 approval",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("admin review write status = %d, body=%s", resp.Code, resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugins/phase5-proxy/governance/preflight", adminToken, map[string]any{
		"artifact_id": artifact.ID,
		"profile":     pluginmanager.PolicyProfileProd,
		"action":      pluginmanager.GovernanceActionEnable,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("preflight status = %d, body=%s", resp.Code, resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugins/phase5-proxy/governance/benchmark", adminToken, map[string]any{
		"artifact_id":           artifact.ID,
		"profile":               pluginmanager.PolicyProfileProd,
		"benchmark_profile":     "release",
		"p95_ms":                10,
		"p99_ms":                20,
		"baseline_diff":         0.25,
		"active_proxy_capacity": 100,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("benchmark status = %d, body=%s", resp.Code, resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugins/phase5-proxy/governance/override", adminToken, map[string]any{
		"artifact_id": artifact.ID,
		"profile":     pluginmanager.PolicyProfileProd,
		"action":      pluginmanager.GovernanceActionEnable,
		"reason":      "accepted warning for rollout",
		"ttl_seconds": 3600,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("override status = %d, body=%s", resp.Code, resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugin-advisories", adminToken, map[string]any{
		"advisory_id":     "MCG-2026-ADMIN",
		"status":          pluginmanager.AdvisoryStatusRevoked,
		"action":          pluginmanager.AdvisoryActionRevoke,
		"artifact_sha256": artifact.SHA256,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("advisory status = %d, body=%s", resp.Code, resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/plugin-advisories?plugin_id=phase5-proxy", memberToken, nil)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "MCG-2026-ADMIN") {
		t.Fatalf("member advisory read status = %d, body=%s", resp.Code, resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugin-advisories", adminToken, map[string]any{
		"source": "admin-feed",
		"advisories": []map[string]any{{
			"advisory_id":   "MCG-2026-FEED",
			"status":        pluginmanager.AdvisoryStatusActive,
			"action":        pluginmanager.AdvisoryActionDenylist,
			"plugin_id":     "phase5-proxy",
			"version_range": "*",
		}},
	})
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"imported":1`) || !strings.Contains(resp.Body.String(), "MCG-2026-FEED") {
		t.Fatalf("advisory feed status = %d, body=%s", resp.Code, resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugin-advisories", adminToken, map[string]any{
		"rescan":      true,
		"plugin_id":   "phase5-proxy",
		"artifact_id": artifact.ID,
	})
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"blocking"`) || !strings.Contains(resp.Body.String(), "MCG-2026-ADMIN") {
		t.Fatalf("advisory rescan status = %d, body=%s", resp.Code, resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugin-vulnerabilities", adminToken, map[string]any{
		"vulnerability_id": "CVE-2026-ADMIN",
		"status":           pluginmanager.AdvisoryStatusActive,
		"action":           pluginmanager.AdvisoryActionDenylist,
		"package_name":     "example.com/bad",
		"version_range":    "<2.0.0",
		"severity":         pluginmanager.VulnerabilitySeverityCritical,
		"fixed_version":    "v2.0.0",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("vulnerability status = %d, body=%s", resp.Code, resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/plugin-vulnerabilities?package_name=example.com/bad", memberToken, nil)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "CVE-2026-ADMIN") {
		t.Fatalf("member vulnerability read status = %d, body=%s", resp.Code, resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugin-vulnerabilities", adminToken, map[string]any{
		"source": "admin-vuln-db",
		"vulnerabilities": []map[string]any{{
			"vulnerability_id": "CVE-2026-FEED",
			"status":           pluginmanager.AdvisoryStatusActive,
			"action":           pluginmanager.AdvisoryActionDenylist,
			"package_name":     "example.com/bad",
			"version_range":    "<2.0.0",
			"severity":         pluginmanager.VulnerabilitySeverityHigh,
		}},
	})
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"imported":1`) || !strings.Contains(resp.Body.String(), "CVE-2026-FEED") {
		t.Fatalf("vulnerability database status = %d, body=%s", resp.Code, resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugin-vulnerabilities", adminToken, map[string]any{
		"rescan":      true,
		"plugin_id":   "phase5-proxy",
		"artifact_id": artifact.ID,
	})
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"blocking"`) || !strings.Contains(resp.Body.String(), "CVE-2026-ADMIN") {
		t.Fatalf("vulnerability scan status = %d, body=%s", resp.Code, resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/audit-logs", adminToken, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("audit status = %d, body=%s", resp.Code, resp.Body.String())
	}
	for _, action := range []string{"plugin_governance_review", "plugin_governance_override", "plugin_governance_advisory", "plugin_governance_advisory_feed", "plugin_governance_advisory_rescan", "plugin_governance_vulnerability", "plugin_governance_vulnerability_db", "plugin_governance_vulnerability_scan"} {
		if !strings.Contains(resp.Body.String(), action) {
			t.Fatalf("audit body missing %s: %s", action, resp.Body.String())
		}
	}
}

func TestAdminPluginExternalDependencyHealthCheckAPI(t *testing.T) {
	handler := newAdminTestHandlerWithAdmin(t)
	pluginsManager = pluginmanager.New(pluginmanager.Options{
		DB:           adminDB,
		ArtifactRoot: filepath.Join(filepath.Dir(adminDBPath), "plugins", "artifacts"),
		Adapter:      gatewayTestPluginAdapter{},
	})
	adminToken := adminTestLogin(t, handler, "admin", "secret")
	dependency := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer dependency.Close()

	artifact := uploadGatewayExternalDependencyArtifact(t, "external-plugin", dependency.URL)
	if _, err := pluginsManager.SetDesired(context.Background(), "admin", "external-plugin", artifact.ID, pluginmanager.DesiredDisabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	resp := adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugins/external-plugin/operations/external/session/health-check", adminToken, nil)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"ok":true`) || !strings.Contains(resp.Body.String(), `"name":"session"`) {
		t.Fatalf("external health status = %d, body=%s", resp.Code, resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/audit-logs", adminToken, nil)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "plugin_external_dependency_health_check") {
		t.Fatalf("audit status = %d, body=%s", resp.Code, resp.Body.String())
	}
}

func TestAdminPluginInstrumentationReleaseEvidenceAPI(t *testing.T) {
	handler := newAdminTestHandlerWithAdmin(t)
	pluginsManager = pluginmanager.New(pluginmanager.Options{
		DB:           adminDB,
		ArtifactRoot: filepath.Join(filepath.Dir(adminDBPath), "plugins", "artifacts"),
		Adapter:      gatewayTestPluginAdapter{},
	})
	adminToken := adminTestLogin(t, handler, "admin", "secret")
	resp := adminTestRequest(t, handler, http.MethodPost, "/admin/api/users", adminToken, map[string]any{
		"username": "member",
		"role":     "member",
		"password": "member-secret",
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("create member status = %d, body=%s", resp.Code, resp.Body.String())
	}
	memberToken := adminTestLogin(t, handler, "member", "member-secret")

	req := map[string]any{
		"name":                  "official-trace",
		"version":               "0.1.0",
		"profile":               "official",
		"generated_diff_hash":   "diff1234567890",
		"gateway_binary_sha256": "gateway1234567890",
		"ci_artifact_sha256":    "artifact1234567890",
		"provenance": map[string]any{
			"builder":     "ci",
			"digest":      "sha256:abc",
			"policy_hash": "policyabc123",
			"governance": map[string]any{
				"ok":          true,
				"action":      pluginmanager.GovernanceActionPromotion,
				"profile":     pluginmanager.PolicyProfileProd,
				"policy_hash": "policyabc123",
			},
		},
		"conformance":      map[string]any{"ok": true},
		"benchmark":        map[string]any{"ok": true},
		"smoke":            map[string]any{"ok": true},
		"runbook_rollback": "Deploy the previous gateway image.",
	}
	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugin-instrumentation", memberToken, req)
	if resp.Code != http.StatusForbidden {
		t.Fatalf("member instrumentation create status = %d, want %d, body=%s", resp.Code, http.StatusForbidden, resp.Body.String())
	}

	failedReq := map[string]any{}
	for key, value := range req {
		failedReq[key] = value
	}
	failedReq["conformance"] = map[string]any{"ok": false}
	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugin-instrumentation", adminToken, failedReq)
	if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), "conformance") {
		t.Fatalf("failed conformance status = %d, body=%s", resp.Code, resp.Body.String())
	}

	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugin-instrumentation", adminToken, req)
	if resp.Code != http.StatusCreated {
		t.Fatalf("instrumentation create status = %d, body=%s", resp.Code, resp.Body.String())
	}
	body := adminTestJSON(t, resp)
	instrumentation, ok := body["instrumentation"].(map[string]any)
	if !ok {
		t.Fatalf("instrumentation body = %#v", body["instrumentation"])
	}
	if instrumentation["status"] != pluginmanager.InstrumentationStatusAvailable {
		t.Fatalf("instrumentation status = %#v, want available", instrumentation["status"])
	}
	if instrumentation["runbook_rollback"] != "Deploy the previous gateway image." {
		t.Fatalf("runbook_rollback = %#v", instrumentation["runbook_rollback"])
	}
	if instrumentation["gateway_binary_sha256"] != "gateway1234567890" || instrumentation["ci_artifact_sha256"] != "artifact1234567890" {
		t.Fatalf("instrumentation binding = gateway:%#v ci:%#v", instrumentation["gateway_binary_sha256"], instrumentation["ci_artifact_sha256"])
	}
	for _, field := range []string{"conformance_json", "benchmark_json", "smoke_json"} {
		raw, ok := instrumentation[field].(string)
		if !ok || raw == "" {
			t.Fatalf("%s = %#v, want JSON evidence string", field, instrumentation[field])
		}
		var evidence map[string]any
		if err := json.Unmarshal([]byte(raw), &evidence); err != nil {
			t.Fatalf("Unmarshal(%s=%q) error = %v", field, raw, err)
		}
		if passed, _ := evidence["ok"].(bool); !passed {
			t.Fatalf("%s evidence = %#v, want ok=true", field, evidence)
		}
	}

	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/plugin-instrumentation", memberToken, nil)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "official-trace") || !strings.Contains(resp.Body.String(), "Deploy the previous gateway image.") {
		t.Fatalf("member instrumentation list status = %d, body=%s", resp.Code, resp.Body.String())
	}
}

func TestAdminPluginDispatchPlanActions(t *testing.T) {
	handler := newAdminTestHandlerWithAdmin(t)
	pluginsManager = pluginmanager.New(pluginmanager.Options{
		DB:           adminDB,
		ArtifactRoot: filepath.Join(filepath.Dir(adminDBPath), "plugins", "artifacts"),
		Adapter:      gatewayTestPluginAdapter{},
	})
	adminToken := adminTestLogin(t, handler, "admin", "secret")

	resp := adminTestRequest(t, handler, http.MethodGet, "/admin/api/plugins/dispatch-plan", adminToken, nil)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "dispatch_plan") {
		t.Fatalf("dispatch plan status = %d, body=%s", resp.Code, resp.Body.String())
	}
	for _, tc := range []struct {
		action string
		field  string
	}{
		{action: "refresh-routes", field: "route_cache"},
		{action: "replay-subscribers", field: "replayed"},
		{action: "drop-subscriber-dead-letter", field: "dropped"},
	} {
		resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugins/dispatch-plan", adminToken, map[string]any{"action": tc.action})
		if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), tc.field) {
			t.Fatalf("dispatch action %s status = %d, body=%s", tc.action, resp.Code, resp.Body.String())
		}
	}
	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugins/dispatch-plan", adminToken, map[string]any{"action": "unknown"})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("unknown dispatch action status = %d, body=%s", resp.Code, resp.Body.String())
	}
	resp = adminTestRequest(t, handler, http.MethodGet, "/admin/api/audit-logs", adminToken, nil)
	if resp.Code != http.StatusOK ||
		!strings.Contains(resp.Body.String(), "plugin_route_provider_refresh") ||
		!strings.Contains(resp.Body.String(), "plugin_event_subscriber_replay") ||
		!strings.Contains(resp.Body.String(), "plugin_event_subscriber_drop") {
		t.Fatalf("dispatch audit status = %d, body=%s", resp.Code, resp.Body.String())
	}
}

func TestAdminPluginPromotionAPI(t *testing.T) {
	handler := newAdminTestHandlerWithAdmin(t)
	pluginsManager = pluginmanager.New(pluginmanager.Options{
		DB:           adminDB,
		ArtifactRoot: filepath.Join(filepath.Dir(adminDBPath), "plugins", "artifacts"),
		Adapter:      gatewayTestPluginAdapter{},
	})
	adminToken := adminTestLogin(t, handler, "admin", "secret")
	artifact := uploadGatewayPhase4Artifact(t, "promotion-api-plugin")

	resp := adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugin-promotions", adminToken, map[string]any{
		"action":      "export",
		"plugin_id":   artifact.PluginID,
		"artifact_id": artifact.ID,
		"profile":     pluginmanager.PolicyProfileDev,
		"config_json": `{"token":"promotion-secret","host":"blue"}`,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("promotion export status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "promotion-secret") || strings.Contains(resp.Body.String(), "blue") {
		t.Fatalf("promotion export leaked config value: %s", resp.Body.String())
	}
	var exportResp struct {
		Bundle pluginmanager.PromotionBundle `json:"bundle"`
		Report pluginmanager.PromotionReport `json:"report"`
		DryRun bool                          `json:"dry_run"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &exportResp); err != nil {
		t.Fatalf("Unmarshal(export) error = %v\n%s", err, resp.Body.String())
	}
	if exportResp.Bundle.BundleID == "" || len(exportResp.Bundle.Plugins) != 1 || exportResp.Report.OK || exportResp.Report.Status != pluginmanager.PromotionStatusBlocked || !exportResp.DryRun {
		t.Fatalf("export response = %+v, want dry-run bundle report blocked until target secret mapping is provided", exportResp)
	}
	if exportResp.Bundle.Plugins[0].DesiredState != pluginmanager.DesiredDisabled || exportResp.Bundle.Plugins[0].ConfigHash == "" {
		t.Fatalf("export plugin = %+v, want disabled desired state with config hash", exportResp.Bundle.Plugins[0])
	}

	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugin-promotions", adminToken, map[string]any{
		"action": "import",
		"bundle": exportResp.Bundle,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("promotion import status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if got := adminTestJSON(t, resp)["dry_run"]; got != true {
		t.Fatalf("promotion import dry_run = %#v, want true", got)
	}

	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugin-promotions", adminToken, map[string]any{
		"action":      "apply",
		"bundle":      exportResp.Bundle,
		"config_json": `{"token":"promotion-secret","host":"blue"}`,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("promotion apply status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "promotion-secret") || strings.Contains(resp.Body.String(), "blue") {
		t.Fatalf("promotion apply leaked config value: %s", resp.Body.String())
	}
	if got := adminTestJSON(t, resp)["dry_run"]; got != false {
		t.Fatalf("promotion apply dry_run = %#v, want false", got)
	}
	if !strings.Contains(resp.Body.String(), `"ok":false`) && !strings.Contains(resp.Body.String(), `"ok": false`) ||
		!strings.Contains(resp.Body.String(), `"secret_mapping_missing"`) {
		t.Fatalf("promotion apply body=%s, want blocked result with missing secret mapping", resp.Body.String())
	}

	target := exportResp.Bundle
	target.Plugins = append([]pluginmanager.PromotionPlugin(nil), exportResp.Bundle.Plugins...)
	target.Plugins[0].Version = "9.9.9"
	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugin-promotions", adminToken, map[string]any{
		"action":   "diff",
		"baseline": exportResp.Bundle,
		"target":   target,
	})
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"field":"version"`) && !strings.Contains(resp.Body.String(), `"field": "version"`) {
		t.Fatalf("promotion diff status = %d, body=%s", resp.Code, resp.Body.String())
	}

	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugin-promotions", adminToken, map[string]any{
		"action":   "drift",
		"baseline": exportResp.Bundle,
		"current":  exportResp.Bundle,
	})
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"ok":true`) && !strings.Contains(resp.Body.String(), `"ok": true`) {
		t.Fatalf("promotion drift status = %d, body=%s", resp.Code, resp.Body.String())
	}

	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugin-promotions", adminToken, map[string]any{
		"action": "dr-drill",
		"bundle": exportResp.Bundle,
	})
	if resp.Code != http.StatusOK ||
		!strings.Contains(resp.Body.String(), `"dry_run":true`) && !strings.Contains(resp.Body.String(), `"dry_run": true`) ||
		!strings.Contains(resp.Body.String(), `"ok":false`) && !strings.Contains(resp.Body.String(), `"ok": false`) ||
		!strings.Contains(resp.Body.String(), `"secret_mapping_missing"`) ||
		!strings.Contains(resp.Body.String(), `"config_hash_mismatch"`) {
		t.Fatalf("promotion dr-drill status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if _, err := pluginsManager.Plugin(context.Background(), artifact.PluginID); err != pluginmanager.ErrPluginNotFound {
		t.Fatalf("Plugin(after blocked promotion apply) error = %v, want not found because gate must not write desired state", err)
	}
}

func TestAdminPluginRepositoryImportApplyAPI(t *testing.T) {
	handler := newAdminTestHandlerWithAdmin(t)
	pluginsManager = pluginmanager.New(pluginmanager.Options{
		DB:           adminDB,
		ArtifactRoot: filepath.Join(filepath.Dir(adminDBPath), "plugins", "artifacts"),
		Adapter:      gatewayTestPluginAdapter{},
	})
	adminToken := adminTestLogin(t, handler, "admin", "secret")

	var manifest pluginmanager.Manifest
	if err := json.Unmarshal(gatewayTestManifest(t, "repo-apply-api-plugin"), &manifest); err != nil {
		t.Fatalf("Unmarshal manifest error = %v", err)
	}
	manifest.ConfigSchema = json.RawMessage(`{"type":"object","properties":{"token":{"type":"string","sensitive":true},"host":{"type":"string"}}}`)
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal manifest error = %v", err)
	}
	packagePath := writeGatewayTestMCGPEntries(t, map[string][]byte{
		"manifest.json": manifestBytes,
		"plugin.so":     []byte("repo apply api plugin bytes"),
	})
	indexPath := filepath.Join(t.TempDir(), "repo-index.json")
	indexBytes, _ := json.Marshal(map[string]any{
		"name": "admin-repo",
		"artifacts": []map[string]any{{
			"id":            "repo-apply-api-plugin-0.1.0",
			"plugin_id":     "repo-apply-api-plugin",
			"version":       manifest.Version,
			"artifact_path": packagePath,
		}},
	})
	if err := os.WriteFile(indexPath, indexBytes, 0644); err != nil {
		t.Fatalf("WriteFile(repository index) error = %v", err)
	}

	resp := adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugin-repositories/imports", adminToken, map[string]any{
		"repository_type": pluginmanager.RepositoryTypeFile,
		"index_path":      indexPath,
		"artifact_id":     "repo-apply-api-plugin-0.1.0",
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("repository import status = %d, body=%s", resp.Code, resp.Body.String())
	}
	var importResp struct {
		Import   pluginmanager.RepositoryImportRecord `json:"import"`
		Artifact pluginmanager.ArtifactRecord         `json:"artifact"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &importResp); err != nil {
		t.Fatalf("Unmarshal(import) error = %v\n%s", err, resp.Body.String())
	}
	if importResp.Import.ID == 0 || importResp.Import.ArtifactID != importResp.Artifact.ID {
		t.Fatalf("repository import response = %+v, want import bound to artifact", importResp)
	}

	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugin-repositories/imports", adminToken, map[string]any{
		"action":      "apply",
		"import_id":   importResp.Import.ID,
		"config_json": `{"token":"repo-secret","host":"target"}`,
		"priority":    30,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("repository apply status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "repo-secret") || strings.Contains(resp.Body.String(), "target") {
		t.Fatalf("repository apply leaked config value: %s", resp.Body.String())
	}
	plugin, err := pluginsManager.Plugin(context.Background(), importResp.Artifact.PluginID)
	if err != nil {
		t.Fatalf("Plugin(after repository apply) error = %v", err)
	}
	if plugin.DesiredArtifactID != importResp.Artifact.ID || plugin.DesiredState != pluginmanager.DesiredDisabled || plugin.ActiveArtifactID != "" || plugin.ConfigJSON != `{"token":"repo-secret","host":"target"}` || plugin.Priority != 30 {
		t.Fatalf("plugin after repository apply = %+v, want disabled desired state without active traffic", plugin)
	}
}

func TestAdminPluginRepositoryUpdateAvailabilityAPI(t *testing.T) {
	handler := newAdminTestHandlerWithAdmin(t)
	pluginsManager = pluginmanager.New(pluginmanager.Options{
		DB:           adminDB,
		ArtifactRoot: filepath.Join(filepath.Dir(adminDBPath), "plugins", "artifacts"),
		Adapter:      gatewayTestPluginAdapter{},
	})
	adminToken := adminTestLogin(t, handler, "admin", "secret")
	resp := adminTestRequest(t, handler, http.MethodPost, "/admin/api/users", adminToken, map[string]any{
		"username": "member",
		"role":     "member",
		"password": "member-secret",
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("create member status = %d, body=%s", resp.Code, resp.Body.String())
	}
	memberToken := adminTestLogin(t, handler, "member", "member-secret")

	current := uploadGatewayPhase4Artifact(t, "repo-update-api-plugin")
	if _, err := pluginsManager.SetDesired(context.Background(), "admin", "repo-update-api-plugin", current.ID, pluginmanager.DesiredDisabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired(current) error = %v", err)
	}
	var manifest pluginmanager.Manifest
	if err := json.Unmarshal(gatewayTestManifest(t, "repo-update-api-plugin"), &manifest); err != nil {
		t.Fatalf("Unmarshal manifest error = %v", err)
	}
	manifest.Version = "0.2.0"
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal manifest error = %v", err)
	}
	packagePath := writeGatewayTestMCGPEntries(t, map[string][]byte{
		"manifest.json": manifestBytes,
		"plugin.so":     []byte("repo update api plugin bytes"),
	})
	indexPath := filepath.Join(t.TempDir(), "repo-update-index.json")
	indexBytes, _ := json.Marshal(map[string]any{
		"name": "admin-update-repo",
		"artifacts": []map[string]any{{
			"id":            "repo-update-api-plugin-0.2.0",
			"plugin_id":     "repo-update-api-plugin",
			"version":       "0.2.0",
			"artifact_path": packagePath,
		}},
	})
	if err := os.WriteFile(indexPath, indexBytes, 0644); err != nil {
		t.Fatalf("WriteFile(update index) error = %v", err)
	}

	resp = adminTestRequest(t, handler, http.MethodPost, "/admin/api/plugin-repositories/imports", memberToken, map[string]any{
		"action":          "updates",
		"repository_type": pluginmanager.RepositoryTypeFile,
		"index_path":      indexPath,
		"plugin_id":       "repo-update-api-plugin",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("repository updates status = %d, body=%s", resp.Code, resp.Body.String())
	}
	var updatesResp struct {
		Updates pluginmanager.RepositoryUpdateReport `json:"updates"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &updatesResp); err != nil {
		t.Fatalf("Unmarshal(updates) error = %v\n%s", err, resp.Body.String())
	}
	if updatesResp.Updates.RepositoryName != "admin-update-repo" || len(updatesResp.Updates.Updates) != 1 || updatesResp.Updates.Updates[0].Reason != "newer_version" {
		t.Fatalf("updates response = %+v, want one newer version", updatesResp)
	}
	plugin, err := pluginsManager.Plugin(context.Background(), "repo-update-api-plugin")
	if err != nil {
		t.Fatalf("Plugin(repo-update-api-plugin) error = %v", err)
	}
	if plugin.DesiredArtifactID != current.ID || plugin.ActiveArtifactID != "" || plugin.DesiredState != pluginmanager.DesiredDisabled {
		t.Fatalf("plugin after update availability = %+v, want unchanged desired state", plugin)
	}
}

func TestAdminPluginArtifactPackageDownload(t *testing.T) {
	handler := newAdminTestHandlerWithAdmin(t)
	pluginsManager = pluginmanager.New(pluginmanager.Options{
		DB:           adminDB,
		ArtifactRoot: filepath.Join(filepath.Dir(adminDBPath), "plugins", "artifacts"),
		Adapter:      gatewayTestPluginAdapter{},
	})
	adminToken := adminTestLogin(t, handler, "admin", "secret")
	var manifest pluginmanager.Manifest
	if err := json.Unmarshal(gatewayTestManifest(t, "download-plugin"), &manifest); err != nil {
		t.Fatalf("Unmarshal manifest error = %v", err)
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal manifest error = %v", err)
	}
	packagePath := writeGatewayTestMCGPEntries(t, map[string][]byte{
		"manifest.json": manifestBytes,
		"plugin.so":     []byte("download plugin bytes"),
	})
	wantPackage, err := os.ReadFile(packagePath)
	if err != nil {
		t.Fatalf("ReadFile(package) error = %v", err)
	}
	artifact, err := pluginsManager.UploadArtifact(context.Background(), pluginmanager.ArtifactUpload{
		SourcePath: packagePath,
		FileName:   "download-plugin.mcgp",
		Actor:      "admin",
	})
	if err != nil {
		t.Fatalf("UploadArtifact() error = %v", err)
	}
	resp := adminTestRequest(t, handler, http.MethodGet, "/admin/api/plugin-artifacts/"+artifact.ID+"/package", adminToken, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("package download status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("X-Plugin-Package-SHA256"); got != artifact.PackageSHA256 {
		t.Fatalf("package sha header = %q, want %q", got, artifact.PackageSHA256)
	}
	if !bytes.Equal(resp.Body.Bytes(), wantPackage) {
		t.Fatalf("downloaded package bytes differ from uploaded package")
	}
}

func uploadGatewayPhase4Artifact(t *testing.T, pluginID string) pluginmanager.ArtifactRecord {
	t.Helper()
	var manifest pluginmanager.Manifest
	if err := json.Unmarshal(gatewayTestManifest(t, pluginID), &manifest); err != nil {
		t.Fatalf("Unmarshal manifest error = %v", err)
	}
	manifest.ConfigSchema = json.RawMessage(`{"type":"object","properties":{"token":{"type":"string","sensitive":true},"host":{"type":"string"}}}`)
	manifest.Secrets = []pluginmanager.SecretSpec{{
		Name:     "api_token",
		Required: false,
		Type:     "api_token",
		Rotation: pluginmanager.SecretRotation{
			Reload: "reload_required",
		},
	}}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal manifest error = %v", err)
	}
	artifact, err := pluginsManager.UploadArtifact(context.Background(), pluginmanager.ArtifactUpload{
		SourcePath: writeGatewayTestMCGPEntries(t, map[string][]byte{
			"manifest.json": manifestBytes,
			"plugin.so":     []byte("fake plugin bytes " + pluginID),
		}),
		FileName: pluginID + ".mcgp",
		Actor:    "admin",
	})
	if err != nil {
		t.Fatalf("UploadArtifact() error = %v", err)
	}
	return artifact
}

func uploadGatewayExternalDependencyArtifact(t *testing.T, pluginID, endpoint string) pluginmanager.ArtifactRecord {
	t.Helper()
	var manifest pluginmanager.Manifest
	if err := json.Unmarshal(gatewayTestManifest(t, pluginID), &manifest); err != nil {
		t.Fatalf("Unmarshal manifest error = %v", err)
	}
	manifest.ExternalDeps = []pluginmanager.ExternalSpec{{
		Name:       "session",
		Endpoint:   endpoint,
		Purpose:    "auth",
		Timeout:    "1s",
		Required:   true,
		FailPolicy: "fail_closed",
	}}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal manifest error = %v", err)
	}
	artifact, err := pluginsManager.UploadArtifact(context.Background(), pluginmanager.ArtifactUpload{
		SourcePath: writeGatewayTestMCGPEntries(t, map[string][]byte{
			"manifest.json": manifestBytes,
			"plugin.so":     []byte("fake plugin bytes " + pluginID),
		}),
		FileName: pluginID + ".mcgp",
		Actor:    "admin",
	})
	if err != nil {
		t.Fatalf("UploadArtifact() error = %v", err)
	}
	return artifact
}

func uploadGatewayPhase5ProtocolProxyArtifact(t *testing.T, pluginID string) pluginmanager.ArtifactRecord {
	t.Helper()
	var manifest pluginmanager.Manifest
	if err := json.Unmarshal(gatewayTestManifestWithCapabilities(t, pluginID, gatewayProtocolProxyCapabilities()), &manifest); err != nil {
		t.Fatalf("Unmarshal manifest error = %v", err)
	}
	manifest.RuntimeLimits = pluginmanager.RuntimeLimits{HandlerTimeoutMS: 3000}
	manifest.SupplyChain = json.RawMessage(`{"dependencies":[{"name":"example.com/bad","version":"v1.2.3"}]}`)
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal manifest error = %v", err)
	}
	artifact, err := pluginsManager.UploadArtifact(context.Background(), pluginmanager.ArtifactUpload{
		SourcePath: writeGatewayTestMCGPEntries(t, map[string][]byte{
			"manifest.json": manifestBytes,
			"plugin.so":     []byte("fake plugin bytes " + pluginID),
		}),
		FileName: pluginID + ".mcgp",
		Actor:    "admin",
	})
	if err != nil {
		t.Fatalf("UploadArtifact() error = %v", err)
	}
	return artifact
}

type adminDryRunFailureAdapter struct {
	gatewayTestPluginAdapter
	err error
}

func (a *adminDryRunFailureAdapter) DryRunConfig(_ context.Context, _ pluginmanager.ArtifactRecord, _ pluginmanager.PluginRecord) error {
	return a.err
}

func newAdminTestHandler(t *testing.T) http.Handler {
	t.Helper()
	t.Cleanup(saveGatewayState(t))

	t.Setenv(adminEnvDB, filepath.Join(t.TempDir(), "gateway.sqlite3"))
	if err := loadConfig(); err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	return newGatewayHTTPHandler()
}

func newAdminTestHandlerWithAdmin(t *testing.T) http.Handler {
	t.Helper()
	handler := newAdminTestHandler(t)
	resp := adminTestRequest(t, handler, http.MethodPost, "/admin/api/setup", "", map[string]any{
		"username": "admin",
		"password": "secret",
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("setup status = %d, body=%s", resp.Code, resp.Body.String())
	}
	return handler
}

func adminTestLogin(t *testing.T, handler http.Handler, username, password string) string {
	t.Helper()

	resp := adminTestRequest(t, handler, http.MethodPost, "/admin/api/auth/login", "", map[string]any{
		"username": username,
		"password": password,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("login status = %d, body=%s", resp.Code, resp.Body.String())
	}
	body := adminTestJSON(t, resp)
	token, ok := body["token"].(string)
	if !ok || token == "" {
		t.Fatalf("login token = %#v", body["token"])
	}
	return token
}

func adminTestRequest(t *testing.T, handler http.Handler, method, target, token string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatalf("Encode() error = %v", err)
		}
	}
	req := httptest.NewRequest(method, target, &payload)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	return resp
}

func adminTestJSON(t *testing.T, resp *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	var body map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal(%q) error = %v", resp.Body.String(), err)
	}
	return body
}

func firstSnapshotID(t *testing.T, resp *httptest.ResponseRecorder) int64 {
	t.Helper()
	body := adminTestJSON(t, resp)
	snapshots, ok := body["snapshots"].([]any)
	if !ok || len(snapshots) == 0 {
		t.Fatalf("snapshots = %#v, want at least one", body["snapshots"])
	}
	first, ok := snapshots[0].(map[string]any)
	if !ok {
		t.Fatalf("first snapshot = %#v", snapshots[0])
	}
	id, ok := first["id"].(float64)
	if !ok || id <= 0 {
		t.Fatalf("snapshot id = %#v", first["id"])
	}
	return int64(id)
}

func findAdminServiceModeFeature(t *testing.T, modes []any, mode string) map[string]any {
	t.Helper()
	for _, item := range modes {
		feature, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("service mode feature = %#v, want object", item)
		}
		if feature["mode"] == mode {
			return feature
		}
	}
	t.Fatalf("service mode %q not found in %#v", mode, modes)
	return nil
}

func findAdminRuntimeAdapterStatus(t *testing.T, adapters []any, mode, runtimeType string) map[string]any {
	t.Helper()
	for _, item := range adapters {
		status, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("runtime adapter status = %#v, want object", item)
		}
		if status["service_mode"] == mode && status["runtime_type"] == runtimeType {
			return status
		}
	}
	t.Fatalf("runtime adapter %q/%q not found in %#v", mode, runtimeType, adapters)
	return nil
}

func findPluginRuntimeAdapterStatus(statuses []pluginmanager.RuntimeAdapterFactoryStatus, mode, runtimeType string) pluginmanager.RuntimeAdapterFactoryStatus {
	for _, status := range statuses {
		if status.ServiceMode == mode && status.RuntimeType == runtimeType {
			return status
		}
	}
	return pluginmanager.RuntimeAdapterFactoryStatus{}
}

func findPluginExtensionPointFeature(features []pluginmanager.ExtensionPointFeature, key string) pluginmanager.ExtensionPointFeature {
	for _, feature := range features {
		if feature.Key == key {
			return feature
		}
	}
	return pluginmanager.ExtensionPointFeature{}
}

func findAdminRuntimeFeature(t *testing.T, features []any, runtimeType string) map[string]any {
	t.Helper()
	for _, item := range features {
		feature, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("runtime feature = %#v, want object", item)
		}
		if feature["type"] == runtimeType {
			return feature
		}
	}
	t.Fatalf("runtime feature %q not found in %#v", runtimeType, features)
	return nil
}

func findAdminExtensionPointFeature(t *testing.T, features []any, key string) map[string]any {
	t.Helper()
	for _, item := range features {
		feature, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("extension point feature = %#v, want object", item)
		}
		if feature["key"] == key {
			return feature
		}
	}
	t.Fatalf("extension point %q not found in %#v", key, features)
	return nil
}
