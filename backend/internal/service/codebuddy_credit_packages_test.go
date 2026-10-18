package service

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// codeBuddyCreditPackagesFixture 读取脱敏精简 fixture
// （源自 ~/.sub2api-acceptance/sub2api-b4-probe-20260929 的 01 号脱敏抓包，
// 保持字段结构，凭证/UIN/AccountId 原值均已替换为占位符）。
func codeBuddyCreditPackagesFixture(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile("testdata/codebuddy_billing_credit_packages.json")
	require.NoError(t, err)
	return body
}

// codeBuddyCreditPackagesBody 组装只含一个分包的最小响应信封。
func codeBuddyCreditPackagesBody(entry string) []byte {
	return []byte(`{"code":0,"msg":"OK","data":{"Response":{"Data":{"TotalCount":1,"TotalDosage":999,"Accounts":[` + entry + `]}}}}`)
}

func mustDecimal(t *testing.T, raw string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(raw)
	require.NoError(t, err)
	return d
}

// codeBuddyCreditPackageKeys 返回序列化后首条分包的 JSON 键顺序（校验契约键名/顺序）。
func codeBuddyCreditPackageKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	var arr []json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &arr))
	require.NotEmpty(t, arr)
	dec := json.NewDecoder(bytes.NewReader(arr[0]))
	tok, err := dec.Token()
	require.NoError(t, err)
	require.Equal(t, json.Delim('{'), tok)
	keys := make([]string, 0, 8)
	for dec.More() {
		key, err := dec.Token()
		require.NoError(t, err)
		keys = append(keys, key.(string))
		if _, err := dec.Token(); err != nil { // 跳过对应值，只收集键
			require.NoError(t, err)
		}
	}
	return keys
}

// TestParseCodeBuddyCreditPackages_ContractAndDerivation 验证 §4.1 契约字段映射、
// NUMERIC(20,8) 量化、Asia/Shanghai→UTC 转换，以及 used_percent 从基础谓词推导
// （Status=3 耗尽包 total 不进分子/分母）。
func TestParseCodeBuddyCreditPackages_ContractAndDerivation(t *testing.T) {
	packages, errEntries, envelopeErr := parseCodeBuddyCreditPackages(codeBuddyCreditPackagesFixture(t), "")
	require.False(t, envelopeErr)
	require.Empty(t, errEntries)
	require.Len(t, packages, 3, "三条分包（含 Status=3）均为合法存储条目")

	// 契约字段顺序/键名逐项核对。
	raw, err := json.Marshal(packages)
	require.NoError(t, err)
	require.Equal(t, []string{"id", "name", "unit", "remaining", "total", "expires_at", "status"},
		codeBuddyCreditPackageKeys(t, raw))

	var decoded []map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	for _, item := range decoded {
		require.Equal(t, codebuddyCreditCanonicalUnit, item["unit"])
		require.IsType(t, "", item["remaining"], "remaining 必须以字符串承载以避免精度损失")
		require.IsType(t, "", item["total"], "total 必须以字符串承载以避免精度损失")
	}

	// 整改②：Status=3（耗尽/过期）包纳入 used_percent 与 totals 的同一参与集合
	// （参与性谓词 codeBuddyCreditParticipatesInPercent 接受 Status∈{0,3}），
	// 不再被稀释为低用量。
	exhausted := packages[0]
	require.Equal(t, int64(codebuddyCreditPackageStatusExhausted), exhausted.Status)
	require.Equal(t, "3000", exhausted.Total.String())

	// ② 参与集合 = Status∈{0,3} 全部三包：Σtotal = 3000+1000+100 = 4100，
	// Σremaining = 0 + 207.49000063 + 100 = 307.49000063；
	// used = (4100 − 307.49000063)/4100 × 100 = 92.500243887。
	usedPercent, ok := DeriveCodeBuddyCreditUsedPercent(packages)
	require.True(t, ok)
	require.InDelta(t, 92.500243887, usedPercent, 1e-6,
		"整改②：Status=3 的 total=3000 须纳入分母（耗尽不再被稀释）")

	// 整改②：totals 同一参与集合（含 Status=3），十进制字符串精确求和。
	totalRemaining, totalTotal := deriveTotalsFromPackages(packages)
	require.Equal(t, "307.49000063", totalRemaining, "Σremaining 含 Status=3 包")
	require.Equal(t, "4100", totalTotal, "Σtotal 含 Status=3 包")

	// Asia/Shanghai 字面量 → UTC RFC3339。
	require.Equal(t, "2026-10-15T21:26:43Z", packages[1].ExpiresAt)
	require.Equal(t, "2027-04-21T08:49:49Z", packages[0].ExpiresAt)
}

