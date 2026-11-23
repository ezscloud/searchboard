// client.go：HTTP/1.1 客户端。
//
// 拨号走 webfw/transport（unix 系平台为 netpoll Dialer，其余标准库），
// 请求/响应编解码与服务端同源（CL 定长 / chunked 流式）。
//
// 连接池（v2）：按 host:port 池化 keep-alive 连接；响应体读完且连接
// 仍活跃时自动回收复用；空闲连接由后台清扫关闭。
package framework

import (
	"bufio"
	"context"
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
	"sync"
	"time"

	"github.com/ezscloud/searchboard/framework/jsonx"
	"github.com/ezscloud/searchboard/framework/transport"
)

// Client HTTP 客户端。
type Client struct {
	// Timeout 单请求总时限（拨号+写+读响应头），默认 30s。
	Timeout time.Duration

	// TLSClientConfig https 配置（nil 则按默认，ServerName 自动取 host）。
	TLSClientConfig *tls.Config

	// UserAgent 默认 "webfw/0.1"。
	UserAgent string

	// DisablePool 禁用连接池（每请求一连接，Connection: close）。
	DisablePool bool

	// MaxIdleConnsPerHost 每 host 空闲连接上限（默认 4；DisablePool 时无效）。
	MaxIdleConnsPerHost int

	// IdleTimeout 空闲连接保活时限（默认 90s；服务端通常 60-120s）。
	IdleTimeout time.Duration

	pool         *connPool // 惰性初始化
	initPoolOnce sync.Once
}

// NewClient 默认客户端（连接池启用）。
func NewClient() *Client {
	return &Client{
		Timeout:             30 * time.Second,
		UserAgent:           serverName,
		MaxIdleConnsPerHost: 4,
		IdleTimeout:         90 * time.Second,
	}
}

// pooledConn 池化连接：连接 + 已包好的 bufio 读缓冲（复用时保留状态，
// 避免上一响应残留在新 bufio 中错位）。
type pooledConn struct {
	conn net.Conn
	br   *bufio.Reader
	key  string // host:port（+scheme 区分 http/https 池）
	at   time.Time
}

// ClientRequest 客户端请求。
type ClientRequest struct {
	Method  string
	URL     string
	Header  http.Header
	Body    io.Reader // 未知长度 → chunked 上行
	BodyLen int64     // ≥0 时写 Content-Length；-1 未知（默认）
	Ctx     context.Context
}

// NewClientRequest 组装请求。
func NewClientRequest(ctx context.Context, method, rawurl string, body io.Reader) *ClientRequest {
	return &ClientRequest{Method: method, URL: rawurl, Header: make(http.Header), Body: body, BodyLen: -1, Ctx: ctx}
}

// SetHeader 设置请求头。
func (r *ClientRequest) SetHeader(k, v string) *ClientRequest {
	r.Header.Set(k, v)
	return r
}

// SetBodyBytes 以字节为体（自动定长）。
func (r *ClientRequest) SetBodyBytes(b []byte) *ClientRequest {
	r.Body = bytesReaderOf(b)
	r.BodyLen = int64(len(b))
	return r
}

// SetBasicAuth 设置 HTTP Basic 凭证。
func (r *ClientRequest) SetBasicAuth(user, pass string) *ClientRequest {
	r.Header.Set("Authorization", "Basic "+basicAuthValue(user, pass))
	return r
}

// Response 客户端响应。
type Response struct {
	StatusCode    int
	Status        string
	Proto         string
	Header        http.Header
	ContentLength int64 // -1 = chunked/EOF 界定
	RemoteAddr    string

	body io.Reader
	conn net.Conn
	br   *bufio.Reader // 池化模式：连接的读缓冲

	// 归还回调（池化模式；nil=非池化直连）
	release func(reusable bool)

	// 复用判定材料
	reqMethod string
	drained   bool // 响应体已读尽（EOF/CL 满/chunked 终止块）
	isHead    bool
}

// Body 响应体读取。
func (r *Response) Body() io.Reader { return r.body }

