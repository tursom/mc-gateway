package main

import (
	"bufio"
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
	protocol   string
	startedAt  int64
	socketPath string

	mu         sync.Mutex
	pluginID   string
	artifactID string
	instance   api.Plugin
	gateway    *pluginmanager.Gateway
	state      string
	updatedAt  int64
	takeovers  map[string]*pluginHostTakeoverSession
}

type pluginHostTakeoverSession struct {
	action   chan pluginmanager.PluginHostTakeover
	complete chan error
	done     chan error
	cancel   context.CancelFunc
}

type cancelOnCloseConn struct {
	net.Conn
	once   sync.Once
	cancel context.CancelFunc
}

func (c *cancelOnCloseConn) Close() error {
	c.once.Do(c.cancel)
	return c.Conn.Close()
}

func (c *cancelOnCloseConn) CloseWrite() error {
	if closer, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return c.Conn.Close()
}

func (c *cancelOnCloseConn) CloseRead() error {
	if closer, ok := c.Conn.(interface{ CloseRead() error }); ok {
		return closer.CloseRead()
	}
	return nil
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

	server := &pluginHostControlServer{
		protocol: protocol, startedAt: time.Now().Unix(), socketPath: socketPath,
		state: pluginmanager.RuntimeNotLoaded, takeovers: make(map[string]*pluginHostTakeoverSession),
	}
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
	reader := bufio.NewReader(conn)
	encoder := json.NewEncoder(conn)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil && len(line) == 0 {
			if !errors.Is(err, io.EOF) {
				_ = encoder.Encode(pluginmanager.PluginHostControlResponse{OK: false, Code: "invalid_json", Error: err.Error()})
			}
			return
		}
		var req pluginmanager.PluginHostControlRequest
		if err := json.Unmarshal(line, &req); err != nil {
			_ = encoder.Encode(pluginmanager.PluginHostControlResponse{OK: false, Code: "invalid_json", Error: err.Error()})
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
			relayPluginHostStream(&bufferedPluginHostConn{Conn: conn, reader: reader}, stream)
			return
		}
		if shouldStop {
			stop()
			return
		}
	}
}

type bufferedPluginHostConn struct {
	net.Conn
	reader io.Reader
}

func (c *bufferedPluginHostConn) Read(p []byte) (int, error) {
	if c.reader != nil {
		n, err := c.reader.Read(p)
		if n > 0 {
			return n, nil
		}
		if !errors.Is(err, io.EOF) {
			return n, err
		}
		c.reader = nil
	}
	return c.Conn.Read(p)
}

func (c *bufferedPluginHostConn) CloseWrite() error {
	if closer, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return c.Conn.Close()
}

