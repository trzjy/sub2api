package service

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/paymentproviderinstance"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/Wei-Shaw/sub2api/internal/payment/provider"
)

// --- Order Status Constants ---

const (
	OrderStatusPending           = payment.OrderStatusPending
	OrderStatusPaid              = payment.OrderStatusPaid
	OrderStatusRecharging        = payment.OrderStatusRecharging
	OrderStatusCompleted         = payment.OrderStatusCompleted
	OrderStatusExpired           = payment.OrderStatusExpired
	OrderStatusCancelled         = payment.OrderStatusCancelled
	OrderStatusFailed            = payment.OrderStatusFailed
	OrderStatusRefundRequested   = payment.OrderStatusRefundRequested
	OrderStatusRefunding         = payment.OrderStatusRefunding
	OrderStatusRefundPending     = payment.OrderStatusRefundPending
	OrderStatusPartiallyRefunded = payment.OrderStatusPartiallyRefunded
	OrderStatusRefunded          = payment.OrderStatusRefunded
	OrderStatusRefundFailed      = payment.OrderStatusRefundFailed
)

const (
	// defaultMaxPendingOrders and defaultOrderTimeoutMin are defined in
	// payment_config_service.go alongside other payment configuration defaults.
	paymentGraceMinutes = 5

	defaultPageSize    = 20
	maxPageSize        = 100
	topUsersLimit      = 10
	amountToleranceCNY = 0.01

	orderIDPrefix = "sub2_"
)

const paymentResumeSigningKeyEnv = "PAYMENT_RESUME_SIGNING_KEY"

// --- Types ---

// generateOutTradeNo creates a unique external order ID for payment providers.
// Format: sub2_20250409aB3kX9mQ (prefix + date + 8-char random)
func generateOutTradeNo() string {
	date := time.Now().Format("20060102")
	rnd := generateRandomString(8)
	return orderIDPrefix + date + rnd
}

func generateRandomString(n int) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = charset[rand.IntN(len(charset))]
	}
	return string(b)
}

type CreateOrderRequest struct {
	UserID          int64
	Amount          float64
	PaymentType     string
	OpenID          string
	ClientIP        string
	IsMobile        bool
	IsWeChatBrowser bool
	SrcHost         string
	SrcURL          string
	ReturnURL       string
	PaymentSource   string
	OrderType       string
	PlanID          int64
	Locale          string
}

type CreateOrderResponse struct {
	OrderID                       int64                           `json:"order_id"`
	Amount                        float64                         `json:"amount"`
	PayAmount                     float64                         `json:"pay_amount"`
	FeeRate                       float64                         `json:"fee_rate"`
	Status                        string                          `json:"status"`
	ResultType                    payment.CreatePaymentResultType `json:"result_type,omitempty"`
	PaymentType                   string                          `json:"payment_type"`
	OutTradeNo                    string                          `json:"out_trade_no,omitempty"`
	PayURL                        string                          `json:"pay_url,omitempty"`
	QRCode                        string                          `json:"qr_code,omitempty"`
	ClientSecret                  string                          `json:"client_secret,omitempty"`
	IntentID                      string                          `json:"intent_id,omitempty"`
	Currency                      string                          `json:"currency,omitempty"`
	CountryCode                   string                          `json:"country_code,omitempty"`
	PaymentEnv                    string                          `json:"payment_env,omitempty"`
	OAuth                         *payment.WechatOAuthInfo        `json:"oauth,omitempty"`
	JSAPI                         *payment.WechatJSAPIPayload     `json:"jsapi,omitempty"`
	JSAPIPayload                  *payment.WechatJSAPIPayload     `json:"jsapi_payload,omitempty"`
	ExpiresAt                     time.Time                       `json:"expires_at"`
	PaymentMode                   string                          `json:"payment_mode,omitempty"`
	ResumeToken                   string                          `json:"resume_token,omitempty"`
	AlipayMobilePrecreateDeepLink bool                            `json:"alipay_mobile_precreate_deep_link,omitempty"`
}

type OrderListParams struct {
	Page        int
	PageSize    int
	Status      string
	OrderType   string
	PaymentType string
	Keyword     string
}

type RefundPlan struct {
	OrderID         int64
	Order           *dbent.PaymentOrder
	RefundAmount    float64
	GatewayAmount   float64
	Reason          string
	Force           bool
	DeductBalance   bool
	DeductionType   string
	BalanceToDeduct float64
	SubDaysToDeduct int
	SubscriptionID  int64
}

type RefundResult struct {
	Success         bool    `json:"success"`
	Warning         string  `json:"warning,omitempty"`
	RequireForce    bool    `json:"require_force,omitempty"`
	BalanceDeducted float64 `json:"balance_deducted,omitempty"`
	SubDaysDeducted int     `json:"subscription_days_deducted,omitempty"`
}

