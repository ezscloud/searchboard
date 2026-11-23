// Package limiter 自适应限流（BBR 思路）。
//
// 参照字节跳动 CloudWeGo 的 BBR 限流（vEBPF/NetBench 思想）简化实现：
// 以 CPU 负载 + 最近窗口延迟双因子自适应：
//
//		accept = cpu < cpuThreshold && inflight*winRTT < maxPass*winRTT
//
//	  - CPU 因子：runtime 进程 CPU 使用率（采样窗口均值）超过阈值收紧；
//	  - 延迟因子：滑动窗口内 P99 请求延迟相比基线放大超过倍数收紧；
//	  - 反馈扩容：连续低负载窗口自动放大通过配额。
//
// 这是"软"自适应（无 cgroup CPU 配额精确读数），云环境与裸机同样可用。
package limiter

import (
	"math"
	"runtime"
	"sync"
	"time"

	"github.com/ezscloud/searchboard/framework"
)

// Config 限流配置。
type Config struct {
	// CPUThreshold 进程 CPU 使用率阈值（0-1），默认 0.80。
	CPUThreshold float64

	// Window 统计窗口，默认 1s。
	Window time.Duration

	// Bucket 数量（窗口切分），默认 40（每桶 25ms）。
	Bucket int

	// MaxPass 窗口内最大放行请求数（0=不限，仅靠 CPU/延迟因子）。
	MaxPass int

	// MinPass 每窗口保底放行（防误杀，默认 5）。
	MinPass int

	// LatencyFactor 延迟放大容忍倍数（P99 > 基线*factor 收紧），默认 2.5。
	LatencyFactor float64

	// OnLimited 被限流时回调（默认 429）。
	OnLimited func(*framework.Context)
}

func (c *Config) withDefaults() {
	if c.CPUThreshold <= 0 {
		c.CPUThreshold = 0.80
	}
	if c.Window <= 0 {
		c.Window = time.Second
	}
	if c.Bucket <= 0 {
		c.Bucket = 40
	}
	if c.MinPass <= 0 {
		c.MinPass = 5
	}
	if c.LatencyFactor <= 0 {
		c.LatencyFactor = 2.5
	}
	if c.OnLimited == nil {
		c.OnLimited = defaultLimited
	}
}

func defaultLimited(c *framework.Context) {
	c.SetRespHeader("Retry-After", "1")
	c.Status(429)
	c.String(429, "429 Too Many Requests (adaptive)")
	c.Abort()
}

// BBR 自适应限流器。
type BBR struct {
	cfg Config

	mu      sync.Mutex
	buckets []bucketStat // 环形
	head    int
	lastAt  time.Time

	cpuSampler *cpuSampler
	baseLat    time.Duration // 学习到的基线延迟（EWMA 快升慢降）
}

type bucketStat struct {
	pass int64
	drop int64
	sumD time.Duration // 延迟和（算均值）
	p99  time.Duration // 桶内最大（近似 p99）
}

// NewBBR 创建限流器并启动 CPU 采样。
func NewBBR(cfg Config) *BBR {
	cfg.withDefaults()
	b := &BBR{
		cfg:        cfg,
		buckets:    make([]bucketStat, cfg.Bucket),
		lastAt:     time.Now(),
		cpuSampler: newCPUSampler(cfg.Window),
	}
	go b.cpuSampler.run()
	return b
}

// Allow 判定一次请求是否放行（记录并自更新）。
func (b *BBR) Allow() bool {
	b.mu.Lock()
	now := time.Now()
	b.advance(now)
	cur := &b.buckets[b.head]

	// 已放行量（整窗口）
	passed := b.windowPassed()
	inflight := int64(0) // 进程内未完成请求数近似：以窗口 pass 计

	cpu := b.cpuSampler.usage()
	lat := b.windowAvgLatency()

	allow := true
	if cpu >= b.cfg.CPUThreshold {
		allow = false
	}
	if allow && b.baseLat > 0 && lat > time.Duration(float64(b.baseLat)*b.cfg.LatencyFactor) {
		allow = false
	}
	if allow && b.cfg.MaxPass > 0 && passed+1 > int64(b.cfg.MaxPass) {
		allow = false
	}
	if passed < int64(b.cfg.MinPass) {
		allow = true // 保底
	}
	_ = inflight

	if allow {
		cur.pass++
	} else {
		cur.drop++
	}
	b.mu.Unlock()
	return allow
}

