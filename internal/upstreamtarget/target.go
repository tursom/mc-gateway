// internal/upstreamtarget/target.go 把上游目标字符串解析为传输协议和拨号地址。

package upstreamtarget

import "strings"

type Protocol string

const (
	ProtocolTCP     Protocol = "tcp"
	ProtocolQUIC    Protocol = "quic"
	ProtocolKCP     Protocol = "kcp"
	ProtocolHAProxy Protocol = "haproxy"
)

type Target struct {
	Protocol Protocol
	Address  string
}

func Parse(raw string) Target {
	if address, ok := strings.CutPrefix(raw, "quic://"); ok {
		return Target{Protocol: ProtocolQUIC, Address: address}
	}
	if address, ok := strings.CutPrefix(raw, "kcp://"); ok {
		return Target{Protocol: ProtocolKCP, Address: address}
	}
	if address, ok := strings.CutPrefix(raw, "haproxy://"); ok {
		return Target{Protocol: ProtocolHAProxy, Address: address}
	}
	return Target{Protocol: ProtocolTCP, Address: raw}
}
