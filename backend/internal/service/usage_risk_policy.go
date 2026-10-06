package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// UsageRiskPolicy 是异常调用分析的完整策略快照（纯配置，无 DB 依赖）。
// 全部阈值/开关/白名单/入榜线变化都会驱动新策略版本（见 UsageRiskPolicyVersion），
// run 与报告持久化此快照，启动时固化，运行期只读。
type UsageRiskPolicy struct {
	// 全局开关（方案 §7，默认开）。
	Enabled bool
	// TZName 是应用时区名（方案 §5：时区名持久化于策略快照）。loadPolicy/policyFn 填充；
	// 版本哈希与 run 快照均含该字段，跨重启时区变更检测依赖上一轮快照中的本字段。
	TZName string `json:"tz"`
	// 仅分析不限量订阅分组（方案 §4，默认 true）。
	UnlimitedGroupsOnly bool
	// 进入分析的资格门槛（分组/用户级），计数 ≥1。
	MinDailyRequests int

	// R1 全天候活跃：活跃小时 ≥ 阈值 且连续活跃天数 ≥ 阈值。
	R1ActiveHours       int
	R1ConsecutiveDays   int
	R1Enabled           bool

	// R2 用量离群：倍数 > 阈值 且 peer 群计数 ≥ 阈值。
	R2Multiple  float64
	R2PeerCount int
	R2Enabled   bool

	// R3a RPM 贴线：贴线比 ≥ 阈值 且命中分钟 ≥ 阈值。
	R3ARatio   float64
	R3AMinutes int
	R3AEnabled bool

	// R3b 占用率贴线：占用率 ≥ 阈值。
	R3BOccupancy float64
	R3BEnabled   bool

	// R4 IP 离散：distinct IP ≥ 阈值。
	R4DistinctIP int
	R4Enabled    bool

	// R5 同 IP 聚簇：关联用户数 ≥ 阈值 且该 IP 请求量 ≥ 阈值。
	R5UserCount  int
	R5IPRequests int
	R5Enabled    bool

	// R6 客户端指纹：非白名单 UA 占比 ≥ 阈值。
	R6UARatio float64
	R6Enabled bool

	// R7 缓存失效：cache 比 ≤ 阈值 且当日输入 token ≥ 阈值。
	R7CacheRatio      float64
	R7MinInputTokens  int64
	R7Enabled         bool

	// R8 多 key 均摊：key 数 ≥ 阈值 且每 key 承载 ≥ 阈值。
	R8KeyCount int
	R8KeyRatio float64
	R8Enabled  bool

	// UAWhitelist 已知编程客户端 UA 子串白名单（默认空）。
	UAWhitelist []string
	// ListingMinScore 入榜阈值（≥0，默认 40）。
	ListingMinScore int
	// RetentionDays 风险数据保留期（天数 ≥1，默认 90，须 ≤ usage_logs_days）。
	RetentionDays int
}

// usageRiskKeyKind 描述单个设置键的校验类别。
type usageRiskKeyKind int

const (
	// kindRatio：比例类，合法范围 (0,1]。
	kindRatio usageRiskKeyKind = iota
	// kindMultiple：倍数类，必须 >1。
	kindMultiple
	// kindCountPos：计数类，必须 ≥1。
	kindCountPos
	// kindCountNonNeg：计数类，允许 0（如入榜线）。
	kindCountNonNeg
	// kindBool：布尔开关。
	kindBool
	// kindWhitelist：JSON 数组字符串。
	kindWhitelist
)

