// internal/pluginmanager/future_runtime_managers.go holds concrete future-runtime
// manager constructors shared by sandbox, WASM, and ingress adapters.

package pluginmanager

import "sync"

func NewWASMRunner() *WASMRunner {
	return &WASMRunner{compiled: make(map[string][]byte)}
}

type IngressLifecycleManager struct {
	repo      Repository
	reserved  []IngressReservedListener
	mu        sync.Mutex
	listeners map[string]*IngressListener
}

func NewIngressLifecycleManager(repo Repository, reserved []IngressReservedListener) *IngressLifecycleManager {
	return &IngressLifecycleManager{
		repo:      repo,
		reserved:  append([]IngressReservedListener(nil), reserved...),
		listeners: make(map[string]*IngressListener),
	}
}
