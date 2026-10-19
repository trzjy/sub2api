package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	ChannelMonitorV2OtherModel      = "__other__"
	ChannelMonitorV2TaxonomyVersion = 1
)

var (
	ErrChannelMonitorV2InvalidRange   = errors.New("invalid channel monitor v2 range")
	ErrChannelMonitorV2InvalidGroupBy = errors.New("invalid channel monitor v2 group_by")
	ErrChannelMonitorV2InvalidConfig  = errors.New("invalid channel monitor v2 config")
	ErrChannelMonitorV2ConfigConflict = errors.New("channel monitor v2 config was modified")
)

type ChannelMonitorV2GroupBy string

const (
	ChannelMonitorV2GroupByPlatform           ChannelMonitorV2GroupBy = "platform"
	ChannelMonitorV2GroupByPlatformGroup      ChannelMonitorV2GroupBy = "platform_group"
	ChannelMonitorV2GroupByPlatformModel      ChannelMonitorV2GroupBy = "platform_model"
	ChannelMonitorV2GroupByPlatformGroupModel ChannelMonitorV2GroupBy = "platform_group_model"
	// ChannelMonitorV2GroupByModel collapses the same display model across all
	// platforms and groups into a single row (C2/C6: cross-platform merge, no
	// channel leakage). It is the new default presentation grain.
	ChannelMonitorV2GroupByModel ChannelMonitorV2GroupBy = "model"
)

type ChannelMonitorV2PlatformConfig struct {
	Platform string   `json:"platform"`
	Enabled  bool     `json:"enabled"`
	Models   []string `json:"models"`
}

type ChannelMonitorV2Config struct {
	Version                int                              `json:"version"`
	Enabled                bool                             `json:"enabled"`
	RefreshIntervalSeconds int                              `json:"refresh_interval_seconds"`
	Platforms              []ChannelMonitorV2PlatformConfig `json:"platforms"`
	GroupIDs               []int64                          `json:"group_ids"`
	HealthThresholds       ChannelMonitorV2HealthThresholds `json:"health_thresholds"`
	// IgnoredErrorCategories are excluded from error_rate / health scoring.
	// They still appear in the error breakdown with ignored=true (greyed in UI).
	// Unknown categories always roll into "other" via the taxonomy classifier.
	IgnoredErrorCategories []string  `json:"ignored_error_categories"`
	UpdatedAt              time.Time `json:"updated_at"`
	UpdatedBy              *int64    `json:"updated_by,omitempty"`
}

// ChannelMonitorV2ErrorCategories is the ordered, versioned taxonomy used by the
// classifier and admin settings UI. Unmatched errors become "other".
var ChannelMonitorV2ErrorCategories = []string{
	"content_policy",
	"authentication",
	"context_limit",
	"invalid_request",
	"model_unsupported",
	"group_access",
	"quota_or_balance",
	"account_pool_unavailable",
	"rate_or_capacity",
	"timeout",
	"transport_or_stream",
	"upstream_forbidden",
	"not_found",
	"client_cancelled",
	"upstream_5xx",
	"internal",
	"other",
}

type ChannelMonitorV2Filter struct {
	Range     string
	Platforms []string
	GroupIDs  []int64
	// AllowedGroupIDs is the authenticated viewer's server-derived group scope.
	// RestrictGroups distinguishes an ordinary user with no allowed groups from
	// the unrestricted admin/configured scope represented by an empty slice.
	AllowedGroupIDs []int64
	RestrictGroups  bool
	Models          []string
	Start           time.Time
	End             time.Time
	Bucket          time.Duration
}

type ChannelMonitorV2Metric struct {
	SuccessRequests          int64                   `json:"success_requests"`
	ErrorRequests            int64                   `json:"error_requests"`
	RequestCount             int64                   `json:"request_count"`
	InputTokens              int64                   `json:"input_tokens"`
	OutputTokens             int64                   `json:"output_tokens"`
	CacheCreationTokens      int64                   `json:"cache_creation_tokens"`
	CacheReadTokens          int64                   `json:"cache_read_tokens"`
	TokenCount               int64                   `json:"token_count"`
	RPM                      float64                 `json:"rpm"`
	TPM                      float64                 `json:"tpm"`
	ErrorRate                float64                 `json:"error_rate"`
	SuccessRate              float64                 `json:"success_rate"`
	CacheRate                float64                 `json:"cache_rate"`
	CacheRateNumerator       int64                   `json:"cache_rate_numerator"`
	CacheRateDenominator     int64                   `json:"cache_rate_denominator"`
	TTFT                     ChannelMonitorV2Latency `json:"ttft"`
	Duration                 ChannelMonitorV2Latency `json:"duration"`
	UpstreamAffectedRequests *int64                  `json:"upstream_affected_requests,omitempty"`
	UpstreamAttemptCount     *int64                  `json:"upstream_attempt_count,omitempty"`
}

type ChannelMonitorV2Latency struct {
	SampleCount int64    `json:"sample_count"`
	P50Ms       *int64   `json:"p50_ms"`
	P90Ms       *int64   `json:"p90_ms"`
	P95Ms       *int64   `json:"p95_ms"`
	AvgMs       *float64 `json:"avg_ms"`
}

type ChannelMonitorV2Health struct {
	Overall   string `json:"overall"`
	ErrorRate string `json:"error_rate"`
	TTFT      string `json:"ttft"`
	Cache     string `json:"cache"`
	// Score is 0–100 when samples are sufficient; omitted/null when unknown.
	// Overall blends error-rate, TTFT p50, and cache rate (weights in Thresholds).
	Score          *float64                         `json:"score,omitempty"`
	ErrorRateScore *float64                         `json:"error_rate_score,omitempty"`
	TTFTScore      *float64                         `json:"ttft_score,omitempty"`
	CacheScore     *float64                         `json:"cache_score,omitempty"`
	MinimumSample  int64                            `json:"minimum_sample"`
	Thresholds     ChannelMonitorV2HealthThresholds `json:"thresholds"`
}

type ChannelMonitorV2HealthThresholds struct {
	// MinimumSample is required before scoring request/latency/cache signals.
	MinimumSample int64 `json:"minimum_sample"`
	// WarningErrorRate / CriticalErrorRate map to discrete bands for legacy UI.
	WarningErrorRate  float64 `json:"warning_error_rate"`
	CriticalErrorRate float64 `json:"critical_error_rate"`
	// TargetTTFTMs is the primary TTFT p50 budget (ms). At or below this is full TTFT score.
	TargetTTFTMs   int64 `json:"target_ttft_ms"`
	WarningTTFTMs  int64 `json:"warning_ttft_ms"`
	CriticalTTFTMs int64 `json:"critical_ttft_ms"`
	// WarningCacheRate / CriticalCacheRate: cache rate below these → warning/critical bands.
	// Higher cache rate is better; defaults 20% warning / 5% critical.
	WarningCacheRate  float64 `json:"warning_cache_rate"`
	CriticalCacheRate float64 `json:"critical_cache_rate"`
	// ErrorWeight + TTFTWeight + CacheWeight should sum to 1.0.
	ErrorWeight float64 `json:"error_weight"`
	TTFTWeight  float64 `json:"ttft_weight"`
	CacheWeight float64 `json:"cache_weight"`
}

