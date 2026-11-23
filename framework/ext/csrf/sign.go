// Package csrf：HMAC 签名实现（独立文件便于审计）。
package csrf

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
)

// sign 计算 HMAC-SHA256（base64 RawURL），比对侧使用 hmac.Equal 恒时比较。
func sign(secret []byte, v string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(v))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verify 恒时校验签名。
func verify(secret []byte, v, sig string) bool {
	return hmac.Equal([]byte(sign(secret, v)), []byte(sig))
}
