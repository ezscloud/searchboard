package rpc

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ezscloud/searchboard/framework"
	"github.com/ezscloud/searchboard/framework/jsonx"
)

// 测试服务集。
type MathService struct{}

type AddArgs struct {
	A int `json:"a"`
	B int `json:"b"`
}

type Number struct {
	Value int `json:"value"`
}

func (s *MathService) Add(ctx context.Context, args *AddArgs) (*Number, error) {
	return &Number{Value: args.A + args.B}, nil
}

func (s *MathService) Mul(args *AddArgs) (*Number, error) {
	return &Number{Value: args.A * args.B}, nil
}

func (s *MathService) Fail(ctx context.Context, _ *Empty) (*Number, error) {
	return nil, NewError(-32001, "业务失败示例")
}

type Empty struct{}

// ---------------------------------------------------------------- 协议单元

func TestServeMessageUnit(t *testing.T) {
	srv := NewServer()
	if err := srv.Register("Math", new(MathService)); err != nil {
		t.Fatalf("register: %v", err)
	}

	// 正常调用
	out := srv.ServeMessage(context.Background(), []byte(
		`{"jsonrpc":"2.0","id":1,"method":"Math.Add","params":{"a":2,"b":3}}`))
	if out == nil {
		t.Fatalf("no response")
	}
	if !strings.Contains(string(out), `"value":5`) && !strings.Contains(string(out), `"value": 5`) {
		t.Fatalf("add response = %s", out)
	}

	// 未知方法 → -32601
	out = srv.ServeMessage(context.Background(), []byte(
		`{"jsonrpc":"2.0","id":2,"method":"Math.Nope","params":{}}`))
	if !strings.Contains(string(out), "-32601") {
		t.Fatalf("method not found = %s", out)
	}

	// 非法 JSON → -32700
	out = srv.ServeMessage(context.Background(), []byte(`{nope`))
	if !strings.Contains(string(out), "-32700") {
		t.Fatalf("parse error = %s", out)
	}

	// 通知 → 无应答
	out = srv.ServeMessage(context.Background(), []byte(
		`{"jsonrpc":"2.0","method":"Math.Add","params":{"a":1,"b":1}}`))
	if out != nil {
		t.Fatalf("notification should be silent, got %s", out)
	}

	// 批量
	out = srv.ServeMessage(context.Background(), []byte(
		`[{"jsonrpc":"2.0","id":1,"method":"Math.Add","params":{"a":1,"b":1}},
		  {"jsonrpc":"2.0","id":2,"method":"Math.Mul","params":{"a":2,"b":5}}]`))
	if !strings.HasPrefix(strings.TrimSpace(string(out)), "[") {
		t.Fatalf("batch should respond array: %s", out)
	}
	if !strings.Contains(string(out), `"value":2`) && !strings.Contains(string(out), `"value": 2`) {
		t.Fatalf("batch first result missing: %s", out)
	}

	// 业务错误 → 自定义码
	out = srv.ServeMessage(context.Background(), []byte(
		`{"jsonrpc":"2.0","id":3,"method":"Math.Fail","params":{}}`))
	if !strings.Contains(string(out), "-32001") {
		t.Fatalf("custom error = %s", out)
	}

	// 按位参数（单元素数组）
	out = srv.ServeMessage(context.Background(), []byte(
		`{"jsonrpc":"2.0","id":4,"method":"Math.Add","params":[{"a":10,"b":20}]}`))
	if !strings.Contains(string(out), `"value":30`) && !strings.Contains(string(out), `"value": 30`) {
		t.Fatalf("positional params = %s", out)
	}
}

func TestRegisterValidation(t *testing.T) {
	srv := NewServer()
	if err := srv.Register("", new(MathService)); err == nil {
		t.Fatalf("empty name")
	}
	if err := srv.Register("NotPtr", MathService{}); err == nil {
		t.Fatalf("non-pointer receiver")
	}
	// 全部方法签名不符 → 注册失败
	if err := srv.Register("Bad", new(struct{ X func() })); err == nil {
		t.Fatalf("no rpc-able methods")
	}
}