// ReadAll 读完响应体。
func (r *Response) ReadAll() ([]byte, error) {
	if r.body == nil {
		r.drained = true
		return nil, nil
	}
	b, err := io.ReadAll(r.body)
	if err == nil || err == io.EOF {
		r.drained = true
	}
	return b, err
}

// Close 关闭/归还连接。
//
// 池化模式：响应体已读完（EOF/CL 满/chunked 终止块）且服务端允许
// keep-alive 时归还池复用；否则关闭。非池化模式直接关闭。
func (r *Response) Close() error {
	if r.release != nil {
		r.release(r.reusable())
		r.release = nil
		return nil
	}
	if r.conn != nil {
		return r.conn.Close()
	}
	return nil
}

// reusable 连接是否可复用：
//   - 响应未声明 Connection: close；
//   - 响应体已读尽（drained）；
//   - EOF 界定响应不可复用（分界即断开语义）；HEAD 无体天然读尽。
func (r *Response) reusable() bool {
	if r.conn == nil || r.release == nil {
		return false
	}
	for _, tok := range headerTokens(r.Header.Get("Connection")) {
		if strings.EqualFold(tok, "close") {
			return false
		}
	}
	if r.eofBounded() == eofBoundedTrue && !r.isHead {
		return false
	}
	return r.drained || r.isHead
}

// eofBounded 响应体是否以 EOF（而非 CL/chunked）界定。
type eofBounded int

const (
	eofBoundedNo   eofBounded = iota // CL 或 chunked 界定
	eofBoundedTrue                   // EOF 界定
	eofBoundedUnknown
)

func (r *Response) eofBounded() eofBounded {
	te := headerTokens(r.Header.Get("Transfer-Encoding"))
	if isChunked(te) {
		return eofBoundedNo
	}
	if len(r.Header.Values("Content-Length")) > 0 {
		return eofBoundedNo
	}
	return eofBoundedTrue
}

// JSON 以 sonic 解析响应体到 v。
func (r *Response) JSON(v any) error {
	b, err := r.ReadAll()
	if err != nil {
		return err
	}
	return jsonx.Unmarshal(b, v)
}

// Get 便捷请求。
func (c *Client) Get(ctx context.Context, rawurl string) (*Response, error) {
	return c.Do(NewClientRequest(ctx, http.MethodGet, rawurl, nil))
}

// PostJSON 便捷 JSON POST。
func (c *Client) PostJSON(ctx context.Context, rawurl string, v any) (*Response, error) {
	b, err := jsonx.Marshal(v)
	if err != nil {
		return nil, err
	}
	req := NewClientRequest(ctx, http.MethodPost, rawurl, bytesReaderOf(b))
	req.BodyLen = int64(len(b))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	return c.Do(req)
}

// Do 执行请求。
func (c *Client) Do(req *ClientRequest) (*Response, error) {
	if req.Ctx == nil {
		req.Ctx = context.Background()
	}
	if err := req.Ctx.Err(); err != nil {
		return nil, err
	}
	deadline := time.Time{}
	if c.Timeout > 0 {
		deadline = time.Now().Add(c.Timeout)
	}
	if d, ok := req.Ctx.Deadline(); ok && (deadline.IsZero() || d.Before(deadline)) {
		deadline = d
	}

	u, err := url.Parse(req.URL)
	if err != nil {
		return nil, err
	}
	hostPort, err := hostPortOf(u)
	if err != nil {
		return nil, err
	}
	poolKey := u.Scheme + "://" + hostPort
	pool := c.poolOf()

	// 优先取池化连接
	var pc *pooledConn
	if pool != nil {
		if got := pool.acquire(poolKey); got != nil {
			pc = got
		}
	}

	dialTimeout := 10 * time.Second
	if !deadline.IsZero() {
		if rest := time.Until(deadline); rest <= 0 {
			return nil, context.DeadlineExceeded
		} else if rest < dialTimeout {
			dialTimeout = rest
		}
	}

	var conn net.Conn
	var br *bufio.Reader
	if pc != nil {
		conn = pc.conn
		br = pc.reader()
		_ = conn.SetDeadline(time.Time{}) // 清掉取用时可能残留的期限
	} else {
		conn, err = transport.Dial("tcp", hostPort, dialTimeout)
		if err != nil {
			return nil, err
		}
		if u.Scheme == "https" {
			tlsCfg := c.TLSClientConfig
			if tlsCfg == nil {
				tlsCfg = &tls.Config{ServerName: u.Hostname()}
			}
			tconn := tls.Client(conn, tlsCfg)
			hsDeadline := 10 * time.Second
			if !deadline.IsZero() && time.Until(deadline) < hsDeadline {
				hsDeadline = time.Until(deadline)
			}
			_ = tconn.SetDeadline(time.Now().Add(hsDeadline))
			if err := tconn.Handshake(); err != nil {
				_ = conn.Close()
				return nil, err
			}
			conn = tconn
		}
	}

	// 池化：释放回调（响应侧 Close 时把 br 注入 finalPC 后归还）
	var release func(reusable bool)
	var finalPC *pooledConn
	var brForPool *bufio.Reader
	if pool != nil {
		finalPC = &pooledConn{conn: conn, key: poolKey}
		release = func(reusable bool) {
			finalPC.br = brForPool
			pool.release(finalPC, reusable)
		}
	}

	resp, rtErr := c.roundTripPooled(conn, br, req, u, deadline, release, poolKey, &brForPool)
	if rtErr != nil {
		_ = conn.Close()
		return nil, rtErr
	}
	return resp, nil
}

