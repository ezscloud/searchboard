// router.go：段前缀树路由。
//
// 匹配优先级：静态段 > :param > *wildcard（对齐 httprouter 惯例）。
//   - /api/users/:id     捕获单段参数
//   - /static/*filepath  捕获余下全部路径（必须位于末端）
//
// 路径区分尾随斜杠（/a 与 /a/ 视为不同路由，与标准库一致）。
// HEAD 未显式注册时自动回落 GET 处理器（响应侧自动丢弃响应体）。
package framework

import (
	"errors"
	"net/http"
	"sort"
	"strings"
)

func errNew(msg string) error { return errors.New("framework: " + msg) }

const (
	kindStatic uint8 = iota
	kindParam
	kindWildcard
)

type node struct {
	seg      string // 静态字面量 / 参数名
	kind     uint8
	children map[string]*node
	param    *node // :name 子节点
	wild     *node // *name 子节点（必须末端）
	handler  HandlerFunc
}

// Router 路由器。
type Router struct {
	roots map[string]*node
	mws   []HandlerFunc // 全局中间件（注册即生效，按注册顺序）

	notFound         HandlerFunc
	methodNotAllowed HandlerFunc
}

// NewRouter 创建路由器。
func NewRouter() *Router {
	return &Router{
		roots:            map[string]*node{},
		notFound:         defaultNotFound,
		methodNotAllowed: defaultMethodNotAllowed,
	}
}

func defaultNotFound(c *Context) {
	c.String(http.StatusNotFound, "404 page not found: %s", c.Request.Path)
}

func defaultMethodNotAllowed(c *Context) {
	c.String(http.StatusMethodNotAllowed, "405 method not allowed")
}

// Use 追加全局中间件。
func (r *Router) Use(m ...HandlerFunc) { r.mws = append(r.mws, m...) }

// NotFound 自定义 404 处理器。
func (r *Router) NotFound(h HandlerFunc) { r.notFound = h }

// MethodNotAllowed 自定义 405 处理器。
func (r *Router) MethodNotAllowed(h HandlerFunc) { r.methodNotAllowed = h }

// Handle 注册路由；method 用标准库常量（http.MethodGet 等）。
// 重复注册同一 method+path 返回错误。
func (r *Router) Handle(method, path string, h HandlerFunc) error {
	if h == nil {
		return errNilHandler
	}
	method = strings.ToUpper(method)
	segs, err := splitPath(path)
	if err != nil {
		return err
	}
	root := r.roots[method]
	if root == nil {
		root = &node{kind: kindStatic}
		r.roots[method] = root
	}
	cur := root
	for i, seg := range segs {
		switch {
		case strings.HasPrefix(seg, "*"):
			if i != len(segs)-1 {
				return errWildCardNotLast
			}
			name := strings.TrimPrefix(seg, "*")
			if cur.wild == nil {
				cur.wild = &node{seg: name, kind: kindWildcard}
			}
			cur = cur.wild
		case strings.HasPrefix(seg, ":"):
			if cur.param == nil {
				cur.param = &node{seg: strings.TrimPrefix(seg, ":"), kind: kindParam}
			}
			cur = cur.param
		default:
			if cur.children == nil {
				cur.children = map[string]*node{}
			}
			child, ok := cur.children[seg]
			if !ok {
				child = &node{seg: seg, kind: kindStatic}
				cur.children[seg] = child
			}
			cur = child
		}
	}
	if cur.handler != nil {
		return &routeConflictError{method: method, path: path}
	}
	cur.handler = h
	return nil
}

type routeConflictError struct{ method, path string }

func (e *routeConflictError) Error() string {
	return "framework: route conflict: " + e.method + " " + e.path
}

var (
	errNilHandler      = errNew("handler is nil")
	errWildCardNotLast = errNew("wildcard segment must be the last one")
	errEmptySeg        = errNew("empty path segment (double slash)")
)

