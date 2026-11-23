// rpc/server.go：JSON-RPC 服务端。
//
// 反射注册：Register("Math", new(MathSvc))，方法按签名约定校验并索引。
// 派发入口 ServeMessage 做纯协议处理（可嵌 HTTP / WS 任意传输）。
package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/ezscloud/searchboard/framework/jsonx"
)

// Server JSON-RPC 服务端。
type Server struct {
	mu       sync.RWMutex
	services map[string]*service
}

// NewServer 创建服务端。
func NewServer() *Server { return &Server{services: map[string]*service{}} }

type service struct {
	name    string
	rcvr    reflect.Value
	methods map[string]*methodType // 小写方法名索引
}

type methodType struct {
	fn     reflect.Value
	method string // 原始导出名
	hasCtx bool
	argTyp reflect.Type // 必须为 *T
	retTyp reflect.Type // 必须为 *T
}

var typeOfContext = reflect.TypeOf((*context.Context)(nil)).Elem()

// Register 注册服务（name 即 "name.Method" 前缀；重复注册覆盖旧项）。
//
// 方法签名（导出方法，二选一，返回值末位必须 error）：
//
//	func (s *S) Method(ctx context.Context, args *A) (*R, error)
//	func (s *S) Method(args *A) (*R, error)
func (s *Server) Register(name string, rcvr any) error {
	if name == "" {
		return fmt.Errorf("rpc: empty service name")
	}
	typ := reflect.TypeOf(rcvr)
	if typ == nil || typ.Kind() != reflect.Ptr {
		return fmt.Errorf("rpc: receiver must be a non-nil pointer")
	}
	svc := &service{
		name:    name,
		rcvr:    reflect.ValueOf(rcvr),
		methods: map[string]*methodType{},
	}
	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i)
		if !m.IsExported() {
			continue
		}
		mt, err := parseMethod(m, typ)
		if err != nil {
			continue // 签名不符的方法直接忽略（允许结构体上存在辅助方法）
		}
		svc.methods[strings.ToLower(m.Name)] = mt
	}
	if len(svc.methods) == 0 {
		return fmt.Errorf("rpc: service %q has no rpc-able methods", name)
	}
	s.mu.Lock()
	s.services[name] = svc
	s.mu.Unlock()
	return nil
}

func parseMethod(m reflect.Method, typ reflect.Type) (*methodType, error) {
	fn := typ.Method(m.Index).Func
	mt := &methodType{fn: fn, method: m.Name}
	t := fn.Type()

	// 输入：recv, [ctx], args
	hasCtx := false
	switch t.NumIn() {
	case 3:
		if t.In(1) != typeOfContext {
			return nil, fmt.Errorf("arg#1 must be context.Context")
		}
		hasCtx = true
	case 2:
	default:
		return nil, fmt.Errorf("bad arg count")
	}
	mt.hasCtx = hasCtx
	argTyp := t.In(t.NumIn() - 1)
	if argTyp.Kind() != reflect.Ptr || argTyp.Elem().Kind() != reflect.Struct {
		return nil, fmt.Errorf("args must be *struct")
	}
	mt.argTyp = argTyp

	// 输出：(*Reply, error)
	if t.NumOut() != 2 {
		return nil, fmt.Errorf("must return (reply, error)")
	}
	retTyp := t.Out(0)
	if retTyp.Kind() != reflect.Ptr || retTyp.Elem().Kind() != reflect.Struct {
		return nil, fmt.Errorf("reply must be *struct")
	}
	if t.Out(1) != reflect.TypeOf((*error)(nil)).Elem() {
		return nil, fmt.Errorf("last return must be error")
	}
	mt.retTyp = retTyp
	return mt, nil
}

