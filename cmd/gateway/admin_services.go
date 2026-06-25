package main

import (
	"context"
	"database/sql"

	"github.com/tursom/mc-gateway/internal/adminservice"
)

func ensureDefaultServices(ctx context.Context, db *sql.DB, tcpAdminPort int) error {
	return adminservice.NewRepository(db).EnsureDefaults(ctx, tcpAdminPort)
}

func applyServiceConfig(ctx context.Context, db *sql.DB) error {
	services, err := listServiceConfigs(ctx, db)
	if err != nil {
		return err
	}

	config.Tcp.Enable = true
	config.Tcp.Port = adminStartup.TCPAdminPort

	for _, service := range services {
		switch service.Name {
		case serviceNameKCP:
			config.Kcp.Enable = service.Enabled
			config.Kcp.Port = service.Port
			config.Kcp.DataShards = adminservice.IntOption(service.Options, "data_shards", defaultKCPDataShards)
			config.Kcp.ParityShards = adminservice.IntOption(service.Options, "parity_shards", defaultKCPParityShards)
		case serviceNameQUIC:
			config.Quic.Enable = service.Enabled
			config.Quic.Port = service.Port
			config.Quic.ApplicationProtocols = adminservice.StringSliceOption(service.Options, "application_protocols")
		case serviceNameWebSocket:
			config.WebSocket.Enable = service.Enabled
			config.WebSocket.Port = service.Port
			config.WebSocket.Path = adminservice.StringOption(service.Options, "path", defaultWebSocketPath)
		}
	}

	if config.Kcp.Port == 0 {
		config.Kcp.Port = defaultKCPPort
	}
	if config.Quic.Port == 0 {
		config.Quic.Port = defaultQUICPort
	}
	if config.WebSocket.Port == 0 {
		config.WebSocket.Port = defaultWebSocketPort
	}
	if config.WebSocket.Path == "" {
		config.WebSocket.Path = defaultWebSocketPath
	}

	return nil
}

func listServiceConfigs(ctx context.Context, db *sql.DB) ([]adminservice.Record, error) {
	services, err := adminservice.NewRepository(db).List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range services {
		services[i].Running = serviceIsRunning(services[i])
	}
	return services, nil
}

func serviceIsRunning(service adminservice.Record) bool {
	switch service.Name {
	case serviceNameTCPAdmin:
		return true
	case serviceNameKCP:
		return config.Kcp.Enable
	case serviceNameQUIC:
		return config.Quic.Enable
	case serviceNameWebSocket:
		return config.WebSocket.Enable
	default:
		return false
	}
}

func updateServiceConfig(ctx context.Context, actor, name string, enabled bool, port int, options map[string]any) error {
	return adminservice.NewRepository(adminDB).Update(ctx, actor, name, enabled, port, options)
}
