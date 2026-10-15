package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

	// insertFailureMarkerWriter 是 R5-1 插入失败后"独立事务补写 last_refresh_error 标记"
	// 的可替换执行点（与生产默认 writeInsertFailureMarker 同签名）。测试可替换以注入
	// 标记写失败（R5-1 b：仅本轮哨兵停轮的边界），与生产 groupResolver 同一类测试 seams。
	insertFailureMarkerWriter func(ctx context.Context, quotaKeyID int64, cause error) error

	// reconciler 是软删后水位快路径唤醒目标（C12-v2）。通过 SetReconciler 注入（wire 装配），
	// 软删事务提交成功后调用其 NotifyAccountRemoved 触发 reconciler 重判水位；nil 时跳过
	// （未装配则不触发快路径，退回 C10 周期补池）。
	reconciler quotaKeyReconcilerNotifier
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
	s.insertFailureMarkerWriter = s.writeInsertFailureMarker
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
// 该 2 参入口不携带模型上下文（reconciler / 手动触发共用，保持与 quotaKeyPuller 窄接口
// 兼容），故不执行 §13.3 分组挂载；挂载语义经 PullAccountsForModel 暴露。
func (s *QuotaKeyService) PullAccounts(ctx context.Context, quotaKeyID int64) error {
	return s.pull(ctx, quotaKeyID, "")
}

// PullAccountsForModel 携带模型 M 上下文的拉号入口（§13.2 触发方传入 / §13.3 挂载）。
// 成功拉号后，新号在落库事务提交前自动挂入"白名单含 M"的全部分组；M 无覆盖则失败关闭
// （回滚、不落裸号）。其内部复用与 PullAccounts 完全相同的 pull 状态机（同一 advisory
// 互斥域 + 同一事务 + ≤15 强制），不另造第二拉号机制。
func (s *QuotaKeyService) PullAccountsForModel(ctx context.Context, quotaKeyID int64, modelM string) error {
	return s.pull(ctx, quotaKeyID, modelM)
}

// RefreshAccounts 手动触发拉号（§4.4），与 PullAccounts 共享同一 advisory 互斥域。
// 手动入口不携带模型上下文，故不执行 §13.3 分组挂载。
func (s *QuotaKeyService) RefreshAccounts(ctx context.Context, quotaKeyID int64) error {
	return s.pull(ctx, quotaKeyID, "")
}

