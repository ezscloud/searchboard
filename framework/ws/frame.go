// Package ws 实现 RFC 6455 WebSocket：
//
//   - frame.go   帧编解码（FIN/opcode/掩码/7-16-64 位长度）
//   - conn.go    面向消息的连接（分片重组、控制帧、close 握手）
//   - upgrade.go 服务端升级（从 framework.Context 移交连接）
//   - client.go  客户端拨号（ws:// wss://，掩码自动开启）
package ws

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

// Opcode 帧操作码。
type Opcode byte

// 帧操作码常量（RFC 6455 §5.2）。
const (
	OpContinuation Opcode = 0x0
	OpText         Opcode = 0x1
	OpBinary       Opcode = 0x2
	OpClose        Opcode = 0x8
	OpPing         Opcode = 0x9
	OpPong         Opcode = 0xA
)

// String 友好名称。
func (op Opcode) String() string {
	switch op {
	case OpText:
		return "text"
	case OpBinary:
		return "binary"
	case OpClose:
		return "close"
	case OpPing:
		return "ping"
	case OpPong:
		return "pong"
	case OpContinuation:
		return "continuation"
	default:
		return fmt.Sprintf("op(%d)", byte(op))
	}
}

// 错误集合。
var (
	ErrProtocol      = errors.New("ws: protocol error")
	ErrReadLimit     = errors.New("ws: read limit exceeded")
	ErrConnClosed    = errors.New("ws: connection closed")
	ErrWriteClosed   = errors.New("ws: write after close")
	ErrControlTooBig = errors.New("ws: control frame payload > 125")
)

// CloseError 关闭握手携带的代码与原因。
type CloseError struct {
	Code   int
	Reason string
}

func (e *CloseError) Error() string {
	return fmt.Sprintf("ws: closed: code=%d reason=%q", e.Code, e.Reason)
}

// 常用关闭代码（RFC 6455 §7.4.1）。
const (
	CloseNormal           = 1000
	CloseGoingAway        = 1001
	CloseProtocolError    = 1002
	CloseUnsupported      = 1003
	CloseNoStatus         = 1005 // 仅内部使用，不在线上传输
	CloseAbnormal         = 1006 // 仅内部使用
	CloseInvalidPayload   = 1007
	ClosePolicyViolation  = 1008
	CloseMessageTooBig    = 1009
	CloseMandatoryExt     = 1010
	CloseInternalErr      = 1011
	CloseServiceRestart   = 1012
	CloseTryAgainLater    = 1013
	CloseBadGateway       = 1014
	CloseTLSHandshakeFail = 1015
)

// frameHeader 帧头。
type frameHeader struct {
	fin     bool
	rsv     byte
	opcode  Opcode
	masked  bool
	length  int64
	maskKey [4]byte
}

// readFrameHeader 逐字节解析帧头（2..14 字节）。
func readFrameHeader(r io.Reader) (frameHeader, error) {
	var b [2]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return frameHeader{}, err
	}
	h := frameHeader{
		fin:    b[0]&0x80 != 0,
		rsv:    b[0] & 0x70,
		opcode: Opcode(b[0] & 0x0f),
		masked: b[1]&0x80 != 0,
		length: int64(b[1] & 0x7f),
	}
	switch h.length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return frameHeader{}, err
		}
		h.length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return frameHeader{}, err
		}
		v := binary.BigEndian.Uint64(ext[:])
		if v > 1<<62 {
			return frameHeader{}, ErrProtocol
		}
		h.length = int64(v)
	}
	if h.masked {
		if _, err := io.ReadFull(r, h.maskKey[:]); err != nil {
			return frameHeader{}, err
		}
	}
	return h, nil
}

// applyMask 掩码异或（编码/解码同一函数）。
func (h frameHeader) applyMask(p []byte) {
	if !h.masked {
		return
	}
	k := h.maskKey
	for i := range p {
		p[i] ^= k[i&3]
	}
}

// writeFrame 写一帧；masked 时以 key 掩码载荷。
func writeFrame(w io.Writer, fin bool, op Opcode, payload []byte, masked bool, key [4]byte) error {
	head := make([]byte, 0, 14)
	b0 := byte(op)
	if fin {
		b0 |= 0x80
	}
	head = append(head, b0)
	n := len(payload)
	var b1 byte
	if masked {
		b1 |= 0x80
	}
	switch {
	case n <= 125:
		head = append(head, b1|byte(n))
	case n <= 0xffff:
		head = append(head, b1|126)
		var ext [2]byte
		binary.BigEndian.PutUint16(ext[:], uint16(n))
		head = append(head, ext[:]...)
	default:
		head = append(head, b1|127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		head = append(head, ext[:]...)
	}
	if masked {
		head = append(head, key[:]...)
	}

	if _, err := w.Write(head); err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	if masked {
		p := make([]byte, n)
		copy(p, payload)
		k := frameHeader{masked: true, maskKey: key}
		k.applyMask(p)
		_, err := w.Write(p)
		return err
	}
	_, err := w.Write(payload)
	return err
}

// randomMaskKey 生成客户端掩码键。
func randomMaskKey() [4]byte {
	var k [4]byte
	_, _ = rand.Read(k[:])
	return k
}

// writeFrameRSV1 写带 RSV1 的帧（permessage-deflate 压缩数据首帧标记）。
func writeFrameRSV1(w io.Writer, fin bool, op Opcode, payload []byte, masked bool, key [4]byte) error {
	return writeFrameOpt(w, fin, op, payload, masked, key, true)
}

// writeFrameOpt 帧写出（可带 RSV1）。
func writeFrameOpt(w io.Writer, fin bool, op Opcode, payload []byte, masked bool, key [4]byte, rsv1 bool) error {
	head := make([]byte, 0, 14)
	b0 := byte(op)
	if fin {
		b0 |= 0x80
	}
	if rsv1 {
		b0 |= 0x40
	}
	head = append(head, b0)
	n := len(payload)
	var b1 byte
	if masked {
		b1 |= 0x80
	}
	switch {
	case n <= 125:
		head = append(head, b1|byte(n))
	case n <= 0xffff:
		head = append(head, b1|126)
		var ext [2]byte
		binary.BigEndian.PutUint16(ext[:], uint16(n))
		head = append(head, ext[:]...)
	default:
		head = append(head, b1|127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		head = append(head, ext[:]...)
	}
	if masked {
		head = append(head, key[:]...)
	}
	if _, err := w.Write(head); err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	if masked {
		p := make([]byte, n)
		copy(p, payload)
		k := frameHeader{masked: true, maskKey: key}
		k.applyMask(p)
		_, err := w.Write(p)
		return err
	}
	_, err := w.Write(payload)
	return err
}

// Conn WebSocket 连接（面向消息，详细语义见 conn.go）。
type Conn struct {
	conn   net.Conn
	br     *bufio.Reader
	client bool // 客户端侧：发送帧必须掩码

	writeMu sync.Mutex

	// 读状态
	inMsg     bool
	msgOpcode Opcode
	msgBuf    []byte

	readLimit int64
	closeSent bool

	onPong func(string) error

	// permessage-deflate（RFC 7692）：协商成功后启用；nil=未启用
	pmd  *pmDeflate
	rsv1 bool // 当前消息首帧 RSV1（收齐后需解压）

	closedOnce sync.Once
	closeErr   error
}
