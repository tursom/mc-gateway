package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	stdplugin "plugin"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tursom/mc-gateway/internal/pluginmanager"
	"github.com/tursom/mc-gateway/plugin/api"
)

type pluginHostControlServer struct {
	protocol  string
	startedAt int64

	mu         sync.Mutex
	pluginID   string
	artifactID string
	instance   api.Plugin
	gateway    *pluginmanager.Gateway
	state      string
	updatedAt  int64
}

func runPluginHostCLI(args []string) (bool, int) {
	if len(args) < 1 || args[0] != "plugin-host" {
		return false, 0
	}
	if len(args) < 2 {
		printPluginHostUsage()
		return true, 2
	}
	var err error
	switch args[1] {
	case "handshake":
		err = runPluginHostHandshakeCLI(args[2:], os.Stdout)
	case "serve":
		err = runPluginHostServeCLI(args[2:])
	default:
		printPluginHostUsage()
		return true, 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return true, 1
	}
	return true, 0
}

func printPluginHostUsage() {
	fmt.Fprintln(os.Stderr, "usage: gateway plugin-host handshake [--protocol mc-gateway-plugin-host/v1]")
	fmt.Fprintln(os.Stderr, "       gateway plugin-host serve --control-socket PATH [--protocol mc-gateway-plugin-host/v1]")
}

func runPluginHostHandshakeCLI(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("plugin-host handshake", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	protocol := flags.String("protocol", pluginmanager.PluginHostProtocol, "plugin-host protocol")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("plugin-host handshake does not accept positional arguments")
	}
	if err := pluginmanager.ValidatePluginHostProtocol(*protocol); err != nil {
		return err
	}
	handshake := pluginmanager.NewPluginHostHandshake(*protocol, os.Getpid(), os.Getppid(), time.Now().Unix())
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(handshake)
}

func runPluginHostServeCLI(args []string) error {
	flags := flag.NewFlagSet("plugin-host serve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	controlSocket := flags.String("control-socket", "", "Unix-domain control socket path")
	protocol := flags.String("protocol", pluginmanager.PluginHostProtocol, "plugin-host protocol")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("plugin-host serve does not accept positional arguments")
	}
	return servePluginHostControl(*controlSocket, *protocol)
}

func servePluginHostControl(socketPath, protocol string) error {
	socketPath = strings.TrimSpace(socketPath)
	if socketPath == "" {
		return fmt.Errorf("plugin-host serve requires --control-socket")
	}
	if err := pluginmanager.ValidatePluginHostProtocol(protocol); err != nil {
		return err
	}
	if err := preparePluginHostSocketPath(socketPath); err != nil {
		return err
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(socketPath)

	server := &pluginHostControlServer{protocol: protocol, startedAt: time.Now().Unix(), state: pluginmanager.RuntimeNotLoaded}
	shutdown := make(chan struct{})
	var closeOnce sync.Once
	stop := func() {
		closeOnce.Do(func() {
			close(shutdown)
			_ = listener.Close()
		})
	}

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-shutdown:
				return nil
			default:
				return err
			}
		}
		go handlePluginHostControlConn(conn, server, stop)
	}
}

func preparePluginHostSocketPath(socketPath string) error {
	if info, err := os.Stat(socketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("control socket path %q already exists and is not a socket", socketPath)
		}
		if err := os.Remove(socketPath); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir := filepath.Dir(socketPath)
	if dir != "." && dir != "" {
		return os.MkdirAll(dir, 0755)
	}
	return nil
}

func handlePluginHostControlConn(conn net.Conn, server *pluginHostControlServer, stop func()) {
	defer conn.Close()
	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)
	for {
		var req pluginmanager.PluginHostControlRequest
		if err := decoder.Decode(&req); err != nil {
			if !errors.Is(err, io.EOF) {
				_ = encoder.Encode(pluginmanager.PluginHostControlResponse{
					OK:    false,
					Code:  "invalid_json",
					Error: err.Error(),
				})
			}
			return
		}
		resp, shouldStop, stream := handlePluginHostControlRequest(server, req)
		if err := encoder.Encode(resp); err != nil {
			if stream != nil {
				_ = stream.Close()
			}
			return
		}
		if stream != nil {
			relayPluginHostStream(conn, stream)
			return
		}
		if shouldStop {
			stop()
			return
		}
	}
}

