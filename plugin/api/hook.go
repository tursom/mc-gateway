package api

import (
	"context"
	"errors"
	"net"
	"unsafe"
)

var (
	UnsupportedHookType = errors.New("unsupported hook type")
	ErrPass             = errors.New("plugin handler pass")
	ErrBlocked          = errors.New("plugin handler blocked")
)

type (
	HookType[Accept, Handler any] struct {
		key string
	}

	HookHandler[Accept, Handler any] struct {
		acceptor Accept
		handler  Handler
	}

	UpstreamConnectRequest struct {
		Context          context.Context
		Source           net.Conn
		Host             string
		Upstream         string
		InitialData      []byte
		Metadata         map[string]string
		ConnectionID     string
		TraceID          string
		SourceAddr       string
		ServerHost       string
		RawServerHost    string
		ProtocolVersion  int
		NextState        int
		RouteID          string
		RouteTags        []string
		UpstreamRaw      string
		UpstreamProtocol string
		UpstreamAddress  string
		Transport        string
		ServiceName      string
		ListenerPort     int
	}

	UpstreamConnectAcceptor func(UpstreamConnectRequest) bool
	UpstreamConnectHandler  func(UpstreamConnectRequest) (net.Conn, error)
)

var (
	HookUpstreamConnect = HookType[
		UpstreamConnectAcceptor,
		UpstreamConnectHandler,
	]{
		key: "upstream.connect/v1",
	}

	HookUpstream = HookType[
		func(source net.Conn, host string) bool,
		func(source net.Conn, host string) (net.Conn, error),
	]{
		key: "upstream",
	}
)

func (h HookType[Accept, Handler]) Key() string {
	return h.key
}

func (h HookType[Accept, Handler]) AsAny() HookType[any, any] {
	return *(*HookType[any, any])(unsafe.Pointer(&h))
}

func (h HookHandler[Acceptor, Handler]) Acceptor() Acceptor {
	return h.acceptor
}

func (h HookHandler[Acceptor, Handler]) Handler() Handler {
	return h.handler
}
