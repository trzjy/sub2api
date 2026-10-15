package service

import (
	// 内嵌原版 wasm_crypto_bg.wasm 的纯 Go（wazero）解密实现。
	wasmcrypto "github.com/Wei-Shaw/sub2api/internal/service/wasm"
)

// 本文件是 shareai 原版 zk01 WASM 解密在 service 包的对外入口。
//
// 实现采用 wazero 纯 Go 运行时内嵌原版 wasm_crypto_bg.wasm（见 internal/service/wasm），
// 与 shareai 扩展原版逐字节一致（v3 决策：纯 Go 逆向 HKDF 参数多次未中，故改跑原版 wasm）。
//
// decryptZk01 复刻 wasm_crypto.js 的 exports.zk01 字符串 ABI：每串经 wasm 自身
// malloc 写入内存（ptr+len），第 4 参 key 传 0/0（undefined）；timestamp 必须作为字符串。

// decryptZk01 解密 shareai 返回的 metadata，返回解密后的账号 JSON 字符串。
// metadata/mail/timestamp 均按字符串传入；timestamp 传数字会导致 wasm 报
// "memory access out of bounds"，调用方务必传字符串。
func decryptZk01(metadata, mail, timestamp string) (string, error) {
	return wasmcrypto.DecryptZk01(metadata, mail, timestamp)
}
