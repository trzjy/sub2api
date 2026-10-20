package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/ent/quotakey"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/pkg/credcrypt"
)

// MaxAccountsPerQuotaKey 是单个激活码可持有（working）账号的上限；达到后停止拉号。
const MaxAccountsPerQuotaKey = 15

// quotaKeyCooldownWindow 是 shareai 上游实测的刷新冷却窗口（方案 §10 #26）。带冷却标记的 key
// 在 last_refresh_at 距今不足该窗口内不进入补池候选；窗口期满后自动重新入选，无需人工 clear。
// 写成包内常量并注明出处，不引入可配置项（裁定 3）。
const quotaKeyCooldownWindow = 5 * time.Minute

// QuotaKeyRepository 是 QuotaKeyService 依赖的存储契约。
//
// 仅包含与 accounts 表无关的 quota_keys 操作（C3.1 骨架范围），以及 C3.2 扩展的
// 账号软删编排与刷新扫描能力。实现位于 internal/repository（repository 包引用本
// service 包的类型，故接口定义在此处以避免 service -> repository 的反向依赖）。
type QuotaKeyRepository interface {
	GetByID(ctx context.Context, quotaKeyID int64) (*ent.QuotaKey, error)
	Create(ctx context.Context, input CreateQuotaKeyInput) (*ent.QuotaKey, error)
	UpdateStatusFields(ctx context.Context, tx *ent.Tx, id int64, status string, exhausted bool, lastError *string, lastRefreshAt *time.Time) error
	HardDelete(ctx context.Context, tx *ent.Tx, id int64) error
	UpdateCredentials(ctx context.Context, id int64, codeEncrypted, codeMAC, codeLast4 string) error
	ListExpired(ctx context.Context, now time.Time) ([]*ent.QuotaKey, error)
	ListByStatus(ctx context.Context, status string) ([]*ent.QuotaKey, error)
	// C5a 扩展（D-CURSOR-020）：admin CRUD 读/写支撑方法。
	List(ctx context.Context) ([]*ent.QuotaKey, error)
	UpdateMeta(ctx context.Context, id int64, expiresAt **time.Time, notes **string) error
	ListAccountsByQuotaKey(ctx context.Context, quotaKeyID int64) ([]*ent.Account, error)
	// ListAccountsByQuotaKeys 批量返回归属给定激活码的账号（一次 QuotaKeyIDIn 查询），
	// 供 List 端点按 quota_key_id 分组聚合计数，避免逐激活码查询退化为 N+1。
	// SoftDeleteMixin 已自动过滤软删账号；仅读取，不改写 accounts。
	ListAccountsByQuotaKeys(ctx context.Context, ids []int64) ([]*ent.Account, error)
	// C3.2 扩展：账号软删编排与刷新扫描（均在调用方已开启的事务内执行，除 ListEligibleForRefresh）。
	// 注：working 计数（§3.2 谓词）已收敛到集中强制入口内的 countWorkingQuotaKeyAccounts
	// （D-CURSOR-034），不再是本契约的方法——计数口径只此一处，写入路径无处可自带谓词。
	SoftDeleteAccountsByQuotaKey(ctx context.Context, tx *ent.Tx, quotaKeyID int64) ([]int64, error)
	EnqueueAccountChangedOutbox(ctx context.Context, tx *ent.Tx, accountIDs []int64) error
	SoftDelete(ctx context.Context, tx *ent.Tx, id int64) error
	ListEligibleForRefresh(ctx context.Context) ([]*ent.QuotaKey, error)
	// MarkExpiredIfStillExpired 在事务内将激活码标记为 expired，但仅当数据库当前行
	// 仍满足"当前已到期"条件：status='active' AND expires_at IS NOT NULL AND expires_at < now。
	// 这是幂等、按当前值判定的条件化写入：即使 ListExpired 取回的是旧值、管理员在两步间
	// 用 UpdateMeta 清空 expires_at，本语句按 DB 当前条件重新判定，不会将"曾经到期但已
	// 被清空"的 key 误标 expired（D-CURSOR-032）。返回受影响行数（0 或 1）。
	MarkExpiredIfStillExpired(ctx context.Context, tx *ent.Tx, id int64, now time.Time) (int, error)
	// SetDeviceIDIfAbsent 在 device_id 当前为 NULL 或空串时写入给定值，返回受影响行数
	// （命中 1，已被并发写入则 0）。这是 B-10 首次并发的防重闸门：把"是否已有值"的
	// 判定下沉到 UPDATE 的 WHERE 条件，由 DB 原子裁决，多个并发首次请求中只有一个
	// 能写入，其余拿到 0 行后重读赢家值，避免同一激活码分裂出两份 deviceId。
	// 判定口径（NULL 或空串）须与 service 侧 DeviceIDFor 的读判定逐字一致。
	SetDeviceIDIfAbsent(ctx context.Context, id int64, deviceID string) (int, error)
}

// QuotaKeyUpstreamClient 抽象"向上游拉取账号"的客户端。
//
// 合同 §5 与 D-CURSOR-014 硬口径（2026-10-13 用户裁定，锁定，不得改）：
// 占位实现必须是"失败"占位——任意错误返回 exhausted=true 分支所需的信息，
// 绝不允许返回假账号 / 固定成功（上一轮 D-CURSOR-009 就因此作废）。
// 生产默认占位见 NotConnectedUpstreamClient。
type QuotaKeyUpstreamClient interface {
	// PullBatch 尝试从上游拉取一批账号。
	// 成功返回待入库账号列表；任意错误必须返回（不得返回假账号）。
	PullBatch(ctx context.Context, qk *ent.QuotaKey) (*UpstreamPullResult, error)
}

// UpstreamAccount 是上游返回的一条待入库账号的规格。
// 不含 id / quota_key_id（由 service 在插入时写入归属）。
type UpstreamAccount struct {
	Name        string
	Platform    string
	Type        string
	Credentials map[string]any
	Extra       map[string]any
}

// UpstreamPullResult 是上游一次拉号返回的批量结果。
type UpstreamPullResult struct {
	Accounts []*UpstreamAccount
	// ExpiresAt 是本次响应 key_info.expired_at 解析出的【激活码】到期时间，
	// 由拉号成功路径写入 quota_keys.expires_at（sweeper 的判定依据）。
	// nil = 上游本次未给出可用到期时间；调用方必须保持 expires_at 原值不动，
	// 不得据未知推算补日期（见 B-11）。
	ExpiresAt *time.Time
}

// ErrUpstreamNotConnected 是生产默认占位客户端返回的"未接入上游"错误。
var ErrUpstreamNotConnected = errors.New("quota key upstream not connected")

// 注：上游冷却哨兵 ErrUpstreamCooldown 定义于 cursor_upstream_client.go（§13.2 F-b / R4 消化）：
// 上游返回冷却时，PullAccounts 在落库事务内写 last_refresh_error + last_refresh_at（唯独豁免
// exhausted），并以该哨兵（errors.Is 可识别）返回触发方，使其执行"冷却即停"。

// ErrQuotaKeyModelNoCoverage 表示触发补拉所携带的模型 M 无任何分组白名单覆盖：
// 失败关闭（fail-closed），不静默落成裸号（§13.3）。触发方据此记录告警。
var ErrQuotaKeyModelNoCoverage = errors.New("quota key model M has no covering group")

// ErrQuotaKeyMountFailed 表示上游成功、但本地挂载/落库失败（shareai 非幂等，上游发号
// 不可回滚）：本地事务回滚（号不入库），触发方据此记录"上游可能已被消耗一次"告警（§13.3 F-c）。
var ErrQuotaKeyMountFailed = errors.New("quota key account mount failed after upstream success")

// UpstreamPullError 是 pull 对上游失败/终态结果的结构化归类错误（R1-1）。
//
// 它保留内部分类哨兵（errors.Is 命中 ErrUpstreamCooldown / ErrUpstreamKeyExpired），
// 并携带上游中文 msg 原文（Message），供 handler 在手动链路把它翻译成带原文的
// QUOTA_KEY_UPSTREAM 失败响应（不得用英文前缀/映射，不得显示成功）。
//
// 自动链路（reconciler）消费端仅以"本轮失败"记日志用（PullAccounts/PullAccountsForModel
// 对未知结果经 writeUpstreamFailureMarker 落 last_refresh_error 后透传 ErrUpstreamKeyBlocked，
// exhausted 维度不动）；手动链路的可见性由 RefreshAccounts 负责上抛此错误，handler 负责回显原文。
type UpstreamPullError struct {
	// Kind 是内部归类（冷却 / 过期耗尽 / 未知），供 errors.Is 与 handler 区分。
	Kind UpstreamResultKind
	// message 是上游中文 msg 原文（或明确失败描述），handler 须逐字回显到响应 message。
	message string
	// cause 是底层错误链（包分类哨兵），Error() 委托给它以保留 DB last_refresh_error
	// 的形态（以 ErrUpstreamCooldown/ErrUpstreamKeyExpired 前缀开头，供 isCooldownRefreshError 识别）。
	cause error
}

// UpstreamResultKind 是上游失败结果的内部归类。
type UpstreamResultKind int

const (
	// UpstreamCooldown 限频类（5min 冷却窗口）。
	UpstreamCooldown UpstreamResultKind = iota
	// UpstreamExpired 过期/耗尽终态（标 expired）。
	UpstreamExpired
	// UpstreamUnknown 未知结果（失败关闭，不得判冷却/过期）。
	UpstreamUnknown
)

// Message 返回上游中文 msg 原文（或明确失败描述），handler 须逐字回显到响应 message。
func (e *UpstreamPullError) Message() string { return e.message }

// Error 委托给 cause，以保留"以分类哨兵前缀开头"的形态（DB last_refresh_error 与
// isCooldownRefreshError 依赖该前缀）；caller 取干净原文用 Message()。
func (e *UpstreamPullError) Error() string {
	if e.cause != nil {
		return e.cause.Error()
	}
	return e.message
}

// Unwrap 暴露 cause，使 errors.Is(err, ErrUpstreamCooldown/...) 等链路判定可用。
func (e *UpstreamPullError) Unwrap() error { return e.cause }

// Is 使 UpstreamPullError 能被 errors.Is 识别为对应的内部分类哨兵。
func (e *UpstreamPullError) Is(target error) bool {
	switch e.Kind {
	case UpstreamCooldown:
		return errors.Is(target, ErrUpstreamCooldown)
	case UpstreamExpired:
		return errors.Is(target, ErrUpstreamKeyExpired)
	}
	return false
}

// NewUpstreamCooldownError / NewUpstreamExpiredError / NewUpstreamUnknownError 是分类错误的
// 构造入口，message 即上游中文 msg 原文（或明确失败描述）。导出供测试与 classifyUpstreamError 使用。
func NewUpstreamCooldownError(message string) error {
	return &UpstreamPullError{Kind: UpstreamCooldown, message: message, cause: fmt.Errorf("%w: %s", ErrUpstreamCooldown, message)}
}

func NewUpstreamExpiredError(message string) error {
	return &UpstreamPullError{Kind: UpstreamExpired, message: message, cause: fmt.Errorf("%w: %s", ErrUpstreamKeyExpired, message)}
}

func NewUpstreamUnknownError(message string) error {
	return &UpstreamPullError{Kind: UpstreamUnknown, message: message, cause: fmt.Errorf("shareai: upstream code=1 unknown result: %s", message)}
}

// newUpstreamUnknownErrorFrom 把任意上游错误（可能是已归类的 UpstreamPullError，也可能是
// 传输/默认业务码错误）归一为 kind=unknown 的 UpstreamPullError，供手动链路在未知分支上抛。
// 若已是 UpstreamPullError 则原样返回（保留其 Kind/message）。
func newUpstreamUnknownErrorFrom(err error) *UpstreamPullError {
	if upe, ok := err.(*UpstreamPullError); ok {
		return upe
	}
	return &UpstreamPullError{Kind: UpstreamUnknown, message: err.Error(), cause: err}
}

// errInsertFailureRollback 是 pull 内部在【多账号部分插入成功之后】遇到某账号落库失败
// 时返回的私有哨兵：它仅用于信号化"请回滚当前容量事务、并在事后用独立事务补写标记"，
// 不直接上抛给触发方（真正上抛的是随后由 writeInsertFailureMarker + ErrQuotaKeyMountFailed
// 重新构造的错误）。之所以需要这一私有哨兵，是因为插入失败分支必须做到：
//   - 当前容量事务整体回滚（已插入的账号不能提交成裸号 —— R5-1 核心）；
//   - 但 last_refresh_error 标记仍要持久化（跨轮退出候选 —— R5-1 a）。
//
// 两者无法在同一事务内同时成立（回滚会连标记一起丢弃），故采用"先回滚当前事务 → 再用
// 独立事务写标记"的两段编排（实现方向取自方案 SSOT v3.5 §13.3，零新表/字段）。
var errInsertFailureRollback = errors.New("quota key pull: rollback partial inserts on account insert failure")

// NotConnectedUpstreamClient 是生产默认的占位客户端：未接入上游时返回明确错误，
// 由 PullAccounts 走 exhausted 分支。绝不返回假账号 / 固定成功。
type NotConnectedUpstreamClient struct{}

// PullBatch 返回"未接入上游"错误，触发 PullAccounts 的 exhausted 占位分支。
func (NotConnectedUpstreamClient) PullBatch(_ context.Context, _ *ent.QuotaKey) (*UpstreamPullResult, error) {
	return nil, ErrUpstreamNotConnected
}

