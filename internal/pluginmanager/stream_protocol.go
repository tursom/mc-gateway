package pluginmanager

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	StreamProxyProtocolV1 = "stream.proxy/v1"

	StreamFrameData       = "data"
	StreamFrameHalfClose  = "half_close"
	StreamFrameWindow     = "window"
	StreamFrameCancel     = "cancel"
	StreamFrameDeadline   = "deadline"
	StreamFrameAccounting = "accounting"
)

type StreamProxyContract struct {
	Protocol               string   `json:"protocol"`
	Commands               []string `json:"commands"`
	RequiresHalfClose      bool     `json:"requires_half_close"`
	RequiresDeadline       bool     `json:"requires_deadline"`
	RequiresBackpressure   bool     `json:"requires_backpressure"`
	RequiresCancel         bool     `json:"requires_cancel"`
	RequiresByteAccounting bool     `json:"requires_byte_accounting"`
}

type StreamProxyFrame struct {
	Type           string `json:"type"`
	Direction      string `json:"direction,omitempty"`
	StreamID       string `json:"stream_id,omitempty"`
	Seq            uint64 `json:"seq,omitempty"`
	Bytes          uint64 `json:"bytes,omitempty"`
	WindowBytes    uint64 `json:"window_bytes,omitempty"`
	DeadlineUnixMS int64  `json:"deadline_unix_ms,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

type StreamProxyFixture struct {
	Name              string             `json:"name"`
	Protocol          string             `json:"protocol"`
	Status            string             `json:"status"`
	Expected          string             `json:"expected"`
	Frames            []StreamProxyFrame `json:"frames"`
	DeadlineUnixMS    int64              `json:"deadline_unix_ms,omitempty"`
	BytesToPlugin     uint64             `json:"bytes_to_plugin,omitempty"`
	BytesToGateway    uint64             `json:"bytes_to_gateway,omitempty"`
	BackpressureBytes uint64             `json:"backpressure_bytes,omitempty"`
}

type StreamProxyFixtureFile struct {
	StreamProxyScenarios []StreamProxyFixture `json:"stream_proxy_scenarios,omitempty"`
}

func StreamProxyV1Contract() StreamProxyContract {
	return StreamProxyContract{
		Protocol: StreamProxyProtocolV1,
		Commands: []string{
			PluginHostCommandTakeoverOpen,
			StreamFrameData,
			StreamFrameHalfClose,
			StreamFrameWindow,
			StreamFrameCancel,
			StreamFrameDeadline,
			StreamFrameAccounting,
		},
		RequiresHalfClose:      true,
		RequiresDeadline:       true,
		RequiresBackpressure:   true,
		RequiresCancel:         true,
		RequiresByteAccounting: true,
	}
}

func StreamProxyCapabilities() []string {
	return []string{
		StreamProxyProtocolV1 + ".half_close",
		StreamProxyProtocolV1 + ".deadline",
		StreamProxyProtocolV1 + ".backpressure",
		StreamProxyProtocolV1 + ".cancel",
		StreamProxyProtocolV1 + ".byte_accounting",
	}
}

func ValidateStreamProxyFixtureJSON(data []byte) ([]StreamProxyFixture, error) {
	var file StreamProxyFixtureFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}
	fixtures := append([]StreamProxyFixture(nil), file.StreamProxyScenarios...)
	sort.SliceStable(fixtures, func(i, j int) bool { return fixtures[i].Name < fixtures[j].Name })
	for i := range fixtures {
		if err := ValidateStreamProxyFixture(fixtures[i]); err != nil {
			return nil, err
		}
	}
	return fixtures, nil
}

func ValidateStreamProxyFixture(fixture StreamProxyFixture) error {
	name := strings.TrimSpace(fixture.Name)
	if name == "" {
		return errors.New("stream proxy fixture name is required")
	}
	if protocol := strings.TrimSpace(fixture.Protocol); protocol != StreamProxyProtocolV1 {
		return fmt.Errorf("stream proxy fixture %q protocol = %q, want %s", name, protocol, StreamProxyProtocolV1)
	}
	seen := map[string]bool{}
	var bytesToPlugin, bytesToGateway uint64
	for _, frame := range fixture.Frames {
		frameType := strings.TrimSpace(frame.Type)
		switch frameType {
		case StreamFrameData:
			if frame.Bytes == 0 {
				return fmt.Errorf("stream proxy fixture %q data frame requires bytes", name)
			}
			switch strings.TrimSpace(frame.Direction) {
			case "gateway_to_plugin":
				bytesToPlugin += frame.Bytes
			case "plugin_to_gateway":
				bytesToGateway += frame.Bytes
			default:
				return fmt.Errorf("stream proxy fixture %q data frame direction is invalid", name)
			}
		case StreamFrameHalfClose:
			seen["half_close"] = true
			switch strings.TrimSpace(frame.Direction) {
			case "gateway_to_plugin", "plugin_to_gateway":
			default:
				return fmt.Errorf("stream proxy fixture %q half_close frame direction is invalid", name)
			}
		case StreamFrameDeadline:
			seen["deadline"] = true
			if frame.DeadlineUnixMS <= 0 && fixture.DeadlineUnixMS <= 0 {
				return fmt.Errorf("stream proxy fixture %q deadline frame requires deadline_unix_ms", name)
			}
		case StreamFrameWindow:
			seen["backpressure"] = true
			if frame.WindowBytes == 0 {
				return fmt.Errorf("stream proxy fixture %q window frame requires window_bytes", name)
			}
		case StreamFrameCancel:
			seen["cancel"] = true
			if strings.TrimSpace(frame.Reason) == "" {
				return fmt.Errorf("stream proxy fixture %q cancel frame requires reason", name)
			}
		case StreamFrameAccounting:
			seen["accounting"] = true
			if frame.Bytes != 0 || frame.WindowBytes != 0 {
				return fmt.Errorf("stream proxy fixture %q accounting frame must not carry data or window bytes", name)
			}
		default:
			return fmt.Errorf("stream proxy fixture %q has unsupported frame type %q", name, frameType)
		}
	}
	if fixture.BackpressureBytes != 0 && fixture.BackpressureBytes != bytesToPlugin {
		return fmt.Errorf("stream proxy fixture %q backpressure_bytes = %d, computed %d", name, fixture.BackpressureBytes, bytesToPlugin)
	}
	if fixture.BytesToPlugin != 0 && fixture.BytesToPlugin != bytesToPlugin {
		return fmt.Errorf("stream proxy fixture %q bytes_to_plugin = %d, computed %d", name, fixture.BytesToPlugin, bytesToPlugin)
	}
	if fixture.BytesToGateway != 0 && fixture.BytesToGateway != bytesToGateway {
		return fmt.Errorf("stream proxy fixture %q bytes_to_gateway = %d, computed %d", name, fixture.BytesToGateway, bytesToGateway)
	}
	expected := strings.TrimSpace(fixture.Expected)
	required := map[string]string{
		"half_close":   "half_close_propagated",
		"deadline":     "deadline_enforced",
		"backpressure": "backpressure_window_respected",
		"cancel":       "cancel_closes_stream",
		"accounting":   "byte_accounting_exact",
	}
	if want, ok := required[strings.TrimPrefix(name, StreamProxyProtocolV1+".")]; ok && expected != want {
		return fmt.Errorf("stream proxy fixture %q expected = %q, want %q", name, expected, want)
	}
	for key := range required {
		if strings.Contains(name, key) && !seen[key] {
			return fmt.Errorf("stream proxy fixture %q missing %s frame", name, key)
		}
	}
	if strings.Contains(name, "deadline") && fixture.DeadlineUnixMS > 0 {
		if time.UnixMilli(fixture.DeadlineUnixMS).IsZero() {
			return fmt.Errorf("stream proxy fixture %q deadline_unix_ms is invalid", name)
		}
	}
	return nil
}
