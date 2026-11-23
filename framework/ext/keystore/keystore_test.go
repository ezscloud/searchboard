// keystore_test.go：MemStore/FileStore CRUD、持久化、穿越防护、并发、
// autocert 适配器与 TLS 证书解析。
package keystore

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

func ctx() context.Context { return context.Background() }

func sampleEntry(tag string) Entry {
	return Entry{
		Data: []byte("payload-" + tag),
		Meta: map[string]string{"tag": tag, "source": "test"},
	}
}

// certPEM 生成一对自签证书/私钥 PEM（测试用，ECDSA P-256）。
func certPEM(t *testing.T, cn string) (cert, key []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     []string{cn},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cert = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	key = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	return cert, key
}

func TestValidateName(t *testing.T) {
	ok := []string{"a", "cert.example.com", "acme_account+v2", "a-b_c.d"}
	bad := []string{"", "a/b", `a\b`, "..", ".hidden", "with space", "中文", string(make([]byte, 200))}
	for _, n := range ok {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) = %v, want ok", n, err)
		}
	}
	for _, n := range bad {
		if err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", n)
		}
	}
}

// assertStoreConsistency 对任一 Store 跑同一套 CRUD 语义断言。
func assertStoreConsistency(t *testing.T, st Store) {
	t.Helper()
	ctx := ctx()

	// miss
	if _, err := st.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}
	if err := st.Delete(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete missing = %v, want ErrNotFound", err)
	}

	// put/get roundtrip（含中文与二进制 Data）
	e := Entry{
		Data: []byte{0x00, 0x01, 0xFE, 0xFF, 'x'},
		Meta: map[string]string{"域名": "example.com", "k": "v"},
	}
	if err := st.Put(ctx, "entry.one", e); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := st.Get(ctx, "entry.one")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.Data) != string(e.Data) || got.Meta["域名"] != "example.com" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}

	// 覆盖写
	if err := st.Put(ctx, "entry.one", sampleEntry("v2")); err != nil {
		t.Fatalf("Put overwrite: %v", err)
	}
	got, _ = st.Get(ctx, "entry.one")
	if string(got.Data) != "payload-v2" {
		t.Fatalf("overwrite failed: %q", got.Data)
	}

	// 非法名拒绝
	if err := st.Put(ctx, "../evil", sampleEntry("x")); err == nil {
		t.Fatal("Put ../evil should be rejected")
	}

	// list + delete
	if err := st.Put(ctx, "entry.two", sampleEntry("b")); err != nil {
		t.Fatal(err)
	}
	names, err := st.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "entry.one" || names[1] != "entry.two" {
		t.Fatalf("List = %v", names)
	}
	if err := st.Delete(ctx, "entry.one"); err != nil {
		t.Fatal(err)
	}
	names, _ = st.List(ctx)
	if len(names) != 1 || names[0] != "entry.two" {
		t.Fatalf("List after delete = %v", names)
	}
}

func TestMemStore(t *testing.T) {
	assertStoreConsistency(t, NewMemStore())
}

func TestFileStore(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertStoreConsistency(t, st)

	// 目录穿越防御：Get/Delete 同样拒绝
	if _, err := st.Get(ctx(), "../evil"); err == nil {
		t.Fatal("Get ../evil should be rejected")
	}
	if err := st.Delete(ctx(), "..\\evil"); err == nil {
		t.Fatal("Delete ..\\evil should be rejected")
	}
}

func TestFileStorePersistence(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM := certPEM(t, "persist.example.com")
	if err := st.Put(ctx(), "tls.site", Entry{CertPEM: certPEM, KeyPEM: keyPEM, Meta: map[string]string{"cn": "persist.example.com"}}); err != nil {
		t.Fatal(err)
	}

	// 重新打开（模拟进程重启）
	st2, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, err := st2.Get(ctx(), "tls.site")
	if err != nil {
		t.Fatalf("reopened Get: %v", err)
	}
	c, err := TLSCertificate(e)
	if err != nil {
		t.Fatalf("TLSCertificate: %v", err)
	}
	if c.Leaf == nil && len(c.Certificate) == 0 {
		t.Fatal("empty certificate")
	}

	// 磁盘布局与权限（条目文件存在且为单文件）
	if _, err := os.Stat(filepath.Join(dir, "tls.site.json")); err != nil {
		t.Fatalf("entry file missing: %v", err)
	}
}

func TestTLSCertificate(t *testing.T) {
	certPEM, keyPEM := certPEM(t, "pair.example.com")

	// 配对正确
	c, err := TLSCertificate(Entry{CertPEM: certPEM, KeyPEM: keyPEM})
	if err != nil {
		t.Fatalf("valid pair: %v", err)
	}
	if len(c.Certificate) == 0 {
		t.Fatal("no certificate chain")
	}

	// 缺字段
	if _, err := TLSCertificate(Entry{CertPEM: certPEM}); err == nil {
		t.Fatal("missing key should error")
	}
	// 错配（cert+无关 key）
	if _, err := TLSCertificate(Entry{CertPEM: certPEM, KeyPEM: []byte("garbage")}); err == nil {
		t.Fatal("mismatched key should error")
	}
}

func TestMemStoreConcurrent(t *testing.T) {
	st := NewMemStore()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "k" + string(rune('a'+i%8))
			_ = st.Put(ctx(), name, sampleEntry("w"))
			_, _ = st.Get(ctx(), name)
			_, _ = st.List(ctx())
			_ = st.Delete(ctx(), name)
		}(i)
	}
	wg.Wait()
}

func TestAutocertCache(t *testing.T) {
	for name, st := range map[string]Store{
		"mem":  NewMemStore(),
		"file": mustFileStore(t),
	} {
		t.Run(name, func(t *testing.T) {
			c := AutocertCache{Store: st}

			// miss → autocert.ErrCacheMiss（autocert 依赖此哨兵）
			if _, err := c.Get(ctx(), "acme/site"); !errors.Is(err, autocert.ErrCacheMiss) {
				t.Fatalf("Get miss = %v, want ErrCacheMiss", err)
			}
			// delete miss 容忍
			if err := c.Delete(ctx(), "acme/site"); err != nil {
				t.Fatalf("Delete miss = %v", err)
			}

			// put/get roundtrip（DER 数据）
			data := []byte{0x30, 0x82, 0x01, 0x00}
			if err := c.Put(ctx(), "acme/site", data); err != nil {
				t.Fatalf("Put: %v", err)
			}
			got, err := c.Get(ctx(), "acme/site")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if string(got) != string(data) {
				t.Fatal("roundtrip mismatch")
			}

			// delete 后再 miss
			if err := c.Delete(ctx(), "acme/site"); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Get(ctx(), "acme/site"); !errors.Is(err, autocert.ErrCacheMiss) {
				t.Fatalf("Get after delete = %v, want ErrCacheMiss", err)
			}
		})
	}
}

func mustFileStore(t *testing.T) Store {
	t.Helper()
	st, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return st
}