// ErrQuotaKeyNotFound 表示激活码不存在（或已软删，普通查询不可见）。
var ErrQuotaKeyNotFound = errors.New("quota key not found")

// QuotaKeyService 提供激活码凭据加密落库/读取 + C3.2 拉号状态机 + 软删编排。
type QuotaKeyService struct {
	repo     QuotaKeyRepository
	enc      QuotaKeyEncryptor
	upstream QuotaKeyUpstreamClient
	client   *ent.Client
	// groupResolver 解析"白名单含模型 M"的全部分组 ID（§13.3 挂载事实源）。
	// 生产默认走 findGroupsAllowingModel；测试可替换以注入挂载失败等边界。
	groupResolver func(ctx context.Context, tx *ent.Tx, modelM string) ([]int64, error)

	// cursorGroupResolver 解析 B1 手动挂载目标分组 ID（B1 事实源）。用于手动入口
	// RefreshAccounts：手动刷新不携带模型上下文，挂载目标 = 【配置勾选模型集合】∩【分组
	// 白名单覆盖】的交集（findCursorPlatformGroups 裁定 1 口径），与自动链路 ListAllowedModels
	// 同源同判。勾选空 → ErrQuotaKeyModelNoCoverage 失败关闭。
	// 生产默认走 findCursorPlatformGroups；测试可替换以注入边界。
	// 注意：该解析器不计模型，与 groupResolver(modelM) 是两个独立的 seam。
	cursorGroupResolver func(ctx context.Context, tx *ent.Tx) ([]int64, error)

	// insertFailureMarkerWriter 是 R5-1 插入失败后"独立事务补写 last_refresh_error 标记"
	// 的可替换执行点（与生产默认 writeInsertFailureMarker 同签名）。测试可替换以注入
	// 标记写失败（R5-1 b：仅本轮哨兵停轮的边界），与生产 groupResolver 同一类测试 seams。
	insertFailureMarkerWriter func(ctx context.Context, quotaKeyID int64, cause error) error

	// reconciler 是软删后水位快路径唤醒目标（C12-v2）。通过 SetReconciler 注入（wire 装配），
	// 软删事务提交成功后调用其 NotifyAccountRemoved 触发 reconciler 重判水位；nil 时跳过
	// （未装配则不触发快路径，退回 C10 周期补池）。
	reconciler quotaKeyReconcilerNotifier

	// autoEntry 是 D2-a 入库可配置提供方（窄接口）：挂组前调用 ApplyEntryDefaults 写账号属性，
	// 并对外提供配置读取。nil 时跳过（向后兼容 D1 既有测试与未装配场景）。
	autoEntry AutoEntryConfigProvider
}

// AutoEntryConfigProvider 是 D1 挂载点调用的窄接口（D2-a 注入）：提供配置并应用入库默认值。
// 由 *CursorAutoEntryService 实现；QuotaKeyService 仅依赖此窄面，避免反向依赖。
type AutoEntryConfigProvider interface {
	// GetConfig 读取当前 cursor 入库可配置（settings 缺省时返回缺省配置）。
	GetConfig(ctx context.Context) (*CursorAutoEntryConfig, error)
	// ApplyEntryDefaults 把配置中的入库默认值与有效模型集合写入指定账号（列 + extra）。
	// tx 非 nil 时必须在同一挂载事务内写入（account 尚未提交，复用 tx 才能写成功）。
	ApplyEntryDefaults(ctx context.Context, tx *ent.Tx, account *ent.Account, cfg *CursorAutoEntryConfig) (*ent.Account, error)
}

// quotaKeyReconcilerNotifier 是软删后水位快路径唤醒的窄接口（C12-v2）。
// 仅暴露 NotifyAccountRemoved：软删事务提交成功后由删除路径调用，触发 reconciler 重判水位
// （只置 pending，不直接拉号）。生产传 *QuotaKeyReconciler，测试可替身（合同 §5：替身仅存于测试）。
type quotaKeyReconcilerNotifier interface {
	NotifyAccountRemoved()
}

// SetReconciler 注入软删水位快路径唤醒目标（C12-v2）。生产由 wire 在 ProvideQuotaKeyReconciler
// 装配点一行注入 *QuotaKeyReconciler；测试可注入替身。nil 传入时清空引用（不触发快路径）。
func (s *QuotaKeyService) SetReconciler(r quotaKeyReconcilerNotifier) {
	if s == nil {
		return
	}
	s.reconciler = r
}

// SetAutoEntry 注入 D2-a 入库可配置提供方（窄接口）。生产由 wire 在装配点注入
// *CursorAutoEntryService；测试可注入替身或留 nil（nil 时挂载点不写账号属性，向后兼容 D1）。
// 此处仅依赖 AutoEntryConfigProvider 窄面，避免 QuotaKeyService 反向依赖具体实现。
func (s *QuotaKeyService) SetAutoEntry(a AutoEntryConfigProvider) {
	if s == nil {
		return
	}
	s.autoEntry = a
}

// NewQuotaKeyService 构造激活码服务。
// enc 为 nil 时 panic：加密器缺失禁止静默回落明文（合同 §5）。
// upstream 为 nil 时默认使用 NotConnectedUpstreamClient（失败占位）。
// client 为 ent 客户端，用于 advisory 事务锁与账号/激活码的事务编排。
func NewQuotaKeyService(repo QuotaKeyRepository, enc QuotaKeyEncryptor, upstream QuotaKeyUpstreamClient, client *ent.Client) *QuotaKeyService {
	if enc == nil {
		panic("quota_key_service: encryptor must not be nil")
	}
	if upstream == nil {
		upstream = NotConnectedUpstreamClient{}
	}
	s := &QuotaKeyService{repo: repo, enc: enc, upstream: upstream, client: client}
	s.groupResolver = s.findGroupsAllowingModel
	s.cursorGroupResolver = s.findCursorPlatformGroups
	s.insertFailureMarkerWriter = s.writeInsertFailureMarker
	// P1-B4 设备同源闭环：真实拉号客户端需要反查本服务才能分流"存量派生值 / DeviceIDFor"，
	// 构造顺序天然成环（客户端先建 → 服务后建），故在此回填本服务自身作为设备解析器
	// （*QuotaKeyService 同时提供 DeviceIDFor 与 HasAccount）。
	// 这样 wire / 测试无须新增装配语句，也不必改动构造函数签名；
	// 传入的是占位或替身上游（不满足类型断言）时整段跳过，行为不变。
	if sac, ok := upstream.(*shareAIUpstreamClient); ok {
		sac.SetDeviceResolver(s)
	}
	// P1-S：cursor 调度资格门所需的激活码状态读取器（包级安装，沿
	// codeBuddyQuotaInstance 的模式）。调度谓词是包级纯函数、拿不到 service 句柄，
	// 故在此装配点一次性注入；未装配时资格门降级为不收紧（见
	// cursorAccountQuotaKeyVetoReason 注释）。
	InstallCursorQuotaKeyStateProvider(s)
	return s
}

// GetStatus 读取激活码状态并脱敏（明文 code 绝不出现在返回 DTO 中）。
func (s *QuotaKeyService) GetStatus(ctx context.Context, id int64) (*QuotaKeyStatus, error) {
	qk, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return quotaKeyToStatus(qk), nil
}

// CursorQuotaKeyStatus 返回激活码在调度口径下的状态，供 cursor 账号的调度资格门消费
// （P1-S §15.4-I2：key 终态只约束账号的调度资格，账号本体不动）。
//
// 归一化：「落盘 status=expired」与「expires_at 已到期但 sweeper 尚未落盘」是同一事实的
// 两个时序切片，对调度而言没有区别，统一归一为 domain.QuotaKeyStatusExpired——
// 资格门只关心"这个码是否已停供"，不该被后台落盘时序左右。
// 其余状态（active/exhausted/disabled…）原样返回，由资格门按自身语义裁决。
//
// 读取失败（码不存在 / 存储故障）返回 error：调用方据此失败关闭，不静默放行。
func (s *QuotaKeyService) CursorQuotaKeyStatus(ctx context.Context, quotaKeyID int64) (string, error) {
	if s == nil || s.repo == nil {
		return "", errors.New("quota_key_service: quota key repository unavailable")
	}
	if quotaKeyID <= 0 {
		return "", fmt.Errorf("quota_key_service: invalid quota key id %d", quotaKeyID)
	}
	qk, err := s.repo.GetByID(ctx, quotaKeyID)
	if err != nil {
		return "", err
	}
	if qk == nil {
		return "", fmt.Errorf("quota_key_service: quota key %d not found", quotaKeyID)
	}
	if qk.Status == domain.QuotaKeyStatusExpired || isExpired(qk) {
		return domain.QuotaKeyStatusExpired, nil
	}
	return qk.Status, nil
}

// RotateCredentials 加密新激活码原文后写入 code_encrypted / code_mac / code_last4，
// 返回脱敏后的状态。
//
// 不触碰 accounts 表、不发起任何上游请求、不引入 mock。MAC 复用与
// account_credentials 同型的 HMAC-SHA256 指纹（供 uq_quota_keys_code_mac 唯一索引
// 做 SQL 级查重），不引入新加密机制。
func (s *QuotaKeyService) RotateCredentials(ctx context.Context, id int64, newPlain string) (*QuotaKeyStatus, error) {
	enc, err := s.enc.Encrypt([]byte(newPlain))
	if err != nil {
		return nil, err
	}
	codeEncrypted := string(enc)
	codeMAC := quotaKeyMACOfPlaintext(newPlain)
	codeLast4 := last4Of(newPlain)
	if err := s.repo.UpdateCredentials(ctx, id, codeEncrypted, codeMAC, codeLast4); err != nil {
		return nil, err
	}
	return s.GetStatus(ctx, id)
}

// List 全量返回脱敏后的激活码状态列表（明文 code 绝不出现在返回 DTO 中）。
func (s *QuotaKeyService) List(ctx context.Context) ([]*QuotaKeyStatus, error) {
	qks, err := s.repo.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*QuotaKeyStatus, 0, len(qks))
	for _, qk := range qks {
		out = append(out, quotaKeyToStatus(qk))
	}
	return out, nil
}

// Create 接收明文激活码 → 加密（复用与 RotateCredentials 同一实现）→ 落库 → 返回脱敏状态。
//
// 明文 code 只进不出：加密后仅以密文/last4/MAC 形态入库，返回值一律脱敏，
// 明文绝不进入任何返回值或日志（F1）。新激活码默认 status=active。
func (s *QuotaKeyService) Create(ctx context.Context, code string, expiresAt *time.Time, notes *string) (*QuotaKeyStatus, error) {
	enc, err := s.enc.Encrypt([]byte(code))
	if err != nil {
		return nil, err
	}
	codeEncrypted := string(enc)
	codeMAC := quotaKeyMACOfPlaintext(code)
	codeLast4 := last4Of(code)
	qk, err := s.repo.Create(ctx, CreateQuotaKeyInput{
		CodeEncrypted: codeEncrypted,
		CodeMAC:       codeMAC,
		CodeLast4:     codeLast4,
		Status:        domain.QuotaKeyStatusActive,
		ExpiresAt:     expiresAt,
		Notes:         notes,
	})
	if err != nil {
		return nil, err
	}
	return quotaKeyToStatus(qk), nil
}

// Update 改激活码：code 非 nil 时重加密写 credentials；expiresAt/notes 外层指针非 nil 时写元数据。
// 元数据外层 nil 表示省略，内层 nil 表示显式清空，否则设置为给定值。
// 返回脱敏状态。明文 code 只进不出（F1）。
func (s *QuotaKeyService) Update(ctx context.Context, id int64, code *string, expiresAt **time.Time, notes **string) (*QuotaKeyStatus, error) {
	if code != nil {
		enc, err := s.enc.Encrypt([]byte(*code))
		if err != nil {
			return nil, err
		}
		codeEncrypted := string(enc)
		codeMAC := quotaKeyMACOfPlaintext(*code)
		codeLast4 := last4Of(*code)
		if err := s.repo.UpdateCredentials(ctx, id, codeEncrypted, codeMAC, codeLast4); err != nil {
			return nil, err
		}
	}
	if expiresAt != nil || notes != nil {
		if err := s.repo.UpdateMeta(ctx, id, expiresAt, notes); err != nil {
			return nil, err
		}
	}
	return s.GetStatus(ctx, id)
}

