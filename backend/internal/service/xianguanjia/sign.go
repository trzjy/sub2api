package xianguanjia

import (
	"crypto/md5"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
)

// Sign 计算闲管家请求签名（占位实现，仅与单测自洽）。
//
// 拼接顺序：把除 sign 外的所有参数按 key 升序拼接为 k=v& 形式，并把 body 的 md5 作为伪参数
// body_md5 加入排序集合，最后追加 appSecret 与 mchSecret，整体再做一次 md5。
//
// 重要：此实现为与单测自洽的占位，上线前须按闲管家 README 校正真实参与签名的字段集合与拼接顺序。
// 本函数不读取任何真实商户凭证——密钥由调用方以参数传入。
func Sign(params url.Values, body []byte, appSecret, mchSecret string) string {
	p := url.Values{}
	for k, v := range params {
		p[k] = v
	}
	p.Set("body_md5", md5hex(body))
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(p.Get(k))
		b.WriteString("&")
	}
	b.WriteString(appSecret)
	b.WriteString(mchSecret)
	return md5hex([]byte(b.String()))
}

func md5hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}