type DashboardStats struct {
	TodayAmount   CurrencyAmounts `json:"today_amount"`
	TotalAmount   CurrencyAmounts `json:"total_amount"`
	TodayCount    int             `json:"today_count"`
	TotalCount    int             `json:"total_count"`
	AvgAmount     CurrencyAmounts `json:"avg_amount"`
	PendingOrders int             `json:"pending_orders"`

	DailySeries    []DailyStats        `json:"daily_series"`
	PaymentMethods []PaymentMethodStat `json:"payment_methods"`
	TopUsers       TopUsersByCurrency  `json:"top_users"`
}

// CurrencyAmounts holds payment amounts keyed by their ISO 4217 currency.
// Amounts in different currencies must never be added together.
type CurrencyAmounts map[string]float64

type DailyStats struct {
	Date   string          `json:"date"`
	Amount CurrencyAmounts `json:"amount"`
	Count  int             `json:"count"`
}

type PaymentMethodStat struct {
	Type   string          `json:"type"`
	Amount CurrencyAmounts `json:"amount"`
	Count  int             `json:"count"`
}

type TopUserStat struct {
	UserID int64   `json:"user_id"`
	Email  string  `json:"email"`
	Amount float64 `json:"amount"`
}

// TopUsersByCurrency contains an independent ranked user list for each
// currency. A single cross-currency leaderboard would be misleading.
type TopUsersByCurrency map[string][]TopUserStat

// --- Service ---

type PaymentService struct {
	providerMu               sync.Mutex
	providersLoaded          bool
	entClient                *dbent.Client
	registry                 *payment.Registry
	loadBalancer             payment.LoadBalancer
	redeemService            *RedeemService
	subscriptionSvc          *SubscriptionService
	configService            *PaymentConfigService
	userRepo                 UserRepository
	groupRepo                GroupRepository
	resumeService            *PaymentResumeService
	affiliateService         *AffiliateService
	notificationEmailService *NotificationEmailService
}

func NewPaymentService(entClient *dbent.Client, registry *payment.Registry, loadBalancer payment.LoadBalancer, redeemService *RedeemService, subscriptionSvc *SubscriptionService, configService *PaymentConfigService, userRepo UserRepository, groupRepo GroupRepository, affiliateService *AffiliateService) *PaymentService {
	svc := &PaymentService{entClient: entClient, registry: registry, loadBalancer: newVisibleMethodLoadBalancer(loadBalancer, configService), redeemService: redeemService, subscriptionSvc: subscriptionSvc, configService: configService, userRepo: userRepo, groupRepo: groupRepo, affiliateService: affiliateService}
	svc.resumeService = psNewPaymentResumeService(configService)
	return svc
}

func (s *PaymentService) SetNotificationEmailService(notificationEmailService *NotificationEmailService) {
	s.notificationEmailService = notificationEmailService
}

// --- Provider Registry ---

// EnsureProviders lazily initializes the provider registry on first call.
func (s *PaymentService) EnsureProviders(ctx context.Context) {
	s.providerMu.Lock()
	defer s.providerMu.Unlock()
	if !s.providersLoaded {
		s.loadProviders(ctx)
		s.providersLoaded = true
	}
}

// RefreshProviders clears and re-registers all providers from the database.
func (s *PaymentService) RefreshProviders(ctx context.Context) {
	s.providerMu.Lock()
	defer s.providerMu.Unlock()
	s.registry.Clear()
	s.loadProviders(ctx)
	s.providersLoaded = true
}

func (s *PaymentService) loadProviders(ctx context.Context) {
	instances, err := s.entClient.PaymentProviderInstance.Query().
		Where(paymentproviderinstance.EnabledEQ(true)).
		All(ctx)
	if err != nil {
		slog.Error("[PaymentService] failed to query provider instances", "error", err)
		return
	}
	for _, inst := range instances {
		cfg, err := s.loadBalancer.GetInstanceConfig(ctx, int64(inst.ID))
		if err != nil {
			slog.Warn("[PaymentService] failed to decrypt config for instance", "instanceID", inst.ID, "error", err)
			continue
		}
		if inst.PaymentMode != "" {
			cfg["paymentMode"] = inst.PaymentMode
		}
		instID := fmt.Sprintf("%d", inst.ID)
		p, err := provider.CreateProvider(inst.ProviderKey, instID, cfg)
		if err != nil {
			slog.Warn("[PaymentService] failed to create provider for instance", "instanceID", inst.ID, "key", inst.ProviderKey, "error", err)
			continue
		}
		s.registry.Register(p)
	}
}

// --- Helpers ---

func psIsRefundStatus(s string) bool {
	switch s {
	case OrderStatusRefundRequested, OrderStatusRefunding, OrderStatusRefundPending, OrderStatusPartiallyRefunded, OrderStatusRefunded, OrderStatusRefundFailed:
		return true
	}
	return false
}

func psErrMsg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func psNilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *PaymentService) paymentResume() *PaymentResumeService {
	if s.resumeService != nil {
		return s.resumeService
	}
	return psNewPaymentResumeService(s.configService)
}

func NewLegacyAwarePaymentResumeService(legacyKey []byte) *PaymentResumeService {
	return newLegacyAwarePaymentResumeService(legacyKey)
}