// TestParseCodeBuddyCreditPackages_DropsUnknownStatus 未知 Status（合法仅 0/3）整包剔除。
func TestParseCodeBuddyCreditPackages_DropsUnknownStatus(t *testing.T) {
	body := codeBuddyCreditPackagesBody(
		`{"AccountId":"a-1","PackageName":"p","CapacityUnit":"credits","Status":7,"CycleCapacitySizePrecise":"100","CycleCapacityRemainPrecise":"50","CycleEndTime":"2026-10-16 05:26:43"}`)
	packages, errEntries, envelopeErr := parseCodeBuddyCreditPackages(body, "")
	require.False(t, envelopeErr)
	require.Empty(t, packages)
	require.Len(t, errEntries, 1)
	require.Equal(t, "unknown_status", errEntries[0].Reason)
	require.Equal(t, "7", errEntries[0].RawValues["status"], "错误结构须保留原始值供排查")
}

// TestParseCodeBuddyCreditPackages_DropsRemainingGreaterThanTotal remaining>total 整包剔除。
func TestParseCodeBuddyCreditPackages_DropsRemainingGreaterThanTotal(t *testing.T) {
	body := codeBuddyCreditPackagesBody(
		`{"AccountId":"a-1","PackageName":"p","CapacityUnit":"credits","Status":0,"CycleCapacitySizePrecise":"100","CycleCapacityRemainPrecise":"150","CycleEndTime":"2026-10-16 05:26:43"}`)
	packages, errEntries, _ := parseCodeBuddyCreditPackages(body, "")
	require.Empty(t, packages)
	require.Len(t, errEntries, 1)
	require.Equal(t, "remaining_gt_total", errEntries[0].Reason)
}

// TestParseCodeBuddyCreditPackages_DropsNonCanonicalUnit 异单位分包整包剔除（禁止跨单位相加）。
func TestParseCodeBuddyCreditPackages_DropsNonCanonicalUnit(t *testing.T) {
	body := codeBuddyCreditPackagesBody(
		`{"AccountId":"a-1","PackageName":"p","CapacityUnit":"tokens","Status":0,"CycleCapacitySizePrecise":"100","CycleCapacityRemainPrecise":"50","CycleEndTime":"2026-10-16 05:26:43"}`)
	packages, errEntries, _ := parseCodeBuddyCreditPackages(body, "")
	require.Empty(t, packages)
	require.Len(t, errEntries, 1)
	require.Equal(t, "unit_mismatch", errEntries[0].Reason)
	require.Equal(t, "tokens", errEntries[0].RawValues["unit"])
}

// TestParseCodeBuddyCreditPackages_DropsNegativeValue 负值整包剔除（不钳位）。
func TestParseCodeBuddyCreditPackages_DropsNegativeValue(t *testing.T) {
	body := codeBuddyCreditPackagesBody(
		`{"AccountId":"a-1","PackageName":"p","CapacityUnit":"credits","Status":0,"CycleCapacitySizePrecise":"-100","CycleCapacityRemainPrecise":"0","CycleEndTime":"2026-10-16 05:26:43"}`)
	packages, errEntries, _ := parseCodeBuddyCreditPackages(body, "")
	require.Empty(t, packages)
	require.Len(t, errEntries, 1)
	require.Equal(t, "negative_value", errEntries[0].Reason)
}

// TestParseCodeBuddyCreditPackages_ZeroTotalSignal Σtotal=0 → used_percent 不写入信号
// （ok=false；禁止静默写 0 伪装满额/空额）。
func TestParseCodeBuddyCreditPackages_ZeroTotalSignal(t *testing.T) {
	// 全部分包被剔除 → 无有效分包。
	body := codeBuddyCreditPackagesBody(
		`{"AccountId":"a-1","PackageName":"p","CapacityUnit":"tokens","Status":0,"CycleCapacitySizePrecise":"100","CycleCapacityRemainPrecise":"50","CycleEndTime":"2026-10-16 05:26:43"}`)
	packages, _, _ := parseCodeBuddyCreditPackages(body, "")
	_, ok := DeriveCodeBuddyCreditUsedPercent(packages)
	require.False(t, ok)

	// 合法但 total=0 的分包同样使 Σtotal=0 → 信号（与"无分包"同一失败关闭形态）。
	zero := []CodeBuddyCreditPackage{{
		ID: "z", Name: "zero", Unit: codebuddyCreditCanonicalUnit,
		Remaining: mustDecimal(t, "0"), Total: mustDecimal(t, "0"),
		ExpiresAt: "2026-10-15T21:26:43Z", Status: 0,
	}}
	_, ok = DeriveCodeBuddyCreditUsedPercent(zero)
	require.False(t, ok)
}

