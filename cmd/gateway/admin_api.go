package main

import (
	"net/http"

	"github.com/tursom/mc-gateway/internal/adminhttp"
)

func newAdminAPIHandler() http.HandlerFunc {
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
		PluginDiagnostics:     handleAdminPluginDiagnostics,
		PluginService:         handleAdminPluginService,
		PluginRepositories:    handleAdminPluginRepositories,
		PluginSupplyChain:     handleAdminPluginSupplyChain,
		PluginInstrumentation: handleAdminPluginInstrumentation,
	})
}