func handlePluginHostControlRequest(server *pluginHostControlServer, req pluginmanager.PluginHostControlRequest) (pluginmanager.PluginHostControlResponse, bool, net.Conn) {
	command := strings.TrimSpace(req.Command)
	resp := pluginmanager.PluginHostControlResponse{RequestID: req.RequestID}
	switch command {
	case pluginmanager.PluginHostCommandHandshake:
		protocol := strings.TrimSpace(req.Protocol)
		if protocol == "" {
			protocol = server.protocol
		}
		if err := pluginmanager.ValidatePluginHostProtocol(protocol); err != nil {
			resp.Code = "invalid_protocol"
			resp.Error = err.Error()
			return resp, false, nil
		}
		handshake := pluginmanager.NewPluginHostHandshake(protocol, os.Getpid(), os.Getppid(), server.startedAt)
		resp.OK = true
		resp.Handshake = &handshake
		return resp, false, nil
	case pluginmanager.PluginHostCommandShutdown:
		if err := validatePluginHostRequestProtocol(req.Protocol); err != nil {
			resp.Code = "invalid_protocol"
			resp.Error = err.Error()
			return resp, false, nil
		}
		resp.OK = true
		return resp, true, nil
	case pluginmanager.PluginHostCommandInit:
		if err := validatePluginHostRequestProtocol(req.Protocol); err != nil {
			resp.Code = "invalid_protocol"
			resp.Error = err.Error()
			return resp, false, nil
		}
		var payload pluginmanager.PluginHostInitRequest
		if err := json.Unmarshal(req.Payload, &payload); err != nil {
			resp.Code = "invalid_request"
			resp.Error = err.Error()
			return resp, false, nil
		}
		lifecycle, err := server.initPlugin(payload)
		if err != nil {
			resp.Code = "init_failed"
			resp.Error = err.Error()
			return resp, false, nil
		}
		resp.OK = true
		resp.Lifecycle = &lifecycle
		return resp, false, nil
	case pluginmanager.PluginHostCommandReloadConfig:
		if err := validatePluginHostRequestProtocol(req.Protocol); err != nil {
			resp.Code = "invalid_protocol"
			resp.Error = err.Error()
			return resp, false, nil
		}
		var payload pluginmanager.PluginHostReloadConfigRequest
		if len(req.Payload) > 0 {
			if err := json.Unmarshal(req.Payload, &payload); err != nil {
				resp.Code = "invalid_request"
				resp.Error = err.Error()
				return resp, false, nil
			}
		}
		lifecycle, err := server.reloadConfig(payload.ConfigJSON)
		if err != nil {
			resp.Code = "reload_failed"
			resp.Error = err.Error()
			return resp, false, nil
		}
		resp.OK = true
		resp.Lifecycle = &lifecycle
		return resp, false, nil
	case pluginmanager.PluginHostCommandDrain:
		if err := validatePluginHostRequestProtocol(req.Protocol); err != nil {
			resp.Code = "invalid_protocol"
			resp.Error = err.Error()
			return resp, false, nil
		}
		lifecycle, err := server.markDraining()
		if err != nil {
			resp.Code = "drain_failed"
			resp.Error = err.Error()
			return resp, false, nil
		}
		resp.OK = true
		resp.Lifecycle = &lifecycle
		return resp, false, nil
	case pluginmanager.PluginHostCommandDestroy, pluginmanager.PluginHostCommandStop:
		if err := validatePluginHostRequestProtocol(req.Protocol); err != nil {
			resp.Code = "invalid_protocol"
			resp.Error = err.Error()
			return resp, false, nil
		}
		lifecycle, err := server.destroyPlugin()
		if err != nil {
			resp.Code = "destroy_failed"
			resp.Error = err.Error()
			return resp, false, nil
		}
		resp.OK = true
		resp.Lifecycle = &lifecycle
		return resp, false, nil
	case pluginmanager.PluginHostCommandUpstream:
		if err := validatePluginHostRequestProtocol(req.Protocol); err != nil {
			resp.Code = "invalid_protocol"
			resp.Error = err.Error()
			return resp, false, nil
		}
		var payload pluginmanager.PluginHostUpstreamConnectRequest
		if err := json.Unmarshal(req.Payload, &payload); err != nil {
			resp.Code = "invalid_request"
			resp.Error = err.Error()
			return resp, false, nil
		}
		stream, err := server.connectUpstream(payload)
		if errors.Is(err, api.ErrPass) {
			resp.Code = "pass"
			resp.Error = err.Error()
			return resp, false, nil
		}
		if errors.Is(err, api.ErrBlocked) {
			resp.Code = "blocked"
			resp.Error = err.Error()
			return resp, false, nil
		}
		if err != nil {
			resp.Code = "upstream_failed"
			resp.Error = err.Error()
			return resp, false, nil
		}
		if stream == nil {
			resp.Code = "pass"
			resp.Error = api.ErrPass.Error()
			return resp, false, nil
		}
		resp.OK = true
		resp.Upstream = &pluginmanager.PluginHostUpstream{Connected: true}
		return resp, false, stream
	case pluginmanager.PluginHostCommandStreamProxy:
		if err := validatePluginHostRequestProtocol(req.Protocol); err != nil {
			resp.Code = "invalid_protocol"
			resp.Error = err.Error()
			return resp, false, nil
		}
		var payload pluginmanager.PluginHostUpstreamConnectRequest
		if err := json.Unmarshal(req.Payload, &payload); err != nil {
			resp.Code = "invalid_request"
			resp.Error = err.Error()
			return resp, false, nil
		}
		stream, err := server.streamProxy(payload)
		if errors.Is(err, api.ErrPass) {
			resp.Code = "pass"
			resp.Error = err.Error()
			return resp, false, nil
		}
		if errors.Is(err, api.ErrBlocked) {
			resp.Code = "blocked"
			resp.Error = err.Error()
			return resp, false, nil
		}
		if err != nil {
			resp.Code = "stream_proxy_failed"
			resp.Error = err.Error()
			return resp, false, nil
		}
		if stream == nil {
			resp.Code = "pass"
			resp.Error = api.ErrPass.Error()
			return resp, false, nil
		}
		resp.OK = true
		resp.Stream = &pluginmanager.PluginHostStreamProxy{Connected: true}
		return resp, false, stream
	default:
		resp.Code = "invalid_command"
		if command == "" {
			resp.Error = "plugin-host control command is required"
		} else {
			resp.Error = fmt.Sprintf("unknown plugin-host control command %q", command)
		}
		return resp, false, nil
	}
}

