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
// 算法依据（公开实现相互印证，登录态抓包最终确认前作为当前权威路径）：
//   - aiodeepseek C++ 求解器（github.com/boykopovar/aiodeepseek aiodeepseek/pow/_pow.cpp）：
//     输入 base = f"{salt}_{expire_at}_"，找 nonce ∈ [0, difficulty) 使
//     23 轮 Keccak-256（第一轮使用 rc[1]，省略标准第 24 轮）对
//     base + strconv(nonce) 的状态前 32 字节（lane 0-3 小端）与 challenge hex 全等；
//     SHA-3 终止填充 0x06，块尾 0x80，RATE=136。
//   - x-ds-pow-response 头：base64(JSON{algorithm, challenge, salt, signature,
//     answer(数值), target_path})，JSON 无空格分隔（jishuzhan.net 逆向分析 +
//     91fans.com.cn 实测抓包一致）。
//
// 与标准 Keccak-256 的差异仅在轮数（23 轮）；x/crypto/sha3 的 keccakF1600
// 是 24 轮且不可配置，因此这里内联 23 轮置换（轮常数表与标准一致）。

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
type webDeepseekPowChallenge struct {
	Algorithm  string `json:"algorithm"`
	Challenge  string `json:"challenge"`
	Salt       string `json:"salt"`
	Signature  string `json:"signature"`
	Difficulty int64  `json:"difficulty"`
	ExpireAt   int64  `json:"expire_at"`
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
