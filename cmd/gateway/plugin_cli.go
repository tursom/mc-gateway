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
	if len(args) < 2 || args[0] != "plugin" {
		return false, 0
	}
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: gateway plugin inspect|validate|compat|source-validate <artifact.mcgp> | source-build <source.mcgp> [out.mcgp]")
		return true, 2
	}

	command, packagePath := args[1], args[2]
	switch command {
	case "inspect":
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
		tmpRoot, err := os.MkdirTemp("", "mcgp-cli-*")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		defer os.RemoveAll(tmpRoot)
		store := pluginmanager.NewArtifactStore(tmpRoot)
		artifact, err := store.ValidateAndStore(pluginmanager.ArtifactUpload{
			SourcePath: packagePath,
			FileName:   filepath.Base(packagePath),
			Actor:      "cli",
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		fmt.Fprintf(os.Stdout, "ok plugin=%s version=%s sha256=%s api=%s go=%s %s/%s\n",
			artifact.PluginID, artifact.Version, artifact.SHA256, artifact.APIVersion, artifact.GoVersion, artifact.GOOS, artifact.GOARCH)
		return true, 0
	case "source-validate":
		tmpRoot, err := os.MkdirTemp("", "mcgp-source-cli-*")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		defer os.RemoveAll(tmpRoot)
		store := pluginmanager.NewArtifactStore(tmpRoot)
		source, err := store.ValidateAndStoreSource(pluginmanager.ArtifactUpload{
			SourcePath: packagePath,
			FileName:   filepath.Base(packagePath),
			Actor:      "cli",
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 1
		}
		fmt.Fprintf(os.Stdout, "ok source plugin=%s version=%s source_sha256=%s api=%s go=%s %s/%s\n",
			source.PluginID, source.Version, source.SHA256, source.APIVersion, source.GoVersion, source.GOOS, source.GOARCH)
		return true, 0
	case "source-build":
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
		DB:           db,
		ArtifactRoot: filepath.Join(tmpRoot, "artifacts"),
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
