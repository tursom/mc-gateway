// cmd/gateway/pid_unix.go 实现 Unix 平台的 pid 文件占用检查，避免覆盖仍在运行的进程记录。

// pid_unix.go
//go:build unix || plan9

package main

func getPidFileFromConfig() string {
	pidFile := config.PidFile
	if pidFile == "" {
		return "/dev/shm/mc-gateway.pid"
	}
	return pidFile
}
