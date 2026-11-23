// autocert.go：keystore → x/crypto/autocert.Cache 适配器。
//
// autotls（ext/autotls）默认用 autocert.DirCache 落盘；本适配器让证书与
// 账户密钥统一进入 keystore（FileStore 或任何自定义 Store，如后续接入
// 加密卷/远端存储）。
//
// autocert.Cache 接口仅三个方法（Get/Put/Delete，data 为证书 DER 或
// 账户私钥 PEM），映射到 Entry.Data 单字段。
package keystore

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/acme/autocert"
)

// AutocertCache keystore → autocert.Cache 适配器。
//
// autocert 的 cache key 可能含 '/'（目录分隔）与 '*'（通配域名），
// 经 safeName 归一化后入库（非安全字符集统一替换为 '_'）。
//
// 用法：
//
//	st, _ := keystore.NewFileStore("/data/keystore")
//	m := autocert.Manager{Cache: keystore.AutocertCache{Store: st}, ...}
type AutocertCache struct {
	Store Store
}

// safeName 将 autocert key 归一化为 keystore 合法条目名。
// 注意：不同原 key 归一化后可能碰撞（如 "a/b" 与 "a_b"）；
// autocert 的 key 域（域名/acme_account+key）内无实际碰撞风险。
func safeName(name string) string {
	const okSet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._+-"
	var b strings.Builder
	for _, r := range name {
		if strings.ContainsRune(okSet, r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// Get 实现 autocert.Cache。
func (c AutocertCache) Get(ctx context.Context, name string) ([]byte, error) {
	e, err := c.Store.Get(ctx, safeName(name))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, autocert.ErrCacheMiss // autocert 依赖此哨兵判定 miss
		}
		return nil, fmt.Errorf("keystore/autocert: get %q: %w", name, err)
	}
	return e.Data, nil
}

// Put 实现 autocert.Cache。
func (c AutocertCache) Put(ctx context.Context, name string, data []byte) error {
	if err := c.Store.Put(ctx, safeName(name), Entry{Data: data}); err != nil {
		return fmt.Errorf("keystore/autocert: put %q: %w", name, err)
	}
	return nil
}

// Delete 实现 autocert.Cache。
func (c AutocertCache) Delete(ctx context.Context, name string) error {
	if err := c.Store.Delete(ctx, safeName(name)); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil // autocert.Delete 对 miss 容忍
		}
		return fmt.Errorf("keystore/autocert: delete %q: %w", name, err)
	}
	return nil
}

// 编译期接口断言。
var _ autocert.Cache = AutocertCache{}