func psNewPaymentResumeService(configService *PaymentConfigService) *PaymentResumeService {
	return newLegacyAwarePaymentResumeService(psResumeLegacyVerificationKey(configService))
}

func newLegacyAwarePaymentResumeService(legacyKey []byte) *PaymentResumeService {
	signingKey, verifyFallbacks := resolvePaymentResumeSigningKeys(legacyKey)
	return NewPaymentResumeService(signingKey, verifyFallbacks...)
}

func psResumeLegacyVerificationKey(configService *PaymentConfigService) []byte {
	if configService == nil {
		return nil
	}
	return configService.encryptionKey
}

func resolvePaymentResumeSigningKeys(legacyKey []byte) ([]byte, [][]byte) {
	signingKey := parsePaymentResumeSigningKey(os.Getenv(paymentResumeSigningKeyEnv))
	if len(signingKey) == 0 {
		if len(legacyKey) == 0 {
			return nil, nil
		}
		return legacyKey, nil
	}
	if len(legacyKey) == 0 || bytes.Equal(legacyKey, signingKey) {
		return signingKey, nil
	}
	return signingKey, [][]byte{legacyKey}
}

func parsePaymentResumeSigningKey(raw string) []byte {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if len(raw) >= 64 && len(raw)%2 == 0 {
		if decoded, err := hex.DecodeString(raw); err == nil && len(decoded) > 0 {
			return decoded
		}
	}
	return []byte(raw)
}

func psSliceContains(sl []string, s string) bool {
	for _, v := range sl {
		if v == s {
			return true
		}
	}
	return false
}

// Subscription validity period unit constants.
const (
	validityUnitWeek   = "week"
	validityUnitWeeks  = "weeks"
	validityUnitMonth  = "month"
	validityUnitMonths = "months"
)

func psComputeValidityDays(days int, unit string) int {
	switch unit {
	case validityUnitWeek, validityUnitWeeks:
		return days * 7
	case validityUnitMonth, validityUnitMonths:
		return days * 30
	default:
		return days
	}
}

// PSComputeValidityDays 是 psComputeValidityDays 的导出包装，供货源下单现场生成兑换码时
// 复用同一套「周×7 / 月×30 / 原值」换算逻辑，避免第二份换算实现（D6F-A）。
func PSComputeValidityDays(days int, unit string) int {
	return psComputeValidityDays(days, unit)
}

// queryRowContext 是「能执行参数化单行查询」的最小接口。*sql.DB 与 *sql.Tx 均天然
// 满足（都拥有 QueryRowContext(context.Context, string, ...any) *sql.Row），借此把
// 套餐有效期查询体抽为单一实现，使 DB 版与 Tx 版共用同一段 SQL/换算逻辑（D6F-R 修复#1）。
type queryRowContext interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// psResolveGroupValidityDays 是套餐有效期解析的共享实现。经 q（*sql.DB 或 *sql.Tx）查询
// 该分组下排序最靠前的套餐（subscription_plans WHERE group_id=? ORDER BY sort_order, id
// LIMIT 1）的 validity_days/validity_unit，再经 PSComputeValidityDays 换算。无套餐分组 →
// 内部错误（fail-closed，货源下单须绑定真实套餐）。此为唯一 SQL/换算来源，禁止第二份。
func psResolveGroupValidityDays(ctx context.Context, q queryRowContext, groupID int64) (int, error) {
	var days int
	var unit string
	err := q.QueryRowContext(ctx, `
		SELECT validity_days, validity_unit
		FROM subscription_plans
		WHERE group_id = $1
		ORDER BY sort_order, id
		LIMIT 1`, groupID).Scan(&days, &unit)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("xianguanjia supply: no subscription plan for group %d", groupID)
		}
		return 0, fmt.Errorf("xianguanjia supply query subscription plan for group %d: %w", groupID, err)
	}
	return PSComputeValidityDays(days, unit), nil
}

// PSResolveGroupValidityDays 返回套餐分组兑换码的有效天数（DB 版入口）。逻辑完全委托
// psResolveGroupValidityDays 共享体；签名与既有调用方（非事务场景）保持不变。
func PSResolveGroupValidityDays(ctx context.Context, db *sql.DB, groupID int64) (int, error) {
	return psResolveGroupValidityDays(ctx, db, groupID)
}

// PSResolveGroupValidityDaysTx 是 PSResolveGroupValidityDays 的事务版入口：同一共享体，
// 但查询经 *sql.Tx 执行，使套餐读取与 redeem_codes 插入共享同一事务快照（D6F-R 修复#1）。
// *sql.Tx 满足 queryRowContext 接口，无需第二份 SQL/换算逻辑。
func PSResolveGroupValidityDaysTx(ctx context.Context, tx *sql.Tx, groupID int64) (int, error) {
	return psResolveGroupValidityDays(ctx, tx, groupID)
}

func psStartOfDayUTC(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func applyPagination(pageSize, page int) (size, pg int) {
	size = pageSize
	if size <= 0 {
		size = defaultPageSize
	}
	if size > maxPageSize {
		size = maxPageSize
	}
	pg = page
	if pg < 1 {
		pg = 1
	}
	return size, pg
}
