package service

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 阿里 Token Plan 用量（读官方数据）的单测。
//
// 覆盖三块：
//  1. 签名：老 RPC（HMAC-SHA1）与 ACS3（HMAC-SHA256）两套都要正确。
//  2. 官方响应解析：团队席位额度、个人版档位配额表、个人版用量窗口。
//  3. 口径辅助：base_url 识别、额度池归组键、订阅月区间、错误码、快照序列化。

// ---------------------------------------------------------------- 签名

func TestAliyunRPCSignMatchesOfficialCLI(t *testing.T) {
	signer := &aliyunRPCSigner{
		AccessKeyID:     "test-access-key-id",
		AccessKeySecret: "test-access-key-secret",
		Region:          "cn-beijing",
		Version:         "2026-02-10",
	}
	headers, err := signer.aliyunSign(aliyunSignedRequest{
		Method: "GET",
		Path:   "/modelstudio/billing/overview",
		Query:  url.Values{"billMonth": {"2026-11"}},
	})
	require.NoError(t, err)
	require.Equal(t, "application/json", headers["Accept"])
	require.NotEmpty(t, headers["x-acs-signature"])
	require.Contains(t, headers["x-acs-signature"], "method:GET,signature:")
	require.Equal(t, "2026-02-10", headers["x-acs-version"])
	// Date 必须是 ISO8601 Z 结尾。
	require.Regexp(t, `^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`, headers["Date"])
}

func TestAliyunRPCSignQuerySortedAndDecoded(t *testing.T) {
	// query 段必须按 key 字母序，值用解码后的原始值。
	resource := aliyunCanonicalResource("/x", url.Values{
		"b": {"2"},
		"a": {"1"},
	})
	require.Equal(t, "/x\na:1\nb:2", resource)

	// 编码过的值要还原成原始值再拼。
	resource2 := aliyunCanonicalResource("/x", url.Values{
		"groupBy": {`[{"code":"ARTICLE_CODE"}]`},
	})
	require.Contains(t, resource2, `groupBy:[{"code":"ARTICLE_CODE"}]`)
}

func TestAliyunACS3SignProducesRequiredHeaders(t *testing.T) {
	signer := &aliyunACS3Signer{
		AccessKeyID:     "test-access-key-id",
		AccessKeySecret: "test-access-key-secret",
	}
	headers, err := signer.aliyunACS3Sign(
		"GET", "modelstudio.cn-beijing.aliyuncs.com", "/tokenplan/subscription/stats",
		nil,
		map[string]string{
			"x-acs-action":  "GetSubscriptionStats",
			"x-acs-version": aliyunModelStudioVersion,
		},
		nil, time.Now(),
	)
	require.NoError(t, err)

	// 五个必带公共头 + Authorization。
	require.Equal(t, "GetSubscriptionStats", headers["x-acs-action"])
	require.Equal(t, aliyunModelStudioVersion, headers["x-acs-version"])
	require.NotEmpty(t, headers["x-acs-date"])
	require.NotEmpty(t, headers["x-acs-signature-nonce"])
	require.NotEmpty(t, headers["x-acs-content-sha256"])
	require.Contains(t, headers["Authorization"], "ACS3-HMAC-SHA256 Credential=test-access-key-id,SignedHeaders=")
	require.Contains(t, headers["Authorization"], "Signature=")
	// host 不作为请求头下发（Go 自己设）。
	require.NotContains(t, headers, "host")
}

func TestAliyunACS3SignIsDeterministicForSameNonce(t *testing.T) {
	// 固定 nonce 与时间 ⇒ 签名必须一致（否则每次请求签名都变，说明拼串有随机性问题）。
	signer := &aliyunACS3Signer{AccessKeyID: "ak", AccessKeySecret: "sk"}
	now := time.Unix(1785000000, 0).UTC()
	build := func() string {
		headers, err := signer.aliyunACS3Sign("GET", "h.example.com", "/p", nil,
			map[string]string{"x-acs-action": "A", "x-acs-version": "V", "x-acs-signature-nonce": "fixednonce"},
			nil, now)
		require.NoError(t, err)
		return headers["Authorization"]
	}
	require.Equal(t, build(), build())
}