// GetWithAccounts 返回脱敏状态 + 该激活码下的账号切片（不嵌套聚合成单一对象，F7）。
func (s *QuotaKeyService) GetWithAccounts(ctx context.Context, id int64) (*QuotaKeyStatus, []*ent.Account, error) {
	qk, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	accounts, err := s.repo.ListAccountsByQuotaKey(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	return quotaKeyToStatus(qk), accounts, nil
}

// QuotaKeyAccountCounts 是某激活码下的账号计数（与详情端点 Get 的计数口径一致）。
type QuotaKeyAccountCounts struct {
	AccountCount int64
	WorkingCount int64
}

// countQuotaKeyAccountStats 对一批账号按"详情端点 Get 的计数口径"聚合：
// 未软删（DeletedAt == nil）计入 AccountCount；其中 Schedulable==true 且 Status=="active"
// 计入 WorkingCount。这是全仓唯一的账户计数口径实现：详情端点 Get（经导出封装
// CountQuotaKeyAccountStats）与 List 批量计数（经 CountAccountsByQuotaKeys）都收敛于此，
// 不得出现第二种口径。
func countQuotaKeyAccountStats(accounts []*ent.Account) (accountCount, workingCount int64) {
	for _, acc := range accounts {
		if acc.DeletedAt == nil {
			accountCount++
			if acc.Schedulable && acc.Status == "active" {
				workingCount++
			}
		}
	}
	return accountCount, workingCount
}

// CountQuotaKeyAccountStats 是 countQuotaKeyAccountStats 的导出薄封装，供 handler 详情
// 端点 Get 复用同一处计数口径（与 List 的批量计数同源）。纯内存计算，无副作用：
// 仅做"未软删 + working 谓词"投影，使该谓词在生产代码中只有一份实现。
func CountQuotaKeyAccountStats(accounts []*ent.Account) (accountCount, workingCount int64) {
	return countQuotaKeyAccountStats(accounts)
}

// CountAccountsByQuotaKeys 批量统计各激活码下的账号数（一次查询，按 quota_key_id 分组）。
//
// 计数口径见 countQuotaKeyAccountStats，与详情端点 Get 逐字一致。查询次数与激活码数量
// 无关（常数次：一次 repo 批量查询 + 内存分组），严守 List 既有的"杜绝 N+1"承诺
// （respondQuotaKeySummaries 注释：与页数、条目数无关的常数次查询）。
func (s *QuotaKeyService) CountAccountsByQuotaKeys(ctx context.Context, ids []int64) (map[int64]QuotaKeyAccountCounts, error) {
	if len(ids) == 0 {
		return map[int64]QuotaKeyAccountCounts{}, nil
	}
	// 一次查询取全部归属账号，再按 quota_key_id 分组聚合：避免按激活码逐个查询的 N+1。
	accounts, err := s.repo.ListAccountsByQuotaKeys(ctx, ids)
	if err != nil {
		return nil, err
	}
	grouped := make(map[int64][]*ent.Account, len(ids))
	for _, acc := range accounts {
		if acc.QuotaKeyID == nil {
			continue
		}
		grouped[*acc.QuotaKeyID] = append(grouped[*acc.QuotaKeyID], acc)
	}
	out := make(map[int64]QuotaKeyAccountCounts, len(ids))
	for _, id := range ids {
		ac, wc := countQuotaKeyAccountStats(grouped[id])
		out[id] = QuotaKeyAccountCounts{AccountCount: ac, WorkingCount: wc}
	}
	return out, nil
}

// ErrQuotaKeyNotActive 表示激活码存在但已非 active（禁用/过期），不再承载任何账号写入。
var ErrQuotaKeyNotActive = errors.New("quota key is not active")

// ErrQuotaKeyAccountLimitReached 是"归属某激活码的 working 账号已达 ≤15 上限"的唯一
// 容量拒绝错误（400 QUOTA_KEY_ACCOUNT_LIMIT_REACHED）。三条写路径（拉号插入 / admin
// 建号 / admin 改绑）统一返回此错误，不再各自构造各自的计数与拒绝错误。
var ErrQuotaKeyAccountLimitReached = infraerrors.New(http.StatusBadRequest, "QUOTA_KEY_ACCOUNT_LIMIT_REACHED",
	"quota key already holds the maximum number of accounts")

// errQuotaKeyNotFoundForWrite 构造"写路径目标激活码不存在"的拒绝错误。
// cause 挂 ErrQuotaKeyNotFound，使既有 handler 的 404 映射（errors.Is）不受影响。
func errQuotaKeyNotFoundForWrite(quotaKeyID int64) error {
	return infraerrors.Newf(http.StatusBadRequest, "QUOTA_KEY_NOT_FOUND",
		"quota key %d does not exist", quotaKeyID).WithCause(ErrQuotaKeyNotFound)
}

// countWorkingQuotaKeyAccounts 在事务内统计归属某激活码、且未软删的账号数（§3.2 working 谓词）。
//
// 这是 ≤15 判定唯一的计数实现：三条写路径共用，谓词（quota_key_id + deleted_at IS NULL）
// 与原 QuotaKeyRepository.CountWorkingAccounts 完全一致，但不再有第二份副本。
func countWorkingQuotaKeyAccounts(ctx context.Context, tx *ent.Tx, quotaKeyID int64) (int, error) {
	return tx.Account.Query().
		Where(
			account.QuotaKeyID(quotaKeyID),
			account.DeletedAtIsNil(),
		).
		Count(ctx)
}

// checkQuotaKeyCapacityLocked 是 ≤15 强制的唯一判定实现，运行在"已持有该激活码 advisory
// 事务锁"的 tx 内（软删校验随之前移进锁内，修 r4 #2），返回判定时刻的 working 计数：
//  1. quota_key 必须存在且未软删（普通查询天然排除软删行）；
//  2. status 必须为 active；
//  3. working 必须 < MaxAccountsPerQuotaKey。
//
// 调用方必须在【同一 tx】内完成写入：判定与写入分属两个事务时，锁一释放就有并发漏网窗口。
func checkQuotaKeyCapacityLocked(ctx context.Context, tx *ent.Tx, quotaKeyID int64) (int, error) {
	qk, err := tx.QuotaKey.Query().Where(quotakey.IDEQ(quotaKeyID)).ForUpdate().Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return 0, errQuotaKeyNotFoundForWrite(quotaKeyID)
		}
		return 0, fmt.Errorf("quota_key_service: load quota key: %w", err)
	}
	if qk.Status != domain.QuotaKeyStatusActive {
		return 0, ErrQuotaKeyNotActive
	}
	working, err := countWorkingQuotaKeyAccounts(ctx, tx, quotaKeyID)
	if err != nil {
		return 0, fmt.Errorf("quota_key_service: count working accounts: %w", err)
	}
	if working >= MaxAccountsPerQuotaKey {
		return working, ErrQuotaKeyAccountLimitReached.WithMetadata(map[string]string{
			"quota_key_id": strconv.FormatInt(quotaKeyID, 10),
			"working":      strconv.Itoa(working),
			"limit":        strconv.Itoa(MaxAccountsPerQuotaKey),
		})
	}
	return working, nil
}

// withQuotaKeyCapacity 在单激活码 advisory 互斥域内执行"容量判定 + 写入"：
// 取锁 → working 计数（§3.2 谓词）→ 校验 quota_key 未软删且 active → 判定 < MaxAccountsPerQuotaKey
// → 在【同一事务】内调用 fn(tx) 写入 → 提交（锁随事务释放）。
// fn 内不得再自行取锁/计数/开事务：那样会与本入口形成嵌套 tx 或两个事务的两个临界区
// （即 r4 #1/#3 的漏网形态）。容量不足返回 ErrQuotaKeyAccountLimitReached。
func (s *QuotaKeyService) withQuotaKeyCapacity(ctx context.Context, quotaKeyID int64, fn func(tx *ent.Tx) error) error {
	if s.client == nil {
		return errors.New("quota_key_service: ent client not configured")
	}
	return withQuotaKeyCapacityTx(ctx, s.client, quotaKeyID, fn)
}

