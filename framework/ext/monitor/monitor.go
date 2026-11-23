// Package monitor Prometheus 指标暴露（文本格式 0.0.4）。
//
// 与 hertz-contrib/monitor-prometheus 差异：不拉 client_golang 依赖，
// 内置计数器/直方图/仪表（内存注册表），按 Prom 文本格式输出 /metrics。
// 与客户端注册的语义差异见 README（多实例聚合由 Prometheus 服务端完成）。
package monitor

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ezscloud/searchboard/framework"
)

// Registry 指标注册表。
type Registry struct {
	mu       sync.RWMutex
	counters map[string]*Counter
	gauges   map[string]*Gauge
	hists    map[string]*Histogram

	namespace string
}

// New 创建注册表（namespace 可为空）。
func New(namespace string) *Registry {
	return &Registry{
		counters:  map[string]*Counter{},
		gauges:    map[string]*Gauge{},
		hists:     map[string]*Histogram{},
		namespace: namespace,
	}
}

// Counter 单调计数器。
type Counter struct {
	name, help string
	mu         sync.Mutex
	byLabels   map[string]float64
}

// Gauge 即时值。
type Gauge struct {
	name, help string
	mu         sync.Mutex
	byLabels   map[string]float64
	fn         func() float64 // 采集函数（如内存用量）
}

// Histogram 桶式直方图（线性/指数桶由注册方给定）。
type Histogram struct {
	name, help string
	buckets    []float64
	mu         sync.Mutex
	counts     map[string][]uint64
	sums       map[string]float64
	totals     map[string]uint64
}

// Counter 注册/取用计数器（不存在则创建）。
func (r *Registry) Counter(name, help string) *Counter {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := r.full(name)
	c, ok := r.counters[key]
	if !ok {
		c = &Counter{name: key, help: help, byLabels: map[string]float64{}}
		r.counters[key] = c
	}
	return c
}

// Gauge 注册/取用仪表（fn 为周期采集函数，可为 nil 由 Add/Set 驱动）。
func (r *Registry) Gauge(name, help string, fn func() float64) *Gauge {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := r.full(name)
	g, ok := r.gauges[key]
	if !ok {
		g = &Gauge{name: key, help: help, byLabels: map[string]float64{}, fn: fn}
		r.gauges[key] = g
	}
	return g
}

// DefaultBuckets 默认直方图桶（秒级延迟友好）。
var DefaultBuckets = []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Histogram 注册/取用直方图。
func (r *Registry) Histogram(name, help string, buckets []float64) *Histogram {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := r.full(name)
	h, ok := r.hists[key]
	if !ok {
		if len(buckets) == 0 {
			buckets = DefaultBuckets
		}
		h = &Histogram{
			name: key, help: help, buckets: append([]float64(nil), buckets...),
			counts: map[string][]uint64{}, sums: map[string]float64{}, totals: map[string]uint64{},
		}
		r.hists[key] = h
	}
	return h
}

func (r *Registry) full(name string) string {
	if r.namespace == "" {
		return name
	}
	return r.namespace + "_" + name
}

// ---------------------------------------------------------------- 计数操作

// Inc 无标签计数 +1。
func (c *Counter) Inc() { c.Add(1, "") }

// Add 带标签计数（labels 形如 `method="GET",code="200"`）。
func (c *Counter) Add(v float64, labels string) {
	c.mu.Lock()
	c.byLabels[labels] += v
	c.mu.Unlock()
}

// Set 仪表置值。
func (g *Gauge) Set(v float64, labels string) {
	g.mu.Lock()
	g.byLabels[labels] = v
	g.mu.Unlock()
}

// Observe 直方图采样。
func (h *Histogram) Observe(v float64, labels string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cs, ok := h.counts[labels]
	if !ok {
		cs = make([]uint64, len(h.buckets))
		h.counts[labels] = cs
	}
	for i, up := range h.buckets {
		if v <= up {
			cs[i]++
		}
	}
	h.sums[labels] += v
	h.totals[labels]++
}

