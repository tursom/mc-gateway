// Package sandboxsdk provides the small public helper surface needed by
// sandbox-process example binaries. It intentionally mirrors only the stable
// JSON protocol fields needed for handshake, init, register, and basic invoke
// responses; gateway-owned enforcement and lifecycle policy remain in the host.
package sandboxsdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tursom/mc-gateway/plugin/api"
)

const (
	Protocol      = "mc-gateway-sandbox-process/v1"
	ABIVersion    = "mc-gateway.sandbox-process.abi/v1"
	EnvControl    = "MC_GATEWAY_SANDBOX_CONTROL"
	EnvPluginID   = "MC_GATEWAY_PLUGIN_ID"
	EnvArtifactID = "MC_GATEWAY_ARTIFACT_ID"
	EnvRuntimeID  = "MC_GATEWAY_RUNTIME_INSTANCE_ID"
	EnvGeneration = "MC_GATEWAY_GENERATION"

	CommandHandshake   = "handshake"
	CommandInit        = "init"
	CommandRegister    = "register"
	CommandInvoke      = "invoke"
	CommandStreamOpen  = "stream_open"
	CommandStreamClose = "stream_close"
)

const (
	maxFrameBytes   = 64 * 1024
	maxPayloadBytes = 32 * 1024
)

type Client struct {
	SocketPath        string
	PluginID          string
	ArtifactID        string
	RuntimeInstanceID string
	Generation        int64
	Timeout           time.Duration
}

type HandlerRegistration struct {
	ExtensionPoint       string   `json:"extension_point"`
	HandlerID            string   `json:"handler_id"`
	FailPolicy           string   `json:"fail_policy"`
	TimeoutMS            int64    `json:"timeout_ms"`
	SchemaVersion        int      `json:"schema_version"`
	DeclaredCapabilities []string `json:"declared_capabilities,omitempty"`
}

type ControlRequest struct {
	RequestID         string          `json:"request_id,omitempty"`
	Command           string          `json:"command"`
	Protocol          string          `json:"protocol"`
	PluginID          string          `json:"plugin_id,omitempty"`
	ArtifactID        string          `json:"artifact_id,omitempty"`
	RuntimeInstanceID string          `json:"runtime_instance_id,omitempty"`
	Generation        int64           `json:"generation"`
	TraceID           string          `json:"trace_id,omitempty"`
	DeadlineUnixMS    int64           `json:"deadline,omitempty"`
	Payload           json.RawMessage `json:"payload,omitempty"`
}

type ControlResponse struct {
	RequestID         string              `json:"request_id,omitempty"`
	Command           string              `json:"command,omitempty"`
	Protocol          string              `json:"protocol"`
	PluginID          string              `json:"plugin_id,omitempty"`
	ArtifactID        string              `json:"artifact_id,omitempty"`
	RuntimeInstanceID string              `json:"runtime_instance_id,omitempty"`
	Generation        int64               `json:"generation"`
	TraceID           string              `json:"trace_id,omitempty"`
	DeadlineUnixMS    int64               `json:"deadline,omitempty"`
	OK                bool                `json:"ok"`
	ErrorCode         string              `json:"error_code,omitempty"`
	Error             string              `json:"error,omitempty"`
	Handshake         *HandshakeResponse  `json:"handshake,omitempty"`
	Init              *InitResponse       `json:"init,omitempty"`
	Register          *RegisterResponse   `json:"register,omitempty"`
	Invoke            *InvokeResponse     `json:"invoke,omitempty"`
	Stream            *StreamOpenResponse `json:"stream,omitempty"`
}