// usageRiskKeyMeta 单一域校验器逐键元数据（键 → 类型/范围）。
// 这是唯一一份键定义/范围来源；settings 保存与启动加载共用 ValidateUsageRiskSettings。
var usageRiskKeyMeta = map[string]usageRiskKeyKind{
	SettingKeyUsageRiskEnabled:             kindBool,
	SettingKeyUsageRiskUnlimitedGroupsOnly:  kindBool,
	SettingKeyUsageRiskMinDailyRequests:     kindCountPos,

	SettingKeyUsageRiskR1ActiveHours:       kindCountPos,
	SettingKeyUsageRiskR1ConsecutiveDays:   kindCountPos,
	SettingKeyUsageRiskR1Enabled:           kindBool,

	SettingKeyUsageRiskR2Multiple:          kindMultiple,
	SettingKeyUsageRiskR2PeerCount:         kindCountPos,
	SettingKeyUsageRiskR2Enabled:           kindBool,

	SettingKeyUsageRiskR3ARatio:            kindRatio,
	SettingKeyUsageRiskR3AMinutes:          kindCountPos,
	SettingKeyUsageRiskR3AEnabled:          kindBool,

	SettingKeyUsageRiskR3BOccupancy:        kindRatio,
	SettingKeyUsageRiskR3BEnabled:          kindBool,

	SettingKeyUsageRiskR4DistinctIP:        kindCountPos,
	SettingKeyUsageRiskR4Enabled:           kindBool,

	SettingKeyUsageRiskR5UserCount:         kindCountPos,
	SettingKeyUsageRiskR5IPRequests:       kindCountPos,
	SettingKeyUsageRiskR5Enabled:           kindBool,

	SettingKeyUsageRiskR6UARatio:           kindRatio,
	SettingKeyUsageRiskR6Enabled:           kindBool,

	SettingKeyUsageRiskR7CacheRatio:        kindRatio,
	SettingKeyUsageRiskR7MinInputTokens:   kindCountPos,
	SettingKeyUsageRiskR7Enabled:          kindBool,

	SettingKeyUsageRiskR8KeyCount:          kindCountPos,
	SettingKeyUsageRiskR8KeyRatio:          kindRatio,
	SettingKeyUsageRiskR8Enabled:           kindBool,

	SettingKeyUsageRiskUAWhitelist:         kindWhitelist,
	SettingKeyUsageRiskListingMinScore:     kindCountNonNeg,
	SettingKeyUsageRiskRetentionDays:       kindCountPos,
}

// defaultUsageRiskSettings 返回与方案 §4 一致的默认值。
// min_daily_requests 默认 10：方案 §4 既定，2026-09-25 用户追认（方案修订历史⑧）。
func defaultUsageRiskSettings() map[string]string {
	return map[string]string{
		SettingKeyUsageRiskEnabled:              "true",
		SettingKeyUsageRiskUnlimitedGroupsOnly:  "true",
		SettingKeyUsageRiskMinDailyRequests:     "10",

		SettingKeyUsageRiskR1ActiveHours:       "20",
		SettingKeyUsageRiskR1ConsecutiveDays:   "3",
		SettingKeyUsageRiskR1Enabled:           "true",

		SettingKeyUsageRiskR2Multiple:          "1.5",
		SettingKeyUsageRiskR2PeerCount:         "10",
		SettingKeyUsageRiskR2Enabled:           "true",

		SettingKeyUsageRiskR3ARatio:            "0.9",
		SettingKeyUsageRiskR3AMinutes:          "30",
		SettingKeyUsageRiskR3AEnabled:          "true",

		SettingKeyUsageRiskR3BOccupancy:        "0.5",
		SettingKeyUsageRiskR3BEnabled:          "true",

		SettingKeyUsageRiskR4DistinctIP:        "10",
		SettingKeyUsageRiskR4Enabled:           "true",

		SettingKeyUsageRiskR5UserCount:         "3",
		SettingKeyUsageRiskR5IPRequests:        "100",
		SettingKeyUsageRiskR5Enabled:           "true",

		SettingKeyUsageRiskR6UARatio:           "0.8",
		SettingKeyUsageRiskR6Enabled:          "true",

		SettingKeyUsageRiskR7CacheRatio:       "0.05",
		SettingKeyUsageRiskR7MinInputTokens:   "5000000",
		SettingKeyUsageRiskR7Enabled:          "true",

		SettingKeyUsageRiskR8KeyCount:         "3",
		SettingKeyUsageRiskR8KeyRatio:         "0.2",
		SettingKeyUsageRiskR8Enabled:          "true",

		SettingKeyUsageRiskUAWhitelist:        "[]",
		SettingKeyUsageRiskListingMinScore:    "40",
		SettingKeyUsageRiskRetentionDays:      "90",
	}
}

