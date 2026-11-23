// searchboard：Elasticsearch / OpenSearch 监控管理台。
//
// 定位：替代 cerebro / elasticvue 的轻量单二进制管理台，特色能力是
// 与 ES 节点同机部署时可直接实时查看节点日志（SSE）。
//
// 架构：webfw（自研 HTTP 框架，netpoll/std 双传输）+ VFS 分层前端
// （embed 基座 → public_dir 覆盖层）+ gzip 压缩。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ezscloud/searchboard/framework"
	"github.com/ezscloud/searchboard/framework/ext/gzip"
	"github.com/ezscloud/searchboard/framework/ext/static"
	"github.com/ezscloud/searchboard/framework/vfs"
	"github.com/ezscloud/searchboard/searchboard/internal/api"
	"github.com/ezscloud/searchboard/searchboard/internal/config"
	"github.com/ezscloud/searchboard/searchboard/internal/es"
	"github.com/ezscloud/searchboard/searchboard/internal/logtail"
)

// version 管理台版本。
const version = "0.1.0"

func main() {
	cfgPath := flag.String("config", "searchboard.yml", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("[searchboard] %v", err)
	}

	// ---- 组件装配 ----
	esClient := es.New(cfg.Elasticsearch.URL, cfg.Elasticsearch.Username,
		cfg.Elasticsearch.Password, cfg.Elasticsearch.InsecureSkipVerify)
	tailer := logtail.New(cfg.Logs.Dir, cfg.Logs.Pattern,
		cfg.Logs.PollInterval.D(), cfg.Logs.MaxFiles)
	brand := api.NewBrandState(api.Brand{
		Name:   cfg.Brand.Name,
		Slogan: cfg.Brand.Slogan,
		Logo:   cfg.Brand.Logo,
	})

	// VFS：embed 基座（底层）→ public_dir 覆盖层（顶层，参数顺序即优先级）
	layers := []vfs.Layer{vfs.EmbedLayer(webFS, "web")}
	if cfg.PublicDir != "" {
		if st, err := os.Stat(cfg.PublicDir); err == nil && st.IsDir() {
			layers = append(layers, vfs.DirLayer(cfg.PublicDir))
			log.Printf("[searchboard] 覆盖层已启用: %s", cfg.PublicDir)
		} else {
			log.Printf("[searchboard] 覆盖层目录不存在，跳过: %s", cfg.PublicDir)
		}
	}
	stack := vfs.NewStack(layers...)

	srv := framework.New(framework.Config{
		Addr:      cfg.Listen,
		Transport: cfg.Transport,
		Logf: func(f string, a ...any) {
			log.Printf("[framework] "+f, a...)
		},
	})

	srv.Use(gzip.Middleware(gzip.Config{}))
	api.Register(srv, api.Options{ES: esClient, Tail: tailer, Brand: brand, Version: version})

	// /public/* → 覆盖层静态资源（品牌 logo 等）；404 由前端回退默认图
	srv.GET("/public/*path", static.Handler(stack, static.Options{StripPrefix: "/public"}))

	// 其余路径 → SPA（embed 基座 index.html + 覆盖层优先）
	srv.GET("/", static.Handler(stack, static.Options{SPA: "index.html"}))

	log.Printf("[searchboard] v%s 监听 http://%s  ES=%s  日志目录=%s",
		version, cfg.Listen, cfg.Elasticsearch.URL, cfg.Logs.Dir)

	// ---- 优雅停机 ----
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		log.Println("[searchboard] 收到退出信号，优雅停机…")
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("[searchboard] 停机: %v", err)
		}
		os.Exit(0)
	}()

	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "[searchboard]", err)
		os.Exit(1)
	}
}
