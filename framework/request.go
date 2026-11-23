// request.go：HTTP/1.1 请求解析（服务端）。
//
// 直接面向传输层连接解析：请求行 + 头部（限额）+ 请求体
// （Content-Length 定长 / Transfer-Encoding: chunked 流式）。
// 解析出的 *Request 同时服务于两条链路：
//
//   - 传输连接链路（本文件 readRequest）；
//   - HTTP/3-QUIC 链路（stdadapter.go requestFromStd，从 net/http 请求转换）。
package framework

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
)

// 请求解析错误（服务端据此决定响应码与连接处置）。
var (
	ErrBadRequest       = errors.New("framework: bad request")
	ErrRequestLineToo   = errors.New("framework: request line too long")
	ErrHeaderTooLarge   = errors.New("framework: header too large")
	ErrBadProtocol      = errors.New("framework: unsupported protocol version")
	ErrBodyTooLarge     = errors.New("framework: body too large")
	ErrChunkedBadFormat = errors.New("framework: malformed chunked body")
)

const (
	maxRequestLine = 8 * 1024  // 请求行上限
	defaultMaxHdr  = 64 * 1024 // 头部总大小上限
	defaultMaxBody = 32 << 20  // 请求体上限 32MB
	maxDrainBody   = 64 * 1024 // keep-alive 复用前最多排水量
)

var base64Encoding = base64.StdEncoding

// Request 是框架内的统一请求视图。
type Request struct {
	Method        string
	Proto         string // "HTTP/1.1"
	Target        string // 原始请求目标（含 query）
	Path          string // 已解码路径
	RawQuery      string
	Query         url.Values
	Header        http.Header
	Host          string
	ContentLength int64 // -1 = chunked/未知
	RemoteAddr    string

	Body io.Reader // nil = 无请求体

	// 连接链路内部字段（HTTP/3 链路为 nil）：
	rawConn    net.Conn      // 原始连接（Hijack/WebSocket 升级用）
	br         *bufio.Reader // 头部解析后的残留缓冲（升级时移交给 ws.Conn）
	expectCont bool          // Expect: 100-continue
	closeAfter bool          // 请求声明 Connection: close
	http10     bool          // HTTP/1.0
}

// BasicAuth 解析 Authorization: Basic。
func (r *Request) BasicAuth() (user, pass string, ok bool) {
	auth := r.Header.Get("Authorization")
	const prefix = "Basic "
	if len(auth) < len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		return "", "", false
	}
	return parseBasicAuth(auth[len(prefix):])
}

func parseBasicAuth(auth string) (user, pass string, ok bool) {
	const maxLen = 512
	if len(auth) > maxLen {
		return "", "", false
	}
	dec := make([]byte, base64Encoding.DecodedLen(len(auth)))
	n, err := base64Encoding.Decode(dec, []byte(auth))
	if err != nil {
		return "", "", false
	}
	cs := string(dec[:n])
	i := strings.IndexByte(cs, ':')
	if i < 0 {
		return "", "", false
	}
	return cs[:i], cs[i+1:], true
}

// readLine 读取单行（bufio 自带缓冲上限内），返回去掉行尾 CRLF/LF 的内容。
func readLine(br *bufio.Reader) (line []byte, err error) {
	var buf []byte
	for {
		chunk, more, e := br.ReadLine()
		if e == io.EOF && len(chunk) == 0 && len(buf) == 0 {
			return nil, io.EOF
		}
		if e != nil && e != bufio.ErrBufferFull {
			return nil, e
		}
		buf = append(buf, chunk...)
		if !more {
			return buf, nil
		}
		if len(buf) > 1<<20 { // 单行绝对上限
			return nil, ErrHeaderTooLarge
		}
	}
}

// parseLimits 解析限额。
type parseLimits struct {
	maxHeader int
	maxBody   int64
}

