//go:build unit

package service

import (
	"testing"
)

// TestBuildCustomRelayURL 锁定 A3 收敛：path 由版本感知原语拼接后，再追加 beta=true 与
// 可选 proxy 查询参数。base 自带版本段时不得出现 /v1/v1 双段，proxy query 追加语义不变。
func TestBuildCustomRelayURL(t *testing.T) {
	s := &GatewayService{}

	cases := []struct {
		name string
		base string
		path string
		want string
	}{
		{
			name: "no version suffix",
			base: "https://upstream.example",
			path: "/v1/messages",
			want: "https://upstream.example/v1/messages?beta=true",
		},
		{
			// 核心回归：base 已带 /v1，只追加相对路径。
			name: "base has /v1",
			base: "https://upstream.example/v1",
			path: "/v1/messages",
			want: "https://upstream.example/v1/messages?beta=true",
		},
		{
			name: "count tokens with /v1 base",
			base: "https://upstream.example/v1",
			path: "/v1/messages/count_tokens",
			want: "https://upstream.example/v1/messages/count_tokens?beta=true",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := s.buildCustomRelayURL(c.base, c.path, &Account{ID: 1})
			if got != c.want {
				t.Fatalf("base=%q path=%q: expected %q, got %q", c.base, c.path, c.want, got)
			}
			if idx := len(got); idx == 0 {
				t.Fatalf("expected non-empty url")
			}
		})
	}
}

// TestBuildCustomRelayURLWithProxyQuery 锁定 proxy 查询参数在原语产出之上正确追加
// （account.ProxyID/Proxy 均非空时追加 &proxy=<escaped>）。
func TestBuildCustomRelayURLWithProxyQuery(t *testing.T) {
	s := &GatewayService{}
	proxyID := int64(7)
	account := &Account{
		ID:      1,
		ProxyID: &proxyID,
		Proxy:   &Proxy{ID: proxyID, Protocol: "http", Host: "127.0.0.1", Port: 7890},
	}

	// base 无版本段：proxy query 追加在原语产出之后。
	if got, want := s.buildCustomRelayURL("https://upstream.example", "/v1/messages", account),
		"https://upstream.example/v1/messages?beta=true&proxy=http%3A%2F%2F127.0.0.1%3A7890"; got != want {
		t.Fatalf("no version suffix: expected %q, got %q", want, got)
	}

	// base 带 /v1 版本段：原语避免双段，proxy query 追加语义不变。
	if got, want := s.buildCustomRelayURL("https://upstream.example/v1", "/v1/messages", account),
		"https://upstream.example/v1/messages?beta=true&proxy=http%3A%2F%2F127.0.0.1%3A7890"; got != want {
		t.Fatalf("base has /v1: expected %q, got %q", want, got)
	}
}