// withQuotaKeyCapacityTx 是 withQuotaKeyCapacity 的实现本体，供不持有 QuotaKeyService
// 的写路径（admin 建号 / admin 改绑）复用同一入口、同一互斥域，不复制一份临界区。
func withQuotaKeyCapacityTx(ctx context.Context, client *ent.Client, quotaKeyID int64, fn func(tx *ent.Tx) error) error {
	tx, err := client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("quota_key_service: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// advisory xact lock：同 key 串行，异 key 不阻塞；提交/回滚时自动释放。
	if err := acquireAdvisoryXactLock(ctx, tx, quotaKeyAdvisoryLockID(quotaKeyID)); err != nil {
		return err
	}
	if _, err := checkQuotaKeyCapacityLocked(ctx, tx, quotaKeyID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("quota_key_service: commit quota key capacity tx: %w", err)
	}
	committed = true
	return nil
}

// PullAccounts 按 §3.4 拉号状态机拉取账号：全程在集中入口持有的同一 advisory 互斥域与
// 同一事务内完成——容量判定（读状态 + working 计数 + ≤15）→ 二次读防竞态 → 发上游 →
// 成功插入并清 exhausted / 失败 CAS 写 exhausted。
//
// 该 2 参入口不携带模型上下文（reconciler 内部补池调用），故不执行 §13.3 分组挂载
// （mountResolver=nil：拉到裸号入库，挂载由触发方另行编排）；挂载语义经 PullAccountsForModel
// 暴露，手动挂载入口见 RefreshAccounts。
func (s *QuotaKeyService) PullAccounts(ctx context.Context, quotaKeyID int64) error {
	return s.pull(ctx, quotaKeyID, nil, false)
}

// PullAccountsForModel 携带模型 M 上下文的拉号入口（§13.2 触发方传入 / §13.3 挂载）。
// 成功拉号后，新号在落库事务提交前自动挂入"白名单含 M"的全部分组；M 无覆盖则失败关闭
// （回滚、不落裸号）。其内部复用与 PullAccounts 完全相同的 pull 状态机（同一 advisory
// 互斥域 + 同一事务 + ≤15 强制），不另造第二拉号机制。
func (s *QuotaKeyService) PullAccountsForModel(ctx context.Context, quotaKeyID int64, modelM string) error {
	// 挂载解析器：以 modelM 调 groupResolver，复用既有 §13.3 挂载事实源与测试 seam。
	return s.pull(ctx, quotaKeyID, func(ctx context.Context, tx *ent.Tx) ([]int64, error) {
		return s.groupResolver(ctx, tx, modelM)
	}, false)
}

// RefreshAccounts 手动触发拉号（§4.4），与 PullAccounts 共享同一 advisory 互斥域。
//
// 与 PullAccounts 的唯一差异（B1 缺陷修复，2026-10-15）：手动入口【走挂载语义】。手动刷新
// 不携带模型上下文，挂载目标 = 配置勾选模型 ∩ 现有活跃分组白名单的交集（findCursorPlatformGroups，
// 与自动链路 ListAllowedModels 同源同判）。这样手动拉到的 cursor 账号会挂入白名单分组
// （group_count>0、可用），而非落成裸号（group_count=0）。勾选空 → ErrQuotaKeyModelNoCoverage
// 失败关闭。
// PullAccounts(reconciler 内部) 保持 mountResolver=nil、不挂载——两入口行为差异在此显式区分。
func (s *QuotaKeyService) RefreshAccounts(ctx context.Context, quotaKeyID int64) error {
	return s.pull(ctx, quotaKeyID, s.cursorGroupResolver, true)
}

// pull 是 PullAccounts / PullAccountsForModel / RefreshAccounts 共享的拉号状态机实现。
// mountResolver 非空时启用 §13.3 挂载：新号提交前挂入 mountResolver 解析出的全部分组；
// 为空（nil）则跳过挂载（reconciler 内部入口 PullAccounts 即此形态）。
//
// 全链路在 ≤15 集中入口（withQuotaKeyCapacity）持有的同一 advisory 互斥域 + 同一事务内：
// 入口先完成"未软删 + active + working < MaxAccountsPerQuotaKey"判定，fn 内不得再取锁、
// 不再开事务，插入/状态写入与判定同事务提交（修 r4 #3 事务边界）。
//
// 结果透传约定：
//   - 冷却（ErrUpstreamCooldown）：fn 内写标记后返回 nil 以便事务提交，再由 pull 透传哨兵；
//   - 落库失败（create.Save）：R5-1 —— 返回 errInsertFailureRollback 哨兵使容量事务整体回滚
//     （已部分插入的账号随之消失，号不入库）；pull 在 withQuotaKeyCapacity 返回后用【独立事务】
//     补写 last_refresh_error 标记（豁免 exhausted）。标记写成功 ⇒ 跨轮退出候选（R5-1 a）；
//     标记写失败 ⇒ 仅本轮哨兵停轮（R5-2 边界，不夸大保护范围为跨轮退出）。哨兵 ErrQuotaKeyMountFailed
//     由 pull 透传给触发方（非幂等边界 = 告警 + 停轮）；
//   - 解析失败 / 零覆盖 / 挂组失败：fn 返回错误 → 事务回滚（号不入库）→ pull 直接返回该错误；
//     零覆盖发生在上游成功之后，同时挂 ErrQuotaKeyMountFailed + ErrQuotaKeyModelNoCoverage；
//   - 容量上限 / 已非 active：fn 返回 nil，不改变 exhausted，不上抛。
func (s *QuotaKeyService) pull(ctx context.Context, quotaKeyID int64, mountResolver func(ctx context.Context, tx *ent.Tx) ([]int64, error), manual bool) error {
	if s.client == nil {
		return errors.New("quota_key_service: ent client not configured")
	}
	// resultErr 捕获"需提交后再透传给触发方"的结果（冷却分支：fn 返回 nil 以提交标记，
	// 但调用方仍需拿到可识别哨兵）。挂载失败 / 模型无覆盖则走 fn 错误返回（已回滚）。
	var resultErr error
	// insertFailErr 捕获插入循环中"首个落库失败"的底层原因（R5-1）：仅当该值非 nil 时，
	// fn 才返回 errInsertFailureRollback 哨兵，事后由独立事务补写标记。
	var insertFailErr error
	err := s.withQuotaKeyCapacity(ctx, quotaKeyID, func(tx *ent.Tx) error {
		// 二次读状态（防 sweep 竞态）：重新 FOR UPDATE 确认仍为 active 且未过期。
		qk2, err := tx.QuotaKey.Query().Where(quotakey.IDEQ(quotaKeyID)).ForUpdate().Only(ctx)
		if err != nil {
			return fmt.Errorf("quota_key_service: re-load quota key: %w", err)
		}
		if qk2.Status != domain.QuotaKeyStatusActive || isExpired(qk2) {
			return nil
		}

		// 发上游。
		result, pullErr := s.upstream.PullBatch(ctx, qk2)
		if pullErr != nil {
			// 冷却分支（F-b，R4 消化）：同事务写 last_refresh_error + last_refresh_at，
			// 唯独豁免 exhausted；返回 nil 使事务提交标记，哨兵由 resultErr 透传。
			// R1-1：resultErr 保留携带上游中文 msg 原文的归类错误（不再用裸哨兵丢失 msg）。
			if errors.Is(pullErr, ErrUpstreamCooldown) {
				if werr := s.writeCooldownMarker(ctx, tx, quotaKeyID, pullErr); werr != nil {
					return fmt.Errorf("quota_key_service: write cooldown marker: %w", werr)
				}
				resultErr = pullErr
				return nil
			}
			// 上游确认"密钥额度耗尽或已过期"一类终态（裁定 2）：同事务把该 key 标 expired
			// （复用既有 QuotaKeyStatusExpired 与 sweeper 同型的条件写口径：仅当当前
			// status='active' 才写入，不覆盖 sweep 已置的禁用/过期），不写任何由 msg 推导的日期
			// （expires_at 始终保持原值）。返回 nil 使事务提交标记，哨兵由 resultErr 透传。
			// R1-1：resultErr 保留携带上游中文 msg 原文的归类错误（不再用裸哨兵丢失 msg）。
			if errors.Is(pullErr, ErrUpstreamKeyExpired) {
				if werr := s.markKeyExpiredByUpstream(ctx, tx, quotaKeyID, pullErr); werr != nil {
					return fmt.Errorf("quota_key_service: mark key expired by upstream: %w", werr)
				}
				resultErr = pullErr
				return nil
			}
			// 其余上游错误（含未知结果失败关闭）：写 last_refresh_error + last_refresh_at（失败心跳
			// 与排障可见性），但【不得】写 exhausted=true（CUR-P1-FIX-KEY-EXHAUSTED，方案 §15.2/§16.1）：
			// 未知/普通上游失败不是已确认的 key 级终态，不能由"一次失败"或"拉到少量账号"推导停供，
			// 否则管理页会把仍可用的激活码显示成"已耗尽"。停供只认已确认终态（ErrUpstreamKeyExpired →
			// status=expired）与非幂等挂载失败（插入失败标记，见 writeInsertFailureMarker）两类事实。
			// 手动链路（RefreshAccounts, manual=true）必须上抛带原文的错误（*UpstreamPullError），
			// 不得静默显示"刷新成功"；自动链路（reconciler, manual=false）原先静默 return nil——但 nil
			// 会被 reconciler 当成功（心跳"reconciled N quota keys"把失败算成成功，R13-2）。改为透传
			// 含 ErrUpstreamKeyBlocked 哨兵的包裹错误，reconciler 据以如实计入失败（记失败日志、不加
			// pulled、返回 false、继续下一候选），单 key 未知失败不中断其余候选的既有语义保持不变。
			if cerr := s.writeUpstreamFailureMarker(ctx, tx, quotaKeyID, pullErr); cerr != nil {
				return fmt.Errorf("quota_key_service: write upstream failure marker: %w", cerr)
			}
			if manual {
				resultErr = newUpstreamUnknownErrorFrom(pullErr)
			} else {
				// R13-2：自动链路未知上游失败——标记已落 last_refresh_error（exhausted 维度不动），
				// 经既有 resultErr 透传机制返回（fn 返回 nil → 末尾 resultErr 非空即透传给触发方）。
				// 包裹 ErrUpstreamKeyBlocked 哨兵（与底层原因摘要）以便 reconciler 以 errors.Is 识别。
				resultErr = fmt.Errorf("%w: %s", ErrUpstreamKeyBlocked, pullErr.Error())
			}
			return nil
		}

		// 成功：四读（再次 FOR UPDATE 确认状态）后重取 working，按剩余容量截断插入，再清 exhausted。
		qk3, err := tx.QuotaKey.Query().Where(quotakey.IDEQ(quotaKeyID)).ForUpdate().Only(ctx)
		if err != nil {
			return fmt.Errorf("quota_key_service: final-load quota key: %w", err)
		}
		if qk3.Status != domain.QuotaKeyStatusActive {
			return nil
		}
		// 插入前在同一事务/同一锁内重判容量：并发抢位（如上一次拉号刚提交）在此拦住。
		working, err := checkQuotaKeyCapacityLocked(ctx, tx, quotaKeyID)
		if err != nil {
			if errors.Is(err, ErrQuotaKeyAccountLimitReached) || errors.Is(err, ErrQuotaKeyNotActive) {
				return nil
			}
			return err
		}

		// 容量判定与插入在同一事务/同一 advisory 互斥域内：maxInsert 以插入前读到的
		// working 为基准，working=14 而上游返 2 时只落 1 条，总数恰好 15（不多不少）。
		// upstream 溢出的账号直接丢弃：UpstreamPullResult 不带重放字段，不做二次落库。
		maxInsert := MaxAccountsPerQuotaKey - working
		if maxInsert <= 0 {
			return nil
		}

		// 配置单次读取（同批号一致性口径，方案审 F2 消化）：本快照供既有 ApplyEntryDefaults
		// （挂组路径，替换原挂组内的第二次 GetConfig）使用；model_mapping 已改为静态清单
		// （P1-B2：入库退出人工勾选定义能力，entry 模型 = CursorRealSupplyModels 恒等映射），
		// 不再随配置勾选漂移——首次按静态清单写 model_mapping，后续校准由 B1 入口覆盖同一字段。
		// autoEntry 未装配（nil）时跳过：账号无映射 → IsModelSupported 对 cursor 失败
		// 关闭（既有闸门，不放宽），与 D1 既有测试/未装配场景行为一致。
		//
		// 读取失败 = 上游成功发号之后的本地边界失败（shareai 非幂等，号不可回滚）：
		// 挂 ErrQuotaKeyMountFailed 哨兵使事务回滚（号不入库），触发方走既有
		// 「非幂等边界 = 告警 + 停轮」语义，不得当作普通错误继续拉下一候选白耗额度
		// （方案审 F1 must_fix，2026-10-18）。
		var entryCfg *CursorAutoEntryConfig
		var entryModelMapping map[string]any
		if s.autoEntry != nil {
			cfg, cerr := s.autoEntry.GetConfig(ctx)
			if cerr != nil {
				return fmt.Errorf("quota_key_service: load auto entry config: %w: %w", ErrQuotaKeyMountFailed, cerr)
			}
			entryCfg = cfg
			entryModelMapping = CursorAccountModelMapping(CursorStaticEntryModels())
		}
		insertedIDs := make([]int64, 0, len(result.Accounts))
		for i, ua := range result.Accounts {
			if i >= maxInsert {
				break
			}
			// 裁定 5：落库前走既有静态加密写路径（口径照 repository.prepareCredentialsForStorage /
			// account_repo.go 的 createAccountRecord）：对 SensitiveCredentialKeys 命中的非空字符串
			// 子键（cursor 凭证为 access_token / refresh_token）以 credcrypt.Encrypt 加密（前缀
			// enc:v1:，credcrypt.IsEncrypted 幂等），并对整份明文凭证计算 HMAC-SHA256 指纹写入
			// credentials_mac。不新增加密机制/字段；明文绝不直落库。
			//
			// 模型映射在加密之前并入（浅拷贝，不改上游产物）：model_mapping 非敏感键、明文存
			// JSONB（与其他平台 admin 建号形态一致），MAC 覆盖含映射的最终明文——单次加密+
			// 单次指纹，契约零改动。映射为静态实供清单恒等映射（P1-B2），仅当 autoEntry 未装配
			// （entryModelMapping 为 nil）时不写键 → cursor 账号对一切模型失败关闭
			// （account.go IsModelSupported 平台分支）。
			creds := ua.Credentials
			if entryModelMapping != nil {
				merged := make(map[string]any, len(creds)+1)
				for k, v := range creds {
					merged[k] = v
				}
				merged["model_mapping"] = entryModelMapping
				creds = merged
			}
			credStorage, credMAC, cerr := prepareCursorAccountCredentials(creds)
			if cerr != nil {
				insertFailErr = fmt.Errorf("prepare credentials: %w", cerr)
				return fmt.Errorf("quota_key_service: %w", errInsertFailureRollback)
			}
			create := tx.Account.Create().
				SetName(ua.Name).
				SetPlatform(ua.Platform).
				SetType(ua.Type).
				SetCredentials(credStorage).
				SetCredentialsMAC(credMAC).
				SetQuotaKeyID(quotaKeyID)
			if ua.Extra != nil {
				create.SetExtra(ua.Extra)
			}
			acc, err := create.Save(ctx)
			if err != nil {
				// R5-1 核心（增量复审 R5 整改）：本分支位于【上游发号成功之后】——号已发出、
				// 上游不可回滚（shareai 非幂等），但本地落库失败。此时同一批次中可能已有
				// 前序账号被插入到【当前容量事务】内（多账号部分成功）。
				//
				// 关键不变量：失败时【不得提交任何已部分插入的账号】——否则会留下"裸号"
				//（号已入库、但上游本就非幂等、且无挂载/无标记，等于悄悄消耗上游且泄漏）。
				// 因此这里【不再】于同事务内写 last_refresh_error 后提交；改为：
				//   1) 记录底层落库失败原因到 insertFailErr（供事后独立事务补标）；
				//   2) 返回私有哨兵 errInsertFailureRollback，使 withQuotaKeyCapacity 整体
				//      【回滚】当前容量事务（已插入账号随之消失，号不入库）；
				//   3) pull 在 withQuotaKeyCapacity 返回后，用【独立事务】补写 last_refresh_error
				//      标记（R5-1 a：标记写成功 → 跨轮退出候选）；若该独立事务也失败（R5-1 b：
				//      标记写失败），则仅本轮哨兵停轮（R5-2 边界，不夸大保护范围）。
				//
				// 与解析失败 / 零覆盖 / 挂组失败同一哨兵族 ErrQuotaKeyMountFailed（非幂等边界，
				// 触发方走"告警 + 停轮"），底层落库失败原因经 insertFailErr 重新 %w 回链。
				insertFailErr = fmt.Errorf("create account: %w", err)
				return fmt.Errorf("quota_key_service: %w", errInsertFailureRollback)
			}
			insertedIDs = append(insertedIDs, acc.ID)
		}

		// §13.3 新号挂载：mountResolver 由触发方在落库事务内执行，提交前挂入其解析出的
		// 全部分组。mountResolver 为 nil（reconciler 内部入口 PullAccounts）时跳过挂载。
		// mountResolver 非空即代表"触发方要求挂载"——无论携带模型 M（PullAccountsForModel）
		// 还是手动交集口径挂载（RefreshAccounts，B1，经 cursorGroupResolver → findCursorPlatformGroups）。
		if mountResolver != nil {
			groupIDs, err := mountResolver(ctx, tx)
			if err != nil {
				// F4：上游已成功发号（号未入库，事务尚未提交），但本地分组解析失败
				//（shareai 非幂等，上游发号不可回滚）→ 事务将回滚（号不入库）+ 与挂载失败
				// 同哨兵 ErrQuotaKeyMountFailed 返回，调用方可识别"上游可能已被消耗一次"。
				return fmt.Errorf("quota_key_service: resolve mount groups: %w: %w", ErrQuotaKeyMountFailed, err)
			}
			if len(groupIDs) == 0 {
				// 失败关闭（fail-closed）：解析出零覆盖分组，不得落成裸号；返回错误使事务回滚，
				// 号不入库。触发方据此记录告警。
				//
				// F4'（2026-10-15 增量复审 R2）：本分支发生在【上游发号成功之后】——白名单
				// 与上游发号不同快照，期间可变更，覆盖可能恰好此刻消失。上游号已消耗且不可
				// 回滚（shareai 非幂等），故除 ErrQuotaKeyModelNoCoverage 外同时补挂
				// ErrQuotaKeyMountFailed 哨兵（%w 双链，两个 errors.Is 皆为真），使触发方
				// 走"非幂等边界 = 告警 + 停轮"，不得当作普通错误继续拉下一个候选。
				return fmt.Errorf("quota_key_service: no covering group after upstream success: %w: %w",
					ErrQuotaKeyMountFailed, ErrQuotaKeyModelNoCoverage)
			}
			// 上游成功、本地挂载失败（shareai 非幂等，上游发号不可回滚）→ 本地事务回滚
			//（号不入库）+ 可识别错误供触发方记录"上游可能已被消耗一次"告警；不得表述为
			//"拉号动作整体回滚"。
			// D2-a 入库可配置：entryCfg 已在插入前单次读取（同批号一致性口径，方案审 F2），
			// 此处直接复用同一快照逐个写账号属性，不再二次 GetConfig。
			for _, accID := range insertedIDs {
				// 仅写账号属性，不动挂组集合计算（禁区）。nil 时跳过（向后兼容 D1 与未装配）。
				if s.autoEntry != nil {
					acc, lerr := tx.Account.Get(ctx, accID)
					if lerr != nil {
						return fmt.Errorf("quota_key_service: load account %d for entry defaults: %w", accID, lerr)
					}
					if _, aerr := s.autoEntry.ApplyEntryDefaults(ctx, tx, acc, entryCfg); aerr != nil {
						return fmt.Errorf("quota_key_service: apply entry defaults to account %d: %w", accID, aerr)
					}
				}
				if err := s.mountAccountToGroups(ctx, tx, accID, groupIDs); err != nil {
					return fmt.Errorf("quota_key_service: mount account %d to groups: %w: %w", accID, ErrQuotaKeyMountFailed, err)
				}
			}
		}

		// 清 exhausted 与错误占位。
		// SetNillableLastRefreshError(nil) 对 nil 指针是 no-op（仅 v != nil 时才写），
		// 必须用 ClearLastRefreshError() 才能真正把 last_refresh_error 置空。
		//
		// B-11：同一次写入顺带落 expires_at（上游 key_info.expired_at 的事实源）。
		// SetNillableExpiresAt 对 nil 是 no-op —— 上游未给出到期时间时保持原值不动，
		// 既不写假日期、也不清空管理员手工设置的值。上游给出的值始终覆盖本地值：
		// 上游是激活码有效期的唯一权威，本地任何旧值都可能是陈旧的。
		if _, err := tx.QuotaKey.UpdateOneID(quotaKeyID).
			SetExhausted(false).
			SetLastRefreshAt(time.Now()).
			ClearLastRefreshError().
			SetNillableExpiresAt(result.ExpiresAt).
			Save(ctx); err != nil {
			return fmt.Errorf("quota_key_service: clear exhausted: %w", err)
		}
		return nil
	})
	// 达到上限 / 已非 active：与既有语义一致，停止拉号且不改 exhausted，不上抛为错误。
	if errors.Is(err, ErrQuotaKeyAccountLimitReached) || errors.Is(err, ErrQuotaKeyNotActive) {
		return nil
	}
	// fn 返回非 nil：
	if err != nil {
		// R5-1 插入失败回滚分支：fn 以 errInsertFailureRollback 哨兵信号"整体回滚当前容量
		// 事务"（已部分插入的账号随之消失，号不入库）。此处用【独立事务】补写 last_refresh_error
		// 标记，使"回滚全部账号"与"标记持久化"两目标同时成立（二者无法同事务共存）。
		if errors.Is(err, errInsertFailureRollback) && insertFailErr != nil {
			// a) 标记写成功 → 事务回滚全部已插入账号（号不入库）+ 标记持久化；
			//    标记存在 ⇒ ListCandidateKeys 的 LastRefreshErrorIsNil() 过滤把该 key 排出
			//    后续候选 ⇒ 跨轮退出候选（R5-2：标记写成功 = 跨轮退出候选）。
			if werr := s.insertFailureMarkerWriter(ctx, quotaKeyID, insertFailErr); werr != nil {
				// b) 标记写失败 → 整体回滚（现状已对）。此时 last_refresh_error 未持久，
				//    该 key 仍可能被后续周期重新选为候选（R5-2 边界：标记写失败 = 仅本轮
				//    哨兵停轮，不夸大保护范围为"跨轮退出"）。哨兵仍上抛，使本轮停轮。
				return fmt.Errorf("quota_key_service: create account (marker write failed: %v): %w: %w",
					werr, ErrQuotaKeyMountFailed, insertFailErr)
			}
			// a) 标记已持久：上抛 ErrQuotaKeyMountFailed 哨兵（非幂等边界，触发方停轮 +
			//    记"上游可能已被消耗一次"告警），底层落库失败原因回链。
			return fmt.Errorf("quota_key_service: create account: %w: %w",
				ErrQuotaKeyMountFailed, insertFailErr)
		}
		// 其余 fn 错误（挂载失败 / 模型无覆盖 / 解析失败）→ 事务已回滚，错误即结果。
		return err
	}
	// fn 返回 nil（含冷却分支已提交标记）→ 透传捕获的结果（冷却哨兵 / 无则 nil）。
	if resultErr != nil {
		return resultErr
	}
	return nil
}

// DeleteWithAccounts 软删激活码及其全部账号（§3.2 删除唯一路径 = 软删口径）。
// 锁 → tx：逐账号软删 → 写 scheduler outbox（既有入口，同事务持久）→ 软删 quota_key
// → commit（全删或不删）。
// 数据库账号行与全部历史记录保留，禁止物理 DELETE 账号行。
func (s *QuotaKeyService) DeleteWithAccounts(ctx context.Context, quotaKeyID int64) error {
	if s.client == nil {
		return errors.New("quota_key_service: ent client not configured")
	}
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("quota_key_service: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err := acquireAdvisoryXactLock(ctx, tx, quotaKeyAdvisoryLockID(quotaKeyID)); err != nil {
		return err
	}

	// 确认存在（FOR UPDATE 锁行）。已软删的行在普通查询中不可见。
	if _, err := tx.QuotaKey.Query().Where(quotakey.IDEQ(quotaKeyID)).ForUpdate().Only(ctx); err != nil {
		if ent.IsNotFound(err) {
			return ErrQuotaKeyNotFound
		}
		return fmt.Errorf("quota_key_service: load quota key for delete: %w", err)
	}

	// 逐账号软删（置 deleted_at；禁止物理删除 accounts 表账号行）。返回被软删账号 ID。
	deletedIDs, err := s.repo.SoftDeleteAccountsByQuotaKey(ctx, tx, quotaKeyID)
	if err != nil {
		return fmt.Errorf("quota_key_service: soft-delete accounts: %w", err)
	}
	// 与被软删账号同一事务写既有 scheduler outbox 入口：outbox 是持久层（durable），
	// 其 consumer 消费 account_changed 事件时把账号从 Redis 调度快照移除。
	// 不写 outbox ⇒ Redis 快照继续保留已软删账号（§3.2 DB outbox → Redis snapshot 两层）。
	if err := s.repo.EnqueueAccountChangedOutbox(ctx, tx, deletedIDs); err != nil {
		return fmt.Errorf("quota_key_service: enqueue scheduler outbox: %w", err)
	}
	// 软删 quota_key。
	if err := s.repo.SoftDelete(ctx, tx, quotaKeyID); err != nil {
		return fmt.Errorf("quota_key_service: soft-delete quota key: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("quota_key_service: commit delete: %w", err)
	}
	committed = true
	// C12-v2 水位快路径：软删事务提交成功后唤醒 reconciler 重判水位（只置 pending，不直接拉号；
	// reconciler 轮出前追加一次水位判定，有缺口才补）。reconciler 未装配（nil）时跳过。
	if s.reconciler != nil {
		s.reconciler.NotifyAccountRemoved()
	}
	return nil
}

// ClearExhausted 仅清除 exhausted 标记，不触发拉号（§0.1 #4）。
func (s *QuotaKeyService) ClearExhausted(ctx context.Context, quotaKeyID int64) error {
	if s.client == nil {
		return errors.New("quota_key_service: ent client not configured")
	}
	_, err := s.client.QuotaKey.UpdateOneID(quotaKeyID).
		SetExhausted(false).
		ClearLastRefreshError().
		Save(ctx)
	if err != nil {
		return fmt.Errorf("quota_key_service: clear exhausted: %w", err)
	}
	return nil
}

// writeUpstreamFailureMarker 在【未知 / 普通上游失败】分支同事务写 last_refresh_error +
// last_refresh_at（CAS：仅当当前 status='active' 才写入，若 sweep 已将状态改为 expired/disabled
// 则 0 行受影响，不覆盖），供失败心跳与排障可见。
//
// 关键约束（CUR-P1-FIX-KEY-EXHAUSTED，方案 §15.2/§15.4/§16.1）：本分支【绝不】写 exhausted
// （既不置 true，也不清 false）。未知/普通上游失败不是已确认的 key 级终态——停供只能依据
// 已确认终态（ErrUpstreamKeyExpired → markKeyExpiredByUpstream 写 status=expired）或上游已
// 发号的非幂等边界（writeInsertFailureMarker），不得由账号数、普通错误、未知错误推导耗尽。
// 该 key 因此仍留在候选集内（ListCandidateKeys 的 Exhausted(false) 过滤不排除它），后续周期
// 可继续尝试；上游若持续不可用，由冷却哨兵（ErrUpstreamCooldown）按限频窗口自然收敛。
func (s *QuotaKeyService) writeUpstreamFailureMarker(ctx context.Context, tx *ent.Tx, quotaKeyID int64, pullErr error) error {
	_, err := tx.QuotaKey.Update().
		Where(quotakey.IDEQ(quotaKeyID), quotakey.Status(domain.QuotaKeyStatusActive)).
		SetLastRefreshAt(time.Now()).
		SetLastRefreshError(pullErr.Error()).
		Save(ctx)
	if err != nil {
		return err
	}
	return nil
}

// writeCooldownMarker 在冷却分支（ErrUpstreamCooldown）同事务写 last_refresh_error +
// last_refresh_at，并顺带 SetExhausted(false)（B2：冷却≠耗尽）。
//
// 冷却分支历来豁免 exhausted（R1 F1：冷却是限流态，不是号池耗尽态）——但 v3.5 部署前旧代码
// 曾在冷却响应里把 exhausted 写为 true（存量 exhausted=true + last_refresh_error 为冷却文案的
// key1/key2 即此旧态）。本分支若只写标记不纠正 exhausted，存量 key 会永久显示"已耗尽"。故
// 冷却分支在同事务显式 SetExhausted(false)，把冷却态与耗尽态解耦（存量纠正；既有 exhausted===false
// 的冷却 key 不受影响，仍写 false）。沿用 writeUpstreamFailureMarker 的 CAS 约束：仅当当前 status='active'
// 才写入（不覆盖 sweep 已置的禁用/过期），否则 0 行。
//
// 注意区分：本冷却分支写 SetExhausted(false)（冷却≠耗尽，B2）。插入失败是独立函数
// writeInsertFailureMarker（R5-1）——它写 SetExhausted(true)，属于非限频错误占位排除
// （裁定 3），与冷却分支的 SetExhausted(false) 口径不同，不可混为一谈。冷却 5min 时间窗
// 判定不在本函数内。
// cooldownMarkerText 显式构造冷却标记的落库文本：固定以 ErrUpstreamCooldown 哨兵前缀开头
// 拼接上游原文（UpstreamPullError.Message()），【不再】依赖 pullErr.Error() 整链 toString。
//
// 背景（R3-1）：activate 限频错误经多层包装（shareai: activate code %d: %w(shareai: upstream
// cooldown, retry later: <原文>)），其 Error() 以 "shareai: activate code" 开头，前缀不匹配
// isCooldownRefreshError 依赖的 ErrUpstreamCooldown 哨兵 → 5min 冷却窗口判定失效。
// 改为"显式构造"后，activate / refresh 两条路径落库文本形态一致（均以 ErrUpstreamCooldown
// 前缀开头 + 上游原文），前缀匹配继续成立且语义不变（非冷却错误文本不带该前缀，不误判）。
func cooldownMarkerText(pullErr error) string {
	if upe, ok := pullErr.(*UpstreamPullError); ok && upe.Kind == UpstreamCooldown {
		return fmt.Sprintf("%s: %s", ErrUpstreamCooldown.Error(), upe.Message())
	}
	// 防御：writeCooldownMarker 仅在 ErrUpstreamCooldown 分支调用，理论不会走到此处；
	// 仍显式以冷却哨兵前缀落库，保证 isCooldownRefreshError 可识别。
	return fmt.Sprintf("%s: %s", ErrUpstreamCooldown.Error(), pullErr.Error())
}

func (s *QuotaKeyService) writeCooldownMarker(ctx context.Context, tx *ent.Tx, quotaKeyID int64, pullErr error) error {
	_, err := tx.QuotaKey.Update().
		Where(quotakey.IDEQ(quotaKeyID), quotakey.Status(domain.QuotaKeyStatusActive)).
		SetExhausted(false).
		SetLastRefreshAt(time.Now()).
		SetLastRefreshError(cooldownMarkerText(pullErr)).
		Save(ctx)
	if err != nil {
		return err
	}
	return nil
}

// writeInsertFailureMarker 在【独立事务】内写 last_refresh_error 标记（R5-1）。
//
// 与"插入失败即同事务写标记后提交"的旧实现不同：多账号部分成功时，若在同一容量事务内写
// 标记后提交，会连带把【已部分插入但本应回滚的账号】一起提交成裸号（R5-1 缺陷形态）。故
// 改为两段编排——pull 先在容量事务内整体回滚（号不入库），再以本独立事务补写标记，使
// "回滚全部账号"与"标记持久化"两目标同时成立。
//
// 语义约束（零新表/字段）：写 last_refresh_error + last_refresh_at，CAS 仅当 status='active'
// （不覆盖 sweep 已置的禁用/过期），并顺带 SetExhausted(true)。
//
// 裁定 3（候选 key 冷却规则）：last_refresh_error IS NULL 这一永久排除已删除，不得换皮重演。
// 插入失败标记属"上游已成功发号、仅本地落库失败"的非幂等边界（不同于 CUR-P1-FIX-KEY-EXHAUSTED
// 所约束的"未知/普通上游失败"——后者只写标记、不动 exhausted，见 writeUpstreamFailureMarker），
// 故仍以 exhausted=true 占位排除——由此被 ListCandidateKeys 的 Exhausted(false) 查询自然排出
// 候选集（原 LastRefreshErrorIsNil() 过滤的替代口径）。标记写失败（如 CAS 0 行 / DB 错误）由
// 调用方降格为"仅本轮哨兵停轮"（R5-2 边界）。
func (s *QuotaKeyService) writeInsertFailureMarker(ctx context.Context, quotaKeyID int64, cause error) error {
	if s.client == nil {
		return errors.New("quota_key_service: ent client not configured")
	}
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("quota_key_service: begin insert-failure marker tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	n, err := tx.QuotaKey.Update().
		Where(quotakey.IDEQ(quotaKeyID), quotakey.Status(domain.QuotaKeyStatusActive)).
		SetExhausted(true).
		SetLastRefreshAt(time.Now()).
		SetLastRefreshError(cause.Error()).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("quota_key_service: write insert-failure marker: %w", err)
	}
	// CAS 未命中（status 已非 active，例如 sweep 在回滚与本事务之间把 key 置 expired/disabled）
	// ⇒ 标记实际未写入。此时降级为 R5-1(b)：仅本轮哨兵停轮，不夸大"跨轮退出"保护范围。
	if n == 0 {
		return fmt.Errorf("quota_key_service: insert-failure marker not written (status no longer active)")
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("quota_key_service: commit insert-failure marker tx: %w", err)
	}
	committed = true
	return nil
}

// markKeyExpiredByUpstream 在【上游确认密钥额度耗尽或已过期】分支（ErrUpstreamKeyExpired），
// 同事务把该 key 标 expired（裁定 2）。
//
// 口径与 sweeper 同型（quota_key_sweeper.go）：仅当当前 status='active' 才写入（不覆盖 sweep
// 已置的禁用/过期状态），且只改 status 维度、保留 exhausted / last_error / last_refresh_at
// （两个维度独立，与 sweeper 一致）。关键约束：绝不据上游 msg 反推或伪造任何日期——
// 此处不触碰 expires_at，保持原值不动（上游 msg 是终态判定来源，不是日期来源）。
func (s *QuotaKeyService) markKeyExpiredByUpstream(ctx context.Context, tx *ent.Tx, quotaKeyID int64, pullErr error) error {
	_, err := tx.QuotaKey.Update().
		Where(quotakey.IDEQ(quotaKeyID), quotakey.Status(domain.QuotaKeyStatusActive)).
		SetStatus(domain.QuotaKeyStatusExpired).
		SetLastRefreshAt(time.Now()).
		SetLastRefreshError(pullErr.Error()).
		Save(ctx)
	if err != nil {
		return err
	}
	return nil
}

// prepareCursorAccountCredentials 为 cursor 拉号落库的账号凭证做静态加密（裁定 5，口径照
// repository.prepareCredentialsForStorage / account_repo.go 的 createAccountRecord）：
//   - 对 SensitiveCredentialKeys 命中的非空字符串子键（cursor 凭证为 access_token /
//     refresh_token）加密（enc:v1: 前缀，credcrypt.Encrypt + credcrypt.IsEncrypted 幂等判断）；
//   - 对整份明文凭证计算 HMAC-SHA256 指纹（写入 credentials_mac，供 SQL 级相等比对）。
//
// 不新增加密机制/字段；敏感键集合以 service.SensitiveCredentialKeys 为准。明文绝不直落库。
// 返回：加密后的存储形态 + 指纹字符串。credcrypt 未配置主密钥时 Encrypt 退化为 passthrough
// （与既有 repository 行为一致），指纹仍照常按明文维护。
//
// 裁定 5 / R4-3：cursor 凭证必须被加密保护。credcrypt 未启用（进程未安装主密钥）时，
// 本函数直接报错——由调用方走 errInsertFailureRollback 路径（事务回滚、号不入库），绝不
// 留明文透传落库的可能。生产已启用加密（enc:v1 在产），此分支在生产不可达；仅防御性
// 关掉加密的部署会被失败关闭拦截，而非悄悄落明文。
func prepareCursorAccountCredentials(in map[string]any) (map[string]any, string, error) {
	// R4-3：加密未启用 → 失败关闭，不允许明文落库。
	if !credcrypt.Enabled() {
		return nil, "", fmt.Errorf("credential encryption not enabled")
	}
	// 先复制一份明文用于指纹计算（指纹始终基于明文，与加密无关）。
	plain := make(map[string]any, len(in))
	for k, v := range in {
		plain[k] = v
	}
	canonical, err := json.Marshal(plain)
	if err != nil {
		return nil, "", fmt.Errorf("canonicalize credentials: %w", err)
	}
	mac := credentialsHMACOfJSON(canonical)

	storage := make(map[string]any, len(plain))
	for k, v := range plain {
		storage[k] = v
	}
	if credcrypt.Enabled() {
		for _, key := range SensitiveCredentialKeys {
			str, ok := storage[key].(string)
			if !ok || str == "" {
				continue
			}
			if credcrypt.IsEncrypted(str) {
				continue // 已加密：幂等，不重复加密
			}
			enc, eerr := credcrypt.Encrypt(str)
			if eerr != nil {
				return nil, "", fmt.Errorf("encrypt credentials key %q: %w", key, eerr)
			}
			storage[key] = enc
		}
	}
	return storage, mac, nil
}

// credentialsHMACOfJSON 计算凭证明文 canonical JSON 的 HMAC-SHA256 hex（与
// repository.credentialsMACOfJSON 同算法：credcrypt.MACKey() 派生的 32 字节密钥）。
func credentialsHMACOfJSON(canonical []byte) string {
	sum := hmac.New(sha256.New, credcrypt.MACKey())
	sum.Write(canonical)
	return hex.EncodeToString(sum.Sum(nil))
}

// findGroupsAllowingModel 是 §13.3 挂载事实源：返回"白名单含模型 M"的全部分组 ID。
// 白名单事实源 = 既有分组模型白名单（domain.GroupModelAllowlist），不新造映射表；
// 复用 service.GroupModelAllowlist.Allows 的既有通配/前缀匹配语义。仅取未软删分组。
func (s *QuotaKeyService) findGroupsAllowingModel(ctx context.Context, tx *ent.Tx, modelM string) ([]int64, error) {
	// R1-2：与枚举/水位（ListAllowedModels / WaterLevel）共用同一挂组门槛——
	// 仅 active + 未软删分组参与白名单判定。判定逻辑本身（GroupModelAllowlistFromDomain
	// 的 Allows）保持不变；仅补齐与枚举/水位对齐的过滤条件，避免 disabled/软删分组仍被
	// 当成"含 M 的覆盖组"导致新号误挂（findCursorPlatformGroups 已有同条件，不用动）。
	groups, err := tx.Group.Query().
		Where(
			group.Status(domain.StatusActive),
			group.DeletedAtIsNil(),
		).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("quota_key_service: query groups: %w", err)
	}
	out := make([]int64, 0, len(groups))
	for _, g := range groups {
		if GroupModelAllowlistFromDomain(g.ModelAllowlist).Allows(modelM) {
			out = append(out, g.ID)
		}
	}
	return out, nil
}

// cursorModelOwningPlatform 是 cursor 实供清单模型的【所属平台】。
//
// 判定依据：实供清单模型由 cursor 平台账号经 credentials.model_mapping 供给，composite 归属层
// （resolveCompositeModelOwnership）亦按"账号平台"判定模型归属 → 清单模型恒归 cursor 平台。
// P1-B2「按模型平台挂组」以此为挂载判据，取代此前"配置勾选模型 ∩ 白名单"的人工勾选口径。
const cursorModelOwningPlatform = PlatformCursor

// cursorModelPlatformByVendor 把实供清单的真实厂商映射到站点的模型所属平台（U7 挂组判据）。
//
// U7 原话："入库按照模型平台入库，比如 claude 模型就入库到 claude 分组去"——挂载平台取
// 模型自身厂商，而不是账号平台。composer-2.5 归类厂商 composer 在站点没有对应平台，
// 返回空串（不参与平台组匹配，也不因此阻断其它模型的挂载）。
func cursorModelPlatformByVendor(vendor string) string {
	switch vendor {
	case CursorVendorAnthropic:
		return PlatformAnthropic
	case CursorVendorOpenAI:
		return PlatformOpenAI
	case CursorVendorGoogle:
		return PlatformGemini
	case CursorVendorXAI:
		return PlatformGrok
	case CursorVendorMuse:
		return PlatformMuse
	}
	return ""
}

// cursorMountTargetGroups 是 P1-B2 U7「按模型平台挂组」的唯一判定出口（手动挂组与自动补池
// 同源同判，避免两处口径漂移）。返回 groups 中可作为入库挂载目标的子集（保序）。
//
// 入选三条件（AND）：
//  1. 平台：分组平台必须是"模型所属平台（cursor）的目标分组"——
//     group.platform = cursor 的专属分组，或能把请求目标平台解析到 cursor 的 composite 聚合分组
//     （聚合组自身无原生模型，只有在其白名单实际声明了实供模型时才是该模型的目标分组）；
//  2. 有对应模型：白名单必须启用（enabled=true）——allow-all（enabled=false，Allows 恒真）
//     恒真判入已收敛：未启用白名单 = 无对应模型，不挂（聚合组/福利组的典型形态）；
//  3. 覆盖：白名单对静态实供清单（CursorStaticEntryModels）至少一个模型 Allows 命中
//     （沿用既有 Allows 的前缀/通配语义；仅含 "cursor" 字样、不含任何实供模型的组不挂）。
func cursorMountTargetGroups(groups []*ent.Group, entryModels []string) []*ent.Group {
	if len(entryModels) == 0 {
		return nil
	}
	vendorByID := CursorRealSupplyVendorByID()
	modelPlatforms := make(map[string]struct{}, len(entryModels))
	for _, m := range entryModels {
		if p := cursorModelPlatformByVendor(vendorByID[m]); p != "" {
			modelPlatforms[p] = struct{}{}
		}
	}
	if len(modelPlatforms) == 0 {
		return nil
	}
	out := make([]*ent.Group, 0, len(groups))
	for _, g := range groups {
		if g == nil {
			continue
		}
		platform := strings.TrimSpace(g.Platform)
		// 目标平台边界（U7）：模型真实厂商对应的原生平台组；cursor 专属组与 composite
		// 聚合组沿用既有承载语义（cursor 组按账号平台承载全量实供清单）。
		if platform != PlatformComposite && platform != PlatformCursor {
			if _, ok := modelPlatforms[platform]; !ok {
				continue // 不是任一实供模型所属平台的目标分组
			}
		}
		al := GroupModelAllowlistFromDomain(g.ModelAllowlist)
		if !al.Enabled {
			continue // allow-all（无对应模型）→ 不挂
		}
		hit := false
		for _, m := range entryModels {
			if !al.Allows(m) {
				continue
			}
			// U7：只认该模型真实厂商对应的平台组；composite 聚合层按其白名单声明算目标。
			if platform == PlatformComposite || platform == PlatformCursor || cursorModelPlatformByVendor(vendorByID[m]) == platform {
				hit = true
				break
			}
		}
		if hit {
			out = append(out, g)
		}
	}
	return out
}

// findCursorPlatformGroups 是 B1 手动挂载事实源（P1-B2 起改为「按模型平台挂组」U7 口径）。
//
// 手动挂载目标 = 【静态实供清单】∩【模型所属平台（cursor）的目标分组】∩【分组白名单实际覆盖】，
// 与自动链路 ListAllowedModels 共用 cursorMountTargetGroups（同源同判）。模型集合取自
// CursorStaticEntryModels（静态实供清单），不再取"人工勾选"——入库退出人工勾选定义能力。
//
// 零覆盖（无任何目标分组）= 无模型上下文：此时"挂到允许相应模型的现有分组"无从满足，
// 直接失败关闭（ErrQuotaKeyModelNoCoverage，走既有回滚路径，号不入库）。
func (s *QuotaKeyService) findCursorPlatformGroups(ctx context.Context, tx *ent.Tx) ([]int64, error) {
	groups, err := tx.Group.Query().
		Where(
			group.Status(domain.StatusActive),
			group.DeletedAtIsNil(),
		).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("quota_key_service: query candidate groups: %w", err)
	}
	targets := cursorMountTargetGroups(groups, CursorStaticEntryModels())
	out := make([]int64, 0, len(targets))
	for _, g := range targets {
		out = append(out, g.ID)
	}
	if len(out) == 0 {
		return nil, ErrQuotaKeyModelNoCoverage
	}
	return out, nil
}