type ChannelMonitorV2Coverage struct {
	RequestedStart time.Time `json:"requested_start"`
	// RequestedEnd is the exclusive upper bound of the UI-selected window
	// (filter.End). Charts/matrices should plot [RequestedStart, RequestedEnd)
	// even when CoverageStart is later (partial backfill).
	RequestedEnd          time.Time `json:"requested_end"`
	CoverageStart         time.Time `json:"coverage_start"`
	DataThrough           time.Time `json:"data_through"`
	ComputedAt            time.Time `json:"computed_at"`
	AggregationLagSeconds int64     `json:"aggregation_lag_seconds"`
	// CoverageComplete is true when aggregated history reaches the requested
	// window start (backfill / product coverage for this range is done).
	// Trailing lag — data_through behind filter.End because UI ends align to
	// the next bucket and may sit slightly in the future — does not make this
	// false once history is covered.
	CoverageComplete bool `json:"coverage_complete"`
	BucketSeconds    int  `json:"bucket_seconds"`
	// Bootstrap is set while the first-upgrade historical backfill toward the
	// product windows (90m / 24h / 7d / 30d) is still running. Omitted or
	// inactive once the 30d UI range is fully covered; longer retention (90d)
	// may continue silently without a progress banner.
	Bootstrap *ChannelMonitorV2Bootstrap `json:"bootstrap,omitempty"`
}

// ChannelMonitorV2Bootstrap reports first-upgrade aggregation progress for the UI.
// Progress is measured against the longest product window (30d), not full 90d retention.
type ChannelMonitorV2Bootstrap struct {
	// Active is true while historical backfill has not yet covered the 30d product window.
	Active bool `json:"active"`
	// ProgressPercent is 0–100 of history covered from now back toward TargetStart.
	ProgressPercent int `json:"progress_percent"`
	// CoveredFrom is the earliest minute already recomputed (backfill cursor).
	CoveredFrom time.Time `json:"covered_from,omitempty"`
	// TargetStart is now−30d (product bootstrap goal for 90m/24h/7d/30d).
	TargetStart time.Time `json:"target_start,omitempty"`
}

// ChannelMonitorV2AggregationWatermark is the durable aggregator cursor state.
type ChannelMonitorV2AggregationWatermark struct {
	UsageCoverageStart time.Time
	ErrorCoverageStart time.Time
	DataThrough        time.Time
	LastSuccessfulAt   time.Time
	// BackfillCursor is the earliest start already recomputed; zero when never set.
	BackfillCursor time.Time
	// HasData is true once any successful recompute wrote data_through.
	HasData bool
}

type ChannelMonitorV2TrendPoint struct {
	BucketStart time.Time              `json:"bucket_start"`
	Metrics     ChannelMonitorV2Metric `json:"metrics"`
	Health      ChannelMonitorV2Health `json:"health"`
}

type ChannelMonitorV2Snapshot struct {
	Config   ChannelMonitorV2Config       `json:"config"`
	Coverage ChannelMonitorV2Coverage     `json:"coverage"`
	Metrics  ChannelMonitorV2Metric       `json:"metrics"`
	Health   ChannelMonitorV2Health       `json:"health"`
	Trend    []ChannelMonitorV2TrendPoint `json:"trend"`
}

type ChannelMonitorV2Dimension struct {
	Value        string `json:"value"`
	Label        string `json:"label"`
	Platform     string `json:"platform,omitempty"`
	RequestCount int64  `json:"request_count"`
}

type ChannelMonitorV2GroupDimension struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Platform     string `json:"platform,omitempty"`
	RequestCount int64  `json:"request_count"`
}

type ChannelMonitorV2Dimensions struct {
	Platforms []ChannelMonitorV2Dimension      `json:"platforms"`
	Groups    []ChannelMonitorV2GroupDimension `json:"groups"`
	Models    []ChannelMonitorV2Dimension      `json:"models"`
}

type ChannelMonitorV2ModelRow struct {
	Platform string                 `json:"platform"`
	Model    string                 `json:"model"`
	Metrics  ChannelMonitorV2Metric `json:"metrics"`
	Health   ChannelMonitorV2Health `json:"health"`
}