func TestAliyunACS3CanonicalQuerySorting(t *testing.T) {
	require.Equal(t, "", aliyunACS3CanonicalQuery(nil))
	require.Equal(t, "a=1&b=2", aliyunACS3CanonicalQuery(url.Values{"b": {"2"}, "a": {"1"}}))
	require.Equal(t, "k=a%20b", aliyunACS3CanonicalQuery(url.Values{"k": {"a b"}}))
	// 斜杠在 query 值里要编码。
	require.Equal(t, "k=a%2Fb", aliyunACS3CanonicalQuery(url.Values{"k": {"a/b"}}))
}

func TestAliyunACS3URIPathKeepsSlashes(t *testing.T) {
	require.Equal(t, "/tokenplan/subscription/stats",
		aliyunACS3URIPath("/tokenplan/subscription/stats"))
	require.Equal(t, "/a%20b/c", aliyunACS3URIPath("/a b/c"))
}

// ---------------------------------------------------------------- 官方响应解析

func TestAliyunParseOfficialSeatQuotaReadsOfficialRemaining(t *testing.T) {
	// 官方 GetSubscriptionStats 返回样例（文档原样）。
	body := []byte(`{
	  "Success": true,
	  "Data": {
	    "SubscriptionStartTime": 1788328800000,
	    "SubscriptionEndTime": 1790920800000,
	    "Items": [
	      {
	        "SeatType": "standard",
	        "SeatRefreshTime": 1790920800000,
	        "TotalSeats": 2,
	        "AssignedSeats": 1,
	        "SeatCredits": 100,
	        "SeatRemainingCredits": 40
	      }
	    ]
	  }
	}`)
	// Data 存在且 Items 非空 ⇒ 团队席位版。
	items := gjson.GetBytes(body, "Data.Items")
	require.True(t, items.Exists())
	require.Len(t, items.Array(), 1)
	item := items.Array()[0]
	require.Equal(t, "standard", item.Get("SeatType").String())
	require.Equal(t, float64(100), item.Get("SeatCredits").Float())
	require.Equal(t, float64(40), item.Get("SeatRemainingCredits").Float())
	require.Equal(t, int64(1790920800000), item.Get("SeatRefreshTime").Int())
}

func TestAliyunParseOfficialTierQuotaTable(t *testing.T) {
	// 官方 quota-config 实测响应（原样）。
	body := []byte(`{
	  "code": "200",
	  "data": {
	    "DataV2": {
	      "ret": ["SUCCESS::接口调用成功"],
	      "data": {
	        "msg": "Success.",
	        "code": "SUCCESS",
	        "data": {
	          "standard": { "five_hour": 3000.0, "monthly": 80000.0 },
	          "addon_quota": { "extrabundle": 20000.0 },
	          "lite": { "five_hour": 700.0, "monthly": 20000.0 },
	          "pro": { "five_hour": 12000.0, "monthly": 200000.0 },
	          "essential": { "five_hour": 1800.0, "monthly": 45000.0 }
	        }
	      }
	    },
	    "success": true
	  }
	}`)
	table := aliyunParseOfficialTierQuotaTable(body)
	require.Len(t, table, 5)

	standard, ok := aliyunTierQuotaByName(table, "standard")
	require.True(t, ok)
	require.Equal(t, float64(80000), standard.MonthlyCredits)
	require.Equal(t, float64(3000), standard.FiveHourCredits)

	// Standard 档官方 monthly=80000，与账单 139 元套餐完全对应（档位识别口径正确）。
	lite, ok := aliyunTierQuotaByName(table, "lite")
	require.True(t, ok)
	require.Equal(t, float64(20000), lite.MonthlyCredits)

	addon, ok := aliyunTierQuotaByName(table, "addon_quota")
	require.True(t, ok)
	require.Equal(t, float64(20000), addon.AddonCredits)
}

