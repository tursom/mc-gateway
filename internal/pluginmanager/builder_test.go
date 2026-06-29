package pluginmanager

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildEnvironmentUsesWhitelistAndExplicitGoPolicy(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("HOME", "/home/tester")
	t.Setenv("GOWORK", "off")
	t.Setenv("SECRET_TOKEN", "leak")
	t.Setenv("AWS_CREDENTIAL", "leak")
	t.Setenv("GOPRIVATE", "github.com/host/private")
	t.Setenv("GONOSUMDB", "host.internal")
	t.Setenv("GOPROXY", "https://host-proxy.example")

	env := envMapFromList(buildEnvironment(BuildRequest{
		GOOS:       "linux",
		GOARCH:     "amd64",
		CGOEnabled: "0",
		GOPROXY:    "https://proxy.example,direct",
		GONOSUMDB:  "corp.example/internal",
		GOPRIVATE:  "github.com/acme/private",
	}))

	if env["GOOS"] != "linux" || env["GOARCH"] != "amd64" || env["CGO_ENABLED"] != "0" {
		t.Fatalf("build env target = %+v, want explicit target tuple", env)
	}
	if env["GOPROXY"] != "https://proxy.example,direct" ||
		env["GONOSUMDB"] != "corp.example/internal" ||
		env["GOPRIVATE"] != "github.com/acme/private" {
		t.Fatalf("build env Go policy = %+v, want request-scoped GOPROXY/GONOSUMDB/GOPRIVATE", env)
	}
	for _, forbidden := range []string{"SECRET_TOKEN", "AWS_CREDENTIAL"} {
		if _, ok := env[forbidden]; ok {
			t.Fatalf("build env forwarded forbidden %s: %+v", forbidden, env)
		}
	}
}

func TestDockerRunArgsEnforceContainerBuildBoundaries(t *testing.T) {
	args := dockerRunArgs(
		"ghcr.io/tursom/mc-gateway-plugin-builder:release-v0-1-0-plugin-api-v1-go1.25.0-linux-amd64@sha256:"+strings.Repeat("a", 64),
		[]string{"/tmp/source:/src:ro", "/tmp/out:/out"},
		[]string{"GOOS=linux", "GOARCH=amd64"},
		"/src",
		"go", "build", "-mod=readonly", "-o", "/out/plugin.so", ".",
	)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"run",
		"--rm",
		"--read-only",
		"--tmpfs /tmp:rw,exec,nosuid,size=1g",
		"-v /tmp/source:/src:ro",
		"-v /tmp/out:/out",
		"-w /src",
		"go build -mod=readonly -o /out/plugin.so .",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("docker args = %q, missing %q", joined, want)
		}
	}
}

func TestSourceBuildModModeDefaultsReadonlyAndHonorsVendor(t *testing.T) {
	root := t.TempDir()
	source := ArtifactRecord{FilePath: root}
	if got := sourceBuildModMode(source, BuildRequest{}); got != "readonly" {
		t.Fatalf("sourceBuildModMode(no vendor) = %q, want readonly", got)
	}
	if err := os.Mkdir(filepath.Join(root, "vendor"), 0755); err != nil {
		t.Fatalf("Mkdir(vendor) error = %v", err)
	}
	if got := sourceBuildModMode(source, BuildRequest{}); got != "vendor" {
		t.Fatalf("sourceBuildModMode(vendor dir) = %q, want vendor", got)
	}
	if got := sourceBuildModMode(source, BuildRequest{VendorRequired: true}); got != "vendor" {
		t.Fatalf("sourceBuildModMode(vendor required) = %q, want vendor", got)
	}
}

func envMapFromList(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			out[key] = value
		}
	}
	return out
}
