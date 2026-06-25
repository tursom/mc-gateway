package main

import (
	"context"
	"sync"

	"github.com/tursom/mc-gateway/internal/adminroute"
)

var (
	routeSnapshot  = adminroute.NewSnapshot()
	routeWriteLock sync.Mutex
)

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

func publishRouteSnapshot(routes map[string]string) {
	routeSnapshot.Store(routes)
}

func lookupRoute(host string) (string, bool) {
	return routeSnapshot.Lookup(host)
}

func listRoutes(ctx context.Context, query string) ([]adminroute.Record, error) {
	return adminroute.NewRepository(adminDB).List(ctx, query)
}

func upsertRoute(ctx context.Context, actor, host, upstream string, enabled bool, note string) error {
	routeWriteLock.Lock()
	defer routeWriteLock.Unlock()

	if err := adminroute.NewRepository(adminDB).Upsert(ctx, actor, host, upstream, enabled, note); err != nil {
		return err
	}
	return refreshRouteSnapshot(ctx)
}

func deleteRoute(ctx context.Context, actor, host string) error {
	routeWriteLock.Lock()
	defer routeWriteLock.Unlock()

	if err := adminroute.NewRepository(adminDB).Delete(ctx, host); err != nil {
		return err
	}
	_ = actor
	return refreshRouteSnapshot(ctx)
}