// mountAccountToGroups 在落库事务内把账号挂入指定全部分组（account_groups M2M，默认
// priority=50，不新造表）。任一分组链接写失败即返回错误，由调用方触发回滚。
func (s *QuotaKeyService) mountAccountToGroups(ctx context.Context, tx *ent.Tx, accountID int64, groupIDs []int64) error {
	for _, gid := range groupIDs {
		if _, err := tx.AccountGroup.Create().
			SetAccountID(accountID).
			SetGroupID(gid).
			Save(ctx); err != nil {
			return fmt.Errorf("link account %d to group %d: %w", accountID, gid, err)
		}
	}
	return nil
}

// HasAccount 判定指定激活码是否已持有（未软删）账号。
//
// P1-B4 设备同源统一用其识别"存量 key"：已持久化 device_id 的码直接走同源链路，
// 未持久化但 HasAccount==true 的码按 §16 红线保持既有派生值 deviceID(code)，不写库、不迁移；
// 仅 HasAccount==false（新码）才经 DeviceIDFor 生成并落库 device_id。查询只读 accounts，不改写。
func (s *QuotaKeyService) HasAccount(ctx context.Context, quotaKeyID int64) (bool, error) {
	if s == nil || s.repo == nil {
		return false, errors.New("quota_key_service: repo not configured")
	}
	accs, err := s.repo.ListAccountsByQuotaKey(ctx, quotaKeyID)
	if err != nil {
		return false, fmt.Errorf("quota_key_service: list accounts by quota key: %w", err)
	}
	return len(accs) > 0, nil
}