func (s *pluginHostControlServer) initPlugin(req pluginmanager.PluginHostInitRequest) (pluginmanager.PluginHostLifecycle, error) {
	pluginID := strings.TrimSpace(req.PluginID)
	artifactID := strings.TrimSpace(req.ArtifactID)
	artifactPath := strings.TrimSpace(req.ArtifactPath)
	if pluginID == "" {
		return pluginmanager.PluginHostLifecycle{}, errors.New("plugin_id is required")
	}
	if artifactID == "" {
		return pluginmanager.PluginHostLifecycle{}, errors.New("artifact_id is required")
	}
	if artifactPath == "" {
		return pluginmanager.PluginHostLifecycle{}, errors.New("artifact_path is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.instance != nil {
		return pluginmanager.PluginHostLifecycle{}, errors.New("plugin-host already has an initialized plugin")
	}
	instance, err := instantiatePluginHostPlugin(artifactPath, req.EntrySymbol, req.ConfigJSON)
	if err != nil {
		return pluginmanager.PluginHostLifecycle{}, err
	}
	gateway := pluginmanager.NewGateway(pluginID, nil, nil, nil)
	if err := instance.Init(gateway); err != nil {
		return pluginmanager.PluginHostLifecycle{}, err
	}
	now := time.Now().Unix()
	s.pluginID = pluginID
	s.artifactID = artifactID
	s.instance = instance
	s.gateway = gateway
	s.state = pluginmanager.RuntimeEnabled
	s.updatedAt = now
	return s.lifecycleLocked(now), nil
}

func instantiatePluginHostPlugin(artifactPath, entrySymbol, configJSON string) (api.Plugin, error) {
	opened, err := stdplugin.Open(artifactPath)
	if err != nil {
		return nil, err
	}
	symbolName := strings.TrimSpace(entrySymbol)
	if symbolName == "" {
		symbolName = "Plugin"
	}
	symbol, err := opened.Lookup(symbolName)
	if err != nil {
		return nil, err
	}
	factory, ok := symbol.(func() api.Plugin)
	if !ok {
		return nil, fmt.Errorf("plugin symbol %q has invalid signature", symbolName)
	}
	instance := factory()
	if err := reloadPluginHostConfig(instance, configJSON); err != nil {
		return nil, err
	}
	return instance, nil
}

func reloadPluginHostConfig(instance api.Plugin, configJSON string) error {
	if instance == nil {
		return errors.New("plugin instance is nil")
	}
	cfg := instance.NewConfigObj()
	if cfg != nil && configJSON != "" && pluginHostCanUnmarshalInto(cfg) {
		if err := json.Unmarshal([]byte(configJSON), cfg); err != nil {
			return fmt.Errorf("decode plugin config: %w", err)
		}
	}
	return instance.ReloadConfig(cfg)
}

func pluginHostCanUnmarshalInto(value any) bool {
	if value == nil {
		return false
	}
	kind := reflect.TypeOf(value).Kind()
	return kind == reflect.Pointer || kind == reflect.Map || kind == reflect.Slice
}

func (s *pluginHostControlServer) reloadConfig(configJSON string) (pluginmanager.PluginHostLifecycle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.instance == nil {
		return pluginmanager.PluginHostLifecycle{}, errors.New("plugin-host has no initialized plugin")
	}
	if err := reloadPluginHostConfig(s.instance, configJSON); err != nil {
		return pluginmanager.PluginHostLifecycle{}, err
	}
	now := time.Now().Unix()
	s.updatedAt = now
	return s.lifecycleLocked(now), nil
}

func (s *pluginHostControlServer) markDraining() (pluginmanager.PluginHostLifecycle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.instance == nil {
		return pluginmanager.PluginHostLifecycle{}, errors.New("plugin-host has no initialized plugin")
	}
	now := time.Now().Unix()
	s.state = pluginmanager.RuntimeDraining
	s.updatedAt = now
	return s.lifecycleLocked(now), nil
}

func (s *pluginHostControlServer) destroyPlugin() (pluginmanager.PluginHostLifecycle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.instance == nil {
		now := time.Now().Unix()
		s.state = pluginmanager.RuntimeDisabled
		s.updatedAt = now
		return s.lifecycleLocked(now), nil
	}
	if err := s.instance.Destroy(); err != nil {
		return pluginmanager.PluginHostLifecycle{}, err
	}
	now := time.Now().Unix()
	s.instance = nil
	s.gateway = nil
	s.state = pluginmanager.RuntimeDisabled
	s.updatedAt = now
	return s.lifecycleLocked(now), nil
}

func (s *pluginHostControlServer) connectUpstream(payload pluginmanager.PluginHostUpstreamConnectRequest) (net.Conn, error) {
	s.mu.Lock()
	gateway := s.gateway
	state := s.state
	s.mu.Unlock()
	if gateway == nil || state == pluginmanager.RuntimeDisabled || state == pluginmanager.RuntimeNotLoaded {
		return nil, errors.New("plugin-host has no initialized plugin")
	}
	req := pluginHostUpstreamRequest(payload)
	if hook, ok := gateway.UpstreamConnectHandler(); ok {
		if accept := hook.Acceptor(); accept != nil && !accept(req) {
			return nil, api.ErrPass
		}
		return hook.Handler()(req)
	}
	if hook, ok := gateway.LegacyUpstreamHandler(); ok {
		if accept := hook.Acceptor(); accept != nil && !accept(nil, payload.Upstream) {
			return nil, api.ErrPass
		}
		return hook.Handler()(nil, payload.Upstream)
	}
	return nil, api.ErrPass
}

func (s *pluginHostControlServer) streamProxy(payload pluginmanager.PluginHostUpstreamConnectRequest) (net.Conn, error) {
	s.mu.Lock()
	gateway := s.gateway
	state := s.state
	s.mu.Unlock()
	if gateway == nil || state == pluginmanager.RuntimeDisabled || state == pluginmanager.RuntimeNotLoaded {
		return nil, errors.New("plugin-host has no initialized plugin")
	}
	req := pluginHostUpstreamRequest(payload)
	if hook, ok := gateway.UpstreamConnectHandler(); ok {
		if accept := hook.Acceptor(); accept != nil && !accept(req) {
			return nil, api.ErrPass
		}
		return hook.Handler()(req)
	}
	return nil, api.ErrPass
}

func pluginHostUpstreamRequest(payload pluginmanager.PluginHostUpstreamConnectRequest) api.UpstreamConnectRequest {
	ctx := context.Background()
	if payload.DeadlineUnixMS > 0 {
		ctx, _ = context.WithDeadline(ctx, time.UnixMilli(payload.DeadlineUnixMS))
	}
	return api.UpstreamConnectRequest{
		Context:          ctx,
		Host:             payload.Host,
		Upstream:         payload.Upstream,
		InitialData:      append([]byte(nil), payload.InitialData...),
		Metadata:         payload.Metadata,
		ConnectionID:     payload.ConnectionID,
		TraceID:          payload.TraceID,
		SourceAddr:       payload.SourceAddr,
		ServerHost:       payload.ServerHost,
		RawServerHost:    payload.RawServerHost,
		ProtocolVersion:  payload.ProtocolVersion,
		NextState:        payload.NextState,
		RouteID:          payload.RouteID,
		RouteTags:        append([]string(nil), payload.RouteTags...),
		UpstreamRaw:      payload.UpstreamRaw,
		UpstreamProtocol: payload.UpstreamProtocol,
		UpstreamAddress:  payload.UpstreamAddress,
		Transport:        payload.Transport,
		ServiceName:      payload.ServiceName,
		ListenerPort:     payload.ListenerPort,
	}
}

func relayPluginHostStream(controlConn, pluginConn net.Conn) {
	defer pluginConn.Close()
	ack := make([]byte, 1)
	_ = controlConn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(controlConn, ack); err != nil {
		return
	}
	_ = controlConn.SetReadDeadline(time.Time{})
	var wg sync.WaitGroup
	wg.Add(2)
	go copyAndClosePluginHostStream(&wg, pluginConn, controlConn)
	go copyAndClosePluginHostStream(&wg, controlConn, pluginConn)
	wg.Wait()
}

func copyAndClosePluginHostStream(wg *sync.WaitGroup, dst, src net.Conn) {
	defer wg.Done()
	_, _ = io.Copy(dst, src)
	if closer, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = closer.CloseWrite()
		return
	}
	_ = dst.Close()
}

func (s *pluginHostControlServer) lifecycleLocked(now int64) pluginmanager.PluginHostLifecycle {
	state := s.state
	if state == "" {
		state = pluginmanager.RuntimeNotLoaded
	}
	var hooks []string
	if s.gateway != nil {
		for key := range s.gateway.RegisteredHooks() {
			hooks = append(hooks, key)
		}
		sort.Strings(hooks)
	}
	return pluginmanager.PluginHostLifecycle{
		PluginID:        s.pluginID,
		ArtifactID:      s.artifactID,
		State:           state,
		RegisteredHooks: hooks,
		StartedAt:       s.startedAt,
		UpdatedAt:       now,
	}
}

func validatePluginHostRequestProtocol(protocol string) error {
	protocol = strings.TrimSpace(protocol)
	if protocol == "" {
		return nil
	}
	return pluginmanager.ValidatePluginHostProtocol(protocol)
}
