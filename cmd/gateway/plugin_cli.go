// cmd/gateway/plugin_cli.go 分发插件相关子命令，包括本地脚手架、构建、清单和远程管理操作。

package main

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/tursom/mc-gateway/internal/admindb"
	"github.com/tursom/mc-gateway/internal/pluginmanager"
)

func runPluginCLI(args []string) (bool, int) {
	if len(args) < 1 || args[0] != "plugin" {
		return false, 0
	}
	if len(args) < 2 {
		printPluginCLIUsage()
		return true, 2
	}

	command := args[1]
	switch command {
	case "init":
		if err := runPluginInitCLI(args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "features":
		if err := runPluginFeaturesCLI(args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "manifest":
		if err := runPluginManifestCLI(args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "preflight":
		if err := runPluginPreflightCLI(args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "self-test":
		if err := runPluginSelfTestCLI(args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "benchmark":
		if err := runPluginBenchmarkCLI(args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "status":
		if err := runPluginRemoteStatusCLI(args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "upload":
		if err := runPluginRemoteUploadCLI(args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "transfer":
		if err := runPluginRemoteTransferCLI(args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "enable":
		if err := runPluginRemoteDesiredCLI(args[2:], pluginmanager.DesiredEnabled); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "disable":
		if err := runPluginRemoteActionCLI(args[2:], "disable"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "delete":
		if err := runPluginRemoteActionCLI(args[2:], "delete"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "rollback":
		if err := runPluginRemoteRollbackCLI(args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "config":
		if err := runPluginRemoteConfigCLI(args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "secret":
		if err := runPluginRemoteSecretCLI(args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "logs", "events", "metrics":
		if err := runPluginRemoteOperationsSectionCLI(command, args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "diagnose":
		if err := runPluginRemoteDiagnoseCLI(args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "task":
		if err := runPluginRemoteTaskCLI(args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "external":
		if err := runPluginRemoteExternalCLI(args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "data", "files":
		if err := runPluginRemoteResourceCLI(command, args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "gc":
		if err := runPluginRemoteGCCLI(args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "review":
		if err := runPluginRemoteReviewCLI(args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "advisory":
		if err := runPluginRemoteAdvisoryCLI(args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "vulnerability":
		if err := runPluginRemoteVulnerabilityCLI(args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "repo":
		if err := runPluginRemoteRepoCLI(args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "sbom", "verify":
		if err := runPluginRemoteSupplyChainCLI(command, args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "runtime":
		if err := runPluginRuntimeCLI(args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "schema":
		if err := runPluginSchemaCLI(args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "contract":
		if err := runPluginContractCLI(args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "conformance":
		if err := runPluginConformanceCLI(args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "export", "import", "diff", "drift", "dr-drill":
		if err := runPluginPromotionCLI(command, args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "apply":
		if err := runPluginRemotePromotionApplyCLI(args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "sign":
		if err := runPluginSignCLI(args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "build":
		if err := runPluginBuildCLI(args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "test":
		if err := runPluginTestCLI(args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "inspect":
		if len(args) < 3 {
			printPluginCLIUsage()
			return true, 2
		}
		packagePath := args[2]
		manifest, err := readPackageManifest(packagePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(manifest); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		return true, 0
	case "validate", "compat":
		artifact, err := runPluginValidatePathCLI(args[2:], "")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		fmt.Fprintf(os.Stdout, "ok plugin=%s version=%s sha256=%s api=%s go=%s %s/%s\n",
			artifact.PluginID, artifact.Version, artifact.SHA256, artifact.APIVersion, artifact.GoVersion, artifact.GOOS, artifact.GOARCH)
		return true, 0
	case "source-validate":
		source, err := runPluginValidatePathCLI(args[2:], pluginmanager.ArtifactTypeSource)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		fmt.Fprintf(os.Stdout, "ok source plugin=%s version=%s source_sha256=%s api=%s go=%s %s/%s\n",
			source.PluginID, source.Version, source.SHA256, source.APIVersion, source.GoVersion, source.GOOS, source.GOARCH)
		return true, 0
	case "source-build":
		if len(args) < 3 {
			printPluginCLIUsage()
			return true, 2
		}
		packagePath := args[2]
		outPath := ""
		if len(args) >= 4 {
			outPath = args[3]
		}
		build, out, err := buildSourcePackageForCLI(packagePath, outPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		fmt.Fprintf(os.Stdout, "ok build=%d plugin=%s source_sha256=%s artifact_sha256=%s builder=%s go=%s status=%s out=%s\n",
			build.ID, build.PluginID, build.SourceSHA256, build.ArtifactSHA256, build.BuilderType, build.GoVersion, build.Status, out)
		return true, 0
	default:
		fmt.Fprintf(os.Stderr, "unknown plugin command %q\n", command)
		return true, 2
	}
}

func printPluginCLIUsage() {
	fmt.Fprintln(os.Stderr, "usage: gateway plugin init|features|manifest|build|test|schema|contract|conformance|preflight|self-test|benchmark|status|upload|transfer|enable|disable|delete|rollback|config|secret|logs|events|metrics|diagnose|task|external|data|files|gc|review|advisory|vulnerability|repo|sbom|verify|runtime|inspect|validate|compat|source-validate|source-build|apply ...")
}

func buildSourcePackageForCLI(packagePath, outPath string) (pluginmanager.BuildRecord, string, error) {
	tmpRoot, err := os.MkdirTemp("", "mcgp-source-build-cli-*")
	if err != nil {
		return pluginmanager.BuildRecord{}, "", err
	}
	defer os.RemoveAll(tmpRoot)
	db, err := openPluginCLIDB(filepath.Join(tmpRoot, "plugins.db"))
	if err != nil {
		return pluginmanager.BuildRecord{}, "", err
	}
	defer db.Close()
	manager := pluginmanager.New(pluginmanager.Options{
		DB:            db,
		ArtifactRoot:  filepath.Join(tmpRoot, "artifacts"),
		PolicyProfile: pluginmanager.PolicyProfileDev,
	})
	source, err := manager.UploadSource(context.Background(), pluginmanager.ArtifactUpload{
		SourcePath: packagePath,
		FileName:   filepath.Base(packagePath),
		Actor:      "cli",
	})
	if err != nil {
		return pluginmanager.BuildRecord{}, "", err
	}
	builds, err := manager.ListBuilds(context.Background(), source.PluginID)
	if err != nil {
		return pluginmanager.BuildRecord{}, "", err
	}
	var queued pluginmanager.BuildRecord
	for _, build := range builds {
		if build.SourceID == source.ID && build.Status == pluginmanager.BuildStatusQueued {
			queued = build
			break
		}
	}
	if queued.ID == 0 {
		return pluginmanager.BuildRecord{}, "", fmt.Errorf("source upload did not create a queued build")
	}
	build, err := manager.RunBuild(context.Background(), "cli", queued.ID)
	if err != nil {
		return pluginmanager.BuildRecord{}, "", err
	}
	if build.Status != pluginmanager.BuildStatusSucceeded {
		if build.LogSummary != "" {
			return build, "", fmt.Errorf("source build status %s: %s\n%s", build.Status, build.Error, build.LogSummary)
		}
		return build, "", fmt.Errorf("source build status %s: %s", build.Status, build.Error)
	}
	artifact, err := manager.Artifact(context.Background(), build.ArtifactID)
	if err != nil {
		return build, "", err
	}
	if outPath == "" {
		outPath = filepath.Join(filepath.Dir(packagePath), artifact.PluginID+"-built.mcgp")
	}
	if err := packageBinaryArtifact(artifact, outPath); err != nil {
		return build, "", err
	}
	return build, outPath, nil
}

func packageBinaryArtifact(artifact pluginmanager.ArtifactRecord, outPath string) error {
	if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
		return err
	}
	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	for _, entry := range []struct {
		name string
		path string
	}{
		{name: "manifest.json", path: filepath.Join(filepath.Dir(artifact.FilePath), "manifest.json")},
		{name: pluginmanager.RuntimeEntry, path: artifact.FilePath},
	} {
		if err := addZipFile(zw, entry.name, entry.path); err != nil {
			zw.Close()
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return out.Close()
}

func addZipFile(zw *zip.Writer, name, path string) error {
	writer, err := zw.Create(name)
	if err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(writer, file)
	return err
}

func openPluginCLIDB(path string) (*sql.DB, error) {
	db, err := admindb.Open(path)
	if err != nil {
		return nil, err
	}
	if err := admindb.Migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func readPackageManifest(packagePath string) (pluginmanager.Manifest, error) {
	reader, err := zip.OpenReader(packagePath)
	if err != nil {
		return pluginmanager.Manifest{}, err
	}
	defer reader.Close()

	for _, file := range reader.File {
		if file.Name != "manifest.json" {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			return pluginmanager.Manifest{}, err
		}
		defer rc.Close()
		data, err := io.ReadAll(io.LimitReader(rc, pluginmanager.DefaultManifestMaxBytes+1))
		if err != nil {
			return pluginmanager.Manifest{}, err
		}
		if len(data) > pluginmanager.DefaultManifestMaxBytes {
			return pluginmanager.Manifest{}, fmt.Errorf("manifest.json exceeds %d bytes", pluginmanager.DefaultManifestMaxBytes)
		}
		var manifest pluginmanager.Manifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			return pluginmanager.Manifest{}, err
		}
		return manifest, nil
	}
	return pluginmanager.Manifest{}, fmt.Errorf("manifest.json is required")
}
