package main

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/tursom/mc-gateway/internal/admindb"
	"github.com/tursom/mc-gateway/internal/pluginmanager"
	"github.com/tursom/mc-gateway/plugin/api"
)

func TestMapToHostUsesManagedPluginBeforeLegacyHook(t *testing.T) {
	defer saveGatewayState(t)()

	packet := gatewayTestPacket("play.example")
	source := newGatewayTestConn(packet)
	managedUpstream := newGatewayTestConn(nil)
	legacyUpstream := newGatewayTestConn(nil)
	setGatewayTestRoutes(map[string]string{
		"play.example": "backend.example:25565",
	})
	pluginsManager = pluginmanager.New(pluginmanager.Options{
		DB:           newGatewayTestPluginDB(t),
		ArtifactRoot: t.TempDir(),
		Adapter: gatewayTestPluginAdapter{handler: func(req api.UpstreamConnectRequest) (net.Conn, error) {
			if req.Host != "play.example" || req.Upstream != "backend.example:25565" || !bytes.Equal(req.InitialData, packet) {
				t.Fatalf("managed request = %+v, initial=%v", req, req.InitialData)
			}
			return managedUpstream, nil
		}},
	})
	artifact := uploadGatewayTestArtifact(t, pluginsManager, "managed-upstream")
	if _, err := pluginsManager.SetDesired(context.Background(), "admin", "managed-upstream", artifact.ID, pluginmanager.DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := pluginsManager.Enable(context.Background(), "admin", "managed-upstream"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	registerGatewayUpstreamHook(
		t,
		func(net.Conn, string) bool { return true },
		func(net.Conn, string) (net.Conn, error) { return legacyUpstream, nil },
	)

	got := mapToHost(source)
	if got != managedUpstream {
		t.Fatalf("mapToHost() = %v, want managed upstream", got)
	}
	if !bytes.Equal(managedUpstream.writeBuf.Bytes(), packet) {
		t.Fatalf("managed upstream initial packet = %v, want %v", managedUpstream.writeBuf.Bytes(), packet)
	}
	if legacyUpstream.writeBuf.Len() != 0 {
		t.Fatalf("legacy upstream was used: %v", legacyUpstream.writeBuf.Bytes())
	}

	if _, err := pluginsManager.Disable(context.Background(), "admin", "managed-upstream"); err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
	nextSource := newGatewayTestConn(packet)
	if got := mapToHost(nextSource); got != legacyUpstream {
		t.Fatalf("mapToHost() after disable = %v, want legacy upstream", got)
	}
}

func TestMapToHostRoutesThroughHookAndForwardsInitialPacket(t *testing.T) {
	defer saveGatewayState(t)()

	packet := gatewayTestPacket("play.example", 0x63, 0x00)
	source := newGatewayTestConn(packet)
	upstream := newGatewayTestConn(nil)
	setGatewayTestRoutes(map[string]string{
		"play.example": "backend.example:25565",
	})

	var gotSource net.Conn
	var gotHost string
	registerGatewayUpstreamHook(
		t,
		func(source net.Conn, host string) bool {
			gotSource = source
			gotHost = host
			return host == "backend.example:25565"
		},
		func(source net.Conn, host string) (net.Conn, error) {
			return upstream, nil
		},
	)

	got := mapToHost(source)
	if got != upstream {
		t.Fatalf("mapToHost() = %v, want upstream conn", got)
	}
	if gotSource != source {
		t.Fatalf("hook source = %v, want original source", gotSource)
	}
	if gotHost != "backend.example:25565" {
		t.Fatalf("hook host = %q, want backend.example:25565", gotHost)
	}
	if !bytes.Equal(upstream.writeBuf.Bytes(), packet) {
		t.Fatalf("upstream initial packet = %v, want %v", upstream.writeBuf.Bytes(), packet)
	}
}

func TestMapToHostUsesDefaultRoute(t *testing.T) {
	defer saveGatewayState(t)()

	packet := gatewayTestPacket("unknown.example")
	source := newGatewayTestConn(packet)
	upstream := newGatewayTestConn(nil)
	setGatewayTestRoutes(map[string]string{
		"default": "fallback.example:25565",
	})

	registerGatewayUpstreamHook(
		t,
		func(_ net.Conn, host string) bool {
			return host == "fallback.example:25565"
		},
		func(net.Conn, string) (net.Conn, error) {
			return upstream, nil
		},
	)

	if got := mapToHost(source); got != upstream {
		t.Fatalf("mapToHost() = %v, want fallback upstream", got)
	}
	if !bytes.Equal(upstream.writeBuf.Bytes(), packet) {
		t.Fatalf("upstream initial packet = %v, want %v", upstream.writeBuf.Bytes(), packet)
	}
}

func TestMapToHostRejectsInvalidOrUnroutedPackets(t *testing.T) {
	tests := []struct {
		name   string
		packet []byte
		hosts  map[string]string
	}{
		{
			name:   "read error",
			packet: nil,
			hosts:  map[string]string{"default": "fallback.example:25565"},
		},
		{
			name:   "malformed packet",
			packet: []byte{0x01, 0x02, 0x03, 0x04, 0x08, 'a'},
			hosts:  map[string]string{"default": "fallback.example:25565"},
		},
		{
			name:   "missing route",
			packet: gatewayTestPacket("unknown.example"),
			hosts:  map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer saveGatewayState(t)()

			source := newGatewayTestConn(tt.packet)
			if tt.packet == nil {
				source.readErr = errors.New("read failed")
			}
			setGatewayTestRoutes(tt.hosts)

			if got := mapToHost(source); got != nil {
				t.Fatalf("mapToHost() = %v, want nil", got)
			}
		})
	}
}

func TestMapToHostReturnsNilWhenHookFails(t *testing.T) {
	defer saveGatewayState(t)()

	source := newGatewayTestConn(gatewayTestPacket("play.example"))
	setGatewayTestRoutes(map[string]string{
		"play.example": "backend.example:25565",
	})
	wantErr := errors.New("hook failed")

	registerGatewayUpstreamHook(
		t,
		func(net.Conn, string) bool { return true },
		func(net.Conn, string) (net.Conn, error) {
			return nil, wantErr
		},
	)

	if got := mapToHost(source); got != nil {
		t.Fatalf("mapToHost() = %v, want nil", got)
	}
}

func TestMapToHostClosesUpstreamWhenInitialWriteFails(t *testing.T) {
	defer saveGatewayState(t)()

	source := newGatewayTestConn(gatewayTestPacket("play.example"))
	upstream := newGatewayTestConn(nil)
	upstream.writeErr = errors.New("write failed")
	setGatewayTestRoutes(map[string]string{
		"play.example": "backend.example:25565",
	})

	registerGatewayUpstreamHook(
		t,
		func(net.Conn, string) bool { return true },
		func(net.Conn, string) (net.Conn, error) {
			return upstream, nil
		},
	)

	if got := mapToHost(source); got != nil {
		t.Fatalf("mapToHost() = %v, want nil", got)
	}
	if !upstream.closed {
		t.Fatal("upstream was not closed after write failure")
	}
}

type gatewayTestPluginAdapter struct {
	handler api.UpstreamConnectHandler
}

func (a gatewayTestPluginAdapter) Load(_ context.Context, _ pluginmanager.ArtifactRecord, _ pluginmanager.PluginRecord, gateway *pluginmanager.Gateway) (api.Plugin, error) {
	handler := a.handler
	if handler == nil {
		handler = func(api.UpstreamConnectRequest) (net.Conn, error) {
			return nil, api.ErrPass
		}
	}
	if err := api.RegisterHookHandler(
		gateway,
		api.HookUpstreamConnect,
		func(api.UpstreamConnectRequest) bool { return true },
		handler,
	); err != nil {
		return nil, err
	}
	return &gatewayPluginStub{}, nil
}

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
	artifact, err := manager.UploadArtifact(context.Background(), pluginmanager.ArtifactUpload{
		SourcePath: writeGatewayTestMCGP(t, pluginID),
		FileName:   pluginID + ".mcgp",
		Actor:      "admin",
	})
	if err != nil {
		t.Fatalf("UploadArtifact() error = %v", err)
	}
	return artifact
}

func writeGatewayTestMCGP(t *testing.T, pluginID string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plugin.mcgp")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create zip error = %v", err)
	}
	writer := zip.NewWriter(file)
	entries := map[string][]byte{
		"manifest.json": gatewayTestManifest(t, pluginID),
		"plugin.so":     []byte("fake plugin bytes " + pluginID),
	}
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
	t.Helper()
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
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal manifest error = %v", err)
	}
	return data
}
