// Package es Elasticsearch / OpenSearch 管理客户端。
//
// 基于 framework.Client（池化连接 + keep-alive），提供：
//   - 原始透传：Do（任意 method/path/body）——响应原样返回，不解析；
//   - 便捷封装：Root / Health / ClusterStats / NodesStats / CatIndices / CatShards，
//     供总览页一次取数。
package es

import (
	"context"
	"crypto/tls"
	"strings"
	"time"

	"github.com/ezscloud/searchboard/framework"
)

// Client ES/OpenSearch 客户端。
type Client struct {
	http *framework.Client
	base string // 不含尾斜杠，如 http://127.0.0.1:9200

	username string
	password string
}

// New 创建客户端（insecureSkipVerify 用于自签 https 集群）。
func New(base, username, password string, insecureSkipVerify bool) *Client {
	c := framework.NewClient()
	c.Timeout = 30 * time.Second
	if insecureSkipVerify {
		c.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // 用户显式配置
	}
	return &Client{
		http:     c,
		base:     strings.TrimSuffix(base, "/"),
		username: username,
		password: password,
	}
}

// Response 一次 ES 请求的原始响应。
type Response struct {
	StatusCode int
	Body       []byte
}

// Do 执行原始请求。path 含查询串（如 "/_cat/indices?format=json"）。
func (c *Client) Do(ctx context.Context, method, path string, body []byte) (*Response, error) {
	req := framework.NewClientRequest(ctx, method, c.base+path, nil)
	if c.username != "" || c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	if len(body) > 0 {
		req.SetBodyBytes(body)
		req.SetHeader("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Close()
	b, err := resp.ReadAll()
	if err != nil {
		return nil, err
	}
	return &Response{StatusCode: resp.StatusCode, Body: b}, nil
}

// Ping 根请求 GET /（含集群名、版本号）。
func (c *Client) Ping(ctx context.Context) (*Response, error) {
	return c.Do(ctx, "GET", "/", nil)
}

// Health GET /_cluster/health。
func (c *Client) Health(ctx context.Context) (*Response, error) {
	return c.Do(ctx, "GET", "/_cluster/health", nil)
}

// ClusterStats GET /_cluster/stats。
func (c *Client) ClusterStats(ctx context.Context) (*Response, error) {
	return c.Do(ctx, "GET", "/_cluster/stats", nil)
}

// NodesStats GET /_nodes/stats（OS/JVM/线程池/磁盘/索引缓冲）。
func (c *Client) NodesStats(ctx context.Context) (*Response, error) {
	return c.Do(ctx, "GET", "/_nodes/stats", nil)
}

// CatIndices GET /_cat/indices?format=json&bytes=b（按字节返回存储量便于排序）。
func (c *Client) CatIndices(ctx context.Context) (*Response, error) {
	return c.Do(ctx, "GET", "/_cat/indices?format=json&bytes=b", nil)
}

// CatShards GET /_cat/shards?format=json&bytes=b。
func (c *Client) CatShards(ctx context.Context) (*Response, error) {
	return c.Do(ctx, "GET", "/_cat/shards?format=json&bytes=b", nil)
}
