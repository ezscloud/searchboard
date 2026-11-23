// client_pool.go：HTTP 客户端连接池（keep-alive 复用 + 空闲回收）。
//
// 池键 = scheme://host:port（http 与 https 分池）。复用前置条件见
// Response.reusable：体读尽 + 服务端未声明 close + 非 EOF 界定。
package framework

import (
	"bufio"
	"sync"
	"time"
)

// connPool 按 host 的空闲连接池。
type connPool struct {
	mu     sync.Mutex
	idle   map[string][]*pooledConn // host 键 → 空闲连接栈
	caps   map[string]int           // 每 host 空闲上限
	closed bool

	maxPerHost int
	idleTTL    time.Duration
	sweeper    chan struct{}
}

// newConnPool 创建池并启动后台清扫（idleTTL 过期关闭）。
func newConnPool(maxPerHost int, idleTTL time.Duration) *connPool {
	if maxPerHost <= 0 {
		maxPerHost = 4
	}
	if idleTTL <= 0 {
		idleTTL = 90 * time.Second
	}
	p := &connPool{
		idle:       map[string][]*pooledConn{},
		caps:       map[string]int{},
		maxPerHost: maxPerHost,
		idleTTL:    idleTTL,
		sweeper:    make(chan struct{}),
	}
	go p.sweepLoop(idleTTL / 3)
	return p
}

// acquire 取空闲连接；无则返回 nil（由调用方拨号）。
func (p *connPool) acquire(key string) *pooledConn {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.idle[key]) > 0 {
		pc := p.idle[key][len(p.idle[key])-1]
		p.idle[key] = p.idle[key][:len(p.idle[key])-1]
		if time.Since(pc.at) < p.idleTTL {
			return pc
		}
		// 过期：直接关闭换下一个
		_ = pc.conn.Close()
	}
	return nil
}

// release 归还连接（reusable=false 时关闭；超容量关闭最旧）。
func (p *connPool) release(pc *pooledConn, reusable bool) {
	if pc == nil {
		return
	}
	if !reusable {
		_ = pc.conn.Close()
		return
	}
	pc.at = time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		_ = pc.conn.Close()
		return
	}
	p.idle[pc.key] = append(p.idle[pc.key], pc)
	// 超容量：关最旧（栈底）
	for len(p.idle[pc.key]) > p.maxPerHost {
		oldest := p.idle[pc.key][0]
		p.idle[pc.key] = p.idle[pc.key][1:]
		_ = oldest.conn.Close()
	}
}

// sweepLoop 后台清扫过期空闲连接。
func (p *connPool) sweepLoop(interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-p.sweeper:
			return
		case <-t.C:
			now := time.Now()
			p.mu.Lock()
			for key, list := range p.idle {
				keep := list[:0]
				for _, pc := range list {
					if now.Sub(pc.at) < p.idleTTL {
						keep = append(keep, pc)
					} else {
						_ = pc.conn.Close()
					}
				}
				if len(keep) == 0 {
					delete(p.idle, key)
				} else {
					p.idle[key] = keep
				}
			}
			p.mu.Unlock()
		}
	}
}

// ClosePool 关闭池（清空全部空闲连接）。
func (p *connPool) ClosePool() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	close(p.sweeper)
	for _, list := range p.idle {
		for _, pc := range list {
			_ = pc.conn.Close()
		}
	}
	p.idle = map[string][]*pooledConn{}
}

// poolOf Client 惰性取池。
func (c *Client) poolOf() *connPool {
	if c.DisablePool {
		return nil
	}
	c.initPoolOnce.Do(func() {
		c.pool = newConnPool(c.MaxIdleConnsPerHost, c.IdleTimeout)
	})
	return c.pool
}

// CloseIdleConnections 关闭全部空闲连接（对标 http.Transport 同名方法）。
func (c *Client) CloseIdleConnections() {
	if p := c.poolOf(); p != nil {
		p.ClosePool()
		// 允许再次使用：重建
		c.initPoolOnce = sync.Once{}
	}
}

// wrapPooledRead 把池化连接读包装为 *bufio.Reader（沿用旧缓冲状态）。
func (pc *pooledConn) reader() *bufio.Reader {
	if pc.br == nil {
		pc.br = bufio.NewReaderSize(pc.conn, 4096)
	}
	return pc.br
}
