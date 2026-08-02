package pluginmanager

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tursom/mc-gateway/plugin/api"
	"github.com/tursom/mc-gateway/plugin/sandboxsdk"
)

const (
	PluginHostSchemaVersion           = "mc-gateway.plugin-host/v1"
	PluginHostProtocol                = "mc-gateway-plugin-host/v1"
	PluginHostControlChannelUnix      = "unix-socket"
	PluginHostCommandHandshake        = "handshake"
	PluginHostCommandInit             = "init"
	PluginHostCommandReloadConfig     = "reload_config"
	PluginHostCommandDestroy          = "destroy"
	PluginHostCommandDrain            = "drain"
	PluginHostCommandStop             = "stop"
	PluginHostCommandShutdown         = "shutdown"
	PluginHostCommandTakeoverOpen     = "takeover_open"
	PluginHostCommandTakeoverWait     = "takeover_wait"
	PluginHostCommandTakeoverComplete = "takeover_complete"
)

type TakeoverAction = sandboxsdk.TakeoverAction

const (
	TakeoverActionHandled = sandboxsdk.TakeoverActionHandled
	TakeoverActionNext    = sandboxsdk.TakeoverActionNext
	TakeoverActionCore    = sandboxsdk.TakeoverActionCore
)

type PluginHostFeature struct {
	Protocol                         string   `json:"protocol"`
	Command                          string   `json:"command"`
	Handshake                        bool     `json:"handshake"`
	ControlChannel                   string   `json:"control_channel"`
	ControlChannelImplemented        bool     `json:"control_channel_implemented"`
	SupervisorStartStop              bool     `json:"supervisor_start_stop"`
	SupervisorDataPlane              bool     `json:"supervisor_data_plane"`
	ProcessTableOrphanCleanup        bool     `json:"process_table_orphan_cleanup"`
	OrphanDiscoveryOrder             []string `json:"orphan_discovery_order"`
	OrphanDiscoveryUnsupportedReason string   `json:"orphan_discovery_unsupported_reason,omitempty"`
	CrashTracking                    bool     `json:"crash_tracking"`
	CrashPolicy                      bool     `json:"crash_policy"`
	Backoff                          bool     `json:"backoff"`
	StreamProtocol                   string   `json:"stream_protocol,omitempty"`
	StreamHalfClose                  bool     `json:"stream_half_close"`
	StreamDeadline                   bool     `json:"stream_deadline"`
	StreamBackpressure               bool     `json:"stream_backpressure"`
	StreamCancel                     bool     `json:"stream_cancel"`
	StreamByteAccounting             bool     `json:"stream_byte_accounting"`
	LifecycleCommands                []string `json:"lifecycle_commands"`
	LifecycleImplemented             bool     `json:"lifecycle_implemented"`
	DataPlane                        bool     `json:"data_plane"`
	Maturity                         string   `json:"maturity"`
	UnsupportedReason                string   `json:"unsupported_reason,omitempty"`
}

type PluginHostHandshake struct {
	SchemaVersion        string   `json:"schema_version"`
	Protocol             string   `json:"protocol"`
	PID                  int      `json:"pid"`
	PPID                 int      `json:"ppid"`
	StartedAt            int64    `json:"started_at"`
	ControlChannel       string   `json:"control_channel"`
	Capabilities         []string `json:"capabilities"`
	LifecycleCommands    []string `json:"lifecycle_commands"`
	LifecycleImplemented bool     `json:"lifecycle_implemented"`
	DataPlane            bool     `json:"data_plane"`
}

