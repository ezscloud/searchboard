// Package gzip 提供响应压缩中间件。
//
// 工作模式（自动选择）：
//  1. 流式管道（主路径，任意大小响应）：前置包装响应出口——Write 字节
//     实时流过 gzip.Writer，产物实时写出（chunked/流式）。累计达到
//     MinLength 才落头启用；此前保持 warmup 缓冲。
//  2. 小响应整体压缩：请求结束时仍在 warmup 且体在缓冲窗口内，整体
//     压缩定长替换（有收益才启用）。
//
// 跳过条件：显式 Content-Encoding、text/event-stream、multipart、
// 音视频/图片 Content-Type、Range 请求、HEAD。
package gzip

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"strings"

	"github.com/ezscloud/searchboard/framework"
)

// Config 压缩配置。
type Config struct {
	Level     int // 0-9；0/缺省=5
	MinLength int // 最小累计体长，默认 1024（低于走小响应路径）
}

func (c *Config) withDefaults() {
	if c.Level <= 0 || c.Level > 9 {
		c.Level = 5
	}
	if c.MinLength <= 0 {
		c.MinLength = 1024
	}
}

// Middleware gzip 响应压缩。
func Middleware(cfg Config) framework.Middleware {
	cfg.withDefaults()
	return func(c *framework.Context) {
		if !acceptsGzip(c.Header("Accept-Encoding")) || c.Header("Range") != "" {
			c.Next()
			return
		}
		gz := newGzipSink(c, cfg.Level, cfg.MinLength)
		c.WithSink(gz)
		c.Next()
		gz.finishRound()
	}
}

// acceptsGzip 客户端是否接受 gzip。
func acceptsGzip(ae string) bool {
	for _, part := range strings.Split(ae, ",") {
		enc, _, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(enc, "gzip") {
			return true
		}
	}
	return false
}

// skipContentType 该 Content-Type 是否跳过压缩。
// 空视为文本类（常见于流式 JSON），可压。
func skipContentType(ct string) bool {
	if ct == "" {
		return false
	}
	ct = strings.ToLower(ct)
	return strings.Contains(ct, "text/event-stream") ||
		strings.Contains(ct, "multipart/") ||
		strings.HasPrefix(ct, "video/") ||
		strings.HasPrefix(ct, "audio/") ||
		strings.HasPrefix(ct, "image/")
}

// compressBytes 整体压缩（小响应路径）。
func compressBytes(b []byte, level int) ([]byte, bool) {
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, level)
	if err != nil {
		return nil, false
	}
	if _, err := w.Write(b); err != nil {
		return nil, false
	}
	if err := w.Close(); err != nil {
		return nil, false
	}
	out := buf.Bytes()
	if len(out) >= len(b) {
		return nil, false
	}
	return out, true
}

// ---------------------------------------------------------------- 流式管道

// gzipSink 响应出口包装：Write 引入 gzip 实时压缩。
//
// 状态机：
//   - warmup（0）：缓冲写入；达到 MinLength 或被 Flush 时决策；
//   - active（1）：删 CL、设 Content-Encoding，历史缓冲与后续 Write 全部
//     流过压缩器，产物实时写出（任意大小响应）；
//   - passthrough（2）：命中跳过条件，透传原出口。
//
// 实现 framework 内部 responseSink 接口（方法全导出）。
type gzipSink struct {
	ctx        *framework.Context
	orig       framework.ResponseSinkHandle
	level, min int

	mode    int // 0 warmup / 1 active / 2 passthrough
	buf     []byte
	gz      *gzip.Writer
	decided bool
}

func newGzipSink(c *framework.Context, level, min int) *gzipSink {
	return &gzipSink{ctx: c, orig: c.CurrentSink(), level: level, min: min}
}

// Header 响应头（决策前可自由设置；透传原出口）。
func (g *gzipSink) Header() http.Header { return g.orig.Header() }

// WriteHeader 状态码（透传）。
func (g *gzipSink) WriteHeader(code int) { g.orig.WriteHeader(code) }

// Write 数据流入：按模式分流。
func (g *gzipSink) Write(p []byte) (int, error) {
	switch g.mode {
	case 2:
		return g.orig.Write(p)
	case 1:
		if _, err := g.gz.Write(p); err != nil {
			return 0, err
		}
		return len(p), nil
	default: // warmup
		g.buf = append(g.buf, p...)
		g.decide(false)
		return len(p), nil // 历史缓冲已在 decide 中分流
	}
}