// splitPath 规整为段切片；"/" → [""]，"/a/" → ["a",""]。
func splitPath(path string) ([]string, error) {
	if path == "" || path[0] != '/' {
		return nil, errNew("path must begin with '/'")
	}
	segs := strings.Split(path[1:], "/")
	for i, s := range segs {
		if s == "" && i != len(segs)-1 {
			return nil, errEmptySeg
		}
	}
	return segs, nil
}

// 便捷注册（出错即 panic，适合字面量路由声明）。
func (r *Router) GET(path string, h HandlerFunc)    { must(r.Handle(http.MethodGet, path, h)) }
func (r *Router) POST(path string, h HandlerFunc)   { must(r.Handle(http.MethodPost, path, h)) }
func (r *Router) PUT(path string, h HandlerFunc)    { must(r.Handle(http.MethodPut, path, h)) }
func (r *Router) DELETE(path string, h HandlerFunc) { must(r.Handle(http.MethodDelete, path, h)) }
func (r *Router) PATCH(path string, h HandlerFunc)  { must(r.Handle(http.MethodPatch, path, h)) }
func (r *Router) HEAD(path string, h HandlerFunc)   { must(r.Handle(http.MethodHead, path, h)) }
func (r *Router) OPTIONS(path string, h HandlerFunc) {
	must(r.Handle(http.MethodOptions, path, h))
}

// Any 注册全部标准方法。
func (r *Router) Any(path string, h HandlerFunc) {
	for _, m := range []string{
		http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete,
		http.MethodPatch, http.MethodHead, http.MethodOptions,
	} {
		must(r.Handle(m, path, h))
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// Lookup 查找路由。
//
// 返回：处理器（可能为 nil）、路由参数、该路径支持的 method 列表（405 用）。
func (r *Router) Lookup(method, path string) (h HandlerFunc, params map[string]string, allow []string) {
	segs, err := splitPath(path)
	if err != nil {
		return nil, nil, nil
	}
	method = strings.ToUpper(method)

	// HEAD 回落 GET
	root := r.roots[method]
	if root == nil && method == http.MethodHead {
		root = r.roots[http.MethodGet]
	}
	if root != nil {
		h, params = matchNode(root, segs)
		if h != nil {
			return h, params, nil
		}
	}

	// 路径存在但 method 不匹配 → 收集 allow
	allow = r.allowedMethods(path, method)
	return nil, nil, allow
}

func matchNode(root *node, segs []string) (HandlerFunc, map[string]string) {
	var params map[string]string
	cur := root
	for i, seg := range segs {
		if child, ok := cur.children[seg]; ok {
			cur = child
			continue
		}
		if cur.param != nil {
			if params == nil {
				params = map[string]string{}
			}
			params[cur.param.seg] = seg
			cur = cur.param
			continue
		}
		if cur.wild != nil {
			if params == nil {
				params = map[string]string{}
			}
			params[cur.wild.seg] = strings.Join(segs[i:], "/")
			return cur.wild.handler, params
		}
		return nil, nil
	}
	return cur.handler, params
}

func (r *Router) allowedMethods(path string, exclude string) []string {
	var allow []string
	for m, root := range r.roots {
		if m == exclude {
			continue
		}
		if h, _ := matchNode(root, mustSplit(path)); h != nil {
			allow = append(allow, m)
		}
	}
	sort.Strings(allow)
	return allow
}

func mustSplit(path string) []string {
	segs, _ := splitPath(path)
	if segs == nil {
		return []string{}
	}
	return segs
}

// Dispatch 执行路由（中间件链 + 路由处理器或 404/405）。
func (r *Router) Dispatch(ctx *Context) {
	h, params, allow := r.Lookup(ctx.Request.Method, ctx.Request.Path)
	ctx.Params = params

	chain := make([]HandlerFunc, 0, len(r.mws)+1)
	chain = append(chain, r.mws...)

	switch {
	case h != nil:
		chain = append(chain, h)
	case len(allow) > 0:
		ctx.SetRespHeader("Allow", strings.Join(append(allow, "OPTIONS"), ", "))
		chain = append(chain, r.methodNotAllowed)
	default:
		chain = append(chain, r.notFound)
	}
	ctx.dispatch(chain)
}
