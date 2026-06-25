package adminroute

import "sync/atomic"

type Snapshot struct {
	value atomic.Value
}

func NewSnapshot() *Snapshot {
	return &Snapshot{}
}

func (s *Snapshot) Store(routes map[string]string) {
	if routes == nil {
		routes = map[string]string{}
	}
	copied := make(map[string]string, len(routes))
	for host, upstream := range routes {
		copied[host] = upstream
	}
	s.value.Store(copied)
}

func (s *Snapshot) Clone() map[string]string {
	value := s.value.Load()
	if value == nil {
		return nil
	}
	routes, ok := value.(map[string]string)
	if !ok {
		return nil
	}
	copied := make(map[string]string, len(routes))
	for host, upstream := range routes {
		copied[host] = upstream
	}
	return copied
}

func (s *Snapshot) Lookup(host string) (string, bool) {
	value := s.value.Load()
	if value == nil {
		return "", false
	}
	routes, ok := value.(map[string]string)
	if !ok {
		return "", false
	}
	upstream, ok := routes[host]
	if ok {
		return upstream, true
	}
	upstream, ok = routes["default"]
	return upstream, ok
}
