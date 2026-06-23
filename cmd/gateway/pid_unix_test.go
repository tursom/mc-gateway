//go:build unix || plan9

package main

import "testing"

func TestGetPidFileFromConfigDefaultOnUnix(t *testing.T) {
	defer saveGatewayState(t)()

	if got := getPidFileFromConfig(); got != "/dev/shm/mc-gateway.pid" {
		t.Fatalf("getPidFileFromConfig() = %q, want /dev/shm/mc-gateway.pid", got)
	}
}