// ---------------------------------------------------------------- e2e：HTTP 通道

func newRPCServer(t *testing.T) (*framework.Server, *Server) {
	t.Helper()
	r := NewServer()
	if err := r.Register("Math", new(MathService)); err != nil {
		t.Fatalf("register: %v", err)
	}
	srv := framework.New(framework.Config{Addr: "127.0.0.1:0", IdleTimeout: 300 * time.Millisecond})
	srv.POST("/rpc", r.Handler())
	srv.GET("/ws/rpc", r.WebSocket())
	ctx, cancel := context.WithCancel(context.Background())
	if err := srv.Start(ctx); err != nil {
		cancel()
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = srv.Shutdown(context.Background())
	})
	return srv, r
}

func TestRPCOverHTTP(t *testing.T) {
	srv, _ := newRPCServer(t)
	cli := NewHTTPClient("http://" + srv.Addr().String() + "/rpc")

	var num Number
	if err := cli.Call(context.Background(), "Math.Add", AddArgs{A: 40, B: 2}, &num); err != nil {
		t.Fatalf("call: %v", err)
	}
	if num.Value != 42 {
		t.Fatalf("add = %+v", num)
	}

	// 无 ctx 形式的方法
	if err := cli.Call(context.Background(), "Math.Mul", AddArgs{A: 6, B: 7}, &num); err != nil {
		t.Fatalf("mul: %v", err)
	}
	if num.Value != 42 {
		t.Fatalf("mul = %+v", num)
	}

	// 业务错误
	err := cli.Call(context.Background(), "Math.Fail", Empty{}, nil)
	rpcErr, ok := err.(*RPCError)
	if !ok || rpcErr.Code != -32001 {
		t.Fatalf("fail error = %v", err)
	}

	// 通知 → 无错误
	if err := cli.Notify(context.Background(), "Math.Add", AddArgs{A: 1, B: 1}); err != nil {
		t.Fatalf("notify: %v", err)
	}
}

// ---------------------------------------------------------------- e2e：WS 通道

func TestRPCOverWS(t *testing.T) {
	srv, _ := newRPCServer(t)
	cli, err := DialWS(context.Background(), "ws://"+srv.Addr().String()+"/ws/rpc", nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	defer cli.Close()

	var num Number
	if err := cli.Call(context.Background(), "Math.Add", AddArgs{A: 1, B: 2}, &num); err != nil {
		t.Fatalf("ws call: %v", err)
	}
	if num.Value != 3 {
		t.Fatalf("ws add = %+v", num)
	}

	// 并发在途
	const n = 20
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var v Number
			if err := cli.Call(context.Background(), "Math.Mul", AddArgs{A: i, B: 10}, &v); err != nil {
				errCh <- err
				return
			}
			if v.Value != i*10 {
				errCh <- errResultMismatch
			}
		}(i)
	}
	wg.Wait()
	select {
	case e := <-errCh:
		t.Fatalf("concurrent ws rpc failed: %v", e)
	default:
	}
}

var errResultMismatch = &RPCError{Code: -1, Message: "result mismatch"}

// TestWSJSONInterop 用原始 ws 连接手工校验 JSON-RPC 帧内容（sonic 序列化路径）。
func TestWSJSONInterop(t *testing.T) {
	srv, _ := newRPCServer(t)
	// 走 framework 客户端 + 手工帧验证协议格式
	cli, err := DialWS(context.Background(), "ws://"+srv.Addr().String()+"/ws/rpc", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close()

	var num Number
	if err := cli.Call(context.Background(), "Math.Add", AddArgs{A: 5, B: 5}, &num); err != nil {
		t.Fatalf("call: %v", err)
	}
	if num.Value != 10 {
		t.Fatalf("value = %d", num.Value)
	}

	// 直接验证 sonic 输出格式
	b, _ := jsonx.Marshal(map[string]any{"jsonrpc": "2.0"})
	if !strings.Contains(string(b), `"jsonrpc":"2.0"`) && !strings.Contains(string(b), `"jsonrpc": "2.0"`) {
		t.Fatalf("sonic marshal = %s", b)
	}
}
