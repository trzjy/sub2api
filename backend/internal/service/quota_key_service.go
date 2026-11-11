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
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/pkg/credcrypt"
)

// MaxAccountsPerQuotaKey 是单个激活码可持有（working）账号的**硬上限**（人工与自动共用）。
//
// 2026-11-10 用户裁定：自动补位固定 2 个（1 主 1 热备），人工拉取不受限制，存量超出的裁掉。
// 所以这个常量现在只作为"人工也不能无限堆"的兜底护栏，不再决定自动补位目标——
// 自动补位的目标值见 CursorAutoPullAccountsPerKey。
//
// 历史：原值 15（把「上游每日 5 次拉号额度」和「耗尽账号」混在一个桶里导致死锁），
// 中途调到 50 用于观察，现在回落到 50 只作护栏。
const MaxAccountsPerQuotaKey = 50

// CursorAutoPullAccountsPerKey 是**自动补位**的每码固定目标账号数（用户裁定 2026-11-10）。
//
// 1 主 1 热备：实测单号并发峰值 1，2 个号（1 主 1 热备）即可满足调度；多号只增加管理面
// 与失败轮转面，不增加真实吞吐，故从 5 收敛为 2（用户裁定 2026-11-10）。
//
// 历史：上游每个激活码每天只允许拉 5 次号（ErrUpstreamDailyRefreshLimit），曾定 5 刚好用尽
// 当天额度；2026-11-10 重新裁定为 2。人工拉取（后台手动建号/改绑）不走这个限制，因为人是有
// 意为之，不需要替他省额度。
//
// 为什么不再看"水位缺口"：之前按额度水位算缺口，会在额度满的时候一个都不拉、额度紧张时
// 一次拉一堆，账号数忽多忽少。固定目标后，每码每天稳定消耗固定个号，
// 「400 分到底要几个号」这个账才能算清楚。
const CursorAutoPullAccountsPerKey = 2

// quotaKeyCooldownWindow 是 shareai 上游实测的刷新冷却窗口（方案 §10 #26）。
//
// 注意（2026-11-06 起）：该窗口**不再参与补池候选过滤**——用户裁定拉号只看号数
// （少于 5 个就拉），冷却窗口属于"推断出来的状态"，不得到此为止挡住号不够的码。
// 常量保留用于错误分类与展示判定（日志分级、看板区分限流与真失败）。
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
	// UpdateMeta 在**调用方已开启的事务内**更新请求中显式出现的 expires_at / notes，
	// 不触碰 code / status。外层指针 nil 表示省略，内层指针 nil 表示显式清空，否则设置为给定值。
	//
	// tx 必填：生产元数据写入只在管理员编辑的同 key 事务（withQuotaKeyLockTx）内发生，
	// 不得绕回默认 client 走锁外独立提交——否则管理员的日期编辑可越过来源观测的同 key 串行边界
	// （§25.14-C / §25.17.6）。仓储实现在 tx 为 nil 时失败关闭，不退化为锁外写。
	UpdateMeta(ctx context.Context, tx *ent.Tx, id int64, expiresAt **time.Time, notes **string) error
	// UpdatePriority 在**调用方已开启的事务内**改写激活码优先级，并同步该码名下未软删账号
	// 的 accounts.priority（"码的优先级 = 码下账号的优先级"，用户裁定 2026-11-03）。
	// 只改 priority 一列；返回同步的账号行数。tx 必填（元数据编辑与拉号共用同一 per-key
	// 串行边界，不得绕回默认 client 锁外独立提交）。
	UpdatePriority(ctx context.Context, tx *ent.Tx, id int64, priority int) (int, error)
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
	// SoftDeleteCursorAccountsByExpiredQuotaKey 在调用方已开启的事务内，对「数据库当前仍到期」的
	// 激活码（status='expired' AND expires_at IS NOT NULL AND expires_at < now）软删其
	// platform='cursor' 且未软删的账号，返回本次实际被软删的账号 ID（供调用方在同一事务内写
	// scheduler outbox）。激活码条件不满足 / 行不存在 / 已软删 → 返回空集合，不改任何账号行。
	// 与人工删除口径（SoftDeleteAccountsByQuotaKey，覆盖全部平台）严格区分：本方法只覆盖 cursor，
	// 只由「到期」触发；不物理删除、不触碰 usage_logs；tx 必填（nil 时失败关闭）。
	SoftDeleteCursorAccountsByExpiredQuotaKey(ctx context.Context, tx *ent.Tx, quotaKeyID int64, now time.Time) ([]int64, error)
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
	// 注意：本方法走全局连接池，供"非拉号事务"路径（出站鉴权、admin 读取后写入）使用。
	// O01：首次拉号首写已前置到容量事务之外（pull 经 DeviceIDFor 走本方法，无行锁、
	// 独立提交），故不再需要事务内版本——事务内更新同一行会等待自身持有的行锁 → 自死锁。
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
	// RealSupplyModelsFor 返回指定上游标识自报的实供模型列表（恒等映射用）。
	// 上游未自报（返回空切片）时，调用方回落到 CursorStaticEntryModels()；
	// 未知上游标识返回 error（失败关闭，绝不静默按 shareai 处理）。
	RealSupplyModelsFor(upstreamKey string) ([]string, error)
	// TargetAccountsPerKeyFor 返回指定上游标识每张激活码的目标账号数（自动补池口径）。
	// 未知上游标识返回 error（失败关闭），绝不静默回落到 shareai 的 5。
	TargetAccountsPerKeyFor(upstreamKey string) (int, error)
}

// QuotaUpstreamRegistry 是上游注册表（quota_upstreams）的窄依赖：仅用于创建激活码时
// 校验上游标识是否存在且未软删（防止 kn-cy 卡密被当成 shareai 拉号，正是本卡要消灭的故障）。
// 由 repository 包的 *quotaUpstreamRepository 实现（service 包不反向导入 repository）。
type QuotaUpstreamRegistry interface {
	// ExistsByKey 报告给定上游稳定标识 key 是否存在且未软删。
	// 找不到（含软删）返回 false, nil；仅有 DB 错误才返回非 nil error。
	ExistsByKey(ctx context.Context, key string) (bool, error)
}

// upstreamKey 是 quota_keys.upstream_key 的取值集合（激活码所属上行标识）。
//
// 与 KC-16 新建的 quota_upstreams 表中 type 列的取值集合保持一致：
//   - shareai：既有 ShareAI 上游（默认，存量激活码经 NOT NULL DEFAULT 'shareai' 落此值）；
//   - kn_cy：kn-cy.com 上游（K7 第二上游，与 ShareAI 激活码并列走同一套号池/挂组/额度/停调）。
//
// 取值由迁移 297 的 CHECK 约束限定，不得在别处另起一套字符串字面量——
// 这里集中定义供分派器（upstreamDispatchingClient）按 qk.UpstreamKey 选择具体实现。
const (
	// upstreamKeyShareAI 是默认上行标识（quota_keys.upstream_key 的 NOT NULL DEFAULT）。
	upstreamKeyShareAI = "shareai"
	// upstreamKeyKnCy 是 kn-cy.com 上行标识。
	upstreamKeyKnCy = "kn_cy"
)

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
	// UpstreamDailyRefreshLimit 每日刷新次数用尽（可预期节流，次日重置后自动恢复）。
	//
	// 与 UpstreamCooldown 的区别在恢复窗口：冷却是分钟级，每日上限是天级
	// （shareai 每 key 每日 5 次，8:00 重置）。两者都不改变 exhausted 维度
	// （都不是已确认终态），但都不应被自动链路记为「任务失败」——
	// 生产实证 2026-10-27：quota_key_reconcile 周期 5 分钟必然撞上该上限，
	// 此前落 UpstreamUnknown 导致 ops jobScore 长期被扣并掩盖真故障。
	UpstreamDailyRefreshLimit
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
	case UpstreamDailyRefreshLimit:
		return errors.Is(target, ErrUpstreamDailyRefreshLimit)
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

// newUpstreamDailyRefreshLimitError 构造「当日刷新次数用尽」错误（可预期节流）。
// 走冷却标记路径（writeCooldownMarker），exhausted 维度不动 —— 每日节流不是终态。
func newUpstreamDailyRefreshLimitError(message string) error {
	return &UpstreamPullError{Kind: UpstreamDailyRefreshLimit, message: message, cause: fmt.Errorf("%w: %s", ErrUpstreamDailyRefreshLimit, message)}
}