// pull 是 PullAccounts / PullAccountsForModel / RefreshAccounts 共享的拉号状态机实现。
// modelM 非空时启用 §13.3 挂载：新号提交前挂入白名单含 M 的全部分组；为空则跳过挂载。
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
func (s *QuotaKeyService) pull(ctx context.Context, quotaKeyID int64, modelM string) error {
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
			if errors.Is(pullErr, ErrUpstreamCooldown) {
				if werr := s.writeCooldownMarker(ctx, tx, quotaKeyID, pullErr); werr != nil {
					return fmt.Errorf("quota_key_service: write cooldown marker: %w", werr)
				}
				resultErr = ErrUpstreamCooldown
				return nil
			}
			// 其余上游错误：CAS 写 exhausted=true（沿用既有语义），不向上抛。
			if cerr := s.casSetExhausted(ctx, tx, quotaKeyID, pullErr); cerr != nil {
				return fmt.Errorf("quota_key_service: cas set exhausted: %w", cerr)
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
		insertedIDs := make([]int64, 0, len(result.Accounts))
		for i, ua := range result.Accounts {
			if i >= maxInsert {
				break
			}
			create := tx.Account.Create().
				SetName(ua.Name).
				SetPlatform(ua.Platform).
				SetType(ua.Type).
				SetCredentials(ua.Credentials).
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

		// §13.3 新号挂载：M 上下文由触发方传入落库事务，提交前挂入"白名单含 M"的全部分组。
		// modelM 为空表示触发方未携带模型上下文（既有手动/默认路径），跳过挂载。
		if modelM != "" {
			groupIDs, err := s.groupResolver(ctx, tx, modelM)
			if err != nil {
				// F4：上游已成功发号（号未入库，事务尚未提交），但本地分组解析失败
				//（shareai 非幂等，上游发号不可回滚）→ 事务将回滚（号不入库）+ 与挂载失败
				// 同哨兵 ErrQuotaKeyMountFailed 返回，调用方可识别"上游可能已被消耗一次"。
				return fmt.Errorf("quota_key_service: resolve covering groups for model %q: %w: %w", modelM, ErrQuotaKeyMountFailed, err)
			}
			if len(groupIDs) == 0 {
				// 失败关闭（fail-closed）：M 无分组覆盖，不得落成裸号；返回错误使事务回滚，
				// 号不入库。触发方据此记录告警。
				//
				// F4'（2026-10-15 增量复审 R2）：本分支发生在【上游发号成功之后】——白名单
				// 与上游发号不同快照，期间可变更，覆盖可能恰好此刻消失。上游号已消耗且不可
				// 回滚（shareai 非幂等），故除 ErrQuotaKeyModelNoCoverage 外同时补挂
				// ErrQuotaKeyMountFailed 哨兵（%w 双链，两个 errors.Is 皆为真），使触发方
				// 走"非幂等边界 = 告警 + 停轮"，不得当作普通错误继续拉下一个候选。
				return fmt.Errorf("quota_key_service: model %q has no covering group after upstream success: %w: %w",
					modelM, ErrQuotaKeyMountFailed, ErrQuotaKeyModelNoCoverage)
			}
			// 上游成功、本地挂载失败（shareai 非幂等，上游发号不可回滚）→ 本地事务回滚
			//（号不入库）+ 可识别错误供触发方记录"上游可能已被消耗一次"告警；不得表述为
			//"拉号动作整体回滚"。
			for _, accID := range insertedIDs {
				if err := s.mountAccountToGroups(ctx, tx, accID, groupIDs); err != nil {
					return fmt.Errorf("quota_key_service: mount account %d to groups: %w: %w", accID, ErrQuotaKeyMountFailed, err)
				}
			}
		}

		// 清 exhausted 与错误占位。
		// SetNillableLastRefreshError(nil) 对 nil 指针是 no-op（仅 v != nil 时才写），
		// 必须用 ClearLastRefreshError() 才能真正把 last_refresh_error 置空。
		if _, err := tx.QuotaKey.UpdateOneID(quotaKeyID).
			SetExhausted(false).
			SetLastRefreshAt(time.Now()).
			ClearLastRefreshError().
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

// casSetExhausted 仅在当前 status='active' 时把 exhausted 写为 true（§3.4 CAS 写）。
// 若 sweep 已将状态改为 expired/disabled，则 0 行受影响，exhausted 不被覆盖。
func (s *QuotaKeyService) casSetExhausted(ctx context.Context, tx *ent.Tx, quotaKeyID int64, pullErr error) error {
	_, err := tx.QuotaKey.Update().
		Where(quotakey.IDEQ(quotaKeyID), quotakey.Status(domain.QuotaKeyStatusActive)).
		SetExhausted(true).
		SetLastRefreshAt(time.Now()).
		SetLastRefreshError(pullErr.Error()).
		Save(ctx)
	if err != nil {
		return err
	}
	return nil
}

// writeCooldownMarker 在冷却分支（ErrUpstreamCooldown）同事务写 last_refresh_error +
// last_refresh_at，但【不写 exhausted】（R1 F1 语义：冷却豁免 exhausted）。沿用 casSetExhausted
// 的 CAS 约束：仅当当前 status='active' 才写入（不覆盖 sweep 已置的禁用/过期），否则 0 行。
//
// R4-1 起本函数复用为"落库失败（上游成功、本地失败）"标记的同一写入实现：两者同型
// （同事务 / CAS 仅 active / 唯独豁免 exhausted），仅语义不同（冷却 vs 需人工介入）。
// 冷却 5min 时间窗判定不在本函数内，故复用不触碰冷却语义。
func (s *QuotaKeyService) writeCooldownMarker(ctx context.Context, tx *ent.Tx, quotaKeyID int64, pullErr error) error {
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

// writeInsertFailureMarker 在【独立事务】内写 last_refresh_error 标记（R5-1）。
//
// 与"插入失败即同事务写标记后提交"的旧实现不同：多账号部分成功时，若在同一容量事务内写
// 标记后提交，会连带把【已部分插入但本应回滚的账号】一起提交成裸号（R5-1 缺陷形态）。故
// 改为两段编排——pull 先在容量事务内整体回滚（号不入库），再以本独立事务补写标记，使
// "回滚全部账号"与"标记持久化"两目标同时成立。
//
// 语义约束（零新表/字段）：仅写 last_refresh_error + last_refresh_at，CAS 仅当 status='active'
// （不覆盖 sweep 已置的禁用/过期），【不写 exhausted】（与 writeCooldownMarker 同豁免口径，
// 与"上游失败"的 exhausted 占位语义区分）。标记写失败（如 CAS 0 行 / DB 错误）由调用方
// 降格为"仅本轮哨兵停轮"（R5-2 边界）。
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

// findGroupsAllowingModel 是 §13.3 挂载事实源：返回"白名单含模型 M"的全部分组 ID。
// 白名单事实源 = 既有分组模型白名单（domain.GroupModelAllowlist），不新造映射表；
// 复用 service.GroupModelAllowlist.Allows 的既有通配/前缀匹配语义。仅取未软删分组。
func (s *QuotaKeyService) findGroupsAllowingModel(ctx context.Context, tx *ent.Tx, modelM string) ([]int64, error) {
	groups, err := tx.Group.Query().All(ctx)
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
}

// NewQuotaKeyModelWater 构造生产用模型水位提供方（F1 装配点由 wire 注入 *ent.Client）。
// 返回接口类型，使 reconciler 侧仅依赖窄面。
func NewQuotaKeyModelWater(client *ent.Client) QuotaKeyModelWaterProvider {
	return &quotaKeyModelWater{client: client}
}

// ListAllowedModels 枚举分组白名单覆盖的 cursor 服务模型：
// 遍历 active 且未软删的【cursor 平台】分组，收集其启用白名单中的全部模型条目（去重保序）。
// 限定 platform=cursor：补池只发 cursor 账号（§13.3 挂载与水位口径均为 cursor 账号），
// 若把 anthropic 等其它平台分组的白名单也枚举进来，会给与 cursor 无关的型号各补一批号。
// 返回的是白名单条目（可能含通配，如 claude-*）；WaterLevel 按同一 Allows 口径判定。
func (w *quotaKeyModelWater) ListAllowedModels(ctx context.Context) ([]string, error) {
	groups, err := w.client.Group.Query().
		Where(
			group.Platform(domain.PlatformCursor),
			group.Status(domain.StatusActive),
			group.DeletedAtIsNil(),
		).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("quota_key_model_water: list groups: %w", err)
	}
	seen := make(map[string]struct{}, 8)
	out := make([]string, 0, 8)
	for _, g := range groups {
		aw := GroupModelAllowlistFromDomain(g.ModelAllowlist)
		if !aw.Enabled {
			continue
		}
		for _, m := range aw.Models {
			k := strings.ToLower(strings.TrimSpace(m))
			if k == "" {
				continue
			}
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, m)
		}
	}
	return out, nil
}

// WaterLevel 返回模型 M 的当前水位（候选计数）与统一阈值。
// 口径（§13.1）：account.platform=cursor + 可调度 + 正常(active) + 未软删 +
// 当前允许 M 分组（白名单 Allows(M) 的任意未软删活跃【cursor 平台】分组）+ 非 temp 窗口
// （temp_unschedulable_until 为空或早于 now）。阈值恒为 quotaKeyWaterLevelThreshold。
//
// R3-2：分组侧同样限定 platform=cursor，与 ListAllowedModels 的 cursor 范围对齐——
// §13.1"当前允许 M 的分组"指 cursor 服务分组；跨平台（anthropic 等）的同名模型分组
// 不计入水位，不得让与其无关的账号虚增 cursor 水位（否则水位虚高 → 该补的号不补）。
func (w *quotaKeyModelWater) WaterLevel(ctx context.Context, modelM string) (int, int, error) {
	now := time.Now()
	// 当前允许 M 的 cursor 分组 ID 集合（同 Allows 判定，平台范围同 ListAllowedModels）。
	groups, err := w.client.Group.Query().
		Where(
			group.Platform(domain.PlatformCursor),
			group.Status(domain.StatusActive),
			group.DeletedAtIsNil(),
		).
		All(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("quota_key_model_water: list groups: %w", err)
	}
	gids := make([]int64, 0, len(groups))
	for _, g := range groups {
		if GroupModelAllowlistFromDomain(g.ModelAllowlist).Allows(modelM) {
			gids = append(gids, g.ID)
		}
	}
	current := 0
	if len(gids) > 0 {
		current, err = w.client.Account.Query().
			Where(
				account.Platform(domain.PlatformCursor),
				account.Schedulable(true),
				account.Status(domain.StatusActive),
				account.DeletedAtIsNil(),
				account.Or(
					account.TempUnschedulableUntilIsNil(),
					account.TempUnschedulableUntilLT(now),
				),
				account.HasGroupsWith(group.IDIn(gids...)),
			).
			Count(ctx)
		if err != nil {
			return 0, 0, fmt.Errorf("quota_key_model_water: count water level: %w", err)
		}
	}
	return current, quotaKeyWaterLevelThreshold, nil
}

// ListCandidateKeys 返回模型 M 下需要补池的候选 quota_key ID：
// active + 非 exhausted + 非冷却中（冷却只写 last_refresh_error、不写 exhausted，
// 故以 last_refresh_error 为空代理"非冷却中"）+ 未过期 + 未软删 + 有剩余容量
// （working < MaxAccountsPerQuotaKey）。limit>0 时由实现方按需截断（差额）；
// 模型 M 不收窄候选集——挂载语义由 PullAccountsForModel 决定目标分组。
func (w *quotaKeyModelWater) ListCandidateKeys(ctx context.Context, _ string, limit int) ([]int64, error) {
	now := time.Now()
	keys, err := w.client.QuotaKey.Query().
		Where(
			quotakey.Status(domain.QuotaKeyStatusActive),
			quotakey.Exhausted(false),
			quotakey.LastRefreshErrorIsNil(),
			quotakey.Or(quotakey.ExpiresAtIsNil(), quotakey.ExpiresAtGT(now)),
			quotakey.DeletedAtIsNil(),
		).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("quota_key_model_water: list candidate keys: %w", err)
	}
	out := make([]int64, 0, len(keys))
	for _, k := range keys {
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
