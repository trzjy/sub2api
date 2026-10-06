package xianguanjia

import (
	"crypto/md5"
	"encoding/hex"
)

// Sign 计算闲管家（开放平台 ERP 方向）请求/推送签名。
//
// 官方契约（reference/open-platform/doc-2686716.md:54-68；SDK client.go:85-98）：
//
//	sign = md5("{appKey},{bodyMd5},{timestamp},{appSecret}")
//
// 四段、英文逗号分隔、无 key 排序、无 mch、无 nonce、无 seller_id。
// appKey 即应用 AppKey（252 表 app_id 下发值），appSecret 即应用 AppSecret（解密后）。
func Sign(appKey, bodyMd5, timestamp, appSecret string) string {
	raw := appKey + "," + bodyMd5 + "," + timestamp + "," + appSecret
	sum := md5.Sum([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// BodyMd5 计算请求体 MD5。body 必须是以"压缩 JSON"（无多余空格）原样参与签名与发送的字节。
// 无 body 时按官方约定返回 md5("{}")（reference/open-platform/doc-2686717.md:19）。
func BodyMd5(body []byte) string {
	if len(body) == 0 {
		body = []byte("{}")
	}
	sum := md5.Sum(body)
	return hex.EncodeToString(sum[:])
}

// Md5Hex 通用 MD5（十六进制小写）。
func Md5Hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}
