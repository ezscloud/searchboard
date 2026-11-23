// Package cache HTTP 接口缓存中间件。
//
// key 可定制（默认 method+path+query）；Store 可插拔（内置带 TTL 的内存
// LRU）。命中时直接回放缓存（含状态码与响应头），并以 X-Cache 头标注。
package cache

import (
	"container/list"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ezscloud/searchboard/framework"
)

// KeyFunc 缓存键生成。
type KeyFunc func(c *framework.Context) string

// Store 缓存存储接口。
type Store interface {
	Get(key string) (*Entry, bool)
	Set(key string, e *Entry, ttl time.Duration)
	Delete(key string)
	Len() int
}

// Entry 缓存条目。
type Entry struct {
	Status int
	Header http.Header
	Body   []byte
}

// Config 缓存配置。
type Config struct {
	// TTL 默认缓存寿命（默认 30s）。
	TTL time.Duration

	// Key 缓存键（默认 method+path+"?"+query）。
	Key KeyFunc

	// Store 存储（默认 NewMemoryStore(4096)）。
	Store Store

	// OnHit / OnMiss 观测回调。
	OnHit  func(c *framework.Context)
	OnMiss func(c *framework.Context)

	// Excluded 跳过缓存判定的方法（默认 POST/PUT/DELETE/PATCH 不缓存）。
	IsCacheable func(method string) bool
}

func (c *Config) withDefaults() {
	if c.TTL <= 0 {
		c.TTL = 30 * time.Second
	}
	if c.Key == nil {
		c.Key = DefaultKey
	}
	if c.Store == nil {
		c.Store = NewMemoryStore(4096)
	}
	if c.IsCacheable == nil {
		c.IsCacheable = func(m string) bool {
			return m == http.MethodGet || m == http.MethodHead
		}
	}
}

// DefaultKey 默认缓存键：METHOD PATH?QUERY。
func DefaultKey(c *framework.Context) string {
	q := c.Request.RawQuery
	if q == "" {
		return c.Request.Method + " " + c.Request.Path
	}
	return c.Request.Method + " " + c.Request.Path + "?" + q
}

// Middleware 缓存中间件。
func Middleware(cfg Config) framework.Middleware {
	cfg.withDefaults()
	return func(c *framework.Context) {
		if !cfg.IsCacheable(c.Request.Method) {
			c.Next()
			return
		}
		key := cfg.Key(c)
		if e, ok := cfg.Store.Get(key); ok {
			replay(c, e)
			c.SetRespHeader("X-Cache", "HIT")
			c.Abort() // 命中即止：阻断后续链（含路由处理器），防止重复写出
			if cfg.OnHit != nil {
				cfg.OnHit(c)
			}
			return
		}
		if cfg.OnMiss != nil {
			cfg.OnMiss(c)
		}
		c.Set("framework.cache.key", key)
		c.Next()

		// 捕获缓冲窗口内响应入缓存
		body := c.ResponseSnapshot()
		if body == nil {
			return // 流式响应不缓存
		}
		cfg.Store.Set(key, &Entry{
			Status: c.CurrentStatus(),
			Header: cloneHeader(c.RespHeader()),
			Body:   append([]byte(nil), body...),
		}, cfg.TTL)
	}
}

func replay(c *framework.Context, e *Entry) {
	for k, vs := range e.Header {
		for _, v := range vs {
			c.RespHeader().Add(k, v)
		}
	}
	c.SetRespHeader("Content-Length", strconv.Itoa(len(e.Body)))
	c.Status(e.Status)
	_, _ = c.Write(e.Body)
}

func cloneHeader(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vs := range h {
		out[k] = append([]string(nil), vs...)
	}
	return out
}

// Purge 主动失效（处理器内更新数据后调用）。
func Purge(c *framework.Context, store Store) {
	v, _ := c.Get("framework.cache.key")
	key, _ := v.(string)
	if key != "" {
		store.Delete(key)
	}
}

// ---------------------------------------------------------------- MemoryStore

// MemoryStore 带 TTL 的 LRU 内存缓存。
type MemoryStore struct {
	mu       sync.Mutex
	capacity int
	ll       *list.List
	items    map[string]*list.Element
}

type lruEntry struct {
	key     string
	entry   *Entry
	expires time.Time
}

// NewMemoryStore 创建 LRU 存储（capacity ≤0 → 4096）。
func NewMemoryStore(capacity int) *MemoryStore {
	if capacity <= 0 {
		capacity = 4096
	}
	return &MemoryStore{capacity: capacity, ll: list.New(), items: map[string]*list.Element{}}
}

// Get implements Store。
func (m *MemoryStore) Get(key string) (*Entry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	el, ok := m.items[key]
	if !ok {
		return nil, false
	}
	e := el.Value.(*lruEntry)
	if time.Now().After(e.expires) {
		m.ll.Remove(el)
		delete(m.items, key)
		return nil, false
	}
	m.ll.MoveToFront(el)
	return e.entry, true
}

// Set implements Store。
func (m *MemoryStore) Set(key string, entry *Entry, ttl time.Duration) {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.items[key]; ok {
		e := el.Value.(*lruEntry)
		e.entry = entry
		e.expires = time.Now().Add(ttl)
		m.ll.MoveToFront(el)
		return
	}
	el := m.ll.PushFront(&lruEntry{key: key, entry: entry, expires: time.Now().Add(ttl)})
	m.items[key] = el
	for m.ll.Len() > m.capacity {
		oldest := m.ll.Back()
		if oldest == nil {
			break
		}
		m.ll.Remove(oldest)
		delete(m.items, oldest.Value.(*lruEntry).key)
	}
}

// Delete implements Store。
func (m *MemoryStore) Delete(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.items[key]; ok {
		m.ll.Remove(el)
		delete(m.items, key)
	}
}

// Len implements Store。
func (m *MemoryStore) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ll.Len()
}

var _ = strings.TrimSpace
