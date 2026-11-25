// Package cluster 多集群注册表：YAML 持久化、按名缓存 es.Client、运行时 CRUD。
//
// 存储文件（默认 clusters.yml，与运行目录同级）：
//
//	current: local
//	clusters:
//	  - name: local
//	    url: http://127.0.0.1:9200
//	    username: ""
//	    password: ""            # 明文落盘：本机管理台定位；暴露公网请前置反向代理
//	    insecure_skip_verify: false
//	    timeout_ms: 30000
//	    note: 本机开发
//
// 首次启动时若文件不存在，用启动配置（searchboard.yml 的 elasticsearch 段）
// 生成名为 local 的默认集群并立即落盘。
package cluster

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ezscloud/searchboard/framework/jsonx"
	"github.com/ezscloud/searchboard/internal/es"
)

// Cluster 单个 ES/OpenSearch 集群连接信息（YAML 持久化字段）。
type Cluster struct {
	Name               string `yaml:"name" json:"name"`
	URL                string `yaml:"url" json:"url"`
	Username           string `yaml:"username" json:"username"`
	Password           string `yaml:"password" json:"-"` // 仅落盘；API 输出走 View 脱敏
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify" json:"insecure_skip_verify"`
	TimeoutMS          int    `yaml:"timeout_ms" json:"timeout_ms"` // 0=30000
	Note               string `yaml:"note" json:"note"`             // 备注（环境/用途）
}

// View 对外视图：密码不返回，另用 HasPassword 标记是否设置过。
type View struct {
	Cluster
	HasPassword bool `json:"has_password"`
	Current     bool `json:"current"`
}

// Pick CRUD 入参（Update 时 Password 空=保持原值）。
type Pick struct {
	Name               string `json:"name"`
	URL                string `json:"url"`
	Username           string `json:"username"`
	Password           string `json:"password"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify"`
	TimeoutMS          int    `json:"timeout_ms"`
	Note               string `json:"note"`
}

// store 文件结构。
type store struct {
	Current  string     `yaml:"current"`
	Clusters []*Cluster `yaml:"clusters"`
}

// Manager 多集群注册表（并发安全）。
type Manager struct {
	mu       sync.RWMutex
	path     string
	clusters []*Cluster
	current  string
	clients  map[string]*es.Client // 按名缓存（连接参数变更时失效）
}

// Open 加载（或初始化）集群注册表。boot 为 searchboard.yml 的启动连接，
// 首次运行（文件不存在）时据此生成名为 local 的默认集群并落盘。
func Open(path string, boot Cluster) (*Manager, error) {
	m := &Manager{path: path, clients: map[string]*es.Client{}}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		var st store
		if err := yaml.Unmarshal(b, &st); err != nil {
			return nil, fmt.Errorf("cluster: parse %s: %w", path, err)
		}
		if len(st.Clusters) == 0 {
			return nil, fmt.Errorf("cluster: %s 无集群定义", path)
		}
		m.clusters = st.Clusters
		m.current = st.Current
		if m.byNameLocked(m.current) == nil {
			m.current = m.clusters[0].Name
			if err := m.saveLocked(); err != nil {
				return nil, err
			}
		}
	case errors.Is(err, os.ErrNotExist):
		if boot.Name == "" {
			boot.Name = "local"
		}
		if boot.URL == "" {
			boot.URL = "http://127.0.0.1:9200"
		}
		bc := boot
		m.clusters = []*Cluster{&bc}
		m.current = bc.Name
		if err := m.saveLocked(); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("cluster: read %s: %w", path, err)
	}
	for _, c := range m.clusters {
		c.URL = strings.TrimSuffix(strings.TrimSpace(c.URL), "/")
		c.Name = strings.TrimSpace(c.Name)
	}
	return m, nil
}

// Path 存储文件路径（供界面展示）。
func (m *Manager) Path() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.path
}

// Current 当前集群名。
func (m *Manager) Current() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

// Get 按名取集群（副本，避免外部改内部状态）；name 空=当前；不存在返回 nil。
func (m *Manager) Get(name string) *Cluster {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if name == "" {
		name = m.current
	}
	if c := m.byNameLocked(name); c != nil {
		cp := *c
		return &cp
	}
	return nil
}

// List 集群列表（脱敏视图，按名称排序）。
func (m *Manager) List() []View {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]View, 0, len(m.clusters))
	for _, c := range m.clusters {
		out = append(out, View{Cluster: *c, HasPassword: c.Password != "", Current: c.Name == m.current})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Names 集群名列表。
func (m *Manager) Names() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.clusters))
	for _, c := range m.clusters {
		out = append(out, c.Name)
	}
	return out
}

// Add 新增集群（名字唯一、URL 必填）。
func (m *Manager) Add(p Pick) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	name := strings.TrimSpace(p.Name)
	url := strings.TrimSuffix(strings.TrimSpace(p.URL), "/")
	if name == "" {
		return errors.New("集群名称不能为空")
	}
	if url == "" {
		return errors.New("集群地址不能为空")
	}
	if m.byNameLocked(name) != nil {
		return fmt.Errorf("集群 %q 已存在", name)
	}
	m.clusters = append(m.clusters, &Cluster{
		Name: name, URL: url, Username: strings.TrimSpace(p.Username),
		Password: p.Password, InsecureSkipVerify: p.InsecureSkipVerify,
		TimeoutMS: p.TimeoutMS, Note: strings.TrimSpace(p.Note),
	})
	return m.saveLocked()
}