// TestParseCodeBuddyCreditPackages_AsiaShanghaiToUTCBoundary 跨时区边界：
// Asia/Shanghai 字面量换算 UTC 后可能跨日，必须按 UTC+8 解释。
func TestParseCodeBuddyCreditPackages_AsiaShanghaiToUTCBoundary(t *testing.T) {
	cases := []struct {
		name     string
		cycleEnd string
		wantUTC  string
	}{
		{"跨日前推一天", "2026-10-01 07:59:59", "2026-09-30T23:59:59Z"},
		{"UTC+8 整点", "2026-10-01 08:00:00", "2026-10-01T00:00:00Z"},
		{"当地零点", "2026-10-01 00:00:00", "2026-09-30T16:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := codeBuddyCreditPackagesBody(
				`{"AccountId":"a-1","PackageName":"p","CapacityUnit":"credits","Status":0,"CycleCapacitySizePrecise":"100","CycleCapacityRemainPrecise":"50","CycleEndTime":"` + tc.cycleEnd + `"}`)
			packages, errEntries, _ := parseCodeBuddyCreditPackages(body, "")
			require.Empty(t, errEntries)
			require.Len(t, packages, 1)
			require.Equal(t, tc.wantUTC, packages[0].ExpiresAt)
		})
	}
}

// TestParseCodeBuddyCreditPackages_MissingOrInvalidExpiryKept 到期时间缺失/非法不整包剔除
// （§4.2：仅不计入紧迫度窗口），仍进入存储与 used_percent 求和。
func TestParseCodeBuddyCreditPackages_MissingOrInvalidExpiryKept(t *testing.T) {
	for _, cycleEnd := range []string{"", "not-a-timestamp"} {
		body := codeBuddyCreditPackagesBody(
			`{"AccountId":"a-1","PackageName":"p","CapacityUnit":"credits","Status":0,"CycleCapacitySizePrecise":"100","CycleCapacityRemainPrecise":"40","CycleEndTime":"` + cycleEnd + `"}`)
		packages, errEntries, _ := parseCodeBuddyCreditPackages(body, "")
		require.Empty(t, errEntries)
		require.Len(t, packages, 1)
		require.Equal(t, "", packages[0].ExpiresAt)
		require.True(t, IsValidCodeBuddyCreditPackage(packages[0]),
			"基础谓词不含时间条件，缺失到期时间仍参与 used_percent")
	}
}

// TestParseCodeBuddyCreditPackages_UnknownStatusSetsHasExcluded 验证②排除项：
// 未知 Status（非 0 非 3）分包被整包剔除——不计入 used_percent/totals（parse 不入集合），
// 且其 reason=unknown_status → codeBuddyCreditHasExcludedFromErrors 返回 true（可比性标注）。
// 异单位（unit_mismatch）条目同样使 has_excluded=true。
func TestParseCodeBuddyCreditPackages_UnknownStatusSetsHasExcluded(t *testing.T) {
	body := codeBuddyCreditPackagesBody(
		`{"AccountId":"a-1","PackageName":"p","CapacityUnit":"credits","Status":7,"CycleCapacitySizePrecise":"100","CycleCapacityRemainPrecise":"50","CycleEndTime":"2026-10-16 05:26:43"}`)
	packages, errEntries, envelopeErr := parseCodeBuddyCreditPackages(body, "")
	require.False(t, envelopeErr)
	require.Empty(t, packages, "未知 status 分包不计入参与集合（不计入 used_percent/totals）")
	require.Len(t, errEntries, 1)
	require.Equal(t, "unknown_status", errEntries[0].Reason)
	require.True(t, codeBuddyCreditHasExcludedFromErrors(errEntries),
		"未知 status 条目 → has_excluded=true（可比性标注）")

	// 合法包 + 异单位包混合：仅合法包进集合，异单位条目 → has_excluded=true。
	mixed := codeBuddyCreditPackagesBody(
		`{"AccountId":"a-2","PackageName":"ok","CapacityUnit":"credits","Status":0,"CycleCapacitySizePrecise":"100","CycleCapacityRemainPrecise":"50","CycleEndTime":"2026-10-16 05:26:43"},` +
			`{"AccountId":"a-3","PackageName":"bad","CapacityUnit":"tokens","Status":0,"CycleCapacitySizePrecise":"100","CycleCapacityRemainPrecise":"50","CycleEndTime":"2026-10-16 05:26:43"}`)
	pkgs2, errs2, _ := parseCodeBuddyCreditPackages(mixed, "")
	require.Len(t, pkgs2, 1, "仅合法分包进集合，异单位被剔除")
	require.True(t, codeBuddyCreditHasExcludedFromErrors(errs2),
		"异单位条目 → has_excluded=true（可比性标注）")
}