// quotaKeyToStatus 将 ent 实体映射为脱敏 DTO。
func quotaKeyToStatus(qk *ent.QuotaKey) *QuotaKeyStatus {
	var lastErr string
	if qk.LastRefreshError != nil {
		lastErr = *qk.LastRefreshError
	}
	return &QuotaKeyStatus{
		ID:               qk.ID,
		Status:           qk.Status,
		Exhausted:        qk.Exhausted,
		LastRefreshAt:    qk.LastRefreshAt,
		LastRefreshError: lastErr,
		CodeValueMasked:  maskCodeValue(qk.CodeLast4),
		ExpiresAt:        qk.ExpiresAt,
	}
}

// maskCodeValue 仅保留末 4 位，其余以掩码替代，绝不回显原文。
//
// 脱敏兜底（D-CURSOR-033 #3）：当 last4 不足 4 位（非空）时，说明可用字符数不足以
// 遮蔽，输出固定全掩码而非回显原文任何片段；last4 为空时返回 "****"。
func maskCodeValue(last4 string) string {
	switch {
	case last4 == "":
		return "****"
	case len(last4) < 4:
		return "XXXX-****-****-****"
	default:
		return "XXXX-****-****-" + last4
	}
}

// last4Of 取原文末 4 位；不足 4 位（原文过短）时返回空。
//
// 关键不变量（D-CURSOR-033 #3）：原文长度 ≤ 4 时，其"末 4 位"即全文，若直接存为
// CodeLast4 再交给 maskCodeValue 会回显完整短码（触 L11/L16）。故过短原文返回空，
// 由 maskCodeValue 输出全掩码，绝不把短码原文落入任何可展示字段。
func last4Of(plain string) string {
	if len(plain) <= 4 {
		return ""
	}
	return plain[len(plain)-4:]
}

