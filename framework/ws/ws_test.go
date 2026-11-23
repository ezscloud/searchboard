package ws

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

// IsControl 是否控制帧操作码（测试辅助）。
func (op Opcode) IsControl() bool { return op&0x8 != 0 }

// ---------------------------------------------------------------- 帧编解码单元

func TestFrameRoundtrip(t *testing.T) {
	// 客户端帧（带掩码，长度跨 7/16/64 位三档）
	for _, n := range []int{0, 5, 125, 126, 1000, 70000} {
		payload := make([]byte, n)
		for i := range payload {
			payload[i] = byte(i % 251)
		}
		key := [4]byte{0x12, 0x34, 0x56, 0x78}
		var buf bytes.Buffer
		if err := writeFrame(&buf, true, OpBinary, payload, true, key); err != nil {
			t.Fatalf("writeFrame(%d): %v", n, err)
		}
		br := bufio.NewReader(&buf)
		h, err := readFrameHeader(br)
		if err != nil {
			t.Fatalf("readFrameHeader(%d): %v", n, err)
		}
		if !h.fin || h.opcode != OpBinary || !h.masked || h.length != int64(n) {
			t.Fatalf("header mismatch n=%d: %+v", n, h)
		}
		got := make([]byte, n)
		if _, err := io.ReadFull(br, got); err != nil {
			t.Fatalf("read payload(%d): %v", n, err)
		}
		h.applyMask(got)
		if !bytes.Equal(got, payload) {
			t.Fatalf("payload mismatch n=%d", n)
		}
	}
}

func TestFrameControlLimits(t *testing.T) {
	// WriteControl 应拒绝 >125 的控制帧载荷
	c := newConn(nil, nil, false)
	if err := c.WriteControl(OpPing, make([]byte, 200), time.Time{}); err != ErrControlTooBig {
		t.Fatalf("oversized control payload should be rejected, got %v", err)
	}
}

// ---------------------------------------------------------------- e2e

func TestWSEchoE2E(t *testing.T) {
	srv := newTestServer(t, func(c *frameworkContext) {
		conn, err := Upgrade(c, &UpgradeOptions{ReadLimit: 8 << 20})
		if err != nil {
			return
		}
		defer conn.Close(CloseNormal, "bye")
		for {
			op, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(op, data); err != nil {
				return
			}
		}
	}, "/ws/echo")

	conn, err := Dial(context.Background(), "ws://"+srv.addr+"/ws/echo", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	// 文本/二进制
	if err := conn.WriteMessage(OpText, []byte("你好，webfw")); err != nil {
		t.Fatalf("write text: %v", err)
	}
	op, data, err := conn.ReadMessage()
	if err != nil || op != OpText || string(data) != "你好，webfw" {
		t.Fatalf("echo text: op=%v data=%q err=%v", op, data, err)
	}

	// 大消息（16 位长度档）
	big := bytes.Repeat([]byte{0xAB}, 300*1024)
	if err := conn.WriteMessage(OpBinary, big); err != nil {
		t.Fatalf("write big: %v", err)
	}
	op, data, err = conn.ReadMessage()
	if err != nil || op != OpBinary || len(data) != len(big) || data[0] != 0xAB || data[len(data)-1] != 0xAB {
		t.Fatalf("echo big: op=%v n=%d err=%v", op, len(data), err)
	}

	// ping → 自动 pong：发送 ping 后紧跟一条文本消息，
	// ReadMessage 内部会先消化 pong 帧并触发 handler，再返回该文本回声。
	pongCh := make(chan string, 1)
	conn.SetPongHandler(func(s string) error { pongCh <- s; return nil })
	if err := conn.Ping([]byte("hb1")); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := conn.WriteMessage(OpText, []byte("after-ping")); err != nil {
		t.Fatalf("write after ping: %v", err)
	}
	op, data, err = conn.ReadMessage()
	if err != nil || string(data) != "after-ping" {
		t.Fatalf("echo after ping: op=%v data=%q err=%v", op, data, err)
	}
	select {
	case got := <-pongCh:
		if got != "hb1" {
			t.Fatalf("pong data = %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("no pong received")
	}

	// JSON 帧消息（sonic 编解码）
	type payload struct {
		N int `json:"n"`
	}
	if err := conn.WriteJSON(payload{N: 42}); err != nil {
		t.Fatalf("write json: %v", err)
	}
	var got payload
	if err := conn.ReadJSON(&got); err != nil {
		t.Fatalf("read json: %v", err)
	}
	if got.N != 42 {
		t.Fatalf("json echo = %+v", got)
	}

	// 优雅关闭握手
	if err := conn.Close(CloseNormal, "client bye"); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestWSConcurrentEcho(t *testing.T) {
	srv := newTestServer(t, func(c *frameworkContext) {
		conn, err := Upgrade(c, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			op, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(op, data); err != nil {
				return
			}
		}
	}, "/ws/echo")

	const clients = 8
	const rounds = 20
	var wg sync.WaitGroup
	errCh := make(chan error, clients)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			conn, err := Dial(context.Background(), "ws://"+srv.addr+"/ws/echo", nil)
			if err != nil {
				errCh <- err
				return
			}
			defer conn.CloseNow()
			for r := 0; r < rounds; r++ {
				msg := []byte{byte(id), byte(r)}
				if err := conn.WriteMessage(OpBinary, msg); err != nil {
					errCh <- err
					return
				}
				_, data, err := conn.ReadMessage()
				if err != nil {
					errCh <- err
					return
				}
				if len(data) != 2 || data[0] != byte(id) || data[1] != byte(r) {
					errCh <- io.ErrUnexpectedEOF
					return
				}
			}
		}(i)
	}
	wg.Wait()
	select {
	case err := <-errCh:
		t.Fatalf("concurrent echo failed: %v", err)
	default:
	}
}

func TestUpgradeRejections(t *testing.T) {
	srv := newTestServer(t, func(c *frameworkContext) {
		if _, err := Upgrade(c, nil); err != nil {
			return
		}
	}, "/ws/any")

	// 非 websocket 请求：普通 GET → 400
	cli := &http.Client{Timeout: 3 * time.Second}
	resp, err := cli.Get("http://" + srv.addr + "/ws/any")
	if err != nil {
		t.Fatalf("plain get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("plain GET should be rejected with 400, got %d", resp.StatusCode)
	}
}

func TestClosePayloadCodec(t *testing.T) {
	p := closePayload(1000, "so long")
	code, reason := parseClosePayload(p)
	if code != 1000 || reason != "so long" {
		t.Fatalf("close payload roundtrip: %d %q", code, reason)
	}
	if _, r := parseClosePayload(nil); r != "" {
		t.Fatalf("empty close payload")
	}
	// 大端序校验
	if binary.BigEndian.Uint16(p[:2]) != 1000 {
		t.Fatalf("close code should be big-endian")
	}
}
