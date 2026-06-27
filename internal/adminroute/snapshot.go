// internal/adminroute/snapshot.go 把路由行转换为热路径使用的不可变查找映射。

package adminroute

import "sync/atomic"

type Snapshot struct {
	// atomic.Value 保存整张路由表，连接热路径读取时无需加锁。
	value atomic.Value
}

func NewSnapshot() *Snapshot {
	return &Snapshot{}
}

func (s *Snapshot) Store(routes map[string]string) {
	if routes == nil {
		routes = map[string]string{}
	}
	// Store 前复制 map，避免调用方发布后继续修改导致并发读写 map。
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
	// Clone 返回副本，管理端或测试修改结果不会影响热路径快照。
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
	// default 是显式兜底路由，只有精确 host 未命中时才使用。
	upstream, ok = routes["default"]
	return upstream, ok
}