// Update 更新连接信息（名称不可改；密码空=保持原值）。
func (m *Manager) Update(name string, p Pick) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.byNameLocked(name)
	if c == nil {
		return fmt.Errorf("集群 %q 不存在", name)
	}
	url := strings.TrimSuffix(strings.TrimSpace(p.URL), "/")
	if url == "" {
		return errors.New("集群地址不能为空")
	}
	c.URL = url
	c.Username = strings.TrimSpace(p.Username)
	if p.Password != "" {
		c.Password = p.Password
	}
	c.InsecureSkipVerify = p.InsecureSkipVerify
	c.TimeoutMS = p.TimeoutMS
	c.Note = strings.TrimSpace(p.Note)
	delete(m.clients, name) // 连接参数变化，缓存失效
	return m.saveLocked()
}

// Remove 删除集群（至少保留一个；删除当前集群时回落首个）。
func (m *Manager) Remove(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.clusters) <= 1 {
		return errors.New("至少保留一个集群")
	}
	idx := -1
	for i, c := range m.clusters {
		if c.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("集群 %q 不存在", name)
	}
	m.clusters = append(m.clusters[:idx], m.clusters[idx+1:]...)
	delete(m.clients, name)
	if m.current == name {
		m.current = m.clusters[0].Name
	}
	return m.saveLocked()
}

// SetCurrent 切换当前集群。
func (m *Manager) SetCurrent(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byNameLocked(name) == nil {
		return fmt.Errorf("集群 %q 不存在", name)
	}
	m.current = name
	return m.saveLocked()
}

// Client 按名取（缓存）es.Client；name 空=当前集群。
func (m *Manager) Client(name string) (*es.Client, error) {
	m.mu.RLock()
	if name == "" {
		name = m.current
	}
	c := m.byNameLocked(name)
	m.mu.RUnlock()
	if c == nil {
		return nil, fmt.Errorf("集群 %q 不存在", name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if c2 := m.byNameLocked(name); c2 != nil { // 读锁释放后可能被并发更新
		c = c2
	}
	if cl, ok := m.clients[name]; ok {
		return cl, nil
	}
	cl := es.NewWithTimeout(c.URL, c.Username, c.Password, c.InsecureSkipVerify,
		time.Duration(c.TimeoutMS)*time.Millisecond)
	m.clients[name] = cl
	return cl, nil
}

// Ping 探测指定集群（name 空=当前），返回概要信息。
func (m *Manager) Ping(ctx context.Context, name string) (map[string]any, error) {
	cl, err := m.Client(name)
	if err != nil {
		return nil, err
	}
	return pingClient(ctx, cl)
}

// Test 临时探测（不落盘、不缓存），用于新增/编辑弹窗的连接测试。
func Test(ctx context.Context, p Pick) (map[string]any, error) {
	url := strings.TrimSuffix(strings.TrimSpace(p.URL), "/")
	if url == "" {
		return nil, errors.New("集群地址不能为空")
	}
	timeout := time.Duration(p.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	cl := es.NewWithTimeout(url, strings.TrimSpace(p.Username), p.Password,
		p.InsecureSkipVerify, timeout)
	return pingClient(ctx, cl)
}

// pingClient 执行 GET / 并组装概要（与真实 ES/OpenSearch 一致）。
func pingClient(ctx context.Context, cl *es.Client) (map[string]any, error) {
	t0 := time.Now()
	r, err := cl.Ping(ctx)
	if err != nil {
		return nil, err
	}
	ms := time.Since(t0).Milliseconds()
	if r.StatusCode != 200 {
		body := r.Body
		if len(body) > 400 {
			body = body[:400]
		}
		return map[string]any{"ok": false, "status": r.StatusCode, "ms": ms,
			"error": string(body)}, nil
	}
	var root map[string]any
	if jsonx.Unmarshal(r.Body, &root) != nil {
		return map[string]any{"ok": false, "status": r.StatusCode, "ms": ms,
			"error": "响应不是合法 JSON"}, nil
	}
	// 版本取 version.number（字符串），避免整个 version 对象直接下发界面
	ver := ""
	if vm, ok := root["version"].(map[string]any); ok {
		if vn, ok := vm["number"].(string); ok {
			ver = vn
		}
	}
	return map[string]any{
		"ok":           true,
		"status":       r.StatusCode,
		"ms":           ms,
		"name":         root["name"],
		"cluster_name": root["cluster_name"],
		"uuid":         root["cluster_uuid"],
		"version":      ver,
	}, nil
}

// saveLocked 原子落盘（调用方须持写锁）。
func (m *Manager) saveLocked() error {
	b, err := yaml.Marshal(store{Current: m.current, Clusters: m.clusters})
	if err != nil {
		return fmt.Errorf("cluster: marshal: %w", err)
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("cluster: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, m.path); err != nil {
		return fmt.Errorf("cluster: rename %s: %w", m.path, err)
	}
	return nil
}

func (m *Manager) byNameLocked(name string) *Cluster {
	for _, c := range m.clusters {
		if c.Name == name {
			return c
		}
	}
	return nil
}
