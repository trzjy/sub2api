package service

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// DeepSeek 网页版 PoW（DeepSeekHashV1）求解器。
//
// ============================================================================
// 算法取证依据（官方来源，已实证，非猜测）
// ----------------------------------------------------------------------------
// 官方前端主包引用 WASM：
//   https://fe-static.deepseek.com/chat/static/sha3_wasm_bg.7b9ca65ddd.wasm
// （留档 /tmp/sha3_wasm_bg.wasm，source 路径戳 sha3-wasm/src/lib.rs）。
// 该 wasm 导出两个函数：
//   - wasm_deepseek_hash_v1(ret_slot, input_ptr, input_len)：
//       对输入字节串做 DeepSeekHashV1 摘要，结果以 64 字符小写 hex 字符串写回
//       ret_slot（bindgen &str -> String 约定：首参为返回槽 (ptr,len)）。
//   - wasm_solve(ret_slot, challenge_ptr, challenge_len, salt_ptr, salt_len,
//       difficulty:i32, expire_at:i64)：在 nonce ∈ [0, difficulty) 内暴力搜索，
//       直到 hash(salt_expire_nonce) 的 32 字节与 challenge 字节全等（wasm 内
//       为 32 次 i32.ne + br_if 的逐字节比较循环，非前导零阈值），返回命中的
//       nonce 数值。
//
// 本实现与官方 wasm 的实证对照（输入串 = salt + "_" + expire_at + "_" + nonce，
// 十进制拼 nonce；expire_at 取 challenge.expire_at 的十进制串）：
//   salt123_1739764288699_0     -> 369a2319faf63a9d8af03c55d41c6ba3c0f08824643f069874d5b1bdb86f6fd8
//   salt123_1739764288699_42    -> 6de3393aba4cece63e3e6a761752722b05f2cfe531bc1b5c82e01985e93fddd2
//   salt123_1739764288699_77906 -> ce810d6ec165115438096ce6cf0b11fda1e99fd95ff670b48e8d8347260e18a4
// 以上三行由官方 wasm_deepseek_hash_v1 与 webDeepseekPowStateDigest 各自独立计算，
// 输出逐字节一致（见 web_deepseek_pow_test.go 已知向量用例）。
//
// 摘要内部语义（与官方一致）：23 轮 Keccak-f[1600]（首轮使用 rc[1]，省略标准第
// 24 轮；轮常数表与标准一致），RATE=136，SHA-3 终止填充 0x06 + 块尾 0x80，取状态
// lane 0-3（32 字节小端）。x/crypto/sha3 的 keccakF1600 是 24 轮且不可配置，故内联
// 23 轮置换。
//
// 社区逆向（aiodeepseek C++ 求解器 github.com/boykopovar/aiodeepseek、jishuzhan.net
// 逆向、91fans.com.cn 实测抓包）结论与上述官方取证相互印证，仅作为交叉佐证。
// ============================================================================

// webDeepseekKeccakRC Keccak-f[1600] 轮常数（与标准 24 轮一致；23 轮实现用 rc[1..23]）。
var webDeepseekKeccakRC = [24]uint64{
	0x0000000000000001, 0x0000000000008082, 0x800000000000808A, 0x8000000080008000,
	0x000000000000808B, 0x0000000080000001, 0x8000000080008081, 0x8000000000008009,
	0x000000000000008A, 0x0000000000000088, 0x0000000080008009, 0x000000008000000A,
	0x000000008000808B, 0x800000000000008B, 0x8000000000008089, 0x8000000000008003,
	0x8000000000008002, 0x8000000000000080, 0x000000000000800A, 0x800000008000000A,
	0x8000000080008081, 0x8000000000008080, 0x0000000080000001, 0x8000000080008008,
}

// webDeepseekKeccakRotations ρ 步旋转偏移（lane (x,y) → r[x][y]）。
var webDeepseekKeccakRotations = [25]uint{
	0, 1, 62, 28, 27,
	36, 44, 6, 55, 20,
	3, 10, 43, 25, 39,
	41, 45, 15, 21, 8,
	18, 2, 61, 56, 14,
}