// call 执行单个请求，返回响应消息（通知返回 nil）。
func (s *Server) call(ctx context.Context, req *message) *message {
	resp := &message{JSONRPC: Version, ID: req.ID}

	full := req.Method
	svcName, mName, ok := splitMethod(full)
	if !ok {
		resp.Error = errInvalid()
		return resp
	}
	s.mu.RLock()
	svc := s.services[svcName]
	s.mu.RUnlock()
	if svc == nil {
		resp.Error = errNotFound(full)
		return resp
	}
	mt := svc.methods[strings.ToLower(mName)]
	if mt == nil {
		resp.Error = errNotFound(full)
		return resp
	}

	// 参数绑定
	arg := reflect.New(mt.argTyp.Elem())
	params := unwrapParams(req.Params)
	if len(params) > 0 {
		if err := jsonx.Unmarshal(params, arg.Interface()); err != nil {
			resp.Error = errParams(err.Error())
			return resp
		}
	}

	if ctx == nil {
		ctx = context.Background()
	}
	if mt.hasCtx {
		// 调用超时保护：单方法 30s 上限（可由调用方 ctx 收紧）
		if _, has := ctx.Deadline(); !has {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
		}
	}

	in := []reflect.Value{svc.rcvr}
	if mt.hasCtx {
		in = append(in, reflect.ValueOf(ctx))
	}
	in = append(in, arg)

	out := mt.fn.Call(in)
	if err := out[1].Interface(); err != nil {
		if e, ok := err.(*Error); ok {
			resp.Error = e
		} else if e, ok := err.(error); ok {
			resp.Error = errInternal(e.Error())
		} else {
			resp.Error = errInternal(fmt.Sprintf("%v", err))
		}
		return resp
	}
	if reply := out[0].Interface(); reply != nil {
		b, err := jsonx.Marshal(reply)
		if err != nil {
			resp.Error = errInternal(err.Error())
			return resp
		}
		resp.Result = b
	}
	return resp
}

// unwrapParams 按位参数（单元素数组）解包为对象绑定。
func unwrapParams(p []byte) []byte {
	if len(p) == 0 {
		return p
	}
	trim := strings.TrimSpace(string(p))
	if strings.HasPrefix(trim, "[") {
		var arr []json.RawMessage
		if err := jsonx.Unmarshal(p, &arr); err == nil {
			switch len(arr) {
			case 0:
				return nil
			case 1:
				return arr[0]
			default:
				return p // 多元素：交给 Unmarshal 报参数错误
			}
		}
	}
	return p
}

func splitMethod(m string) (svc, method string, ok bool) {
	svc, method, ok = strings.Cut(m, ".")
	return svc, method, ok && svc != "" && method != ""
}

// ServeMessage 处理一条入站字节（对象或数组批量），返回应答字节
// （无应答返回 nil：全部为通知，或空批量）。
func (s *Server) ServeMessage(ctx context.Context, data []byte) []byte {
	trim := strings.TrimSpace(string(data))
	if strings.HasPrefix(trim, "[") {
		var batch []*message
		if err := jsonx.Unmarshal(data, &batch); err != nil {
			return marshalMessage(&message{JSONRPC: Version, ID: nil, Error: errParse()})
		}
		if len(batch) == 0 {
			return marshalMessage(&message{JSONRPC: Version, ID: nil, Error: errInvalid()})
		}
		out := make([]*message, 0, len(batch))
		for _, req := range batch {
			if resp := s.serveOne(ctx, req); resp != nil {
				out = append(out, resp)
			}
		}
		if len(out) == 0 {
			return nil
		}
		b, err := jsonx.Marshal(out)
		if err != nil {
			return marshalMessage(&message{JSONRPC: Version, ID: nil, Error: errInternal(err.Error())})
		}
		return b
	}

	var req message
	if err := jsonx.Unmarshal(data, &req); err != nil {
		return marshalMessage(&message{JSONRPC: Version, ID: nil, Error: errParse()})
	}
	if resp := s.serveOne(ctx, &req); resp != nil {
		return marshalMessage(resp)
	}
	return nil // 通知：静默
}

// serveOne 处理单个已解析请求（通知不产生应答）。
func (s *Server) serveOne(ctx context.Context, req *message) *message {
	if req.JSONRPC != Version || req.Method == "" {
		if req.isNotification() {
			return nil
		}
		return &message{JSONRPC: Version, ID: req.ID, Error: errInvalid()}
	}
	resp := s.call(ctx, req)
	if resp != nil && req.isNotification() {
		return nil
	}
	return resp
}

// marshalMessage 序列化（含 id 原样回显）。
func marshalMessage(m *message) []byte {
	b, err := jsonx.Marshal(m)
	if err != nil {
		// 绝路兜底：id 无法序列化几乎不可能
		return []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"marshal error"}}`)
	}
	return b
}