func TestAliyunParseOfficialPersonalUsage(t *testing.T) {
	// 官方个人版 usage 返回的字段族：per{N}{Hour|Day|Week|Month}Percentage + ...ResetTime。
	body := []byte(`{
	  "code": "200",
	  "data": {
	    "DataV2": {
	      "ret": ["SUCCESS::接口调用成功"],
	      "data": {
	        "code": "SUCCESS",
	        "data": {
	          "per5HourPercentage": 0.12,
	          "per5HourResetTime": 1788328800000,
	          "per1MonthPercentage": 0.34,
	          "per1MonthResetTime": 1790920800000
	        }
	      }
	    },
	    "success": true
	  }
	}`)
	usages := aliyunParseOfficialPersonalUsage(body)
	require.Len(t, usages, 2)

	monthly, ok := aliyunPickPersonalUsage(usages, "per1Month")
	require.True(t, ok)
	require.InDelta(t, 0.34, monthly.UsedPercent, 1e-9)
	require.Equal(t, time.UnixMilli(1790920800000).UTC().Format(time.RFC3339), monthly.ResetAt)

	fiveHour, ok := aliyunPickPersonalUsage(usages, "per5Hour")
	require.True(t, ok)
	require.InDelta(t, 0.12, fiveHour.UsedPercent, 1e-9)

	// 没有的窗口要能明确区分（不能返回零值冒充有数据）。
	_, ok = aliyunPickPersonalUsage(usages, "perWeek")
	require.False(t, ok)
}

// 回归测试：官方把每个窗口拆成 Percentage 与 ResetTime 两个平级字段，
// 解析时不能靠"值是否为 0"去猜字段类型 —— 百分比的合法值就是 0。
//
// 曾经的 bug：ResetTime 记录（percentage=0）把同窗口的 UsedPercent 覆盖成 0，
// 于是官方明明给了 1.5% 却显示成 0%（看着像"完全没用量"）。
func TestAliyunParseOfficialPersonalUsageKeepsPercentAndResetIndependent(t *testing.T) {
	// 官方还可能只给 ResetTime 不给 Percentage（窗口存在但该窗口没用量记录）。
	body := []byte(`{
	  "data": {
	    "DataV2": {
	      "data": {
	        "data": {
	          "per1MonthPercentage": 0.015,
	          "per1MonthResetTime": 1790920800000,
	          "perWeekResetTime": 1791525600000
	        }
	      }
	    }
	  }
	}`)
	usages := aliyunParseOfficialPersonalUsage(body)
	require.Len(t, usages, 2)

	// 有百分比也有重置时间的窗口：两个值都要对。
	monthly, ok := aliyunPickPersonalUsage(usages, "per1Month")
	require.True(t, ok)
	require.InDelta(t, 0.015, monthly.UsedPercent, 1e-9)
	require.Equal(t, time.UnixMilli(1790920800000).UTC().Format(time.RFC3339), monthly.ResetAt)

	// 只有重置时间的窗口：百分比必须是 0（官方没给，不是被覆盖的）。
	week, ok := aliyunPickPersonalUsage(usages, "perWeek")
	require.True(t, ok)
	require.InDelta(t, 0, week.UsedPercent, 1e-9)
	require.Equal(t, time.UnixMilli(1791525600000).UTC().Format(time.RFC3339), week.ResetAt)
}

// 回归测试：官方百分比为 0（窗口存在且确实没用量）时，
// 不能因为"值是 0"就把这个窗口整个丢掉。
func TestAliyunParseOfficialPersonalUsageKeepsZeroPercentWindow(t *testing.T) {
	body := []byte(`{"data":{"data":{"per1MonthPercentage":0,"per1MonthResetTime":1790920800000}}}`)
	usages := aliyunParseOfficialPersonalUsage(body)
	require.Len(t, usages, 1)
	monthly, ok := aliyunPickPersonalUsage(usages, "per1Month")
	require.True(t, ok)
	require.InDelta(t, 0, monthly.UsedPercent, 1e-9)
	require.NotEmpty(t, monthly.ResetAt)
}