type HandshakeRequest struct {
	ABIVersion   string   `json:"abi_version"`
	PID          int      `json:"pid,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

type HandshakeResponse struct {
	Protocol          string   `json:"protocol"`
	ABIVersion        string   `json:"abi_version"`
	RuntimeInstanceID string   `json:"runtime_instance_id"`
	ControlChannel    string   `json:"control_channel"`
	Commands          []string `json:"commands"`
	Capabilities      []string `json:"capabilities,omitempty"`
}

type InitRequest struct {
	Capabilities []string `json:"capabilities,omitempty"`
}

type InitResponse struct {
	PluginID          string `json:"plugin_id"`
	ArtifactID        string `json:"artifact_id"`
	RuntimeInstanceID string `json:"runtime_instance_id"`
	Generation        int64  `json:"generation"`
	ConfigJSON        string `json:"config_json,omitempty"`
	State             string `json:"state"`
}

type RegisterRequest struct {
	ExtensionPoint       string                `json:"extension_point,omitempty"`
	HandlerID            string                `json:"handler_id,omitempty"`
	FailPolicy           string                `json:"fail_policy,omitempty"`
	TimeoutMS            int64                 `json:"timeout_ms,omitempty"`
	SchemaVersion        int                   `json:"schema_version,omitempty"`
	DeclaredCapabilities []string              `json:"declared_capabilities,omitempty"`
	Handlers             []HandlerRegistration `json:"handlers,omitempty"`
}

type RegisterResponse struct {
	Registrations        []HandlerRegistration `json:"registrations"`
	DeclaredCapabilities []string              `json:"declared_capabilities,omitempty"`
}

type InvokeRequest struct {
	ExtensionPoint string                   `json:"extension_point"`
	HandlerID      string                   `json:"handler_id"`
	FailPolicy     string                   `json:"fail_policy,omitempty"`
	ConfigJSON     json.RawMessage          `json:"config_json,omitempty"`
	RouteResolve   *api.RouteResolveRequest `json:"route_resolve,omitempty"`
	RuleEvaluate   *api.RuleEvaluateRequest `json:"rule_evaluate,omitempty"`
	StatusPing     *api.StatusPingRequest   `json:"status_ping,omitempty"`
}

type InvokeResponse struct {
	ExtensionPoint string                    `json:"extension_point"`
	HandlerID      string                    `json:"handler_id"`
	OK             bool                      `json:"ok"`
	Valid          *bool                     `json:"valid,omitempty"`
	RouteDecision  *api.RouteDecision        `json:"route_decision,omitempty"`
	RuleDecision   *api.RuleEvaluateDecision `json:"rule_decision,omitempty"`
	StatusResponse *api.StatusPingResponse   `json:"status_response,omitempty"`
	Reason         string                    `json:"reason,omitempty"`
	ErrorCode      string                    `json:"error_code,omitempty"`
	Error          string                    `json:"error,omitempty"`
}

type StreamOpenRequest struct {
	ExtensionPoint      string             `json:"extension_point"`
	HandlerID           string             `json:"handler_id"`
	FailPolicy          string             `json:"fail_policy,omitempty"`
	Protocol            string             `json:"protocol"`
	StreamID            string             `json:"stream_id"`
	ConnectionID        string             `json:"connection_id,omitempty"`
	TraceID             string             `json:"trace_id,omitempty"`
	PeerAddr            string             `json:"peer_addr,omitempty"`
	LocalAddr           string             `json:"local_addr,omitempty"`
	EffectiveSourceAddr string             `json:"effective_source_addr,omitempty"`
	Metadata            map[string]string  `json:"metadata,omitempty"`
	Ingress             api.IngressContext `json:"ingress"`
	DeadlineUnixMS      int64              `json:"deadline_unix_ms,omitempty"`
}

type TakeoverAction string

const (
	TakeoverActionHandled TakeoverAction = "handled"
	TakeoverActionNext    TakeoverAction = "next"
	TakeoverActionCore    TakeoverAction = "core"
)

type StreamOpenResponse struct {
	Connected           bool              `json:"connected"`
	Protocol            string            `json:"protocol"`
	StreamID            string            `json:"stream_id"`
	Action              TakeoverAction    `json:"action,omitempty"`
	Endpoint            string            `json:"endpoint"`
	ReplacementEndpoint string            `json:"replacement_endpoint,omitempty"`
	EndpointType        string            `json:"endpoint_type,omitempty"`
	EffectiveSourceAddr string            `json:"effective_source_addr,omitempty"`
	Metadata            map[string]string `json:"metadata,omitempty"`
}

type StreamCloseRequest struct {
	Protocol string `json:"protocol"`
	StreamID string `json:"stream_id"`
	Reason   string `json:"reason,omitempty"`
}

type Service struct {
	Registrations  []HandlerRegistration
	Capabilities   []string
	ConfigValidate func(context.Context, json.RawMessage) (bool, string, error)
	RouteResolve   func(context.Context, api.RouteResolveRequest) (api.RouteDecision, error)
	RuleEvaluate   func(context.Context, api.RuleEvaluateRequest) (api.RuleEvaluateDecision, error)
	StatusPing     func(context.Context, api.StatusPingRequest) (api.StatusPingResponse, error)
	StreamOpen     func(context.Context, StreamOpenRequest) (StreamOpenResponse, error)
	StreamClose    func(context.Context, StreamCloseRequest) error
}

func NewFromEnv() (Client, error) {
	socket, err := controlSocketFromEnv()
	if err != nil {
		return Client{}, err
	}
	generation, _ := strconv.ParseInt(os.Getenv(EnvGeneration), 10, 64)
	return Client{
		SocketPath:        socket,
		PluginID:          os.Getenv(EnvPluginID),
		ArtifactID:        os.Getenv(EnvArtifactID),
		RuntimeInstanceID: os.Getenv(EnvRuntimeID),
		Generation:        generation,
		Timeout:           5 * time.Second,
	}, nil
}

func (c Client) Startup(ctx context.Context, handlers []HandlerRegistration, capabilities []string) error {
	if _, err := c.Send(ctx, CommandHandshake, HandshakeRequest{
		ABIVersion:   ABIVersion,
		PID:          os.Getpid(),
		Capabilities: capabilities,
	}); err != nil {
		return err
	}
	if _, err := c.Send(ctx, CommandInit, InitRequest{Capabilities: capabilities}); err != nil {
		return err
	}
	_, err := c.Send(ctx, CommandRegister, RegisterRequest{
		Handlers:             handlers,
		DeclaredCapabilities: capabilities,
	})
	return err
}

func (c Client) Run(ctx context.Context, service Service) error {
	if err := c.Startup(ctx, service.Registrations, service.Capabilities); err != nil {
		return err
	}
	<-ctx.Done()
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil
	}
	return ctx.Err()
}

func (s Service) HandleControlRequest(ctx context.Context, req ControlRequest) ControlResponse {
	resp := ControlResponse{
		RequestID:         req.RequestID,
		Command:           req.Command,
		Protocol:          Protocol,
		PluginID:          req.PluginID,
		ArtifactID:        req.ArtifactID,
		RuntimeInstanceID: req.RuntimeInstanceID,
		Generation:        req.Generation,
		TraceID:           req.TraceID,
		DeadlineUnixMS:    req.DeadlineUnixMS,
	}
	if ctx == nil {
		ctx = context.Background()
	}
	switch req.Command {
	case CommandInvoke:
		var invoke InvokeRequest
		if err := json.Unmarshal(req.Payload, &invoke); err != nil {
			return controlErrorResponse(resp, "schema_invalid", err.Error())
		}
		out := InvokeResponse{ExtensionPoint: invoke.ExtensionPoint, HandlerID: invoke.HandlerID, OK: true}
		switch {
		case invoke.ConfigJSON != nil:
			if s.ConfigValidate == nil {
				return controlErrorResponse(resp, "not_implemented", "config.validate/v1 handler is not configured")
			}
			valid, reason, err := s.ConfigValidate(ctx, invoke.ConfigJSON)
			if err != nil {
				return controlErrorResponse(resp, "handler_failed", err.Error())
			}
			out.Valid = &valid
			out.Reason = reason
		case invoke.RouteResolve != nil:
			if s.RouteResolve == nil {
				return controlErrorResponse(resp, "not_implemented", "route.resolve/v1 handler is not configured")
			}
			decision, err := s.RouteResolve(ctx, *invoke.RouteResolve)
			if err != nil {
				return controlErrorResponse(resp, "handler_failed", err.Error())
			}
			out.RouteDecision = &decision
		case invoke.RuleEvaluate != nil:
			if s.RuleEvaluate == nil {
				return controlErrorResponse(resp, "not_implemented", "rule.evaluate/v1 handler is not configured")
			}
			decision, err := s.RuleEvaluate(ctx, *invoke.RuleEvaluate)
			if err != nil {
				return controlErrorResponse(resp, "handler_failed", err.Error())
			}
			out.RuleDecision = &decision
		case invoke.StatusPing != nil:
			if s.StatusPing == nil {
				return controlErrorResponse(resp, "not_implemented", "status.ping/v1 handler is not configured")
			}
			status, err := s.StatusPing(ctx, *invoke.StatusPing)
			if err != nil {
				return controlErrorResponse(resp, "handler_failed", err.Error())
			}
			out.StatusResponse = &status
		default:
			return controlErrorResponse(resp, "schema_invalid", "invoke payload does not contain a supported request")
		}
		resp.OK = true
		resp.Invoke = &out
		return resp
	case CommandStreamOpen:
		if s.StreamOpen == nil {
			return controlErrorResponse(resp, "not_implemented", "stream_open handler is not configured")
		}
		var stream StreamOpenRequest
		if err := json.Unmarshal(req.Payload, &stream); err != nil {
			return controlErrorResponse(resp, "schema_invalid", err.Error())
		}
		out, err := s.StreamOpen(ctx, stream)
		if err != nil {
			return controlErrorResponse(resp, "handler_failed", err.Error())
		}
		resp.OK = true
		resp.Stream = &out
		return resp
	case CommandStreamClose:
		if s.StreamClose == nil {
			resp.OK = true
			return resp
		}
		var stream StreamCloseRequest
		if err := json.Unmarshal(req.Payload, &stream); err != nil {
			return controlErrorResponse(resp, "schema_invalid", err.Error())
		}
		if err := s.StreamClose(ctx, stream); err != nil {
			return controlErrorResponse(resp, "handler_failed", err.Error())
		}
		resp.OK = true
		return resp
	default:
		return controlErrorResponse(resp, "unknown_command", "unsupported sandbox service command")
	}
}

func controlErrorResponse(resp ControlResponse, code, message string) ControlResponse {
	resp.OK = false
	resp.ErrorCode = code
	resp.Error = message
	return resp
}

func (c Client) Send(ctx context.Context, command string, payload any) (ControlResponse, error) {
	if c.SocketPath == "" {
		return ControlResponse{}, errors.New("sandbox control socket path is empty")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return ControlResponse{}, err
	}
	if len(body) > maxPayloadBytes {
		return ControlResponse{}, fmt.Errorf("sandbox control payload exceeds %d bytes", maxPayloadBytes)
	}
	req := ControlRequest{
		RequestID:         fmt.Sprintf("sdk-%d", time.Now().UnixNano()),
		Command:           command,
		Protocol:          Protocol,
		PluginID:          c.PluginID,
		ArtifactID:        c.ArtifactID,
		RuntimeInstanceID: c.RuntimeInstanceID,
		Generation:        c.Generation,
		Payload:           body,
	}
	if deadline, ok := ctx.Deadline(); ok {
		req.DeadlineUnixMS = deadline.UnixMilli()
	}
	frame, err := json.Marshal(req)
	if err != nil {
		return ControlResponse{}, err
	}
	if len(frame) > maxFrameBytes {
		return ControlResponse{}, fmt.Errorf("sandbox control frame exceeds %d bytes", maxFrameBytes)
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.SocketPath)
	if err != nil {
		return ControlResponse{}, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := conn.Write(append(frame, '\n')); err != nil {
		return ControlResponse{}, err
	}
	var resp ControlResponse
	limited := &io.LimitedReader{R: conn, N: maxFrameBytes + 1}
	if err := json.NewDecoder(limited).Decode(&resp); err != nil {
		return ControlResponse{}, err
	}
	if limited.N <= 0 {
		return ControlResponse{}, fmt.Errorf("sandbox control response exceeds %d bytes", maxFrameBytes)
	}
	if !resp.OK {
		return resp, fmt.Errorf("%s failed: %s %s", command, resp.ErrorCode, resp.Error)
	}
	return resp, nil
}

func controlSocketFromEnv() (string, error) {
	value := strings.TrimSpace(os.Getenv(EnvControl))
	if value == "" {
		return "", fmt.Errorf("%s is required", EnvControl)
	}
	value = strings.TrimPrefix(value, "unix://")
	if value == "" {
		return "", fmt.Errorf("%s is empty", EnvControl)
	}
	return value, nil
}
