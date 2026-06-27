// cmd/gateway/haproxy.go 为需要 HAProxy PROXY 头的上游 TCP 连接先写入代理头，再回放 Minecraft 流量。

package main

import (
	"net"

	proxyproto "github.com/pires/go-proxyproto"
	"github.com/rs/zerolog/log"
)

func haProxyUpstream(source net.Conn, host string) net.Conn {
	target, err := net.ResolveTCPAddr("tcp", host)
	if err != nil {
		gatewayMetrics.UpstreamDialError()
		log.Err(err).Msg("failed to resolve TCP address")
		return nil
	}

	conn, err := tcpDialer.Dial("tcp", target.String())
	if err != nil {
		gatewayMetrics.UpstreamDialError()
		log.Err(err).Msg("failed to dial TCP")
		return nil
	}
	setSocketOptions(conn)

	sourceAddr, err := net.ResolveTCPAddr(
		source.RemoteAddr().Network(),
		source.RemoteAddr().String(),
	)
	if err != nil {
		log.Err(err).Msg("failed to resolve TCP address")
		conn.Close()
		return nil
	}

	TransportProtocol := proxyproto.TCPv4
	if sourceAddr.IP.To4() == nil {
		TransportProtocol = proxyproto.TCPv6
	}

	header := &proxyproto.Header{
		Version:           1,
		Command:           proxyproto.PROXY,
		TransportProtocol: TransportProtocol,
		SourceAddr:        sourceAddr,
		DestinationAddr:   target,
	}
	// 连接建立后先写入 PROXY 头，再转发 Minecraft 首包。
	_, err = header.WriteTo(conn)
	if err != nil {
		log.Err(err).Msg("failed to write proxy header")
		conn.Close()
		return nil
	}

	return conn
}