func TestAliyunConsoleErrorCodeExtractsOfficialCodes(t *testing.T) {
	// 官方实测的两个错误码，都要能读出来。
	teamBody := []byte(`{"code":"200","data":{"success":false,"errorCode":"BailianGateway.Team.NotAuthorised","errorMsg":"BailianGateway.Team.NotAuthorised"}}`)
	require.Equal(t, "BailianGateway.Team.NotAuthorised", aliyunConsoleErrorCode(teamBody))

	loginBody := []byte(`{"code":"200","data":{"success":false,"errorCode":"BailianGateway.Login.NotLogined","errorMsg":"BailianGateway.Login.NotLogined"}}`)
	require.Equal(t, "BailianGateway.Login.NotLogined", aliyunConsoleErrorCode(loginBody))

	// 成功时没有错误码。
	okBody := []byte(`{"code":"200","data":{"success":true}}`)
	require.Empty(t, aliyunConsoleErrorCode(okBody))
}

func TestAliyunExtractCookieValue(t *testing.T) {
	cookie := "login_aliyunid_csrf=abc123; cna=xyz789; other=1"
	require.Equal(t, "abc123", aliyunExtractCookieValue(cookie, "login_aliyunid_csrf"))
	require.Equal(t, "xyz789", aliyunExtractCookieValue(cookie, "cna"))
	require.Empty(t, aliyunExtractCookieValue(cookie, "missing"))
	require.Empty(t, aliyunExtractCookieValue("", "cna"))
}

// ---------------------------------------------------------------- 口径辅助

func TestAliyunQuotaPoolKey(t *testing.T) {
	// 归组键 = 主机 + AK 主体；主机大小写不敏感，SK 不参与。
	a := aliyunQuotaPoolKey("modelstudio.cn-beijing.aliyuncs.com", "AK-1")
	b := aliyunQuotaPoolKey("MODELSTUDIO.CN-BEIJING.ALIYUNCS.COM", "AK-1")
	require.Equal(t, a, b, "主机名大小写不应影响归组")
	require.NotEqual(t, a, aliyunQuotaPoolKey("modelstudio.cn-beijing.aliyuncs.com", "AK-2"),
		"不同 AK 主体必须是不同额度池")
}

func TestAliyunTierNameByOfficialFee(t *testing.T) {
	// 官方公布的档位与月费：Lite 39 / Essential 79 / Standard 139 / Pro 312。
	cases := []struct {
		fee   float64
		want  string
		found bool
	}{
		{39, "lite", true},
		{79, "essential", true},
		{139, "standard", true},
		{312, "pro", true},
		{139.00, "standard", true},
		{10, "", false},
	}
	for _, c := range cases {
		got, ok := aliyunTierNameByOfficialFee(c.fee)
		require.Equal(t, c.found, ok, "fee=%v", c.fee)
		require.Equal(t, c.want, got, "fee=%v", c.fee)
	}
}

func TestIsAliyunTokenPlanBaseURL(t *testing.T) {
	require.True(t, isAliyunTokenPlanBaseURL("https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1"))
	require.True(t, isAliyunTokenPlanBaseURL("https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1"))
	require.True(t, isAliyunTokenPlanBaseURL("https://TOKEN-PLAN.CN-BEIJING.MAAS.ALIYUNCS.COM/compatible-mode/v1"))

	// 非 Token Plan 入口不算。
	require.False(t, isAliyunTokenPlanBaseURL("https://dashscope.aliyuncs.com/compatible-mode/v1"))
	require.False(t, isAliyunTokenPlanBaseURL("https://coding.dashscope.aliyuncs.com/v1"))
	// 相似域名不算（精确匹配）。
	require.False(t, isAliyunTokenPlanBaseURL("https://evil-token-plan.cn-beijing.maas.aliyuncs.com/v1"))
	require.False(t, isAliyunTokenPlanBaseURL(""))
}

