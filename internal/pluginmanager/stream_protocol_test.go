package pluginmanager

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStreamProxyV1ContractDeclaresRequiredSemantics(t *testing.T) {
	contract := StreamProxyV1Contract()
	if contract.Protocol != StreamProxyProtocolV1 ||
		!contract.RequiresHalfClose ||
		!contract.RequiresDeadline ||
		!contract.RequiresBackpressure ||
		!contract.RequiresCancel ||
		!contract.RequiresByteAccounting {
		t.Fatalf("StreamProxyV1Contract() = %+v, want stable stream.proxy/v1 semantics", contract)
	}
	capabilities := strings.Join(StreamProxyCapabilities(), ",")
	for _, want := range []string{"half_close", "deadline", "backpressure", "cancel", "byte_accounting"} {
		if !strings.Contains(capabilities, StreamProxyProtocolV1+"."+want) {
			t.Fatalf("StreamProxyCapabilities() = %v, missing %s", capabilities, want)
		}
	}
}

func TestRelayTakeoverStreamPreservesHalfCloseAndBackpressure(t *testing.T) {
	root, client := newTestTCPConnPair(t)
	endpoint, plugin := newTestTCPConnPair(t)
	defer client.Close()
	defer plugin.Close()
	done := make(chan struct{})
	go func() {
		relayTakeoverStream(root, endpoint)
		close(done)
	}()

	payload := bytes.Repeat([]byte("x"), 512*1024)
	received := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 4096)
		var out []byte
		for {
			n, err := plugin.Read(buf)
			out = append(out, buf[:n]...)
			if err != nil {
				received <- out
				return
			}
			time.Sleep(100 * time.Microsecond)
		}
	}()
	if _, err := client.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		if !bytes.Equal(got, payload) {
			t.Fatalf("relayed bytes = %d, want %d", len(got), len(payload))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("slow reader did not receive half-closed payload")
	}

	if _, err := plugin.Write([]byte("reply")); err != nil {
		t.Fatal(err)
	}
	if err := plugin.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	reply, err := io.ReadAll(client)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if string(reply) != "reply" {
		t.Fatalf("reply = %q, want reply", reply)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not finish after both half-closes")
	}
}

func TestValidateStreamProxyFixtureJSON(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "plugins", "extension-ecosystem", "stream-proxy-conformance.json"))
	if err != nil {
		t.Fatalf("ReadFile(stream-proxy-conformance.json) error = %v", err)
	}
	fixtures, err := ValidateStreamProxyFixtureJSON(data)
	if err != nil {
		t.Fatalf("ValidateStreamProxyFixtureJSON() error = %v", err)
	}
	if len(fixtures) != 5 {
		t.Fatalf("fixtures = %d, want 5", len(fixtures))
	}
	encoded, err := json.Marshal(fixtures)
	if err != nil || !strings.Contains(string(encoded), StreamProxyProtocolV1) {
		t.Fatalf("Marshal fixtures = %q err=%v, want protocol in fixture", encoded, err)
	}
}

func TestValidateStreamProxyFixtureRejectsMissingSemantics(t *testing.T) {
	err := ValidateStreamProxyFixture(StreamProxyFixture{
		Name:     "stream.proxy/v1.backpressure",
		Protocol: StreamProxyProtocolV1,
		Expected: "backpressure_window_respected",
		Frames:   []StreamProxyFrame{{Type: StreamFrameData, Direction: "gateway_to_plugin", Bytes: 1}},
	})
	if err == nil || !strings.Contains(err.Error(), "missing backpressure frame") {
		t.Fatalf("ValidateStreamProxyFixture() error = %v, want missing backpressure frame", err)
	}
}

func TestValidateStreamProxyFixtureRejectsMalformedControlFrames(t *testing.T) {
	tests := []struct {
		name    string
		fixture StreamProxyFixture
		want    string
	}{
		{
			name: "half-close direction",
			fixture: StreamProxyFixture{
				Name:     "stream.proxy/v1.half_close",
				Protocol: StreamProxyProtocolV1,
				Expected: "half_close_propagated",
				Frames:   []StreamProxyFrame{{Type: StreamFrameHalfClose}},
			},
			want: "half_close frame direction is invalid",
		},
		{
			name: "cancel reason",
			fixture: StreamProxyFixture{
				Name:     "stream.proxy/v1.cancel",
				Protocol: StreamProxyProtocolV1,
				Expected: "cancel_closes_stream",
				Frames:   []StreamProxyFrame{{Type: StreamFrameCancel}},
			},
			want: "cancel frame requires reason",
		},
		{
			name: "accounting payload",
			fixture: StreamProxyFixture{
				Name:     "stream.proxy/v1.accounting",
				Protocol: StreamProxyProtocolV1,
				Expected: "byte_accounting_exact",
				Frames:   []StreamProxyFrame{{Type: StreamFrameAccounting, Bytes: 1}},
			},
			want: "accounting frame must not carry data",
		},
		{
			name: "backpressure accounting",
			fixture: StreamProxyFixture{
				Name:              "stream.proxy/v1.backpressure",
				Protocol:          StreamProxyProtocolV1,
				Expected:          "backpressure_window_respected",
				BytesToPlugin:     4,
				BackpressureBytes: 8,
				Frames: []StreamProxyFrame{
					{Type: StreamFrameWindow, WindowBytes: 4},
					{Type: StreamFrameData, Direction: "gateway_to_plugin", Bytes: 4},
				},
			},
			want: "backpressure_bytes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateStreamProxyFixture(tt.fixture)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateStreamProxyFixture() error = %v, want %q", err, tt.want)
			}
		})
	}
}