// ---------------------------------------------------------------- 输出

// Render 输出 Prom 文本格式（0.0.4）。
func (r *Registry) Render() string {
	var b strings.Builder
	r.mu.RLock()
	counters, gauges, hists := r.counters, r.gauges, r.hists
	r.mu.RUnlock()

	var keys []string
	for k := range counters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		c := counters[k]
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n", k, c.help, k)
		c.mu.Lock()
		for labels, v := range c.byLabels {
			writeSample(&b, k, labels, v)
		}
		c.mu.Unlock()
	}

	keys = keys[:0]
	for k := range gauges {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		g := gauges[k]
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", k, g.help, k)
		g.mu.Lock()
		for labels, v := range g.byLabels {
			writeSample(&b, k, labels, v)
		}
		fn := g.fn
		g.mu.Unlock()
		if fn != nil {
			writeSample(&b, k, "", fn())
		}
	}

	keys = keys[:0]
	for k := range hists {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h := hists[k]
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s histogram\n", k, h.help, k)
		h.mu.Lock()
		for labels, cs := range h.counts {
			var cum uint64
			for i, up := range h.buckets {
				cum = cs[i] // 桶即累计（Observe 时已按上界累计）
				fmt.Fprintf(&b, "%s_bucket{%s%s%v=\"%v\"} %d\n", k, labels, labelSep(labels), leName, formatFloat(up), cum)
			}
			fmt.Fprintf(&b, "%s_bucket{%s%s%v=\"+Inf\"} %d\n", k, labels, labelSep(labels), leName, h.totals[labels])
			fmt.Fprintf(&b, "%s_sum%s %v\n", k, brace(labels), formatFloat(h.sums[labels]))
			fmt.Fprintf(&b, "%s_count%s %d\n", k, brace(labels), h.totals[labels])
		}
		h.mu.Unlock()
	}
	return b.String()
}

const leName = "le"

func labelSep(labels string) string {
	if labels == "" {
		return ""
	}
	return ","
}

func brace(labels string) string {
	if labels == "" {
		return ""
	}
	return "{" + labels + "}"
}

func writeSample(b *strings.Builder, name, labels string, v float64) {
	if labels == "" {
		fmt.Fprintf(b, "%s %v\n", name, formatFloat(v))
		return
	}
	fmt.Fprintf(b, "%s{%s} %v\n", name, labels, formatFloat(v))
}

func formatFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case v == float64(int64(v)) && math.Abs(v) < 1e15:
		return fmt.Sprintf("%d", int64(v))
	default:
		return fmt.Sprintf("%g", v)
	}
}

// Handler /metrics 处理器。
func (r *Registry) Handler(c *framework.Context) {
	c.SetRespHeader("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	c.Status(200)
	_, _ = c.WriteString(r.Render())
}

// ---------------------------------------------------------------- 框架装配

// Middleware 常规指标中间件（http_requests_total / http_request_duration_seconds）。
func Middleware(r *Registry) framework.Middleware {
	total := r.Counter("http_requests_total", "Total HTTP requests")
	duration := r.Histogram("http_request_duration_seconds", "HTTP request latency", nil)
	r.Gauge("http_requests_inflight", "In-flight requests", nil)
	return func(c *framework.Context) {
		total.Add(1, fmt.Sprintf(`method=%q,route=%q`, c.Request.Method, c.Request.Path))
		start := time.Now()
		c.Next()
		duration.Observe(time.Since(start).Seconds(),
			fmt.Sprintf(`method=%q,route=%q,code=%q`, c.Request.Method, c.Request.Path, fmt.Sprint(statusOf(c))))
	}
}

func statusOf(c *framework.Context) int {
	if s := c.CurrentStatus(); s != 0 {
		return s
	}
	return 200
}
