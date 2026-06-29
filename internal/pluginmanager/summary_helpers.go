// internal/pluginmanager/summary_helpers.go provides small summary adapters shared by governance code.

package pluginmanager

func rolloutNodesTotal(rollout *PluginRolloutStatus) int {
	if rollout == nil {
		return 0
	}
	return rollout.NodesTotal
}

func rolloutNodesReady(rollout *PluginRolloutStatus) int {
	if rollout == nil {
		return 0
	}
	return rollout.NodesReady
}

func rolloutArtifactDistributionMode(rollout *PluginRolloutStatus) string {
	if rollout == nil {
		return ""
	}
	return rollout.ArtifactDistributionMode
}

func rolloutArtifactDistributionStatus(rollout *PluginRolloutStatus) string {
	if rollout == nil {
		return ""
	}
	return rollout.ArtifactDistributionStatus
}

func promotionRolloutNodes(rollouts []PluginRolloutStatus) map[string]map[string]int {
	nodes := make(map[string]map[string]int, len(rollouts))
	for _, rollout := range rollouts {
		nodes[rollout.PluginID] = map[string]int{
			"total":  rollout.NodesTotal,
			"ready":  rollout.NodesReady,
			"failed": rollout.NodesFailed,
			"stale":  rollout.NodesStale,
		}
	}
	return nodes
}

func promotionRolloutCrossNode(rollouts []PluginRolloutStatus) bool {
	for _, rollout := range rollouts {
		if rollout.CrossNodeApply {
			return true
		}
	}
	return false
}

func preflightChecksFromGovernanceIssues(issues []GovernanceIssue) []PreflightCheck {
	checks := make([]PreflightCheck, 0, len(issues))
	for _, issue := range issues {
		checks = append(checks, PreflightCheck{
			Code:     issue.Code,
			Severity: issue.Severity,
			Message:  issue.Message,
			Details:  issue.Details,
		})
	}
	return checks
}
