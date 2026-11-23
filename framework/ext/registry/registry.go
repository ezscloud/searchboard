// Package registry 服务注册与发现（接口抽象 + 内存实现）。
//
// 对标 hertz-contrib/registry（nacos/consul/etcd/eureka/polaris/
// servicecomb/zookeeper/redis 多实现）。本仓库的取舍：
//   - 核心接口（Registry/Resolver）与 Hertz 语义同构，业务可无缝换具体实现；
//   - 内置 RegistryMemory（开发/测试/单机网格）；
//   - 生产接入任一注册中心 = 实现两个接口（见 README 的对接指引表）。
package registry

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

// ServiceInfo 服务实例信息。
type ServiceInfo struct {
	Service  string // 服务名
	Addr     string // host:port
	Metadata map[string]string
}

// Registry 注册中心（注册方）。
type Registry interface {
	// Register 注册实例（TTL 心跳由实现方保证；ctx 取消即注销）。
	Register(ctx context.Context, info ServiceInfo) error
	// Deregister 主动注销。
	Deregister(ctx context.Context, info ServiceInfo) error
}

// Resolver 服务发现（消费方）。
type Resolver interface {
	// Resolve 解析服务全部健康实例。
	Resolve(ctx context.Context, service string) ([]ServiceInfo, error)
}

// ErrServiceNotFound 服务无健康实例。
var ErrServiceNotFound = errors.New("registry: service not found")

// ---------------------------------------------------------------- 内存实现

// Memory 内存注册中心（含 TTL 健康剔除与变更通知）。
type Memory struct {
	mu       sync.RWMutex
	services map[string]map[string]serviceEntry // service → addr → entry
	watchers map[string][]chan struct{}         // service → 订阅者
	ttl      time.Duration
}

type serviceEntry struct {
	info          ServiceInfo
	lastHeartbeat time.Time
}

// NewMemory 创建内存注册中心（ttl 默认 15s，实例超时未心跳即剔除）。
func NewMemory(ttl time.Duration) *Memory {
	if ttl <= 0 {
		ttl = 15 * time.Second
	}
	m := &Memory{
		services: map[string]map[string]serviceEntry{},
		watchers: map[string][]chan struct{}{},
		ttl:      ttl,
	}
	go m.sweepLoop(ttl / 3)
	return m
}

// Register implements Registry（同时内置心跳自续）。
func (m *Memory) Register(ctx context.Context, info ServiceInfo) error {
	m.mu.Lock()
	eps := m.services[info.Service]
	if eps == nil {
		eps = map[string]serviceEntry{}
		m.services[info.Service] = eps
	}
	eps[info.Addr] = serviceEntry{info: info, lastHeartbeat: time.Now()}
	m.mu.Unlock()
	m.notify(info.Service)

	// ctx 取消即注销（长生命周期实例把服务 ctx 传入即可）
	if ctx != nil {
		go func() {
			<-ctx.Done()
			_ = m.Deregister(context.Background(), info)
		}()
	}
	return nil
}

// Deregister implements Registry。
func (m *Memory) Deregister(ctx context.Context, info ServiceInfo) error {
	m.mu.Lock()
	if eps := m.services[info.Service]; eps != nil {
		delete(eps, info.Addr)
		if len(eps) == 0 {
			delete(m.services, info.Service)
		}
	}
	m.mu.Unlock()
	m.notify(info.Service)
	return nil
}

// Resolve implements Resolver（剔除超时实例）。
func (m *Memory) Resolve(ctx context.Context, service string) ([]ServiceInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	eps := m.services[service]
	if len(eps) == 0 {
		return nil, ErrServiceNotFound
	}
	now := time.Now()
	var out []ServiceInfo
	for _, e := range eps {
		if now.Sub(e.lastHeartbeat) <= m.ttl {
			out = append(out, e.info)
		}
	}
	if len(out) == 0 {
		return nil, ErrServiceNotFound
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr < out[j].Addr })
	return out, nil
}

// Heartbeat 心跳续期（实现方为外部探活时调用）。
func (m *Memory) Heartbeat(service, addr string) {
	m.mu.Lock()
	if eps := m.services[service]; eps != nil {
		if e, ok := eps[addr]; ok {
			e.lastHeartbeat = time.Now()
			eps[addr] = e
		}
	}
	m.mu.Unlock()
}

// Watch 订阅服务变更（每实例一次信号）。
func (m *Memory) Watch(service string) <-chan struct{} {
	ch := make(chan struct{}, 1)
	m.mu.Lock()
	m.watchers[service] = append(m.watchers[service], ch)
	m.mu.Unlock()
	return ch
}

func (m *Memory) notify(service string) {
	m.mu.Lock()
	ws := m.watchers[service]
	m.mu.Unlock()
	for _, ch := range ws {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (m *Memory) sweepLoop(interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		m.mu.Lock()
		for svc, eps := range m.services {
			for addr, e := range eps {
				if now.Sub(e.lastHeartbeat) > m.ttl {
					delete(eps, addr)
				}
			}
			if len(eps) == 0 {
				delete(m.services, svc)
			}
		}
		m.mu.Unlock()
	}
}

var _ = strings.TrimSpace
