// cmd/gateway/admin_runtime.go 打开 SQLite 运行态数据库、写入默认数据，并为在线流量发布首个路由快照。

package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/tursom/mc-gateway/internal/adminconfig"
	"github.com/tursom/mc-gateway/internal/admindb"
	"github.com/tursom/mc-gateway/internal/adminservice"
	"github.com/tursom/mc-gateway/internal/pluginmanager"
)

const (
	defaultAdminDBPath                      = adminconfig.DefaultDBPath
	defaultAdminPath                        = adminconfig.DefaultAdminPath
	defaultAdminAPIPrefix                   = adminconfig.DefaultAdminAPIPrefix
	defaultAdminSessionTTL                  = adminconfig.DefaultSessionTTL
	defaultKCPPort                          = adminservice.DefaultKCPPort
	defaultKCPDataShards                    = adminservice.DefaultKCPDataShards
	defaultKCPParityShards                  = adminservice.DefaultKCPParityShards
	defaultQUICPort                         = adminservice.DefaultQUICPort
	defaultWebSocketPort                    = adminservice.DefaultWebSocketPort
	defaultWebSocketPath                    = adminservice.DefaultWebSocketPath
	serviceNameTCPAdmin                     = adminservice.NameTCPAdmin
	serviceNameKCP                          = adminservice.NameKCP
	serviceNameQUIC                         = adminservice.NameQUIC
	serviceNameWebSocket                    = adminservice.NameWebSocket
	adminEnvDB                              = adminconfig.EnvDB
	adminEnvTCPAdminPort                    = adminconfig.EnvTCPAdminPort
	adminEnvPath                            = adminconfig.EnvPath
	adminEnvAPIPrefix                       = adminconfig.EnvAPIPrefix
	adminEnvInitialPassword                 = adminconfig.EnvInitialPassword
	adminEnvPluginRequireConformanceFixture = adminconfig.EnvPluginRequireConformanceFixture
)

var (
	// adminStartup 是启动时解析出的管理端配置；后续 HTTP handler 和静态资源注入都会读取它。
	adminStartup = adminconfig.Config{
		DBPath:                          defaultAdminDBPath,
		TCPAdminPort:                    defaultTCPPort,
		AdminPath:                       defaultAdminPath,
		AdminAPIPrefix:                  defaultAdminAPIPrefix,
		SessionTTL:                      defaultAdminSessionTTL,
		PluginRequireConformanceFixture: false,
	}

	adminDB        *sql.DB
	adminDBPath    string
	pluginsManager *pluginmanager.Manager
	processStartAt = time.Now()
)

// initializeGatewayRuntime 按固定顺序准备运行态：解析配置、打开数据库、迁移 schema、
// 写入默认服务、应用服务配置、创建初始管理员、发布路由快照、最后启动插件管理器。
func initializeGatewayRuntime() error {
	ctx := context.Background()
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
	// 默认服务必须先存在，applyServiceConfig 才能把 SQLite 中的运行态端口写回 config。
	if err := ensureDefaultServices(ctx, db, startup.TCPAdminPort); err != nil {
		return err
	}
	if err := applyServiceConfig(ctx, db); err != nil {
		return err
	}
	reservedIngressListeners, err := pluginIngressReservedListeners(ctx, db)
	if err != nil {
		return err
	}
	if err := ensureInitialAdminFromEnv(ctx, db, os.Getenv(adminEnvInitialPassword)); err != nil {
		return err
	}
	if err := refreshRouteSnapshot(ctx); err != nil {
		return err
	}
	runtimeFacts, err := pluginRuntimeFeatureFactsOptionsFromEnv(os.Getenv)
	if err != nil {
		return err
	}

	// 插件制品放在数据库同级目录下，便于容器挂载一个 data volume 即可保留全部运行态。
	pluginsManager = pluginmanager.New(pluginmanager.Options{
		DB:                        db,
		ArtifactRoot:              filepath.Join(filepath.Dir(startup.DBPath), "plugins", "artifacts"),
		WaitGroup:                 &pluginExitWaitGroup,
		IngressReservedListeners:  reservedIngressListeners,
		RequireConformanceFixture: startup.PluginRequireConformanceFixture,
		FutureRuntimeGates:        runtimeFacts.FutureRuntimeGates,
		SandboxPolicy:             runtimeFacts.SandboxPolicy,
	})
	return pluginsManager.Reconcile(ctx)
}

// closeGatewayRuntime 先停止插件运行态，再关闭当前进程持有的数据库连接。
// SQLite 文件、插件期望状态和插件制品都保留在数据目录中。
func closeGatewayRuntime(ctx context.Context) error {
	var errs []error
	if pluginsManager != nil {
		if err := pluginsManager.Close(ctx); err != nil {
			errs = append(errs, err)
		}
		pluginsManager = nil
	}
	if adminDB != nil {
		if err := adminDB.Close(); err != nil {
			errs = append(errs, err)
		}
		adminDB = nil
	}
	return errors.Join(errs...)
}

func parseStartupConfig(getenv func(string) string) (adminconfig.Config, error) {
	return adminconfig.Parse(getenv)
}
