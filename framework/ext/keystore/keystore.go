// Package keystore 提供框架级密钥/证书存储抽象。
//
// 设计目标：零第三方依赖（纯标准库），为 TLS 证书、ACME 账户密钥、
// API token 等敏感条目提供统一的命名存取接口。
//
// 两种实现：
//   - MemStore：内存（进程内缓存 / 测试）
//   - FileStore：目录落盘（单文件原子写：tmp + rename；权限 0600）
//
// 另提供 AutocertCache 适配器，可直接接入 ext/autotls（x/crypto/autocert）。
//
// 安全边界：条目不做应用层加密——私钥落盘依赖文件系统权限（0600）与
// 部署环境管控；需要静态加密时应先在调用侧用 KMS/HSM 加密 Data 字段。
package keystore

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// ErrNotFound 条目不存在（Get 返回；调用方可 errors.Is 判定）。
var ErrNotFound = errors.New("keystore: entry not found")

// Entry 一条密钥/证书条目。
type Entry struct {
	CertPEM []byte            // 证书链 PEM（可选；TLS 证书条目用）
	KeyPEM  []byte            // 私钥 PEM（可选；与 CertPEM 配对）
	Data    []byte            // 任意二进制数据（ACME 账户密钥 / API token / 证书 DER 等）
	Meta    map[string]string // 自由元数据（如域名、签发时间、来源）
}

// Store 命名条目存储接口。所有实现必须并发安全。
type Store interface {
	Put(ctx context.Context, name string, e Entry) error
	Get(ctx context.Context, name string) (Entry, error)
	Delete(ctx context.Context, name string) error
	List(ctx context.Context) ([]string, error)
}

// nameRe 条目名字符集：字母数字、点、下划线、中划线、加号
// （autocert 账户密钥名含 +）；首字符不允许点，含 ".." 由 ValidateName 显式拒绝。
var nameRe = regexp.MustCompile(`^[A-Za-z0-9_+\-][A-Za-z0-9._+\-]{0,127}$`)

// ValidateName 校验条目名。
func ValidateName(name string) error {
	if !nameRe.MatchString(name) || strings.Contains(name, "..") {
		return fmt.Errorf("keystore: invalid entry name %q (allowed: [A-Za-z0-9._+-], no leading dot, no '..', max 128)", name)
	}
	return nil
}

// ---------------------------------------------------------------------------
// MemStore

// MemStore 内存实现（进程内缓存 / 测试用）。
type MemStore struct {
	mu      sync.RWMutex
	entries map[string]Entry
}

// NewMemStore 创建内存存储。
func NewMemStore() *MemStore {
	return &MemStore{entries: make(map[string]Entry)}
}

// Put 写入条目（覆盖同名）。
func (m *MemStore) Put(_ context.Context, name string, e Entry) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[name] = e
	return nil
}

// Get 读取条目；不存在返回 ErrNotFound。
func (m *MemStore) Get(_ context.Context, name string) (Entry, error) {
	if err := ValidateName(name); err != nil {
		return Entry{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.entries[name]
	if !ok {
		return Entry{}, ErrNotFound
	}
	return e, nil
}

// Delete 删除条目；不存在返回 ErrNotFound。
func (m *MemStore) Delete(_ context.Context, name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.entries[name]; !ok {
		return ErrNotFound
	}
	delete(m.entries, name)
	return nil
}

// List 全部条目名（字典序）。
func (m *MemStore) List(_ context.Context) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.entries))
	for k := range m.entries {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// ---------------------------------------------------------------------------
// FileStore

// FileStore 目录落盘实现。
//
// 布局：root/<name>.json，单文件包含全部字段（PEM/Data base64 编码）。
// 写入为原子操作（写临时文件 + rename），进程崩溃不会留下半截条目。
// 条目文件权限 0600（Windows 下由目录 ACL 继承兜底）。
type FileStore struct {
	root string
}

// NewFileStore 创建目录存储（目录不存在则创建，权限 0700）。
func NewFileStore(root string) (*FileStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("keystore: empty root dir")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("keystore: mkdir %s: %w", root, err)
	}
	return &FileStore{root: root}, nil
}

func (f *FileStore) path(name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	// 二次防御：即使名字合法，也禁止目录成分
	if strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("keystore: invalid entry name %q", name)
	}
	return filepath.Join(f.root, name+".json"), nil
}

// Put 原子写入条目（tmp + rename）。
func (f *FileStore) Put(_ context.Context, name string, e Entry) error {
	p, err := f.path(name)
	if err != nil {
		return err
	}
	b, err := encodeEntry(e)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("keystore: write %s: %w", name, err)
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("keystore: rename %s: %w", name, err)
	}
	return nil
}

// Get 读取条目；不存在返回 ErrNotFound。
func (f *FileStore) Get(_ context.Context, name string) (Entry, error) {
	p, err := f.path(name)
	if err != nil {
		return Entry{}, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return Entry{}, ErrNotFound
		}
		return Entry{}, fmt.Errorf("keystore: read %s: %w", name, err)
	}
	return decodeEntry(b)
}

// Delete 删除条目；不存在返回 ErrNotFound。
func (f *FileStore) Delete(_ context.Context, name string) error {
	p, err := f.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return fmt.Errorf("keystore: delete %s: %w", name, err)
	}
	return nil
}

// List 全部条目名（字典序；剥离 .json 后缀，跳过临时文件与坏名）。
func (f *FileStore) List(_ context.Context) ([]string, error) {
	entries, err := os.ReadDir(f.root)
	if err != nil {
		return nil, fmt.Errorf("keystore: readdir %s: %w", f.root, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		if !nameRe.MatchString(name) {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// ---------------------------------------------------------------------------
// 条目编解码（JSON；PEM 原文、Data base64）

type fileEntry struct {
	CertPEM string            `json:"certPem,omitempty"`
	KeyPEM  string            `json:"keyPem,omitempty"`
	Data    string            `json:"data,omitempty"` // base64
	Meta    map[string]string `json:"meta,omitempty"`
}

func encodeEntry(e Entry) ([]byte, error) {
	fe := fileEntry{
		CertPEM: string(e.CertPEM),
		KeyPEM:  string(e.KeyPEM),
		Data:    base64.StdEncoding.EncodeToString(e.Data),
		Meta:    e.Meta,
	}
	b, err := json.MarshalIndent(fe, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("keystore: encode entry: %w", err)
	}
	return b, nil
}

func decodeEntry(b []byte) (Entry, error) {
	var fe fileEntry
	if err := json.Unmarshal(b, &fe); err != nil {
		return Entry{}, fmt.Errorf("keystore: decode entry: %w", err)
	}
	data, err := base64.StdEncoding.DecodeString(fe.Data)
	if err != nil {
		return Entry{}, fmt.Errorf("keystore: decode data: %w", err)
	}
	return Entry{
		CertPEM: []byte(fe.CertPEM),
		KeyPEM:  []byte(fe.KeyPEM),
		Data:    data,
		Meta:    fe.Meta,
	}, nil
}

// ---------------------------------------------------------------------------
// 便捷方法

// TLSCertificate 将条目解析为 tls.Certificate（CertPEM+KeyPEM 配对校验）。
func TLSCertificate(e Entry) (tls.Certificate, error) {
	if len(e.CertPEM) == 0 || len(e.KeyPEM) == 0 {
		return tls.Certificate{}, errors.New("keystore: entry missing cert/key PEM")
	}
	cert, err := tls.X509KeyPair(e.CertPEM, e.KeyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("keystore: x509 keypair: %w", err)
	}
	return cert, nil
}
