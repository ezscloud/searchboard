package ws

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// TestPerMessageDeflateE2E 协商启用压缩后：大消息自动压缩传输、读侧自动解压。
func TestPerMessageDeflateE2E(t *testing.T) {
	srv := newTestServer(t, func(c *frameworkContext) {
		conn, err := Upgrade(c, &UpgradeOptions{PerMessageDeflate: true, ReadLimit: 8 << 20})
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
	}, "/ws/deflate")

	conn, err := Dial(context.Background(), "ws://"+srv.addr+"/ws/deflate",
		&DialOptions{PerMessageDeflate: true})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	if !conn.pmd.Enabled() {
		t.Fatalf("deflate should be negotiated")
	}
	if !srvHasDeflate(t, srv) {
		t.Fatalf("server side deflate missing")
	}

	// 高重复度大消息（压缩收益明显）
	big := []byte(strings.Repeat("webfw-deflate-roundtrip-", 4096)) // ~110KB
	if err := conn.WriteMessage(OpBinary, big); err != nil {
		t.Fatalf("write big: %v", err)
	}
	op, got, err := conn.ReadMessage()
	if err != nil || op != OpBinary {
		t.Fatalf("read big: op=%v err=%v", op, err)
	}
	if !bytes.Equal(got, big) {
		t.Fatalf("big echo mismatch: got %d bytes want %d", len(got), len(big))
	}

	// 文本
	txt := []byte(strings.Repeat("你好，世界 ", 300))
	if err := conn.WriteMessage(OpText, txt); err != nil {
		t.Fatalf("write text: %v", err)
	}
	_, got2, err := conn.ReadMessage()
	if err != nil || !bytes.Equal(got2, txt) {
		t.Fatalf("text echo: %d bytes err=%v", len(got2), err)
	}

	// 小消息（压缩无收益时直通路径）
	if err := conn.WriteMessage(OpText, []byte("hi")); err != nil {
		t.Fatalf("write small: %v", err)
	}
	_, got3, err := conn.ReadMessage()
	if err != nil || string(got3) != "hi" {
		t.Fatalf("small echo: %q err=%v", got3, err)
	}
}

// TestDeflateNotNegotiatedByDefault 默认不协商：行为与原路径一致。
func TestDeflateNotNegotiatedByDefault(t *testing.T) {
	srv := newTestServer(t, func(c *frameworkContext) {
		conn, err := Upgrade(c, nil) // PerMessageDeflate 未开
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			op, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			_ = conn.WriteMessage(op, data)
		}
	}, "/ws/plain")

	conn, err := Dial(context.Background(), "ws://"+srv.addr+"/ws/plain", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()
	if conn.pmd != nil && conn.pmd.Enabled() {
		t.Fatalf("deflate should not be enabled by default")
	}
	if err := conn.WriteMessage(OpText, []byte("plain")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, got, err := conn.ReadMessage()
	if err != nil || string(got) != "plain" {
		t.Fatalf("echo: %q err=%v", got, err)
	}
}

// TestDeflateRejectRSVWithoutNegotiation 未协商时 RSV1 帧 → 协议错误断连。
func TestDeflateRejectRSVWithoutNegotiation(t *testing.T) {
	srv := newTestServer(t, func(c *frameworkContext) {
		conn, err := Upgrade(c, nil) // 未协商 deflate
		if err != nil {
			return
		}
		conn.ReadMessage() // 读到 RSV1 帧应报 ErrProtocol
		conn.CloseNow()
	}, "/ws/rsrv")

	conn, err := Dial(context.Background(), "ws://"+srv.addr+"/ws/rsrv", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	// 直接注入 RSV1 帧（未协商）
	_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if err := writeFrameRSV1(conn.conn, true, OpText, []byte("x"), true, randomMaskKey()); err != nil {
		t.Fatalf("write rsv1: %v", err)
	}
	// 对端应断连：读返回错误
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, rerr := conn.ReadMessage()
	if rerr == nil {
		t.Fatalf("expected protocol error on un-negotiated RSV1")
	}
}

// srvHasDeflate 服务端是否协商了 deflate（读一条响应确认）。
func srvHasDeflate(t *testing.T, srv *testServer) bool {
	t.Helper()
	// 通过另一个协商连接检测服务端响应头
	conn, err := Dial(context.Background(), "ws://"+srv.addr+"/ws/deflate",
		&DialOptions{PerMessageDeflate: true})
	if err != nil {
		return false
	}
	defer conn.CloseNow()
	return conn.pmd != nil && conn.pmd.Enabled()
}
