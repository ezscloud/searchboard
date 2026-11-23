// Package logtail 日志文件轮询追踪（tail -f 的 Go 等价实现，零外部依赖）。
//
// 设计要点：
//   - 轮询而非 inotify：跨平台一致（Windows 无 inotify），且对网络盘更稳；
//   - 行缓冲：只下发完整行（以 \n 结尾），残行留在缓冲等待补齐；
//   - 轮转检测：size 突然小于上次 offset（truncate）或文件被替换时重开，
//     从头继续追踪（对齐 ES log4j2 滚动场景）；
//   - Follow 以批回调交付，返回 false 可让调用方终止。
package logtail

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// FileInfo 日志文件元信息。
type FileInfo struct {
	Name    string `json:"name"`  // 文件名（不含目录）
	Size    int64  `json:"size"`  // 字节
	ModTime int64  `json:"mtime"` // Unix 毫秒
}

// Tailer 日志目录追踪器。
type Tailer struct {
	dir      string
	pattern  string
	interval time.Duration
	maxFiles int

	mu     sync.Mutex
	closed bool
}

// New 创建追踪器（pattern 为文件名 glob，如 "*.log"）。
func New(dir, pattern string, interval time.Duration, maxFiles int) *Tailer {
	if pattern == "" {
		pattern = "*.log"
	}
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	if maxFiles <= 0 {
		maxFiles = 200
	}
	return &Tailer{dir: dir, pattern: pattern, interval: interval, maxFiles: maxFiles}
}

// Dir 日志目录。
func (t *Tailer) Dir() string { return t.dir }

// MatchPattern 文件名是否匹配追踪 pattern。
func (t *Tailer) MatchPattern(name string) bool {
	if strings.ContainsAny(name, "/\\") {
		return false // 只允许单级文件名，防目录穿越
	}
	ok, err := path.Match(t.pattern, name)
	return err == nil && ok
}

// ListFiles 列出目录下匹配的日志文件（按修改时间倒序，超 maxFiles 截断）。
func (t *Tailer) ListFiles() ([]FileInfo, error) {
	if t.dir == "" {
		return nil, fmt.Errorf("logtail: log dir not configured")
	}
	entries, err := os.ReadDir(t.dir)
	if err != nil {
		return nil, fmt.Errorf("logtail: read dir %s: %w", t.dir, err)
	}
	var out []FileInfo
	for _, e := range entries {
		if e.IsDir() || !t.MatchPattern(e.Name()) {
			continue
		}
		st, err := e.Info()
		if err != nil {
			continue // 竞态删除容忍
		}
		out = append(out, FileInfo{
			Name:    e.Name(),
			Size:    st.Size(),
			ModTime: st.ModTime().UnixMilli(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime > out[j].ModTime })
	if len(out) > t.maxFiles {
		out = out[:t.maxFiles]
	}
	return out, nil
}

// ReadLastLines 读文件尾部 n 行（完整行；不足则全量返回）。
func (t *Tailer) ReadLastLines(name string, n int) ([]string, error) {
	if !t.MatchPattern(name) {
		return nil, fmt.Errorf("logtail: file %q not matched by pattern %q", name, t.pattern)
	}
	if n <= 0 {
		n = 1
	}
	b, err := os.ReadFile(filepath.Join(t.dir, name))
	if err != nil {
		return nil, fmt.Errorf("logtail: read %s: %w", name, err)
	}
	// 文件可能很大：只留尾部 ~4MB（ES 日志单行一般 <1MB）
	const maxWindow = 4 << 20
	if len(b) > maxWindow {
		b = b[len(b)-maxWindow:]
		if i := strings.IndexByte(string(b), '\n'); i >= 0 {
			b = b[i+1:] // 丢弃首残行
		}
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	// 空文件时 Split 返回 [""]，过滤
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if ln != "" {
			out = append(out, ln)
		}
	}
	return out, nil
}

// Follow 轮询追踪文件新增内容（从 offset 起）。
//
// onBatch 收到一批完整行；返回 false 终止追踪（如客户端断开）。
// onRotate 在检测到轮转（truncate/替换）时回调（可空）。
//
// 返回 nil 表示正常结束（ctx 取消或 onBatch 终止）。
func (t *Tailer) Follow(ctx context.Context, name string, offset int64,
	onBatch func(lines []string) bool, onRotate func()) error {

	if !t.MatchPattern(name) {
		return fmt.Errorf("logtail: file %q not matched by pattern %q", name, t.pattern)
	}
	full := filepath.Join(t.dir, name)

	f, pos, err := openAt(full, offset)
	if err != nil {
		return err
	}

	var pending []byte // 残行缓冲
	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil
		case <-ticker.C:
		}

		// 文件消失或缩小 → 轮转/截断：重开从头读
		st, err := os.Stat(full)
		if err != nil || st.Size() < pos {
			_ = f.Close()
			if onRotate != nil {
				onRotate()
			}
			// 重试打开（轮转中文件可能短暂不存在）
			nf, npos, oerr := openAt(full, 0)
			if oerr != nil {
				// 等待下一轮再试
				pos = 0
				f = nil
			} else {
				f, pos = nf, npos
			}
			if f == nil {
				continue
			}
		}

		if f == nil {
			continue
		}

		// 读取新增数据（一次 ≤ 256KB，防突发大块占内存）
		buf := make([]byte, 256<<10)
		var batch []string
		for {
			n, rerr := f.Read(buf)
			if n > 0 {
				pos += int64(n)
				pending = append(pending, buf[:n]...)
				// 按 \n 切完整行，残行留在 pending
				for {
					i := strings.IndexByte(string(pending), '\n')
					if i < 0 {
						break
					}
					line := strings.TrimSuffix(string(pending[:i]), "\r")
					pending = pending[i+1:]
					if line != "" {
						batch = append(batch, line)
					}
				}
				// pending 过大防泄漏（单行 > 1MB 视为异常，截断丢弃）
				if len(pending) > 1<<20 {
					pending = nil
				}
			}
			if rerr != nil || n == 0 {
				break
			}
			if len(batch) >= 2000 { // 单批上限，防止一次 poll 撑爆下游
				break
			}
		}
		if len(batch) > 0 && !onBatch(batch) {
			_ = f.Close()
			return nil
		}
	}
}

// Size 文件当前大小（不存在返回 -1；Follow 起点用）。
func (t *Tailer) Size(name string) int64 {
	if !t.MatchPattern(name) {
		return -1
	}
	st, err := os.Stat(filepath.Join(t.dir, name))
	if err != nil || st.Mode()&os.ModeType != 0 {
		return -1
	}
	return st.Size()
}

// openAt 打开文件并 seek 到 offset（文件不存在或 offset 越界时从 0 开始）。
func openAt(full string, offset int64) (*os.File, int64, error) {
	f, err := os.Open(full)
	if err != nil {
		return nil, 0, fmt.Errorf("logtail: open %s: %w", path.Base(full), err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("logtail: stat %s: %w", path.Base(full), err)
	}
	mode := st.Mode()
	if mode&os.ModeType != 0 { // 非普通文件（目录/管道等）
		_ = f.Close()
		return nil, 0, fmt.Errorf("logtail: %s is not a regular file", path.Base(full))
	}
	pos := offset
	if pos > st.Size() || pos < 0 {
		pos = 0
	}
	if _, err := f.Seek(pos, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("logtail: seek %s: %w", path.Base(full), err)
	}
	return f, pos, nil
}
