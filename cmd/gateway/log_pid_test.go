package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func TestWritePIDFileCreatesAndSwitchesFiles(t *testing.T) {
	defer saveGatewayState(t)()

	tmpDir := t.TempDir()
	firstPID := filepath.Join(tmpDir, "first.pid")
	secondPID := filepath.Join(tmpDir, "second.pid")

	config.PidFile = firstPID
	if err := writePIDFile(); err != nil {
		t.Fatalf("writePIDFile(first) error = %v", err)
	}
	assertPIDFile(t, firstPID)

	config.PidFile = secondPID
	if err := writePIDFile(); err != nil {
		t.Fatalf("writePIDFile(second) error = %v", err)
	}
	if _, err := os.Stat(firstPID); !os.IsNotExist(err) {
		t.Fatalf("first pid file still exists or stat failed: %v", err)
	}
	assertPIDFile(t, secondPID)

	removePIDFile()
	if _, err := os.Stat(secondPID); !os.IsNotExist(err) {
		t.Fatalf("second pid file still exists or stat failed: %v", err)
	}
}

func TestWritePIDFileNoopsWhenPathUnchanged(t *testing.T) {
	defer saveGatewayState(t)()

	pidPath := filepath.Join(t.TempDir(), "gateway.pid")
	config.PidFile = pidPath
	if err := writePIDFile(); err != nil {
		t.Fatalf("writePIDFile() error = %v", err)
	}
	firstStat, err := os.Stat(pidPath)
	if err != nil {
		t.Fatalf("Stat(first) error = %v", err)
	}

	if err := writePIDFile(); err != nil {
		t.Fatalf("writePIDFile() second error = %v", err)
	}
	secondStat, err := os.Stat(pidPath)
	if err != nil {
		t.Fatalf("Stat(second) error = %v", err)
	}
	if !firstStat.ModTime().Equal(secondStat.ModTime()) {
		t.Fatalf("pid file mod time changed: %v -> %v", firstStat.ModTime(), secondStat.ModTime())
	}
}

func TestLoadLoggerDefaultLevelAndInvalidLevel(t *testing.T) {
	defer saveGatewayState(t)()

	if err := loadLogger(); err != nil {
		t.Fatalf("loadLogger() error = %v", err)
	}
	if config.Log.Level != "info" {
		t.Fatalf("default log level = %q, want info", config.Log.Level)
	}
	if got := log.Logger.GetLevel(); got != zerolog.InfoLevel {
		t.Fatalf("logger level = %v, want %v", got, zerolog.InfoLevel)
	}

	config.Log.Level = "not-a-level"
	if err := loadLogger(); err == nil {
		t.Fatal("loadLogger() error = nil, want invalid level error")
	}
}

func TestLoadLoggerCreatesLogFile(t *testing.T) {
	defer saveGatewayState(t)()

	logPath := filepath.Join(t.TempDir(), "nested", "gateway.log")
	config.Log.Level = "debug"
	config.Log.File = logPath

	if err := loadLogger(); err != nil {
		t.Fatalf("loadLogger() error = %v", err)
	}
	if currentLogFile != logPath {
		t.Fatalf("currentLogFile = %q, want %q", currentLogFile, logPath)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("Stat(log file) error = %v", err)
	}
	if got := log.Logger.GetLevel(); got != zerolog.DebugLevel {
		t.Fatalf("logger level = %v, want %v", got, zerolog.DebugLevel)
	}
}

func TestReopenLogFile(t *testing.T) {
	defer saveGatewayState(t)()

	if err := reopenLogFile(); err != nil {
		t.Fatalf("reopenLogFile(empty) error = %v", err)
	}

	logPath := filepath.Join(t.TempDir(), "gateway.log")
	config.Log.File = logPath
	if err := reopenLogFile(); err != nil {
		t.Fatalf("reopenLogFile() error = %v", err)
	}
	if currentLogFile != logPath {
		t.Fatalf("currentLogFile = %q, want %q", currentLogFile, logPath)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("Stat(log file) error = %v", err)
	}
}

func assertPIDFile(t *testing.T, path string) {
	t.Helper()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	want := fmt.Sprintf("%d\n", os.Getpid())
	if string(got) != want {
		t.Fatalf("pid file %s = %q, want %q", path, string(got), want)
	}
}