// isUpstreamDailyRefreshLimitMessage 判定上游 msg 是否表达「当日刷新次数已达上限」。
//
// 只匹配肯定措辞，杜绝否定句误判（如「未达到刷新次数上限」不是节流）。
// 覆盖上游已知措辞：刷新次数达到上限 / 刷新次数上限 / 每日刷新次数 / 当日刷新次数 /
// 今日刷新次数 + 达到上限/已达上限/用尽 的组合。
// 绝不据 msg 反推或伪造任何日期或重置时刻。
func isUpstreamDailyRefreshLimitMessage(m string) bool {
	if m == "" {
		return false
	}
	// 否定句保护：出现否定词即不判节流。
	for _, neg := range []string{"未达到", "未达", "没有达到", "未用尽", "未超"} {
		if strings.Contains(m, neg) {
			return false
		}
	}
	// 计数/额度语境 + 达到上限语义，两者同时命中才算每日刷新上限。
	hasCount := strings.Contains(m, "刷新次数") || strings.Contains(m, "刷新额度") ||
		strings.Contains(m, "当日刷新") || strings.Contains(m, "今日刷新") ||
		strings.Contains(m, "每日刷新")
	if !hasCount {
		return false
	}
	return strings.Contains(m, "达到上限") || strings.Contains(m, "已达上限") ||
		strings.Contains(m, "用尽") || strings.Contains(m, "超出") ||
		strings.Contains(m, "上限")
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

// RealSupplyModelsFor 返回"未接入上游"错误：未接入时无法自报实供模型，失败关闭。
func (NotConnectedUpstreamClient) RealSupplyModelsFor(_ string) ([]string, error) {
	return nil, ErrUpstreamNotConnected
}

// TargetAccountsPerKeyFor 返回"未接入上游"错误：未接入时无法确定每码目标账号数，失败关闭。
func (NotConnectedUpstreamClient) TargetAccountsPerKeyFor(_ string) (int, error) {
	return 0, ErrUpstreamNotConnected
}

// upstreamDispatchingClient 实现 QuotaKeyUpstreamClient：按 quota_key 的 upstream_key
// 分派到对应的具体上游客户端（ShareAI / kn-cy），而非单一上游。
//
// 设计要点（方案 §/ KC-17）：
//   - 持有两个具体实现，PullBatch 仅做"按上游标识选路"这一件事，不引入任何额外行为，
//     既有的 shareAI / knCy 客户端行为零变化。
//   - 未知上游标识**失败关闭**：返回 error，绝不静默回落到任一已知实现。
//     回落会让一张 kn-cy 卡密去打 ShareAI 上游，凭据泄露 + 白耗额度。
//   - 空标识（qk.UpstreamKey == ""，理论上不该出现：字段 NOT NULL DEFAULT 'shareai'）
//     同样按未知处理 = 失败关闭。默认值是"入库时"的行为，运行时读取到空串说明数据已
//     偏离契约（要么直接构造的测试对象漏设，要么 DB 行被异常改写），应暴露而非静默走错链。
//
// 装配在 wire（KC-17B）里完成：先构造 shareAI 客户端（不注入 resolver），构造
// QuotaKeyService 后再回填 resolver（打破循环依赖），最后把 shareAI + knCy 两个具体
// 实现注入本分派器。本类型不负责那套装配顺序，只保证"选路 + 失败关闭"语义正确。
type upstreamDispatchingClient struct {
	shareAI QuotaKeyUpstreamClient
	knCy    QuotaKeyUpstreamClient
}

// NewUpstreamDispatchingClient 构造分派器。两个具体客户端均不可为 nil——
// 缺任一即失败关闭（分派器本身无法兜底，回落会打错上游）。
//
// KC-17B 的 wire 提供者会读这个构造器：把 NewShareAIUpstreamClient 与
// NewKnCyQuotaKeyUpstreamClient 的产物传进来，得到 QuotaKeyUpstreamClient 后传给
// NewQuotaKeyService。
func NewUpstreamDispatchingClient(shareAI, knCy QuotaKeyUpstreamClient) (*upstreamDispatchingClient, error) {
	if shareAI == nil {
		return nil, errors.New("quota_key_service: shareai upstream client must not be nil")
	}
	if knCy == nil {
		return nil, errors.New("quota_key_service: kn-cy upstream client must not be nil")
	}
	return &upstreamDispatchingClient{shareAI: shareAI, knCy: knCy}, nil
}

// PullBatch 按 qk.UpstreamKey 选择具体上游实现。
func (c *upstreamDispatchingClient) PullBatch(ctx context.Context, qk *ent.QuotaKey) (*UpstreamPullResult, error) {
	switch qk.UpstreamKey {
	case upstreamKeyShareAI:
		return c.shareAI.PullBatch(ctx, qk)
	case upstreamKeyKnCy:
		return c.knCy.PullBatch(ctx, qk)
	default:
		// 未知上游标识 = 失败关闭：返回 error，绝不静默回落到 shareai/knCy。
		// （回落会让一张 kn-cy 卡密去打 ShareAI 上游，凭据泄露 + 白耗额度。）
		return nil, fmt.Errorf("quota_key_service: unknown upstream key %q", qk.UpstreamKey)
	}
}

// RealSupplyModelsFor 按上游标识分派到对应具体上游实现，取其实供模型清单。
// 未知上游标识失败关闭（Return error），绝不静默回落到任一已知实现。
func (c *upstreamDispatchingClient) RealSupplyModelsFor(key string) ([]string, error) {
	switch key {
	case upstreamKeyShareAI:
		return c.shareAI.RealSupplyModelsFor(key)
	case upstreamKeyKnCy:
		return c.knCy.RealSupplyModelsFor(key)
	default:
		return nil, fmt.Errorf("quota_key_service: unknown upstream key %q", key)
	}
}

// TargetAccountsPerKeyFor 按上游标识分派到对应具体上游实现，取其每码目标账号数。
// 未知上游标识失败关闭（Return error），绝不静默按 shareai 的 5 处理。
func (c *upstreamDispatchingClient) TargetAccountsPerKeyFor(key string) (int, error) {
	switch key {
	case upstreamKeyShareAI:
		return c.shareAI.TargetAccountsPerKeyFor(key)
	case upstreamKeyKnCy:
		return c.knCy.TargetAccountsPerKeyFor(key)
	default:
		return 0, fmt.Errorf("quota_key_service: unknown upstream key %q", key)
	}
}

// ErrQuotaKeyNotFound 表示激活码不存在（或已软删，普通查询不可见）。
var ErrQuotaKeyNotFound = errors.New("quota key not found")

// ErrQuotaKeyDuplicateCode 表示该激活码原文已存在一条未软删的记录（code_mac 唯一冲突）。
// 由仓储层在 Create 撞 uq_quota_keys_code_mac 时返回，handler 映射为 409 + 明确文案。
var ErrQuotaKeyDuplicateCode = errors.New("quota key code already exists")

// ErrQuotaKeyUnknownUpstream 在创建激活码时上游标识校验失败（quota_upstreams 中不存在
// 或已软删）时返回；调用方据以失败关闭，绝不静默落成默认 shareai（否则 kn-cy 卡密会被
// 当 shareai 拉号，正是本卡要消灭的故障）。
var ErrQuotaKeyUnknownUpstream = errors.New("quota_key_service: unknown upstream key")

// QuotaKeyService 提供激活码凭据加密落库/读取 + C3.2 拉号状态机 + 软删编排。
type QuotaKeyService struct {
	repo     QuotaKeyRepository
	enc      QuotaKeyEncryptor
	upstream QuotaKeyUpstreamClient
	client   *ent.Client
	// cursorGroupResolver 解析挂载目标分组 ID（U7 挂载事实源，§20.2-A）。同时服务两个入口：
	// 手动 RefreshAccounts 与自动补池 PullAccountsForModel（reconciler）——两者同源同判，
	// 挂载目标 = 【静态实供清单 CursorStaticEntryModels】∩【模型所属平台的目标分组】∩【白名单
	// 实际覆盖】（findCursorPlatformGroups → cursorMountTargetGroups U7 口径：模型真实厂商
	// 原生平台组 ∪ cursor 专属 ∪ composite，allow-all（无对应模型）不挂）。
	// 零目标组 → ErrQuotaKeyModelNoCoverage 失败关闭（号不入库）。
	// 生产默认走 findCursorPlatformGroups；测试可替换以注入边界。
	cursorGroupResolver func(ctx context.Context, tx *ent.Tx) ([]int64, error)

	// insertFailureMarkerWriter 是 R5-1 插入失败后"独立事务补写 last_refresh_error 标记"
	// 的可替换执行点（与生产默认 writeInsertFailureMarker 同签名）。测试可替换以注入
	// 标记写失败（R5-1 b：仅本轮哨兵停轮的边界），与生产 cursorGroupResolver 同一类测试 seams。
	insertFailureMarkerWriter func(ctx context.Context, quotaKeyID int64, cause error) error

	// reconciler 是软删后水位快路径唤醒目标（C12-v2）。通过 SetReconciler 注入（wire 装配），
	// 软删事务提交成功后调用其 NotifyAccountRemoved 触发 reconciler 重判水位；nil 时跳过
	// （未装配则不触发快路径，退回 C10 周期补池）。
	reconciler quotaKeyReconcilerNotifier

	// autoEntry 是 D2-a 入库可配置提供方（窄接口）：挂组前调用 ApplyEntryDefaults 写账号属性，
	// 并对外提供配置读取。nil 时跳过（向后兼容 D1 既有测试与未装配场景）。
	autoEntry AutoEntryConfigProvider

	// registry 是上游注册表（quota_upstreams）的窄依赖：创建激活码时校验上游标识
	// 是否存在且未软删，防止 kn-cy 卡密被当 shareai 拉号（本卡要消灭的故障）。
	// nil 时 Create 对非空 upstream_key 失败关闭（绝不静默落成默认 shareai）。
	// 生产由 wire 在 ProvideQuotaKeyService 装配点注入 *quotaUpstreamRepository；
	// 测试可注入替身或留 nil（nil 仅允许空 upstream_key 的创建路径）。
	registry QuotaUpstreamRegistry

	// runMode 是运行模式（config.RunModeSimple / config.RunModeStandard），用于分组绑定策略：
	// Simple 模式下 composite 分组不可绑定（复用 IsGroupBindableInSimpleMode 口径）。
	// 生产由 wire 在 ProvideQuotaKeyService 装配点经 SetRunMode 注入；测试可注入替身。
	// 空串视为 standard（composite 可绑定），与 cfg == nil 时的既有降级口径一致。
	runMode string

	// authCache 是分组认证缓存失效的窄依赖（APIKeyService 实现）；
	// 创建/更新路径提交成功后逐个失效受影响分组的认证缓存。
	// 未装配（nil）时跳过，不阻断主流程（登记为残余风险）。
	authCache quotaKeyAuthCacheInvalidator
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

// SetUpstreamRegistry 注入上游注册表窄依赖（QuotaUpstreamRegistry），用于创建激活码时
// 校验上游标识存在性。生产由 wire 在装配点注入 *quotaUpstreamRepository；测试可注入替身。
func (s *QuotaKeyService) SetUpstreamRegistry(r QuotaUpstreamRegistry) {
	if s == nil {
		return
	}
	s.registry = r
}

// SetRunMode 注入运行模式（config.RunModeSimple / config.RunModeStandard），供分组绑定策略
// 判定 composite 分组在 simple 模式下是否可绑定。空串视为 standard（与 cfg == nil 降级口径一致）。
func (s *QuotaKeyService) SetRunMode(mode string) {
	if s == nil {
		return
	}
	s.runMode = mode
}

// quotaKeyAuthCacheInvalidator 是分组认证缓存的窄依赖（APIKeyService 实现）。
type quotaKeyAuthCacheInvalidator interface {
	InvalidateAuthCacheByGroupID(ctx context.Context, groupID int64)
}

// SetAuthCacheInvalidator 注入分组认证缓存失效器（wire 装配点注入）。
func (s *QuotaKeyService) SetAuthCacheInvalidator(v quotaKeyAuthCacheInvalidator) {
	if s == nil {
		return
	}
	s.authCache = v
}

// invalidateGroupAuthCache 逐个失效受影响分组的认证缓存；未装配（nil）或空列表时跳过
// （不阻断主流程，登记为残余风险）。
func (s *QuotaKeyService) invalidateGroupAuthCache(ctx context.Context, groupIDs []int64) {
	if s.authCache == nil || len(groupIDs) == 0 {
		return
	}
	for _, gid := range groupIDs {
		s.authCache.InvalidateAuthCacheByGroupID(ctx, gid)
	}
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
	s.cursorGroupResolver = s.findCursorPlatformGroups
	s.insertFailureMarkerWriter = s.writeInsertFailureMarker
	// P1-B4 设备同源闭环：真实拉号客户端需要反查本服务才能分流"存量派生值 / DeviceIDFor"，
	// 构造顺序天然成环（客户端先建 → 服务后建），故在此回填本服务自身作为设备解析器
	// （*QuotaKeyService 同时提供 DeviceIDFor 与 HasAccount）。
	// 这样 wire / 测试无须新增装配语句，也不必改动构造函数签名；
	// 传入的是占位或替身上游（不满足类型断言）时整段跳过，行为不变。
	//
	// KC-17：注入的可能是分派器 upstreamDispatchingClient（wire 在 KC-17B 里把它绑成
	// QuotaKeyUpstreamClient 的实现）。分派器只是把 *shareAIUpstreamClient 包了一层，
	// resolver 必须落到真正的 shareAI 客户端上——否则 shareAI 路径在需要设备解析时会退化为
	// 既有 deviceID(code) 分支，破坏同源闭环。两种情况都照抄既有"先建客户端 → 后回填 resolver"
	// 的顺序，不重排。
	if sac, ok := upstream.(*shareAIUpstreamClient); ok {
		sac.SetDeviceResolver(s)
	} else if dc, ok := upstream.(*upstreamDispatchingClient); ok {
		if sac, ok := dc.shareAI.(*shareAIUpstreamClient); ok {
			sac.SetDeviceResolver(s)
		}
	}
	// P1-S：cursor 调度资格门所需的激活码状态读取器（包级安装，沿
	// codeBuddyQuotaInstance 的模式）。调度谓词是包级纯函数、拿不到 service 句柄，
	// 故在此装配点一次性注入；未装配时资格门降级为不收紧（见
	// cursorAccountQuotaKeyVetoReason 注释）。
	//
	// 仅在**真实装配**（带 ent client）时安装：内存替身装配（client==nil，只存在于测试）
	// 不能冒充生产级来源状态读取器——它会用替身仓储回答真实 key 的查询并失败关闭，
	// 把"来源状态不可读"污染到其它用例/其它路径（§25.14-C 要求该门只由真实依赖承担）。
	if client != nil {
		InstallCursorQuotaKeyStateProvider(s)
		// 来源门额度快照读取器与状态读取器同源安装（同一 *QuotaKeyService 实现两个窄接口，
		// 仍是同一来源门的一份读依赖）；内存替身装配（client==nil）不冒充生产级来源读取器。
		InstallCursorQuotaKeyUsageSnapshotProvider(s)
	}
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

// ResolvePreviewAccountForModelSync 返回该激活码名下可用于上游模型同步（QuotaKeyHandler.
// SyncUpstreamModels）的 cursor 账号：优先 active && schedulable，其次任意未软删的 cursor 账号；
// 该码下无任何未软删 cursor 账号时返回 400 QUOTA_KEY_NO_ACCOUNT_FOR_SYNC（失败关闭，不让
// 上游模型同步在"无可用账号"时静默成功）。
//
// 选择优先级（用户裁定，本卡同步上游模型入口）：先挑健康可调度账号，避免选到已禁用/出错账号去
// 触发上游模型清单拉取；健康账号为空时退一步用任意一个未软删 cursor 账号兜底；都没有则明确报错。
func (s *QuotaKeyService) ResolvePreviewAccountForModelSync(ctx context.Context, quotaKeyID int64) (*ent.Account, error) {
	if s == nil || s.repo == nil {
		return nil, errors.New("quota_key_service: service not configured")
	}
	accounts, err := s.repo.ListAccountsByQuotaKey(ctx, quotaKeyID)
	if err != nil {
		return nil, err
	}
	var fallback *ent.Account
	for _, acc := range accounts {
		if acc == nil || acc.DeletedAt != nil || acc.Platform != domain.PlatformCursor {
			continue
		}
		// 首个未软删 cursor 账号作为兜底。
		if fallback == nil {
			fallback = acc
		}
		// 健康可调度账号优先返回。
		if acc.Status == domain.StatusActive && acc.Schedulable {
			return acc, nil
		}
	}
	if fallback != nil {
		return fallback, nil
	}
	return nil, infraerrors.BadRequest("QUOTA_KEY_NO_ACCOUNT_FOR_SYNC", "no available cursor account under this quota key for model sync")
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

// CursorQuotaKeyUsageSnapshot 返回激活码 usage_snapshot 列的原始字节，供来源门（§25.6
// 同一来源门）判定真实来源剩余。落盘该列的写入者只有 CursorAutoEntryService 的观测入口
// （§25.14-C 唯一写入者），本方法只读、不解析、不发起网络请求。
//
// quotaKeyID 无效（≤0）按"无快照"返回 (nil, nil)；读取失败（含 key 不存在）返回 error，
// 来源门据此失败关闭。
func (s *QuotaKeyService) CursorQuotaKeyUsageSnapshot(ctx context.Context, quotaKeyID int64) ([]byte, error) {
	if s == nil || s.repo == nil {
		return nil, errors.New("quota_key_service: quota key repository unavailable")
	}
	if quotaKeyID <= 0 {
		return nil, nil
	}
	qk, err := s.repo.GetByID(ctx, quotaKeyID)
	if err != nil {
		return nil, err
	}
	if qk == nil {
		return nil, fmt.Errorf("quota_key_service: quota key %d not found", quotaKeyID)
	}
	return qk.UsageSnapshot, nil
}

// RotateCredentials 加密新激活码原文后写入 code_encrypted / code_mac / code_last4，
// 返回脱敏后的状态。
//
// 不触碰 accounts 表、不发起任何上游请求、不引入 mock。MAC 复用与
// account_credentials 同型的 HMAC-SHA256 指纹（供 uq_quota_keys_code_mac 唯一索引
// 做 SQL 级查重），不引入新加密机制。
//
// §25.14-C「换码原子边界」：与 Update(code) 归入唯一凭据写方法 rotateCredentials，
// 同一每 key 锁 + 同一事务内按新旧 MAC 决定是否清空 usage_snapshot。
func (s *QuotaKeyService) RotateCredentials(ctx context.Context, id int64, newPlain string) (*QuotaKeyStatus, error) {
	if err := s.rotateCredentials(ctx, id, newPlain); err != nil {
		return nil, err
	}
	return s.GetStatus(ctx, id)
}

// rotateCredentials 是**唯一**的凭据写实现（RotateCredentials / Update(code) 共用）。
//
// 原子边界（§25.14-C）：
//   - 持同一每 key advisory 锁 + 同一事务，锁内重读旧行比较 MAC；
//   - 新旧 MAC 不同（换了一串 code）→ 同事务写密文/MAC/last4 并**清空 usage_snapshot**：
//     旧来源的观测对新来源无意义，留着会让资格门拿过期事实放行；
//   - 新旧 MAC 相同（同一来源重新加密）→ 只重加密，**不清**已有观测（观测仍然成立）；
//   - 来源不存在/已软删 → 明确错误（不复活来源），且不写任何东西。
//
// 换码不触碰 device_id、账号归属、status、expires_at 及其他元数据；也不自动清 exhausted
// 或恢复子账号（status 已 expired 的码不会因换串 code 而被重新激活）。
// rotateCredentials 是**唯一**的凭据写实现的加锁入口（RotateCredentials / Update(code) 共用）。
//
// 加密在锁外完成（同 key 事务只包住"重读 + 比 MAC + 写凭据"），锁内写本体是
// writeCredentialsLocked —— Update 的同 key 事务直接复用它，不嵌套再取同 key 锁。
func (s *QuotaKeyService) rotateCredentials(ctx context.Context, id int64, newPlain string) error {
	enc, err := s.enc.Encrypt([]byte(newPlain))
	if err != nil {
		return err
	}
	obs, err := asObservationRepo(s.repo)
	if err != nil {
		return err
	}
	return s.withQuotaKeyLock(ctx, id, func(tx *ent.Tx) error {
		current, lerr := obs.GetByIDInTx(ctx, tx, id)
		if lerr != nil {
			// 不存在 / 已软删：明确错误，原样上抛（不复活来源、不静默成功）。
			return lerr
		}
		if current == nil {
			return fmt.Errorf("quota_key_service: quota key %d not found", id)
		}
		return writeCredentialsLocked(ctx, obs, tx, id, current, string(enc), quotaKeyMACOfPlaintext(newPlain), last4Of(newPlain))
	})
}

// writeCredentialsLocked 是**唯一**的锁内凭据写本体：按 MAC 决定是否清空快照。
//
// 调用方必须已持有该 key 的 advisory 锁、已在同一事务内重读过来源行，并把该行传进来
// （current）；本方法不再取锁、也不再重读，故 Update 的同 key 事务不会读两次同一行。
func writeCredentialsLocked(ctx context.Context, obs quotaKeyObservationRepo, tx *ent.Tx, id int64, current *ent.QuotaKey, codeEncrypted, codeMAC, codeLast4 string) error {
	// 旧 MAC 为空（历史行 / 未落 MAC）无法证明同源，按"来源已变更"保守处理。
	sameSource := current.CodeMAC != "" && current.CodeMAC == codeMAC
	return obs.UpdateCredentialsAtomically(ctx, tx, id, codeEncrypted, codeMAC, codeLast4, !sameSource)
}

// withQuotaKeyLock 在"每 key advisory 互斥 + 单事务"骨架上执行 fn（观测/换码共用）。
//
// 未配置 ent client（内存替身装配 / 纯单元测试）时退化为直接调用 fn：此时无事务与
// advisory 锁可用，替身仓储自身即单进程独占实现。生产构造（NewQuotaKeyService）总是带
// ent client，不会走到该退化分支。
func (s *QuotaKeyService) withQuotaKeyLock(ctx context.Context, quotaKeyID int64, fn func(tx *ent.Tx) error) error {
	if s == nil {
		return errors.New("quota_key_service: service not configured")
	}
	if s.client == nil {
		return fn(nil)
	}
	return withQuotaKeyLockTx(ctx, ctx, s.client, quotaKeyID, fn)
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
//
// priority 为该码的调度优先级（数值越小越优先）；<=0 时按列默认值 1 落库，
// 该值会成为本码拉出账号的入库优先级（用户裁定 2026-11-03）。
//
// upstreamKey 是新建激活码所属上游的稳定标识（引用 quota_upstreams.key）：
//   - 空串 = 未指定，由仓储层落列默认值（'shareai'）；
//   - 非空 = 必须对应 quota_upstreams 中存在的、未软删的上游标识，否则返回
//     ErrQuotaKeyUnknownUpstream（绝不静默落成 shareai，那会让 kn-cy 卡密被当
//     shareai 拉号，正是本卡要消灭的故障）。
func (s *QuotaKeyService) Create(ctx context.Context, code string, expiresAt *time.Time, notes *string, priority int, upstreamKey string) (*QuotaKeyStatus, error) {
	return s.CreateWithSettings(ctx, code, expiresAt, notes, priority, upstreamKey, nil)
}

// CreateWithSettings 是 Create 的扩展入口：在创建激活码时一并落库码级设置（并发数 / 负载因子 /
// 计费倍率 / 模型映射），新增账号拉取时据此继承。既有的 Create（6 参）直接委托本方法传 nil，
// 不改动既有调用方签名。
//
// 校验口径（与 UpdateWithSettings 同一套 normalizeQuotaKeySettings）：
//   - ModelMappingSet=true 时规范化后必须至少有 1 个有效（key/value 皆 trim 非空）模型，
//     空映射 / 空键值一律返回 400 QUOTA_KEY_MODELS_REQUIRED，禁止落库（没有任何支持的模型
//     就禁止落库，这是本卡模型管控的硬门槛）。
//   - Concurrency 内层指针 < 0 拒绝；== 0 视为清空/默认（不落库、沿用账号列默认 3）。
//   - LoadFactor 必须在 1..10000；RateMultiplier 必须 >= 0（允许 0）。
//
// 校验在落库前发生，校验失败不触碰任何存储。码级设置经 CreateQuotaKeyInput 同一次插入落库——
// 新建激活码名下尚无账号，无需账号同步（账号同步是 UpdateWithSettings / pull 的职责）。
func (s *QuotaKeyService) CreateWithSettings(ctx context.Context, code string, expiresAt *time.Time, notes *string, priority int, upstreamKey string, settings *UpdateQuotaKeySettingsInput) (*QuotaKeyStatus, error) {
	if priority < 0 {
		return nil, errors.New("quota_key_service: priority must not be negative")
	}
	// 非空上游标识必须存在且未软删；缺失注册表（nil）时失败关闭，不静默落成 shareai。
	if upstreamKey != "" {
		if s.registry == nil {
			return nil, fmt.Errorf("quota_key_service: upstream registry unavailable, cannot validate upstream_key %q", upstreamKey)
		}
		exists, err := s.registry.ExistsByKey(ctx, upstreamKey)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("%w: %q", ErrQuotaKeyUnknownUpstream, upstreamKey)
		}
	}
	var (
		concurrency  *int
		loadFactor   *int
		rateMult     *float64
		modelMapping map[string]string
	)
	if settings != nil {
		norm, err := normalizeQuotaKeySettings(settings)
		if err != nil {
			return nil, err
		}
		concurrency = norm.concurrency
		loadFactor = norm.loadFactor
		rateMult = norm.rateMultiplier
		modelMapping = norm.modelMapping
	}
	// 落库前若提供了码级分组：去重/正整数校验 + 存在性/软删校验 + simple 模式 composite 拦截。
	var groupIDs []int64
	var groupIDsSet bool
	if settings != nil && settings.GroupIDs != nil {
		normGroupIDs, gerr := s.normalizeQuotaKeyGroupIDs(ctx, *settings.GroupIDs)
		if gerr != nil {
			return nil, gerr
		}
		groupIDs = normGroupIDs
		groupIDsSet = true
	}
	enc, err := s.enc.Encrypt([]byte(code))
	if err != nil {
		return nil, err
	}
	codeEncrypted := string(enc)
	codeMAC := quotaKeyMACOfPlaintext(code)
	codeLast4 := last4Of(code)
	qk, err := s.repo.Create(ctx, CreateQuotaKeyInput{
		CodeEncrypted:  codeEncrypted,
		CodeMAC:        codeMAC,
		CodeLast4:      codeLast4,
		Status:         domain.QuotaKeyStatusActive,
		ExpiresAt:      expiresAt,
		Notes:          notes,
		Priority:       priority,
		UpstreamKey:    upstreamKey,
		Concurrency:    concurrency,
		LoadFactor:     loadFactor,
		RateMultiplier: rateMult,
		ModelMapping:   modelMapping,
		GroupIDs:       groupIDs,
		GroupIDsSet:    groupIDsSet,
	})
	if err != nil {
		return nil, err
	}
	// 创建路径无既有事务：单独开事务做白名单追加；失败返回错误（码已创建，重新保存可修复）。
	if groupIDsSet && len(groupIDs) > 0 && len(qk.ModelMapping) > 0 {
		if s.client != nil {
			tx, terr := s.client.Tx(ctx)
			if terr != nil {
				return nil, terr
			}
			changed, aerr := s.appendModelsToBoundGroupAllowlists(ctx, tx, qk.ID, qk.ModelMapping)
			if aerr != nil {
				_ = tx.Rollback()
				return nil, fmt.Errorf("quota_key_service: append group allowlist on create: %w", aerr)
			}
			if cerr := tx.Commit(); cerr != nil {
				return nil, fmt.Errorf("quota_key_service: commit group allowlist on create: %w", cerr)
			}
			s.invalidateGroupAuthCache(ctx, changed)
		}
	}
	return quotaKeyToStatus(qk), nil
}

// normalizedQuotaKeySettings 是 normalizeQuotaKeySettings 的校验后结果。
type normalizedQuotaKeySettings struct {
	concurrency    *int
	loadFactor     *int
	rateMultiplier *float64
	modelMapping   map[string]string
}

// normalizeQuotaKeySettings 校验并规范化激活码设置输入（CreateWithSettings / UpdateWithSettings
// 共用）。任何非法值立即返回 fail-closed 错误；合法值规范化后输出可直接落库的形态。
//
// 三态语义（详见 UpdateQuotaKeySettingsInput 注释）：
//   - Concurrency 外层 nil=不修改；内层 nil=清空恢复默认 3；非 nil 时 < 0 拒绝、== 0 视为清空、
//     正数落库。
//   - LoadFactor 外层 nil=不修改；内层 nil=清空/卸载；非 nil 时必须在 1..10000（否则拒绝）。
//   - RateMultiplier nil=不修改；非 nil 时 < 0 拒绝（0 合法，允许把某码计费倍率置 0）。
//   - ModelMappingSet=false=不修改；为 true 时规范化后必须 >= 1 个有效条目，否则 400 拒绝。
func normalizeQuotaKeySettings(settings *UpdateQuotaKeySettingsInput) (*normalizedQuotaKeySettings, error) {
	out := &normalizedQuotaKeySettings{}
	// Concurrency。
	if settings.Concurrency != nil {
		inner := *settings.Concurrency
		if inner != nil {
			if *inner < 0 {
				return nil, infraerrors.BadRequest("QUOTA_KEY_CONCURRENCY_INVALID", "concurrency must be >= 0")
			}
			if *inner > 0 {
				v := *inner
				out.concurrency = &v
			}
			// *inner == 0 → 清空/默认，输出 nil。
		}
	}
	// LoadFactor。
	if settings.LoadFactor != nil {
		inner := *settings.LoadFactor
		if inner != nil {
			if *inner < 1 || *inner > 10000 {
				return nil, infraerrors.BadRequest("QUOTA_KEY_LOAD_FACTOR_INVALID", "load_factor must be in 1..10000")
			}
			v := *inner
			out.loadFactor = &v
		}
	}
	// RateMultiplier。
	if settings.RateMultiplier != nil {
		if *settings.RateMultiplier < 0 {
			return nil, infraerrors.BadRequest("QUOTA_KEY_RATE_MULTIPLIER_INVALID", "rate_multiplier must be >= 0")
		}
		v := *settings.RateMultiplier
		out.rateMultiplier = &v
	}
	// ModelMapping：ModelMappingSet=true 时必须非空（去空白、去空键空值后）。
	if settings.ModelMappingSet {
		cleaned := make(map[string]string, len(settings.ModelMapping))
		for k, v := range settings.ModelMapping {
			kk := strings.TrimSpace(k)
			vv := strings.TrimSpace(v)
			if kk == "" || vv == "" {
				continue
			}
			cleaned[kk] = vv
		}
		if len(cleaned) == 0 {
			return nil, infraerrors.BadRequest("QUOTA_KEY_MODELS_REQUIRED", "at least one supported model is required")
		}
		out.modelMapping = cleaned
	}
	return out, nil
}

// normalizeQuotaKeyGroupIDs 去重保序、剔除非正数，并校验全部分组存在且未软删；
// Simple 模式复用 IsGroupBindableInSimpleMode 口径拦截 composite。
//
// 非法（含 ≤0）→ QUOTA_KEY_GROUP_IDS_INVALID。
// 不存在 / 已软删 → QUOTA_KEY_GROUP_NOT_FOUND。
// Simple 模式 + composite → 复用既有错误码 SIMPLE_MODE_GROUP_NOT_BINDABLE。
//
// s.client == nil（测试替身装配）→ 跳过存在性校验，仅做去重/正整数校验，
// 与既有 checkQuotaKeyExists 的 nil-client 口径一致。
func (s *QuotaKeyService) normalizeQuotaKeyGroupIDs(ctx context.Context, ids []int64) ([]int64, error) {
	// 去重保序，剔除非正数（≤0 视为非法）。
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, infraerrors.BadRequest("QUOTA_KEY_GROUP_IDS_INVALID", "group id must be a positive integer")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if s.client == nil {
		// 测试替身装配：跳过存在性校验，仅做去重/正整数校验。
		return out, nil
	}
	// 校验全部分组存在且未软删。
	groups, err := s.client.Group.Query().
		Where(group.IDIn(out...), group.DeletedAtIsNil()).
		All(ctx)
	if err != nil {
		return nil, err
	}
	if len(groups) != len(out) {
		return nil, infraerrors.BadRequest("QUOTA_KEY_GROUP_NOT_FOUND", "one or more groups do not exist or have been soft-deleted")
	}
	// Simple 模式：composite 分组不可绑定。
	if s.runMode == config.RunModeSimple {
		for _, g := range groups {
			if g.Platform == PlatformComposite {
				return nil, infraerrors.BadRequest("SIMPLE_MODE_GROUP_NOT_BINDABLE", "composite groups cannot be bound in simple mode")
			}
		}
	}
	return out, nil
}

// codeLevelModelMappingToAccountMapping 把码级 model_mapping（map[string]string）转换为账号
// credentials.model_mapping 所需的形态（map[string]any，{"from":"to"} 或白名单恒等 {"m":"m"}）。
// 码级映射为空时返回 nil（调用方保持无映射，沿用既有闸门）。
func codeLevelModelMappingToAccountMapping(mm map[string]string) map[string]any {
	if len(mm) == 0 {
		return nil
	}
	out := make(map[string]any, len(mm))
	for k, v := range mm {
		out[k] = v
	}
	return out
}

// Update 改激活码：code 非 nil 时重加密写 credentials；expiresAt/notes 外层指针非 nil 时写元数据。
// 元数据外层 nil 表示省略，内层 nil 表示显式清空，否则设置为给定值。
// 返回脱敏状态。明文 code 只进不出（F1）。
//
// §25.17.6：code 与元数据收进**一次**既有 withQuotaKeyLockTx 事务——锁内重读未删除行、
// 按 MAC 执行原凭据/快照操作、按本请求字段存在性写 expires_at/notes，任一步失败整笔回滚。
// 此前 rotate 的锁内提交与 UpdateMeta 的锁外默认 client 提交是分离的两笔，管理员的日期编辑
// 可以在 Probe 持锁等待 lookup 期间提交，再被 Probe 的旧观测结果覆盖。
// 凭据写本体复用 writeCredentialsLocked（同 key 锁内），不再嵌套取同 key 锁。
func (s *QuotaKeyService) Update(ctx context.Context, id int64, code *string, expiresAt **time.Time, notes **string, priority *int) (*QuotaKeyStatus, error) {
	return s.UpdateWithSettings(ctx, id, code, expiresAt, notes, priority, nil)
}

// UpdateWithSettings 是 Update 的扩展入口：在既有 code / 元数据 / 优先级编辑之上，增加码级设置
// （并发数 / 负载因子 / 计费倍率 / 模型映射）的更新，并在同一 per-key 锁事务内同步名下未软删账号。
// 既有的 Update（6 参）直接委托本方法传 nil，不改动既有调用方签名。
//
// base 字段（code/expiresAt/notes/priority）与 settings 字段在**同一** withQuotaKeyLock 事务内落库：
//   - base 字段走 updateBaseFieldsLocked（与 Update 行为完全一致）；
//   - settings 字段经窄可选接口 quotaKeySettingsRepo.UpdateSettingsAndSyncAccounts 落库并同步账号；
//     仓库未实现该接口时失败关闭（fail-closed），绝不静默绕过。
//
// 校验（normalizeQuotaKeySettings）在开事务前完成；ModelMappingSet=true 且规范化后为空立即返回
// 400 QUOTA_KEY_MODELS_REQUIRED，禁止落库。
func (s *QuotaKeyService) UpdateWithSettings(ctx context.Context, id int64, code *string, expiresAt **time.Time, notes **string, priority *int, settings *UpdateQuotaKeySettingsInput) (*QuotaKeyStatus, error) {
	if priority != nil && *priority <= 0 {
		return nil, errors.New("quota_key_service: priority must be positive")
	}
	if settings != nil {
		if _, err := normalizeQuotaKeySettings(settings); err != nil {
			return nil, err
		}
	}
	baseChanged := code != nil || expiresAt != nil || notes != nil || priority != nil
	settingsChanged := settings != nil && (settings.Concurrency != nil || settings.LoadFactor != nil || settings.RateMultiplier != nil || settings.ModelMappingSet)
	if !baseChanged && !settingsChanged {
		return s.GetStatus(ctx, id)
	}
	// 加密在锁外完成：同 key 事务只包住重读、MAC 比对与写入。
	var codeEncrypted, codeMAC, codeLast4 string
	if code != nil {
		enc, eerr := s.enc.Encrypt([]byte(*code))
		if eerr != nil {
			return nil, eerr
		}
		codeEncrypted = string(enc)
		codeMAC = quotaKeyMACOfPlaintext(*code)
		codeLast4 = last4Of(*code)
	}
	lockCtx := ctx
	if lockCtx == nil {
		lockCtx = context.Background()
	}
	// 事务提交成功后失效的分组认证缓存（由回调内 appendModelsToBoundGroupAllowlists 返回）。
	var changedGroupIDs []int64
	err := s.withQuotaKeyLock(lockCtx, id, func(tx *ent.Tx) error {
		if baseChanged {
			if werr := s.updateBaseFieldsLocked(lockCtx, tx, id, code, codeEncrypted, codeMAC, codeLast4, expiresAt, notes, priority); werr != nil {
				return werr
			}
		}
		if settingsChanged {
			sr, rerr := asSettingsRepo(s.repo)
			if rerr != nil {
				return rerr
			}
			if werr := sr.UpdateSettingsAndSyncAccounts(lockCtx, tx, id, *settings); werr != nil {
				return werr
			}
		}
		// 创建/更新路径在锁内（同一 per-key 事务）把白名单追加进已绑定分组；
		// 提交成功后失效受影响分组的认证缓存（见下方 lockCtx 外层调用）。
		if settings != nil && settings.ModelMappingSet && len(settings.ModelMapping) > 0 {
			changed, aerr := s.appendModelsToBoundGroupAllowlists(lockCtx, tx, id, settings.ModelMapping)
			if aerr != nil {
				return fmt.Errorf("quota_key_service: append group allowlist: %w", aerr)
			}
			changedGroupIDs = changed
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// 事务已提交：逐个失效受影响分组的认证缓存（未装配时 skip，不阻断主流程）。
	s.invalidateGroupAuthCache(ctx, changedGroupIDs)
	return s.GetStatus(ctx, id)
}

// updateBaseFieldsLocked 在已持 per-key 锁的事务内执行「来源重读 + 凭据重加密 + 元数据 + 优先级同步」。
// 从既有 Update / UpdateWithSettings 抽出，保证两者行为完全一致、且设置更新与基础字段编辑落在同一事务。
// codeEncrypted/codeMAC/codeLast4 由调用方在锁外预加密后传入（code 非 nil 时有效）。
func (s *QuotaKeyService) updateBaseFieldsLocked(ctx context.Context, tx *ent.Tx, id int64, code *string, codeEncrypted, codeMAC, codeLast4 string, expiresAt **time.Time, notes **string, priority *int) error {
	obs, err := asObservationRepo(s.repo)
	if err != nil {
		return err
	}
	// 锁内重读未删除行：来源已不存在/已软删时不复活、不写任何东西。
	current, rerr := obs.GetByIDInTx(ctx, tx, id)
	if rerr != nil {
		return rerr
	}
	if current == nil {
		return fmt.Errorf("quota_key_service: quota key %d not found", id)
	}
	if code != nil {
		if werr := writeCredentialsLocked(ctx, obs, tx, id, current, codeEncrypted, codeMAC, codeLast4); werr != nil {
			return werr
		}
	}
	if expiresAt != nil || notes != nil {
		if werr := s.repo.UpdateMeta(ctx, tx, id, expiresAt, notes); werr != nil {
			return werr
		}
	}
	// 优先级与名下账号同事务同步：码的优先级即码下账号的优先级（用户裁定 2026-11-03）。
	// 未软删账号全部改写；软删账号不改（其调度资格已失效）。
	if priority != nil {
		if _, werr := s.repo.UpdatePriority(ctx, tx, id, *priority); werr != nil {
			return werr
		}
	}
	return nil
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

// 来源额度层面的「这个激活码还能不能用」归因取值。
//
// 背景（2026-11-06 生产排查）：后台「可用账号」原来是 working 计数（schedulable && active），
// 完全没看激活码额度，于是出现"显示可用 15 个、实际一个都跑不动"的错觉——15 个账号确实
// 都活着，但它们的激活码只剩 1 次额度，调度侧早就把它们全否决了。计数口径必须带上来源，
// 否则这个数字永远在骗人。
const (
	// QuotaKeySourceBlockExhausted：快照新鲜、观测成功，但真实剩余低于调度下限（<10，含 0）。
	QuotaKeySourceBlockExhausted = "quota_exhausted"
	// QuotaKeySourceBlockKeyInvalid：最近一次观测就失败了（error_code 非空），
	// 即密钥无效 / 过期 / 被上游拒绝。此时名下账号一个都用不了。
	QuotaKeySourceBlockKeyInvalid = "key_invalid"
)

// QuotaKeySourceBlockReason 归纳激活码来源额度是否已经不足以支撑任何调度。
//
// 返回空串表示"还能用"（含"从未观测"——缺失不等于耗尽，不能凭空断言不可用）；
// 返回非空即名下所有账号都不可调度，原因值见上面两个常量。
//
// 刻意不判快照新鲜度：宁可显示上一轮的确定结论，也不要因为刷新滞后在"耗尽/可用"之间
// 来回跳。真实恢复后 5 分钟内一定会有新一轮探测把它翻过来。
func QuotaKeySourceBlockReason(usage *QuotaKeyUsageProjection) string {
	if usage == nil || !usage.HasSnapshot {
		return ""
	}
	if usage.QueryFailed {
		return QuotaKeySourceBlockKeyInvalid
	}
	if usage.Remaining == nil {
		return ""
	}
	if *usage.Remaining < cursorQuotaKeyRemainingFloor {
		return QuotaKeySourceBlockExhausted
	}
	return ""
}

// UsableAccountsForSource 把「账号自己活着」纠正成「现在真的能跑」：
// 来源被归因为耗尽或密钥无效时，可用数归零；否则沿用 working 计数。
func UsableAccountsForSource(workingCount int64, usage *QuotaKeyUsageProjection) int64 {
	if QuotaKeySourceBlockReason(usage) != "" {
		return 0
	}
	return workingCount
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

// quotaKeyObservationRepo 是「来源额度观测 / 换码」所需的**事务内**仓储能力（§25.14-C）。
//
// 单独声明、按需断言，而不是直接扩宽 QuotaKeyRepository：观测写入与换码原子清快照的
// 消费者只有 CursorAutoEntryService 与 QuotaKeyService 两个入口，而既有的仓储窄替身
// （出站/执行器侧的测试替身）与它们无关，不应被逼迫实现不相干的写方法。
//
// 生产实现 *repository.quotaKeyRepository 全量满足；未实现即失败关闭（不静默跳过写入，
// 否则会把"没写"当"已提交事实"）。
type quotaKeyObservationRepo interface {
	// GetByIDInTx 在给定事务内（已持每 key 锁）重读来源行，用于"未删 + 来源身份（MAC/device）
	// 复核"；tx 为 nil 时退化为默认连接（内存替身装配）。
	GetByIDInTx(ctx context.Context, tx *ent.Tx, id int64) (*ent.QuotaKey, error)
	// UpdateUsageSnapshot 在同一事务内写 usage_snapshot，并在给了合法期限时同事务更新
	// expires_at（expiresAt 为 nil 表示上游未给出/无效 → 保留原值）。
	UpdateUsageSnapshot(ctx context.Context, tx *ent.Tx, id int64, snapshot []byte, expiresAt **time.Time) error
	// UpdateCredentialsAtomically 在同一事务内写凭据（密文/MAC/last4）；clearUsageSnapshot
	// 为 true 时同事务清空 usage_snapshot（来源身份已变，旧观测对新来源无效）。
	UpdateCredentialsAtomically(ctx context.Context, tx *ent.Tx, id int64, codeEncrypted, codeMAC, codeLast4 string, clearUsageSnapshot bool) error
}

// asObservationRepo 断言仓储是否支持观测/换码的事务内能力；不支持即失败关闭。
func asObservationRepo(repo QuotaKeyRepository) (quotaKeyObservationRepo, error) {
	if repo == nil {
		return nil, errors.New("quota_key_service: quota key repository unavailable")
	}
	obs, ok := repo.(quotaKeyObservationRepo)
	if !ok {
		return nil, errors.New("quota_key_service: quota key repository does not support transactional usage observation")
	}
	return obs, nil
}

// quotaKeySettingsRepo 是「激活码设置更新 + 名下账号同步」所需的窄可选仓储能力（本卡 B2）。
//
// 与 quotaKeyObservationRepo 同型：单独声明、按需断言，避免无关测试替身被迫实现不相关的写方法。
// 仅当生产 *repository.quotaKeyRepository 实现本接口时，CreateWithSettings / UpdateWithSettings /
// pull 才会在既有的 per-key 事务内行使该能力；否则（test stub、未实现方）调用处失败关闭
// （fail-closed），绝不静默绕过设置落库与账号同步（§25.14-C：绕过即把"没写"当"已提交事实"）。
type quotaKeySettingsRepo interface {
	// UpdateSettingsAndSyncAccounts 在调用方已开启的事务内，把激活码级设置
	// （并发数 / 负载因子 / 计费倍率 / 模型映射）落库到 quota_keys，并同步该码名下未软删账号的
	// 同名列。tx 必填；仓库未实现本接口时由调用方失败关闭。
	UpdateSettingsAndSyncAccounts(ctx context.Context, tx *ent.Tx, id int64, settings UpdateQuotaKeySettingsInput) error
	// SyncAccountsExpiry 在调用方已开启的事务内，把该码名下未软删账号的 expires_at 同步为
	// 激活码的到期时间（拉号成功路径继承码级到期）。tx 必填。
	SyncAccountsExpiry(ctx context.Context, tx *ent.Tx, id int64, expiresAt *time.Time) error
}

// asSettingsRepo 断言仓储是否支持设置更新 + 账号同步能力；不支持即失败关闭。
func asSettingsRepo(repo QuotaKeyRepository) (quotaKeySettingsRepo, error) {
	if repo == nil {
		return nil, errors.New("quota_key_service: quota key repository unavailable")
	}
	sr, ok := repo.(quotaKeySettingsRepo)
	if !ok {
		return nil, errors.New("quota_key_service: quota key repository does not support settings update and account sync")
	}
	return sr, nil
}

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

// countUsableQuotaKeyAccounts 统计归属某激活码、且**现在真的能干活**的账号数。
//
// "能用"的谓词（2026-11-06 口径；自动补位目标已于 2026-11-10 收敛为 1 主 1 热备 = 2）：
//   - 未软删；
//   - schedulable = true（没被人工/状态机停用）；
//   - status = active（不是 error / disabled）；
//   - 没有未到期的临时停调（temp_unschedulable_until 为空或已过期）。
//
// 与 countWorkingQuotaKeyAccounts 的区别：后者只排除软删，被上游挡住、正在停调的号
// 依然算进去。用它做"够不够 5 个"的判据，就会出现"5 个号全停调、池子空了、
// 却因为号数还是 5 而永远不补号"——这正是用户反复指出的问题。
func countUsableQuotaKeyAccounts(ctx context.Context, tx *ent.Tx, quotaKeyID int64) (int, error) {
	now := time.Now()
	return tx.Account.Query().
		Where(
			account.QuotaKeyID(quotaKeyID),
			account.DeletedAtIsNil(),
			account.Schedulable(true),
			account.StatusEQ(domain.StatusActive),
			account.Or(
				account.TempUnschedulableUntilIsNil(),
				account.TempUnschedulableUntilLTE(now),
			),
		).
		Count(ctx)
}

// checkQuotaKeyCapacityLocked 是容量判定的唯一实现，运行在"已持有该激活码 advisory
// 事务锁"的 tx 内（软删校验随之前移进锁内，修 r4 #2），返回判定时刻的 working 计数：
//  1. quota_key 必须存在且未软删（普通查询天然排除软删行）；
//  2. status 必须为 active；
//  3. working 必须 < limit（limit <= 0 表示不设上限）。
//
// limit 由调用方按"人工 / 自动"两条路分别给：
//   - 自动补位传 CursorAutoPullAccountsPerKey（1 主 1 热备 = 2）；
//   - 人工建号/改绑传 0（不受限制，用户裁定 2026-11-06）。
//
// 调用方必须在【同一 tx】内完成写入：判定与写入分属两个事务时，锁一释放就有并发漏网窗口。
func checkQuotaKeyCapacityLocked(ctx context.Context, tx *ent.Tx, quotaKeyID int64, limit int) (int, error) {
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
	// 自动补位（limit > 0）判的是"可用号数"，不是"表里还剩几行"：停调中的号不算数，
	// 否则 5 个号全停调时会因为"还是 5 行"而永远拉不进新号（2026-11-06 用户裁定）。
	// 人工建号/改绑（limit <= 0）没有上限，仍返回总行数供调用方展示。
	if limit > 0 {
		usable, uerr := countUsableQuotaKeyAccounts(ctx, tx, quotaKeyID)
		if uerr != nil {
			return 0, fmt.Errorf("quota_key_service: count usable accounts: %w", uerr)
		}
		working = usable
	}
	if limit > 0 && working >= limit {
		return working, ErrQuotaKeyAccountLimitReached.WithMetadata(map[string]string{
			"quota_key_id": strconv.FormatInt(quotaKeyID, 10),
			"working":      strconv.Itoa(working),
			"limit":        strconv.Itoa(limit),
		})
	}
	return working, nil
}

// withQuotaKeyCapacity 在单激活码 advisory 互斥域内执行"容量判定 + 写入"：
// 取锁 → working 计数（§3.2 谓词）→ 校验 quota_key 未软删且 active → 判定 < MaxAccountsPerQuotaKey
// → 在【同一事务】内调用 fn(tx) 写入 → 提交（锁随事务释放）。
// fn 内不得再自行取锁/计数/开事务：那样会与本入口形成嵌套 tx 或两个事务的两个临界区
// （即 r4 #1/#3 的漏网形态）。容量不足返回 ErrQuotaKeyAccountLimitReached。
func (s *QuotaKeyService) withQuotaKeyCapacity(ctx context.Context, quotaKeyID int64, limit int, fn func(tx *ent.Tx) error) error {
	if s.client == nil {
		return errors.New("quota_key_service: ent client not configured")
	}
	return withQuotaKeyCapacityTx(ctx, s.client, quotaKeyID, limit, fn)
}

// withQuotaKeyCapacityTx 是 withQuotaKeyCapacity 的实现本体，供不持有 QuotaKeyService
// 的写路径（admin 建号 / admin 改绑）复用同一入口、同一互斥域，不复制一份临界区。
//
// limit 语义见 checkQuotaKeyCapacityLocked：自动补位传 CursorAutoPullAccountsPerKey，
// 人工建号/改绑传 0（不限）。
//
// §25.14-C：只抽出"same-key advisory lock + 单事务"这一层公共骨架
// （withQuotaKeyLockTx），容量入口在骨架内执行**自己的**容量校验；观测/换码入口复用
// 同一骨架但**不**借用容量/active 门（额度刷新不因容量已满或状态变更被拒绝）。
func withQuotaKeyCapacityTx(ctx context.Context, client *ent.Client, quotaKeyID int64, limit int, fn func(tx *ent.Tx) error) error {
	return withQuotaKeyLockTx(ctx, ctx, client, quotaKeyID, func(tx *ent.Tx) error {
		if _, err := checkQuotaKeyCapacityLocked(ctx, tx, quotaKeyID, limit); err != nil {
			return err
		}
		return fn(tx)
	})
}

// withQuotaKeyLockTx 是"每 key advisory 互斥 + 单事务"的公共骨架（§25.6 / §25.14-C）。
//
// 只提供锁与事务，不内含任何容量/active/来源判定——判定由各自入口在骨架内完成。
//
// txCtx 与 lockCtx 分开是为了"网络取消不得带走 DB 收尾"（§25.14-C 步骤 5）：
//   - txCtx 开启事务，必须是 context.WithoutCancel 派生的**绝对期限**上下文。事务的
//     生命周期若挂在调用者的可取消 context 上，调用者在 lookup 途中取消会让 transaction
//     被驱动自动回滚，刚写入的"失败观测"随之一并消失；
//   - lockCtx 只用于锁等待重试，受调用者期限约束（调用者已取消则不启动、不空转）。
func withQuotaKeyLockTx(txCtx, lockCtx context.Context, client *ent.Client, quotaKeyID int64, fn func(tx *ent.Tx) error) error {
	if client == nil {
		return errors.New("quota_key_service: ent client not configured")
	}
	tx, err := client.Tx(txCtx)
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
	if err := acquireAdvisoryXactLock(lockCtx, tx, quotaKeyAdvisoryLockID(quotaKeyID)); err != nil {
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

// PullAccountsForModel 携带模型 M 上下文的拉号入口（§13.2 触发方传入 / §20.2-A 挂载）。
// 成功拉号后，新号在落库事务提交前挂入 U7 目标分组（与手动 RefreshAccounts 同源同判：
// cursorMountTargetGroups 按模型真实厂商原生平台 ∪ cursor 专属 ∪ composite，白名单启用且
// 实际覆盖；allow-all（无对应模型）聚合/福利组不挂）；零目标组失败关闭（回滚、不落裸号）。
// modelM 仅作为 reconciler 水位/触发上下文保留，不再收窄挂载——挂载按账号模型能力
// （全量静态实供清单）计算（U7：模型按所属原生平台进组，§20.2-A）。
// 其内部复用与 PullAccounts 完全相同的 pull 状态机（同一 advisory 互斥域 + 同一事务 +
// ≤15 强制），不另造第二拉号机制。
func (s *QuotaKeyService) PullAccountsForModel(ctx context.Context, quotaKeyID int64, modelM string) error {
	// 挂载解析器：自动补池与手动挂组同源同判。GroupIDs==nil 时经 findCursorPlatformGroupsForKey
	// 按码级 model_mapping → 上游自报实供清单 → 静态实供清单 推导模型族挂载目标（U7 唯一判定出口）；
	// GroupIDs!=nil 时由 pull 内码级分组分支直接采用码级分组（不进 resolver）。
	// resolver 在事务内重读 qk，保证读到最新码级设置。
	_ = modelM // 模型上下文仅作触发语义保留；挂载目标按账号能力全量计算（§20.2-A）。
	return s.pull(ctx, quotaKeyID, func(ctx context.Context, tx *ent.Tx) ([]int64, error) {
		qk, err := tx.QuotaKey.Get(ctx, quotaKeyID)
		if err != nil {
			return nil, fmt.Errorf("quota_key_service: re-read quota key %d in mount resolver: %w", quotaKeyID, err)
		}
		return s.findCursorPlatformGroupsForKey(ctx, tx, qk)
	}, false)
}

// RefreshAccounts 手动触发拉号（§4.4），与 PullAccountsForModel 共享同一 advisory 互斥域。
//
// 手动入口【走挂载语义】：挂载目标 = U7 目标分组（findCursorPlatformGroups →
// cursorMountTargetGroups：模型真实厂商原生平台组 ∪ cursor 专属 ∪ composite，白名单启用且
// 覆盖静态实供模型；allow-all 不挂）。与自动补池 PullAccountsForModel 同源同判（§20.2-A）。
// 这样拉到的 cursor 账号会挂入目标分组（group_count>0、可用），而非落成裸号
// （group_count=0）。零目标组 → ErrQuotaKeyModelNoCoverage 失败关闭。
// PullAccounts(2 参，reconciler 历史内部入口) 保持 mountResolver=nil、不挂载——行为差异在此显式区分。
func (s *QuotaKeyService) RefreshAccounts(ctx context.Context, quotaKeyID int64) error {
	// 挂载解析器：与 PullAccountsForModel 同源同判。resolver 在事务内重读 qk，
	// GroupIDs==nil 时按码级 model_mapping → 上游自报实供清单 → 静态实供清单 推导挂载目标。
	return s.pull(ctx, quotaKeyID, func(ctx context.Context, tx *ent.Tx) ([]int64, error) {
		qk, err := tx.QuotaKey.Get(ctx, quotaKeyID)
		if err != nil {
			return nil, fmt.Errorf("quota_key_service: re-read quota key %d in mount resolver: %w", quotaKeyID, err)
		}
		return s.findCursorPlatformGroupsForKey(ctx, tx, qk)
	}, true)
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

	// O01：device_id 首写前置到容量事务之外（无事务、无行锁，独立提交）。
	// 修复「新码只生成一份稳定值」的回滚竞态：上游 PullBatch 成功后若配置读取 / 挂组解析 /
	// 链接写入 / 提交任一失败 → 容量事务回滚 → device_id 回到 NULL，下次重试生成第二份 ID。
	// 此处先行把 device_id 落地（独立提交），事务回滚不再波及：
	//   - 普通读 key（无 FOR UPDATE）：读错误 → 失败关闭（不进上游请求）；
	//   - 已非 active / 过期 → 跳过首写照旧进事务（后续路径不变）；
	//   - device_id 为空 → 交给三分支唯一实现 resolveCursorDeviceIdentity：
	//     已有账号（HasAccount==true）的存量码取派生值、不写不迁移；真新码才首写
	//     （此刻无行锁，不会自死锁）落地一份稳定值。
	qk0, preErr := s.client.QuotaKey.Query().Where(quotakey.IDEQ(quotaKeyID)).Only(ctx)
	if preErr != nil {
		return fmt.Errorf("quota_key_service: pre-pull load quota key for device first-write: %w", preErr)
	}
	if qk0.Status == domain.QuotaKeyStatusActive && !isExpired(qk0) {
		if qk0.DeviceID == nil || *qk0.DeviceID == "" {
			// §25.14-C：三分支判定只有一份实现（cursorDeviceBranch），取号 / 出站 / Probe
			// 共用。取号此处只负责"首写必须在容量事务之前"，且**不消费明文**（取号本来
			// 就不解密；存量码走派生分支，不写库、不迁移）。
			if _, derr := ensureCursorDeviceFirstWrite(ctx, s.repo, qk0); derr != nil {
				return fmt.Errorf("quota_key_service: device first-write before pull: %w", derr)
			}
		}
	}

	err := s.withQuotaKeyCapacity(ctx, quotaKeyID, CursorAutoPullAccountsPerKey, func(tx *ent.Tx) error {
		// 二次读状态（防 sweep 竞态）：重新 FOR UPDATE 确认仍为 active 且未过期。
		qk2, err := tx.QuotaKey.Query().Where(quotakey.IDEQ(quotaKeyID)).ForUpdate().Only(ctx)
		if err != nil {
			return fmt.Errorf("quota_key_service: re-load quota key: %w", err)
		}
		if qk2.Status != domain.QuotaKeyStatusActive || isExpired(qk2) {
			return nil
		}

		// 码级分组是权威值：显式绑定时只挂码级分组；显式 [] 表示不服务任何分组——
		// 必须在发上游之前拦下，避免白耗每日拉号额度。
		// 外审 R1c 强制：该分支必须保持在 mountResolver != nil 门控之内——
		// PullAccounts 传 nil resolver 语义就是"不挂组"，即便码级绑定了分组也不挂
		// （挂载由触发方另行编排）；同时这也让 reconciler pending wakeup 路径
		// （绕过 ListCandidateKeys 直进 pull）在发上游前就被拦截。
		var codeLevelGroupIDs []int64
		if qk2.GroupIds != nil && mountResolver != nil {
			if len(qk2.GroupIds) == 0 {
				return fmt.Errorf("quota_key_service: quota key %d has no bound groups", quotaKeyID)
			}
			live, lerr := s.filterLiveGroupIDs(ctx, tx, qk2.GroupIds)
			if lerr != nil {
				return fmt.Errorf("quota_key_service: filter live groups: %w", lerr)
			}
			if len(live) == 0 {
				return fmt.Errorf("quota_key_service: no covering group after upstream success: %w: %w",
					ErrQuotaKeyMountFailed, ErrQuotaKeyModelNoCoverage)
			}
			codeLevelGroupIDs = live
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
			// 「当日刷新次数已达上限」是可预期节流（上游每 key 每日 N 次配额用尽，
			// 次日重置后自动恢复），不是 key 级终态也不是链路故障：
			//   - 走冷却标记路径（同事务写 last_refresh_error + last_refresh_at，豁免 exhausted），
			//     与短窗冷却同型 —— exhausted 维度不动（每日节流不是已确认终态）；
			//   - 不落未知失败路径，避免自动链路把它记成任务失败（生产实证 2026-10-27：
			//     quota_key_reconcile 周期 5 分钟必然撞上每日上限，
			//     此前落 UpstreamUnknown 导致 ops jobScore 长期被扣并掩盖真故障）。
			if errors.Is(pullErr, ErrUpstreamDailyRefreshLimit) {
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
		working, err := checkQuotaKeyCapacityLocked(ctx, tx, quotaKeyID, CursorAutoPullAccountsPerKey)
		if err != nil {
			if errors.Is(err, ErrQuotaKeyAccountLimitReached) || errors.Is(err, ErrQuotaKeyNotActive) {
				return nil
			}
			return err
		}

		// 容量判定与插入在同一事务/同一 advisory 互斥域内：maxInsert 以插入前读到的
		// 基准数为准。
		// upstream 溢出的账号直接丢弃：UpstreamPullResult 不带重放字段，不做二次落库。
		//
		// 目标账号数按"该卡所属上游"分派（KC-26）：shareai=5、kn-cy=1（1 卡 1 号）。
		// 未知上游标识失败关闭（不静默按 shareai 的 5 处理）。
		targetPerKey := CursorAutoPullAccountsPerKey
		if t, terr := s.upstream.TargetAccountsPerKeyFor(qk3.UpstreamKey); terr != nil {
			return fmt.Errorf("quota_key_service: resolve target accounts per key %q: %w", qk3.UpstreamKey, terr)
		} else {
			targetPerKey = t
		}
		// 2026-11-10 派发单 c1p1-A：自动路径以「非软删总行数」为基准，而非仅 active 可用数
		// （checkQuotaKeyCapacityLocked 的 working）。理由同上——"1 主 1 热备"看的是该码名下
		// 所有未软删账号总数，停调/待恢复的号也占水位，避免热备码被补超过目标数。
		// 人工路径（manual=true，RefreshAccounts）保持现状：仍以 working 为基准，不受总闸影响。
		base := working
		if !manual {
			total, terr := tx.Account.Query().
				Where(account.QuotaKeyID(quotaKeyID), account.DeletedAtIsNil()).
				Count(ctx)
			if terr != nil {
				return fmt.Errorf("quota_key_service: count total accounts for %d: %w", quotaKeyID, terr)
			}
			base = total
		}
		maxInsert := targetPerKey - base
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
		// 码级模型映射优先：若 quota_keys.model_mapping 非空，直接由它转换生成 entry 映射，
		// 即使 s.autoEntry == nil 也优先采用码级映射（码级映射 = 该码允许使用的模型权威值）。
		// 否则回落既有 RealSupplyModelsFor / CursorStaticEntryModels 路径。
		if len(qk3.ModelMapping) > 0 {
			entryModelMapping = codeLevelModelMappingToAccountMapping(qk3.ModelMapping)
		} else if s.autoEntry != nil {
			cfg, cerr := s.autoEntry.GetConfig(ctx)
			if cerr != nil {
				return fmt.Errorf("quota_key_service: load auto entry config: %w: %w", ErrQuotaKeyMountFailed, cerr)
			}
			entryCfg = cfg
			// model_mapping 按"该卡所属上游"自报的实供模型写恒等映射（KC-26）：
			//   - 上游声明了实供模型（kn-cy 返回 3 个）→ 用上游声明的恒等映射；
			//   - 上游未声明（返回空）→ 回落既有静态清单（shareai 走这条，行为零变化）；
			//   - 未知上游标识 → 失败关闭，绝不静默按 shareai 处理。
			// 恒等映射构造复用既有 CursorAccountModelMapping（不另写映射构造器）。
			models := CursorStaticEntryModels()
			if s.upstream != nil {
				if rm, rerr := s.upstream.RealSupplyModelsFor(qk3.UpstreamKey); rerr != nil {
					return fmt.Errorf("quota_key_service: resolve real supply models for upstream %q: %w", qk3.UpstreamKey, rerr)
				} else if len(rm) > 0 {
					models = rm
				}
			}
			entryModelMapping = CursorAccountModelMapping(models)
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
			// 新账号继承码级设置（码级权威值，覆盖账号列默认）：
			//   - Concurrency / LoadFactor：仅当码级指针非空且为正值时写入（0/负/空 = 沿用账号默认）；
			//   - RateMultiplier：码级值（float64，>=0）直接继承（含 0，0 是合法计费倍率）；
			//   - Priority：码级 > 0 时采用，否则回落账号默认；
			//   - ExpiresAt：优先用上游本次返回的 result.ExpiresAt，其次码级 qk3.ExpiresAt。
			if qk3.Concurrency != nil && *qk3.Concurrency > 0 {
				create = create.SetConcurrency(*qk3.Concurrency)
			}
			if qk3.LoadFactor != nil && *qk3.LoadFactor > 0 {
				create = create.SetLoadFactor(*qk3.LoadFactor)
			}
			if qk3.RateMultiplier >= 0 {
				create = create.SetRateMultiplier(qk3.RateMultiplier)
			}
			if qk3.Priority > 0 {
				create = create.SetPriority(qk3.Priority)
			}
			switch {
			case result.ExpiresAt != nil:
				create = create.SetExpiresAt(*result.ExpiresAt)
			case qk3.ExpiresAt != nil:
				create = create.SetExpiresAt(*qk3.ExpiresAt)
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
			var groupIDs []int64
			if qk3.GroupIds == nil {
				var err error
				groupIDs, err = mountResolver(ctx, tx)
				if err != nil {
					// F4：上游已成功发号（号未入库，事务尚未提交），但本地分组解析失败
					//（shareai 非幂等，上游发号不可回滚）→ 事务将回滚（号不入库）+ 与挂载失败
					// 同哨兵 ErrQuotaKeyMountFailed 返回，调用方可识别"上游可能已被消耗一次"。
					return fmt.Errorf("quota_key_service: resolve mount groups: %w: %w", ErrQuotaKeyMountFailed, err)
				}
			} else {
				// 码级分组绑定（qk3.GroupIds != nil）：跳过 mountResolver，直接使用
				// 发上游前已 filter 过的码级分组（权威值），不再经模型推导。
				groupIDs = codeLevelGroupIDs
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
		// 同步名下存量（未软删）账号的过期时间（继承码级到期）：仅在仓库实现 quotaKeySettingsRepo
		// 时执行；未实现即跳过（test stub 不实现该窄接口，不强行同步）。result.ExpiresAt 为 nil
		// 时上游未给到期时间，保持账号原值不动，不同步。
		if result.ExpiresAt != nil {
			if sr, ok := s.repo.(quotaKeySettingsRepo); ok {
				if serr := sr.SyncAccountsExpiry(ctx, tx, quotaKeyID, result.ExpiresAt); serr != nil {
					return fmt.Errorf("quota_key_service: sync accounts expiry: %w", serr)
				}
			}
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

// filterLiveGroupIDs 返回 ids 中"存活"的分组 ID：status='active' 且未软删（deleted_at IS NULL）。
// 保序：按入参 ids 的顺序输出命中的 id，便于挂载时维持码级分组声明的优先级顺序。
func (s *QuotaKeyService) filterLiveGroupIDs(ctx context.Context, tx *ent.Tx, ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	groups, err := tx.Group.Query().
		Where(
			group.IDIn(ids...),
			group.DeletedAtIsNil(),
			group.Status(domain.StatusActive),
		).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("quota_key_service: query live groups: %w", err)
	}
	liveSet := make(map[int64]struct{}, len(groups))
	for _, g := range groups {
		liveSet[g.ID] = struct{}{}
	}
	out := make([]int64, 0, len(groups))
	for _, id := range ids {
		if _, ok := liveSet[id]; ok {
			out = append(out, id)
		}
	}
	return out, nil
}

// findCursorPlatformGroupsForKey 是 GroupIDs==nil 路径（自动补池 / 手动挂组）的挂载推导入口。
//
// 模型输入优先级：
//  1. 码级 model_mapping 的精确键（不含通配 *）——码级权威值优先；
//  2. 上游自报实供清单（RealSupplyModelsFor）；
//  3. 静态实供清单（CursorStaticEntryModels）。
//
// 拿到模型集后，其余判定完全复用 cursorMountTargetGroups（零改动）：按模型真实厂商原生平台
// ∪ cursor 专属 ∪ composite 计算目标分组，白名单启用且实际覆盖才挂。零覆盖 → ErrQuotaKeyModelNoCoverage。
func (s *QuotaKeyService) findCursorPlatformGroupsForKey(ctx context.Context, tx *ent.Tx, qk *ent.QuotaKey) ([]int64, error) {
	// 1. 码级 model_mapping 推导模型集（精确键 + 同厂商实供模型 / 通配键展开）。
	entryModels := codeLevelEntryModels(qk)
	// 2/3. 码级模型集为空（无映射键、或键均未解析到已知厂商）时回落：上游自报实供清单 → 静态实供清单。
	if len(entryModels) == 0 {
		models := CursorStaticEntryModels()
		if s.upstream != nil {
			if rm, rerr := s.upstream.RealSupplyModelsFor(qk.UpstreamKey); rerr != nil {
				return nil, fmt.Errorf("quota_key_service: resolve real supply models for upstream %q: %w", qk.UpstreamKey, rerr)
			} else if len(rm) > 0 {
				models = rm
			}
		}
		entryModels = models
	}
	groups, err := tx.Group.Query().
		Where(
			group.Status(domain.StatusActive),
			group.DeletedAtIsNil(),
		).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("quota_key_service: query candidate groups: %w", err)
	}
	targets := cursorMountTargetGroupsForCodeLevel(groups, entryModels)
	out := make([]int64, 0, len(targets))
	for _, g := range targets {
		out = append(out, g.ID)
	}
	if len(out) == 0 {
		return nil, ErrQuotaKeyModelNoCoverage
	}
	return out, nil
}

// cursorMountTargetGroupsForCodeLevel 是 findCursorPlatformGroupsForKey（GroupIds==nil 的码级
// model_mapping 推导路径）专用的分组判定出口。语义与 cursorMountTargetGroups 完全一致：平台边界
// ∪ 白名单启用 ∪ 实际覆盖；差异仅在"厂商解析"数据源——cursorMountTargetGroups 仅依据实供清单
// （CursorRealSupplyVendorByID），而码级 model_mapping 的精确键可能是【官方名】（如
// claude-3-5-sonnet / claude-3-7-sonnet），这些名不在实供清单内、无 real-supply 厂商，
// cursorMountTargetGroups 据此无法推导平台、命中不了任何平台组。
//
// 本函数在实供厂商解析之外补入【官方目录厂商】（CursorModelCatalog），使官方名键也能落到其真实
// 厂商原生平台组（anthropic 家族官方名 → anthropic 平台组）。实供模型仍优先走实供厂商，故对
// 实供模型的判定结果与 cursorMountTargetGroups 恒等——零回归、零行为漂移。
// cursorMountTargetGroups 本身保持零改动（禁区不动）。
func cursorMountTargetGroupsForCodeLevel(groups []*ent.Group, entryModels []string) []*ent.Group {
	if len(entryModels) == 0 {
		return nil
	}
	// 厂商解析：实供清单优先，官方目录兜底（覆盖不在实供清单的官方名键）。
	rsVendor := CursorRealSupplyVendorByID()
	catalogVendor := make(map[string]string, len(CursorModelCatalog))
	for _, e := range CursorModelCatalog {
		if e.ID != "" {
			catalogVendor[e.ID] = e.Vendor
		}
	}
	resolvePlatform := func(m string) string {
		if v, ok := rsVendor[m]; ok {
			return cursorModelPlatformByVendor(v)
		}
		if v, ok := catalogVendor[m]; ok {
			return cursorModelPlatformByVendor(v)
		}
		return ""
	}
	modelPlatforms := make(map[string]struct{}, len(entryModels))
	for _, m := range entryModels {
		if p := resolvePlatform(m); p != "" {
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
		// 聚合组沿用既有承载语义。
		if platform != PlatformComposite && platform != PlatformCursor {
			if _, ok := modelPlatforms[platform]; !ok {
				continue // 不是任一实供/官方模型所属平台的目标分组
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
			if platform == PlatformComposite || platform == PlatformCursor || resolvePlatform(m) == platform {
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

// codeLevelEntryModels 把激活码码级 model_mapping 的键展开为"实供模型清单"，供
// cursorMountTargetGroups 判定挂载目标分组（零改动，仅在此派生模型集）：
//
//   - 精确键（不含末尾 *）：原样纳入（供分组白名单 Allows 命中），并补入该键所属厂商在
//     CursorRealSupplyModels 中的全部实供模型——保证平台推导能落到该厂商原生平台组，即便该
//     精确键本身不在实供清单内（如官方名 claude-3-5-sonnet 与实供名 claude-opus-5-5 同属
//     anthropic 厂商，前者不入实供目录、后者入，二者都应只挂 anthropic 家族组）；
//   - 末尾 * 通配键：按前缀展开到 CursorRealSupplyModels 命中 ID（如 claude-* → 全部 claude
//     实供模型，归 anthropic 厂商）。
//
// 空 model_mapping 返回 nil（调用方回落上游自报 / 静态实供清单）。
func codeLevelEntryModels(qk *ent.QuotaKey) []string {
	if len(qk.ModelMapping) == 0 {
		return nil
	}
	// 全量官方目录 → 厂商（覆盖不在实供清单内的官方名，用于把精确键解析到厂商）。
	catalogVendor := make(map[string]string, len(CursorModelCatalog))
	for _, e := range CursorModelCatalog {
		if e.ID != "" {
			catalogVendor[e.ID] = e.Vendor
		}
	}
	// 厂商 → 实供模型（用于补入同厂商实供模型，确保平台推导命中）。
	realSupplyByVendor := make(map[string][]string)
	for id, vendor := range CursorRealSupplyVendorByID() {
		realSupplyByVendor[vendor] = append(realSupplyByVendor[vendor], id)
	}
	seen := make(map[string]struct{})
	out := make([]string, 0, len(qk.ModelMapping)*2)
	add := func(m string) {
		if _, ok := seen[m]; !ok {
			seen[m] = struct{}{}
			out = append(out, m)
		}
	}
	for key := range qk.ModelMapping {
		if !strings.HasSuffix(key, "*") {
			add(key) // 精确键本身（供分组白名单 Allows 命中）
			if v, ok := catalogVendor[key]; ok {
				for _, rm := range realSupplyByVendor[v] {
					add(rm) // 同厂商实供模型（供平台推导）
				}
			}
			continue
		}
		// 末尾 * 通配：展开到命中实供模型 ID。
		prefix := key[:len(key)-1]
		for id := range CursorRealSupplyVendorByID() {
			if strings.HasPrefix(id, prefix) {
				add(id)
			}
		}
	}
	return out
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
		Notes:            qk.Notes,
		Priority:         qk.Priority,
		UpstreamKey:      qk.UpstreamKey,
		// 码级设置：唯一投影点，自动覆盖 List / ListWithUsageProjections / GetStatus /
		// GetWithAccounts / Create / Update（它们共用 quotaKeyToStatus）。
		Concurrency:    qk.Concurrency,
		LoadFactor:     qk.LoadFactor,
		RateMultiplier: qk.RateMultiplier,
		// ModelMapping 深拷贝，避免调用方持有实体内部 map 后被后台 goroutine 修改。
		ModelMapping: copyStringMap(qk.ModelMapping),
		// GroupIDs 投影：深拷贝实体切片，nil 保持 nil，使"未指定"与"空分组"可区分。
		GroupIDs: copyInt64SlicePtr(qk.GroupIds),
	}
}

// copyStringMap 返回 map[string]string 的浅拷贝（值均为不可变字符串）。
// 传入 nil 时返回 nil，避免把 nil 翻转为空 map（保持"未设置"语义）。
func copyStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// copyInt64SlicePtr 深拷贝 []int64，避免调用方改到实体内部切片；nil 切片保持 nil。
// 用于 quotaKeyToStatus 投影 group_ids：区分"未指定（nil）"与"显式空分组（空切片）"。
func copyInt64SlicePtr(v []int64) *[]int64 {
	if v == nil {
		return nil
	}
	out := make([]int64, len(v))
	copy(out, v)
	return &out
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

	// upstream 是上游客户端分派器（KC-26）：供 ListCandidateKeys 按每张激活码的
	// upstream_key 取"每码目标账号数"，使 kn-cy（1 卡 1 号）不再反复补池。
	// 生产由 wire 在 ProvideQuotaKeyReconciler 装配点注入；为 nil 时回落到
	// CursorAutoPullAccountsPerKey（与既有 shareai 口径一致），不静默按未知上游处理。
	upstream QuotaKeyUpstreamClient
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
	// key 终态门（I2）+ 真实来源剩余（§25.6 同一来源门）：来源被否决的账号不计入水位。
	// 来源不可用（含读不到快照，fail-closed）一律不计数，与调度候选粗筛同谓词；
	// 快照缺失等"按既有缺失规则"的情形不在此否决，账号仍计（与既有水位语义一致）。
	if reason := cursorSupplySourceAvailabilityVetoReasonString(ctx, candidate); reason != "" {
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
			// 准入判定（isAccountAdmittedForPlatform）的组门优先从 ctxkey.Group 取组；水位链路
			// 本身已持有该组行，按 handler 注入"认证时刻分组"的同一口径写入上下文，无需回源。
			admissionCtx := context.WithValue(ctx, ctxkey.Group, waterLevelAdmissionGroup(g))
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
				// 显式 route 把请求改向某平台后，该组合是否计入完全消费调度器的唯一准入判定
				// （isAccountAdmittedForPlatform：平台门 + cursor 跨平台桥接 + 来源门 + 分组白名单），
				// 不再另写"cursor 可服务目标"的第二份规则——两份实现必然分叉。
				// 本门只施加于显式改向组合：未显式改向（detector/ownership 命中、gemini default）
				// 沿用既有准入，不扩围。
				if decision.Source == CompositeRouteSourceExplicit &&
					!w.gateway.isAccountAdmittedForPlatform(admissionCtx, candidate, target, false, model, &gid) {
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

// waterLevelAdmissionGroup 把水位链路已持有的 ent 分组投影成准入判定读取的分组上下文
// （ctxkey.Group）。字段与 IsGroupContextValid 的要求同源：ID/Platform/Status 必填，
// Hydrated 表示该组来自可信存储（此处即数据库行读取结果），分组模型白名单走既有转换。
func waterLevelAdmissionGroup(g *ent.Group) *Group {
	if g == nil {
		return nil
	}
	return &Group{
		ID:             g.ID,
		Platform:       g.Platform,
		Status:         g.Status,
		Hydrated:       true,
		ModelAllowlist: GroupModelAllowlistFromDomain(g.ModelAllowlist),
	}
}

// isCooldownRefreshError 判定该 key 的 last_refresh_error 是否为【冷却】类错误。
// 判据是错误分类（错误链是否包 ErrUpstreamCooldown），而非时间：冷却错误由 writeCooldownMarker
// 以 fmt.Errorf("%w: ...", ErrUpstreamCooldown, ...) 写入，故以哨兵前缀识别。
//
// 注意（2026-11-06 起）：本函数**不再参与 ListCandidateKeys 的候选过滤**——用户裁定
// 拉号只看号数，冷却窗口挡拉号属于"用推断状态挡住号不够的码"，已删除。现仅用于
// 错误分类与展示层判断（如日志、看板区分限流与真失败）。
func isCooldownRefreshError(errStr *string) bool {
	if errStr == nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(*errStr), ErrUpstreamCooldown.Error())
}

// isDeadKeyRefreshError 判定最近一次拉号错误是否是「密钥级死码」——上游明确说这个码
// 不存在 / 无效 / 已过期。这类码再拉一万次也拉不出账号，只会每轮给上游送一次无效请求、
// 并把 reconcile 心跳写成失败，所以在候选筛选阶段直接排除，等人工换码。
//
// 与冷却类严格区分：冷却窗口过了会自动回候选；死码不回，因为状态不会自己变好。
// 与"未知失败"也严格区分（CUR-P1-FIX-KEY-EXHAUSTED）：未知失败仍留候选，不得据此停供。
//
// 匹配的是上游原文片段（shareai 的 code=1 文案），故用包含匹配而非前缀匹配。
func isDeadKeyRefreshError(errStr *string) bool {
	if errStr == nil {
		return false
	}
	s := strings.ToLower(strings.TrimSpace(*errStr))
	if s == "" {
		return false
	}
	for _, marker := range []string{
		"密钥不存在",
		"无效的密钥",
		"密钥已过期",
		"key not found",
		"invalid key",
		"key expired",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// ListCandidateKeys 返回池级（不按模型收窄）需要补池的候选 quota_key ID。
//
// 判据（2026-11-10 派发单 c1p1-A，收敛为「非软删总行数 total」）：
//
//	这个码名下还有几个【未软删】账号：少于 CursorAutoPullAccountsPerKey(2) 个就入选，
//	够 2 个（1 主 1 热备）就不入选。计数口径为「非软删总行数」（含停调 / 待恢复），
//	不再数"可用数"——停调号也占水位，避免热备码被反复补超过目标数。
//
// 历史：2026-11-06 用户裁定数"可用账号"（未软删 + schedulable + active + 无未到期停调）；
// 2026-11-10 收敛为 total 行数（用户裁定 1 主 1 热备）。
//
// 码级额度耗尽门（2026-11-10 新增；c1p1-G 修正）：来源额度快照最近一次成功观测确认真实剩余
// 已触底时，本轮不对该码发起自动拉号（cursorQuotaKeyPullBlocked，与 sweepQuotaKeyLevelStops /
// 来源门同源常量与窗口）；失败关闭——拿不到"最近成功观测确认耗尽"证据时放行，只有明确耗尽才跳过；
// 与 sweep 不同，快照陈旧也不放行（沿用最近成功结论，宁可少拉）。
//
// 保留的排除项不是"业务条件"，而是"这个码还成立"的前提：
//   - status 非 active / exhausted / 已软删 / 已过期：这些码拉号必失败；
//   - 密钥级死码（上游明确说该密钥不存在 / 无效 / 已过期）：拉一万次也拉不出号，等人工换码；
//   - 未知上游标识：失败关闭，不参与自动补池。
//
// 优先级顺序（用户裁定 2026-11-03）：候选按 quota_keys.priority 升序（数值越小越优先），
// 同优先级按 id 升序稳定输出。这样补池时先拉"优先码"（日限额码）的号，月总限额码排在后面
// 作为兜底；配合账号入库优先级跟随码优先级，调度层同样先消耗优先码的号。
func (w *quotaKeyModelWater) ListCandidateKeys(ctx context.Context, _ string, limit int) ([]int64, error) {
	now := time.Now()
	keys, err := w.client.QuotaKey.Query().
		Order(ent.Asc(quotakey.FieldPriority), ent.Asc(quotakey.FieldID)).
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
		if isDeadKeyRefreshError(k.LastRefreshError) {
			// 密钥级死码（上游说这个码不存在/无效/已过期）：拉一万次也拉不出号，
			// 只会每轮给上游送一次无效请求并把心跳写成失败。等人工换码。
			continue
		}
		// 显式解绑全部分组的码不参与自动补池（拉号必然无目标组，纯浪费每日额度）。
		// 注意与 pull 内的"发上游前拦截 group_ids==[]"互为双保险：此处减少进入 pull 的候选，
		// 但 reconciler pending wakeup 可绕过本过滤直进 pull，故 pull 内仍有拦截点。
		if k.GroupIds != nil && len(k.GroupIds) == 0 {
			continue
		}
		// 新增「码级额度耗尽门」（2026-11-10 派发单 c1p1-A；2026-11-10 派发单 c1p1-G 修正）：
		// 来源额度快照一旦最近一次成功观测确认真实剩余已触底（remaining < cursorQuotaKeyRemainingFloor），
		// 本轮不再对该码发起自动拉号——拉出来的号也跑不动，只会浪费上游每日拉号额度、
		// 并撞"密钥额度耗尽"失败心跳。与 sweepQuotaKeyLevelStops / cursorSupplySourceUsageVetoReason
		// 使用同一组常量（cursorQuotaKeyRemainingFloor）。失败关闭：任何拿不到"最近成功观测确认耗尽"
		// 证据的情形（解析失败、观测失败、字段缺失）都放行（让既有水位/人工路径继续工作），只有明确
		// 耗尽才跳过。与 sweep 不同，本门不因快照陈旧而放行：宁可少拉、不浪费每日拉号额度，因此沿用
		// 最近成功结论。
		if cursorQuotaKeyPullBlocked(k.UsageSnapshot, now) {
			continue
		}
		// 2026-11-10 派发单 c1p1-A：候选计数由「可用数 working」收敛为「非软删总行数 total」。
		// 背景：实测单号并发峰值 1，「1 主 1 热备」即可满足，多号只增加管理面与失败轮转面；
		// 用"非软删总行数"而非"可用数"是把停调/待恢复的号也计入水位，避免热备码被反复补到
		// 超过目标数。数的是该码名下所有未软删账号（含停调、含待恢复），达到目标即不补。
		// 原"5 分钟冷却窗口内不入选"过滤此前已删除；死码过滤保留（上游明确说密钥作废，不是推断状态）。
		total, terr := w.client.Account.Query().
			Where(
				account.QuotaKeyID(k.ID),
				account.DeletedAtIsNil(),
			).
			Count(ctx)
		if terr != nil {
			return nil, fmt.Errorf("quota_key_model_water: count total for key %d: %w", k.ID, terr)
		}
		// 自动补位的目标按"该码所属上游"分派（KC-26）：shareai=5、kn-cy=1（1 卡 1 号）。
		// upstream 未装配（测试/未注入）时回落到 CursorAutoPullAccountsPerKey（与既有 shareai 口径一致）；
		// 未知上游标识失败关闭：该码不参与自动补池（跳过），绝不静默按 shareai 的 5 处理。
		// 注意这里用的是"自动补位目标"而不是 MaxAccountsPerQuotaKey（人工护栏）：
		// 人工拉到 8 个的码不该被自动逻辑当成"已满"，但也不该继续自动补。
		target := CursorAutoPullAccountsPerKey
		if w.upstream != nil {
			if t, terr := w.upstream.TargetAccountsPerKeyFor(k.UpstreamKey); terr != nil {
				// 未知上游标识：失败关闭，不参与自动补池。
				continue
			} else {
				target = t
			}
		}
		if total >= target {
			continue // 自动补位名额已满：跳过
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

// cursorQuotaKeyPullBlocked 返回 true 表示本轮不应对该码发起自动拉号。
//
// 与 sweepQuotaKeyLevelStops 的区别（外审 must_fix #3，派发单 c1p1-G）：
//   - sweep 用新鲜度窗口来“避免变更账号既有停调状态”——它只在能确认新鲜结论时才动账号态，
//     陈旧快照就保持原状、不做变更；
//   - 本门只决定“是否发起一次新的自动拉号”。宁可少拉、不浪费上游每日拉号额度，因此沿用
//     最近一次成功观测的 total/used 结论，陈旧与否不再放行：
//     陈旧但曾确认耗尽（remaining < floor）→ 继续阻止补池；
//     陈旧但曾确认健康（remaining >= floor）→ 放行；
//
// 失败关闭（不下结论、放行）的情形：解析失败、观测失败（ErrorCode 非空）、关键字段缺失
// （Total/Used/CheckedAt 任一为 nil）。这些情形下拿不到“最近成功观测确认耗尽”的证据，
// 留由既有水位/人工路径继续工作，只有明确（曾确认）耗尽才跳过。
func cursorQuotaKeyPullBlocked(raw json.RawMessage, now time.Time) bool {
	_ = now // 本门沿用最近成功结论，不再按新鲜度放行，now 仅保留签名兼容。
	snap, ok := ParseQuotaKeyUsageSnapshot(raw)
	if !ok || snap.ErrorCode != "" || snap.Total == nil || snap.Used == nil || snap.CheckedAt == nil {
		return false
	}
	return (*snap.Total - *snap.Used) < cursorQuotaKeyRemainingFloor
}
