package service

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// 已知向量：与参考实现（aiodeepseek 23 轮 Keccak 语义）同构的独立 Go 实现生成，
// 用于锁定 webDeepseekPowStateDigest 的位级正确性。
func TestWebDeepseekPowStateDigest_KnownVectors(t *testing.T) {
	base := "salt123_1739764288699_"
	vectors := map[int64]string{
		0:     "369a2319faf63a9d8af03c55d41c6ba3c0f08824643f069874d5b1bdb86f6fd8",
		42:    "6de3393aba4cece63e3e6a761752722b05f2cfe531bc1b5c82e01985e93fddd2",
		77906: "ce810d6ec165115438096ce6cf0b11fda1e99fd95ff670b48e8d8347260e18a4",
	}
	for nonce, want := range vectors {
		msg := base + itoa(nonce)
		got := webDeepseekPowStateDigest([]byte(msg))
		require.Equalf(t, want, hexString(got[:]), "digest mismatch for nonce %d", nonce)
	}
}

func TestWebDeepseekSolvePoW_FindsNonce(t *testing.T) {
	// 用 nonce=42 的摘要构造挑战，验证求解器收敛到 42。
	challengeHex := "6de3393aba4cece63e3e6a761752722b05f2cfe531bc1b5c82e01985e93fddd2"
	challenge := webDeepseekPowChallenge{
		Algorithm:  "DeepSeekHashV1",
		Challenge:  challengeHex,
		Salt:       "salt123",
		Signature:  "sig",
		Difficulty: 144000,
		ExpireAt:   1739764288699,
	}
	header, err := webDeepseekSolvePoW(challenge, "/api/v0/chat/completion")
	require.NoError(t, err)

	payload, err := base64.StdEncoding.DecodeString(header)
	require.NoError(t, err)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(payload, &parsed))
	require.Equal(t, "DeepSeekHashV1", parsed["algorithm"])
	require.Equal(t, challengeHex, parsed["challenge"])
	require.Equal(t, "salt123", parsed["salt"])
	require.Equal(t, "sig", parsed["signature"])
	require.Equal(t, "/api/v0/chat/completion", parsed["target_path"])
	// answer 为数值（非字符串）。
	answer, ok := parsed["answer"].(float64)
	require.True(t, ok, "answer must be a number")
	require.Equal(t, float64(42), answer)
}

func TestWebDeepseekSolvePoW_NoNonceWithinDifficulty(t *testing.T) {
	challenge := webDeepseekPowChallenge{
		Algorithm:  "DeepSeekHashV1",
		Challenge:  "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		Salt:       "salt123",
		Difficulty: 100,
		ExpireAt:   1739764288699,
	}
	_, err := webDeepseekSolvePoW(challenge, "/api/v0/chat/completion")
	require.ErrorIs(t, err, ErrWebDeepseekPoWNotImplemented)
}

func TestWebDeepseekSolvePoW_InputValidation(t *testing.T) {
	base := webDeepseekPowChallenge{Salt: "s", Difficulty: 10, ExpireAt: 1}

	// 不支持的算法失败关闭。
	_, err := webDeepseekSolvePoW(webDeepseekPowChallenge{Algorithm: "OtherV2", Challenge: "00", Salt: "s", Difficulty: 10}, "p")
	require.ErrorIs(t, err, ErrWebDeepseekPoWNotImplemented)

	// 非 64 位 hex 失败关闭。
	_, err = webDeepseekSolvePoW(webDeepseekPowChallenge{Algorithm: "DeepSeekHashV1", Challenge: "abc", Salt: "s", Difficulty: 10}, "p")
	require.ErrorIs(t, err, ErrWebDeepseekPoWNotImplemented)

	// 非正难度失败关闭。
	_, err = webDeepseekSolvePoW(webDeepseekPowChallenge{Algorithm: "DeepSeekHashV1", Challenge: "369a2319faf63a9d8af03c55d41c6ba3c0f08824643f069874d5b1bdb86f6fd8", Salt: "s", Difficulty: 0}, "p")
	require.ErrorIs(t, err, ErrWebDeepseekPoWNotImplemented)

	// 空 challenge / salt 失败关闭。
	_, err = webDeepseekSolvePoW(base, "p")
	require.ErrorIs(t, err, ErrWebDeepseekPoWNotImplemented)
}

func TestWebDeepseekExtractPoWChallenge_BizDataPath(t *testing.T) {
	// 实测响应结构（91fans 抓包）：data.biz_data.challenge。
	body := []byte(`{"code":0,"msg":"","data":{"biz_code":0,"biz_msg":"","biz_data":{"challenge":{"algorithm":"DeepSeekHashV1","challenge":"cebab4aa8e50955666f589816c66811144e056a8f41c43a43bd78cedc4b5f4a1","salt":"8360f8c9205c96c32b7a","signature":"10b7849b1e6203b288dc4975a1e23753ed9d6fb656964b8e86daebab3628a40c","difficulty":144000,"expire_at":1739764288699,"expire_after":300000,"target_path":"/api/v0/chat/completion"}}}}`)
	challenge, ok := webDeepseekExtractPoWChallenge(body)
	require.True(t, ok)
	require.Equal(t, "DeepSeekHashV1", challenge.Algorithm)
	require.Equal(t, "cebab4aa8e50955666f589816c66811144e056a8f41c43a43bd78cedc4b5f4a1", challenge.Challenge)
	require.Equal(t, "8360f8c9205c96c32b7a", challenge.Salt)
	require.Equal(t, int64(144000), challenge.Difficulty)
	require.Equal(t, int64(1739764288699), challenge.ExpireAt)
}

func TestWebDeepseekExtractPoWChallenge_MissingTokenShape(t *testing.T) {
	// 未登录态实测形态：无 challenge → false。
	body := []byte(`{"code":40002,"msg":"Missing Token"}`)
	_, ok := webDeepseekExtractPoWChallenge(body)
	require.False(t, ok)
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}

func hexString(b []byte) string {
	return hex.EncodeToString(b)
}