type PluginHostControlRequest struct {
	RequestID string          `json:"request_id,omitempty"`
	Command   string          `json:"command"`
	Protocol  string          `json:"protocol,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type PluginHostControlResponse struct {
	RequestID string               `json:"request_id,omitempty"`
	OK        bool                 `json:"ok"`
	Code      string               `json:"code,omitempty"`
	Error     string               `json:"error,omitempty"`
	Handshake *PluginHostHandshake `json:"handshake,omitempty"`
	Lifecycle *PluginHostLifecycle `json:"lifecycle,omitempty"`
	Takeover  *PluginHostTakeover  `json:"takeover,omitempty"`
}

type PluginHostInitRequest struct {
	PluginID     string `json:"plugin_id"`
	ArtifactID   string `json:"artifact_id"`
	ArtifactPath string `json:"artifact_path"`
	EntrySymbol  string `json:"entry_symbol,omitempty"`
	ConfigJSON   string `json:"config_json,omitempty"`
}

type PluginHostReloadConfigRequest struct {
	ConfigJSON string `json:"config_json,omitempty"`
}

type PluginHostLifecycle struct {
	PluginID        string   `json:"plugin_id,omitempty"`
	ArtifactID      string   `json:"artifact_id,omitempty"`
	State           string   `json:"state"`
	RegisteredHooks []string `json:"registered_hooks,omitempty"`
	StartedAt       int64    `json:"started_at,omitempty"`
	UpdatedAt       int64    `json:"updated_at,omitempty"`
}

type PluginHostTakeoverRequest struct {
	SessionID           string             `json:"session_id"`
	ConnectionID        string             `json:"connection_id,omitempty"`
	TraceID             string             `json:"trace_id,omitempty"`
	PeerAddr            string             `json:"peer_addr,omitempty"`
	LocalAddr           string             `json:"local_addr,omitempty"`
	EffectiveSourceAddr string             `json:"effective_source_addr,omitempty"`
	Metadata            map[string]string  `json:"metadata,omitempty"`
	Ingress             api.IngressContext `json:"ingress"`
	DeadlineUnixMS      int64              `json:"deadline_unix_ms,omitempty"`
}

type PluginHostTakeover struct {
	SessionID           string            `json:"session_id,omitempty"`
	Connected           bool              `json:"connected,omitempty"`
	Action              TakeoverAction    `json:"action,omitempty"`
	Endpoint            string            `json:"endpoint,omitempty"`
	EffectiveSourceAddr string            `json:"effective_source_addr,omitempty"`
	Metadata            map[string]string `json:"metadata,omitempty"`
	Error               string            `json:"error,omitempty"`
}

type PluginHostTakeoverSessionRequest struct {
	SessionID string `json:"session_id"`
	Error     string `json:"error,omitempty"`
}

func PluginHostProtocolFeature() PluginHostFeature {
	return PluginHostFeature{
		Protocol:                         PluginHostProtocol,
		Command:                          "plugin-host",
		Handshake:                        true,
		ControlChannel:                   PluginHostControlChannelUnix,
		ControlChannelImplemented:        true,
		SupervisorStartStop:              true,
		SupervisorDataPlane:              true,
		ProcessTableOrphanCleanup:        PluginHostProcessTableDiscoveryAvailable(),
		OrphanDiscoveryOrder:             PluginHostOrphanDiscoveryOrder(),
		OrphanDiscoveryUnsupportedReason: PluginHostProcessTableUnsupportedReason(),
		CrashTracking:                    true,
		CrashPolicy:                      true,
		Backoff:                          true,
		StreamProtocol:                   StreamProxyProtocolV1,
		StreamHalfClose:                  true,
		StreamDeadline:                   true,
		StreamBackpressure:               true,
		StreamCancel:                     true,
		StreamByteAccounting:             true,
		LifecycleCommands:                PluginHostLifecycleCommands(),
		LifecycleImplemented:             true,
		DataPlane:                        true,
		Maturity:                         FeatureMaturityPartial,
		UnsupportedReason:                GoPluginProcessPartialUnsupportedReason,
	}
}

func PluginHostOrphanDiscoveryOrder() []string {
	return []string{
		"metadata-backed sweep",
		"stale control socket handshake",
		"supervisor-owned process cleanup",
		"Linux /proc process-table discovery",
	}
}

func PluginHostCapabilities() []string {
	capabilities := []string{
		"control.handshake",
		"control.shutdown",
		"control.lifecycle",
		"supervisor.process_table_orphan_cleanup",
		"crash.policy",
		"stream.proxy/v1",
		"stream.half_close",
		"stream.deadline",
		"stream.backpressure",
		"stream.cancel",
		"stream.byte_accounting",
	}
	capabilities = append(capabilities, StreamProxyCapabilities()...)
	return capabilities
}

func PluginHostLifecycleCommands() []string {
	return []string{
		PluginHostCommandInit,
		PluginHostCommandReloadConfig,
		PluginHostCommandDestroy,
		PluginHostCommandDrain,
		PluginHostCommandStop,
		PluginHostCommandTakeoverOpen,
		PluginHostCommandTakeoverWait,
		PluginHostCommandTakeoverComplete,
	}
}

func NewPluginHostHandshake(protocol string, pid, ppid int, startedAt int64) PluginHostHandshake {
	return PluginHostHandshake{
		SchemaVersion:        PluginHostSchemaVersion,
		Protocol:             protocol,
		PID:                  pid,
		PPID:                 ppid,
		StartedAt:            startedAt,
		ControlChannel:       PluginHostControlChannelUnix,
		Capabilities:         PluginHostCapabilities(),
		LifecycleCommands:    PluginHostLifecycleCommands(),
		LifecycleImplemented: true,
		DataPlane:            false,
	}
}

func ValidatePluginHostProtocol(protocol string) error {
	protocol = strings.TrimSpace(protocol)
	if protocol == "" {
		return fmt.Errorf("plugin-host protocol is required")
	}
	if protocol != PluginHostProtocol {
		return fmt.Errorf("unsupported plugin-host protocol %q", protocol)
	}
	return nil
}