func (c *bufferedPluginHostConn) CloseRead() error {
	if closer, ok := c.Conn.(interface{ CloseRead() error }); ok {
		return closer.CloseRead()
	}
	return nil
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
	case pluginmanager.PluginHostCommandTakeoverOpen:
		if err := validatePluginHostRequestProtocol(req.Protocol); err != nil {
			resp.Code = "invalid_protocol"
			resp.Error = err.Error()
			return resp, false, nil
		}
		var payload pluginmanager.PluginHostTakeoverRequest
		if err := json.Unmarshal(req.Payload, &payload); err != nil {
			resp.Code = "invalid_request"
			resp.Error = err.Error()
			return resp, false, nil
		}
		stream, err := server.openTakeover(payload)
		if err != nil {
			resp.Code = "takeover_open_failed"
			resp.Error = err.Error()
			return resp, false, nil
		}
		resp.OK = true
		resp.Takeover = &pluginmanager.PluginHostTakeover{SessionID: payload.SessionID, Connected: true}
		return resp, false, stream
	case pluginmanager.PluginHostCommandTakeoverWait:
		if err := validatePluginHostRequestProtocol(req.Protocol); err != nil {
			resp.Code = "invalid_protocol"
			resp.Error = err.Error()
			return resp, false, nil
		}
		var payload pluginmanager.PluginHostTakeoverSessionRequest
		if err := json.Unmarshal(req.Payload, &payload); err != nil {
			resp.Code = "invalid_request"
			resp.Error = err.Error()
			return resp, false, nil
		}
		action, err := server.waitTakeover(payload.SessionID)
		if err != nil {
			resp.Code = "takeover_wait_failed"
			resp.Error = err.Error()
			return resp, false, nil
		}
		resp.OK = true
		resp.Takeover = &action
		return resp, false, nil
	case pluginmanager.PluginHostCommandTakeoverComplete:
		if err := validatePluginHostRequestProtocol(req.Protocol); err != nil {
			resp.Code = "invalid_protocol"
			resp.Error = err.Error()
			return resp, false, nil
		}
		var payload pluginmanager.PluginHostTakeoverSessionRequest
		if err := json.Unmarshal(req.Payload, &payload); err != nil {
			resp.Code = "invalid_request"
			resp.Error = err.Error()
			return resp, false, nil
		}
		finalErr := server.completeTakeover(payload)
		resp.OK = true
		resp.Takeover = &pluginmanager.PluginHostTakeover{SessionID: payload.SessionID}
		if finalErr != nil {
			resp.Takeover.Error = finalErr.Error()
		}
		return resp, false, nil
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
	gateway := pluginmanager.NewGateway(pluginID, nil, nil)
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

func (s *pluginHostControlServer) openTakeover(payload pluginmanager.PluginHostTakeoverRequest) (net.Conn, error) {
	s.mu.Lock()
	gateway := s.gateway
	state := s.state
	if gateway == nil || state == pluginmanager.RuntimeDisabled || state == pluginmanager.RuntimeNotLoaded {
		s.mu.Unlock()
		return nil, errors.New("plugin-host has no initialized plugin")
	}
	handler, ok := gateway.UpstreamConnectHandlerV2()
	if !ok {
		s.mu.Unlock()
		return nil, errors.New("plugin-host has no upstream.connect/v2 handler")
	}
	if payload.SessionID == "" {
		s.mu.Unlock()
		return nil, errors.New("takeover session id is required")
	}
	if _, exists := s.takeovers[payload.SessionID]; exists {
		s.mu.Unlock()
		return nil, errors.New("takeover session already exists")
	}
	takeoverCtx, cancel := context.WithCancel(context.Background())
	if payload.DeadlineUnixMS > 0 {
		var deadlineCancel context.CancelFunc
		takeoverCtx, deadlineCancel = context.WithDeadline(takeoverCtx, time.UnixMilli(payload.DeadlineUnixMS))
		baseCancel := cancel
		cancel = func() {
			deadlineCancel()
			baseCancel()
		}
	}
	session := &pluginHostTakeoverSession{
		action: make(chan pluginmanager.PluginHostTakeover, 1), complete: make(chan error, 1), done: make(chan error, 1),
		cancel: cancel,
	}
	pluginStream, relayStream, err := newPluginHostStreamPair()
	if err != nil {
		cancel()
		s.mu.Unlock()
		return nil, err
	}
	s.takeovers[payload.SessionID] = session
	s.mu.Unlock()

	flow := &pluginHostTakeoverFlow{server: s, sessionID: payload.SessionID, session: session}
	req := pluginHostTakeoverRequest(takeoverCtx, payload, pluginStream, flow)
	go func() {
		var handlerErr error
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					handlerErr = fmt.Errorf("plugin-host takeover panic: %v", rec)
				}
			}()
			handlerErr = handler(req)
		}()
		used, running := flow.handlerReturned()
		if running {
			cancel()
			_ = pluginStream.Close()
			if handlerErr == nil {
				handlerErr = errors.New("takeover continuation outlived its handler invocation")
			}
		}
		if !used {
			session.action <- pluginmanager.PluginHostTakeover{
				SessionID: payload.SessionID, Action: pluginmanager.TakeoverActionHandled, Error: errorString(handlerErr),
			}
		}
		_ = pluginStream.Close()
		session.done <- handlerErr
	}()
	return &cancelOnCloseConn{Conn: relayStream, cancel: cancel}, nil
}

func newPluginHostStreamPair() (net.Conn, net.Conn, error) {
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, nil, err
	}
	defer listener.Close()
	accept := make(chan struct {
		conn *net.TCPConn
		err  error
	}, 1)
	go func() {
		conn, acceptErr := listener.AcceptTCP()
		accept <- struct {
			conn *net.TCPConn
			err  error
		}{conn: conn, err: acceptErr}
	}()
	relay, err := net.DialTCP("tcp", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		return nil, nil, err
	}
	accepted := <-accept
	if accepted.err != nil {
		_ = relay.Close()
		return nil, nil, accepted.err
	}
	return accepted.conn, relay, nil
}

func pluginHostTakeoverRequest(ctx context.Context, payload pluginmanager.PluginHostTakeoverRequest, stream net.Conn, flow api.UpstreamConnectFlow) api.UpstreamConnectRequestV2 {
	return api.UpstreamConnectRequestV2{
		Context: ctx, ConnectionID: payload.ConnectionID, TraceID: payload.TraceID,
		PeerAddr: payload.PeerAddr, LocalAddr: payload.LocalAddr,
		Connection: api.ConnectionState{
			Stream: stream, EffectiveSourceAddr: payload.EffectiveSourceAddr,
			Metadata: clonePluginHostMetadata(payload.Metadata),
		},
		Ingress: payload.Ingress.Clone(), Flow: flow,
	}
}

type pluginHostTakeoverFlow struct {
	server    *pluginHostControlServer
	sessionID string
	session   *pluginHostTakeoverSession
	mu        sync.Mutex
	state     pluginHostTakeoverFlowState
}

