// Package rpc 实现 JSON-RPC 2.0（规范见 https://www.jsonrpc.org/specification）。
//
//   - 编解码统一走 sonic（webfw/jsonx）
//   - 服务端：反射注册方法（HTTP POST 与 WebSocket 双通道）
//   - 客户端：Call/Notify（HTTP 与 WS 双通道，WS 支持并发在途请求）
//
// 方法签名约定（二选一，返回值必须有 error）：
//
//	func (s *Svc) Method(ctx context.Context, args *Args) (*Reply, error)
//	func (s *Svc) Method(args *Args) (*Reply, error)
package rpc

import (
	"encoding/json"
	"errors"
)

// 协议版本（规范要求固定 "2.0"）。
const Version = "2.0"

// 标准错误码（规范 §5.1）。
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
	CodeServerError    = -32000
)

// Error JSON-RPC 错误对象。
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return "rpc: " + e.Message }

// NewError 构造错误。
func NewError(code int, message string) *Error { return &Error{Code: code, Message: message} }

func errParse() *Error   { return &Error{Code: CodeParseError, Message: "Parse error"} }
func errInvalid() *Error { return &Error{Code: CodeInvalidRequest, Message: "Invalid Request"} }
func errNotFound(m string) *Error {
	return &Error{Code: CodeMethodNotFound, Message: "Method not found: " + m}
}
func errParams(m string) *Error {
	return &Error{Code: CodeInvalidParams, Message: "Invalid params: " + m}
}
func errInternal(m string) *Error {
	return &Error{Code: CodeInternalError, Message: m}
}

// message 线上协议结构（请求/响应同体）。
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// isNotification 消息是否为通知（无 id）。
func (m *message) isNotification() bool { return len(m.ID) == 0 }

// RPCError 客户端侧暴露的调用错误。
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return "rpc call error: " + e.Message
}

// ErrClientClosed 客户端已关闭。
var ErrClientClosed = errors.New("rpc: client closed")
