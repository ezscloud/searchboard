// trace/inject.go：出站注入桥（避免 trace 包反向依赖 framework 客户端类型细节）。
package trace

import (
	"net/http"
	"strconv"
)

// httpRequestAlias 出站请求别名。
type httpRequestAlias = *http.Request

// reqSetHeader 设置请求头。
func reqSetHeader(req *http.Request, key, value string) {
	if req == nil || req.Header == nil {
		return
	}
	req.Header.Set(key, value)
}

func strconvItoa(n int) string { return strconv.Itoa(n) }
