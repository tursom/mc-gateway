//go:build !((darwin && (amd64 || arm64)) || (freebsd && (amd64 || arm64)) || (linux && (386 || amd64 || arm || arm64 || loong64 || ppc64le || riscv64 || s390x)) || (openbsd && (amd64 || arm64)) || (windows && (386 || amd64 || arm64)))

// internal/admindb/db_unsupported.go 在纯 Go SQLite 驱动未被构建标签启用的平台上返回明确错误。

package admindb

import (
	"database/sql"
	"fmt"
	"runtime"
)

func Open(string) (*sql.DB, error) {
	return nil, fmt.Errorf("sqlite admin database is not supported on %s/%s", runtime.GOOS, runtime.GOARCH)
}

func Migrate(*sql.DB) error {
	return fmt.Errorf("sqlite admin database is not supported on %s/%s", runtime.GOOS, runtime.GOARCH)
}
