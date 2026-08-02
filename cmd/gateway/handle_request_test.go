// cmd/gateway/handle_request_test.go 包含用于约束 handle request 行为的测试。

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tursom/mc-gateway/internal/pluginmanager"
	"github.com/tursom/mc-gateway/plugin/api"
)

func TestHandleRequestProxiesAndClosesConnections(t *testing.T) {
	defer saveGatewayState(t)()

	packet := gatewayTestPacket("play.example")
	source := newGatewayTestConn(packet)
	upstreamAddress, upstreamDone := startGatewayTestUpstream(t, len(packet), []byte("reply"))
	setGatewayTestRoutes(map[string]string{
		"play.example": upstreamAddress,
	})

	handleRequest(source)

	if !source.closed {
		t.Fatal("source connection was not closed")
	}
	if upstreamPacket := waitGatewayTestUpstream(t, upstreamDone); !bytes.Equal(upstreamPacket, packet) {
		t.Fatalf("upstream initial packet = %v, want %v", upstreamPacket, packet)
	}
	if got := source.writeBuf.String(); got != "reply" {
		t.Fatalf("proxied reply = %q, want reply", got)
	}
}

func TestHandleRequestRouteResolverUsesOverrideAndSQLiteFallback(t *testing.T) {
	defer saveGatewayState(t)()

	overridePacket := gatewayTestPacket("override.example")
	fallbackPacket := gatewayTestPacket("fallback.example")
	overrideAddress, overrideDone := startGatewayTestUpstream(t, len(overridePacket), nil)
	fallbackAddress, fallbackDone := startGatewayTestUpstream(t, len(fallbackPacket), nil)
	setGatewayTestRoutes(map[string]string{"fallback.example": fallbackAddress})
	pluginsManager = pluginmanager.New(pluginmanager.Options{
		DB:           newGatewayTestPluginDB(t),
		ArtifactRoot: t.TempDir(),
		Adapter: gatewayTestPluginAdapter{initHook: func(gateway *pluginmanager.Gateway) error {
			return api.RegisterHookHandler(gateway, api.HookRouteResolve,
				func(api.RouteResolveRequest) bool { return true },
				func(req api.RouteResolveRequest) (api.RouteDecision, error) {
					if req.Host == "override.example" {
						return api.RouteDecision{Action: api.RouteDecisionOverride, Upstream: overrideAddress, CacheTTL: time.Minute}, nil
					}
					return api.RouteDecision{Action: api.RouteDecisionPass}, nil
				})
		}},
	})
	artifact := uploadGatewayTestArtifactWithManifest(t, pluginsManager, "route-plugin", func(manifest *pluginmanager.Manifest) {
		manifest.ExtensionPoints = []pluginmanager.ExtensionPoint{{Type: "provider", Key: pluginmanager.ExtensionRouteResolve}}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["route.resolve/v1"]}`)
	})
	if _, err := pluginsManager.SetDesired(context.Background(), "admin", "route-plugin", artifact.ID, pluginmanager.DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := pluginsManager.Enable(context.Background(), "admin", "route-plugin"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	handleRequest(newGatewayTestConn(overridePacket))
	handleRequest(newGatewayTestConn(fallbackPacket))

	if got := waitGatewayTestUpstream(t, overrideDone); !bytes.Equal(got, overridePacket) {
		t.Fatalf("override upstream packet = %v, want %v", got, overridePacket)
	}
	if got := waitGatewayTestUpstream(t, fallbackDone); !bytes.Equal(got, fallbackPacket) {
		t.Fatalf("fallback upstream packet = %v, want %v", got, fallbackPacket)
	}
}

func TestHandleRequestStatusPingPluginRespondsPerHost(t *testing.T) {
	defer saveGatewayState(t)()

	pluginsManager = pluginmanager.New(pluginmanager.Options{
		DB:           newGatewayTestPluginDB(t),
		ArtifactRoot: t.TempDir(),
		Adapter: gatewayTestPluginAdapter{initHook: func(gateway *pluginmanager.Gateway) error {
			return api.RegisterHookHandler(gateway, api.HookStatusPing,
				func(api.StatusPingRequest) bool { return true },
				func(req api.StatusPingRequest) (api.StatusPingResponse, error) {
					return api.StatusPingResponse{MOTD: "hello " + req.Host, VersionText: "phase7", MaxPlayers: 100}, nil
				})
		}},
	})
	artifact := uploadGatewayTestArtifactWithManifest(t, pluginsManager, "status-plugin", func(manifest *pluginmanager.Manifest) {
		manifest.ExtensionPoints = []pluginmanager.ExtensionPoint{{Type: "hook", Key: pluginmanager.ExtensionStatusPing}}
		manifest.Capabilities = json.RawMessage(`{"extension_points":["status.ping/v1"]}`)
	})
	if _, err := pluginsManager.SetDesired(context.Background(), "admin", "status-plugin", artifact.ID, pluginmanager.DesiredEnabled, `{}`, 10); err != nil {
		t.Fatalf("SetDesired() error = %v", err)
	}
	if _, err := pluginsManager.Enable(context.Background(), "admin", "status-plugin"); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}

	source := newGatewayTestConn(gatewayTestPacket("status.example", 0x63, 0x01))
	handleRequest(source)

	if got := source.writeBuf.String(); !strings.Contains(got, "hello status.example") {
		t.Fatalf("status response = %q, want host MOTD", got)
	}
}

func TestHandleRequestRecoversAndClosesConnection(t *testing.T) {
	defer saveGatewayState(t)()

	source := &panicReadGatewayConn{gatewayTestConn: newGatewayTestConn(nil)}

	handleRequest(source)

	if !source.closed {
		t.Fatal("source connection was not closed after panic")
	}
}

type panicReadGatewayConn struct {
	*gatewayTestConn
}

func (c *panicReadGatewayConn) Read([]byte) (int, error) {
	panic("read panic")
}
