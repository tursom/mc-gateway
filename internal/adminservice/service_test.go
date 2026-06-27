// internal/adminservice/service_test.go 包含用于约束 service 行为的测试。

package adminservice

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestDefaultRecords(t *testing.T) {
	records := DefaultRecords(25575)
	if len(records) != 4 {
		t.Fatalf("DefaultRecords() len = %d, want 4", len(records))
	}

	want := []Record{
		{Name: NameTCPAdmin, Enabled: true, Port: 25575, Options: map[string]any{}},
		{Name: NameKCP, Enabled: false, Port: DefaultKCPPort, Options: map[string]any{
			"data_shards":   DefaultKCPDataShards,
			"parity_shards": DefaultKCPParityShards,
		}},
		{Name: NameQUIC, Enabled: false, Port: DefaultQUICPort, Options: map[string]any{
			"application_protocols": []string{"minecraft", "quic", "raw", "h3"},
		}},
		{Name: NameWebSocket, Enabled: false, Port: DefaultWebSocketPort, Options: map[string]any{
			"path": DefaultWebSocketPath,
		}},
	}
	if !reflect.DeepEqual(records, want) {
		t.Fatalf("DefaultRecords() = %#v, want %#v", records, want)
	}

	records[2].Options["application_protocols"].([]string)[0] = "changed"
	if got := DefaultRecords(25575)[2].Options["application_protocols"].([]string)[0]; got != "minecraft" {
		t.Fatalf("DefaultRecords() reused mutable protocol defaults, got %q", got)
	}
}

func TestDefaultPort(t *testing.T) {
	tests := []struct {
		name string
		want int
	}{
		{NameTCPAdmin, 25575},
		{NameKCP, DefaultKCPPort},
		{NameQUIC, DefaultQUICPort},
		{NameWebSocket, DefaultWebSocketPort},
		{"unknown", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DefaultPort(tt.name, 25575); got != tt.want {
				t.Fatalf("DefaultPort(%q) = %d, want %d", tt.name, got, tt.want)
			}
		})
	}
}

func TestValidateUpdate(t *testing.T) {
	valid := []struct {
		name    string
		enabled bool
		port    int
	}{
		{NameTCPAdmin, true, 25565},
		{NameKCP, false, 25566},
		{NameQUIC, true, 25565},
		{NameWebSocket, true, 25566},
	}
	for _, tt := range valid {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateUpdate(tt.name, tt.enabled, tt.port); err != nil {
				t.Fatalf("ValidateUpdate() error = %v", err)
			}
		})
	}

	invalid := []struct {
		name    string
		enabled bool
		port    int
	}{
		{"unknown", true, 25565},
		{NameKCP, true, 0},
		{NameKCP, true, 70000},
		{NameTCPAdmin, false, 25565},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateUpdate(tt.name, tt.enabled, tt.port); err == nil {
				t.Fatal("ValidateUpdate() error = nil, want error")
			}
		})
	}
}

func TestDecodeAndReadOptions(t *testing.T) {
	options := DecodeOptions(`{
		"int": 12,
		"json_number": 13,
		"string_int": "14",
		"text": "value",
		"items": ["minecraft", "", "raw"]
	}`)
	if got := IntOption(options, "int", 0); got != 12 {
		t.Fatalf("IntOption(float64) = %d, want 12", got)
	}
	decoder := json.NewDecoder(strings.NewReader(`{"json_number":15}`))
	decoder.UseNumber()
	var withNumber map[string]any
	if err := decoder.Decode(&withNumber); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if got := IntOption(withNumber, "json_number", 0); got != 15 {
		t.Fatalf("IntOption(json.Number) = %d, want 15", got)
	}
	if got := IntOption(options, "string_int", 0); got != 14 {
		t.Fatalf("IntOption(string) = %d, want 14", got)
	}
	if got := StringOption(options, "text", "fallback"); got != "value" {
		t.Fatalf("StringOption() = %q, want value", got)
	}
	if got := StringSliceOption(options, "items"); !reflect.DeepEqual(got, []string{"minecraft", "raw"}) {
		t.Fatalf("StringSliceOption() = %#v", got)
	}
	if got := DecodeOptions("not-json"); len(got) != 0 {
		t.Fatalf("DecodeOptions(invalid) = %#v, want empty", got)
	}
}

func TestNormalizeOptions(t *testing.T) {
	tests := []struct {
		name    string
		options map[string]any
		want    map[string]any
	}{
		{
			name:    NameKCP,
			options: map[string]any{"data_shards": 0, "parity_shards": "4"},
			want: map[string]any{
				"data_shards":   DefaultKCPDataShards,
				"parity_shards": "4",
			},
		},
		{
			name:    NameQUIC,
			options: map[string]any{"application_protocols": []any{}},
			want: map[string]any{
				"application_protocols": []string{"minecraft", "quic", "raw", "h3"},
			},
		},
		{
			name:    NameWebSocket,
			options: map[string]any{"path": "gateway"},
			want:    map[string]any{"path": "/gateway"},
		},
		{
			name:    NameWebSocket,
			options: map[string]any{"path": "/gateway"},
			want:    map[string]any{"path": "/gateway"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeOptions(tt.name, tt.options)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("NormalizeOptions() = %#v, want %#v", got, tt.want)
			}
		})
	}
}
