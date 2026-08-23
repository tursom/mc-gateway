package sandboxsdk

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tursom/mc-gateway/plugin/api"
)

func TestNewFromEnvParsesControlSocket(t *testing.T) {
	t.Setenv(EnvControl, "unix:///run/control.sock")
	t.Setenv(EnvPluginID, "sandbox-example")
	t.Setenv(EnvArtifactID, "artifact-1")
	t.Setenv(EnvRuntimeID, "runtime-1")
	t.Setenv(EnvGeneration, "7")

	client, err := NewFromEnv()
	if err != nil {
		t.Fatalf("NewFromEnv() error = %v", err)
	}
	if client.SocketPath != "/run/control.sock" || client.PluginID != "sandbox-example" || client.Generation != 7 {
		t.Fatalf("client = %+v, want env-derived socket/plugin/generation", client)
	}
}

func TestStartupSendsHandshakeInitRegister(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "control.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer listener.Close()
	errCh := make(chan error, 1)
	go func() {
		expected := []string{CommandHandshake, CommandInit, CommandRegister}
		for _, command := range expected {
			conn, err := listener.Accept()
			if err != nil {
				errCh <- err
				return
			}
			var req ControlRequest
			if err := json.NewDecoder(conn).Decode(&req); err != nil {
				_ = conn.Close()
				errCh <- err
				return
			}
			if req.Command != command || req.Protocol != Protocol || req.PluginID != "sandbox-example" || req.Generation != 7 {
				_ = conn.Close()
				errCh <- &unexpectedControlRequestError{request: req, command: command}
				return
			}
			if command == CommandRegister {
				var register RegisterRequest
				if err := json.Unmarshal(req.Payload, &register); err != nil {
					_ = conn.Close()
					errCh <- err
					return
				}
				if len(register.Handlers) != 1 || register.Handlers[0].ExtensionPoint != "route.resolve/v1" {
					_ = conn.Close()
					errCh <- &unexpectedControlRequestError{request: req, command: "register payload"}
					return
				}
			}
			resp := ControlResponse{
				RequestID:         req.RequestID,
				Command:           req.Command,
				Protocol:          Protocol,
				PluginID:          req.PluginID,
				ArtifactID:        req.ArtifactID,
				RuntimeInstanceID: req.RuntimeInstanceID,
				Generation:        req.Generation,
				OK:                true,
			}
			if err := json.NewEncoder(conn).Encode(resp); err != nil {
				_ = conn.Close()
				errCh <- err
				return
			}
			_ = conn.Close()
		}
		errCh <- nil
	}()
	client := Client{
		SocketPath:        socketPath,
		PluginID:          "sandbox-example",
		ArtifactID:        "artifact-1",
		RuntimeInstanceID: "runtime-1",
		Generation:        7,
		Timeout:           time.Second,
	}
	err = client.Startup(context.Background(), []HandlerRegistration{{
		ExtensionPoint: "route.resolve/v1",
		HandlerID:      "route-main",
		FailPolicy:     "close",
		TimeoutMS:      1000,
		SchemaVersion:  1,
	}}, []string{"runtime.cpu_memory"})
	if err != nil {
		t.Fatalf("Startup() error = %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("control server error = %v", err)
	}
}

func TestServiceHandlesInvokeAndStreamCommands(t *testing.T) {
	validConfig := false
	service := Service{
		ConfigValidate: func(_ context.Context, raw json.RawMessage) (bool, string, error) {
			validConfig = json.Valid(raw)
			return validConfig, "checked", nil
		},
		RouteResolve: func(_ context.Context, req api.RouteResolveRequest) (api.RouteDecision, error) {
			return api.RouteDecision{Action: api.RouteDecisionOverride, Upstream: "sdk-route:25565", Host: req.Host}, nil
		},
		RuleEvaluate: func(_ context.Context, req api.RuleEvaluateRequest) (api.RuleEvaluateDecision, error) {
			return api.RuleEvaluateDecision{Deny: req.Subject == "blocked", Reason: "sdk rule"}, nil
		},
		StreamOpen: func(_ context.Context, req StreamOpenRequest) (StreamOpenResponse, error) {
			return StreamOpenResponse{Connected: true, Protocol: req.Protocol, StreamID: req.StreamID, Endpoint: "/tmp/" + req.StreamID + ".sock", EndpointType: "unix"}, nil
		},
	}

	configResp := service.HandleControlRequest(context.Background(), controlRequest(t, CommandInvoke, InvokeRequest{
		ExtensionPoint: "config.validate/v1",
		HandlerID:      "config-main",
		ConfigJSON:     json.RawMessage(`{"ok":true}`),
	}))
	if !configResp.OK || configResp.Invoke == nil || configResp.Invoke.Valid == nil || !*configResp.Invoke.Valid || !validConfig {
		t.Fatalf("config invoke response = %+v, want valid config", configResp)
	}

	routeResp := service.HandleControlRequest(context.Background(), controlRequest(t, CommandInvoke, InvokeRequest{
		ExtensionPoint: "route.resolve/v1",
		HandlerID:      "route-main",
		RouteResolve:   &api.RouteResolveRequest{Host: "play.example"},
	}))
	if !routeResp.OK || routeResp.Invoke == nil || routeResp.Invoke.RouteDecision == nil || routeResp.Invoke.RouteDecision.Upstream != "sdk-route:25565" {
		t.Fatalf("route invoke response = %+v, want route decision", routeResp)
	}

	ruleResp := service.HandleControlRequest(context.Background(), controlRequest(t, CommandInvoke, InvokeRequest{
		ExtensionPoint: "rule.evaluate/v1",
		HandlerID:      "rule-main",
		RuleEvaluate:   &api.RuleEvaluateRequest{Subject: "blocked"},
	}))
	if !ruleResp.OK || ruleResp.Invoke == nil || ruleResp.Invoke.RuleDecision == nil || !ruleResp.Invoke.RuleDecision.Deny {
		t.Fatalf("rule invoke response = %+v, want deny decision", ruleResp)
	}

	streamResp := service.HandleControlRequest(context.Background(), controlRequest(t, CommandStreamOpen, StreamOpenRequest{
		ExtensionPoint: "upstream.connect/v2",
		HandlerID:      "stream-main",
		Protocol:       "stream.proxy/v1",
		StreamID:       "stream-1",
	}))
	if !streamResp.OK || streamResp.Stream == nil || !streamResp.Stream.Connected || streamResp.Stream.EndpointType != "unix" {
		t.Fatalf("stream_open response = %+v, want unix stream endpoint", streamResp)
	}
}

func TestClientRunRegistersServiceAndStopsOnCancellation(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "control.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer listener.Close()

	registered := make(chan RegisterRequest, 1)
	serverErr := make(chan error, 1)
	go func() {
		for _, command := range []string{CommandHandshake, CommandInit, CommandRegister} {
			conn, err := listener.Accept()
			if err != nil {
				serverErr <- err
				return
			}
			var req ControlRequest
			if err := json.NewDecoder(conn).Decode(&req); err != nil {
				_ = conn.Close()
				serverErr <- err
				return
			}
			if req.Command != command {
				_ = conn.Close()
				serverErr <- &unexpectedControlRequestError{request: req, command: command}
				return
			}
			if command == CommandRegister {
				var register RegisterRequest
				if err := json.Unmarshal(req.Payload, &register); err != nil {
					_ = conn.Close()
					serverErr <- err
					return
				}
				registered <- register
			}
			if err := json.NewEncoder(conn).Encode(ControlResponse{
				RequestID: req.RequestID,
				Command:   command,
				Protocol:  Protocol,
				OK:        true,
			}); err != nil {
				_ = conn.Close()
				serverErr <- err
				return
			}
			_ = conn.Close()
		}
		serverErr <- nil
	}()

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() {
		runErr <- (Client{SocketPath: socketPath, Timeout: time.Second}).Run(ctx, Service{
			Registrations: []HandlerRegistration{{ExtensionPoint: "status.ping/v1", HandlerID: "status-main"}},
			Capabilities:  []string{"runtime.cpu_memory"},
		})
	}()

	select {
	case register := <-registered:
		if len(register.Handlers) != 1 || register.Handlers[0].HandlerID != "status-main" {
			t.Fatalf("register request = %+v, want status-main handler", register)
		}
		cancel()
	case <-time.After(2 * time.Second):
		t.Fatal("Client.Run() did not register service")
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("control server error = %v", err)
	}
	if err := <-runErr; err != nil {
		t.Fatalf("Client.Run() error = %v, want clean cancellation", err)
	}
}

func TestServiceHandlesStatusCloseAndProtocolErrors(t *testing.T) {
	closedStream := ""
	service := Service{
		StatusPing: func(_ context.Context, req api.StatusPingRequest) (api.StatusPingResponse, error) {
			return api.StatusPingResponse{MOTD: "ready:" + req.Host}, nil
		},
		StreamClose: func(_ context.Context, req StreamCloseRequest) error {
			closedStream = req.StreamID
			return nil
		},
	}

	status := service.HandleControlRequest(nil, controlRequest(t, CommandInvoke, InvokeRequest{
		ExtensionPoint: "status.ping/v1",
		HandlerID:      "status-main",
		StatusPing:     &api.StatusPingRequest{Host: "probe.example"},
	}))
	if !status.OK || status.Invoke == nil || status.Invoke.StatusResponse == nil || status.Invoke.StatusResponse.MOTD != "ready:probe.example" {
		t.Fatalf("status response = %+v, want ready:probe.example", status)
	}

	closed := service.HandleControlRequest(context.Background(), controlRequest(t, CommandStreamClose, StreamCloseRequest{
		Protocol: "stream.proxy/v1",
		StreamID: "stream-7",
		Reason:   "completed",
	}))
	if !closed.OK || closedStream != "stream-7" {
		t.Fatalf("close response = %+v, closed stream = %q", closed, closedStream)
	}

	for name, testCase := range map[string]struct {
		request ControlRequest
		code    string
	}{
		"unknown command": {request: controlRequest(t, "unsupported", struct{}{}), code: "unknown_command"},
		"invalid invoke":  {request: controlRequest(t, CommandInvoke, struct{}{}), code: "schema_invalid"},
		"missing handler": {request: controlRequest(t, CommandStreamOpen, StreamOpenRequest{}), code: "not_implemented"},
	} {
		t.Run(name, func(t *testing.T) {
			response := (Service{}).HandleControlRequest(context.Background(), testCase.request)
			if response.OK || response.ErrorCode != testCase.code || strings.TrimSpace(response.Error) == "" {
				t.Fatalf("response = %+v, want %s error", response, testCase.code)
			}
		})
	}

	failing := Service{StreamClose: func(context.Context, StreamCloseRequest) error { return errors.New("close failed") }}
	response := failing.HandleControlRequest(context.Background(), controlRequest(t, CommandStreamClose, StreamCloseRequest{StreamID: "stream-8"}))
	if response.OK || response.ErrorCode != "handler_failed" || !strings.Contains(response.Error, "close failed") {
		t.Fatalf("failed close response = %+v, want handler_failed", response)
	}
}

func controlRequest(t *testing.T, command string, payload any) ControlRequest {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal(%s payload) error = %v", command, err)
	}
	return ControlRequest{
		RequestID:         "test-" + command,
		Command:           command,
		Protocol:          Protocol,
		PluginID:          "sandbox-example",
		ArtifactID:        "artifact-1",
		RuntimeInstanceID: "runtime-1",
		Generation:        7,
		Payload:           data,
	}
}

type unexpectedControlRequestError struct {
	request ControlRequest
	command string
}

func (e *unexpectedControlRequestError) Error() string {
	return "unexpected sandbox control request for " + e.command
}