// allUsageRiskKeys 返回全部 usage_risk_* 键（用于启动加载全量读取）。
func allUsageRiskKeys() []string {
	keys := make([]string, 0, len(usageRiskKeyMeta))
	for k := range usageRiskKeyMeta {
		keys = append(keys, k)
	}
	return keys
}

// ValidateUsageRiskSettings 是异常调用分析设置的唯一域校验器。
// settings 保存路径（UpdateUsageRiskSettings）与启动加载路径（LoadUsageRiskPolicy）
// 共用本函数，禁止第二份实现。
//
// 语义：对 values 中出现的每个 usage_risk_* 键，按 usageRiskKeyMeta 校验类型/范围/
// 是否允许 0；越界或非法一律返回明确错误（失败关闭，不钳制）。未出现的键不报错
// （由默认值兜底）。未知前缀键被忽略。
func ValidateUsageRiskSettings(values map[string]string) error {
	for key, raw := range values {
		meta, ok := usageRiskKeyMeta[key]
		if !ok {
			continue
		}
		if err := validateUsageRiskKey(key, meta, raw); err != nil {
			return err
		}
	}
	return nil
}

func validateUsageRiskKey(key string, meta usageRiskKeyKind, raw string) error {
	switch meta {
	case kindBool:
		if _, err := strconv.ParseBool(raw); err != nil {
			return fmt.Errorf("%s: must be a boolean (true/false), got %q", key, raw)
		}
	case kindCountPos:
		v, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("%s: must be an integer, got %q", key, raw)
		}
		if v < 1 {
			return fmt.Errorf("%s: must be >= 1, got %d", key, v)
		}
	case kindCountNonNeg:
		v, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("%s: must be an integer, got %q", key, raw)
		}
		if v < 0 {
			return fmt.Errorf("%s: must be >= 0, got %d", key, v)
		}
	case kindRatio:
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("%s: must be a number, got %q", key, raw)
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("%s: must be a finite number, got %q", key, raw)
		}
		if v <= 0 || v > 1 {
			return fmt.Errorf("%s: must be in (0,1], got %v", key, v)
		}
	case kindMultiple:
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("%s: must be a number, got %q", key, raw)
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("%s: must be a finite number, got %q", key, raw)
		}
		if v <= 1 {
			return fmt.Errorf("%s: must be > 1, got %v", key, v)
		}
	case kindWhitelist:
		var list []string
		if err := json.Unmarshal([]byte(raw), &list); err != nil {
			return fmt.Errorf("%s: must be a JSON array of strings, got %q", key, raw)
		}
	}
	return nil
}

// ValidateRetentionBinding 校验风险保留期绑定源日志保留权威：
// riskDays 必须 ≤ usageLogsDays，违反即返回明确错误（失败关闭）。
// 供 settings 保存路径与启动加载路径双重调用（方案 §6.4）。
func ValidateRetentionBinding(riskDays, usageLogsDays int) error {
	if riskDays > usageLogsDays {
		return fmt.Errorf("%s (%d) must be <= %s (%d)",
			SettingKeyUsageRiskRetentionDays, riskDays,
			"dashboard_aggregation.retention.usage_logs_days", usageLogsDays)
	}
	return nil
}

// usageLogsDaysOrDefault 从服务配置读取源日志保留期；配置未注入时回退 viper 默认 90。
func usageLogsDaysOrDefault(s *SettingService) int {
	if s != nil && s.cfg != nil {
		return s.cfg.DashboardAgg.Retention.UsageLogsDays
	}
	return 90
}

