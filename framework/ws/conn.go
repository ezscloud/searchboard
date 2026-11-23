// conn.go：WebSocket 连接的读写语义。
//
//	ReadMessage：自动处理分片重组与控制帧（ping 自动回 pong、pong 回调、
//	            close 触发回执并返回 *CloseError）
//	WriteMessage：单帧发送（客户端自动掩码）；并发安全（内部互斥）
//	Close：优雅关闭握手（发送 close 帧 → 等待对端 close（≤2s）→ 关闭传输）
package ws

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"time"

	"github.com/ezscloud/searchboard/framework/jsonx"
)

// jsonMarshalW / jsonUnmarshalW：消息 JSON 编解码统一走 sonic。
func jsonMarshalW(v any) ([]byte, error)   { return jsonx.Marshal(v) }
func jsonUnmarshalW(b []byte, v any) error { return jsonx.Unmarshal(b, v) }

const defaultReadLimit = 4 << 20 // 单条消息上限 4MB

// newConn 构造连接。
func newConn(c net.Conn, br *bufio.Reader, client bool) *Conn {
	return &Conn{conn: c, br: br, client: client, readLimit: defaultReadLimit}
}

// SetReadLimit 设置单条消息最大字节数（超限连接将以 1009 关闭）。
func (c *Conn) SetReadLimit(n int64) { c.readLimit = n }

// SetPongHandler 设置 pong 回调（心跳对端存活检测用）。
func (c *Conn) SetPongHandler(h func(string) error) { c.onPong = h }

// SetReadDeadline / SetWriteDeadline / SetDeadline 透传底层连接。
func (c *Conn) SetReadDeadline(t time.Time) error  { return c.conn.SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }
func (c *Conn) SetDeadline(t time.Time) error      { return c.conn.SetDeadline(t) }

// RemoteAddr / LocalAddr 底层连接地址。
func (c *Conn) RemoteAddr() net.Addr { return c.conn.RemoteAddr() }
func (c *Conn) LocalAddr() net.Addr  { return c.conn.LocalAddr() }

// IsClient 本端是否为客户端（发送掩码）。
func (c *Conn) IsClient() bool { return c.client }

// ---------------------------------------------------------------- 读

// ReadMessage 读取下一条完整消息（自动重组分片、处理控制帧）。
//
//	返回值：消息类型（OpText/OpBinary）、载荷字节、错误。
//	对端发起关闭时返回 *CloseError（且自动回执 close）。
func (c *Conn) ReadMessage() (Opcode, []byte, error) {
	for {
		h, err := readFrameHeader(c.br)
		if err != nil {
			return 0, nil, err
		}
		// RSV1 仅允许出现在数据首帧且仅在协商了 permessage-deflate 时
		if h.rsv != 0 {
			if h.rsv != 0x40 || !c.pmd.Enabled() {
				return 0, nil, ErrProtocol
			}
		}
		c.rsv1 = h.rsv == 0x40 && c.pmd.Enabled()
		switch h.opcode {
		case OpClose, OpPing, OpPong:
			if h.rsv != 0 { // 控制帧不得置 RSV
				return 0, nil, ErrProtocol
			}
			// 控制帧：载荷 ≤125 且不可分片
			if h.length > 125 || !h.fin {
				return 0, nil, ErrProtocol
			}
			payload := make([]byte, h.length)
			if h.length > 0 {
				if _, err := io.ReadFull(c.br, payload); err != nil {
					return 0, nil, err
				}
				h.applyMask(payload)
			}
			if err := c.handleControl(h.opcode, payload); err != nil {
				return 0, nil, err
			}
			continue
		case OpText, OpBinary:
			if c.inMsg { // 上一条未完结，不能开新消息
				return 0, nil, ErrProtocol
			}
			c.inMsg = true
			c.msgOpcode = h.opcode
			c.msgBuf = c.msgBuf[:0]
		case OpContinuation:
			if !c.inMsg {
				return 0, nil, ErrProtocol
			}
		default:
			return 0, nil, ErrProtocol
		}

		if err := c.readPayload(h); err != nil {
			return 0, nil, err
		}
		if h.fin {
			op, data := c.msgOpcode, c.msgBuf
			c.inMsg = false
			c.msgBuf = nil
			if c.rsv1 { // permessage-deflate：整消息解压
				c.rsv1 = false
				data, err = c.pmd.Decompress(data)
				if err != nil {
					return 0, nil, err
				}
			}
			return op, data, nil
		}
	}
}

// readPayload 读取一帧数据载荷并合并进消息缓冲。
func (c *Conn) readPayload(h frameHeader) error {
	if h.length == 0 {
		return nil
	}
	if int64(len(c.msgBuf))+h.length > c.readLimit {
		c.CloseNow()
		_ = c.WriteControl(OpClose, closePayload(CloseMessageTooBig, "read limit exceeded"), time.Now().Add(2*time.Second))
		return ErrReadLimit
	}
	start := len(c.msgBuf)
	c.msgBuf = append(c.msgBuf, make([]byte, h.length)...)
	if _, err := io.ReadFull(c.br, c.msgBuf[start:]); err != nil {
		return err
	}
	h.applyMask(c.msgBuf[start:])
	return nil
}

