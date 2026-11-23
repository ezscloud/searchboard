// Package session 提供 Cookie Session 中间件（多存储后端可插拔）。
//
// 内置 MemoryStore（TTL 清扫）；自定义实现 Store 接口即可接 Redis 等。
//
// 用法：
//
//	store := session.NewMemoryStore(30 * time.Minute)
//	srv.Use(session.Middleware(store, "mysite", func(cfg *session.Config) {
//	    cfg.MaxAge = 30 * time.Minute
//	}))
//	// 处理器内：
//	s := session.From(c)
//	s.Set("uid", 42)
//	if v, ok := s.Get("uid"); ok { ... }
//	s.Save(c)   // 回写（重登录等敏感变更时立即下发）
package session

import (
	"crypto/rand"
	"encoding/base64"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ezscloud/searchboard/framework"
)

// Store 会话存储接口。
type Store interface {
	// Load 按 ID 取会话（不存在返回 nil）。
	Load(id string) *Data
	// Save 保存会话（含 TTL）。
	Save(id string, d *Data, ttl time.Duration)
	// Delete 删除会话。
	Delete(id string)
}

// Data 会话数据。
type Data struct {
	Values map[string]any
}

// Get 取值。
func (d *Data) Get(key string) (any, bool) {
	if d == nil {
		return nil, false
	}
	v, ok := d.Values[key]
	return v, ok
}

// Set 存值。
func (d *Data) Set(key string, v any) {
	if d.Values == nil {
		d.Values = map[string]any{}
	}
	d.Values[key] = v
}