// quotaKeyMACOfPlaintext 计算原文 HMAC-SHA256 指纹（hex）。
// 使用与 account_credentials 同型的 credcrypt.MACKey() 派生密钥，
// 供 uq_quota_keys_code_mac 唯一索引做 SQL 级查重。
func quotaKeyMACOfPlaintext(plain string) string {
	sum := hmac.New(sha256.New, credcrypt.MACKey())
	sum.Write([]byte(plain))
	return hex.EncodeToString(sum.Sum(nil))
}

// isExpired 判断激活码是否已过期（expires_at 非 NULL 且早于 now）。
func isExpired(qk *ent.QuotaKey) bool {
	return qk.ExpiresAt != nil && qk.ExpiresAt.Before(time.Now())
}

// ---------------------------------------------------------------------------
// F1：生产用 QuotaKeyModelWaterProvider 实现（方案 §13.1，C11 接入点）。
// 所有 field/聚合 SQL 收敛于此，不新造表/字段；口径与调度谓词同判
//（account.platform=cursor / schedulable / status=active / 未软删 / 非 temp 窗口 /
// 当前允许 M 分组）。阈值恒为统一常量 quotaKeyWaterLevelThreshold（=10）。
// ---------------------------------------------------------------------------

// quotaKeyModelWater 是 QuotaKeyModelWaterProvider 的生产实现。
// 只读聚合，命中真实 Postgres；与 C11 收口装配一致，不碰 schema/migration。
type quotaKeyModelWater struct {
	client *ent.Client
	// cfgSrc 是"入库可配置"配置源（裁定 1 交集左操作数：配置勾选模型）。为 nil 时退化为
	// DefaultAutoEntryConfig（实供清单全集），与 production 未配置 cursor_auto_entry_config
	// 时的口径一致。
	cfgSrc AutoEntryConfigProvider

	// §18.4 W 真实路由/渠道/配额依赖：生产由 wire 在 ProvideQuotaKeyReconciler 装配点注入。
	// 三者齐备时水位走真实路由/端点/渠道/额度门；任一缺失视为装配错误，必须显式失败
	// （禁止静默退化回粗交集口径，§18.4 第11条；共享合同 §5 禁止降级分支）。
	gateway  *GatewayService
	channel  *ChannelService
	resolver *CompositeRouteResolver
}

// SetRoutingDependencies 注入 §18.4 W 所需的真实路由/渠道/网关依赖。生产装配必须提供，
// 使水位沿用现有 resolver（显式 route → 账号精确 ownership → detector）与组级渠道、账号
// 配额维度门；依赖缺失时水位显式失败（不退回粗交集），与 §18.4 第10/11条失败语义一致。
func (w *quotaKeyModelWater) SetRoutingDependencies(gw *GatewayService, ch *ChannelService, res *CompositeRouteResolver) {
	if w == nil {
		return
	}
	w.gateway = gw
	w.channel = ch
	w.resolver = res
}

// NewQuotaKeyModelWater 构造生产用模型水位提供方（F1 装配点由 wire 注入 *ent.Client）。
// 返回接口类型，使 reconciler 侧仅依赖窄面。
func NewQuotaKeyModelWater(client *ent.Client, cfgSrc AutoEntryConfigProvider) QuotaKeyModelWaterProvider {
	return &quotaKeyModelWater{client: client, cfgSrc: cfgSrc}
}

// SetConfigSource 注入配置源（wire 装配 / 测试使用）。为 nil 时退化为缺省配置。
func (w *quotaKeyModelWater) SetConfigSource(s AutoEntryConfigProvider) {
	if w != nil {
		w.cfgSrc = s
	}
}

// ListAllowedModels 枚举当前可补池的模型 M（P1-B2：按模型平台挂组口径，与手动挂组同源同判）：
// 【静态实供清单 CursorStaticEntryModels】∩【模型所属平台（cursor）的目标分组白名单实际覆盖的模型】。
//
// 交集左项不再取"人工勾选"（入库退出人工勾选定义能力），右项与 findCursorPlatformGroups 共用
// cursorMountTargetGroups：只认"模型所属平台的目标分组 + 白名单启用且 Allows(M)"——allow-all
// （无对应模型）的聚合/福利组不构成覆盖，不再让补池把号灌进对模型无声明的裸组。
//
// 空集语义（运营空态，非配置错误）：清单内模型但无目标分组覆盖 → 返回空集 + nil，reconciler
// 正常空跑停轮（沿用 CUR-F17 外审 R14 回修的两态分界：分组覆盖是运营态，不记失败心跳）。
func (w *quotaKeyModelWater) ListAllowedModels(ctx context.Context) ([]string, error) {
	if w.client == nil {
		return nil, fmt.Errorf("quota_key_model_water: ent client not configured")
	}
	groups, err := w.client.Group.Query().
		Where(
			group.Status(domain.StatusActive),
			group.DeletedAtIsNil(),
		).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("quota_key_model_water: list groups: %w", err)
	}
	entryModels := CursorStaticEntryModels()
	targets := cursorMountTargetGroups(groups, entryModels)
	seen := make(map[string]struct{}, len(entryModels))
	out := make([]string, 0, len(entryModels))
	for _, m := range entryModels {
		km := strings.ToLower(strings.TrimSpace(m))
		if km == "" {
			continue
		}
		if _, ok := seen[km]; ok {
			continue
		}
		covered := false
		for _, g := range targets {
			if GroupModelAllowlistFromDomain(g.ModelAllowlist).Allows(m) {
				covered = true
				break
			}
		}
		if !covered {
			continue
		}
		seen[km] = struct{}{}
		out = append(out, m)
	}
	return out, nil
}

// rawConfigModelIDs 返回配置中勾选的原始模型 ID（未经过实供清单门控），用于区分
// "用户真关了" 与 "勾选了但全被实供清单剔除" 两种空集语义（R8-1：CUR-F11）。
func (w *quotaKeyModelWater) rawConfigModelIDs(ctx context.Context) ([]string, error) {
	if w.cfgSrc == nil {
		return modelIDsOf(DefaultAutoEntryConfig()), nil
	}
	cfg, err := w.cfgSrc.GetConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("quota_key_model_water: load auto entry config: %w", err)
	}
	return modelIDsOf(cfg), nil
}

