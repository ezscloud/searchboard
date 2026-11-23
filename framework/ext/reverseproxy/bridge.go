// reverseproxy/bridge.go：上游转发桥（集中封装，便于审计与替换）。
package reverseproxy

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/ezscloud/searchboard/framework"
)

// httpResponse 转发响应视图。
type httpResponse struct {
	StatusCode int
	Header     http.Header
	Body       func() io.Reader
	Close      func() error
}

// newClientRequest 组装上游请求（BodyLen 由调用方补）。
func newClientRequest(ctx context.Context, method, url string, body io.Reader) *framework.ClientRequest {
	return framework.NewClientRequest(ctx, method, url, body)
}

// clientDo 执行上游请求并包装为 httpResponse。
func clientDo(ctx context.Context, req *framework.ClientRequest) (*httpResponse, error) {
	cli := framework.NewClient()
	cli.Timeout = 15 * time.Second
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	return &httpResponse{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       func() io.Reader { return resp.Body() },
		Close:      func() error { return resp.Close() },
	}, nil
}

var _ = context.Background