// Flush 流式下推：先决策，再把压缩产物（或透传数据）推出。
func (g *gzipSink) Flush() {
	if !g.decided {
		g.decide(true)
	}
	if g.mode == 1 {
		_ = g.gz.Flush()
	}
	g.orig.Flush()
}

// Status / Size / Hijacked 状态查询透传。
func (g *gzipSink) Status() int    { return g.orig.Status() }
func (g *gzipSink) Size() int64    { return g.orig.Size() }
func (g *gzipSink) Hijacked() bool { return g.orig.Hijacked() }

// MarkHijacked 透传。
func (g *gzipSink) MarkHijacked() { g.orig.MarkHijacked() }

// Snapshot 仅 warmup 支持小响应快照路径。
func (g *gzipSink) Snapshot() []byte {
	if g.mode != 0 {
		return nil
	}
	return g.orig.Snapshot()
}

// Rewrite 仅 warmup 支持替换。
func (g *gzipSink) Rewrite(b []byte) bool {
	if g.mode != 0 {
		return false
	}
	return g.orig.Rewrite(b)
}

// Finish 收尾（异常路径兜底；正常由 finishRound 恢复后驱动原出口）。
func (g *gzipSink) Finish() error {
	if g.mode == 1 && g.gz != nil {
		_ = g.gz.Close()
	}
	return g.orig.Finish()
}

// decide warmup 决策（force=true 来自 Flush：立即定夺）。
func (g *gzipSink) decide(force bool) {
	if g.decided {
		return
	}
	if g.ctx.RespHeader().Get("Content-Encoding") != "" ||
		skipContentType(g.ctx.RespHeader().Get("Content-Type")) ||
		(g.ctx.Request != nil && g.ctx.Request.Method == http.MethodHead) {
		g.toPassthrough()
		g.decided = true
		return
	}
	if !force && len(g.buf) < g.min {
		return // 继续 warmup
	}
	// 激活流式压缩：删 CL（压缩后长度必然改变），头按 chunked 提交
	h := g.ctx.RespHeader()
	h.Del("Content-Length")
	setHeaders(g.ctx)
	g.mode = 1
	g.decided = true
	var out sinkWriter
	out.handle = g.orig
	g.gz, _ = gzip.NewWriterLevel(out, g.level)
	if len(g.buf) > 0 {
		_, _ = g.gz.Write(g.buf)
		g.buf = nil
	}
}

// toPassthrough 转透传：历史缓冲先刷给原出口。
func (g *gzipSink) toPassthrough() {
	g.mode = 2
	if len(g.buf) > 0 {
		_, _ = g.orig.Write(g.buf)
		g.buf = nil
	}
}

// finishRound 中间件收尾：压缩尾块 / 小响应路径处理 + 恢复原出口。
//
// active：关压缩器（尾块+trailer 已入原出口缓冲），随后服务器对原出口
// 的 finish 补终止帧/自动定长。
// warmup：小响应整体压缩（有收益才替换；顺序：先设 CE/Vary → Rewrite →
// 再设 CL——Rewrite 依赖"未定长"判定）。
func (g *gzipSink) finishRound() {
	switch g.mode {
	case 1:
		if g.gz != nil {
			_ = g.gz.Close()
		}
	case 0:
		// 小响应路径：warmup 缓冲（g.buf）尚未写原出口，此处决定压缩或透传。
		// 注意不能用 orig.Snapshot——数据在 g.buf，原出口缓冲是空的。
		if len(g.buf) > 0 {
			if out, ok := compressBytes(g.buf, g.level); ok {
				setHeaders(g.ctx)
				// 先删 handler 可能预设的 CL（压缩后长度改变），
				// orig.Write 走缓冲路径，Finish 时自动定长为压缩后尺寸
				g.ctx.RespHeader().Del("Content-Length")
				_, _ = g.orig.Write(out)
			} else {
				_, _ = g.orig.Write(g.buf) // 无压缩收益：原文透传
			}
			g.buf = nil
		}
	}
	g.ctx.RestoreSink(g.orig)
}

// setHeaders 压缩相关响应头。
func setHeaders(c *framework.Context) {
	c.SetRespHeader("Content-Encoding", "gzip")
	if v := c.RespHeader().Get("Vary"); v == "" {
		c.SetRespHeader("Vary", "Accept-Encoding")
	} else if !strings.Contains(v, "Accept-Encoding") {
		c.SetRespHeader("Vary", v+", Accept-Encoding")
	}
}

// sinkWriter 压缩产物直写响应（经原出口句柄）。
type sinkWriter struct{ handle framework.ResponseSinkHandle }

func (w sinkWriter) Write(p []byte) (int, error) { return w.handle.Write(p) }
