//go:build unit

// DEVIN-PROBE 定向测试：账号健康恢复探针的 devin（Cascade）分支。
//
// 覆盖：
//   - probeUpstream 把 devin 账号改派 devin 专用探针（不再落数据面报
//     "probe account has no api key" 而永远解封不了）；
//   - probeDevinUpstream 的失败关闭路径（空 token / 无 concrete 模型键 / 映射不到
//     上游 uid / 非 200 / END_STREAM trailer error / 空正文）；
//   - 成功路径：收到非空正文 → (true, nil)。
package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// devinAuthTransport 拦截 GetUserJwt 的出站 HTTP，返回预置的 JWT protobuf 响应，
// 避免单测真实访问 server.codeium.com。仅覆盖 GetUserJwt（聊天请求走注入的
// devinProbeUpstream，不经 DefaultTransport）。
type devinAuthTransport struct{ jwt, base string }

func (rt devinAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	e := &ProtoEncoder{}
	e.String(1, rt.jwt)  // GetUserJwtResponse.UserJWT
	e.String(2, rt.base) // GetUserJwtResponse.CustomAPIServerURL
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(e.Bytes())),
		Request:    req,
	}, nil
}

func devinProbeWithAuth(t *testing.T, jwt, base string) {
	t.Helper()
	old := http.DefaultTransport
	http.DefaultTransport = devinAuthTransport{jwt: jwt, base: base}
	t.Cleanup(func() { http.DefaultTransport = old })
}

type devinProbeUpstream struct {
	status int
	body   []byte
	err    error
}

func (u *devinProbeUpstream) response() (*http.Response, error) {
	if u.err != nil {
		return nil, u.err
	}
	return &http.Response{
		StatusCode: u.status,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(u.body)),
	}, nil
}

func (u *devinProbeUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.response()
}

func (u *devinProbeUpstream) DoWithTLS(_ *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.response()
}

func newDevinProbeAccount(id int64, token string, mapping map[string]any) *Account {
	creds := map[string]any{"api_key": token}
	if mapping != nil {
		creds["model_mapping"] = mapping
	}
	return &Account{ID: id, Platform: PlatformDevin, Credentials: creds}
}

func newDevinProbeService(up HTTPUpstream) *AccountHealthRecoveryProbeService {
	return &AccountHealthRecoveryProbeService{
		cfg:                 &config.Config{},
		httpUpstream:        up,
		tlsFPProfileService: &TLSFingerprintProfileService{},
	}
}

// devinFrame 拼一个 Connect 帧：1 字节 flags + 4 字节大端长度 + payload（不压缩）。
func devinFrame(flags byte, payload []byte) []byte {
	frame := make([]byte, 5+len(payload))
	frame[0] = flags
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame
}

func devinTextFrame(text string) []byte {
	e := &ProtoEncoder{}
	e.String(3, text) // GetChatMessageResponse.DeltaText
	return devinFrame(0, e.Bytes())
}

func devinEndFrame(jsonPayload string) []byte {
	return devinFrame(ConnectEndStreamFlag, []byte(jsonPayload))
}

func devinOKStream() []byte {
	return append(devinTextFrame("ok"), devinEndFrame("{}")...)
}

// 分支分派：devin 账号空 token 时报 devin 专属错误，证明走的是 devin 分支，
// 而非数据面的 "probe account has no api key"。
func TestProbeUpstreamDevinBranch(t *testing.T) {
	p := newDevinProbeService(&devinProbeUpstream{status: http.StatusOK})
	acc := newDevinProbeAccount(1, "", map[string]any{"gpt-probe": "upstream-uid"})
	_, err := p.probeUpstream(context.Background(), acc)
	require.Error(t, err)
	require.Contains(t, err.Error(), "devin account has no api key")
	require.False(t, strings.Contains(err.Error(), "probe account has no api key"))
}

// 鉴权前的失败关闭路径：不触发任何出站请求。
func TestProbeDevinUpstreamFailClosedBeforeAuth(t *testing.T) {
	p := newDevinProbeService(&devinProbeUpstream{status: http.StatusOK})
	ctx := context.Background()

	t.Run("empty token", func(t *testing.T) {
		acc := newDevinProbeAccount(2, "  ", map[string]any{"gpt-probe": "upstream-uid"})
		ok, err := p.probeDevinUpstream(ctx, acc)
		require.False(t, ok)
		require.EqualError(t, err, "devin account has no api key")
	})

	t.Run("no concrete model key", func(t *testing.T) {
		acc := newDevinProbeAccount(3, "token", map[string]any{"*": "upstream-uid"})
		ok, err := p.probeDevinUpstream(ctx, acc)
		require.False(t, ok)
		require.ErrorContains(t, err, "no testable model")
	})

	t.Run("model maps to empty uid", func(t *testing.T) {
		acc := newDevinProbeAccount(4, "token", map[string]any{"gpt-probe": ""})
		ok, err := p.probeDevinUpstream(ctx, acc)
		require.False(t, ok)
		require.ErrorContains(t, err, "no mapped upstream model")
	})
}

// 鉴权后聊天阶段的成功/失败判定。
func TestProbeDevinUpstreamChatOutcome(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       []byte
		wantOK     bool
		errContain string
	}{
		{"non-200", http.StatusBadGateway, devinOKStream(), false, "upstream status 502"},
		{"end stream trailer error", http.StatusOK, append(devinTextFrame("ok"), devinEndFrame(`{"error":{"code":"rate_limited","message":"quota"}}`)...), false, "trailer error"},
		{"empty text", http.StatusOK, devinEndFrame("{}"), false, "no text"},
		{"ok", http.StatusOK, devinOKStream(), true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			devinProbeWithAuth(t, "jwt-test", "https://chat.example.com")
			p := newDevinProbeService(&devinProbeUpstream{status: tc.status, body: tc.body})
			acc := newDevinProbeAccount(10, "session-token", map[string]any{"gpt-probe": "upstream-uid"})
			ok, err := p.probeDevinUpstream(context.Background(), acc)
			require.Equal(t, tc.wantOK, ok)
			if tc.errContain == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.ErrorContains(t, err, tc.errContain)
			}
		})
	}
}
