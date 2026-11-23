// Package i18n 轻量国际化：字典 + Accept-Language 协商。
//
// 用法：
//
//	bundle := i18n.New(map[string]map[string]string{
//	    "zh-CN": {"hello": "你好"},
//	    "en":     {"hello": "Hello"},
//	}, i18n.Options{Default: "zh-CN"})
//	srv.Use(i18n.Middleware(bundle))
//	// 处理器内：c.Get(i18n.Key).(string) == "zh-CN"
//	i18n.T(c, "hello")
package i18n

import (
	"sort"
	"strings"

	"github.com/ezscloud/searchboard/framework"
)

// Key 语言标签存取键。
const Key = "framework.lang"

// bundleKey 语言包存取键。
const bundleKey = "framework.i18n.bundle"

// Options 配置。
type Options struct {
	Default  string // 兜底语言（缺省 en）
	QueryKey string // 语言覆盖参数名（缺省 lang），优先于 cookie
	Cookie   string // cookie 名（空=不读）
}

// Bundle 多语言包。
type Bundle struct {
	dict   map[string]map[string]string
	def    string
	sorted []string // 已排序的可用语言（quality 匹配用）
}

// New 创建语言包。
func New(dict map[string]map[string]string, opts Options) *Bundle {
	if opts.Default == "" {
		opts.Default = "en"
	}
	if opts.QueryKey == "" {
		opts.QueryKey = "lang"
	}
	b := &Bundle{dict: dict, def: opts.Default}
	for lang := range dict {
		b.sorted = append(b.sorted, lang)
	}
	sort.Strings(b.sorted)
	return b
}

// Middleware 注入协商语言与语言包到 Context。
func Middleware(b *Bundle) framework.Middleware {
	return func(c *framework.Context) {
		lang := b.negotiate(c)
		c.Set(Key, lang)
		c.Set(bundleKey, b)
		c.Next()
	}
}

// Lang 当前协商语言。
func Lang(c *framework.Context) string {
	v, _ := c.Get(Key)
	s, _ := v.(string)
	return s
}

// T 取当前语言词条（缺 key 返回 key 本身）。
func T(c *framework.Context, key string) string {
	v, _ := c.Get(bundleKey)
	b, _ := v.(*Bundle)
	if b == nil {
		return key
	}
	return b.lookup(Lang(c), key)
}

// Tf 带参词条（{0} {1} 占位替换）。
func Tf(c *framework.Context, key string, args ...any) string {
	s := T(c, key)
	for i, a := range args {
		s = strings.ReplaceAll(s, "{"+itoa(i)+"}", str(a))
	}
	return s
}

// lookup 语言查词条：精确 → 包默认 → key。
func (b *Bundle) lookup(lang, key string) string {
	if m, ok := b.dict[lang]; ok {
		if v, ok := m[key]; ok {
			return v
		}
	}
	if m, ok := b.dict[b.def]; ok {
		if v, ok := m[key]; ok {
			return v
		}
	}
	return key
}

// negotiate 语言协商优先级：query > cookie > Accept-Language > default。
func (b *Bundle) negotiate(c *framework.Context) string {
	if q := c.Query("lang"); q != "" {
		if l := b.match(q); l != "" {
			return l
		}
	}
	if al := c.Header("Accept-Language"); al != "" {
		if l := b.matchQ(al); l != "" {
			return l
		}
	}
	return b.def
}

// match 精确/前缀匹配（zh-Hans → zh）。
func (b *Bundle) match(lang string) string {
	lang = strings.TrimSpace(lang)
	if lang == "" {
		return ""
	}
	lang = strings.ToLower(lang)
	for _, l := range b.sorted {
		if strings.ToLower(l) == lang {
			return l
		}
	}
	base := strings.SplitN(lang, "-", 2)[0]
	for _, l := range b.sorted {
		if strings.HasPrefix(strings.ToLower(l), base) {
			return l
		}
	}
	return ""
}

// matchQ 按 q 值排序的 Accept-Language 匹配。
func (b *Bundle) matchQ(al string) string {
	type pick struct {
		tag string
		q   float64
	}
	var picks []pick
	for _, part := range strings.Split(al, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		tag, params, _ := strings.Cut(part, ";")
		q := 1.0
		for _, p := range strings.Split(params, ";") {
			if kv := strings.TrimSpace(p); strings.HasPrefix(kv, "q=") {
				q = parseFloat(kv[2:])
			}
		}
		if q > 0 {
			picks = append(picks, pick{tag: tag, q: q})
		}
	}
	// 稳定排序：q 降序
	sort.SliceStable(picks, func(i, j int) bool { return picks[i].q > picks[j].q })
	for _, p := range picks {
		if l := b.match(p.tag); l != "" {
			return l
		}
	}
	return ""
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

func str(a any) string {
	if s, ok := a.(string); ok {
		return s
	}
	return itoa(int(fastFloat(a)))
}

func fastFloat(a any) float64 {
	switch v := a.(type) {
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case float64:
		return v
	}
	return 0
}

func parseFloat(s string) float64 {
	var f float64
	var frac float64 = 0.1
	var seenDot bool
	neg := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '-':
			neg = true
		case ch >= '0' && ch <= '9':
			if seenDot {
				f += float64(ch-'0') * frac
				frac /= 10
			} else {
				f = f*10 + float64(ch-'0')
			}
		case ch == '.':
			seenDot = true
		}
	}
	if neg {
		return -f
	}
	return f
}
