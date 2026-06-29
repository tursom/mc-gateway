// cmd/gateway/admin_api.go 组装 Admin API 的共享依赖，并提供嵌入式控制台使用的顶层 HTTP 路由。

package main

import (
	"net/http"

	"github.com/tursom/mc-gateway/internal/adminhttp"
)

func newAdminAPIHandler() http.HandlerFunc {
	// Admin API 的路径解析放在 internal/adminhttp 中，主包只提供各业务 handler。
	// 这样测试可以复用同一套路由表，而不会依赖真实监听器。
	return adminhttp.NewAPIHandler(adminStartup.AdminAPIPrefix, adminhttp.APIHandlers{
		SetupStatus: handleAdminSetupStatus,
		Setup:       handleAdminSetup,
		Login:       handleAdminLogin,
		Logout:      handleAdminLogout,
		Me:          handleAdminMe,
		Status:      handleAdminStatus,

		RoutesList: handleAdminRoutesList,
		RouteItem:  handleAdminRouteItem,

		ServicesList: handleAdminServicesList,
		ServiceItem:  handleAdminServiceItem,

		Metrics: handleAdminMetrics,

		UsersList:   handleAdminUsersList,
		UsersCreate: handleAdminUsersCreate,
		UserItem:    handleAdminUserItem,

		AuditLogs: handleAdminAuditLogs,

		// 插件相关接口数量较多，统一在这里接入，确保嵌入式 UI 和远程 CLI
		// 看到的是同一套 Admin API 行为。
		PluginArtifacts:       handleAdminPluginArtifacts,
		PluginArtifact:        handleAdminPluginArtifact,
		PluginSources:         handleAdminPluginSources,
		PluginBuilds:          handleAdminPluginBuilds,
		PluginBuild:           handleAdminPluginBuild,
		PluginGC:              handleAdminPluginGC,
		PluginOperationsGC:    handleAdminPluginOperationsGC,
		PluginsList:           handleAdminPluginsList,
		PluginItem:            handleAdminPluginItem,
		PluginAction:          handleAdminPluginAction,
		PluginConfig:          handleAdminPluginConfig,
		PluginSecrets:         handleAdminPluginSecrets,
		PluginRollback:        handleAdminPluginRollback,
		PluginOperations:      handleAdminPluginOperations,
		PluginDraining:        handleAdminPluginDraining,
		PluginDispatch:        handleAdminPluginDispatchPlan,
		PluginGovernance:      handleAdminPluginGovernance,
		PluginAdvisories:      handleAdminPluginAdvisories,
		PluginVulnerabilities: handleAdminPluginVulnerabilities,
		PluginDiagnostics:     handleAdminPluginDiagnostics,
		PluginFeatures:        handleAdminPluginFeatures,
		PluginService:         handleAdminPluginService,
		PluginRepositories:    handleAdminPluginRepositories,
		PluginSupplyChain:     handleAdminPluginSupplyChain,
		PluginInstrumentation: handleAdminPluginInstrumentation,
		PluginPromotions:      handleAdminPluginPromotions,
	})
}