type pluginHostTakeoverFlowState uint8

const (
	pluginHostTakeoverFlowAvailable pluginHostTakeoverFlowState = iota
	pluginHostTakeoverFlowRunning
	pluginHostTakeoverFlowRunningAfterHandlerReturn
	pluginHostTakeoverFlowUsed
	pluginHostTakeoverFlowExpired
)

func (f *pluginHostTakeoverFlow) Next(state api.ConnectionState) error {
	return f.continueWith(pluginmanager.TakeoverActionNext, state)
}
func (f *pluginHostTakeoverFlow) Core(state api.ConnectionState) error {
	return f.continueWith(pluginmanager.TakeoverActionCore, state)
}

func (f *pluginHostTakeoverFlow) continueWith(action pluginmanager.TakeoverAction, state api.ConnectionState) error {
	f.mu.Lock()
	switch f.state {
	case pluginHostTakeoverFlowRunning, pluginHostTakeoverFlowRunningAfterHandlerReturn, pluginHostTakeoverFlowUsed:
		f.mu.Unlock()
		return api.ErrContinuationUsed
	case pluginHostTakeoverFlowExpired:
		f.mu.Unlock()
		return errors.New("takeover continuation is no longer available")
	case pluginHostTakeoverFlowAvailable:
		f.state = pluginHostTakeoverFlowRunning
	default:
		f.mu.Unlock()
		return errors.New("takeover continuation has invalid state")
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.state = pluginHostTakeoverFlowUsed
		f.mu.Unlock()
	}()
	if state.Stream == nil {
		err := errors.New("takeover replacement stream is nil")
		f.session.action <- pluginmanager.PluginHostTakeover{SessionID: f.sessionID, Action: pluginmanager.TakeoverActionHandled, Error: err.Error()}
		return err
	}
	endpoint := filepath.Join(filepath.Dir(f.server.socketPath), "takeover-"+f.sessionID+".sock")
	_ = os.Remove(endpoint)
	listener, err := net.Listen("unix", endpoint)
	if err != nil {
		f.session.action <- pluginmanager.PluginHostTakeover{SessionID: f.sessionID, Action: pluginmanager.TakeoverActionHandled, Error: err.Error()}
		return err
	}
	defer listener.Close()
	defer os.Remove(endpoint)
	relayDone := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			relayPluginHostReplacement(conn, state.Stream)
		}
		close(relayDone)
	}()
	f.session.action <- pluginmanager.PluginHostTakeover{
		SessionID: f.sessionID, Action: action, Endpoint: endpoint,
		EffectiveSourceAddr: state.EffectiveSourceAddr, Metadata: clonePluginHostMetadata(state.Metadata),
	}
	err = <-f.session.complete
	_ = listener.Close()
	<-relayDone
	return err
}

func (f *pluginHostTakeoverFlow) handlerReturned() (used, running bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch f.state {
	case pluginHostTakeoverFlowAvailable:
		f.state = pluginHostTakeoverFlowExpired
		return false, false
	case pluginHostTakeoverFlowRunning:
		f.state = pluginHostTakeoverFlowRunningAfterHandlerReturn
		return true, true
	case pluginHostTakeoverFlowRunningAfterHandlerReturn:
		return true, true
	case pluginHostTakeoverFlowUsed:
		return true, false
	case pluginHostTakeoverFlowExpired:
		return false, false
	default:
		return false, false
	}
}

func (s *pluginHostControlServer) waitTakeover(sessionID string) (pluginmanager.PluginHostTakeover, error) {
	s.mu.Lock()
	session := s.takeovers[sessionID]
	s.mu.Unlock()
	if session == nil {
		return pluginmanager.PluginHostTakeover{}, errors.New("takeover session not found")
	}
	action := <-session.action
	if action.Action == pluginmanager.TakeoverActionHandled {
		<-session.done
		s.mu.Lock()
		delete(s.takeovers, sessionID)
		s.mu.Unlock()
	}
	return action, nil
}

func (s *pluginHostControlServer) completeTakeover(payload pluginmanager.PluginHostTakeoverSessionRequest) error {
	s.mu.Lock()
	session := s.takeovers[payload.SessionID]
	s.mu.Unlock()
	if session == nil {
		return errors.New("takeover session not found")
	}
	var downstreamErr error
	if payload.Error != "" {
		downstreamErr = errors.New(payload.Error)
	}
	session.complete <- downstreamErr
	finalErr := <-session.done
	s.mu.Lock()
	delete(s.takeovers, payload.SessionID)
	s.mu.Unlock()
	session.cancel()
	return finalErr
}

func relayPluginHostReplacement(a, b net.Conn) {
	defer a.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	go copyAndClosePluginHostStream(&wg, a, b)
	go copyAndClosePluginHostStream(&wg, b, a)
	wg.Wait()
}

func clonePluginHostMetadata(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func errorString(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
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