// LoadUsageRiskPolicy 从 settings 体系读取全键 → 走同一校验器 → 返回策略。
// 零配置时返回与方案 §4 一致的默认值。任何校验失败（含保留期绑定违反）均失败关闭。
func (s *SettingService) LoadUsageRiskPolicy(ctx context.Context) (UsageRiskPolicy, error) {
	stored, err := s.settingRepo.GetMultiple(ctx, allUsageRiskKeys())
	if err != nil {
		return UsageRiskPolicy{}, fmt.Errorf("load usage risk settings: %w", err)
	}

	values := defaultUsageRiskSettings()
	for k, v := range stored {
		values[k] = v
	}

	if err := ValidateUsageRiskSettings(values); err != nil {
		return UsageRiskPolicy{}, fmt.Errorf("usage risk settings invalid: %w", err)
	}

	riskDays := parseIntDefault(values[SettingKeyUsageRiskRetentionDays], 0)
	if err := ValidateRetentionBinding(riskDays, usageLogsDaysOrDefault(s)); err != nil {
		return UsageRiskPolicy{}, fmt.Errorf("usage risk retention binding invalid: %w", err)
	}

	return parseUsageRiskPolicy(values), nil
}

// GetUsageRiskSettings 返回 usage_risk_* 全部键的当前生效字符串值（默认兜底 + 存储覆盖）。
// 供 admin 编辑入口 GET 使用，与 UpdateUsageRiskSettings 形成唯一读写闭环（U2 遗留项 2 收口）。
// 注意：本方法返回原始生效值，不做域校验——校验发生在保存路径与启动加载路径；若存储中
// 存在历史非法值（不应发生），由 LoadUsageRiskPolicy 在读取全量策略时失败关闭暴露。
func (s *SettingService) GetUsageRiskSettings(ctx context.Context) (map[string]string, error) {
	stored, err := s.settingRepo.GetMultiple(ctx, allUsageRiskKeys())
	if err != nil {
		return nil, fmt.Errorf("load usage risk settings: %w", err)
	}
	values := defaultUsageRiskSettings()
	for k, v := range stored {
		values[k] = v
	}
	return values, nil
}

// UpdateUsageRiskSettings 是 usage_risk_* 设置的保存路径：先走同一域校验器，
// 再校验保留期绑定（源保留期来自 viper dashboard_aggregation.retention.usage_logs_days），
// 通过后才落库。仅持久化调用方传入的键（部分更新）。
func (s *SettingService) UpdateUsageRiskSettings(ctx context.Context, values map[string]string) error {
	if err := ValidateUsageRiskSettings(values); err != nil {
		return infraerrors.BadRequest("INVALID_USAGE_RISK_SETTINGS", err.Error())
	}
	// D：设置键白名单——只接受 usageRiskKeyMeta 中定义的 usage_risk_* 键；未知键（如支付域）
	// 经 usage-risk 接口越权写入必须拒绝。持久化只传白名单键构成的 map。
	allowed := make(map[string]string, len(values))
	for k, v := range values {
		if _, ok := usageRiskKeyMeta[k]; !ok {
			return infraerrors.BadRequest("INVALID_USAGE_RISK_SETTINGS",
				fmt.Sprintf("unknown usage risk setting key: %s", k))
		}
		allowed[k] = v
	}
	if raw, ok := allowed[SettingKeyUsageRiskRetentionDays]; ok {
		rd, err := strconv.Atoi(raw)
		if err != nil {
			return infraerrors.BadRequest("INVALID_USAGE_RISK_SETTINGS",
				fmt.Sprintf("%s: must be an integer", SettingKeyUsageRiskRetentionDays))
		}
		if err := ValidateRetentionBinding(rd, usageLogsDaysOrDefault(s)); err != nil {
			return infraerrors.BadRequest("INVALID_USAGE_RISK_RETENTION", err.Error())
		}
	}
	if err := s.settingRepo.SetMultiple(ctx, allowed); err != nil {
		return fmt.Errorf("persist usage risk settings: %w", err)
	}
	return nil
}

