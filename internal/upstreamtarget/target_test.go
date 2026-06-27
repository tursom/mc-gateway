// internal/upstreamtarget/target_test.go 包含用于约束 target 行为的测试。

package upstreamtarget

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		raw      string
		protocol Protocol
		address  string
	}{
		{raw: "127.0.0.1:25565", protocol: ProtocolTCP, address: "127.0.0.1:25565"},
		{raw: "quic://127.0.0.1:25565", protocol: ProtocolQUIC, address: "127.0.0.1:25565"},
		{raw: "kcp://127.0.0.1:25565", protocol: ProtocolKCP, address: "127.0.0.1:25565"},
		{raw: "haproxy://127.0.0.1:25565", protocol: ProtocolHAProxy, address: "127.0.0.1:25565"},
		{raw: "quic://", protocol: ProtocolQUIC, address: ""},
		{raw: "http://127.0.0.1:25565", protocol: ProtocolTCP, address: "http://127.0.0.1:25565"},
	}

	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got := Parse(tt.raw)
			if got.Protocol != tt.protocol || got.Address != tt.address {
				t.Fatalf("Parse(%q) = %+v, want protocol=%q address=%q", tt.raw, got, tt.protocol, tt.address)
			}
		})
	}
}
