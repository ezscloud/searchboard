package cluster

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// newMgr 在临时目录创建注册表。
func newMgr(t *testing.T, boot Cluster) *Manager {
	t.Helper()
	dir := t.TempDir()
	m, err := Open(filepath.Join(dir, "clusters.yml"), boot)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return m
}

func TestOpenBootstraps(t *testing.T) {
	m := newMgr(t, Cluster{Name: "local", URL: "http://127.0.0.1:9200"})
	if got := m.Current(); got != "local" {
		t.Fatalf("current=%q want local", got)
	}
	if _, err := os.Stat(m.Path()); err != nil {
		t.Fatalf("clusters.yml 未落盘: %v", err)
	}
	// 再开一次应读到已有文件
	m2, err := Open(m.Path(), Cluster{Name: "other", URL: "http://x"})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := m2.Names(); len(got) != 1 || got[0] != "local" {
		t.Fatalf("names=%v want [local]", got)
	}
}

func TestCRUD(t *testing.T) {
	m := newMgr(t, Cluster{Name: "local", URL: "http://127.0.0.1:9200"})

	if err := m.Add(Pick{Name: "prod", URL: "https://es.prod:9200/",
		Username: "u", Password: "p", Note: "生产集群"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := m.Add(Pick{Name: "prod", URL: "http://dup"}); err == nil {
		t.Fatal("重名应报错")
	}
	if err := m.Add(Pick{URL: "http://x"}); err == nil {
		t.Fatal("空名应报错")
	}
	if err := m.Add(Pick{Name: "x"}); err == nil {
		t.Fatal("空 URL 应报错")
	}

	// URL 尾斜杠规范化
	c := m.Get("prod")
	if c.URL != "https://es.prod:9200" {
		t.Fatalf("url=%q 尾斜杠未规范化", c.URL)
	}

	// Update：密码空=保持；改 URL 后 client 缓存应失效
	if err := m.Update("prod", Pick{URL: "https://es.prod2:9200", Username: "u"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := m.Get("prod").Password; got != "p" {
		t.Fatalf("password=%q 应保持原值", got)
	}
	if got := m.Get("prod").URL; got != "https://es.prod2:9200" {
		t.Fatalf("url=%q", got)
	}

	// SetCurrent
	if err := m.SetCurrent("prod"); err != nil {
		t.Fatalf("SetCurrent: %v", err)
	}
	if got := m.Current(); got != "prod" {
		t.Fatalf("current=%q want prod", got)
	}
	if err := m.SetCurrent("nope"); err == nil {
		t.Fatal("切换不存在的集群应报错")
	}

	// Remove 当前集群 → 回落首个
	if err := m.Remove("prod"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := m.Current(); got != "local" {
		t.Fatalf("current=%q want local（回落）", got)
	}
	// 至少保留一个
	if err := m.Remove("local"); err == nil {
		t.Fatal("删最后一个集群应报错")
	}

	// 持久化 round-trip：重开文件数据一致
	m2, err := Open(m.Path(), Cluster{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := m2.Names(); len(got) != 1 || got[0] != "local" {
		t.Fatalf("names=%v want [local]", got)
	}
}

func TestListMasked(t *testing.T) {
	m := newMgr(t, Cluster{Name: "local", URL: "http://127.0.0.1:9200"})
	_ = m.Add(Pick{Name: "prod", URL: "http://p:9200", Password: "secret"})
	vs := m.List()
	var prod View
	for _, v := range vs {
		if v.Name == "prod" {
			prod = v
		}
	}
	if !prod.HasPassword {
		t.Fatal("HasPassword 应为 true")
	}
	b, _ := json.Marshal(prod)
	if strings.Contains(string(b), "secret") {
		t.Fatalf("序列化泄露密码: %s", b)
	}
	if strings.Contains(string(b), `"password"`) {
		t.Fatalf("序列化应无 password 字段: %s", b)
	}
}

func TestPingAndTest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"node-x","cluster_name":"c1","cluster_uuid":"u1","version":{"number":"8.13.0"}}`))
	}))
	defer srv.Close()

	// Test（无落盘）
	info, err := Test(context.Background(), Pick{URL: srv.URL})
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if info["ok"] != true || info["cluster_name"] != "c1" {
		t.Fatalf("info=%v", info)
	}

	// Manager.Ping（当前集群）
	m := newMgr(t, Cluster{Name: "local", URL: srv.URL})
	info2, err := m.Ping(context.Background(), "")
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if info2["ok"] != true {
		t.Fatalf("ping info=%v", info2)
	}

	// 不存在的集群
	if _, err := m.Ping(context.Background(), "nope"); err == nil {
		t.Fatal("Ping 不存在的集群应报错")
	}
}

func TestConcurrentAccess(t *testing.T) {
	m := newMgr(t, Cluster{Name: "local", URL: "http://127.0.0.1:9200"})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = m.List()
			_ = m.Current()
			_ = m.Names()
			_, _ = m.Client("")
		}(i)
	}
	wg.Wait()
}