// UsageRiskPolicyVersion 返回策略内容哈希（sha256 前 16 hex），含时区名。
// 任何阈值/开关/白名单/入榜线变化即变化；时区名由调用方传入（来自 internal/pkg/timezone.Name()）。
func UsageRiskPolicyVersion(p UsageRiskPolicy, tzName string) string {
	type snap struct {
		Enabled               bool     `json:"enabled"`
		UnlimitedGroupsOnly   bool     `json:"unlimited_groups_only"`
		MinDailyRequests      int      `json:"min_daily_requests"`
		R1ActiveHours         int      `json:"r1_active_hours"`
		R1ConsecutiveDays     int      `json:"r1_consecutive_days"`
		R1Enabled             bool     `json:"r1_enabled"`
		R2Multiple            float64  `json:"r2_multiple"`
		R2PeerCount           int      `json:"r2_peer_count"`
		R2Enabled             bool     `json:"r2_enabled"`
		R3ARatio              float64  `json:"r3a_ratio"`
		R3AMinutes            int      `json:"r3a_minutes"`
		R3AEnabled            bool     `json:"r3a_enabled"`
		R3BOccupancy          float64  `json:"r3b_occupancy"`
		R3BEnabled            bool     `json:"r3b_enabled"`
		R4DistinctIP          int      `json:"r4_distinct_ip"`
		R4Enabled             bool     `json:"r4_enabled"`
		R5UserCount           int      `json:"r5_user_count"`
		R5IPRequests          int      `json:"r5_ip_requests"`
		R5Enabled             bool     `json:"r5_enabled"`
		R6UARatio             float64  `json:"r6_ua_ratio"`
		R6Enabled             bool     `json:"r6_enabled"`
		R7CacheRatio          float64  `json:"r7_cache_ratio"`
		R7MinInputTokens      int64    `json:"r7_min_input_tokens"`
		R7Enabled             bool     `json:"r7_enabled"`
		R8KeyCount            int      `json:"r8_key_count"`
		R8KeyRatio            float64  `json:"r8_key_ratio"`
		R8Enabled             bool     `json:"r8_enabled"`
		UAWhitelist           []string `json:"ua_whitelist"`
		ListingMinScore       int      `json:"listing_min_score"`
		RetentionDays         int      `json:"retention_days"`
		TZ                    string   `json:"tz"`
	}
	s := snap{
		Enabled:               p.Enabled,
		UnlimitedGroupsOnly:   p.UnlimitedGroupsOnly,
		MinDailyRequests:      p.MinDailyRequests,
		R1ActiveHours:         p.R1ActiveHours,
		R1ConsecutiveDays:     p.R1ConsecutiveDays,
		R1Enabled:             p.R1Enabled,
		R2Multiple:            p.R2Multiple,
		R2PeerCount:           p.R2PeerCount,
		R2Enabled:             p.R2Enabled,
		R3ARatio:              p.R3ARatio,
		R3AMinutes:            p.R3AMinutes,
		R3AEnabled:            p.R3AEnabled,
		R3BOccupancy:          p.R3BOccupancy,
		R3BEnabled:            p.R3BEnabled,
		R4DistinctIP:          p.R4DistinctIP,
		R4Enabled:             p.R4Enabled,
		R5UserCount:           p.R5UserCount,
		R5IPRequests:          p.R5IPRequests,
		R5Enabled:             p.R5Enabled,
		R6UARatio:             p.R6UARatio,
		R6Enabled:             p.R6Enabled,
		R7CacheRatio:          p.R7CacheRatio,
		R7MinInputTokens:      p.R7MinInputTokens,
		R7Enabled:             p.R7Enabled,
		R8KeyCount:            p.R8KeyCount,
		R8KeyRatio:            p.R8KeyRatio,
		R8Enabled:             p.R8Enabled,
		UAWhitelist:           p.UAWhitelist,
		ListingMinScore:       p.ListingMinScore,
		RetentionDays:         p.RetentionDays,
		TZ:                    tzName,
	}
	b, err := json.Marshal(s)
	if err != nil {
		// 结构化快照序列化不应失败；失败回退到空哈希，调用方仍会触发重评。
		return "0000000000000000"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// parseUsageRiskPolicy 将已校验+默认兜底的 values 解析为 UsageRiskPolicy。
// 调用前 values 必含全部键且通过 ValidateUsageRiskSettings。
func parseUsageRiskPolicy(values map[string]string) UsageRiskPolicy {
	p := UsageRiskPolicy{}
	p.Enabled = parseBoolDefault(values[SettingKeyUsageRiskEnabled], true)
	p.UnlimitedGroupsOnly = parseBoolDefault(values[SettingKeyUsageRiskUnlimitedGroupsOnly], true)
	p.MinDailyRequests = parseIntDefault(values[SettingKeyUsageRiskMinDailyRequests], 10)
	p.R1ActiveHours = parseIntDefault(values[SettingKeyUsageRiskR1ActiveHours], 20)
	p.R1ConsecutiveDays = parseIntDefault(values[SettingKeyUsageRiskR1ConsecutiveDays], 3)
	p.R1Enabled = parseBoolDefault(values[SettingKeyUsageRiskR1Enabled], true)
	p.R2Multiple = parseFloatDefault(values[SettingKeyUsageRiskR2Multiple], 1.5)
	p.R2PeerCount = parseIntDefault(values[SettingKeyUsageRiskR2PeerCount], 10)
	p.R2Enabled = parseBoolDefault(values[SettingKeyUsageRiskR2Enabled], true)
	p.R3ARatio = parseFloatDefault(values[SettingKeyUsageRiskR3ARatio], 0.9)
	p.R3AMinutes = parseIntDefault(values[SettingKeyUsageRiskR3AMinutes], 30)
	p.R3AEnabled = parseBoolDefault(values[SettingKeyUsageRiskR3AEnabled], true)
	p.R3BOccupancy = parseFloatDefault(values[SettingKeyUsageRiskR3BOccupancy], 0.5)
	p.R3BEnabled = parseBoolDefault(values[SettingKeyUsageRiskR3BEnabled], true)
	p.R4DistinctIP = parseIntDefault(values[SettingKeyUsageRiskR4DistinctIP], 10)
	p.R4Enabled = parseBoolDefault(values[SettingKeyUsageRiskR4Enabled], true)
	p.R5UserCount = parseIntDefault(values[SettingKeyUsageRiskR5UserCount], 3)
	p.R5IPRequests = parseIntDefault(values[SettingKeyUsageRiskR5IPRequests], 100)
	p.R5Enabled = parseBoolDefault(values[SettingKeyUsageRiskR5Enabled], true)
	p.R6UARatio = parseFloatDefault(values[SettingKeyUsageRiskR6UARatio], 0.8)
	p.R6Enabled = parseBoolDefault(values[SettingKeyUsageRiskR6Enabled], true)
	p.R7CacheRatio = parseFloatDefault(values[SettingKeyUsageRiskR7CacheRatio], 0.05)
	p.R7MinInputTokens = parseInt64Default(values[SettingKeyUsageRiskR7MinInputTokens], 5000000)
	p.R7Enabled = parseBoolDefault(values[SettingKeyUsageRiskR7Enabled], true)
	p.R8KeyCount = parseIntDefault(values[SettingKeyUsageRiskR8KeyCount], 3)
	p.R8KeyRatio = parseFloatDefault(values[SettingKeyUsageRiskR8KeyRatio], 0.2)
	p.R8Enabled = parseBoolDefault(values[SettingKeyUsageRiskR8Enabled], true)
	if raw := values[SettingKeyUsageRiskUAWhitelist]; raw != "" {
		var list []string
		if err := json.Unmarshal([]byte(raw), &list); err == nil {
			p.UAWhitelist = list
		}
	}
	if p.UAWhitelist == nil {
		p.UAWhitelist = []string{}
	}
	p.ListingMinScore = parseIntDefault(values[SettingKeyUsageRiskListingMinScore], 40)
	p.RetentionDays = parseIntDefault(values[SettingKeyUsageRiskRetentionDays], 90)
	return p
}

func parseBoolDefault(raw string, def bool) bool {
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return def
	}
	return v
}

func parseIntDefault(raw string, def int) int {
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return v
}

func parseInt64Default(raw string, def int64) int64 {
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return def
	}
	return v
}

func parseFloatDefault(raw string, def float64) float64 {
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return def
	}
	return v
}
