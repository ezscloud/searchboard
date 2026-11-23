// tail_test.go logtail 包单元测试：pattern 匹配、文件列表、尾部回放、Follow 与轮转。
package logtail

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMatchPattern(t *testing.T) {
	tr := New("", "*.log", time.Second, 100)
	cases := []struct {
		name string
		want bool
	}{
		{"app.log", true}, {"es.log.1.gz", false}, {"gc.log", true},
		{"", false}, {"a/b.log", false}, {`C:\evil.log`, false},
	}
	for _, c := range cases {
		if got := tr.MatchPattern(c.name); got != c.want {
			t.Errorf("MatchPattern(%q)=%v want %v", c.name, got, c.want)
		}
	}
}

func TestListFiles(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"app.log", "es.log", "notes.txt", "sub.log"} {
		p := filepath.Join(dir, n)
		if n == "sub.log" {
			_ = os.MkdirAll(p, 0o755) // 目录应被排除
			continue
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond) // mtime 区分度
	}
	tr := New(dir, "*.log", time.Second, 100)
	files, err := tr.ListFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("files=%v want 2 entries (app.log, es.log)", files)
	}
	if files[0].Name != "es.log" { // 最新修改在前
		t.Errorf("order: %s should be first (newest)", files[0].Name)
	}
	if files[0].Size != 1 {
		t.Errorf("size=%d want 1", files[0].Size)
	}
}

func TestListFilesMaxLimit(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 15; i++ {
		_ = os.WriteFile(filepath.Join(dir, "f"+string(rune('a'+i))+".log"), []byte("x"), 0o644)
	}
	tr := New(dir, "*.log", time.Second, 5)
	files, _ := tr.ListFiles()
	if len(files) != 5 {
		t.Fatalf("len=%d want 5 (max_files 截断)", len(files))
	}
}

func TestReadLastLines(t *testing.T) {
	dir := t.TempDir()
	name := "app.log"
	content := "l1\nl2\nl3\nl4\nl5\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := New(dir, "*.log", time.Second, 100)

	lines, err := tr.ReadLastLines(name, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 || lines[0] != "l3" || lines[2] != "l5" {
		t.Errorf("lines=%v want [l3 l4 l5]", lines)
	}

	lines, err = tr.ReadLastLines(name, 100)
	if err != nil || len(lines) != 5 {
		t.Errorf("lines=%v err=%v want 5", lines, err)
	}

	// 未匹配文件拒绝
	if _, err := tr.ReadLastLines("evil.log.1", 3); err == nil {
		t.Error("unmatched file should be rejected")
	}
	// 目录穿越拒绝
	if _, err := tr.ReadLastLines("..\\secrets.txt", 3); err == nil {
		t.Error("path traversal should be rejected")
	}
}

func TestReadLastLinesLargeFile(t *testing.T) {
	dir := t.TempDir()
	name := "big.log"
	// >4MB 内容：回放窗口应只保留尾部
	var big []byte
	for i := 0; i < 200000; i++ {
		big = append(big, []byte("row "+string(rune('a'+i%26))+"\n")...)
	}
	if err := os.WriteFile(filepath.Join(dir, name), big, 0o644); err != nil {
		t.Fatal(err)
	}
	tr := New(dir, "*.log", time.Second, 100)
	lines, err := tr.ReadLastLines(name, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 500 {
		t.Errorf("len=%d want 500", len(lines))
	}
	for _, ln := range lines {
		if len(ln) > 40 || !containsOnly(ln) {
			t.Errorf("残行泄漏: %q", ln[:min(20, len(ln))])
		}
	}
}

func containsOnly(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c == ' ' || c == '\r' || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return len(s) > 0
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestFollow(t *testing.T) {
	dir := t.TempDir()
	name := "app.log"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("init-1\ninit-2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := New(dir, "*.log", 20*time.Millisecond, 100)

	size := tr.Size(name)
	if size <= 0 {
		t.Fatalf("size=%d want >0", size)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	lines := make(chan string, 64)
	done := make(chan error, 1)
	go func() {
		done <- tr.Follow(ctx, name, size, func(batch []string) bool {
			for _, ln := range batch {
				lines <- ln
			}
			return true
		}, nil)
	}()

	// 追加两行
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("live-1\nlive-2\n")
	f.Close()

	got := 0
	deadline := time.Now().Add(2 * time.Second)
	for got < 2 && time.Now().Before(deadline) {
		select {
		case ln := <-lines:
			got++
			if ln != "live-1" && ln != "live-2" {
				t.Errorf("unexpected line %q", ln)
			}
		case <-time.After(deadline.Sub(time.Now())):
		}
	}
	if got != 2 {
		t.Fatalf("got %d lines, want 2", got)
	}
	cancel()
	<-done
}

func TestFollowRotate(t *testing.T) {
	dir := t.TempDir()
	name := "app.log"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("before-rotate-1\nbefore-rotate-2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := New(dir, "*.log", 20*time.Millisecond, 100)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	lines := make(chan string, 64)
	rotated := make(chan struct{}, 4)
	done := make(chan error, 1)
	go func() {
		done <- tr.Follow(ctx, name, 0, func(batch []string) bool {
			for _, ln := range batch {
				lines <- ln
			}
			return true
		}, func() {
			select {
			case rotated <- struct{}{}:
			default:
			}
		})
	}()

	// 收回放
	expect := []string{"before-rotate-1", "before-rotate-2"}
	for _, want := range expect {
		select {
		case ln := <-lines:
			if ln != want {
				t.Fatalf("line=%q want %q", ln, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting %q", want)
		}
	}

	// 轮转：truncate（模拟 ES copy+truncate 滚动）
	if err := os.WriteFile(path, []byte("after-rotate-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-rotated:
	case <-time.After(2 * time.Second):
		t.Fatal("rotate not detected")
	}
	select {
	case ln := <-lines:
		if ln != "after-rotate-1" {
			t.Fatalf("after rotate line=%q", ln)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting post-rotate line")
	}
	cancel()
	<-done
}

func TestFollowUnmatched(t *testing.T) {
	tr := New(t.TempDir(), "*.log", time.Second, 100)
	err := tr.Follow(context.Background(), "evil.log.1", 0, nil, nil)
	if err == nil {
		t.Fatal("unmatched follow should error")
	}
}
