// client.go：WebSocket 客户端拨号。
//
// ws://  → transport.Dial（unix 系平台为 netpoll）
// wss:// → 同上 + tls.Client 握手
// 发送帧自动掩码（RFC 6455 客户端→服务端强制）。
package ws

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ezscloud/searchboard/framework/transport"
)

// DialOptions 拨号选项。
type DialOptions struct {
	// Subprotocols 期望的子协议（写入 Sec-WebSocket-Protocol，空格分隔）。
	Subprotocols []string

	// Header 附加请求头。
	Header http.Header

	// TLSClientConfig wss TLS 配置。
	TLSClientConfig *tls.Config

	// HandshakeTimeout 握手总时限（默认 10s）。
	HandshakeTimeout time.Duration

	// ReadLimit 单条消息上限（默认 4MB）。
	ReadLimit int64

	// PerMessageDeflate 请求 permessage-deflate 扩展（RFC 7692）：
	// 服务端在 101 响应中接受该扩展时启用消息压缩。
	PerMessageDeflate bool
}

// Dial 拨号并完成握手，返回已就绪的连接。
func Dial(ctx context.Context, rawurl string, opts *DialOptions) (*Conn, error) {
	if opts == nil {
		opts = &DialOptions{}
	}
	timeout := opts.HandshakeTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	u, err := url.Parse(rawurl)
	if err != nil {
		return nil, err
	}
	var secure bool
	switch u.Scheme {
	case "ws":
	case "wss":
		secure = true
	default:
		return nil, fmt.Errorf("ws: bad scheme %q (want ws/wss)", u.Scheme)
	}
	hostPort := u.Host
	if u.Port() == "" {
		if secure {
			hostPort = net.JoinHostPort(u.Hostname(), "443")
		} else {
			hostPort = net.JoinHostPort(u.Hostname(), "80")
		}
	}
	rest := time.Until(deadline)
	if rest <= 0 {
		return nil, context.DeadlineExceeded
	}
	conn, err := transport.Dial("tcp", hostPort, rest)
	if err != nil {
		return nil, err
	}
	if secure {
		tlsCfg := opts.TLSClientConfig
		if tlsCfg == nil {
			tlsCfg = &tls.Config{ServerName: u.Hostname()}
		}
		tconn := tls.Client(conn, tlsCfg)
		_ = tconn.SetDeadline(deadline)
		if err := tconn.Handshake(); err != nil {
			_ = conn.Close()
			return nil, err
		}
		conn = tconn
	}

	c, err := handshake(conn, u, opts, deadline)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return c, nil
}

// handshake 发送升级请求并校验 101 响应。
func handshake(conn net.Conn, u *url.URL, opts *DialOptions, deadline time.Time) (*Conn, error) {
	_ = conn.SetDeadline(deadline)
	defer conn.SetDeadline(time.Time{})

	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)

	target := u.RequestURI()
	if target == "" {
		target = "/"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\n", target)
	fmt.Fprintf(&b, "Host: %s\r\n", u.Host)
	b.WriteString("Upgrade: websocket\r\n")
	b.WriteString("Connection: Upgrade\r\n")
	fmt.Fprintf(&b, "Sec-WebSocket-Key: %s\r\n", key)
	b.WriteString("Sec-WebSocket-Version: 13\r\n")
	if opts.PerMessageDeflate {
		// 客户端同时声明 no_context_takeover（与本实现语义一致）
		b.WriteString("Sec-WebSocket-Extensions: permessage-deflate; client_no_context_takeover\r\n")
	}
	if len(opts.Subprotocols) > 0 {
		fmt.Fprintf(&b, "Sec-WebSocket-Protocol: %s\r\n", strings.Join(opts.Subprotocols, ", "))
	}
	for k, vs := range opts.Header {
		switch strings.ToLower(k) {
		case "host", "upgrade", "connection", "sec-websocket-key", "sec-websocket-version", "sec-websocket-protocol":
			continue
		}
		for _, v := range vs {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	b.WriteString("\r\n")

	if _, err := conn.Write([]byte(b.String())); err != nil {
		return nil, err
	}

	// 解析响应
	br := bufio.NewReaderSize(conn, 4096)
	line, err := readRawLine(br)
	if err != nil {
		return nil, err
	}
	proto, code, _, ok := splitRespStatus(string(line))
	if !ok {
		return nil, ErrProtocol
	}
	_ = proto
	if code != http.StatusSwitchingProtocols {
		return nil, fmt.Errorf("ws: handshake status %d (want 101)", code)
	}

	hdr := make(http.Header)
	for {
		row, err := readRawLine(br)
		if err != nil {
			return nil, err
		}
		if len(row) == 0 {
			break
		}
		i := strings.IndexByte(string(row), ':')
		if i <= 0 {
			continue
		}
		hdr.Add(strings.TrimSpace(string(row[:i])), strings.TrimSpace(string(row[i+1:])))
	}

	// 校验 Accept
	got := hdr.Get("Sec-WebSocket-Accept")
	if got != acceptOf(key) {
		return nil, errors.New("ws: Sec-WebSocket-Accept mismatch")
	}

	// 服务端接受 permessage-deflate 时启用
	useDeflate := false
	if opts.PerMessageDeflate {
		for _, ext := range headerTokensOf(hdr.Values("Sec-WebSocket-Extensions")) {
			if strings.HasPrefix(strings.ToLower(ext), "permessage-deflate") {
				useDeflate = true
				break
			}
		}
	}

	c := newConn(conn, br, true)
	if opts.ReadLimit > 0 {
		c.SetReadLimit(opts.ReadLimit)
	}
	if useDeflate {
		c.pmd = newPMDeflate(true)
	}
	return c, nil
}

// acceptOf 计算握手应答值。
func acceptOf(key string) string {
	h := sha1.New()
	h.Write([]byte(key))
	h.Write([]byte(websocketGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// readRawLine 读取单行（含 \n，去掉 \r）。
func readRawLine(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		if err == io.EOF && line != "" {
			return []byte(strings.TrimRight(line, "\r\n")), nil
		}
		return nil, err
	}
	return []byte(strings.TrimRight(line, "\r\n")), nil
}

func splitRespStatus(line string) (proto string, code int, text string, ok bool) {
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "HTTP/1.") {
		return "", 0, "", false
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil || n < 100 || n > 599 {
		return "", 0, "", false
	}
	if len(parts) == 3 {
		text = parts[2]
	}
	return parts[0], n, text, true
}
