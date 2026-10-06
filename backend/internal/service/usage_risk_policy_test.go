package service

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// usageRiskSettingRepoStub 是仅实现 LoadUsageRiskPolicy / UpdateUsageRiskSettings
// 所需方法的进程内桩，供本文件单测隔离 DB。
type usageRiskSettingRepoStub struct {
	values          map[string]string
	setMultipleCalls int
}

func (r *usageRiskSettingRepoStub) Get(ctx context.Context, key string) (*Setting, error) {
	v, ok := r.values[key]
	if !ok {
		return nil, errors.New("not found")
	}
	return &Setting{Key: key, Value: v}, nil
}

func (r *usageRiskSettingRepoStub) GetValue(ctx context.Context, key string) (string, error) {
	v, ok := r.values[key]
	if !ok {
		return "", errors.New("not found")
	}
	return v, nil
}

func (r *usageRiskSettingRepoStub) Set(ctx context.Context, key, value string) error {
	if r.values == nil {
		r.values = map[string]string{}
	}
	r.values[key] = value
	return nil
}

func (r *usageRiskSettingRepoStub) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		if v, ok := r.values[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

func (r *usageRiskSettingRepoStub) SetMultiple(ctx context.Context, settings map[string]string) error {
	r.setMultipleCalls++
	if r.values == nil {
		r.values = map[string]string{}
	}
	for k, v := range settings {
		r.values[k] = v
	}
	return nil
}

func (r *usageRiskSettingRepoStub) GetAll(ctx context.Context) (map[string]string, error) {
	out := make(map[string]string, len(r.values))
	for k, v := range r.values {
		out[k] = v
	}
	return out, nil
}

func (r *usageRiskSettingRepoStub) Delete(ctx context.Context, key string) error {
	delete(r.values, key)
	return nil
}

func newUsageRiskTestService() *SettingService {
	return &SettingService{
		settingRepo: &usageRiskSettingRepoStub{values: map[string]string{}},
	}
}

func TestUsageRiskPolicyDefaultsMatchPlan(t *testing.T) {
	s := newUsageRiskTestService()
	p, err := s.LoadUsageRiskPolicy(context.Background())
	if err != nil {
		t.Fatalf("LoadUsageRiskPolicy zero-config returned error: %v", err)
	}

	def := defaultUsageRiskSettings()
	if p.Enabled != true {
		t.Errorf("Enabled = %v, want true", p.Enabled)
	}
	if p.UnlimitedGroupsOnly != true {
		t.Errorf("UnlimitedGroupsOnly = %v, want true", p.UnlimitedGroupsOnly)
	}
	if p.MinDailyRequests != parseIntDefault(def[SettingKeyUsageRiskMinDailyRequests], 0) {
		t.Errorf("MinDailyRequests = %d, want %s", p.MinDailyRequests, def[SettingKeyUsageRiskMinDailyRequests])
	}
	if p.R1ActiveHours != 20 {
		t.Errorf("R1ActiveHours = %d, want 20", p.R1ActiveHours)
	}
	if p.R1ConsecutiveDays != 3 {
		t.Errorf("R1ConsecutiveDays = %d, want 3", p.R1ConsecutiveDays)
	}
	if p.R2Multiple != 1.5 {
		t.Errorf("R2Multiple = %v, want 1.5", p.R2Multiple)
	}
	if p.R2PeerCount != 10 {
		t.Errorf("R2PeerCount = %d, want 10", p.R2PeerCount)
	}
	if p.R3ARatio != 0.9 {
		t.Errorf("R3ARatio = %v, want 0.9", p.R3ARatio)
	}
	if p.R3AMinutes != 30 {
		t.Errorf("R3AMinutes = %d, want 30", p.R3AMinutes)
	}
	if p.R3BOccupancy != 0.5 {
		t.Errorf("R3BOccupancy = %v, want 0.5", p.R3BOccupancy)
	}
	if p.R4DistinctIP != 10 {
		t.Errorf("R4DistinctIP = %d, want 10", p.R4DistinctIP)
	}
	if p.R5UserCount != 3 {
		t.Errorf("R5UserCount = %d, want 3", p.R5UserCount)
	}
	if p.R5IPRequests != 100 {
		t.Errorf("R5IPRequests = %d, want 100", p.R5IPRequests)
	}
	if p.R6UARatio != 0.8 {
		t.Errorf("R6UARatio = %v, want 0.8", p.R6UARatio)
	}
	if p.R7CacheRatio != 0.05 {
		t.Errorf("R7CacheRatio = %v, want 0.05", p.R7CacheRatio)
	}
	if p.R7MinInputTokens != 5000000 {
		t.Errorf("R7MinInputTokens = %d, want 5000000", p.R7MinInputTokens)
	}
	if p.R8KeyCount != 3 {
		t.Errorf("R8KeyCount = %d, want 3", p.R8KeyCount)
	}
	if p.R8KeyRatio != 0.2 {
		t.Errorf("R8KeyRatio = %v, want 0.2", p.R8KeyRatio)
	}
	if len(p.UAWhitelist) != 0 {
		t.Errorf("UAWhitelist = %v, want empty", p.UAWhitelist)
	}
	if p.ListingMinScore != 40 {
		t.Errorf("ListingMinScore = %d, want 40", p.ListingMinScore)
	}
	if p.RetentionDays != 90 {
		t.Errorf("RetentionDays = %d, want 90", p.RetentionDays)
	}
	// 全部 *_enabled 默认开。
	for i, want := range []bool{
		p.R1Enabled, p.R2Enabled, p.R3AEnabled, p.R3BEnabled,
		p.R4Enabled, p.R5Enabled, p.R6Enabled, p.R7Enabled, p.R8Enabled,
	} {
		if !want {
			t.Errorf("rule R%d enabled = false, want true", i+1)
		}
	}
}

func TestUsageRiskPolicyVersionSensitive(t *testing.T) {
	base := defaultPolicyForTest()
	v0 := UsageRiskPolicyVersion(base, "UTC")

	// 任一阈值/开关/白名单/入榜线变化即变化。
	cases := []struct {
		name string
		mut  func(p *UsageRiskPolicy)
	}{
		{"global enabled off", func(p *UsageRiskPolicy) { p.Enabled = false }},
		{"unlimited_groups_only off", func(p *UsageRiskPolicy) { p.UnlimitedGroupsOnly = false }},
		{"min_daily_requests", func(p *UsageRiskPolicy) { p.MinDailyRequests = 99 }},
		{"r1 active hours", func(p *UsageRiskPolicy) { p.R1ActiveHours = 21 }},
		{"r1 consecutive days", func(p *UsageRiskPolicy) { p.R1ConsecutiveDays = 4 }},
		{"r1 enabled off", func(p *UsageRiskPolicy) { p.R1Enabled = false }},
		{"r2 multiple", func(p *UsageRiskPolicy) { p.R2Multiple = 2.0 }},
		{"r2 peer count", func(p *UsageRiskPolicy) { p.R2PeerCount = 11 }},
		{"r3a ratio", func(p *UsageRiskPolicy) { p.R3ARatio = 0.91 }},
		{"r3a minutes", func(p *UsageRiskPolicy) { p.R3AMinutes = 31 }},
		{"r3b occupancy", func(p *UsageRiskPolicy) { p.R3BOccupancy = 0.51 }},
		{"r4 ip", func(p *UsageRiskPolicy) { p.R4DistinctIP = 11 }},
		{"r5 user count", func(p *UsageRiskPolicy) { p.R5UserCount = 4 }},
		{"r5 ip requests", func(p *UsageRiskPolicy) { p.R5IPRequests = 101 }},
		{"r6 ratio", func(p *UsageRiskPolicy) { p.R6UARatio = 0.81 }},
		{"r7 cache ratio", func(p *UsageRiskPolicy) { p.R7CacheRatio = 0.06 }},
		{"r7 min input", func(p *UsageRiskPolicy) { p.R7MinInputTokens = 6000000 }},
		{"r8 key count", func(p *UsageRiskPolicy) { p.R8KeyCount = 4 }},
		{"r8 key ratio", func(p *UsageRiskPolicy) { p.R8KeyRatio = 0.21 }},
		{"ua whitelist", func(p *UsageRiskPolicy) { p.UAWhitelist = []string{"curl"} }},
		{"listing min score", func(p *UsageRiskPolicy) { p.ListingMinScore = 41 }},
		{"retention days", func(p *UsageRiskPolicy) { p.RetentionDays = 91 }},
	}
	for _, c := range cases {
		p := base
		c.mut(&p)
		got := UsageRiskPolicyVersion(p, "UTC")
		if got == v0 {
			t.Errorf("version did not change for %s (both = %s)", c.name, got)
		}
	}

	// 时区名变化即变化。
	if v := UsageRiskPolicyVersion(base, "Asia/Shanghai"); v == v0 {
		t.Errorf("version did not change when tz changed (both = %s)", v)
	}

	// 无关变化（相同内容）保持稳定、可复现。
	if v := UsageRiskPolicyVersion(base, "UTC"); v != v0 {
		t.Errorf("version not stable/deterministic: got %s want %s", v, v0)
	}
	// 不同 tz 但同策略，两次调用一致。
	if v := UsageRiskPolicyVersion(base, "Asia/Shanghai"); v != UsageRiskPolicyVersion(base, "Asia/Shanghai") {
		t.Errorf("version not deterministic across tz")
	}
}

func defaultPolicyForTest() UsageRiskPolicy {
	s := newUsageRiskTestService()
	p, err := s.LoadUsageRiskPolicy(context.Background())
	if err != nil {
		panic(err)
	}
	return p
}

// TestValidateUsageRiskSettingsBoundaries 表驱动：合法默认值 + 逐键边界拒绝。
func TestValidateUsageRiskSettingsBoundaries(t *testing.T) {
	// 合法默认值应通过。
	if err := ValidateUsageRiskSettings(defaultUsageRiskSettings()); err != nil {
		t.Fatalf("defaults should validate, got: %v", err)
	}

	// 逐键负/超范围/0 边界拒绝。
	bad := []struct {
		key   string
		value string
	}{
		// 比例类 ∈ (0,1]
		{SettingKeyUsageRiskR3ARatio, "0"},
		{SettingKeyUsageRiskR3ARatio, "1.1"},
		{SettingKeyUsageRiskR3ARatio, "-0.1"},
		{SettingKeyUsageRiskR3BOccupancy, "0"},
		{SettingKeyUsageRiskR3BOccupancy, "2"},
		{SettingKeyUsageRiskR6UARatio, "0"},
		{SettingKeyUsageRiskR6UARatio, "1.0001"},
		{SettingKeyUsageRiskR7CacheRatio, "0"},
		{SettingKeyUsageRiskR7CacheRatio, "1.5"},
		{SettingKeyUsageRiskR8KeyRatio, "0"},
		{SettingKeyUsageRiskR8KeyRatio, "1.2"},
		// 倍数类 >1
		{SettingKeyUsageRiskR2Multiple, "1"},
		{SettingKeyUsageRiskR2Multiple, "0.9"},
		{SettingKeyUsageRiskR2Multiple, "0"},
		// 计数类 ≥1
		{SettingKeyUsageRiskMinDailyRequests, "0"},
		{SettingKeyUsageRiskMinDailyRequests, "-1"},
		{SettingKeyUsageRiskR1ActiveHours, "0"},
		{SettingKeyUsageRiskR1ConsecutiveDays, "0"},
		{SettingKeyUsageRiskR2PeerCount, "0"},
		{SettingKeyUsageRiskR3AMinutes, "0"},
		{SettingKeyUsageRiskR4DistinctIP, "0"},
		{SettingKeyUsageRiskR5UserCount, "0"},
		{SettingKeyUsageRiskR5IPRequests, "0"},
		{SettingKeyUsageRiskR7MinInputTokens, "0"},
		{SettingKeyUsageRiskR8KeyCount, "0"},
		{SettingKeyUsageRiskRetentionDays, "0"},
		// 入榜线 ≥0（0 允许，负拒绝）
		{SettingKeyUsageRiskListingMinScore, "-1"},
		// 布尔类非法
		{SettingKeyUsageRiskEnabled, "yes"},
		{SettingKeyUsageRiskR1Enabled, "maybe"},
		// 白名单非法 JSON
		{SettingKeyUsageRiskUAWhitelist, "not-json"},
		{SettingKeyUsageRiskUAWhitelist, "{bad}"},
	}
	for _, b := range bad {
		values := map[string]string{b.key: b.value}
		if err := ValidateUsageRiskSettings(values); err == nil {
			t.Errorf("expected rejection for %s=%q, got nil", b.key, b.value)
		}
	}

	// 边界允许值应通过。
	ok := []struct {
		key   string
		value string
	}{
		{SettingKeyUsageRiskR3ARatio, "1"},     // 比例上限允许
		{SettingKeyUsageRiskR2Multiple, "1.0001"}, // 倍数刚过 1
		{SettingKeyUsageRiskMinDailyRequests, "1"},
		{SettingKeyUsageRiskListingMinScore, "0"}, // 入榜线允许 0
		{SettingKeyUsageRiskRetentionDays, "1"},
		{SettingKeyUsageRiskEnabled, "false"},
		{SettingKeyUsageRiskUAWhitelist, "[\"curl\",\"python-requests\"]"},
		{SettingKeyUsageRiskUAWhitelist, "[]"},
	}
	for _, o := range ok {
		values := map[string]string{o.key: o.value}
		if err := ValidateUsageRiskSettings(values); err != nil {
			t.Errorf("expected accept for %s=%q, got: %v", o.key, o.value, err)
		}
	}
}

// TestUsageRiskEnabledDecoupledFromThreshold 校验 *_enabled 开关与阈值解耦：
// 关闭某规则不影响其阈值合法性，阈值非法也不受 enabled 影响。
func TestUsageRiskEnabledDecoupledFromThreshold(t *testing.T) {
	// enabled=false + 阈值合法 → 通过。
	if err := ValidateUsageRiskSettings(map[string]string{
		SettingKeyUsageRiskR1Enabled:    "false",
		SettingKeyUsageRiskR1ActiveHours: "20",
	}); err != nil {
		t.Errorf("enabled=false with valid threshold should pass, got: %v", err)
	}
	// enabled=true + 阈值非法 → 拒绝（因阈值非法，与 enabled 无关）。
	if err := ValidateUsageRiskSettings(map[string]string{
		SettingKeyUsageRiskR1Enabled:     "true",
		SettingKeyUsageRiskR1ActiveHours: "0",
	}); err == nil {
		t.Errorf("valid enabled must not mask invalid threshold")
	}
	// enabled 非法 → 拒绝，即使阈值合法。
	if err := ValidateUsageRiskSettings(map[string]string{
		SettingKeyUsageRiskR1Enabled:     "notbool",
		SettingKeyUsageRiskR1ActiveHours: "20",
	}); err == nil {
		t.Errorf("invalid enabled must be rejected regardless of threshold")
	}
}

// TestUsageRiskRetentionBinding 保留期绑定违反拒绝、合法通过。
func TestUsageRiskRetentionBinding(t *testing.T) {
	if err := ValidateRetentionBinding(90, 90); err != nil {
		t.Errorf("riskDays==usageLogsDays should pass, got: %v", err)
	}
	if err := ValidateRetentionBinding(50, 90); err != nil {
		t.Errorf("riskDays<usageLogsDays should pass, got: %v", err)
	}
	if err := ValidateRetentionBinding(91, 90); err == nil {
		t.Errorf("riskDays>usageLogsDays must be rejected")
	}
}

// TestUsageRiskValidatorSingleSource 校验器在保存与启动两条路径是同一函数：
// 同一非法输入，LoadUsageRiskPolicy（启动）与 UpdateUsageRiskSettings（保存）都应拒绝。
func TestUsageRiskValidatorSingleSource(t *testing.T) {
	bad := map[string]string{SettingKeyUsageRiskR2Multiple: "0.5"} // 倍数必须 >1

	// 启动加载路径：将非法值写入存储后 LoadUsageRiskPolicy 必须失败。
	sLoad := newUsageRiskTestService()
	sLoad.settingRepo.(*usageRiskSettingRepoStub).values[SettingKeyUsageRiskR2Multiple] = "0.5"
	if _, err := sLoad.LoadUsageRiskPolicy(context.Background()); err == nil {
		t.Errorf("LoadUsageRiskPolicy must reject invalid stored value (shares validator)")
	}

	// 保存路径：UpdateUsageRiskSettings 必须拒绝同一非法输入。
	sSave := newUsageRiskTestService()
	if err := sSave.UpdateUsageRiskSettings(context.Background(), bad); err == nil {
		t.Errorf("UpdateUsageRiskSettings must reject invalid value (shares validator)")
	}

	// 合法输入两条路径都接受。
	sOk := newUsageRiskTestService()
	if err := sOk.UpdateUsageRiskSettings(context.Background(), map[string]string{
		SettingKeyUsageRiskR2Multiple: "2.0",
	}); err != nil {
		t.Errorf("UpdateUsageRiskSettings should accept valid value, got: %v", err)
	}
	// 读取回写值应能成功加载（走同一校验器）。
	if _, err := sOk.LoadUsageRiskPolicy(context.Background()); err != nil {
		t.Errorf("LoadUsageRiskPolicy should accept value written by UpdateUsageRiskSettings, got: %v", err)
	}
}

// TestUsageRiskRetentionBindingOnSaveAndStartup 保留期绑定在保存与启动两路径均生效。
func TestUsageRiskRetentionBindingOnSaveAndStartup(t *testing.T) {
	// 启动：存储中 retention_days 超过源保留期（默认 90）必须失败关闭。
	sLoad := newUsageRiskTestService()
	sLoad.settingRepo.(*usageRiskSettingRepoStub).values[SettingKeyUsageRiskRetentionDays] = "91"
	if _, err := sLoad.LoadUsageRiskPolicy(context.Background()); err == nil {
		t.Errorf("startup must reject retention_days > usage_logs_days")
	}

	// 保存：传入 retention_days=91 必须拒绝。
	sSave := newUsageRiskTestService()
	if err := sSave.UpdateUsageRiskSettings(context.Background(), map[string]string{
		SettingKeyUsageRiskRetentionDays: "91",
	}); err == nil {
		t.Errorf("save must reject retention_days > usage_logs_days")
	}

	// 保存：retention_days=90 通过。
	if err := sSave.UpdateUsageRiskSettings(context.Background(), map[string]string{
		SettingKeyUsageRiskRetentionDays: "90",
	}); err != nil {
		t.Errorf("save should accept retention_days == usage_logs_days, got: %v", err)
	}
}

// TestUsageRiskCRejectsNonFiniteFloats 锁定 C 改动：比例/倍数类浮点键拒绝非有限值
// （NaN / +Inf / -Inf）。这些输入经 strconv.ParseFloat 解析成功，但 NaN 比较恒 false 会
// 穿透范围校验、-Inf 会过 multiple 校验、+Inf 会过 multiple 校验，落库后 JSON 序列化失败。
func TestUsageRiskCRejectsNonFiniteFloats(t *testing.T) {
	// 全部浮点键（kindRatio / kindMultiple），显式列出以保证与 usageRiskKeyMeta 一致。
	floatKeys := []string{
		SettingKeyUsageRiskR2Multiple,
		SettingKeyUsageRiskR3ARatio,
		SettingKeyUsageRiskR3BOccupancy,
		SettingKeyUsageRiskR6UARatio,
		SettingKeyUsageRiskR7CacheRatio,
		SettingKeyUsageRiskR8KeyRatio,
	}

	// 复核这些键确实在元数据中属于浮点类，避免测试与定义漂移。
	for _, k := range floatKeys {
		meta, ok := usageRiskKeyMeta[k]
		if !ok || (meta != kindRatio && meta != kindMultiple) {
			t.Fatalf("test setup drift: %s is not a float key (meta=%v, ok=%v)", k, meta, ok)
		}
	}

	nonFinite := []string{"NaN", "Inf", "+Inf", "-Inf"}
	for _, key := range floatKeys {
		for _, val := range nonFinite {
			if err := ValidateUsageRiskSettings(map[string]string{key: val}); err == nil {
				t.Errorf("expected rejection for %s=%q (non-finite), got nil", key, val)
			}
		}
	}

	// 合法浮点值必须放行（不误伤合法阈值）。kindRatio 范围 (0,1]、kindMultiple 范围 >1，
	// 分别用各自范围内的合法值验证。
	ratioKey := SettingKeyUsageRiskR3ARatio
	multipleKey := SettingKeyUsageRiskR2Multiple
	for _, key := range floatKeys {
		meta := usageRiskKeyMeta[key]
		valid := "1.5"
		if meta == kindRatio {
			valid = "0.5"
		}
		if err := ValidateUsageRiskSettings(map[string]string{key: valid}); err != nil {
			t.Errorf("expected accept for %s=%s (finite, in-range), got: %v", key, valid, err)
		}
	}
	if usageRiskKeyMeta[multipleKey] != kindMultiple || usageRiskKeyMeta[ratioKey] != kindRatio {
		t.Fatalf("test setup drift: R2Multiple=%v R3ARatio=%v", usageRiskKeyMeta[multipleKey], usageRiskKeyMeta[ratioKey])
	}
}

// TestUsageRiskDRejectsUnknownSettingKey 锁定 D 改动：UpdateUsageRiskSettings 仅接受
// usageRiskKeyMeta 中定义的 usage_risk_* 键，未知（外域）键必须被拒且不落库；纯白名单
// 部分更新语义保持不变（SetMultiple 仍被调用 1 次）。
func TestUsageRiskDRejectsUnknownSettingKey(t *testing.T) {
	// 外域键（如支付域）经 usage-risk 接口越权写入。
	values := map[string]string{
		SettingKeyUsageRiskEnabled: "true",
		"payment_xxx":              "1",
	}
	s := newUsageRiskTestService()
	err := s.UpdateUsageRiskSettings(context.Background(), values)
	if err == nil {
		t.Fatalf("expected rejection of unknown key payment_xxx, got nil")
	}
	if !strings.Contains(err.Error(), "INVALID_USAGE_RISK_SETTINGS") {
		t.Errorf("error should carry code INVALID_USAGE_RISK_SETTINGS, got: %v", err)
	}
	// 关键断言：被拒同时 SetMultiple 未被调用——证明越域键没有落库。
	if got := s.settingRepo.(*usageRiskSettingRepoStub).setMultipleCalls; got != 0 {
		t.Errorf("SetMultiple must not be called when unknown key rejected, got %d calls", got)
	}

	// 对照：纯白名单键（含 retention 之外的普通键）应正常落库，SetMultiple 调用 1 次。
	okValues := map[string]string{
		SettingKeyUsageRiskEnabled:          "true",
		SettingKeyUsageRiskListingMinScore:  "40",
	}
	sOk := newUsageRiskTestService()
	if err := sOk.UpdateUsageRiskSettings(context.Background(), okValues); err != nil {
		t.Fatalf("expected accept for whitelist-only values, got: %v", err)
	}
	if got := sOk.settingRepo.(*usageRiskSettingRepoStub).setMultipleCalls; got != 1 {
		t.Errorf("SetMultiple should be called exactly once for whitelist-only update, got %d", got)
	}
}
