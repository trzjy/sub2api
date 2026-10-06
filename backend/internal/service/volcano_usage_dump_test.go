package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"
)

// TestDumpVolcanoUsageRaw 手动诊断：打印火山管理面用量接口的原始返回（含 Quota 绝对值）。
// 运行：VOLCANO_AK=... VOLCANO_SK=... VOLCANO_ACTION=GetAFPUsage go test -run TestDumpVolcanoUsageRaw -v
func TestDumpVolcanoUsageRaw(t *testing.T) {
	ak, sk := os.Getenv("VOLCANO_AK"), os.Getenv("VOLCANO_SK")
	action := os.Getenv("VOLCANO_ACTION")
	if ak == "" || sk == "" || action == "" {
		t.Skip("VOLCANO_AK/SK/ACTION not set")
	}
	query := url.Values{}
	query.Set("Action", action)
	query.Set("Version", "2024-01-01")
	canonQuery, signedHeaders, err := volcEngineSignQuery(ak, sk, volcanoQuotaRegion, volcanoQuotaService, volcanoQuotaHost, query, time.Now().UTC())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+volcanoQuotaHost+"/?"+canonQuery, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for name, values := range signedHeaders {
		for _, v := range values {
			req.Header.Set(name, v)
		}
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	fmt.Printf("HTTP %d\n%s\n", resp.StatusCode, string(body))
}
