// cmd/gateway/pid.go 维护 pid 文件的写入和清理，供进程管理器按文件追踪网关进程。

package main

import (
	"fmt"
	"os"
)

var currentPidFile string

func writePIDFile() error {
	newPidFile := getPidFileFromConfig()
	if newPidFile == "" || newPidFile == currentPidFile {
		return nil
	}

	if currentPidFile != "" {
		removePIDFile()
	}
	currentPidFile = newPidFile

	pid := os.Getpid()
	if err := os.WriteFile(newPidFile, []byte(fmt.Sprintf("%d\n", pid)), 0644); err != nil {
		return err
	}

	return nil
}

func removePIDFile() {
	os.Remove(currentPidFile)
}
