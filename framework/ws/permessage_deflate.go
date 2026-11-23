// ws/permessage_deflate.go：permessage-deflate 扩展（RFC 7692）。
//
// 协商：
//   - 服务端 UpgradeOptions.PerMessageDeflate=true → 响应
//     Sec-WebSocket-Extensions: permessage-deflate; server_no_context_takeover
//   - 客户端 DialOptions.PerMessageDeflate=true → 请求协商（对端同意即启用）
//
// 压缩语义：每消息独立 DEFLATE 块 + 尾部 4 字节空块（0x00 0x00 0xFF 0xFF）；
// 分片帧 RSV1 标记首帧。为稳妥起见本实现按 no_context_takeover 语义工作
// （每消息重置滑动窗口，与 gorilla/websocket 的 server_no_context_takeover
// 行为一致），不支持跨消息上下文接管。
package ws

import (
	"bytes"
	"compress/flate"
	"io"
)

// deflateLevel 压缩级别。
const deflateLevel = flate.DefaultCompression

// deflateTail 空块结尾（写侧附加；读侧剥离）。
var deflateTail = []byte{0x00, 0x00, 0xff, 0xff}

// pmDeflate 连接级压缩器（enabled=false 全直通）。
type pmDeflate struct {
	enabled bool
}

// newPMDeflate 创建压缩器。
func newPMDeflate(enabled bool) *pmDeflate {
	return &pmDeflate{enabled: enabled}
}

// Enabled 是否协商启用。
func (p *pmDeflate) Enabled() bool { return p != nil && p.enabled }

// Compress 压缩单条消息（附加空块结尾）；协商未启用返回 nil（直通）。
func (p *pmDeflate) Compress(payload []byte) ([]byte, error) {
	if !p.Enabled() || len(payload) == 0 {
		return nil, nil
	}
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, deflateLevel)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(payload); err != nil {
		return nil, err
	}
	if err := w.Flush(); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return append(buf.Bytes(), deflateTail...), nil
}

// Decompress 解压单条消息（剥离尾部空块；容忍残留尾块的合法流）。
func (p *pmDeflate) Decompress(payload []byte) ([]byte, error) {
	if !p.Enabled() || len(payload) == 0 {
		return payload, nil
	}
	data := payload
	if len(data) >= 4 && bytes.Equal(data[len(data)-4:], deflateTail) {
		data = data[:len(data)-4]
	}
	r := flate.NewReader(bytes.NewReader(data))
	defer r.Close()
	var out bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, ErrProtocol
		}
	}
	return out.Bytes(), nil
}
