// Package trace 分布式链路追踪（OpenTracing 语义的数据模型 + 中间件）。
//
// 与 hertz-contrib/tracer（opentracing 接口）与 obs-opentelemetry 的关系：
// 本包提供零依赖的核心语义（TraceID/SpanID/父子关系/携带字段/W3C
// traceparent 透传），导出接口 Exporter 供接入任一后端（Jaeger/OTel）。
package trace

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ezscloud/searchboard/framework"
)

// ID 128 位追踪标识（32 hex 字符；0 = 无效）。
type ID [16]byte

// SpanID 64 位 span 标识（16 hex 字符）。
type SpanID [8]byte

// String 十六进制呈现。
func (t ID) String() string     { return hex.EncodeToString(t[:]) }
func (s SpanID) String() string { return hex.EncodeToString(s[:]) }

// IsValid 是否有效（非零）。
func (t ID) IsValid() bool     { return t != ID{} }
func (s SpanID) IsValid() bool { return s != SpanID{} }

// NewTraceID 生成新 TraceID。
func NewTraceID() ID {
	var t ID
	_, _ = rand.Read(t[:])
	t[6] = (t[6] & 0x0f) | 0x10 // 版本 01（traceparent 兼容）
	return t
}

// NewSpanID 生成新 SpanID。
func NewSpanID() SpanID {
	var s SpanID
	_, _ = rand.Read(s[:])
	return s
}

// ParseTraceID 解析 32 hex。
func ParseTraceID(s string) (ID, bool) {
	var t ID
	b, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != 16 {
		return t, false
	}
	copy(t[:], b)
	return t, t.IsValid()
}

// ParseSpanID 解析 16 hex。
func ParseSpanID(s string) (SpanID, bool) {
	var id SpanID
	b, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != 8 {
		return id, false
	}
	copy(id[:], b)
	return id, id.IsValid()
}

// Trace 一条追踪链的当前上下文。
type Trace struct {
	TraceID      ID
	SpanID       SpanID
	ParentSpanID SpanID
	Sampled      bool
}

// traceparentW3C 生成 W3C traceparent 头（版本 01）。
func (t Trace) traceparentW3C() string {
	flags := "00"
	if t.Sampled {
		flags = "01"
	}
	return "00-" + t.TraceID.String() + "-" + t.SpanID.String() + "-" + flags
}

// traceparent 解析 W3C traceparent。
func parseTraceparent(v string) (Trace, bool) {
	parts := strings.Split(v, "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 {
		return Trace{}, false
	}
	tid, ok1 := ParseTraceID(parts[1])
	sid, ok2 := ParseSpanID(parts[2])
	if !ok1 || !ok2 {
		return Trace{}, false
	}
	return Trace{TraceID: tid, SpanID: sid, Sampled: parts[3] == "01"}, true
}

// Span 单个 span 数据（导出单元）。
type Span struct {
	Trace
	Name     string
	Start    time.Time
	Duration time.Duration
	Attrs    map[string]string
	Events   []Event
}

// Event span 内时间点。
type Event struct {
	Name string
	At   time.Time
}

// Exporter 导出器接口（对接 Jaeger/OTel/日志 等后端）。
type Exporter interface {
	Export(sp *Span)
}

// LoggerExporter 打日志的导出器（默认）。
type LoggerExporter struct {
	Log func(format string, args ...any)
}

// Export implements Exporter。
func (e *LoggerExporter) Export(sp *Span) {
	if e.Log == nil {
		return
	}
	e.Log("span %s trace=%s span=%s dur=%s", sp.Name, sp.Trace.TraceID, sp.SpanID, sp.Duration)
}

// Tracer 追踪器。
type Tracer struct {
	mu       sync.RWMutex
	exporter Exporter
}

// NewTracer 创建追踪器（nil exporter=只透传不落盘）。
func NewTracer(ex Exporter) *Tracer { return &Tracer{exporter: ex} }

// SetExporter 更换导出器。
func (t *Tracer) SetExporter(ex Exporter) {
	t.mu.Lock()
	t.exporter = ex
	t.mu.Unlock()
}

// Key 上下文存取键。
const Key = "framework.trace"

// Middleware 注入追踪上下文：
//
//   - 入站 traceparent 存在 → 延续链（生成子 SpanID）；
//   - 不存在 → 发起新 Trace；
//   - 出站响应回写 traceparent（供下游继续串联）。
func Middleware(t *Tracer) framework.Middleware {
	return func(c *framework.Context) {
		tr := extract(c.Header("Traceparent"))
		if !tr.TraceID.IsValid() {
			tr = Trace{TraceID: NewTraceID(), SpanID: NewSpanID(), Sampled: true}
		} else {
			tr.ParentSpanID = tr.SpanID
			tr.SpanID = NewSpanID()
		}
		c.Set(Key, tr)
		c.SetRespHeader("Traceparent", tr.traceparentW3C())
		c.SetRespHeader("Tracestate", "webfw=1")

		start := time.Now()
		c.Next()

		if t != nil {
			t.mu.RLock()
			ex := t.exporter
			t.mu.RUnlock()
			if ex != nil {
				ex.Export(&Span{
					Trace: tr, Name: nameOf(c), Start: start,
					Duration: time.Since(start),
					Attrs:    attrsOf(c),
				})
			}
		}
	}
}

// From 取当前追踪上下文（无中间件为零值）。
func From(c *framework.Context) Trace {
	v, _ := c.Get(Key)
	tr, _ := v.(Trace)
	return tr
}

// Inject 给出站请求注入 traceparent（配合 framework.Client/反向代理）。
func Inject(req *http.Request, tr Trace) {
	if !tr.TraceID.IsValid() {
		return
	}
	if req == nil || req.Header == nil {
		return
	}
	req.Header.Set("Traceparent", tr.traceparentW3C())
}

// extract 从 traceparent 解析（含 X-Forwarded 兜底）。
func extract(v string) Trace {
	if tr, ok := parseTraceparent(v); ok {
		return tr
	}
	return Trace{}
}

func nameOf(c *framework.Context) string { return c.Request.Method + " " + c.Request.Path }

func attrsOf(c *framework.Context) map[string]string {
	return map[string]string{
		"http.method":      c.Request.Method,
		"http.path":        c.Request.Path,
		"http.status_code": itoaOf(c),
		"peer.addr":        c.ClientIP(),
	}
}

func itoaOf(c *framework.Context) string {
	s := c.CurrentStatus()
	if s == 0 {
		s = 200
	}
	return strconvItoa(s)
}