// GetString 便捷取字符串。
func (d *Data) GetString(key string) string {
	v, ok := d.Get(key)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// Config Session 配置。
type Config struct {
	Name   string        // cookie 名，默认 "framework_session"
	MaxAge time.Duration // 服务端 TTL 与 cookie Max-Age，默认 30m
	Path   string        // cookie Path，默认 "/"
}

func (c *Config) withDefaults() {
	if c.Name == "" {
		c.Name = "framework_session"
	}
	if c.MaxAge <= 0 {
		c.MaxAge = 30 * time.Minute
	}
	if c.Path == "" {
		c.Path = "/"
	}
}

// Session 当前请求的会话视图。
type Session struct {
	ID        string
	Data      *Data
	store     Store
	cfg       Config
	dirty     bool
	newIssued bool // 本次请求新建（需下发 cookie）
}

// Set 存值并标记脏（中间件收尾自动保存）。
func (s *Session) Set(key string, v any) {
	s.Data.Set(key, v)
	s.dirty = true
}

// Get 取值。
func (s *Session) Get(key string) (any, bool) { return s.Data.Get(key) }

// GetString 便捷取字符串。
func (s *Session) GetString(key string) string { return s.Data.GetString(key) }

// DeleteValue 删单值并标记脏。
func (s *Session) DeleteValue(key string) {
	if s.Data != nil {
		delete(s.Data.Values, key)
		s.dirty = true
	}
}

// Destroy 作废会话（下发过期 cookie + 删存储）。
func (s *Session) Destroy(c *framework.Context) {
	s.store.Delete(s.ID)
	c.RespHeader().Add("Set-Cookie", expiredCookie(s.cfg.Name, s.cfg.Path))
	s.Data = &Data{Values: map[string]any{}}
	s.dirty = false
	s.newIssued = false
}

// Save 立即保存并（若新建）下发 cookie。
func (s *Session) Save(c *framework.Context) {
	s.store.Save(s.ID, s.Data, s.cfg.MaxAge)
	if s.newIssued {
		c.RespHeader().Add("Set-Cookie", sessionCookie(s.cfg, s.ID))
		s.newIssued = false
	}
	s.dirty = false
}

// Middleware Session 中间件。
//
// name 为 cookie/存储命名空间；mutate 可选（调整 MaxAge 等）。
func Middleware(store Store, name string, mutate func(*Config)) framework.Middleware {
	cfg := Config{Name: name}
	if mutate != nil {
		mutate(&cfg)
	}
	cfg.withDefaults()
	return func(c *framework.Context) {
		sid := cookieOf(c, cfg.Name)
		var data *Data
		if sid != "" {
			data = store.Load(sid)
		}
		var fresh bool
		if data == nil {
			sid = newID()
			data = &Data{Values: map[string]any{}}
			fresh = true
		}
		s := &Session{ID: sid, Data: data, store: store, cfg: cfg, newIssued: fresh}
		c.Set(Key, s)
		c.Next()
		if s.dirty {
			s.Save(c)
		}
	}
}

// Key 会话存取键。
const Key = "framework.session"

// From 取当前会话（未装配中间件返回 nil）。
func From(c *framework.Context) *Session {
	v, _ := c.Get(Key)
	s, _ := v.(*Session)
	return s
}

func cookieOf(c *framework.Context, name string) string {
	for _, line := range c.Request.Header.Values("Cookie") {
		for _, part := range strings.Split(line, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
			if ok && k == name {
				return v
			}
		}
	}
	return ""
}

func newID() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// sessionCookie 组装下发 cookie 串（HttpOnly + SameSite=Lax）。
func sessionCookie(cfg Config, id string) string {
	var b strings.Builder
	b.WriteString(cfg.Name + "=" + id)
	b.WriteString("; Path=" + cfg.Path)
	b.WriteString("; Max-Age=" + strconv.FormatInt(int64(cfg.MaxAge/time.Second), 10))
	b.WriteString("; HttpOnly; SameSite=Lax")
	return b.String()
}

// expiredCookie 过期清除 cookie 串。
func expiredCookie(name, path string) string {
	var b strings.Builder
	b.WriteString(name + "=; Path=" + path)
	b.WriteString("; Max-Age=0; Expires=Thu, 01 Jan 1970 00:00:00 GMT")
	return b.String()
}

// ---------------------------------------------------------------- MemoryStore

// MemoryStore 内存会话存储（TTL 惰性清扫 + 定时兜底）。
type MemoryStore struct {
	mu  sync.RWMutex
	m   map[string]memEntry
	ttl time.Duration
}

type memEntry struct {
	data    *Data
	expires time.Time
}

// NewMemoryStore 创建内存存储。
//
// ttl 为默认会话寿命；sweep 间隔为 ttl/4（≥1s）。
func NewMemoryStore(ttl time.Duration) *MemoryStore {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	s := &MemoryStore{m: map[string]memEntry{}, ttl: ttl}
	interval := ttl / 4
	if interval < time.Second {
		interval = time.Second
	}
	go s.sweepLoop(interval)
	return s
}

// Load implements Store。
func (s *MemoryStore) Load(id string) *Data {
	s.mu.RLock()
	e, ok := s.m[id]
	s.mu.RUnlock()
	if !ok || time.Now().After(e.expires) {
		return nil
	}
	return e.data
}

// Save implements Store。
func (s *MemoryStore) Save(id string, d *Data, ttl time.Duration) {
	if ttl <= 0 {
		ttl = s.ttl
	}
	s.mu.Lock()
	s.m[id] = memEntry{data: d, expires: time.Now().Add(ttl)}
	s.mu.Unlock()
}

// Delete implements Store。
func (s *MemoryStore) Delete(id string) {
	s.mu.Lock()
	delete(s.m, id)
	s.mu.Unlock()
}

// Len 存活的会话数（近似）。
func (s *MemoryStore) Len() int {
	s.mu.RLock()
	n := len(s.m)
	s.mu.RUnlock()
	return n
}

func (s *MemoryStore) sweepLoop(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		s.mu.Lock()
		for k, e := range s.m {
			if now.After(e.expires) {
				delete(s.m, k)
			}
		}
		s.mu.Unlock()
	}
}
