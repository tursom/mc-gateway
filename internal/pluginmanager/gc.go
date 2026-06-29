// internal/pluginmanager/gc.go 选择并删除不再被期望状态或活动运行态引用的插件制品。

package pluginmanager

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
)

func (m *Manager) GCCandidates(ctx context.Context) ([]GCCandidate, error) {
	refs, err := m.repo.ReferencedArtifactIDs(ctx)
	if err != nil {
		return nil, err
	}
	artifacts, err := m.repo.ListArtifacts(ctx, "")
	if err != nil {
		return nil, err
	}
	var candidates []GCCandidate
	for _, artifact := range artifacts {
		referenced := refs[artifact.ID]
		artifactPath := artifactGCPath(artifact)
		size := artifact.SizeBytes
		if info, err := os.Stat(artifactPath); err == nil && info.IsDir() {
			size = dirSize(artifactPath)
		} else if err == nil {
			size = info.Size()
		}
		candidate := GCCandidate{
			Kind:       "artifact",
			ID:         artifact.ID,
			PluginID:   artifact.PluginID,
			Path:       artifactPath,
			Protected:  referenced,
			SizeBytes:  size,
			CreatedAt:  artifact.CreatedAt,
			Referenced: referenced,
		}
		switch {
		case referenced:
			candidate.Reason = "referenced by active/desired/snapshot/build"
		case artifact.ArtifactType == ArtifactTypeSource:
			candidate.Reason = "unreferenced source package"
		case artifact.Status == ArtifactStatusRejected || artifact.Status == ArtifactStatusDeleted:
			candidate.Reason = "unreferenced rejected/deleted artifact"
		default:
			candidate.Reason = "unreferenced artifact"
		}
		if !referenced && (artifact.ArtifactType == ArtifactTypeSource || artifact.Status == ArtifactStatusRejected || artifact.Status == ArtifactStatusDeleted) {
			candidates = append(candidates, candidate)
		}
	}
	builds, err := m.repo.ListBuilds(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, build := range builds {
		protectedBuild := build.Status == BuildStatusRunning || build.Status == BuildStatusQueued
		reason := "completed build log"
		if protectedBuild {
			reason = "in-flight build"
		}
		candidates = append(candidates, GCCandidate{
			Kind:      "build_log",
			ID:        buildIDString(build.ID),
			PluginID:  build.PluginID,
			Protected: protectedBuild,
			Reason:    reason,
			SizeBytes: int64(len(build.LogSummary)),
			CreatedAt: build.CreatedAt,
		})
	}
	return candidates, nil
}

func (m *Manager) RunGC(ctx context.Context, actor string, dryRun bool) ([]GCCandidate, error) {
	candidates, err := m.GCCandidates(ctx)
	if err != nil {
		return nil, err
	}
	if dryRun {
		_ = m.repo.RecordOperation(ctx, "", "", "artifact_gc", "dry_run", actor, "artifact gc dry-run completed", map[string]any{
			"candidates": len(candidates),
		})
		return candidates, nil
	}
	var removed []GCCandidate
	for _, candidate := range candidates {
		if candidate.Protected {
			continue
		}
		switch candidate.Kind {
		case "artifact":
			if candidate.Path == "" {
				continue
			}
			if err := os.RemoveAll(candidate.Path); err != nil {
				_ = m.repo.RecordOperation(ctx, candidate.PluginID, candidate.ID, "artifact_gc", "failed", actor, err.Error(), map[string]any{
					"path": candidate.Path,
				})
				continue
			}
			_ = m.repo.UpdateArtifactStatus(ctx, candidate.ID, ArtifactStatusDeleted, "artifact files removed by gc")
			removed = append(removed, candidate)
		case "build_log":
			id, err := strconv.ParseInt(candidate.ID, 10, 64)
			if err != nil {
				continue
			}
			if err := m.repo.ClearBuildLog(ctx, id); err != nil {
				_ = m.repo.RecordOperation(ctx, candidate.PluginID, "", "artifact_gc", "failed", actor, err.Error(), map[string]any{
					"build_id": id,
				})
				continue
			}
			removed = append(removed, candidate)
		}
	}
	_ = m.repo.RecordOperation(ctx, "", "", "artifact_gc", "succeeded", actor, "artifact gc completed", map[string]any{
		"removed": len(removed),
	})
	return removed, nil
}

func artifactGCPath(artifact ArtifactRecord) string {
	if artifact.FilePath == "" {
		return ""
	}
	return filepath.Dir(artifact.FilePath)
}

func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

func buildIDString(id int64) string {
	return strconv.FormatInt(id, 10)
}