// webDeepseekKeccakF23 对 25 个 lane 执行 23 轮 Keccak-f[1600] 置换。
// 轮常数从 rc[1] 开始（DeepSeekHashV1 变体，省略 rc[0] 的第 1 轮）。
func webDeepseekKeccakF23(a *[25]uint64) {
	for round := 1; round <= 23; round++ {
		// θ
		var c [5]uint64
		for x := 0; x < 5; x++ {
			c[x] = a[x] ^ a[x+5] ^ a[x+10] ^ a[x+15] ^ a[x+20]
		}
		var d [5]uint64
		for x := 0; x < 5; x++ {
			d[x] = c[(x+4)%5] ^ (c[(x+1)%5]<<1 | c[(x+1)%5]>>63)
		}
		for x := 0; x < 5; x++ {
			for y := 0; y < 5; y++ {
				a[x+5*y] ^= d[x]
			}
		}
		// ρ + π
		var b [25]uint64
		for x := 0; x < 5; x++ {
			for y := 0; y < 5; y++ {
				src := x + 5*y
				dst := y + 5*((2*x+3*y)%5)
				b[dst] = a[src]<<webDeepseekKeccakRotations[src] | a[src]>>(64-webDeepseekKeccakRotations[src])
			}
		}
		// χ
		for y := 0; y < 5; y++ {
			for x := 0; x < 5; x++ {
				a[x+5*y] = b[x+5*y] ^ (^b[(x+1)%5+5*y] & b[(x+2)%5+5*y])
			}
		}
		// ι
		a[0] ^= webDeepseekKeccakRC[round]
	}
}

// webDeepseekPowStateDigest 对 message 做 23 轮 Keccak（DeepSeekHashV1 摘要语义），
// 返回状态 lane 0-3（32 字节，小端）。
func webDeepseekPowStateDigest(message []byte) [32]byte {
	const rate = 136
	var state [25]uint64

	// 吸收完整块（除最后一块）。
	off := 0
	for len(message)-off >= rate {
		for i := 0; i < rate/8; i++ {
			state[i] ^= binary.LittleEndian.Uint64(message[off+i*8:])
		}
		webDeepseekKeccakF23(&state)
		off += rate
	}

	// 最后一块：0x06 填充 + 块尾 0x80。
	var block [rate]byte
	copy(block[:], message[off:])
	block[len(message)-off] = 0x06
	block[rate-1] = 0x80
	for i := 0; i < rate/8; i++ {
		state[i] ^= binary.LittleEndian.Uint64(block[i*8:])
	}
	webDeepseekKeccakF23(&state)

	var digest [32]byte
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint64(digest[i*8:], state[i])
	}
	return digest
}

// webDeepseekPowChallenge 挑战载荷（create_pow_challenge 响应 data.biz_data.challenge）。
// 字段结构与登录态实测（09 §3）一致：algorithm/challenge/salt/signature/difficulty/
// expire_at/expire_after/target_path。其中 expire_after 为有效期（毫秒，本次求解未使用），
// target_path 在打包出站头时由 webDeepseekSolvePoW 的 targetPath 参数提供。
type webDeepseekPowChallenge struct {
	Algorithm   string `json:"algorithm"`
	Challenge   string `json:"challenge"`
	Salt        string `json:"salt"`
	Signature   string `json:"signature"`
	Difficulty  int64  `json:"difficulty"`
	ExpireAt    int64  `json:"expire_at"`
	ExpireAfter int64  `json:"expire_after"`
	TargetPath  string `json:"target_path"`
}

