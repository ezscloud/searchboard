// rpc/client.go：JSON-RPC 客户端（HTTP 与 WS 双通道）。
//
//	HTTP：每请求一次 POST（framework.Client，unix 平台拨号走 netpoll）
//	WS  ：单连接多路复用，Call 并发在途，读泵分发应答
package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ezscloud/searchboard/framework"
	"github.com/ezscloud/searchboard/framework/jsonx"
	"github.com/ezscloud/searchboard/framework/ws"
)

// CallTimeout 单次调用默认上限。
const CallTimeout = 30 * time.Second

// Client JSON-RPC 客户端。
type Client struct {
	kind   string // "http" | "ws"
	url    string
	http   *framework.Client
	ws     *ws.Conn
	nextID atomic.Int64

	mu       sync.Mutex
	pending  map[int64]chan *message
	closed   bool
	closeErr error
}

// NewHTTPClient HTTP 通道客户端（无连接态；url 形如 http://host:port/rpc）。
func NewHTTPClient(url string) *Client {
	return &Client{kind: "http", url: url, http: framework.NewClient()}
}

// DialWS WS 通道客户端（url 形如 ws://host:port/ws/rpc）。
// 连接后启动读泵；连接断开时在途请求全部报错。
func DialWS(ctx context.Context, url string, opts *ws.DialOptions) (*Client, error) {
	conn, err := ws.Dial(ctx, url, opts)
	if err != nil {
		return nil, err
	}
	c := &Client{kind: "ws", url: url, ws: conn, pending: map[int64]chan *message{}}
	go c.readPump()
	return c, nil
}

// Kind 通道类型。
func (c *Client) Kind() string { return c.kind }

// URL 目标地址。
func (c *Client) URL() string { return c.url }

// Call 远程调用（params 可为任意 JSON 结构；result 可为 nil 丢弃结果）。
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, CallTimeout)
		defer cancel()
	}
	if c.kind == "ws" {
		return c.callWS(ctx, method, params, result)
	}
	return c.callHTTP(ctx, method, params, result)
}

// Notify 通知（无应答）。
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	id := json.RawMessage(nil)
	req := &message{JSONRPC: Version, ID: id, Method: method}
	if params != nil {
		if b, err := jsonx.Marshal(params); err == nil {
			req.Params = b
		} else {
			return err
		}
	}
	if c.kind == "ws" {
		b, err := jsonx.Marshal(req)
		if err != nil {
			return err
		}
		return c.ws.WriteMessage(ws.OpText, b)
	}
	body, err := jsonx.Marshal(req)
	if err != nil {
		return err
	}
	hreq := framework.NewClientRequest(ctx, http.MethodPost, c.url, nil)
	hreq.SetBodyBytes(body)
	hreq.SetHeader("Content-Type", "application/json; charset=utf-8")
	resp, err := c.http.Do(hreq)
	if resp != nil {
		defer resp.Close()
	}
	return err
}

func (c *Client) callHTTP(ctx context.Context, method string, params, result any) error {
	id := c.nextID.Add(1)
	req := &message{
		JSONRPC: Version,
		ID:      json.RawMessage(fmt.Sprintf("%d", id)),
		Method:  method,
	}
	if params != nil {
		b, err := jsonx.Marshal(params)
		if err != nil {
			return err
		}
		req.Params = b
	}
	body, err := jsonx.Marshal(req)
	if err != nil {
		return err
	}

	hreq := framework.NewClientRequest(ctx, http.MethodPost, c.url, nil)
	hreq.SetBodyBytes(body)
	hreq.SetHeader("Content-Type", "application/json; charset=utf-8")
	resp, err := c.http.Do(hreq)
	if err != nil {
		return err
	}
	defer resp.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("rpc: http status %d", resp.StatusCode)
	}
	data, err := resp.ReadAll()
	if err != nil {
		return err
	}
	return decodeResponse(data, result)
}

func (c *Client) callWS(ctx context.Context, method string, params, result any) error {
	id := c.nextID.Add(1)
	req := &message{
		JSONRPC: Version,
		ID:      json.RawMessage(fmt.Sprintf("%d", id)),
		Method:  method,
	}
	if params != nil {
		b, err := jsonx.Marshal(params)
		if err != nil {
			return err
		}
		req.Params = b
	}
	b, err := jsonx.Marshal(req)
	if err != nil {
		return err
	}

	ch := make(chan *message, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClientClosed
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.ws.WriteMessage(ws.OpText, b); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case msg := <-ch:
		if msg == nil {
			return ErrClientClosed
		}
		return decode(msg, result)
	}
}

// readPump WS 读泵：分发应答到在途请求。
func (c *Client) readPump() {
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			c.failPending(err)
			return
		}
		if len(data) == 0 {
			continue
		}
		var msg message
		if err := jsonx.Unmarshal(data, &msg); err != nil {
			continue // 无法路由的坏消息：丢弃（调用侧自身超时）
		}
		id, ok := parseID(msg.ID)
		if !ok {
			continue
		}
		c.mu.Lock()
		ch, has := c.pending[id]
		if has {
			delete(c.pending, id)
		}
		c.mu.Unlock()
		if has {
			ch <- &msg
		}
	}
}

// failPending 连接断开：全部在途请求失败。
func (c *Client) failPending(cause error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.closeErr = cause
	pending := c.pending
	c.pending = map[int64]chan *message{}
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- nil
	}
}

// Close 关闭客户端（WS 通道关闭连接）。
func (c *Client) Close() error {
	if c.kind == "ws" && c.ws != nil {
		return c.ws.Close(1000, "client close")
	}
	return nil
}

// parseID 解析应答 id（数字或字符串）。
func parseID(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var i int64
	if err := jsonx.Unmarshal(raw, &i); err == nil {
		return i, true
	}
	var s string
	if err := jsonx.Unmarshal(raw, &s); err == nil {
		var n int64
		if _, err := fmt.Sscanf(s, "%d", &n); err == nil {
			return n, true
		}
	}
	return 0, false
}

// decodeResponse 解析单条应答并绑定 result。
func decodeResponse(data []byte, result any) error {
	var msg message
	if err := jsonx.Unmarshal(data, &msg); err != nil {
		return err
	}
	return decode(&msg, result)
}

func decode(msg *message, result any) error {
	if msg.Error != nil {
		return &RPCError{Code: msg.Error.Code, Message: msg.Error.Message, Data: msg.Error.Data}
	}
	if result == nil || len(msg.Result) == 0 {
		return nil
	}
	return jsonx.Unmarshal(msg.Result, result)
}
