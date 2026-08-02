// cmd/gateway/main_test.go 包含用于约束 gateway 行为的测试。

package main

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/tursom/mc-gateway/internal/admindb"
	"github.com/tursom/mc-gateway/internal/pluginmanager"
	"github.com/tursom/mc-gateway/plugin/api"
)

type gatewayPluginStub struct{}

type gatewayTestPluginAdapter struct {
	initHook func(*pluginmanager.Gateway) error
	plugin   api.Plugin
}

func (a gatewayTestPluginAdapter) Load(_ context.Context, _ pluginmanager.ArtifactRecord, _ pluginmanager.PluginRecord, gateway *pluginmanager.Gateway) (api.Plugin, error) {
	if a.initHook != nil {
		if err := a.initHook(gateway); err != nil {
			return nil, err
		}
	} else {
		if err := api.RegisterUpstreamConnectHandlerV2(gateway, func(req api.UpstreamConnectRequestV2) error { return req.Flow.Next(req.Connection) }); err != nil {
			return nil, err
		}
	}
	if a.plugin != nil {
		return a.plugin, nil
	}
	return &gatewayPluginStub{}, nil
}

func (*gatewayPluginStub) Init(api.Gateway) error { return nil }
func (*gatewayPluginStub) Destroy() error         { return nil }
func (*gatewayPluginStub) NewConfigObj() any      { return &struct{}{} }
func (*gatewayPluginStub) ReloadConfig(any) error { return nil }

func newGatewayTestPluginDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := admindb.Open(filepath.Join(t.TempDir(), "gateway.sqlite3"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := admindb.Migrate(db); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return db
}

func uploadGatewayTestArtifact(t *testing.T, manager *pluginmanager.Manager, pluginID string) pluginmanager.ArtifactRecord {
	t.Helper()
	return uploadGatewayTestArtifactWithCapabilities(t, manager, pluginID, "")
}

func uploadGatewayTestArtifactWithCapabilities(t *testing.T, manager *pluginmanager.Manager, pluginID string, capabilities string) pluginmanager.ArtifactRecord {
	t.Helper()
	artifact, err := manager.UploadArtifact(context.Background(), pluginmanager.ArtifactUpload{
		SourcePath: writeGatewayTestMCGPWithCapabilities(t, pluginID, capabilities),
		FileName:   pluginID + ".mcgp",
		Actor:      "admin",
	})
	if err != nil {
		t.Fatalf("UploadArtifact() error = %v", err)
	}
	return artifact
}

func uploadGatewayTestArtifactWithManifest(t *testing.T, manager *pluginmanager.Manager, pluginID string, mutate func(*pluginmanager.Manifest)) pluginmanager.ArtifactRecord {
	t.Helper()
	var manifest pluginmanager.Manifest
	if err := json.Unmarshal(gatewayTestManifest(t, pluginID), &manifest); err != nil {
		t.Fatalf("Unmarshal manifest error = %v", err)
	}
	if mutate != nil {
		mutate(&manifest)
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal manifest error = %v", err)
	}
	artifact, err := manager.UploadArtifact(context.Background(), pluginmanager.ArtifactUpload{
		SourcePath: writeGatewayTestMCGPEntries(t, map[string][]byte{
			"manifest.json": data,
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

func approveGatewayPluginGovernanceForTest(t *testing.T, pluginID, artifactID string) {
	t.Helper()
	if _, err := pluginsManager.CreateReview(context.Background(), "admin", pluginID, pluginmanager.GovernanceReviewRequest{
		ArtifactID: artifactID,
		Profile:    pluginmanager.PolicyProfileProd,
		Decision:   pluginmanager.ReviewDecisionApproved,
		Notes:      "test approval",
	}); err != nil {
		t.Fatalf("CreateReview(%s) error = %v", pluginID, err)
	}
}

func writeGatewayTestMCGP(t *testing.T, pluginID string) string {
	return writeGatewayTestMCGPWithCapabilities(t, pluginID, "")
}

func writeGatewayTestMCGPWithCapabilities(t *testing.T, pluginID string, capabilities string) string {
	t.Helper()
	entries := map[string][]byte{
		"manifest.json": gatewayTestManifestWithCapabilities(t, pluginID, capabilities),
		"plugin.so":     []byte("fake plugin bytes " + pluginID),
	}
	return writeGatewayTestMCGPEntries(t, entries)
}

func writeGatewayTestMCGPEntries(t *testing.T, entries map[string][]byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plugin.mcgp")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create zip error = %v", err)
	}
	writer := zip.NewWriter(file)
	for name, data := range entries {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatalf("Create entry error = %v", err)
		}
		if _, err := entry.Write(data); err != nil {
			t.Fatalf("Write entry error = %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close zip writer error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close zip file error = %v", err)
	}
	return path
}

func gatewayTestManifest(t *testing.T, pluginID string) []byte {
	return gatewayTestManifestWithCapabilities(t, pluginID, "")
}

func gatewayTakeoverCapabilities() string {
	return `{"extension_points":["upstream.connect/v2"]}`
}

func gatewayTestManifestWithCapabilities(t *testing.T, pluginID string, capabilities string) []byte {
	t.Helper()
	if capabilities == "" {
		capabilities = `{"extension_points":["upstream.connect/v2"]}`
	}
	manifest := pluginmanager.Manifest{
		SchemaVersion: pluginmanager.SchemaVersion,
		ID:            pluginID,
		Name:          "Managed Upstream",
		Version:       "0.1.0",
		ArtifactType:  pluginmanager.ArtifactTypeBinary,
		Runtime: pluginmanager.RuntimeManifest{
			Type:        pluginmanager.RuntimeGoPlugin,
			Entry:       pluginmanager.RuntimeEntry,
			EntrySymbol: "Plugin",
		},
		APIVersion: pluginmanager.APIVersion,
		GoVersion:  runtime.Version(),
		GOOS:       runtime.GOOS,
		GOARCH:     runtime.GOARCH,
		ExtensionPoints: []pluginmanager.ExtensionPoint{{
			Type: "hook",
			Key:  pluginmanager.ExtensionUpstreamConnect,
		}},
		Capabilities: json.RawMessage(capabilities),
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal manifest error = %v", err)
	}
	return data
}