// webDeepseekSolvePoW 求解挑战，返回出站 x-ds-pow-response 头值
// （base64(JSON{algorithm, challenge, salt, signature, answer, target_path})）。
// 求解失败（nonce 未收敛）返回错误，不伪造应答。
func webDeepseekSolvePoW(challenge webDeepseekPowChallenge, targetPath string) (string, error) {
	if challenge.Challenge == "" || challenge.Salt == "" {
		return "", fmt.Errorf("%w: incomplete challenge", ErrWebDeepseekPoWNotImplemented)
	}
	if challenge.Algorithm != "" && challenge.Algorithm != "DeepSeekHashV1" {
		return "", fmt.Errorf("%w: unsupported algorithm %q", ErrWebDeepseekPoWNotImplemented, challenge.Algorithm)
	}
	challengeHex := strings.ToLower(strings.TrimSpace(challenge.Challenge))
	if len(challengeHex) != 64 {
		return "", fmt.Errorf("%w: challenge must be 64-char hex", ErrWebDeepseekPoWNotImplemented)
	}
	if _, err := hex.DecodeString(challengeHex); err != nil {
		return "", fmt.Errorf("%w: invalid challenge hex: %v", ErrWebDeepseekPoWNotImplemented, err)
	}
	if challenge.Difficulty <= 0 {
		return "", fmt.Errorf("%w: non-positive difficulty %d", ErrWebDeepseekPoWNotImplemented, challenge.Difficulty)
	}

	want, err := hex.DecodeString(challengeHex)
	if err != nil {
		return "", fmt.Errorf("%w: invalid challenge hex: %v", ErrWebDeepseekPoWNotImplemented, err)
	}

	// base = f"{salt}_{expire_at}_"；nonce 十进制拼在尾部。
	base := challenge.Salt + "_" + strconv.FormatInt(challenge.ExpireAt, 10) + "_"
	baseBytes := []byte(base)
	if len(baseBytes) > 136-20 { // RATE - MAX_NONCE_DEC（nonce ≤ difficulty ≤ 20 位十进制）
		return "", fmt.Errorf("%w: base prefix too long", ErrWebDeepseekPoWNotImplemented)
	}

	var nonceBuf [20]byte
	for nonce := int64(0); nonce < challenge.Difficulty; nonce++ {
		n := strconv.AppendInt(nonceBuf[:0], nonce, 10)
		msg := make([]byte, 0, len(baseBytes)+len(n))
		msg = append(msg, baseBytes...)
		msg = append(msg, n...)
		digest := webDeepseekPowStateDigest(msg)
		if [32]byte(digest) == ([32]byte)(want) {
			return webDeepseekPackPoWResponse(challenge, nonce, targetPath)
		}
	}
	return "", fmt.Errorf("%w: nonce not found within difficulty %d", ErrWebDeepseekPoWNotImplemented, challenge.Difficulty)
}

// webDeepseekPackPoWResponse 打包出站头值（base64(JSON)，无空格分隔，answer 为数值）。
func webDeepseekPackPoWResponse(challenge webDeepseekPowChallenge, answer int64, targetPath string) (string, error) {
	payload, err := json.Marshal(map[string]any{
		"algorithm":   challenge.Algorithm,
		"challenge":   challenge.Challenge,
		"salt":        challenge.Salt,
		"signature":   challenge.Signature,
		"answer":      answer,
		"target_path": targetPath,
	})
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(payload), nil
}

// webDeepseekExtractPoWChallenge 从挑战响应中提取 challenge 载荷。
// 实测响应路径 data.biz_data.challenge（91fans.com.cn 抓包 + aiodeepseek 参考实现一致）；
// 顶层 challenge / data.challenge 为旧版兜底位置，结构未识别返回空。
func webDeepseekExtractPoWChallenge(body []byte) (webDeepseekPowChallenge, bool) {
	for _, path := range []string{"data.biz_data.challenge", "challenge", "data.challenge"} {
		raw := gjson.GetBytes(body, path)
		if !raw.IsObject() {
			continue
		}
		var challenge webDeepseekPowChallenge
		if err := json.Unmarshal([]byte(raw.Raw), &challenge); err == nil && challenge.Challenge != "" {
			return challenge, true
		}
	}
	return webDeepseekPowChallenge{}, false
}

// webDeepseekPowRandomPadding 生成 WAF 侧随机填充（预留；当前未使用，登录态实测后收敛）。
func webDeepseekPowRandomPadding(n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	return buf, nil
}