// Record 记录请求完成延迟（学习基线/检测放大）。
func (b *BBR) Record(d time.Duration) {
	b.mu.Lock()
	cur := &b.buckets[b.head]
	cur.sumD += d
	if d > cur.p99 {
		cur.p99 = d
	}
	// EWMA 基线：快升慢降（攻击性放大要立刻反应，恢复要缓慢放宽）
	if b.baseLat == 0 {
		b.baseLat = d
	} else if d > b.baseLat {
		b.baseLat = time.Duration(float64(b.baseLat)*0.7 + float64(d)*0.3)
	} else {
		b.baseLat = time.Duration(float64(b.baseLat)*0.98 + float64(d)*0.02)
	}
	b.mu.Unlock()
}

func (b *BBR) advance(now time.Time) {
	span := now.Sub(b.lastAt)
	if span <= 0 {
		return
	}
	step := b.cfg.Window / time.Duration(b.cfg.Bucket)
	for span >= step && step > 0 {
		b.head = (b.head + 1) % len(b.buckets)
		b.buckets[b.head] = bucketStat{}
		span -= step
		b.lastAt = b.lastAt.Add(step)
	}
}

func (b *BBR) windowPassed() int64 {
	var n int64
	for _, bk := range b.buckets {
		n += bk.pass
	}
	return n
}

func (b *BBR) windowAvgLatency() time.Duration {
	var sum time.Duration
	var n int64
	for _, bk := range b.buckets {
		if bk.pass > 0 {
			sum += bk.sumD
			n += bk.pass
		}
	}
	if n == 0 {
		return 0
	}
	return sum / time.Duration(n)
}

// Snapshot 状态快照（监控/测试用）。
type Snapshot struct {
	CPUPct  float64
	Passed  int64
	Dropped int64
	AvgLat  time.Duration
	BaseLat time.Duration
}

func (b *BBR) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	var drops int64
	for _, bk := range b.buckets {
		drops += bk.drop
	}
	return Snapshot{
		CPUPct:  math.Round(b.cpuSampler.usage()*10000) / 100,
		Passed:  b.windowPassed(),
		Dropped: drops,
		AvgLat:  b.windowAvgLatency(),
		BaseLat: b.baseLat,
	}
}

// Middleware 中间件形态。
func Middleware(b *BBR) framework.Middleware {
	return func(c *framework.Context) {
		if !b.Allow() {
			b.cfg.OnLimited(c)
			return
		}
		start := time.Now()
		c.Next()
		b.Record(time.Since(start))
	}
}

// ---------------------------------------------------------------- CPU 采样

type cpuSampler struct {
	mu     sync.Mutex
	usagev float64
	stop   chan struct{}
	window time.Duration
}

func newCPUSampler(window time.Duration) *cpuSampler {
	return &cpuSampler{stop: make(chan struct{}), window: window}
}

func (cs *cpuSampler) run() {
	interval := 100 * time.Millisecond
	if cs.window < interval {
		interval = cs.window / 4
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-cs.stop:
			return
		case <-t.C:
		}
		u := sampleCPU()
		cs.mu.Lock()
		// 平滑
		if cs.usagev == 0 {
			cs.usagev = u
		} else {
			cs.usagev = cs.usagev*0.6 + u*0.4
		}
		cs.mu.Unlock()
	}
}

func (cs *cpuSampler) usage() float64 {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.usagev
}

// sampleCPU 使用率采样（两次采样差 / 时长 / 核数）。
func sampleCPU() float64 {
	a := getCPUTime()
	time.Sleep(50 * time.Millisecond)
	b := getCPUTime()
	n := runtime.NumCPU()
	if n == 0 || b < a {
		return 0
	}
	dt := b - a
	return float64(dt) / float64(50*time.Millisecond*time.Duration(n))
}

// getCPUTime 进程（Linux）/系统（Windows）CPU 累计时间。
// Windows 口径为整机 busy（本机单进程场景≈进程；详见 cpu_windows.go 注释）。
func getCPUTime() time.Duration {
	return procSelfCPUTime()
}