// modelIDsOf 抽取配置中勾选的模型 ID（忽略空 ID），供 ListAllowedModels 区分空集语义。
func modelIDsOf(cfg *CursorAutoEntryConfig) []string {
	if cfg == nil {
		return nil
	}
	ids := make([]string, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// WaterLevel 返回 Cursor 可服务账号的去重总量（方案 §15.6 / §16.1）。
// 账号可调度性和模型能力复用调度谓词，已挂组白名单必须允许账号实际支持的模型。
// key exhausted 仅代表拉号受阻，不剥夺已有账号供给；只有已知过期停止供给。
// modelM 不收窄池级总量；并发占用不参与计数，统一目标仍为 10。
func (w *quotaKeyModelWater) WaterLevel(ctx context.Context, _ string) (int, int, error) {
	accounts, err := w.client.Account.Query().
		Where(
			account.Platform(domain.PlatformCursor),
			account.Schedulable(true),
			account.Status(domain.StatusActive),
			account.DeletedAtIsNil(),
		).
		WithGroups(func(q *ent.GroupQuery) {
			q.Where(group.Status(domain.StatusActive), group.DeletedAtIsNil())
		}).
		WithQuotaKey().
		All(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("quota_key_model_water: list total water level accounts: %w", err)
	}
	current := 0
	for _, row := range accounts {
		// 必须走 w（已注入路由/渠道/网关依赖）的判定，而非包级 cursorAccountCountsTowardWaterLevel
		// （后者为兼容旧单测用空结构退化为粗交集口径，不会携带生产路由依赖）。
		eligible, err := w.countsTowardWaterLevel(ctx, row)
		if err != nil {
			return 0, 0, err
		}
		if eligible {
			current++
		}
	}
	return current, quotaKeyWaterLevelThreshold, nil
}

// cursorAccountCountsTowardWaterLevel 保留既有包级签名作为历史取证/测试入口：它直接走
// w（空结构，未注入路由依赖）的判定——依赖缺失时按 §18.4 第11条显式失败，不再静默退化回
// 粗交集口径。生产路径通过 w 注入 resolver/ChannelService/GatewayService 走真实
// 路由/端点/渠道/额度门；本入口不是生产 WaterLevel 链路的一部分。
func cursorAccountCountsTowardWaterLevel(ctx context.Context, row *ent.Account) (bool, error) {
	return (&quotaKeyModelWater{}).countsTowardWaterLevel(ctx, row)
}

// countsTowardWaterLevel 是 §18.4 W 的统一入口：沿用既有候选域，对每个具体候选模型 × 显式组
// × 四个文本端点逐一复用真实路由/端点/渠道/账号额度门；任一允许则账号只计一次，全部拒绝则不计。
// 读库/路由/渠道基础设施错误返回整次 WaterLevel 错误，不返回部分数。
func (w *quotaKeyModelWater) countsTowardWaterLevel(ctx context.Context, row *ent.Account) (bool, error) {
	if row == nil || row.Platform != PlatformCursor || row.DeletedAt != nil {
		return false, nil
	}
	// model_mapping 不属于敏感加密子键；只投影调度谓词消费的字段，不解密/读取鉴权材料。
	candidate := &Account{
		Platform: row.Platform, Type: row.Type, Status: row.Status, Schedulable: row.Schedulable,
		Credentials: row.Credentials, Extra: row.Extra, ExpiresAt: row.ExpiresAt, AutoPauseOnExpired: row.AutoPauseOnExpired,
		OverloadUntil: row.OverloadUntil, RateLimitResetAt: row.RateLimitResetAt,
		TempUnschedulableUntil: row.TempUnschedulableUntil, QuotaKeyID: row.QuotaKeyID,
	}
	if !candidate.IsSchedulable() || len(candidate.GetModelMapping()) == 0 {
		return false, nil
	}
	key := row.Edges.QuotaKey
	if key == nil || key.DeletedAt != nil {
		return false, fmt.Errorf("quota_key_model_water: account %d quota key unavailable", row.ID)
	}
	if quotaKeyStatusBlocksCursorScheduling(key.Status) || isExpired(key) {
		return false, nil
	}
	// key 终态门（I2）：归属激活码不可服务 → 不剥夺已有账号供给，仅排除该号。
	if reason := cursorAccountQuotaKeyVetoReason(ctx, candidate); reason != "" {
		return false, nil
	}

	models := w.cursorCandidateModels(candidate)

	// §18.4 第11条：禁止可选依赖导致静默跳过路由。依赖齐备时走真实路由/渠道/额度门；
	// 依赖缺失是装配错误，必须显式失败（WaterLevel 整次返回错误，reconciler 记失败心跳并
	// 停轮，与第10条失败语义一致），不得退回粗交集（§18.2 W 行认定的计数 bug 本体——没有
	// 路由/端点/渠道/额度门会重新把"全部文本端点显式改向 OpenAI、真实平台门已排除 Cursor"
	// 的账号计为 1，使站点以为不缺号、实际调度不到）。共享合同 §5 亦禁止兜底/降级分支。
	if err := w.routingDependenciesReady(); err != nil {
		return false, err
	}
	return w.countsTowardWaterLevelRouted(ctx, candidate, row, models)
}

// routingDependenciesReady 报告 §18.4 W 所需的真实路由/渠道/网关依赖是否齐备。
// 三者任一缺失即视为装配错误：水位必须显式失败，禁止静默退化回粗交集口径
// （§18.4 第11条；共享合同 §5 禁止降级分支会掩盖真实错误）。
// 生产装配点在 cmd/server/wire_gen.go:379（ProvideQuotaKeyReconciler 注入
// gatewayService / channelService / compositeRouteResolver），正常装配下三者齐备。
func (w *quotaKeyModelWater) routingDependenciesReady() error {
	switch {
	case w.resolver == nil:
		return fmt.Errorf("quota_key_model_water: routing dependency not wired: resolver (*CompositeRouteResolver) is nil; production wiring at cmd/server/wire_gen.go:379")
	case w.channel == nil:
		return fmt.Errorf("quota_key_model_water: routing dependency not wired: channel (*ChannelService) is nil; production wiring at cmd/server/wire_gen.go:379")
	case w.gateway == nil:
		return fmt.Errorf("quota_key_model_water: routing dependency not wired: gateway (*GatewayService) is nil; production wiring at cmd/server/wire_gen.go:379")
	}
	return nil
}

// cursorCandidateModels 返回水位候选模型集：静态实供清单 + 账号 model_mapping 的精确键。
// 精确键 = 不以 "*" 结尾（§18.4.1）——中间字面星号（如 gpt-*-high）必须保留，不能因
// Contains("*") 误丢；通配键仍由 IsModelSupported/GetMappedModel 按既有规则匹配具体模型名。
// 候选域沿用现有代码，不实现无限字符串求解器、不新增"未知就停补"的能力限制。
func (w *quotaKeyModelWater) cursorCandidateModels(candidate *Account) []string {
	seen := make(map[string]struct{}, len(CursorStaticEntryModels()))
	out := make([]string, 0, len(CursorStaticEntryModels())+len(candidate.GetModelMapping()))
	add := func(m string) {
		m = strings.TrimSpace(m)
		if m == "" {
			return
		}
		key := strings.ToLower(m)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, m)
	}
	for _, m := range CursorStaticEntryModels() {
		add(m)
	}
	for model := range candidate.GetModelMapping() {
		if strings.HasSuffix(model, "*") {
			continue // 通配键：保留既有"具体模型名 + 通配映射"匹配，不进精确候选集
		}
		add(model)
	}
	return out
}

// countsTowardWaterLevelRouted §18.4 真实门：对每个具体候选模型 × 显式组 × 四个文本端点，
// 从干净 ctx 各自解析路由，复用现有 resolver（显式 route → 账号精确 ownership → detector），
// 再经平台/端点准入、组级渠道（返回 error 的内部读入口）、账号配额维度门。
// 任一允许 → 账号只计一次；全部拒绝 → 不计；基础设施错误 → 整次错误（不返回部分数）。
func (w *quotaKeyModelWater) countsTowardWaterLevelRouted(ctx context.Context, candidate *Account, row *ent.Account, models []string) (bool, error) {
	endpoints := []string{
		CompositeRouteEndpointChatCompletions,
		CompositeRouteEndpointMessages,
		CompositeRouteEndpointResponses,
		CompositeRouteEndpointGemini,
	}
	for _, model := range models {
		// 账号模型支持（候选域已含精确键；通配键经 IsModelSupported 匹配）。
		if !candidate.IsModelSupported(model) {
			continue
		}
		if _, ok := candidate.ResolveMappedModel(model); !ok {
			// 非空账号映射目标缺失 → 真实请求无法落上游，不计。
			continue
		}
		for _, g := range row.Edges.Groups {
			if g == nil || g.DeletedAt != nil || g.Status != StatusActive {
				continue
			}
			if g.Platform != PlatformCursor && g.Platform != PlatformComposite && !cursorNativeBridgePlatforms(g.Platform) {
				continue
			}
			gid := g.ID
			allowlist := GroupModelAllowlistFromDomain(g.ModelAllowlist)
			if !allowlist.Allows(model) {
				continue
			}
			for _, endpoint := range endpoints {
				// 每个组合从干净 ctx 开始，不继承上一组合的 target/channel stamp。
				decision, defaulted, err := w.resolveCompositeRoute(ctx, gid, model, endpoint)
				if err != nil {
					return false, fmt.Errorf("quota_key_model_water: resolve route (group %d, model %q, endpoint %q): %w", gid, model, endpoint, err)
				}
				target, reachable := effectiveTargetForEndpoint(decision, defaulted, endpoint)
				if !reachable {
					continue // JSON 三端点未匹配仍不可达；gemini 未匹配仍未 default
				}
				// 显式 route 把请求改向 Cursor 不可服务的平台（如 OpenAI）→ 该组合不计入。
				// 仅当未显式改向（detector/ownership 或 gemini default 补 Gemini）才沿用既有准入。
				if decision.Source == CompositeRouteSourceExplicit && !cursorServableTarget(target) {
					continue
				}
				// 第8条：OpenAI 兼容 Messages 入口的组 dispatch 改写后的真实调度名，
				// 水位与 handler 共用 ResolveOpenAIMessagesDispatchMappedModel 判定（不假造请求级 tier/body）。
				// 其余端点真实调度名即请求模型本身。
				effectiveModel := model
				if endpoint == CompositeRouteEndpointMessages {
					sg := &Group{Platform: g.Platform, MessagesDispatchModelConfig: g.MessagesDispatchModelConfig}
					if dm := ResolveOpenAIMessagesDispatchMappedModel(sg, target, model); dm != "" {
						effectiveModel = dm
					}
				}
				if !candidate.IsModelSupported(effectiveModel) {
					continue
				}
				if _, ok := candidate.ResolveMappedModel(effectiveModel); !ok {
					continue
				}
				if !candidate.IsSchedulableForModelWithContext(ctx, effectiveModel) {
					continue
				}
				// 组级渠道：读取失败 ≠ 无渠道，必须返回错误而非误判。
				lk, err := w.channel.lookupGroupChannel(ctx, gid)
				if err != nil {
					return false, fmt.Errorf("quota_key_model_water: lookup group channel (group %d): %w", gid, err)
				}
				// 渠道改写/限制：按当前 billing source 消费正确的名字（§18.4.5/7）。
				channelMapped := model
				accountMapped := model
				if m := candidate.GetMappedModel(model); m != "" {
					accountMapped = m
				}
				restrictionModel := model
				if lk != nil {
					cm := resolveMapping(lk, gid, model)
					if cm.Mapped {
						channelMapped = cm.MappedModel
					}
					switch cm.BillingModelSource {
					case BillingModelSourceRequested:
						restrictionModel = model
					case BillingModelSourceUpstream:
						restrictionModel = accountMapped
					case BillingModelSourceResponse, BillingModelSourceChannelMapped:
						restrictionModel = channelMapped
					default:
						restrictionModel = channelMapped
					}
					if lk.channel.RestrictModels && checkRestricted(lk, gid, restrictionModel) {
						continue
					}
				}
				// 账号配额维度门：只消费 scope=account 维度，不把 key 余额复制成账号额度。
				if EvaluateAccountQuotaDimensionGate(ResolveAccountQuotaDimensions(candidate, time.Now())) != QuotaDimensionGateAllow {
					continue
				}
				// 命中任一合法组合 → 同号只计一次。
				return true, nil
			}
		}
	}
	return false, nil
}

// resolveCompositeRoute 统一解析各端点的路由；gemini 端点走薄共用方法（未匹配补 Gemini、
// 不伪造 Matched/ownership），JSON 三端点走原 resolver.Resolve。
func (w *quotaKeyModelWater) resolveCompositeRoute(ctx context.Context, groupID int64, model, endpoint string) (CompositeRouteDecision, bool, error) {
	if endpoint == CompositeRouteEndpointGemini {
		return w.resolver.ResolveGeminiWithDefault(ctx, groupID, model)
	}
	decision, err := w.resolver.Resolve(ctx, groupID, model, endpoint)
	return decision, false, err
}

// effectiveTargetForEndpoint 从决策中抽取真实上游目标平台与可达性。
// JSON 三端点：未匹配 → 不可达；gemini 端点：匹配用其 target，未匹配但 defaulted 仅补 Gemini target。
func effectiveTargetForEndpoint(decision CompositeRouteDecision, defaulted bool, endpoint string) (string, bool) {
	if endpoint == CompositeRouteEndpointGemini {
		if decision.Matched {
			return decision.TargetPlatform, true
		}
		if defaulted {
			return PlatformGemini, true
		}
		return "", false
	}
	if !decision.Matched {
		return "", false
	}
	return decision.TargetPlatform, true
}

// cursorServableTarget 判定显式 route 的目标平台是否 Cursor 账号可服务：
// 仅 cursor 自身与其原生桥接平台（Anthropic/Gemini）。OpenAI/Grok/CN 等平台 Cursor 不借
// CodeBuddy 特例越过平台门，故不算可服务（§18.4.4）。
func cursorServableTarget(target string) bool {
	return target == PlatformCursor || cursorNativeBridgePlatforms(target)
}

// isCooldownRefreshError 判定该 key 的 last_refresh_error 是否为【冷却】类错误。
// 判据是错误分类（错误链是否包 ErrUpstreamCooldown），而非时间：冷却错误由 writeCooldownMarker
// 以 fmt.Errorf("%w: ...", ErrUpstreamCooldown, ...) 写入，故以哨兵前缀识别。非冷却错误
// （exhausted / 插入失败 / 未知结果）即使时间久远也不得被当成限频而重新入选。
func isCooldownRefreshError(errStr *string) bool {
	if errStr == nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(*errStr), ErrUpstreamCooldown.Error())
}

// ListCandidateKeys 返回池级（不按模型收窄）需要补池的候选 quota_key ID：
// active + 非 exhausted + 未过期 + 未软删 + 有剩余容量（working < MaxAccountsPerQuotaKey）。
// limit>0 时由实现方按需截断（reconciler 传入的是总量缺口）；模型 M 不收窄候选集——
// 挂载语义由 PullAccountsForModel 决定目标分组。
//
// P1-D2 总量制：缺号驱动由"cursor 可服务账号总量 < 10"触发，候选集与具体模型无关，
// 故此处对全部有效码统一预检后返回（单码语义不变：只按 key 自身状态过滤、不做跨模型展开；
// 每码 MaxAccountsPerQuotaKey=15 上限与非幂等保护均保持不变）。
//
// 裁定 3 冷却规则（last_refresh_error IS NULL 这一永久排除已删除，不得换皮重演）：
//   - 冷却类错误（last_refresh_error 包 ErrUpstreamCooldown）且距 last_refresh_at 不足
//     quotaKeyCooldownWindow（5min，shareai 实测窗口）→ 跳过（仍在冷却）；
//   - 冷却类错误但窗口已过 → 自动重新入选（不等人工 clear）；
//   - 非冷却错误（未知/普通上游失败）只写 last_refresh_error、不动 exhausted
//     （CUR-P1-FIX-KEY-EXHAUSTED：未知失败不得推导停供），故该类 key 仍留在候选集内，
//     其时间流逝不产生任何限频豁免（不进入冷却窗口判定，窗口仅认冷却类错误）；
//     真正被上方查询排除的是两类已确认事实：status 已非 active（含上游确认过期的 expired）
//     与插入失败标记的 exhausted=true。
func (w *quotaKeyModelWater) ListCandidateKeys(ctx context.Context, _ string, limit int) ([]int64, error) {
	now := time.Now()
	keys, err := w.client.QuotaKey.Query().
		Where(
			quotakey.Status(domain.QuotaKeyStatusActive),
			quotakey.Exhausted(false),
			quotakey.Or(quotakey.ExpiresAtIsNil(), quotakey.ExpiresAtGT(now)),
			quotakey.DeletedAtIsNil(),
		).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("quota_key_model_water: list candidate keys: %w", err)
	}
	out := make([]int64, 0, len(keys))
	for _, k := range keys {
		if isCooldownRefreshError(k.LastRefreshError) {
			if k.LastRefreshAt != nil && now.Sub(*k.LastRefreshAt) < quotaKeyCooldownWindow {
				continue // 冷却窗口内：跳过，不进入候选
			}
			// 冷却窗口已过：自动重新入选（fall through）。
		}
		working, werr := w.client.Account.Query().
			Where(account.QuotaKeyID(k.ID), account.DeletedAtIsNil()).
			Count(ctx)
		if werr != nil {
			return nil, fmt.Errorf("quota_key_model_water: count working for key %d: %w", k.ID, werr)
		}
		if working >= MaxAccountsPerQuotaKey {
			continue // 无剩余容量：working 已满 15，跳过
		}
		out = append(out, k.ID)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// quotaKeyAdvisoryLockID 计算 advisory 锁的 int64 键（与 Pull/Refresh/Delete 共享同一互斥域）。
// 对 quota_key_id 做稳定 fnv 哈希，保证同一 key 映射到同一锁、不同 key 互不阻塞。
func quotaKeyAdvisoryLockID(quotaKeyID int64) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(strconv.FormatInt(quotaKeyID, 10)))
	return int64(h.Sum64())
}

// acquireAdvisoryXactLock 在事务内获取 pg_try_advisory_xact_lock（事务级，提交/回滚自动释放）。
// 非阻塞 try；未获取则短暂退避重试，实现同 key 串行互斥。
func acquireAdvisoryXactLock(ctx context.Context, tx *ent.Tx, lockID int64) error {
	for {
		rows, err := tx.QueryContext(ctx, "SELECT pg_try_advisory_xact_lock($1)", lockID)
		if err != nil {
			return fmt.Errorf("acquire advisory xact lock: %w", err)
		}
		var acquired bool
		if !rows.Next() {
			_ = rows.Close()
			return fmt.Errorf("acquire advisory xact lock: no row returned")
		}
		if err := rows.Scan(&acquired); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan advisory xact lock: %w", err)
		}
		_ = rows.Close()
		if acquired {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("acquire advisory xact lock: %w", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}
