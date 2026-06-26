package main

import (
	"encoding/json"
	"os"
	"runtime"
)

func main() {
	if err := renderManifest(os.Stdout, os.Getenv("ARTIFACT_TYPE")); err != nil {
		panic(err)
	}
}

func renderManifest(out *os.File, artifactType string) error {
	data, err := os.ReadFile("manifest.json")
	if err != nil {
		return err
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		return err
	}
	if artifactType != "" {
		manifest["artifact_type"] = artifactType
	}
	manifest["go_version"] = runtime.Version()
	manifest["go_os"] = runtime.GOOS
	manifest["go_arch"] = runtime.GOARCH
	if manifest["artifact_type"] == "source" {
		manifest["build"] = map[string]any{
			"type":            "go",
			"entry":           ".",
			"go_version":      runtime.Version(),
			"cgo_enabled":     true,
			"tags":            []string{},
			"vendor_required": false,
			"output":          "plugin.so",
		}
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(manifest)
}
