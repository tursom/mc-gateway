package main

import (
	"context"
	"database/sql"
	"os"
	"time"

	"github.com/tursom/mc-gateway/internal/adminconfig"
	"github.com/tursom/mc-gateway/internal/admindb"
	"github.com/tursom/mc-gateway/internal/adminservice"
)

const (
	defaultAdminDBPath      = adminconfig.DefaultDBPath
	defaultAdminPath        = adminconfig.DefaultAdminPath
	defaultAdminAPIPrefix   = adminconfig.DefaultAdminAPIPrefix
	defaultAdminSessionTTL  = adminconfig.DefaultSessionTTL
	defaultKCPPort          = adminservice.DefaultKCPPort
	defaultKCPDataShards    = adminservice.DefaultKCPDataShards
	defaultKCPParityShards  = adminservice.DefaultKCPParityShards
	defaultQUICPort         = adminservice.DefaultQUICPort
	defaultWebSocketPort    = adminservice.DefaultWebSocketPort
	defaultWebSocketPath    = adminservice.DefaultWebSocketPath
	serviceNameTCPAdmin     = adminservice.NameTCPAdmin
	serviceNameKCP          = adminservice.NameKCP
	serviceNameQUIC         = adminservice.NameQUIC
	serviceNameWebSocket    = adminservice.NameWebSocket
	adminEnvDB              = adminconfig.EnvDB
	adminEnvTCPAdminPort    = adminconfig.EnvTCPAdminPort
	adminEnvPath            = adminconfig.EnvPath
	adminEnvAPIPrefix       = adminconfig.EnvAPIPrefix
	adminEnvInitialPassword = adminconfig.EnvInitialPassword
)

var (
	adminStartup = adminconfig.Config{
		DBPath:         defaultAdminDBPath,
		TCPAdminPort:   defaultTCPPort,
		AdminPath:      defaultAdminPath,
		AdminAPIPrefix: defaultAdminAPIPrefix,
		SessionTTL:     defaultAdminSessionTTL,
	}

	adminDB        *sql.DB
	adminDBPath    string
	processStartAt = time.Now()
)

func initializeGatewayRuntime() error {
	startup, err := parseStartupConfig(os.Getenv)
	if err != nil {
		return err
	}
	adminStartup = startup

	db, err := admindb.Open(startup.DBPath)
	if err != nil {
		return err
	}

	if adminDB != nil && adminDB != db {
		_ = adminDB.Close()
	}
	adminDB = db
	adminDBPath = startup.DBPath

	if err := admindb.Migrate(db); err != nil {
		return err
	}
	if err := ensureDefaultServices(context.Background(), db, startup.TCPAdminPort); err != nil {
		return err
	}
	if err := applyServiceConfig(context.Background(), db); err != nil {
		return err
	}
	if err := ensureInitialAdminFromEnv(context.Background(), db, os.Getenv(adminEnvInitialPassword)); err != nil {
		return err
	}
	return refreshRouteSnapshot(context.Background())
}

func closeGatewayRuntime() {
	if adminDB != nil {
		_ = adminDB.Close()
		adminDB = nil
	}
}

func parseStartupConfig(getenv func(string) string) (adminconfig.Config, error) {
	return adminconfig.Parse(getenv)
}