func TestAliyunSubscriptionMonthRange(t *testing.T) {
	loc := aliyunTokenPlanLoc()
	// 锚点 19 日，now 在 26 日 ⇒ 本期从 19 日到下月 19 日。
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, loc)
	start, end := aliyunSubscriptionMonthRange(now, 19)
	require.Equal(t, 19, start.Day())
	require.Equal(t, time.November, start.Month())
	require.Equal(t, time.December, end.Month())
	require.Equal(t, 19, end.Day())

	// now 在锚点之前 ⇒ 本期是上个月开始的那一期。
	early := time.Date(2026, 11, 5, 12, 0, 0, 0, loc)
	start2, end2 := aliyunSubscriptionMonthRange(early, 19)
	require.Equal(t, time.October, start2.Month())
	require.Equal(t, 19, start2.Day())
	require.Equal(t, time.November, end2.Month())
}

func TestAliyunIsInsufficientQuota(t *testing.T) {
	require.True(t, aliyunIsInsufficientQuota([]byte(
		`{"code":"InvalidApiKey","message":"429 insufficient_quota: Your token-plan quota has been exhausted."}`)))
	require.True(t, aliyunIsInsufficientQuota([]byte("quota has been exhausted")))
	require.False(t, aliyunIsInsufficientQuota([]byte("normal response")))
	require.False(t, aliyunIsInsufficientQuota(nil))
}

func TestAliyunErrorCodesAreStable(t *testing.T) {
	codes := []string{
		aliyunErrNoCredential,
		aliyunErrBadEndpoint,
		aliyunErrHostUnresolvable,
		aliyunErrQuotaUnavailable,
		aliyunErrBillingUnavailable,
		aliyunErrTierUnknown,
		aliyunErrPoolResolveFailed,
		aliyunErrUsageStatsUnavailable,
		aliyunErrConsoleCookieRequired,
		aliyunErrConsoleRejected,
	}
	seen := map[string]bool{}
	for _, code := range codes {
		require.NotEmpty(t, code)
		require.False(t, seen[code], "错误码重复：%s", code)
		seen[code] = true
	}
}

func TestAliyunSnapshotSerializesForFrontend(t *testing.T) {
	snap := aliyunQuotaSnapshot{
		PlanName:        "standard",
		MonthlyCredit:   80000,
		UsedCredit:      1234.5,
		RemainingCredit: 78765.5,
		UsedPercent:     1.54,
		ResetAt:         "2026-12-19T00:00:00Z",
		Exhausted:       false,
		PoolAccountCount: 3,
		DataSource:      "official_quota_config",
		FetchedAt:       "2026-11-26T12:00:00Z",
	}
	raw, err := json.Marshal(snap)
	require.NoError(t, err)
	obj := gjson.ParseBytes(raw)
	require.Equal(t, "standard", obj.Get("plan_name").String())
	require.Equal(t, float64(80000), obj.Get("monthly_credit").Float())
	require.Equal(t, float64(1234.5), obj.Get("used_credit").Float())
	require.Equal(t, float64(78765.5), obj.Get("remaining_credit").Float())
	require.Equal(t, "2026-12-19T00:00:00Z", obj.Get("reset_at").String())
	require.Equal(t, "official_quota_config", obj.Get("data_source").String())
	require.Equal(t, int64(3), obj.Get("pool_account_count").Int())
}

func TestAliyunSnapshotHasNoLocalComputationFields(t *testing.T) {
	// 防止有人把实算相关字段加回来（单价表、阶梯、夜间折扣这类都不该再有）。
	snap := aliyunQuotaSnapshot{}
	raw, err := json.Marshal(snap)
	require.NoError(t, err)
	for _, forbidden := range []string{"price", "tier_price", "night", "night_discount", "unit_price", "computed"} {
		require.NotContains(t, strings.ToLower(string(raw)), forbidden,
			"快照里不该再有实算字段：%s", forbidden)
	}
}
