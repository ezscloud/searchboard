// framework-demo：framework 框架全特性演示（YAML 配置驱动）。
//
// 覆盖：HTTP/1.1 路由+中间件+JSON、参数路由、SSE 流式、WebSocket echo/chat、
// JSON-RPC（HTTP/WS 双通道）、QUIC/HTTP3（复用同套路由）。
//
// 传输策略由配置指定：auto（unix 平台走 netpoll，Windows 自动降级 std）/
// netpoll / std。
//
// 运行：
//
//	go build -o framework-demo.exe ./cmd/framework-demo && .\framework-demo.exe
//	（或指定配置：framework-demo.exe -config framework-demo.yml）
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ezscloud/searchboard/framework"
	"github.com/ezscloud/searchboard/framework/h3"
	"github.com/ezscloud/searchboard/framework/rpc"
	"github.com/ezscloud/searchboard/framework/ws"
)

func main() {
	var (
		confPath = flag.String("config", "framework-demo.yml", "YAML 配置文件路径")
		addr     = flag.String("addr", "", "覆盖 HTTP 监听地址")
	)
	flag.Parse()

	cfg, err := loadConfig(*confPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	if *addr != "" {
		cfg.HTTP.Addr = *addr
	}

	logf := func(format string, args ...any) { log.Printf(format, args...) }

	// ---------------------------------------------------------------- HTTP
	srv := framework.New(framework.Config{
		Addr:                 cfg.HTTP.Addr,
		Transport:            cfg.HTTP.Transport,
		IdleTimeout:          cfg.HTTP.IdleTimeout.D(),
		ReadHeaderTimeout:    cfg.HTTP.ReadHeaderTimeout.D(),
		WriteTimeout:         cfg.HTTP.WriteTimeout.D(),
		EnableH2C:            cfg.HTTP.EnableH2C,
		MaxConcurrentStreams: cfg.HTTP.MaxConcurrentStreams,
		Logf:                 logf,
	})

	srv.Use(framework.Recovery(os.Stderr), framework.RequestID(), framework.Logger(os.Stderr))

	registerHTTP(srv)
	registerWS(srv)
	rpcSrv := registerRPC(srv)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	go func() {
		if err := srv.Run(ctx); err != nil {
			logf("http server exit: %v", err)
		}
	}()

	// ---------------------------------------------------------------- QUIC
	if cfg.QUIC.Enable {
		tlsCfg, err := quicTLS(cfg)
		if err != nil {
			log.Fatalf("QUIC TLS 配置失败: %v", err)
		}
		h3srv := h3.New(h3.Config{
			Addr:      cfg.QUIC.Addr,
			TLSConfig: tlsCfg,
			Logf:      logf,
		}, srv.StdHandler())
		go func() {
			if err := h3srv.Run(ctx); err != nil {
				logf("h3 server exit: %v", err)
			}
		}()
		fmt.Printf("QUIC/HTTP3 : https://%s/hello/framework (自签证书, 客户端需跳过校验)\n", cfg.QUIC.Addr)
	}

	_ = rpcSrv
	fmt.Printf("HTTP       : http://%s\n", cfg.HTTP.Addr)
	fmt.Println("按 Ctrl+C 优雅退出 ...")

	<-ctx.Done()
	// Run 内部已做优雅停机；这里只等待打印收尾
	time.Sleep(200 * time.Millisecond)
	fmt.Println("bye")
}

// ---------------------------------------------------------------- 路由注册

func registerHTTP(srv *framework.Server) {
	srv.GET("/", func(c *framework.Context) {
		c.HTML(200, indexHTML)
	})
	srv.GET("/hello/:name", func(c *framework.Context) {
		c.JSON(200, map[string]any{
			"hello": c.Param("name"),
			"query": c.Query("q"),
			"reqID": c.GetString("requestID"),
			"tls":   c.IsTLS(),
			"proto": c.Request.Proto,
		})
	})
	srv.GET("/api/health", func(c *framework.Context) {
		c.JSON(200, map[string]any{"ok": true, "now": time.Now().Format(time.RFC3339)})
	})
	srv.POST("/api/echo", func(c *framework.Context) {
		var in map[string]any
		if err := c.BindJSON(&in); err != nil {
			c.Error(400, "bad json: "+err.Error())
			return
		}
		c.JSON(200, in)
	})
	// SSE 流式演示
	srv.GET("/api/events", func(c *framework.Context) {
		c.SetRespHeader("Content-Type", "text/event-stream")
		c.Status(200)
		c.Flush()
		for i := 0; i < 3; i++ {
			fmt.Fprintf(c, "data: tick %d at %s\n\n", i, time.Now().Format("15:04:05.000"))
			c.Flush()
			time.Sleep(200 * time.Millisecond)
		}
	})
}

// ---------------------------------------------------------------- WebSocket

func registerWS(srv *framework.Server) {
	srv.GET("/ws/echo", func(c *framework.Context) {
		conn, err := ws.Upgrade(c, &ws.UpgradeOptions{ReadLimit: 1 << 20})
		if err != nil {
			return
		}
		defer conn.Close(ws.CloseNormal, "bye")
		for {
			op, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(op, data); err != nil {
				return
			}
		}
	})

	hub := newChatHub()
	go hub.run()
	srv.GET("/ws/chat", func(c *framework.Context) {
		conn, err := ws.Upgrade(c, nil)
		if err != nil {
			return
		}
		hub.join(conn)
	})
}

// chatHub 简单广播聊天室。
type chatHub struct {
	mu     sync.Mutex
	conns  map[*ws.Conn]struct{}
	joinCh chan *ws.Conn
}

func newChatHub() *chatHub {
	return &chatHub{conns: map[*ws.Conn]struct{}{}, joinCh: make(chan *ws.Conn, 64)}
}

func (h *chatHub) run() {
	for conn := range h.joinCh {
		go h.serve(conn)
	}
}

func (h *chatHub) join(conn *ws.Conn) {
	h.mu.Lock()
	h.conns[conn] = struct{}{}
	h.mu.Unlock()
	h.joinCh <- conn
}

func (h *chatHub) broadcast(data []byte) {
	h.mu.Lock()
	conns := make([]*ws.Conn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	for _, c := range conns {
		_ = c.WriteMessage(ws.OpText, data)
	}
}

func (h *chatHub) serve(conn *ws.Conn) {
	defer func() {
		h.mu.Lock()
		delete(h.conns, conn)
		h.mu.Unlock()
		conn.CloseNow()
	}()
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		msg := append([]byte("user> "), data...)
		h.broadcast(msg)
	}
}

// ---------------------------------------------------------------- RPC

func registerRPC(srv *framework.Server) *rpc.Server {
	r := rpc.NewServer()
	_ = r.Register("Math", new(MathService))
	_ = r.Register("Env", new(EnvService))
	srv.POST("/rpc", r.Handler())
	srv.GET("/ws/rpc", r.WebSocket())
	return r
}

// MathService 演示服务：标准两签名（带 ctx / 不带 ctx）。
type MathService struct{}

type AddArgs struct {
	A int `json:"a"`
	B int `json:"b"`
}

type MulArgs struct {
	A int `json:"a"`
	B int `json:"b"`
}

type Number struct {
	Value int `json:"value"`
}

func (s *MathService) Add(ctx context.Context, args *AddArgs) (*Number, error) {
	return &Number{Value: args.A + args.B}, nil
}

func (s *MathService) Mul(args *MulArgs) (*Number, error) {
	return &Number{Value: args.A * args.B}, nil
}

// EnvService 环境信息。
type EnvService struct{}

type InfoReply struct {
	GOOS  string `json:"goos"`
	GoVer string `json:"goVersion"`
	Time  string `json:"time"`
	Host  string `json:"host"`
}

func (s *EnvService) Info(ctx context.Context, _ *Empty) (*InfoReply, error) {
	host, _ := os.Hostname()
	return &InfoReply{
		GOOS:  runtime.GOOS,
		GoVer: runtime.Version(),
		Time:  time.Now().Format(time.RFC3339),
		Host:  host,
	}, nil
}

type Empty struct{}

// ---------------------------------------------------------------- 配置

type demoConfig struct {
	HTTP struct {
		Addr                 string   `yaml:"addr"`
		Transport            string   `yaml:"transport"`
		IdleTimeout          duration `yaml:"idleTimeout"`
		ReadHeaderTimeout    duration `yaml:"readHeaderTimeout"`
		WriteTimeout         duration `yaml:"writeTimeout"`
		EnableH2C            bool     `yaml:"enableH2C"`
		MaxConcurrentStreams uint32   `yaml:"maxConcurrentStreams"`
	}
	QUIC struct {
		Enable    bool     `yaml:"enable"`
		Addr      string   `yaml:"addr"`
		CertFile  string   `yaml:"certFile"`
		KeyFile   string   `yaml:"keyFile"`
		AutoHosts []string `yaml:"autoCertHosts"`
	}
}

type duration string

func (d duration) D() time.Duration {
	if d == "" {
		return 0
	}
	t, err := time.ParseDuration(string(d))
	if err != nil {
		return 0
	}
	return t
}

func loadConfig(path string) (*demoConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &demoConfig{}
	cfg.HTTP.Addr = "127.0.0.1:8080"
	cfg.HTTP.Transport = "auto"
	cfg.HTTP.IdleTimeout = "120s"
	cfg.HTTP.ReadHeaderTimeout = "10s"
	cfg.HTTP.WriteTimeout = "30s"
	cfg.QUIC.Enable = true
	cfg.QUIC.Addr = "127.0.0.1:8443"
	cfg.QUIC.AutoHosts = []string{"127.0.0.1", "localhost"}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// quicTLS 组装 QUIC TLS（证书文件优先，否则按 autoCertHosts 自签）。
func quicTLS(cfg *demoConfig) (*tls.Config, error) {
	if cfg.QUIC.CertFile != "" && cfg.QUIC.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.QUIC.CertFile, cfg.QUIC.KeyFile)
		if err != nil {
			return nil, err
		}
		return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
	}
	hosts := cfg.QUIC.AutoHosts
	if len(hosts) == 0 {
		hosts = []string{"127.0.0.1", "localhost"}
	}
	cert, err := h3.GenerateSelfSignedCert(hosts)
	if err != nil {
		return nil, err
	}
	log.Printf("已生成自签证书（hosts=%v，有效期 365 天）", hosts)
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

const indexHTML = `<!DOCTYPE html>
<html lang="zh">
<head><meta charset="utf-8"><title>framework demo</title></head>
<body style="font-family: system-ui; max-width: 720px; margin: 2rem auto">
<h1>framework 框架 demo</h1>
<p>HTTP/1.1（netpoll/std 传输层）+ WebSocket + JSON-RPC + QUIC/HTTP3。</p>
<ul>
<li>GET <a href="/hello/framework">/hello/:name</a></li>
<li>GET <a href="/api/health">/api/health</a></li>
<li>POST /api/echo（JSON）</li>
<li>GET <a href="/api/events">/api/events</a>（SSE 流式）</li>
<li>WS /ws/echo、WS /ws/chat、WS /ws/rpc</li>
<li>POST /rpc（JSON-RPC 2.0，支持批量）</li>
</ul>
</body></html>`
