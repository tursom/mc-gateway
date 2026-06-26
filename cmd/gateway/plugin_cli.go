package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/tursom/mc-gateway/internal/pluginmanager"
)

func runPluginCLI(args []string) (bool, int) {
	if len(args) < 2 || args[0] != "plugin" {
		return false, 0
	}
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: gateway plugin inspect|validate|compat <artifact.mcgp>")
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
	default:
		fmt.Fprintf(os.Stderr, "unknown plugin command %q\n", command)
		return true, 2
	}
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
