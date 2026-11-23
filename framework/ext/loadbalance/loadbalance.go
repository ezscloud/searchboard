// Package loadbalance 负载均衡器。
//
// 内置：RoundRobin、Random、WeightedRandom、ConsistentHash、LeastRequest。
// 上游健康状态由调用方经 Report 反馈（连续失败摘除 + 冷却期后半开试探）。
package loadbalance

import (
	"errors"
	"hash/fnv"
	"math/rand"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Endpoint 上游节点。
type Endpoint struct {
	Addr   string // host:port
	Weight int    // 默认 1
}

// ErrNoAvailable 全部上游摘除。
var ErrNoAvailable = errors.New("loadbalance: no available endpoint")

// Picker 选择器接口。
type Picker interface {
	// Pick 选择一个可用上游（key 为请求特征；RR/Random 忽略）。
	Pick(key string) (*Endpoint, error)
	// Report 反馈健康（成功清零失败计数；失败累计至阈值后摘除）。
	Report(ep *Endpoint, ok bool)
	// Endpoints 全量视图（监控用）。
	Endpoints() []EndpointStat
}

// EndpointStat 节点状态。
type EndpointStat struct {
	Endpoint
	Available bool
	Fails     int64
	Successes int64
}

// Strategy 均衡策略。
type Strategy string

const (
	RoundRobin     Strategy = "round-robin"
	Random         Strategy = "random"
	WeightedRandom Strategy = "weighted-random"
	ConsistentHash Strategy = "consistent-hash"
	LeastRequest   Strategy = "least-request"
)

// Config 均衡器配置。
type Config struct {
	Strategy Strategy // 默认 RoundRobin

	// FailThreshold 连续失败多少次摘除（默认 3）。
	FailThreshold int

	// Cooldown 摘除后冷却（默认 10s；期满半开放行试探，成功即恢复）。
	Cooldown time.Duration

	// HashReplicas 一致性哈希虚拟节点数（默认 160）。
	HashReplicas int
}

func (c *Config) withDefaults() {
	if c.Strategy == "" {
		c.Strategy = RoundRobin
	}
	if c.FailThreshold <= 0 {
		c.FailThreshold = 3
	}
	if c.Cooldown <= 0 {
		c.Cooldown = 10 * time.Second
	}
	if c.HashReplicas <= 0 {
		c.HashReplicas = 160
	}
}

// balancer 公共骨架：节点表 + 健康状态机。
type balancer struct {
	mu       sync.RWMutex
	eps      []*epState
	rr       atomic.Int64
	rnd      *rand.Rand
	failTh   int
	cooldown time.Duration
}

type epState struct {
	Endpoint
	failCnt          int
	failDeadline     time.Time // 非零=已被摘除，冷却至该时刻
	fails, successes int64
}

// New 创建均衡器。
func New(eps []Endpoint, cfg Config) Picker {
	cfg.withDefaults()
	b := &balancer{
		rnd:      rand.New(rand.NewSource(time.Now().UnixNano())),
		failTh:   cfg.FailThreshold,
		cooldown: cfg.Cooldown,
	}
	for _, e := range eps {
		if e.Weight <= 0 {
			e.Weight = 1
		}
		b.eps = append(b.eps, &epState{Endpoint: e})
	}
	switch cfg.Strategy {
	case Random:
		return &randomPicker{balancer: b}
	case WeightedRandom:
		return &weightedRandomPicker{balancer: b}
	case ConsistentHash:
		return newConsistentHash(b, cfg.HashReplicas)
	case LeastRequest:
		return &leastRequestPicker{balancer: b}
	default:
		return &rrPicker{balancer: b}
	}
}

// available 返回可用节点：正常 + （已摘除但冷却期满的）半开节点。
func (b *balancer) available() []*epState {
	b.mu.RLock()
	defer b.mu.RUnlock()
	now := time.Now()
	var out []*epState
	for _, s := range b.eps {
		if s.failDeadline.IsZero() || now.After(s.failDeadline) {
			out = append(out, s)
		}
	}
	return out
}

// report 健康反馈状态机。
func (b *balancer) report(ep *Endpoint, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.eps {
		if s.Addr != ep.Addr {
			continue
		}
		if ok {
			s.successes++
			s.failCnt = 0
			s.failDeadline = time.Time{} // 半开试探成功 → 恢复
		} else {
			s.fails++
			s.failCnt++
			if s.failCnt >= b.failTh {
				// 摘除；冷却期满后进入半开（available 会放行试探流量）
				s.failDeadline = time.Now().Add(b.cooldown)
				s.failCnt = 0
			}
		}
		return
	}
}

func (b *balancer) stats() []EndpointStat {
	b.mu.RLock()
	defer b.mu.RUnlock()
	now := time.Now()
	out := make([]EndpointStat, 0, len(b.eps))
	for _, s := range b.eps {
		avail := s.failDeadline.IsZero() || now.After(s.failDeadline)
		out = append(out, EndpointStat{
			Endpoint: s.Endpoint, Available: avail,
			Fails: s.fails, Successes: s.successes,
		})
	}
	return out
}

func (b *balancer) all() []*epState {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.eps
}

// ---------------------------------------------------------------- 各策略

type rrPicker struct{ *balancer }

func (p *rrPicker) Pick(string) (*Endpoint, error) {
	av := p.available()
	if len(av) == 0 {
		return nil, ErrNoAvailable
	}
	n := p.rr.Add(1)
	s := av[int(uint64(n)%uint64(len(av)))]
	return &s.Endpoint, nil
}

func (p *rrPicker) Report(ep *Endpoint, ok bool) { p.report(ep, ok) }
func (p *rrPicker) Endpoints() []EndpointStat    { return p.stats() }

type randomPicker struct{ *balancer }

func (p *randomPicker) Pick(string) (*Endpoint, error) {
	av := p.available()
	if len(av) == 0 {
		return nil, ErrNoAvailable
	}
	p.mu.RLock()
	i := p.rnd.Intn(len(av))
	p.mu.RUnlock()
	s := av[i]
	return &s.Endpoint, nil
}

func (p *randomPicker) Report(ep *Endpoint, ok bool) { p.report(ep, ok) }
func (p *randomPicker) Endpoints() []EndpointStat    { return p.stats() }

type weightedRandomPicker struct{ *balancer }

func (p *weightedRandomPicker) Pick(string) (*Endpoint, error) {
	av := p.available()
	if len(av) == 0 {
		return nil, ErrNoAvailable
	}
	total := 0
	for _, s := range av {
		total += s.Weight
	}
	p.mu.RLock()
	r := p.rnd.Intn(total)
	p.mu.RUnlock()
	for _, s := range av {
		r -= s.Weight
		if r < 0 {
			return &s.Endpoint, nil
		}
	}
	last := av[len(av)-1]
	return &last.Endpoint, nil
}

func (p *weightedRandomPicker) Report(ep *Endpoint, ok bool) { p.report(ep, ok) }
func (p *weightedRandomPicker) Endpoints() []EndpointStat    { return p.stats() }

// leastRequestPicker 请求最少者优先（以近期成功数近似负载）。
type leastRequestPicker struct{ *balancer }

func (p *leastRequestPicker) Pick(string) (*Endpoint, error) {
	av := p.available()
	if len(av) == 0 {
		return nil, ErrNoAvailable
	}
	sort.Slice(av, func(i, j int) bool { return av[i].successes < av[j].successes })
	s := av[0]
	return &s.Endpoint, nil
}

func (p *leastRequestPicker) Report(ep *Endpoint, ok bool) { p.report(ep, ok) }
func (p *leastRequestPicker) Endpoints() []EndpointStat    { return p.stats() }

// consistentHashPicker 一致性哈希（虚拟节点环）。
type consistentHashPicker struct {
	*balancer
	ring  []uint32
	owner map[uint32]int // 虚拟节点哈希 → eps 下标
}

func newConsistentHash(b *balancer, replicas int) *consistentHashPicker {
	p := &consistentHashPicker{balancer: b, owner: map[uint32]int{}}
	b.mu.Lock()
	for idx, s := range b.eps {
		for r := 0; r < replicas; r++ {
			h := fnv32(s.Addr + "#" + strconv.Itoa(r))
			p.ring = append(p.ring, h)
			p.owner[h] = idx
		}
	}
	b.mu.Unlock()
	sort.Slice(p.ring, func(i, j int) bool { return p.ring[i] < p.ring[j] })
	return p
}

func (p *consistentHashPicker) Pick(key string) (*Endpoint, error) {
	if key == "" {
		key = "default"
	}
	av := p.available()
	if len(av) == 0 {
		return nil, ErrNoAvailable
	}
	avSet := map[string]bool{}
	for _, s := range av {
		avSet[s.Addr] = true
	}
	all := p.all()

	h := fnv32(key)
	i := sort.Search(len(p.ring), func(i int) bool { return p.ring[i] >= h })
	for n := 0; n < len(p.ring); n++ {
		j := (i + n) % len(p.ring)
		if idx, ok := p.owner[p.ring[j]]; ok && idx < len(all) {
			ep := &all[idx].Endpoint
			if avSet[ep.Addr] {
				return ep, nil
			}
		}
	}
	fallback := av[0]
	return &fallback.Endpoint, nil
}

func (p *consistentHashPicker) Report(ep *Endpoint, ok bool) { p.report(ep, ok) }
func (p *consistentHashPicker) Endpoints() []EndpointStat    { return p.stats() }

func fnv32(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}
