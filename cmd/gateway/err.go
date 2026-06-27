// cmd/gateway/err.go 集中放置网关请求路径使用的少量哨兵错误。

package main

import "errors"

var (
	errEmptyBuffer = errors.New("empty buffer")
)