// roundTripPooled 在 conn 上写出请求并解析响应。
//
// br 为池化连接的既有读缓冲（复用时保留状态）；nil 时新建。
// brOut 用于把（新建的）读缓冲回传给 release 侧注入池化连接。
func (c *Client) roundTripPooled(conn net.Conn, br *bufio.Reader, req *ClientRequest, u *url.URL,
	deadline time.Time, release func(bool), poolKey string, brOut **bufio.Reader) (*Response, error) {

	pooled := release != nil
	if !deadline.IsZero() {
		_ = conn.SetWriteDeadline(deadline)
	}

	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	target := u.RequestURI()
	if target == "" {
		target = "/"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\n", method, target)
	fmt.Fprintf(&b, "Host: %s\r\n", requestHost(u))
	if req.Header.Get("User-Agent") == "" {
		ua := c.UserAgent
		if ua == "" {
			ua = serverName
		}
		fmt.Fprintf(&b, "User-Agent: %s\r\n", ua)
	}
	if req.Header.Get("Accept-Encoding") == "" {
		b.WriteString("Accept-Encoding: identity\r\n")
	}
	// 池化：keep-alive；非池化：close（响应以 EOF 界定）
	if pooled {
		if req.Header.Get("Connection") == "" {
			b.WriteString("Connection: keep-alive\r\n")
		}
	} else {
		if req.Header.Get("Connection") == "" {
			b.WriteString("Connection: close\r\n")
		}
	}
	for k, vs := range req.Header {
		for _, v := range vs {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}

	switch {
	case req.Body == nil:
		b.WriteString("Content-Length: 0\r\n\r\n")
		if err := writeAll(conn, b.String()); err != nil {
			return nil, err
		}
	case req.BodyLen >= 0:
		fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n", req.BodyLen)
		if err := writeAll(conn, b.String()); err != nil {
			return nil, err
		}
		if err := copyN(conn, req.Body, req.BodyLen); err != nil {
			return nil, err
		}
	default:
		b.WriteString("Transfer-Encoding: chunked\r\n\r\n")
		if err := writeAll(conn, b.String()); err != nil {
			return nil, err
		}
		if err := writeChunked(conn, req.Body); err != nil {
			return nil, err
		}
	}
	return c.readResponse(conn, br, req, deadline, release, brOut)
}

// readResponse 解析响应头并构造响应体视图。
//
// br 为既有读缓冲（池化复用）；nil 新建。brOut 回传给池化侧注入。
func (c *Client) readResponse(conn net.Conn, brIn *bufio.Reader, req *ClientRequest,
	deadline time.Time, release func(bool), brOut **bufio.Reader) (*Response, error) {

	if !deadline.IsZero() {
		_ = conn.SetReadDeadline(deadline)
	}
	br := brIn
	if br == nil {
		br = bufio.NewReaderSize(conn, 4096)
	}

	line, err := readLine(br)
	if err != nil {
		return nil, err
	}
	proto, code, text, ok := splitStatusLine(string(line))
	if !ok {
		return nil, ErrBadRequest
	}

	hdr := make(http.Header)
	for {
		row, err := readLine(br)
		if err != nil {
			return nil, err
		}
		if len(row) == 0 {
			break
		}
		i := strings.IndexByte(string(row), ':')
		if i <= 0 {
			continue // 宽松跳过坏行
		}
		hdr.Add(strings.TrimSpace(string(row[:i])), strings.TrimSpace(string(row[i+1:])))
	}

	if brOut != nil {
		*brOut = br
	}

	method := ""
	if req != nil {
		method = req.Method
	}
	resp := &Response{
		StatusCode: code,
		Status:     text,
		Proto:      proto,
		Header:     hdr,
		conn:       conn,
		br:         br,
		reqMethod:  method,
		isHead:     method == http.MethodHead,
		release:    release,
	}

	te := headerTokens(hdr.Get("Transfer-Encoding"))
	cl := hdr.Values("Content-Length")
	switch {
	case isChunked(te):
		resp.ContentLength = -1
		resp.body = newChunkedBody(br, nil, 1<<30)
	case len(cl) > 0:
		n, _ := strconv.ParseInt(cl[len(cl)-1], 10, 64)
		resp.ContentLength = n
		if n > 0 {
			resp.body = newLengthBody(br, nil, n)
		} else {
			resp.drained = true // CL=0：体天然读尽（可立即归还复用）
		}
	default:
		resp.ContentLength = -1
		resp.body = br // 读到 EOF（Connection: close）
	}
	// HEAD 响应不含响应体（CL 仅为元信息）
	if resp.isHead {
		resp.body = nil
		resp.drained = true
	}
	return resp, nil
}

func splitStatusLine(line string) (proto string, code int, text string, ok bool) {
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

func writeChunked(w io.Writer, body io.Reader) error {
	// 注意：全部写路径必须走 Write([]byte)。netpoll.Connection 实现了
	// io.StringWriter（nocopy WriteString 只入缓冲不 flush），io.WriteString
	// 会优先选它导致字节滞留——这是 netpoll 传输上的隐性坑。
	buf := make([]byte, 32*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := fmt.Fprintf(w, "%x\r\n", n); werr != nil {
				return werr
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if _, werr := w.Write([]byte("\r\n")); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			_, werr := w.Write([]byte("0\r\n\r\n"))
			return werr
		}
		if err != nil {
			return err
		}
	}
}

func copyN(w io.Writer, body io.Reader, n int64) error {
	if n == 0 {
		return nil
	}
	_, err := io.Copy(w, io.LimitReader(body, n))
	return err
}

func writeAll(w io.Writer, s string) error {
	// 见 writeChunked 注释：不能 io.WriteString（netpoll nocopy WriteString 不 flush）。
	_, err := w.Write([]byte(s))
	return err
}

func requestHost(u *url.URL) string {
	host := u.Host
	if u.Port() == "" {
		switch u.Scheme {
		case "https":
			host = u.Hostname() + ":443"
		default:
			host = u.Hostname() + ":80"
		}
	}
	return host
}

func hostPortOf(u *url.URL) (string, error) {
	host := u.Hostname()
	if host == "" {
		return "", errors.New("framework: url missing host")
	}
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "http", "":
			port = "80"
		default:
			return "", errors.New("framework: unsupported scheme: " + u.Scheme)
		}
	}
	return net.JoinHostPort(host, port), nil
}

func bytesReaderOf(b []byte) io.Reader { return &bytesSlice{b: b} }

// basicAuthValue 生成 Basic 头值（同 net/http.SetBasicAuth 语义）。
func basicAuthValue(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

// bytesSlice 与 bytes.Reader 等价（避免直接依赖 bytes 包导出集合）。
type bytesSlice struct{ b []byte }

func (r *bytesSlice) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}
