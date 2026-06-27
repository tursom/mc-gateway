//go:build unix || plan9

// cmd/gateway/pid_unix_test.go 包含用于约束 pid unix 行为的测试。

package main

import "testing"

func TestGetPidFileFromConfigDefaultOnUnix(t *testing.T) {
	defer saveGatewayState(t)()

	if got := getPidFileFromConfig(); got != "/dev/shm/mc-gateway.pid" {
		t.Fatalf("getPidFileFromConfig() = %q, want /dev/shm/mc-gateway.pid", got)
	}
}