// readRequest 从 br（底层 conn）解析一个完整请求头与请求体视图。
//
// 只应被服务端 session 循环调用；返回的 Body 在连接复用前必须被排空或放弃。
func readRequest(br *bufio.Reader, conn net.Conn, lim parseLimits) (*Request, error) {
	if lim.maxHeader <= 0 {
		lim.maxHeader = defaultMaxHdr
	}
	if lim.maxBody <= 0 {
		lim.maxBody = defaultMaxBody
	}

	line, err := readLine(br)
	if err != nil {
		return nil, err // io.EOF / 超长
	}
	if len(line) > maxRequestLine {
		return nil, ErrRequestLineToo
	}
	method, target, proto, ok := splitRequestLine(string(line))
	if !ok {
		return nil, ErrBadRequest
	}

	// 头部
	hdr := make(http.Header)
	total := 0
	for {
		row, err := readLine(br)
		if err != nil {
			if err == io.EOF {
				return nil, ErrBadRequest // 头没读完就 EOF
			}
			return nil, err
		}
		if len(row) == 0 {
			break // 空行：头部结束
		}
		total += len(row)
		if total > lim.maxHeader {
			return nil, ErrHeaderTooLarge
		}
		colon := bytes.IndexByte(row, ':')
		if colon <= 0 {
			return nil, ErrBadRequest
		}
		name := textproto.TrimString(string(row[:colon]))
		value := textproto.TrimString(string(row[colon+1:]))
		if !validHeaderName(name) {
			return nil, ErrBadRequest
		}
		hdr.Add(textproto.CanonicalMIMEHeaderKey(name), value)
	}

	protoOK := proto == "HTTP/1.1" || proto == "HTTP/1.0"
	if !protoOK {
		return nil, ErrBadProtocol
	}

	req := &Request{
		Method:  method,
		Proto:   proto,
		Target:  target,
		Header:  hdr,
		Host:    hdr.Get("Host"),
		http10:  proto == "HTTP/1.0",
		rawConn: conn,
		br:      br,
	}

	// 目标解析
	path, rawQuery, _ := strings.Cut(target, "?")
	if u, err := url.ParseRequestURI(target); err == nil && u.Path != "" {
		path, rawQuery = u.Path, u.RawQuery
	}
	if decoded, derr := url.PathUnescape(path); derr == nil {
		req.Path = decoded
	} else {
		req.Path = path
	}
	req.RawQuery = rawQuery
	if rawQuery != "" {
		if q, qerr := url.ParseQuery(rawQuery); qerr == nil {
			req.Query = q
		}
	}
	if req.Query == nil {
		req.Query = url.Values{}
	}

	// 连接语义
	connHdr := hdr.Get("Connection")
	tokens := headerTokens(connHdr)
	for _, t := range tokens {
		switch strings.ToLower(t) {
		case "close":
			req.closeAfter = true
		case "keep-alive":
			// HTTP/1.1 默认 keep-alive；显式声明无需处理
		}
	}
	if req.http10 && !hasToken(tokens, "keep-alive") {
		req.closeAfter = true
	}
	if v := hdr.Get("Expect"); v != "" && strings.Contains(strings.ToLower(v), "100-continue") {
		req.expectCont = true
	}

	// 请求体
	te := headerTokens(hdr.Get("Transfer-Encoding"))
	if len(te) > 0 && !isChunked(te) {
		return nil, ErrBadRequest
	}
	clHdr := hdr.Values("Content-Length")
	switch {
	case isChunked(te):
		req.ContentLength = -1
		req.Body = newChunkedBody(br, req, lim.maxBody)
	case len(clHdr) == 0:
		req.ContentLength = 0
	default:
		n, cerr := strconv.ParseInt(clHdr[len(clHdr)-1], 10, 64)
		if cerr != nil || n < 0 {
			return nil, ErrBadRequest
		}
		if len(clHdr) > 1 { // 多个不一致的 CL
			for _, v := range clHdr {
				if m, _ := strconv.ParseInt(v, 10, 64); m != n {
					return nil, ErrBadRequest
				}
			}
		}
		if n > lim.maxBody {
			return nil, ErrBodyTooLarge
		}
		req.ContentLength = n
		if n > 0 || method == "POST" || method == "PUT" || method == "PATCH" {
			req.Body = newLengthBody(br, req, n)
		}
	}
	return req, nil
}

// splitRequestLine 拆解请求行。
func splitRequestLine(line string) (method, target, proto string, ok bool) {
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return "", "", "", false
	}
	if parts[0] == "" || parts[1] == "" || !strings.HasPrefix(parts[2], "HTTP/1.") {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= ' ' || c >= 0x7f {
			return false
		}
	}
	return true
}

func isChunked(te []string) bool {
	if len(te) != 1 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(te[0]), "chunked")
}

