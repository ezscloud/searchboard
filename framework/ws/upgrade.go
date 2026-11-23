// upgrade.go：服务端 WebSocket 升级（从 framework.Context 移交连接）。
//
// 流程：校验握手头 → 计算 Sec-WebSocket-Accept → Hijack 拿到原始连接
// （含头部解析后的预读残留）→ 直写 101 响应 → 构造 ws.Conn。
// 处理器返回后连接所有权归业务（webfw 服务端不再触碰该连接）。
package ws

import (
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ezscloud/searchboard/framework"
)

// websocketGUID RFC 6455 握手魔串。
const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// UpgradeOptions 升级选项。
type UpgradeOptions struct {
	// Subprotocols 服务端支持的子协议列表（首个匹配客户端声明者生效）。
	Subprotocols []string

	// ReadLimit 单条消息上限（默认 4MB）。
	ReadLimit int64

	// HandshakeTimeout 写出 101 的写超时（默认 3s）。
	HandshakeTimeout time.Duration

	// PerMessageDeflate 启用 permessage-deflate 协商（RFC 7692）：
	// 客户端请求该扩展且回应接受后，消息自动压缩/解压。
	PerMessageDeflate bool
}

// Upgrade 在处理器内升级当前请求为 WebSocket 连接。
//
// 失败时已按 HTTP 语义回写错误响应（400/426），处理器应直接 return。
func Upgrade(ctx *framework.Context, opts *UpgradeOptions) (*Conn, error) {
	if opts == nil {
		opts = &UpgradeOptions{}
	}
	req := ctx.Request
	if ctx.RawConn() == nil {
		return nil, errors.New("ws: upgrade requires transport connection (unsupported on h3 chain)")
	}
	if req.Method != http.MethodGet {
		ctx.Error(http.StatusBadRequest, "websocket upgrade requires GET")
		return nil, errors.New("ws: not GET")
	}
	if req.Proto == "HTTP/1.0" {
		ctx.Error(http.StatusBadRequest, "websocket requires HTTP/1.1")
		return nil, errors.New("ws: http/1.0")
	}
	if !headerContainsToken(req.Header.Get("Upgrade"), "websocket") {
		ctx.Error(http.StatusBadRequest, "missing Upgrade: websocket")
		return nil, errors.New("ws: missing upgrade header")
	}
	if !headerContainsToken(req.Header.Get("Connection"), "upgrade") {
		ctx.Error(http.StatusBadRequest, "missing Connection: Upgrade")
		return nil, errors.New("ws: missing connection header")
	}
	key := strings.TrimSpace(req.Header.Get("Sec-WebSocket-Key"))
	if decoded, err := base64.StdEncoding.DecodeString(key); err != nil || len(decoded) != 16 {
		ctx.Error(http.StatusBadRequest, "invalid Sec-WebSocket-Key")
		return nil, errors.New("ws: bad key")
	}
	if v := req.Header.Get("Sec-WebSocket-Version"); v != "13" {
		ctx.SetRespHeader("Sec-WebSocket-Version", "13")
		ctx.Status(http.StatusUpgradeRequired)
		ctx.String(426, "unsupported websocket version")
		return nil, errors.New("ws: bad version")
	}

	// 子协议协商
	var proto string
	if want := headerTokensOf(req.Header.Values("Sec-WebSocket-Protocol")); len(want) > 0 && len(opts.Subprotocols) > 0 {
		for _, w := range want {
			for _, s := range opts.Subprotocols {
				if s == w {
					proto = s
					break
				}
			}
			if proto != "" {
				break
			}
		}
	}

	// permessage-deflate 协商：客户端请求且服务端开启才启用
	useDeflate := false
	if opts.PerMessageDeflate {
		for _, ext := range headerTokensOf(req.Header.Values("Sec-WebSocket-Extensions")) {
			if strings.HasPrefix(strings.ToLower(ext), "permessage-deflate") {
				useDeflate = true
				break
			}
		}
	}

	hij, err := ctx.Hijack()
	if err != nil {
		return nil, err
	}

	// 计算 Accept
	h := sha1.New()
	h.Write([]byte(key))
	h.Write([]byte(websocketGUID))
	accept := base64.StdEncoding.EncodeToString(h.Sum(nil))

	var b strings.Builder
	b.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	b.WriteString("Upgrade: websocket\r\n")
	b.WriteString("Connection: Upgrade\r\n")
	b.WriteString("Sec-WebSocket-Accept: " + accept + "\r\n")
	if proto != "" {
		b.WriteString("Sec-WebSocket-Protocol: " + proto + "\r\n")
	}
	if useDeflate {
		// no_context_takeover：每消息重置滑动窗口（见 permessage_deflate.go）
		b.WriteString("Sec-WebSocket-Extensions: permessage-deflate; server_no_context_takeover\r\n")
	}
	b.WriteString("\r\n")

	timeout := opts.HandshakeTimeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	_ = hij.Conn.SetWriteDeadline(time.Now().Add(timeout))
	if _, err := hij.Conn.Write([]byte(b.String())); err != nil {
		_ = hij.Conn.Close()
		return nil, err
	}
	_ = hij.Conn.SetWriteDeadline(time.Time{})
	_ = hij.Conn.SetReadDeadline(time.Time{})

	conn := newConn(hij.Conn, hij.Reader, false)
	if opts.ReadLimit > 0 {
		conn.SetReadLimit(opts.ReadLimit)
	}
	if useDeflate {
		conn.pmd = newPMDeflate(true)
	}
	return conn, nil
}

// headerContainsToken 大小写不敏感地判断头值列表是否含 token。
func headerContainsToken(v, token string) bool {
	for _, t := range headerTokensOf([]string{v}) {
		if strings.EqualFold(t, token) {
			return true
		}
	}
	return false
}

// headerTokensOf 展开多值头并按逗号切分。
func headerTokensOf(values []string) []string {
	var out []string
	for _, v := range values {
		for _, t := range strings.Split(v, ",") {
			if t = strings.TrimSpace(t); t != "" {
				out = append(out, t)
			}
		}
	}
	return out
}
