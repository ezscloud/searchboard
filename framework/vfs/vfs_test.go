package vfs

import (
	"bytes"
	"embed"
	"os"
	"path/filepath"
	"testing"
	"time"
)

//go:embed testdata/base
var embedBase embed.FS

func TestStackOverlay(t *testing.T) {
	base := EmbedLayer(embedBase, "testdata/base")
	dir := t.TempDir()
	// 基座里有 a.txt；覆盖层写 a.txt（覆盖）与 b.txt（新增）
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("overlay-a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("overlay-b"), 0o644); err != nil {
		t.Fatal(err)
	}
	over := DirLayer(dir)

	s := NewStack(over, base)

	if got, _ := s.ReadFile("/a.txt"); string(got) != "overlay-a" {
		t.Fatalf("overlay should win: %q", got)
	}
	if got, _ := s.ReadFile("/b.txt"); string(got) != "overlay-b" {
		t.Fatalf("overlay add: %q", got)
	}
	// 基座独有
	if got, _ := s.ReadFile("/only-base.txt"); string(got) != "base-only" {
		t.Fatalf("base fallback: %q", got)
	}
	if _, err := s.ReadFile("/missing.txt"); err != ErrNotExist {
		t.Fatalf("missing want ErrNotExist: %v", err)
	}
}

func TestMemLayerRuntimeOverlay(t *testing.T) {
	base := EmbedLayer(embedBase, "testdata/base")
	mem := MemLayer()
	s := NewStack(mem, base)

	// 运行时写入覆盖
	if err := s.Put("/a.txt", []byte("runtime-a")); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ReadFile("/a.txt"); string(got) != "runtime-a" {
		t.Fatalf("runtime override: %q", got)
	}
	// 删除 → 恢复基座
	if err := s.Delete("/a.txt"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ReadFile("/a.txt"); string(got) != "base-a" {
		t.Fatalf("restore base: %q", got)
	}
	// 列目录
	if err := s.Put("/sub/x.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	items, err := mem.List("/")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range items {
		if it == "sub/" {
			found = true
		}
	}
	if !found {
		t.Fatalf("mem list missing sub/: %v", items)
	}
}

func TestDirLayerEscapes(t *testing.T) {
	l := DirLayer(t.TempDir())
	if _, err := l.Open("/../secret.txt"); err == nil {
		t.Fatalf("escape should be rejected")
	}
}

func TestClean(t *testing.T) {
	cases := map[string]string{
		"/a/b.txt": "/a/b.txt",
		"a.txt":    "/a.txt",
		"/a/../b":  "/b",
		"//x//y":   "/x/y",
		"/":        "/",
	}
	for in, want := range cases {
		got, err := Clean(in)
		if err != nil || got != want {
			t.Errorf("Clean(%q) = %q,%v want %q", in, got, err, want)
		}
	}
	if got, err := Clean("/a/../../escape"); err != nil || got != "/escape" {
		t.Fatalf("Clean should sanitize to /escape, got %q,%v", got, err)
	}
}

var _ = bytes.Equal
var _ = time.Now