// headerTokens 按逗号切分头值（Connection / TE 等）。
func headerTokens(v string) []string {
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := parts[:0]
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func hasToken(tokens []string, want string) bool {
	for _, t := range tokens {
		if strings.EqualFold(t, want) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 请求体读取

// body 是统一请求体：跟踪已读/未读字节数，供 keep-alive 排水决策；
// 首次读取时惰性发送 100-continue。
type body struct {
	src     *bufio.Reader
	remain  int64 // 剩余（CL 模式）；chunked 模式为当前块剩余
	chunked bool
	req     *Request
	max     int64
	read    int64
	sent100 bool
}

func newLengthBody(br *bufio.Reader, req *Request, n int64) *body {
	return &body{src: br, remain: n, req: req, max: n}
}

func newChunkedBody(br *bufio.Reader, req *Request, max int64) *body {
	return &body{src: br, chunked: true, req: req, max: max}
}

func (b *body) Read(p []byte) (int, error) {
	if !b.sent100 && b.req != nil && b.req.expectCont {
		b.sent100 = true
		if b.req.rawConn != nil {
			_, _ = b.req.rawConn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
		}
	}
	if b.chunked {
		return b.readChunked(p)
	}
	if b.remain <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > b.remain {
		p = p[:b.remain]
	}
	n, err := b.src.Read(p)
	b.remain -= int64(n)
	b.read += int64(n)
	if err == io.EOF && b.remain > 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func (b *body) Close() error { return nil }

// unconsumed 返回尚未读取的估算字节数（chunked 返回已读总量）。
func (b *body) unconsumed() int64 {
	if b.chunked {
		return b.read
	}
	return b.remain
}

// readChunked 实现 chunked 流式读取（RFC 9112 §7.1）。
func (b *body) readChunked(p []byte) (int, error) {
	if b.remain == 0 { // 需要读下一个块头
		size, err := readChunkSize(b.src)
		if err != nil {
			return 0, err
		}
		if size == 0 {
			if err := drainChunkTrailers(b.src); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		b.remain = size
	}
	if int64(len(p)) > b.remain {
		p = p[:b.remain]
	}
	n, err := b.src.Read(p)
	b.remain -= int64(n)
	b.read += int64(n)
	if b.read > b.max {
		return n, ErrBodyTooLarge
	}
	if b.remain == 0 && err == nil {
		if e := expectCRLF(b.src); e != nil {
			return n, e
		}
	}
	if err == io.EOF {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

// readChunkSize 读取 "size[;ext]\r\n"。
func readChunkSize(br *bufio.Reader) (int64, error) {
	line, err := readLine(br)
	if err != nil {
		if err == io.EOF {
			return 0, io.ErrUnexpectedEOF
		}
		return 0, err
	}
	spec := string(line)
	if i := strings.IndexByte(spec, ';'); i >= 0 {
		spec = spec[:i]
	}
	spec = strings.TrimSpace(spec)
	n, err := strconv.ParseInt(spec, 16, 64)
	if err != nil || n < 0 {
		return 0, ErrChunkedBadFormat
	}
	return n, nil
}

// expectCRLF 消耗块尾 CRLF。
func expectCRLF(br *bufio.Reader) error {
	b, err := br.ReadByte()
	if err != nil {
		return err
	}
	if b != '\r' {
		return ErrChunkedBadFormat
	}
	b, err = br.ReadByte()
	if err != nil {
		return err
	}
	if b != '\n' {
		return ErrChunkedBadFormat
	}
	return nil
}

// drainChunkTrailers 读取终止块后的 trailer 头直到空行。
func drainChunkTrailers(br *bufio.Reader) error {
	total := 0
	for {
		line, err := readLine(br)
		if err != nil {
			return err
		}
		if len(line) == 0 {
			return nil
		}
		total += len(line)
		if total > defaultMaxHdr {
			return ErrHeaderTooLarge
		}
	}
}

// drainBody 排空剩余请求体（最多 maxDrainBody），返回是否可复用连接。
func drainBody(b *body) bool {
	if b == nil {
		return true
	}
	if !b.chunked && b.unconsumed() > maxDrainBody {
		return false
	}
	if b.chunked && b.read > 0 { // chunked 半途而废：直接不复用
		return false
	}
	buf := make([]byte, 4096)
	for {
		n, err := b.Read(buf)
		if err == io.EOF {
			return true
		}
		if err != nil {
			return false
		}
		if n == 0 {
			return false
		}
	}
}

// requestFromStd 把 net/http 请求转换为框架请求视图（HTTP/2 与 HTTP/3 链路共用）。
func requestFromStd(r *http.Request) *Request {
	proto := r.Proto
	if proto == "" {
		proto = "HTTP/3" // http3 转换路径无 Proto 字段时按 H3 语义
	}
	req := &Request{
		Method:        r.Method,
		Proto:         proto,
		Target:        r.RequestURI,
		Path:          r.URL.Path,
		RawQuery:      r.URL.RawQuery,
		Query:         r.URL.Query(),
		Header:        r.Header,
		Host:          r.Host,
		ContentLength: r.ContentLength,
		RemoteAddr:    r.RemoteAddr,
	}
	if r.Body != nil && r.ContentLength != 0 {
		req.Body = r.Body
	}
	return req
}
