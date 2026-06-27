// internal/adminhttp/requests_test.go 包含用于约束 requests 行为的测试。

package adminhttp

import (
	"encoding/json"
	"testing"
)

func TestRouteRequestKeepsOptionalEnabled(t *testing.T) {
	var req RouteRequest
	if err := json.Unmarshal([]byte(`{"upstream":"127.0.0.1:25565","note":"primary"}`), &req); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if req.Enabled != nil {
		t.Fatalf("Enabled = %v, want nil when omitted", *req.Enabled)
	}

	if err := json.Unmarshal([]byte(`{"enabled":false}`), &req); err != nil {
		t.Fatalf("Unmarshal(enabled) error = %v", err)
	}
	if req.Enabled == nil || *req.Enabled {
		t.Fatalf("Enabled = %v, want false pointer", req.Enabled)
	}
}

func TestPatchUserRequestKeepsOmittedFieldsNil(t *testing.T) {
	var req PatchUserRequest
	if err := json.Unmarshal([]byte(`{"role":"guest"}`), &req); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if req.Role == nil || *req.Role != "guest" {
		t.Fatalf("Role = %v, want guest pointer", req.Role)
	}
	if req.Password != nil {
		t.Fatal("Password pointer should be nil when omitted")
	}
	if req.Disabled != nil {
		t.Fatal("Disabled pointer should be nil when omitted")
	}
}

func TestServiceRequestDecodesOptions(t *testing.T) {
	var req ServiceRequest
	if err := json.Unmarshal([]byte(`{"enabled":true,"port":25570,"options":{"path":"/ws"}}`), &req); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if req.Enabled == nil || !*req.Enabled {
		t.Fatalf("Enabled = %v, want true pointer", req.Enabled)
	}
	if req.Port != 25570 {
		t.Fatalf("Port = %d, want 25570", req.Port)
	}
	if req.Options["path"] != "/ws" {
		t.Fatalf("path option = %#v, want /ws", req.Options["path"])
	}
}
