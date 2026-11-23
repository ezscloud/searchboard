// Package vfs 分层覆盖虚拟文件系统。
//
// 用途：前端资源可被外部覆盖而不改二进制——embed 基座（构建产物）→
// 磁盘目录层（部署时自定义）→ 内存层（运行时 API 改写）。
// 查找自顶向下，第一个命中生效；写/删只作用于顶层。
//
//	Stack{ Dir("./public"), Embed(embedFS, "web") }   // public 覆盖内置资源
//	Stack.Put("/brand/logo.svg", data)               // 运行时覆盖
//	Stack.Delete("/brand/logo.svg")                  // 恢复下层内容
package vfs

import (
	"bytes"
	"embed"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// File 统一文件视图（只读 + 尺寸/修改时间，供静态服务用）。
type File interface {
	io.Reader
	io.Closer
	Size() int64
	ModTime() time.Time
}

// Layer 单层存储。
type Layer interface {
	// Open 打开文件；不存在返回 ErrNotExist。
	Open(name string) (File, error)
	// Stat 元信息。
	Stat(name string) (FileInfo, error)
	// List 列出目录下直接子项（相对名字；目录以 "/" 结尾）。
	List(dir string) ([]string, error)
}

// FileInfo 文件元信息。
type FileInfo struct {
	Name    string
	IsDir   bool
	Size    int64
	ModTime time.Time
}

// ErrNotExist 文件不存在。
var ErrNotExist = errors.New("vfs: file not found")

// Clean 规整路径（/开头、去 ./、消灭 .. 越界）。
func Clean(name string) (string, error) {
	name = path.Clean("/" + strings.ReplaceAll(name, "\\", "/"))
	if name == "/" {
		return "/", nil
	}
	if strings.Contains(name, "..") {
		return "", errors.New("vfs: invalid path")
	}
	return name, nil
}

// ---------------------------------------------------------------- Embed 层

// EmbedLayer 把 embed.FS 子树作为一层。
func EmbedLayer(fsys embed.FS, root string) *embedLayer {
	return &embedLayer{fsys: fsys, root: strings.Trim(root, "/")}
}

type embedLayer struct {
	fsys embed.FS
	root string
}

func (l *embedLayer) resolve(name string) string {
	if l.root == "" {
		return strings.TrimPrefix(name, "/")
	}
	return l.root + name
}

func (l *embedLayer) Open(name string) (File, error) {
	name, err := Clean(name)
	if err != nil {
		return nil, err
	}
	f, err := l.fsys.Open(l.resolve(name))
	if err != nil {
		return nil, ErrNotExist
	}
	st, serr := f.Stat()
	if serr != nil || st.IsDir() {
		_ = f.Close()
		return nil, ErrNotExist
	}
	return &embedFile{File: f.(fs.File), size: st.Size(), mod: st.ModTime()}, nil
}

func (l *embedLayer) Stat(name string) (FileInfo, error) {
	name, err := Clean(name)
	if err != nil {
		return FileInfo{}, err
	}
	f, err := l.fsys.Open(l.resolve(name))
	if err != nil {
		return FileInfo{}, ErrNotExist
	}
	defer f.Close()
	st, serr := f.Stat()
	if serr != nil {
		return FileInfo{}, ErrNotExist
	}
	return FileInfo{Name: name, IsDir: st.IsDir(), Size: st.Size(), ModTime: st.ModTime()}, nil
}

func (l *embedLayer) List(dir string) ([]string, error) {
	dir, err := Clean(dir)
	if err != nil {
		return nil, err
	}
	entries, err := l.fsys.ReadDir(l.resolve(dir))
	if err != nil {
		return nil, ErrNotExist
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() {
			n += "/"
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

type embedFile struct {
	fs.File
	size int64
	mod  time.Time
}

func (f *embedFile) Size() int64        { return f.size }
func (f *embedFile) ModTime() time.Time { return f.mod }

// ---------------------------------------------------------------- Dir 层

// DirLayer 磁盘目录层（部署期覆盖：logo/brand 等）。
func DirLayer(dir string) *dirLayer {
	return &dirLayer{dir: dir}
}

type dirLayer struct{ dir string }

func (l *dirLayer) resolve(name string) (string, error) {
	name, err := Clean(name)
	if err != nil {
		return "", err
	}
	full := filepath.Join(l.dir, filepath.FromSlash(strings.TrimPrefix(name, "/")))
	// 越界防护
	if !strings.HasPrefix(full, filepath.Clean(l.dir)+string(os.PathSeparator)) && full != filepath.Clean(l.dir) {
		return "", errors.New("vfs: path escapes layer root")
	}
	return full, nil
}

func (l *dirLayer) Open(name string) (File, error) {
	full, err := l.resolve(name)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(full)
	if err != nil || st.IsDir() {
		return nil, ErrNotExist
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, ErrNotExist
	}
	return &osFile{File: f, size: st.Size(), mod: st.ModTime()}, nil
}

func (l *dirLayer) Stat(name string) (FileInfo, error) {
	full, err := l.resolve(name)
	if err != nil {
		return FileInfo{}, err
	}
	st, err := os.Stat(full)
	if err != nil {
		return FileInfo{}, ErrNotExist
	}
	return FileInfo{Name: name, IsDir: st.IsDir(), Size: st.Size(), ModTime: st.ModTime()}, nil
}

func (l *dirLayer) List(dir string) ([]string, error) {
	full, err := l.resolve(dir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		return nil, ErrNotExist
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() {
			n += "/"
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

type osFile struct {
	*os.File
	size int64
	mod  time.Time
}

func (f *osFile) Size() int64        { return f.size }
func (f *osFile) ModTime() time.Time { return f.mod }

// ---------------------------------------------------------------- Mem 层

// MemLayer 运行时内存层（可写可删——覆盖生效直到重启）。
func MemLayer() *memLayer {
	return &memLayer{files: map[string]*memFile{}}
}

type memLayer struct {
	mu    sync.RWMutex
	files map[string]*memFile
}

type memFile struct {
	data []byte
	mod  time.Time
}

// Put 写入/覆盖。
func (l *memLayer) Put(name string, data []byte) error {
	name, err := Clean(name)
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.files[name] = &memFile{data: append([]byte(nil), data...), mod: time.Now()}
	l.mu.Unlock()
	return nil
}

// Delete 删除（恢复下层可见性）。
func (l *memLayer) Delete(name string) error {
	name, err := Clean(name)
	if err != nil {
		return err
	}
	l.mu.Lock()
	delete(l.files, name)
	l.mu.Unlock()
	return nil
}

func (l *memLayer) Open(name string) (File, error) {
	name, err := Clean(name)
	if err != nil {
		return nil, err
	}
	l.mu.RLock()
	f, ok := l.files[name]
	l.mu.RUnlock()
	if !ok {
		return nil, ErrNotExist
	}
	return &memReader{Reader: bytes.NewReader(f.data), mod: f.mod, size: int64(len(f.data))}, nil
}

func (l *memLayer) Stat(name string) (FileInfo, error) {
	name, err := Clean(name)
	if err != nil {
		return FileInfo{}, err
	}
	l.mu.RLock()
	f, ok := l.files[name]
	l.mu.RUnlock()
	if !ok {
		return FileInfo{}, ErrNotExist
	}
	return FileInfo{Name: name, Size: int64(len(f.data)), ModTime: f.mod}, nil
}

func (l *memLayer) List(dir string) ([]string, error) {
	dir, err := Clean(dir)
	if err != nil {
		return nil, err
	}
	prefix := strings.TrimSuffix(dir, "/")
	l.mu.RLock()
	defer l.mu.RUnlock()
	var out []string
	seen := map[string]bool{}
	for name := range l.files {
		if prefix != "" && !strings.HasPrefix(name, prefix+"/") {
			continue
		}
		rel := strings.TrimPrefix(name, prefix+"/")
		if rel == "" {
			continue
		}
		// 直接子项（截断深层）
		if i := strings.IndexByte(rel, '/'); i >= 0 {
			rel = rel[:i+1]
		}
		if !seen[rel] {
			seen[rel] = true
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out, nil
}

type memReader struct {
	*bytes.Reader
	mod  time.Time
	size int64
}

func (f *memReader) Close() error       { return nil }
func (f *memReader) Size() int64        { return f.size }
func (f *memReader) ModTime() time.Time { return f.mod }

// ---------------------------------------------------------------- Stack

// Stack 分层栈：layers[0] 为顶层（最先查找；写操作目标）。
type Stack struct {
	layers []Layer
}

// NewStack 创建栈（参数顺序即优先级：先高层后底层）。
func NewStack(layers ...Layer) *Stack {
	return &Stack{layers: layers}
}

// Layers 层列表（顶层在前）。
func (s *Stack) Layers() []Layer { return append([]Layer(nil), s.layers...) }

// Push 顶部加层。
func (s *Stack) Push(l Layer) { s.layers = append([]Layer{l}, s.layers...) }

// Open 自顶向下查找。
func (s *Stack) Open(name string) (File, error) {
	name, err := Clean(name)
	if err != nil {
		return nil, err
	}
	for _, l := range s.layers {
		if f, err := l.Open(name); err == nil {
			return f, nil
		}
	}
	return nil, ErrNotExist
}

// Stat 自顶向下。
func (s *Stack) Stat(name string) (FileInfo, error) {
	name, err := Clean(name)
	if err != nil {
		return FileInfo{}, err
	}
	for _, l := range s.layers {
		if fi, err := l.Stat(name); err == nil {
			return fi, nil
		}
	}
	return FileInfo{}, ErrNotExist
}

// List 自顶向下第一个非空目录视图（并集时可能缺项，够静态服务用）。
func (s *Stack) List(dir string) ([]string, error) {
	dir, err := Clean(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range s.layers {
		if items, err := l.List(dir); err == nil && len(items) > 0 {
			out = items
			break
		}
	}
	if out == nil {
		return nil, ErrNotExist
	}
	return out, nil
}

// Put 写入顶层（若顶层为 MemLayer）；否则报错。
func (s *Stack) Put(name string, data []byte) error {
	if len(s.layers) == 0 {
		return errors.New("vfs: empty stack")
	}
	if m, ok := s.layers[0].(*memLayer); ok {
		return m.Put(name, data)
	}
	return errors.New("vfs: top layer is read-only")
}

// Delete 删除顶层条目（若顶层为 MemLayer）。
func (s *Stack) Delete(name string) error {
	if len(s.layers) == 0 {
		return errors.New("vfs: empty stack")
	}
	if m, ok := s.layers[0].(*memLayer); ok {
		return m.Delete(name)
	}
	return errors.New("vfs: top layer is read-only")
}

// ReadFile 全量读取（便捷）。
func (s *Stack) ReadFile(name string) ([]byte, error) {
	f, err := s.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// Exists 存在性。
func (s *Stack) Exists(name string) bool {
	_, err := s.Stat(name)
	return err == nil
}
