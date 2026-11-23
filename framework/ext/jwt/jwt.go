// Package jwt 轻量 JWT（HS256/384/512）签发与校验，纯标准库实现。
//
// 与 golang-jwt 的差异：仅实现 HMAC 家族 + 常用 claims（覆盖服务间鉴权/
// 短期凭证场景）；RS/ES 非对称算法按需扩展。
//
// 用法：
//
//	signer := jwt.NewHS256([]byte("secret"), 15*time.Minute)
//	tok, _ := signer.Sign(jwt.Claims{"sub": "hardy", "role": "admin"})
//	srv.Use(jwt.Middleware(signer))   // Authorization: Bearer <tok>
//	claims := jwt.From(c)              // 已验证 claims
package jwt

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"hash"
	"net/http"
	"strings"
	"time"

	"github.com/ezscloud/searchboard/framework"
	"github.com/ezscloud/searchboard/framework/jsonx"
)

// Claims JWT 载荷。
type Claims map[string]any

// GetString 取字符串 claim。
func (cl Claims) GetString(key string) string {
	s, _ := cl[key].(string)
	return s
}

// GetNumber 取数值 claim。
func (cl Claims) GetNumber(key string) (float64, bool) {
	n, ok := cl[key].(float64)
	return n, ok
}

// Exp 过期时间（unix 秒；无 exp 返回 false）。
func (cl Claims) Exp() (int64, bool) {
	n, ok := cl.GetNumber("exp")
	return int64(n), ok
}

// 错误集合。
var (
	ErrMalformed    = errors.New("jwt: malformed token")
	ErrSignature    = errors.New("jwt: signature mismatch")
	ErrExpired      = errors.New("jwt: token expired")
	ErrNotYetValid  = errors.New("jwt: token not yet valid (nbf)")
	ErrBadAlgorithm = errors.New("jwt: algorithm mismatch")
)

// Signer 签发与校验器。
type Signer struct {
	alg    string
	ttl    time.Duration
	hashFn func() hash.Hash
	secret []byte
}

func newSigner(alg string, secret []byte, ttl time.Duration, hashFn func() hash.Hash) *Signer {
	return &Signer{alg: alg, ttl: ttl, hashFn: hashFn, secret: secret}
}

// NewHS256 创建 HS256 签发器。ttl 为默认寿命（自动补 iat/exp；0=不补）。
func NewHS256(secret []byte, ttl time.Duration) *Signer {
	return newSigner("HS256", secret, ttl, func() hash.Hash { return hmac.New(sha256.New, secret) })
}

// NewHS384 创建 HS384 签发器。
func NewHS384(secret []byte, ttl time.Duration) *Signer {
	return newSigner("HS384", secret, ttl, func() hash.Hash { return hmac.New(sha512.New384, secret) })
}

// NewHS512 创建 HS512 签发器。
func NewHS512(secret []byte, ttl time.Duration) *Signer {
	return newSigner("HS512", secret, ttl, func() hash.Hash { return hmac.New(sha512.New, secret) })
}

// TTL 设置默认签发寿命。
func (s *Signer) TTL(d time.Duration) *Signer { s.ttl = d; return s }

// Sign 签发 token。
func (s *Signer) Sign(claims Claims) (string, error) {
	now := time.Now().Unix()
	if claims == nil {
		claims = Claims{}
	}
	if _, has := claims["iat"]; !has {
		claims["iat"] = now
	}
	if _, has := claims["exp"]; !has && s.ttl > 0 {
		claims["exp"] = now + int64(s.ttl/time.Second)
	}
	header, err := jsonx.Marshal(map[string]string{"alg": s.alg, "typ": "JWT"})
	if err != nil {
		return "", err
	}
	body, err := jsonx.Marshal(claims)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(body)
	m := s.hashFn()
	_, _ = m.Write([]byte(signing))
	return signing + "." + enc.EncodeToString(m.Sum(nil)), nil
}

// Verify 校验 token（签名 + alg + exp/nbf）。
func (s *Signer) Verify(token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrMalformed
	}
	enc := base64.RawURLEncoding
	h, e1 := enc.DecodeString(parts[0])
	p, e2 := enc.DecodeString(parts[1])
	sig, e3 := enc.DecodeString(parts[2])
	if e1 != nil || e2 != nil || e3 != nil {
		return nil, ErrMalformed
	}
	var hdr map[string]string
	if err := jsonx.Unmarshal(h, &hdr); err != nil || hdr["typ"] != "JWT" {
		return nil, ErrMalformed
	}
	if hdr["alg"] != s.alg {
		return nil, ErrBadAlgorithm
	}
	m := s.hashFn()
	_, _ = m.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(sig, m.Sum(nil)) {
		return nil, ErrSignature
	}
	var claims Claims
	if err := jsonx.Unmarshal(p, &claims); err != nil {
		return nil, ErrMalformed
	}
	now := time.Now().Unix()
	if exp, ok := claims.Exp(); ok && now >= exp {
		return nil, ErrExpired
	}
	if nbf, ok := claims.GetNumber("nbf"); ok && float64(now) < nbf {
		return nil, ErrNotYetValid
	}
	return claims, nil
}

// Key 会话存取键。
const Key = "framework.claims"

// Middleware JWT 认证（Authorization: Bearer <token>）。
func Middleware(s *Signer) framework.Middleware {
	return func(c *framework.Context) {
		auth := c.Header("Authorization")
		const prefix = "Bearer "
		if len(auth) <= len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
			unauthorized(c)
			return
		}
		claims, err := s.Verify(strings.TrimSpace(auth[len(prefix):]))
		if err != nil {
			unauthorized(c)
			return
		}
		c.Set(Key, claims)
		c.Next()
	}
}

// From 取已验证 claims（未认证为 nil）。
func From(c *framework.Context) Claims {
	v, _ := c.Get(Key)
	cl, _ := v.(Claims)
	return cl
}

// Subject 取 sub 声明（便捷）。
func Subject(c *framework.Context) string {
	return From(c).GetString("sub")
}

func unauthorized(c *framework.Context) {
	c.SetRespHeader("WWW-Authenticate", `Bearer realm="webfw", error="invalid_token"`)
	c.Status(http.StatusUnauthorized)
	c.String(401, "401 Unauthorized")
	c.Abort()
}
