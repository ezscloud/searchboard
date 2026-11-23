// pprof/profile.go：具名 profile 的数据采集桥。
package pprof

import (
	"net/url"
	"runtime/pprof"
)

// urlType 别名（标准库 url.URL）。
type urlType = url.URL

// parseURL 构造请求 URL。
func parseURL(path, rawQuery string) *url.URL {
	u := &url.URL{Path: path}
	if rawQuery != "" {
		u.RawQuery = rawQuery
	}
	return u
}

// writeProfileTo 采集具名 profile 输出（debug=1 为可读文本）。
func writeProfileTo(name string, debug int) ([]byte, error) {
	for _, p := range pprof.Profiles() {
		if p.Name() == name {
			var buf byteBuf
			if err := p.WriteTo(&buf, debug); err != nil {
				return nil, err
			}
			return buf.Bytes(), nil
		}
	}
	return nil, errUnknownProfile(name)
}

// byteBuf 轻量缓冲（bytes.Buffer 等价，避免导出面扩散）。
type byteBuf struct{ b []byte }

func (w *byteBuf) Write(p []byte) (int, error) {
	w.b = append(w.b, p...)
	return len(p), nil
}

func (w *byteBuf) Bytes() []byte { return w.b }

func errUnknownProfile(name string) error {
	return &profileError{name: name}
}

type profileError struct{ name string }

func (e *profileError) Error() string { return "unknown profile: " + e.name }
