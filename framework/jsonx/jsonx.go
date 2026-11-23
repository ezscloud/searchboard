// Package jsonx 统一 JSON 序列化入口：底层固定使用 bytedance/sonic（v1.15.5）。
//
// 框架内所有 JSON 编解码（HTTP 绑定/响应、WebSocket JSON 消息、RPC 编解码）
// 都经由本包，好处：
//
//  1. 依赖收敛：sonic 版本只在 go.mod 与此处出现，其余代码零感知；
//  2. 语义统一：统一 escape/number 处理策略（与标准库完全兼容）；
//  3. 可替换：如未来更换编解码实现，只改本文件。
//
// sonic 平台支持：amd64（go1.17+，<go1.28）与 arm64（go1.20+，<go1.28）走 JIT，
// 其余平台/版本自动回退兼容实现（sonic 包自带），因此本包可在全平台直接 import。
package jsonx

import (
	"io"

	"github.com/bytedance/sonic"
)

// Marshal / Unmarshal 与标准库 encoding/json 语义一致，速度走 sonic JIT。
var (
	Marshal       = sonic.Marshal
	Unmarshal     = sonic.Unmarshal
	MarshalString = sonic.MarshalString
)

// MarshalNoFail 序列化，失败时返回 "null"（用于日志等不容错路径）。
func MarshalNoFail(v any) []byte {
	b, err := sonic.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	return b
}

// UnmarshalString 反序列化字符串。
func UnmarshalString(s string, v any) error {
	return sonic.Unmarshal([]byte(s), v)
}

// WriteJSON 序列化并写入 w（一次 Write，附带换行，便于文本流读取）。
func WriteJSON(w io.Writer, v any) error {
	b, err := sonic.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}