// handleControl 处理控制帧；close 返回错误终结 ReadMessage。
func (c *Conn) handleControl(op Opcode, payload []byte) error {
	switch op {
	case OpPing:
		if c.closeSent {
			return nil
		}
		// 自动回 pong（尽力而为）
		_ = c.WriteControl(OpPong, payload, time.Now().Add(5*time.Second))
		return nil
	case OpPong:
		if c.onPong != nil {
			if err := c.onPong(string(payload)); err != nil {
				return err
			}
		}
		return nil
	case OpClose:
		code, reason := parseClosePayload(payload)
		if !c.closeSent {
			_ = c.WriteControl(OpClose, closePayload(code, ""), time.Now().Add(2*time.Second))
			c.closeSent = true
		}
		_ = c.CloseNow()
		return &CloseError{Code: code, Reason: reason}
	default:
		return ErrProtocol
	}
}

// ---------------------------------------------------------------- 写

// WriteMessage 发送一条消息（单帧；客户端自动掩码；并发安全）。
// permessage-deflate 协商启用时自动压缩（RSV1 标记；载荷压缩后置）。
func (c *Conn) WriteMessage(op Opcode, data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closeSent {
		return ErrWriteClosed
	}
	if c.pmd.Enabled() && len(data) > 0 && (op == OpText || op == OpBinary) {
		z, err := c.pmd.Compress(data)
		if err == nil && len(z) < len(data) {
			// 压缩有收益才启用（小消息压缩可能变大）
			key := [4]byte{}
			masked := false
			if c.client {
				key = randomMaskKey()
				masked = true
			}
			return writeFrameRSV1(c.conn, true, op, z, masked, key)
		}
	}
	key := [4]byte{}
	masked := false
	if c.client {
		key = randomMaskKey()
		masked = true
	}
	return writeFrame(c.conn, true, op, data, masked, key)
}

// WriteJSON 以 sonic 序列化并发送文本消息。
func (c *Conn) WriteJSON(v any) error {
	b, err := jsonMarshalW(v)
	if err != nil {
		return err
	}
	return c.WriteMessage(OpText, b)
}

// ReadJSON 读取下一条消息并以 sonic 反序列化。
func (c *Conn) ReadJSON(v any) error {
	_, data, err := c.ReadMessage()
	if err != nil {
		return err
	}
	return jsonUnmarshalW(data, v)
}

// WriteControl 发送控制帧（close/ping/pong；载荷 ≤125；带超时）。
func (c *Conn) WriteControl(op Opcode, data []byte, deadline time.Time) error {
	if len(data) > 125 {
		return ErrControlTooBig
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if !deadline.IsZero() {
		_ = c.conn.SetWriteDeadline(deadline)
		defer c.conn.SetWriteDeadline(time.Time{})
	}
	key := [4]byte{}
	masked := false
	if c.client {
		key = randomMaskKey()
		masked = true
	}
	return writeFrame(c.conn, true, op, data, masked, key)
}

// Ping 发送心跳（携带鉴别数据）。
func (c *Conn) Ping(data []byte) error {
	return c.WriteControl(OpPing, data, time.Now().Add(5*time.Second))
}

// ---------------------------------------------------------------- 关闭

// Close 优雅关闭：发送 close 帧 → 等对端回执（≤2s）→ 关闭传输。
// 多次调用幂等。
func (c *Conn) Close(code int, reason string) error {
	c.closedOnce.Do(func() {
		c.writeMu.Lock()
		if !c.closeSent {
			c.closeSent = true
			key := [4]byte{}
			masked := false
			if c.client {
				key = randomMaskKey()
				masked = true
			}
			_ = c.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			c.closeErr = writeFrame(c.conn, true, OpClose, closePayload(code, reason), masked, key)
		}
		c.writeMu.Unlock()
		// 等待对端 close（尽力）
		_ = c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		for {
			h, err := readFrameHeader(c.br)
			if err != nil {
				break
			}
			if h.length > 0 {
				_, e := io.CopyN(io.Discard, c.br, h.length)
				if e != nil {
					break
				}
			}
			if h.opcode == OpClose {
				break
			}
		}
		_ = c.conn.Close()
	})
	return c.closeErr
}

// CloseNow 直接关闭底层传输（不走握手）。
func (c *Conn) CloseNow() error {
	c.closedOnce.Do(func() {
		c.closeErr = c.conn.Close()
	})
	return c.closeErr
}

// parseClosePayload 解析 close 帧载荷（2 字节代码 + 原因）。
func parseClosePayload(p []byte) (code int, reason string) {
	if len(p) >= 2 {
		code = int(binary.BigEndian.Uint16(p[:2]))
		reason = string(p[2:])
	} else {
		code = CloseNoStatus
	}
	return code, reason
}

// closePayload 组装 close 帧载荷。
func closePayload(code int, reason string) []byte {
	if len(reason) > 123 {
		reason = reason[:123]
	}
	p := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(p[:2], uint16(code))
	copy(p[2:], reason)
	return p
}

// isCloseError 判定错误是否为关闭类（读循环友好退出）。
func isCloseError(err error) bool {
	var ce *CloseError
	return errors.As(err, &ce) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}