// TestParseCodeBuddyCreditPackages_CycleFieldsPreferredOverCapacity 周期字段切换
// （CYCLE-20261019-R3）：remaining/total 只取 CycleCapacity*Precise；共存的累计容量
// 字段 Capacity*Precise 一律不采用。
func TestParseCodeBuddyCreditPackages_CycleFieldsPreferredOverCapacity(t *testing.T) {
	body := codeBuddyCreditPackagesBody(
		`{"AccountId":"a-1","PackageName":"p","CapacityUnit":"credits","Status":0,` +
			`"CapacitySizePrecise":"999","CapacityRemainPrecise":"888",` +
			`"CycleCapacitySizePrecise":"100","CycleCapacityRemainPrecise":"40","CycleEndTime":"2026-10-16 05:26:43"}`)
	packages, errEntries, envelopeErr := parseCodeBuddyCreditPackages(body, "")
	require.False(t, envelopeErr)
	require.Empty(t, errEntries)
	require.Len(t, packages, 1)
	require.Equal(t, "100", packages[0].Total.String(), "total 必须取周期字段，不得回退 Capacity*")
	require.Equal(t, "40", packages[0].Remaining.String(), "remaining 必须取周期字段，不得回退 Capacity*")
	// 周期口径 (100−40)/100×100 = 60（Capacity 口径 888/999 会给出 ≈11.11）。
	usedPercent, ok := DeriveCodeBuddyCreditUsedPercent(packages)
	require.True(t, ok)
	require.InDelta(t, 60.0, usedPercent, 1e-9, "used_percent 必须按周期值推导")
}

// TestParseCodeBuddyCreditPackages_MissingOrInvalidCycleFieldsFailClosed 周期字段缺失或
// 非法时失败关闭（整包剔除），即便 Capacity*Precise 存在且合法也不得回退。
func TestParseCodeBuddyCreditPackages_MissingOrInvalidCycleFieldsFailClosed(t *testing.T) {
	// 周期字段完全缺失（仅 Capacity*Precise）→ 整包剔除。
	missing := codeBuddyCreditPackagesBody(
		`{"AccountId":"a-1","PackageName":"p","CapacityUnit":"credits","Status":0,` +
			`"CapacitySizePrecise":"100","CapacityRemainPrecise":"40","CycleEndTime":"2026-10-16 05:26:43"}`)
	packages, errEntries, envelopeErr := parseCodeBuddyCreditPackages(missing, "")
	require.False(t, envelopeErr)
	require.Empty(t, packages, "周期字段缺失必须整包剔除，不得回退 Capacity*")
	require.Len(t, errEntries, 1)
	require.Equal(t, "invalid_remaining", errEntries[0].Reason)
	require.Equal(t, "", errEntries[0].RawValues["remaining"], "rawValues 反映周期字段（缺失为空）")

	// 周期字段非法（非定点字符串）+ Capacity*Precise 合法 → 同样失败关闭。
	invalid := codeBuddyCreditPackagesBody(
		`{"AccountId":"a-2","PackageName":"p","CapacityUnit":"credits","Status":0,` +
			`"CapacitySizePrecise":"100","CapacityRemainPrecise":"40",` +
			`"CycleCapacitySizePrecise":"100","CycleCapacityRemainPrecise":"not-a-number","CycleEndTime":"2026-10-16 05:26:43"}`)
	packages2, errEntries2, _ := parseCodeBuddyCreditPackages(invalid, "")
	require.Empty(t, packages2)
	require.Len(t, errEntries2, 1)
	require.Equal(t, "invalid_remaining", errEntries2[0].Reason)
	require.Equal(t, "not-a-number", errEntries2[0].RawValues["remaining"])
}

