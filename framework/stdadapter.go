// stdadapter.go：把 framework 的路由以标准 http.Handler 暴露。
//
// 用途：HTTP/3-QUIC（webfw/h3）复用同一套路由处理器——quic-go 的
// http3.Server 只接受 http.Handler，本适配器在 net/http 请求与
// framework.Context 之间做双向转换：
//
//	http.Request ──requestFromStd──> *Request ──Router.Dispatch──> 处理器
//	                             <──stdSink──  http.ResponseWriter
//
// 该链路不支持 Hijack（WS over H3 属 WebTransport 范畴，v1 不支持）。
package framework

import (
	"net/http"
)

// stdSink 标准库 http.ResponseWriter 出口（H3 链路）。
type stdSink struct {
	w           http.ResponseWriter
	status      int
	size        int64
	wroteHeader bool // 头已提交给底层 ResponseWriter
	hijacked    bool

	buf      []byte // 未提交体缓冲（未知长度窗口 ≤2KB）
	streamed bool   // 已 flush（进入流式，不再缓冲）
}

func (k *stdSink) Header() http.Header { return k.w.Header() }

func (k *stdSink) WriteHeader(code int) {
	if k.wroteHeader {
		return
	}
	if code < 100 || code > 599 {
		code = http.StatusInternalServerError
	}
	k.status = code // 仅记录：真正提交在 commit()
}

// commit 把头（与可选缓冲体）提交给底层 ResponseWriter。
func (k *stdSink) commit() {
	if k.wroteHeader {
		return
	}
	k.wroteHeader = true
	if k.status == 0 {
		k.status = http.StatusOK
	}
	k.w.WriteHeader(k.status)
}

func (k *stdSink) Write(p []byte) (int, error) {
	if k.streamed {
		k.commit()
		n, err := k.w.Write(p)
		k.size += int64(n)
		return n, err
	}
	if len(k.buf)+len(p) > maxBuffered {
		// 超出缓冲窗口：提交后直写
		k.commit()
		if len(k.buf) > 0 {
			if _, err := k.w.Write(k.buf); err != nil {
				return 0, err
			}
			k.buf = k.buf[:0]
		}
		n, err := k.w.Write(p)
		k.size += int64(n)
		return n, err
	}
	k.buf = append(k.buf, p...)
	k.size += int64(len(p))
	return len(p), nil
}

func (k *stdSink) Flush() {
	k.streamed = true
	if len(k.buf) > 0 {
		k.commit()
		_, _ = k.w.Write(k.buf)
		k.buf = k.buf[:0]
	} else {
		k.commit()
	}
	if f, ok := k.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (k *stdSink) Status() int {
	if k.status == 0 {
		return http.StatusOK
	}
	return k.status
}

func (k *stdSink) Size() int64    { return k.size }
func (k *stdSink) Hijacked() bool { return k.hijacked }
func (k *stdSink) MarkHijacked()  { k.hijacked = true }

// Snapshot 未提交体的快照（gzip/etag 后置改写用）。
func (k *stdSink) Snapshot() []byte {
	if k.wroteHeader || k.streamed {
		return nil
	}
	return k.buf
}

// Rewrite 缓冲窗口内替换响应体。
func (k *stdSink) Rewrite(b []byte) bool {
	if k.Snapshot() == nil {
		return false
	}
	k.size = int64(len(b))
	k.buf = append(k.buf[:0], b...)
	return true
}

func (k *stdSink) Finish() error {
	if len(k.buf) > 0 {
		k.commit()
		_, err := k.w.Write(k.buf)
		k.buf = k.buf[:0]
		return err
	}
	k.commit()
	return nil
}

// StdHandler 将服务端路由暴露为标准 http.Handler。
func (s *Server) StdHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sink := &stdSink{w: w}
		req := requestFromStd(r)
		ctx := NewContext(req, sink)
		s.router.Dispatch(ctx)
		_ = sink.Finish()
	})
}
