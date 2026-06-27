// cmd/gateway/admin_routes.go 让内存中的主机到上游映射快照与 SQLite 路由记录保持同步。

package main

import (
	"context"
	"sync"

	"github.com/tursom/mc-gateway/internal/adminroute"
)

var (
	// routeSnapshot 是连接热路径读取的不可变快照；写路径通过 Store 整体替换它。
	routeSnapshot = adminroute.NewSnapshot()
	// routeWriteLock 串行化路由写入和快照刷新，避免并发写导致后写库、先发布的顺序错乱。
	routeWriteLock sync.Mutex
)

// refreshRouteSnapshot 从 SQLite 读取启用路由并发布到热路径。数据库尚未初始化时
// 发布空快照，方便测试和早期启动路径调用。
func refreshRouteSnapshot(ctx context.Context) error {
	if adminDB == nil {
		publishRouteSnapshot(map[string]string{})
		return nil
	}

	routes, err := adminroute.NewRepository(adminDB).EnabledMap(ctx)
	if err != nil {
		return err
	}
	publishRouteSnapshot(routes)
	return nil
}

// publishRouteSnapshot 原子替换当前路由快照；调用方应传入新 map，避免发布后继续修改。
func publishRouteSnapshot(routes map[string]string) {
	routeSnapshot.Store(routes)
}

// lookupRoute 是连接热路径使用的只读查找函数，不访问 SQLite。
func lookupRoute(host string) (string, bool) {
	return routeSnapshot.Lookup(host)
}

func listRoutes(ctx context.Context, query string) ([]adminroute.Record, error) {
	return adminroute.NewRepository(adminDB).List(ctx, query)
}

func upsertRoute(ctx context.Context, actor, host, upstream string, enabled bool, note string) error {
	routeWriteLock.Lock()
	defer routeWriteLock.Unlock()

	// 路由写入成功后必须立即刷新内存快照，否则管理端保存的配置不会影响新连接。
	if err := adminroute.NewRepository(adminDB).Upsert(ctx, actor, host, upstream, enabled, note); err != nil {
		return err
	}
	return refreshRouteSnapshot(ctx)
}

func deleteRoute(ctx context.Context, actor, host string) error {
	routeWriteLock.Lock()
	defer routeWriteLock.Unlock()

	// 删除也走同一把锁，确保快照刷新顺序与数据库提交顺序一致。
	if err := adminroute.NewRepository(adminDB).Delete(ctx, host); err != nil {
		return err
	}
	_ = actor
	return refreshRouteSnapshot(ctx)
}