// TestParseCodeBuddyCreditPackages_ExhaustedParticipatesInPercentButNotUrgency 基线语义
// 保持：Status=3 参与 used_percent/totals（codeBuddyCreditParticipatesInPercent），
// 但基础谓词与 urgency 过滤仍排除它。
func TestParseCodeBuddyCreditPackages_ExhaustedParticipatesInPercentButNotUrgency(t *testing.T) {
	body := codeBuddyCreditPackagesBody(
		`{"AccountId":"a-1","PackageName":"p","CapacityUnit":"credits","Status":3,` +
			`"CycleCapacitySizePrecise":"100","CycleCapacityRemainPrecise":"0","CycleEndTime":"2026-10-16 05:26:43"}`)
	packages, errEntries, _ := parseCodeBuddyCreditPackages(body, "")
	require.Empty(t, errEntries)
	require.Len(t, packages, 1)
	require.True(t, codeBuddyCreditParticipatesInPercent(packages[0]), "status=3 参与 used_percent/totals")
	require.False(t, IsValidCodeBuddyCreditPackage(packages[0]), "基础谓词仍排除 status=3")
	require.Empty(t, codeBuddyFilterUrgencyPackages(packages), "urgency 仍排除 status=3")

	usedPercent, ok := DeriveCodeBuddyCreditUsedPercent(packages)
	require.True(t, ok)
	require.InDelta(t, 100.0, usedPercent, 1e-9, "耗尽包全额计入已用")
	totalRemaining, totalTotal := deriveTotalsFromPackages(packages)
	require.Equal(t, "0", totalRemaining)
	require.Equal(t, "100", totalTotal)
}

// TestIsValidCodeBuddyCreditPackage_ExcludesExhausted 基础谓词：字段有效 + 非 Status=3。
func TestIsValidCodeBuddyCreditPackage_ExcludesExhausted(t *testing.T) {
	base := CodeBuddyCreditPackage{
		ID: "a", Name: "p", Unit: codebuddyCreditCanonicalUnit,
		Remaining: mustDecimal(t, "10"), Total: mustDecimal(t, "100"),
		ExpiresAt: "2026-10-15T21:26:43Z", Status: codebuddyCreditPackageStatusActive,
	}
	require.True(t, IsValidCodeBuddyCreditPackage(base))

	exhausted := base
	exhausted.Status = codebuddyCreditPackageStatusExhausted
	require.False(t, IsValidCodeBuddyCreditPackage(exhausted), "耗尽包不参与 used_percent")

	badUnit := base
	badUnit.Unit = "tokens"
	require.False(t, IsValidCodeBuddyCreditPackage(badUnit))

	overRemain := base
	overRemain.Remaining = mustDecimal(t, "101")
	require.False(t, IsValidCodeBuddyCreditPackage(overRemain))
}

// TestCodeBuddyQuotaService_SetBillingHeaders_DomainThreeStates §4.1 唯一例外：
// 空 domain 时占位头必须是 X-No-Authorization: 1（而非 X-No-Domain）。
func TestCodeBuddyQuotaService_SetBillingHeaders_DomainThreeStates(t *testing.T) {
	svc := &CodeBuddyQuotaService{}
	cases := []struct {
		name         string
		site         string
		domain       string
		wantOrigin   string
		wantDomain   string
		wantPlacehdr bool
	}{
		{name: "cn 有 domain", site: CodeBuddySiteCN, domain: "cn.example.com", wantOrigin: "https://www.codebuddy.cn", wantDomain: "cn.example.com"},
		{name: "intl 有 domain", site: CodeBuddySiteIntl, domain: "intl.example.ai", wantOrigin: "https://www.codebuddy.ai", wantDomain: "intl.example.ai"},
		{name: "空 domain", site: CodeBuddySiteCN, domain: "", wantOrigin: "https://www.codebuddy.cn", wantPlacehdr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{
				ID: 1, Platform: PlatformCodeBuddy, Type: AccountTypeOAuth,
				Credentials: map[string]any{
					"access_token": "test-token",
					"uid":          "u-1",
					"domain":       tc.domain,
					"site":         tc.site,
				},
			}
			req := httptest.NewRequest(http.MethodPost, "https://upstream.example", nil)
			svc.setBillingHeaders(req, account)

			require.Equal(t, tc.wantOrigin, req.Header.Get("Origin"))
			require.Equal(t, tc.wantOrigin+"/", req.Header.Get("Referer"))
			if tc.wantPlacehdr {
				require.Equal(t, "1", req.Header.Get("X-No-Authorization"))
				require.Empty(t, req.Header.Get("X-Domain"))
				require.Empty(t, req.Header.Get("X-No-Domain"),
					"空 domain 占位头不得为 X-No-Domain（§4.1 唯一具名例外）")
			} else {
				require.Equal(t, tc.wantDomain, req.Header.Get("X-Domain"))
				require.Empty(t, req.Header.Get("X-No-Authorization"))
				require.Empty(t, req.Header.Get("X-No-Domain"))
			}
		})
	}
}