type ChannelMonitorV2MatrixRow struct {
	Platform  string                       `json:"platform"`
	GroupID   *int64                       `json:"group_id,omitempty"`
	GroupName string                       `json:"group_name,omitempty"`
	Model     string                       `json:"model,omitempty"`
	Metrics   ChannelMonitorV2Metric       `json:"metrics"`
	Health    ChannelMonitorV2Health       `json:"health"`
	Buckets   []ChannelMonitorV2TrendPoint `json:"buckets"`
	// C3 展示三态 + DTO 最小契约（行级字段）。
	//   has_data=false → status / source / observed_at 必须为 null（不变量）；
	//   has_data=true  → 三者非空。
	// 前端不得从百分比 / 错误文本 / 时间差反推 source 或有无数据。
	HasData    bool       `json:"has_data"`
	Status     *string    `json:"status,omitempty"`
	Source     *string    `json:"source,omitempty"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
	// FirstTokenMs 是探测来源行的流式首 token 延迟（毫秒），紧邻 ObservedAt；
	// 仅 probe 来源且上游返回了文本增量时非空（has_data=false 行不写）。
	FirstTokenMs *int `json:"first_token_ms,omitempty"`
}

// C3 展示三态 DTO 枚举（行级 status / source 取值）。
const (
	ChannelMonitorV2SourceBusiness = "business"
	ChannelMonitorV2SourceProbe    = "probe"

	ChannelMonitorV2StatusOperational = "operational"
	ChannelMonitorV2StatusDegraded    = "degraded"
	ChannelMonitorV2StatusFailed      = "failed"
	ChannelMonitorV2StatusError       = "error"
)

// mapV2HealthOverallToStatus 将既有 V2 健康四值（healthy/warning/critical/unknown）
// 映射到 C3 DTO 四值（operational/degraded/failed/error）。
// 不改既有四值枚举语义，仅做展示层映射。
func mapV2HealthOverallToStatus(overall string) string {
	switch overall {
	case "healthy":
		return ChannelMonitorV2StatusOperational
	case "warning":
		return ChannelMonitorV2StatusDegraded
	case "critical":
		return ChannelMonitorV2StatusFailed
	default: // unknown（样本不足）当作观测到但未判故障，归 operational。
		return ChannelMonitorV2StatusOperational
	}
}

type ChannelMonitorV2Matrix struct {
	GroupBy  ChannelMonitorV2GroupBy     `json:"group_by"`
	Coverage ChannelMonitorV2Coverage    `json:"coverage"`
	Items    []ChannelMonitorV2MatrixRow `json:"items"`
}

type ChannelMonitorV2ErrorRow struct {
	Category string                        `json:"category"`
	Count    int64                         `json:"count"`
	Rate     float64                       `json:"rate"`
	Details  []ChannelMonitorV2ErrorDetail `json:"details,omitempty"`
	// Ignored is true when this category is excluded from error_rate scoring
	// via config.ignored_error_categories. Still shown in breakdown (UI greys it).
	Ignored bool `json:"ignored"`
}

type ChannelMonitorV2ErrorDetail struct {
	Platform           string `json:"platform,omitempty"`
	Model              string `json:"model,omitempty"`
	ErrorType          string `json:"error_type,omitempty"`
	StatusCode         int    `json:"status_code,omitempty"`
	UpstreamStatusCode int    `json:"upstream_status_code,omitempty"`
	Message            string `json:"message,omitempty"`
	Count              int64  `json:"count"`
}

type ChannelMonitorV2UserRow struct {
	UserID       *int64                 `json:"user_id,omitempty"`
	Rank         int                    `json:"rank"`
	Email        string                 `json:"email,omitempty"`
	Username     string                 `json:"username,omitempty"`
	DisplayLabel string                 `json:"display_label"`
	IsSelf       bool                   `json:"is_self"`
	CanDrilldown bool                   `json:"can_drilldown"`
	Metrics      ChannelMonitorV2Metric `json:"metrics"`
}

type ChannelMonitorV2List[T any] struct {
	Coverage ChannelMonitorV2Coverage `json:"coverage"`
	Items    []T                      `json:"items"`
}

type ChannelMonitorV2Repository interface {
	GetConfig(ctx context.Context) (*ChannelMonitorV2Config, error)
	UpdateConfig(ctx context.Context, config ChannelMonitorV2Config, expectedVersion int) (*ChannelMonitorV2Config, error)
	GetDimensions(ctx context.Context, filter ChannelMonitorV2Filter, config ChannelMonitorV2Config) (*ChannelMonitorV2Dimensions, error)
	GetSnapshot(ctx context.Context, filter ChannelMonitorV2Filter, config ChannelMonitorV2Config, includeAdmin bool) (*ChannelMonitorV2Snapshot, error)
	GetModels(ctx context.Context, filter ChannelMonitorV2Filter, config ChannelMonitorV2Config, includeAdmin bool) (*ChannelMonitorV2List[ChannelMonitorV2ModelRow], error)
	GetMatrix(ctx context.Context, filter ChannelMonitorV2Filter, config ChannelMonitorV2Config, groupBy ChannelMonitorV2GroupBy, includeAdmin bool) (*ChannelMonitorV2Matrix, error)
	// GetErrors loads category rates. When includeAdmin is false, implementations
	// must omit error Details (no ops_error_logs sample scan) for privacy.
	GetErrors(ctx context.Context, filter ChannelMonitorV2Filter, config ChannelMonitorV2Config, includeAdmin bool) (*ChannelMonitorV2List[ChannelMonitorV2ErrorRow], error)
	GetUsers(ctx context.Context, filter ChannelMonitorV2Filter, config ChannelMonitorV2Config, includeAdmin bool) (*ChannelMonitorV2List[ChannelMonitorV2UserRow], error)
	// GetAggregationWatermark loads durable backfill / coverage cursors for the
	// passive aggregator (and bootstrap progress). Missing row → zero value, nil error.
	GetAggregationWatermark(ctx context.Context) (*ChannelMonitorV2AggregationWatermark, error)
	RecomputeRange(ctx context.Context, start, end time.Time) error
	// GetProbeStates returns the most recent probe result for each requested
	// (platform, model, group) triple, read from channel_monitor_probe_state.
	// Triples with no row are simply absent from the returned map (never an error).
	// Used by the C3 display layer to render the "抽样探测" state for on-sale channels
	// that have no business traffic.
	GetProbeStates(ctx context.Context, keys []ChannelMonitorCatalogKey) (map[ChannelMonitorCatalogKey]*ProbeState, error)
}

// ChannelMonitorV2BootstrapProductWindow is the longest UI range that must be
// filled on first upgrade before the bootstrap banner disappears (30d).
const ChannelMonitorV2BootstrapProductWindow = 30 * 24 * time.Hour

// ChannelMonitorV2BootstrapProgress builds the optional UI progress payload.
// now and coveredFrom should be UTC minute-truncated when possible.
// When history is complete for the 30d product window, returns nil (no banner).
func ChannelMonitorV2BootstrapProgress(now, coveredFrom time.Time, hasData bool) *ChannelMonitorV2Bootstrap {
	now = now.UTC().Truncate(time.Minute)
	targetStart := now.Add(-ChannelMonitorV2BootstrapProductWindow)
	if !hasData {
		return &ChannelMonitorV2Bootstrap{
			Active:          true,
			ProgressPercent: 0,
			TargetStart:     targetStart,
		}
	}
	if coveredFrom.IsZero() {
		// Have data_through but no cursor yet — treat as barely started.
		return &ChannelMonitorV2Bootstrap{
			Active:          true,
			ProgressPercent: 0,
			TargetStart:     targetStart,
		}
	}
	coveredFrom = coveredFrom.UTC().Truncate(time.Minute)
	if !coveredFrom.After(targetStart) {
		// 30d product windows fully covered; hide progress (90d retention may continue).
		return nil
	}
	total := now.Sub(targetStart).Seconds()
	if total <= 0 {
		return nil
	}
	done := now.Sub(coveredFrom).Seconds()
	if done < 0 {
		done = 0
	}
	if done > total {
		done = total
	}
	// Round to nearest percent; any non-zero history shows at least 1% so the
	// bar moves after the first 2h bootstrap tick (2h/30d ≈ 0.3%).
	pct := int((done/total)*100 + 0.5)
	if done > 0 && pct < 1 {
		pct = 1
	}
	if pct > 100 {
		pct = 100
	}
	if pct >= 100 {
		// Cursor still slightly after target due to rounding — keep active until
		// coveredFrom <= targetStart so the banner does not flash 100% then vanish.
		pct = 99
	}
	return &ChannelMonitorV2Bootstrap{
		Active:          true,
		ProgressPercent: pct,
		CoveredFrom:     coveredFrom,
		TargetStart:     targetStart,
	}
}

type ChannelMonitorV2Service struct {
	repo     ChannelMonitorV2Repository
	settings channelMonitorRuntimeReader
	now      func() time.Time
	// accountRepo / groupRepo feed the C3 catalog derivation (DeriveChannelMonitorCatalog)
	// so the display matrix can surface on-sale channels that have no business traffic.
	// Injected via setters to keep the constructor signature stable for existing tests.
	accountRepo AccountRepository
	groupRepo   GroupRepository
}

func NewChannelMonitorV2Service(repo ChannelMonitorV2Repository) *ChannelMonitorV2Service {
	return &ChannelMonitorV2Service{repo: repo, now: func() time.Time { return time.Now().UTC() }}
}

// SetAccountRepository wires the account repository used by C3 catalog derivation.
func (s *ChannelMonitorV2Service) SetAccountRepository(r AccountRepository) {
	if s == nil {
		return
	}
	s.accountRepo = r
}

// SetGroupRepository wires the group repository used by C3 catalog derivation.
func (s *ChannelMonitorV2Service) SetGroupRepository(r GroupRepository) {
	if s == nil {
		return
	}
	s.groupRepo = r
}

// SetRuntimeReader wires optional settings for privacy flags (hide throughput).
func (s *ChannelMonitorV2Service) SetRuntimeReader(r channelMonitorRuntimeReader) {
	if s == nil {
		return
	}
	s.settings = r
}

func (s *ChannelMonitorV2Service) hideThroughputForViewer(ctx context.Context, admin bool) bool {
	if admin {
		return false
	}
	// Privacy is fail-closed: an absent or unavailable settings reader must not
	// expose fleet-scale rates to ordinary users.
	if s == nil || s.settings == nil {
		return true
	}
	return s.settings.GetChannelMonitorRuntime(ctx).HideThroughput
}

func (s *ChannelMonitorV2Service) hideUserRankingForViewer(ctx context.Context, admin bool) bool {
	if admin {
		return false
	}
	// Missing reader keeps the current ranking tab visible. Unlike throughput,
	// ranking is already public and must stay on until an operator turns it off.
	if s == nil || s.settings == nil {
		return false
	}
	return s.settings.GetChannelMonitorRuntime(ctx).HideUserRanking
}

func (s *ChannelMonitorV2Service) GetConfig(ctx context.Context) (*ChannelMonitorV2Config, error) {
	return s.repo.GetConfig(ctx)
}

func (s *ChannelMonitorV2Service) getEnabledConfig(ctx context.Context) (*ChannelMonitorV2Config, error) {
	cfg, err := s.repo.GetConfig(ctx)
	if err != nil {
		return nil, err
	}
	if cfg == nil || !cfg.Enabled {
		return nil, ErrChannelMonitorDisabled
	}
	return cfg, nil
}

func (s *ChannelMonitorV2Service) UpdateConfig(ctx context.Context, cfg ChannelMonitorV2Config, expectedVersion int, actorID int64) (*ChannelMonitorV2Config, error) {
	if err := normalizeChannelMonitorV2Config(&cfg); err != nil {
		return nil, err
	}
	cfg.UpdatedBy = &actorID
	return s.repo.UpdateConfig(ctx, cfg, expectedVersion)
}

func (s *ChannelMonitorV2Service) ParseFilter(rangeValue string, platforms, models []string, groupIDs []int64) (ChannelMonitorV2Filter, error) {
	now := s.now().UTC()
	var window, bucket time.Duration
	switch strings.TrimSpace(rangeValue) {
	case "", "90m":
		rangeValue, window, bucket = "90m", 90*time.Minute, 5*time.Minute
	case "24h":
		window, bucket = 24*time.Hour, time.Hour
	case "7d":
		window, bucket = 7*24*time.Hour, 12*time.Hour
	case "30d":
		window, bucket = 30*24*time.Hour, 24*time.Hour
	default:
		return ChannelMonitorV2Filter{}, fmt.Errorf("%w: %s", ErrChannelMonitorV2InvalidRange, rangeValue)
	}
	start, end := now.Add(-window), now
	if bucket > time.Minute {
		// Align display windows to a fixed number of whole buckets. The trailing
		// bucket may point slightly into the future; SQL simply has no future rows,
		// while the current partial bucket can still be shown and recomputed often.
		end = now.Truncate(bucket).Add(bucket)
		start = end.Add(-window)
	}
	return ChannelMonitorV2Filter{
		Range: rangeValue, Platforms: normalizeStringSet(platforms), Models: normalizeStringSet(models), GroupIDs: normalizeInt64Set(groupIDs),
		Start: start, End: end, Bucket: bucket,
	}, nil
}

func (s *ChannelMonitorV2Service) Dimensions(ctx context.Context, filter ChannelMonitorV2Filter) (*ChannelMonitorV2Dimensions, error) {
	cfg, err := s.getEnabledConfig(ctx)
	if err != nil {
		return nil, err
	}
	dims, err := s.repo.GetDimensions(ctx, filter, *cfg)
	if err != nil {
		return nil, err
	}
	// Dimension request_count is operational volume; strip for non-admin callers
	// at the API edge. Dimensions is shared by user/admin routes — redaction is
	// applied in the handler for user routes only, so keep raw here.
	return dims, nil
}

func (s *ChannelMonitorV2Service) Snapshot(ctx context.Context, filter ChannelMonitorV2Filter, admin bool) (*ChannelMonitorV2Snapshot, error) {
	cfg, err := s.getEnabledConfig(ctx)
	if err != nil {
		return nil, err
	}
	snap, err := s.repo.GetSnapshot(ctx, filter, *cfg, admin)
	if err != nil {
		return nil, err
	}
	if !admin && snap != nil {
		redactChannelMonitorV2Snapshot(snap, s.hideThroughputForViewer(ctx, admin))
	}
	return snap, nil
}

func (s *ChannelMonitorV2Service) Models(ctx context.Context, filter ChannelMonitorV2Filter, admin bool) (*ChannelMonitorV2List[ChannelMonitorV2ModelRow], error) {
	cfg, err := s.getEnabledConfig(ctx)
	if err != nil {
		return nil, err
	}
	list, err := s.repo.GetModels(ctx, filter, *cfg, admin)
	if err != nil {
		return nil, err
	}
	if !admin && list != nil {
		hideTP := s.hideThroughputForViewer(ctx, admin)
		for i := range list.Items {
			redactChannelMonitorV2Metric(&list.Items[i].Metrics, hideTP)
		}
	}
	return list, nil
}

func (s *ChannelMonitorV2Service) Matrix(ctx context.Context, filter ChannelMonitorV2Filter, groupBy ChannelMonitorV2GroupBy, admin bool) (*ChannelMonitorV2Matrix, error) {
	if !groupBy.Valid() {
		return nil, fmt.Errorf("%w: %s", ErrChannelMonitorV2InvalidGroupBy, groupBy)
	}
	cfg, err := s.getEnabledConfig(ctx)
	if err != nil {
		return nil, err
	}
	matrix, err := s.repo.GetMatrix(ctx, filter, *cfg, groupBy, admin)
	if err != nil {
		return nil, err
	}
	matrix = s.enrichMatrixWithCatalog(ctx, *cfg, groupBy, filter, matrix)
	if !admin && matrix != nil {
		hideTP := s.hideThroughputForViewer(ctx, admin)
		for i := range matrix.Items {
			redactChannelMonitorV2Metric(&matrix.Items[i].Metrics, hideTP)
			for j := range matrix.Items[i].Buckets {
				redactChannelMonitorV2Metric(&matrix.Items[i].Buckets[j].Metrics, hideTP)
			}
		}
	}
	return matrix, nil
}

// enrichMatrixWithCatalog applies the C3 three-state display contract to matrix rows:
//
//   - ① business: row has business traffic in the window (RequestCount > 0) → keep the
//     existing V2 row, source=business, status from health.Overall.
//   - ② no business traffic but a fresh probe_state row (≤30m) → source=probe with the
//     "抽样探测" semantics; status reflects the sampled probe result (NOT channel-wide).
//   - ③ neither → has_data=false with status/source/observed_at all null ("无数据").
//
// On-sale channels without business traffic must still appear (user requirement #5);
// the catalog (platform, model, group) triples are projected to the requested groupBy
// grain so coarser views still surface them. When the catalog cannot be derived (account
// or group repository not injected), every traffic row is enriched as business and no
// catalog-derived rows are added (degraded, test-friendly path).
func (s *ChannelMonitorV2Service) enrichMatrixWithCatalog(ctx context.Context, cfg ChannelMonitorV2Config, groupBy ChannelMonitorV2GroupBy, filter ChannelMonitorV2Filter, base *ChannelMonitorV2Matrix) *ChannelMonitorV2Matrix {
	if base == nil {
		return nil
	}
	if s.accountRepo == nil || s.groupRepo == nil {
		// Degraded path: no catalog available. Enrich existing traffic rows as business.
		for i := range base.Items {
			row := &base.Items[i]
			if row.Metrics.RequestCount > 0 {
				enrichBusinessMatrixRow(row, base.Coverage, s.now)
			}
		}
		return base
	}

	accounts, err := s.accountRepo.ListActive(ctx)
	if err != nil {
		return base
	}
	groups, err := s.groupRepo.ListActive(ctx)
	if err != nil {
		return base
	}
	groupsByID := make(map[int64]*Group, len(groups))
	for i := range groups {
		groupsByID[groups[i].ID] = &groups[i]
	}
	catalog := DeriveChannelMonitorCatalog(accounts, groupsByID)
	probeStates, perr := s.repo.GetProbeStates(ctx, catalogTriples(catalog))
	if perr != nil {
		probeStates = map[ChannelMonitorCatalogKey]*ProbeState{}
	}
	return channelMonitorV2MergeCatalogMatrix(cfg, groupBy, catalog, groupsByID, base, filter, probeStates, s.now)
}

// catalogTriples returns the distinct (platform, model, group) keys of a catalog.
func catalogTriples(catalog ChannelMonitorCatalog) []ChannelMonitorCatalogKey {
	keys := make([]ChannelMonitorCatalogKey, 0, len(catalog.Keys))
	for key := range catalog.Keys {
		keys = append(keys, key)
	}
	return keys
}

// channelMonitorV2MergeCatalogMatrix is the pure C3 three-state merge: it combines the
// catalog-derived on-sale set with the traffic matrix and probe states (already fetched)
// into a single display matrix. It is deterministic and independent of any repository,
// which keeps the three-state contract unit-testable without DB or repo fakes.
//
//   - ① business: a grain key has traffic (RequestCount > 0) → keep that V2 row, source=business.
//   - ② no business traffic but a fresh probe (≤30m) exists for the grain's triples → source=probe.
//   - ③ otherwise → has_data=false with status/source/observed_at all null ("无数据").
//
// Zero-traffic seeded matrix rows are dropped; the catalog drives probe/no-data for them.
func channelMonitorV2MergeCatalogMatrix(
	cfg ChannelMonitorV2Config,
	groupBy ChannelMonitorV2GroupBy,
	catalog ChannelMonitorCatalog,
	groupsByID map[int64]*Group,
	base *ChannelMonitorV2Matrix,
	filter ChannelMonitorV2Filter,
	probeStates map[ChannelMonitorCatalogKey]*ProbeState,
	now func() time.Time,
) *ChannelMonitorV2Matrix {
	if base == nil {
		return nil
	}

	// Grain buckets: each on-sale catalog triple projected to the groupBy grain.
	type grainBucket struct {
		platform  string
		groupID   int64
		groupName string
		model     string
		triples   []ChannelMonitorCatalogKey
	}
	grainByKey := make(map[string]*grainBucket, len(catalog.Keys))
	var grainOrder []string
	for key := range catalog.Keys {
		// R1-F1 must_fix: viewer group-scope gate. Catalog triples outside the
		// viewer's AllowedGroupIDs must not surface (their model would otherwise
		// leak into the de-platformed model grain). Admin path keeps RestrictGroups=false.
		if filter.RestrictGroups && !channelMonitorV2GroupAllowed(filter.AllowedGroupIDs, key.GroupID) {
			continue
		}
		// R1-F2 must_fix: reuse the fact-side display-model selection so a viewer
		// who selected model A does not see an unselected no-traffic model B.
		if !channelMonitorV2ModelSelected(filter, key.Model) {
			continue
		}
		gk := channelMonitorV2RowGrainKey(groupBy, cfg, key.Platform, key.GroupID, key.Model)
		bucket := grainByKey[gk]
		if bucket == nil {
			bucketPlatform := key.Platform
			if groupBy == ChannelMonitorV2GroupByModel {
				// C2/C6: model grain must not carry a platform (no channel leakage).
				bucketPlatform = ""
			}
			bucket = &grainBucket{
				platform: bucketPlatform,
				groupID:  channelMonitorV2RowGroupID(groupBy, key.GroupID),
				model:    channelMonitorV2RowModel(groupBy, cfg, key.Model),
			}
			if bucket.groupID > 0 {
				if g, ok := groupsByID[bucket.groupID]; ok {
					bucket.groupName = g.Name
				}
			}
			grainByKey[gk] = bucket
			grainOrder = append(grainOrder, gk)
		}
		bucket.triples = append(bucket.triples, key)
	}

	// Business rows from real traffic (RequestCount > 0). Zero-traffic seeded rows are
	// dropped here; catalog drives probe/no-data for them below.
	businessKeys := make(map[string]bool, len(base.Items))
	result := &ChannelMonitorV2Matrix{GroupBy: groupBy, Coverage: base.Coverage, Items: make([]ChannelMonitorV2MatrixRow, 0, len(base.Items)+len(grainByKey))}
	for i := range base.Items {
		row := base.Items[i]
		if row.Metrics.RequestCount <= 0 {
			continue
		}
		gk := channelMonitorV2RowGrainKey(groupBy, cfg, row.Platform, derefInt64(row.GroupID), row.Model)
		enrichBusinessMatrixRow(&row, base.Coverage, now)
		businessKeys[gk] = true
		result.Items = append(result.Items, row)
	}

	for _, gk := range grainOrder {
		bucket := grainByKey[gk]
		if businessKeys[gk] {
			continue
		}
		var fresh *ProbeState
		for _, t := range bucket.triples {
			st := probeStates[t]
			if st == nil || st.LastProbeAt.IsZero() {
				continue
			}
			// B4：废除 30 分钟新鲜度门——最新一次探测结果无条件作为探测来源状态；
			// 仅当该渠道从未有 probe_state 时（fresh 保持 nil）保持 has_data=false。
			if fresh == nil || st.LastProbeAt.After(fresh.LastProbeAt) {
				fresh = st
			}
		}
		row := ChannelMonitorV2MatrixRow{Platform: bucket.platform, Model: bucket.model, HasData: false}
		if bucket.groupID > 0 {
			id := bucket.groupID
			row.GroupID = &id
			row.GroupName = bucket.groupName
		}
		if fresh != nil {
			// ② probe source: a sampled single-account probe, not channel-wide availability.
			row.HasData = true
			src := ChannelMonitorV2SourceProbe
			row.Source = &src
			status := ChannelMonitorV2StatusFailed
			if fresh.LastOK {
				status = ChannelMonitorV2StatusOperational
			}
			row.Status = &status
			obs := fresh.LastProbeAt
			row.ObservedAt = &obs
			row.FirstTokenMs = fresh.FirstTokenMs
		}
		// ③ else: has_data=false → status/source/observed_at/first_token_ms stay nil ("无数据").
		result.Items = append(result.Items, row)
	}

	sortChannelMonitorV2MatrixRows(result.Items)
	return result
}

// enrichBusinessMatrixRow stamps the business-state DTO fields on a traffic row.
func enrichBusinessMatrixRow(row *ChannelMonitorV2MatrixRow, coverage ChannelMonitorV2Coverage, now func() time.Time) {
	row.HasData = true
	src := ChannelMonitorV2SourceBusiness
	row.Source = &src
	status := mapV2HealthOverallToStatus(row.Health.Overall)
	row.Status = &status
	row.ObservedAt = businessObservedAt(row, coverage, now)
}

// businessObservedAt returns the latest timestamp the business row was actually observed,
// preferring the newest bucket carrying traffic, then the aggregate data_through.
func businessObservedAt(row *ChannelMonitorV2MatrixRow, coverage ChannelMonitorV2Coverage, now func() time.Time) *time.Time {
	var latest *time.Time
	for i := range row.Buckets {
		if row.Buckets[i].Metrics.RequestCount > 0 {
			b := row.Buckets[i].BucketStart
			if latest == nil || b.After(*latest) {
				cp := b
				latest = &cp
			}
		}
	}
	if latest != nil {
		return latest
	}
	if !coverage.DataThrough.IsZero() {
		t := coverage.DataThrough
		return &t
	}
	n := now().UTC()
	return &n
}

// channelMonitorV2RowGrainKey projects a (platform, model, group) triple to the
// groupBy display grain, mirroring repository.channelMonitorV2MatrixDimensionKey.
func channelMonitorV2RowGrainKey(groupBy ChannelMonitorV2GroupBy, cfg ChannelMonitorV2Config, platform string, groupID int64, model string) string {
	// C2/C6: model grain collapses across platform + group — key is only the
	// display model, so same-named models on different platforms merge into one row.
	if groupBy == ChannelMonitorV2GroupByModel {
		return channelMonitorV2RowModel(groupBy, cfg, model)
	}
	key := platform
	if groupBy == ChannelMonitorV2GroupByPlatformGroup || groupBy == ChannelMonitorV2GroupByPlatformGroupModel {
		key += "\x00" + strconv.FormatInt(channelMonitorV2RowGroupID(groupBy, groupID), 10)
	}
	if groupBy == ChannelMonitorV2GroupByPlatformModel || groupBy == ChannelMonitorV2GroupByPlatformGroupModel {
		key += "\x00" + channelMonitorV2RowModel(groupBy, cfg, model)
	}
	return key
}

// channelMonitorV2RowGroupID returns the groupID for the grain, or 0 when the grain
// does not include the group dimension.
func channelMonitorV2RowGroupID(groupBy ChannelMonitorV2GroupBy, groupID int64) int64 {
	if groupBy == ChannelMonitorV2GroupByPlatformGroup || groupBy == ChannelMonitorV2GroupByPlatformGroupModel {
		return groupID
	}
	return 0
}

// channelMonitorV2RowModel returns the display model for the grain, or "" when the
// grain does not include the model dimension. For the model grain (C2/C6) it returns
// the display model directly (empty model name folds to ChannelMonitorV2OtherModel),
// matching repository.channelMonitorV2DisplayModel so cross-platform merge keys agree.
func channelMonitorV2RowModel(groupBy ChannelMonitorV2GroupBy, cfg ChannelMonitorV2Config, model string) string {
	if groupBy != ChannelMonitorV2GroupByPlatformModel &&
		groupBy != ChannelMonitorV2GroupByPlatformGroupModel &&
		groupBy != ChannelMonitorV2GroupByModel {
		return ""
	}
	m := strings.TrimSpace(model)
	if m == "" {
		return ChannelMonitorV2OtherModel
	}
	return m
}

// sortChannelMonitorV2MatrixRows orders rows by platform, group name, group id, model —
// same comparator the repository uses for stable display.
func sortChannelMonitorV2MatrixRows(items []ChannelMonitorV2MatrixRow) {
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.Platform != b.Platform {
			return a.Platform < b.Platform
		}
		if a.GroupName != b.GroupName {
			return a.GroupName < b.GroupName
		}
		if a.GroupID != nil && b.GroupID != nil && *a.GroupID != *b.GroupID {
			return *a.GroupID < *b.GroupID
		}
		return a.Model < b.Model
	})
}

func derefInt64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

// channelMonitorV2GroupAllowed reports whether groupID is within the viewer's
// allowed set. Empty allowed set with RestrictGroups is handled by the caller
// (catalog triples are skipped before reaching here), so a non-empty set is an
// explicit membership test.
func channelMonitorV2GroupAllowed(allowed []int64, groupID int64) bool {
	for _, id := range allowed {
		if id == groupID {
			return true
		}
	}
	return false
}

// channelMonitorV2ModelSelected mirrors repository.channelMonitorV2ModelSelected's
// display-model selection semantics for catalog-derived (probe / no-data) rows, so
// the same rule that filters fact traffic also filters catalog补行. The repository
// function cannot be called from this package (service ← repository import cycle),
// so the identical logic lives here intentionally and must be kept in sync.
func channelMonitorV2ModelSelected(filter ChannelMonitorV2Filter, model string) bool {
	if len(filter.Models) == 0 {
		return true
	}
	display := strings.TrimSpace(model)
	if display == "" {
		display = ChannelMonitorV2OtherModel
	}
	for _, selected := range filter.Models {
		if selected == display {
			return true
		}
	}
	return false
}

func ParseChannelMonitorV2GroupBy(value string) (ChannelMonitorV2GroupBy, error) {
	groupBy := ChannelMonitorV2GroupBy(strings.TrimSpace(value))
	if groupBy == "" {
		// Default presentation is now the de-platformed model grain (C2/C6):
		// cross-platform same-model rows merge into one row with no channel leakage.
		groupBy = ChannelMonitorV2GroupByModel
	}
	if !groupBy.Valid() {
		return "", fmt.Errorf("%w: %s", ErrChannelMonitorV2InvalidGroupBy, value)
	}
	return groupBy, nil
}

func (g ChannelMonitorV2GroupBy) Valid() bool {
	switch g {
	case ChannelMonitorV2GroupByPlatform, ChannelMonitorV2GroupByPlatformGroup, ChannelMonitorV2GroupByPlatformModel, ChannelMonitorV2GroupByPlatformGroupModel, ChannelMonitorV2GroupByModel:
		return true
	default:
		return false
	}
}

func (s *ChannelMonitorV2Service) Errors(ctx context.Context, filter ChannelMonitorV2Filter) (*ChannelMonitorV2List[ChannelMonitorV2ErrorRow], error) {
	return s.ErrorsForViewer(ctx, filter, false)
}

// ErrorsForViewer returns the error breakdown.
// Non-admin callers receive category rates + ignored flags only: absolute Count
// is zeroed and Details (upstream messages / status codes / volume) are omitted.
func (s *ChannelMonitorV2Service) ErrorsForViewer(ctx context.Context, filter ChannelMonitorV2Filter, admin bool) (*ChannelMonitorV2List[ChannelMonitorV2ErrorRow], error) {
	cfg, err := s.getEnabledConfig(ctx)
	if err != nil {
		return nil, err
	}
	list, err := s.repo.GetErrors(ctx, filter, *cfg, admin)
	if err != nil {
		return nil, err
	}
	if !admin && list != nil {
		for i := range list.Items {
			list.Items[i].Count = 0
			list.Items[i].Details = nil
		}
	}
	return list, nil
}

// RedactChannelMonitorV2Dimensions clears absolute request counts on filter chips.
func RedactChannelMonitorV2Dimensions(dims *ChannelMonitorV2Dimensions) {
	if dims == nil {
		return
	}
	for i := range dims.Platforms {
		dims.Platforms[i].RequestCount = 0
	}
	for i := range dims.Models {
		dims.Models[i].RequestCount = 0
	}
	for i := range dims.Groups {
		dims.Groups[i].RequestCount = 0
	}
}

func redactChannelMonitorV2Snapshot(snap *ChannelMonitorV2Snapshot, hideThroughput bool) {
	if snap == nil {
		return
	}
	redactChannelMonitorV2Metric(&snap.Metrics, hideThroughput)
	for i := range snap.Trend {
		redactChannelMonitorV2Metric(&snap.Trend[i].Metrics, hideThroughput)
	}
	// Public snapshot only needs display thresholds + refresh cadence, not
	// operational allow-lists (group_ids, model inventories, ignored categories).
	redactChannelMonitorV2PublicConfig(&snap.Config)
}

// redactChannelMonitorV2PublicConfig strips operator-policy fields from config
// embedded in user-facing snapshots. Full config remains on admin /config.
func redactChannelMonitorV2PublicConfig(cfg *ChannelMonitorV2Config) {
	if cfg == nil {
		return
	}
	cfg.GroupIDs = nil
	cfg.IgnoredErrorCategories = nil
	cfg.UpdatedBy = nil
	for i := range cfg.Platforms {
		cfg.Platforms[i].Models = nil
	}
}

// redactChannelMonitorV2Metric zeros absolute volume counters while keeping rates
// (error_rate, success_rate, cache_rate) and latency percentiles.
// When hideThroughput is true, also zeros RPM/TPM so users cannot reverse-estimate
// fleet scale from rate × window length.
func redactChannelMonitorV2Metric(m *ChannelMonitorV2Metric, hideThroughput bool) {
	if m == nil {
		return
	}
	m.SuccessRequests = 0
	m.ErrorRequests = 0
	m.RequestCount = 0
	m.InputTokens = 0
	m.OutputTokens = 0
	m.CacheCreationTokens = 0
	m.CacheReadTokens = 0
	m.TokenCount = 0
	m.CacheRateNumerator = 0
	m.CacheRateDenominator = 0
	// Latency sample_count is also a volume signal.
	m.TTFT.SampleCount = 0
	m.Duration.SampleCount = 0
	m.UpstreamAffectedRequests = nil
	m.UpstreamAttemptCount = nil
	if hideThroughput {
		m.RPM = 0
		m.TPM = 0
	}
}

func (s *ChannelMonitorV2Service) Users(ctx context.Context, filter ChannelMonitorV2Filter, viewerID int64, admin bool) (*ChannelMonitorV2List[ChannelMonitorV2UserRow], error) {
	cfg, err := s.getEnabledConfig(ctx)
	if err != nil {
		return nil, err
	}
	if s.hideUserRankingForViewer(ctx, admin) {
		return &ChannelMonitorV2List[ChannelMonitorV2UserRow]{Items: []ChannelMonitorV2UserRow{}}, nil
	}
	result, err := s.repo.GetUsers(ctx, filter, *cfg, admin)
	if err != nil {
		return nil, err
	}
	if result == nil {
		result = &ChannelMonitorV2List[ChannelMonitorV2UserRow]{}
	}
	selfIndex := -1
	for i := range result.Items {
		result.Items[i].Rank = i + 1
		if result.Items[i].UserID != nil && *result.Items[i].UserID == viewerID {
			selfIndex = i
			result.Items[i].IsSelf = true
		}
	}
	// Viewer with no traffic in this window is still shown (and highlighted) so
	// ranking always answers "where am I?" — not only when already in the top list.
	if selfIndex < 0 && viewerID > 0 {
		id := viewerID
		selfRow := ChannelMonitorV2UserRow{
			UserID:       &id,
			Rank:         0, // unranked / no traffic in window
			IsSelf:       true,
			CanDrilldown: true,
			DisplayLabel: "Me",
			Metrics:      ChannelMonitorV2Metric{},
		}
		result.Items = append(result.Items, selfRow)
		selfIndex = len(result.Items) - 1
	}
	result.Items = channelMonitorV2TopUsersWithSelf(result.Items, selfIndex, 10)
	hideTP := s.hideThroughputForViewer(ctx, admin)
	if admin {
		// Keep identity for admin; still mark self for UI highlight.
		for i := range result.Items {
			if result.Items[i].UserID != nil && *result.Items[i].UserID == viewerID {
				result.Items[i].IsSelf = true
				if result.Items[i].DisplayLabel == "" || result.Items[i].DisplayLabel == "Me" {
					// Prefer real label when available from repo.
					if result.Items[i].Username != "" {
						result.Items[i].DisplayLabel = result.Items[i].Username
					} else if result.Items[i].Email != "" {
						result.Items[i].DisplayLabel = result.Items[i].Email
					} else {
						result.Items[i].DisplayLabel = "Me"
					}
				}
			}
		}
		return result, nil
	}
	for i := range result.Items {
		redactChannelMonitorV2Metric(&result.Items[i].Metrics, hideTP)
	}
	for i := range result.Items {
		row := &result.Items[i]
		if row.UserID != nil && *row.UserID == viewerID {
			row.IsSelf, row.CanDrilldown, row.DisplayLabel = true, true, "Me"
			continue
		}
		row.UserID, row.Email, row.Username, row.CanDrilldown = nil, "", "", false
		row.DisplayLabel = fmt.Sprintf("Other user #%d", i+1)
	}
	return result, nil
}

func channelMonitorV2TopUsersWithSelf(items []ChannelMonitorV2UserRow, selfIndex int, limit int) []ChannelMonitorV2UserRow {
	if limit <= 0 {
		return items
	}
	if len(items) <= limit {
		return items
	}
	out := append([]ChannelMonitorV2UserRow(nil), items[:limit]...)
	if selfIndex >= limit && selfIndex < len(items) {
		out = append(out, items[selfIndex])
	}
	return out
}

func normalizeChannelMonitorV2Config(cfg *ChannelMonitorV2Config) error {
	if cfg.RefreshIntervalSeconds == 0 {
		cfg.RefreshIntervalSeconds = 300
	}
	if cfg.RefreshIntervalSeconds != 60 && cfg.RefreshIntervalSeconds != 300 {
		return fmt.Errorf("%w: refresh_interval_seconds must be 60 or 300", ErrChannelMonitorV2InvalidConfig)
	}
	var err error
	cfg.GroupIDs, err = normalizeChannelMonitorV2GroupIDs(cfg.GroupIDs)
	if err != nil {
		return err
	}
	cfg.IgnoredErrorCategories = normalizeChannelMonitorV2IgnoredCategories(cfg.IgnoredErrorCategories)
	cfg.HealthThresholds = NormalizeChannelMonitorV2HealthThresholds(cfg.HealthThresholds)
	seen := make(map[string]struct{}, len(cfg.Platforms))
	for i := range cfg.Platforms {
		p := &cfg.Platforms[i]
		p.Platform = strings.ToLower(strings.TrimSpace(p.Platform))
		if p.Platform == "" {
			return fmt.Errorf("%w: empty platform", ErrChannelMonitorV2InvalidConfig)
		}
		if _, ok := seen[p.Platform]; ok {
			return fmt.Errorf("%w: duplicate platform %s", ErrChannelMonitorV2InvalidConfig, p.Platform)
		}
		seen[p.Platform] = struct{}{}
		p.Models = normalizeStringSet(p.Models)
	}
	sort.Slice(cfg.Platforms, func(i, j int) bool { return cfg.Platforms[i].Platform < cfg.Platforms[j].Platform })
	return nil
}

// DefaultChannelMonitorV2IgnoredErrorCategories are factory defaults for
// ignored_error_categories: excluded from error_rate / health scoring only.
// Operators can clear or extend via admin config.
var DefaultChannelMonitorV2IgnoredErrorCategories = []string{
	"authentication",
	"client_cancelled",
	"content_policy",
	"context_limit",
	"group_access",
	"model_unsupported",
	"not_found",
	"quota_or_balance",
}

func DefaultChannelMonitorV2HealthThresholds() ChannelMonitorV2HealthThresholds {
	return ChannelMonitorV2HealthThresholds{
		MinimumSample:     50,
		WarningErrorRate:  0.05,
		CriticalErrorRate: 0.20,
		TargetTTFTMs:      3000,
		WarningTTFTMs:     3000,
		CriticalTTFTMs:    10000,
		// A zero/zero cache threshold means cache misses do not affect health
		// until an operator explicitly configures cache scoring.
		WarningCacheRate:  0,
		CriticalCacheRate: 0,
		ErrorWeight:       0.60,
		TTFTWeight:        0.20,
		CacheWeight:       0.20,
	}
}

func NormalizeChannelMonitorV2HealthThresholds(in ChannelMonitorV2HealthThresholds) ChannelMonitorV2HealthThresholds {
	def := DefaultChannelMonitorV2HealthThresholds()
	if in.MinimumSample <= 0 {
		in.MinimumSample = def.MinimumSample
	}
	if in.MinimumSample < 1 {
		in.MinimumSample = 1
	}
	if in.MinimumSample > 10000 {
		in.MinimumSample = 10000
	}
	if in.WarningErrorRate <= 0 {
		in.WarningErrorRate = def.WarningErrorRate
	}
	if in.CriticalErrorRate <= 0 {
		in.CriticalErrorRate = def.CriticalErrorRate
	}
	if in.CriticalErrorRate < in.WarningErrorRate {
		in.CriticalErrorRate = in.WarningErrorRate
	}
	if in.TargetTTFTMs <= 0 {
		in.TargetTTFTMs = def.TargetTTFTMs
	}
	if in.WarningTTFTMs <= 0 {
		in.WarningTTFTMs = def.WarningTTFTMs
	}
	if in.WarningTTFTMs < in.TargetTTFTMs {
		in.WarningTTFTMs = in.TargetTTFTMs + 1
	}
	if in.CriticalTTFTMs <= 0 {
		in.CriticalTTFTMs = def.CriticalTTFTMs
	}
	if in.CriticalTTFTMs < in.WarningTTFTMs {
		in.CriticalTTFTMs = in.WarningTTFTMs
	}
	if in.WarningCacheRate < 0 {
		in.WarningCacheRate = 0
	}
	if in.CriticalCacheRate < 0 {
		in.CriticalCacheRate = 0
	}
	if in.WarningCacheRate > 1 {
		in.WarningCacheRate = 1
	}
	if in.CriticalCacheRate > 1 {
		in.CriticalCacheRate = 1
	}
	if in.CriticalCacheRate > in.WarningCacheRate {
		in.CriticalCacheRate = in.WarningCacheRate
	}
	if in.ErrorWeight <= 0 && in.TTFTWeight <= 0 && in.CacheWeight <= 0 {
		in.ErrorWeight, in.TTFTWeight, in.CacheWeight = def.ErrorWeight, def.TTFTWeight, def.CacheWeight
	}
	return in
}

func normalizeChannelMonitorV2IgnoredCategories(values []string) []string {
	allowed := make(map[string]struct{}, len(ChannelMonitorV2ErrorCategories))
	for _, c := range ChannelMonitorV2ErrorCategories {
		allowed[c] = struct{}{}
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, ok := allowed[value]; !ok {
			// Unknown labels are dropped so a typo cannot silently create a new category.
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// ChannelMonitorV2IgnoredCategorySet returns a set for O(1) membership checks.
func ChannelMonitorV2IgnoredCategorySet(cfg ChannelMonitorV2Config) map[string]struct{} {
	set := make(map[string]struct{}, len(cfg.IgnoredErrorCategories))
	for _, c := range cfg.IgnoredErrorCategories {
		set[c] = struct{}{}
	}
	return set
}

func normalizeStringSet(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func normalizeInt64Set(values []int64) []int64 {
	seen := make(map[int64]struct{}, len(values))
	out := make([]int64, 0, len(values))
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func normalizeChannelMonitorV2GroupIDs(values []int64) ([]int64, error) {
	seen := make(map[int64]struct{}, len(values))
	out := make([]int64, 0, len(values))
	for _, value := range values {
		if value <= 0 {
			return nil, fmt.Errorf("%w: group_ids must be positive", ErrChannelMonitorV2InvalidConfig)
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func ChannelMonitorV2HealthFor(metrics ChannelMonitorV2Metric) ChannelMonitorV2Health {
	return ChannelMonitorV2HealthForWithThresholds(metrics, DefaultChannelMonitorV2HealthThresholds())
}

func ChannelMonitorV2HealthForWithThresholds(metrics ChannelMonitorV2Metric, thresholds ChannelMonitorV2HealthThresholds) ChannelMonitorV2Health {
	thresholds = NormalizeChannelMonitorV2HealthThresholds(thresholds)
	result := ChannelMonitorV2Health{
		Overall: "unknown", ErrorRate: "unknown", TTFT: "unknown", Cache: "unknown",
		MinimumSample: thresholds.MinimumSample, Thresholds: thresholds,
	}
	if metrics.RequestCount < result.MinimumSample {
		return result
	}

	type scored struct {
		score  float64
		weight float64
		band   string
	}
	parts := make([]scored, 0, 3)

	if metrics.RequestCount >= result.MinimumSample {
		s := errorRateScore(metrics.ErrorRate, thresholds.CriticalErrorRate)
		result.ErrorRateScore = &s
		result.ErrorRate = healthBand(metrics.ErrorRate, thresholds.WarningErrorRate, thresholds.CriticalErrorRate)
		parts = append(parts, scored{score: s, weight: thresholds.ErrorWeight, band: result.ErrorRate})
	}
	// Prefer p50 for TTFT scoring; fall back to p95 only if p50 is missing.
	if metrics.TTFT.SampleCount >= result.MinimumSample {
		var ttftMs *int64
		if metrics.TTFT.P50Ms != nil {
			ttftMs = metrics.TTFT.P50Ms
		} else if metrics.TTFT.P95Ms != nil {
			ttftMs = metrics.TTFT.P95Ms
		}
		if ttftMs != nil {
			s := ttftP50Score(float64(*ttftMs), float64(thresholds.TargetTTFTMs), float64(thresholds.CriticalTTFTMs))
			result.TTFTScore = &s
			result.TTFT = healthBand(float64(*ttftMs), float64(thresholds.WarningTTFTMs), float64(thresholds.CriticalTTFTMs))
			parts = append(parts, scored{score: s, weight: thresholds.TTFTWeight, band: result.TTFT})
		}
	}
	// Cache: need a meaningful denominator; higher rate is better.
	if metrics.CacheRateDenominator >= result.MinimumSample {
		s := cacheRateScore(metrics.CacheRate)
		if thresholds.WarningCacheRate <= 0 && thresholds.CriticalCacheRate <= 0 {
			// A zero/zero cache threshold means "do not penalize cache misses".
			s = 100
		}
		result.CacheScore = &s
		// Invert for healthBand (lower is worse): use (1 - rate) against warning/critical floors.
		result.Cache = cacheRateBand(metrics.CacheRate, thresholds.WarningCacheRate, thresholds.CriticalCacheRate)
		parts = append(parts, scored{score: s, weight: thresholds.CacheWeight, band: result.Cache})
	}

	if len(parts) == 0 {
		return result
	}
	var weightSum, scoreSum float64
	for _, p := range parts {
		weightSum += p.weight
		scoreSum += p.weight * p.score
	}
	if weightSum <= 0 {
		return result
	}
	overall := scoreSum / weightSum
	result.Score = &overall
	result.Overall = scoreBand(overall)
	return result
}

// errorRateScore maps error rate to 0–100. 0% → 100; at/above critical → 0 (linear).
func errorRateScore(errorRate, critical float64) float64 {
	if critical <= 0 {
		critical = 0.05
	}
	if errorRate <= 0 {
		return 100
	}
	if errorRate >= critical {
		return 0
	}
	return 100 * (1 - errorRate/critical)
}

// ttftP50Score maps TTFT p50 ms to 0–100.
// At/below target → 100; at/above critical → 0; linear in between.
func ttftP50Score(p50Ms, targetMs, criticalMs float64) float64 {
	if targetMs <= 0 {
		targetMs = 2500
	}
	if criticalMs <= targetMs {
		criticalMs = targetMs * 2.4
	}
	if p50Ms <= targetMs {
		return 100
	}
	if p50Ms >= criticalMs {
		return 0
	}
	return 100 * (1 - (p50Ms-targetMs)/(criticalMs-targetMs))
}

// cacheRateScore maps cache hit rate to 0–100 (higher is better, linear).
func cacheRateScore(cacheRate float64) float64 {
	if cacheRate <= 0 {
		return 0
	}
	if cacheRate >= 1 {
		return 100
	}
	return 100 * cacheRate
}

// cacheRateBand: below critical → critical; below warning → warning; else healthy.
func cacheRateBand(cacheRate, warning, critical float64) string {
	if cacheRate < critical {
		return "critical"
	}
	if cacheRate < warning {
		return "warning"
	}
	return "healthy"
}

// scoreBand maps continuous 0–100 scores to coarse labels for legacy consumers.
func scoreBand(score float64) string {
	switch {
	case score >= 80:
		return "healthy"
	case score >= 50:
		return "warning"
	default:
		return "critical"
	}
}

func healthBand(value, warning, critical float64) string {
	if value >= critical {
		return "critical"
	}
	if value >= warning {
		return "warning"
	}
	return "healthy"
}
