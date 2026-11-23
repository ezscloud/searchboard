// rpc/transport.go：JSON-RPC 的 HTTP 与 WebSocket 通道适配。
//
//	HTTP：POST 单条或批量数组，同步应答（通知 → 204）
//	WS  ：Upgrade 后逐条消息应答（通知不回），连接生命周期归业务
package rpc

import (
	"context"
	"net/http"

	"github.com/ezscloud/searchboard/framework"
	"github.com/ezscloud/searchboard/framework/ws"
)

// MaxRequestBytes 单条 HTTP 请求体上限。
const MaxRequestBytes = 32 << 20

// Handler 返回 framework HTTP 通道处理器（POST /rpc）。
func (s *Server) Handler() framework.HandlerFunc {
	return func(c *framework.Context) {
		if c.Request.Method != http.MethodPost {
			c.SetRespHeader("Allow", "POST")
			c.Status(http.StatusMethodNotAllowed)
			return
		}
		body, err := c.ReadBody(MaxRequestBytes)
		if err != nil {
			c.JSON(http.StatusBadRequest, map[string]any{"error": "read body failed"})
			return
		}
		if len(body) == 0 {
			c.JSON(http.StatusBadRequest, map[string]any{"error": "empty body"})
			return
		}
		resp := s.ServeMessage(c2ctx(c), body)
		if resp == nil {
			c.Status(http.StatusNoContent)
			return
		}
		c.SetRespHeader("Content-Type", "application/json; charset=utf-8")
		c.Status(http.StatusOK)
		_, _ = c.Write(resp)
	}
}

// WebSocket 返回 framework WS 通道处理器（GET 即升级，逐条消息 JSON-RPC）。
func (s *Server) WebSocket() framework.HandlerFunc {
	return func(c *framework.Context) {
		conn, err := ws.Upgrade(c, &ws.UpgradeOptions{ReadLimit: 8 << 20})
		if err != nil {
			return // Upgrade 已回写错误响应
		}
		s.serveWS(conn)
	}
}

// serveWS WS 消息循环（阻塞直至连接关闭）。
func (s *Server) serveWS(conn *ws.Conn) {
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			_ = conn.CloseNow()
			return
		}
		if len(data) == 0 {
			continue
		}
		if resp := s.ServeMessage(context.Background(), data); resp != nil {
			if err := conn.WriteMessage(ws.OpText, resp); err != nil {
				_ = conn.CloseNow()
				return
			}
		}
	}
}

// c2ctx 从 framework 上下文提取 context（当前无 request-scoped ctx，直接后台）。
func c2ctx(c *framework.Context) context.Context { return context.Background() }
