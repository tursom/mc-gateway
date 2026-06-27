// cmd/gateway/pid_windows.go 在 Windows 上提供可移植的 pid 文件占用检查替代实现。

// pid_windows.go
//go:build windows

package main

func getPidFileFromConfig() string {
	return config.PidFile
}
