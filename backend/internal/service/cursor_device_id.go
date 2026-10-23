package service

// 本文件实现 cursor 出站鉴权头 x-jg-auth 中 `d`（deviceId）的站点侧生成与读取（B-10）。
//
// 事实基础（插件 jg.cursor-free 反混淆源码，取证目录
// ~/.sub2api-acceptance/cursor-20261013/02-webcrack-out/deobfuscated.js）：
//
//	deobfuscated.js:8131-8150  getDeviceId()
//	  读 ~/.cursor_free_data 的 deviceId → 读 globalState → 都没有则
//	  (0, uuid.v4)() 生成随机 uuid，再 hashDeviceId(uuid)，写入文件/globalState 后永久复用。
//	deobfuscated.js:8070-8077  hashDeviceId(uuid)
//	  let a = 0;
//	  for (i...) { a = (a << 5) - a + s.charCodeAt(i); a &= a; }
//	  return Math.abs(a).toString(16).padStart(8,"0").substring(0,8).toUpperCase();
//	deobfuscated.js:8090-8097  generateEncryptedCredentials(deviceId, activationCode)
//	  wasm encryptLocalData(JSON.stringify({d: deviceId, a: activationCode}))
//
// 用户裁定（2026-10-16）：**站点随机生成并落库**，每激活码一份，首次需要时生成、之后复用。
// 绝不按激活码派生（该方向已被 D8 推翻），也不依赖本机插件产物。
//
// ⚠️ 与 deviceID(code)（cursor_upstream_client.go）是两个不同用途，严禁混用：
//   - deviceID(code) → 取号链 POST /api/refresh 的 `device-id` 请求头，本文件不使用它；
//   - 本文件的 deviceId → 出站 x-jg-auth 的 `d`。

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"unicode/utf16"

	"github.com/Wei-Shaw/sub2api/ent"
)

// ErrCursorDeviceIDUnavailable 表示无法为激活码解析出可用的 deviceId（失败关闭）。
//
// 出站鉴权材料不可得必须失败关闭：带着空 deviceId 发出的请求必然被上游拒绝，
// 且会白耗 shareai 额度。故本卡不提供任何兜底默认值。
var ErrCursorDeviceIDUnavailable = errors.New("cursor: device id unavailable (fail closed)")

// CursorDeviceIDResolver 为出站鉴权解析指定激活码的 deviceId。
//
// 只抽这一个方法（而非让调用方依赖整个 *QuotaKeyService）：E2 接线侧只消费本方法，
// 测试替身也无须构造整个 QuotaKeyService。*QuotaKeyService 天然满足本接口，
// 故生产装配可直接注入它。
type CursorDeviceIDResolver interface {
	DeviceIDFor(ctx context.Context, quotaKeyID int64) (string, error)
}

// 编译期断言：生产实现满足上述窄接口。
var _ CursorDeviceIDResolver = (*QuotaKeyService)(nil)

// hashDeviceIDFromUUID 逐字复刻插件 hashDeviceId（deobfuscated.js:8070-8077）。
//
// 与历史实现（cursor_auto_entry_service.go 旧 hashDeviceID，已随 §25.14-C 删除）的差异**不止一处**，
// 历史实现对非 ASCII 输入与插件不等价，故独立实现并单测锁定：
//
//  1. 迭代单位：插件 s.charCodeAt(i) 遍历 **UTF-16 码元**；既有实现 `for _, c := range`
//     遍历 **Unicode 码点**。ASCII 下两者一致，但 BMP 外字符（如 emoji，UTF-16 为代理对
//     2 个码元）会分叉：😀 插件得 001B0D63，既有实现得 0001F600。
//
//  2. 乘法形式：插件 `(a<<5) - a + c`；既有实现 `h*31 + c`。二者对 int32 环绕数学等价
//     （(x<<5)-x == 31x，且两侧都在 int32 内环绕），非差异来源，但按插件字面照抄以免歧义。
//
//  3. 收尾取绝对值：插件 Math.abs 作用在**双精度**上，int32 最小值 -2147483648 取绝对值
//     得 2147483648（不溢出）；Go 若用 int32 取绝对值会溢出为自身，故此处显式转 int64。
//     这是唯一的语义性溢出陷阱。
//
//  4. 格式化：插件 toString(16).padStart(8,"0").substring(0,8).toUpperCase()。对 int32
//     量级的绝对值（≤ 0x80000000）十六进制最多 8 位，substring 不会截断；%08X 同时
//     保证至少 8 位并大写，与 padStart+substring+toUpperCase 等价。
func hashDeviceIDFromUUID(uuid string) string {
	// 转成 UTF-16 码元序列，对齐 JS 字符串的 charCodeAt 语义。
	units := utf16.Encode([]rune(uuid))
	var a int32
	for _, u := range units {
		a = (a << 5) - a + int32(u)
	}
	// int32 最小值取绝对值不溢出：显式提升到 int64 后再取绝对值（见上第 3 点）。
	v := int64(a)
	if v < 0 {
		v = -v
	}
	return fmt.Sprintf("%08X", uint32(v))
}

// randomUUIDv4 生成随机 UUID v4 字符串（crypto/rand），格式与插件 uuid.v4() 一致：
// 8-4-4-4-12 小写十六进制、version=4、variant=10xx。
//
// 用 crypto/rand 而非 math/rand：deviceId 是上游鉴权标识，预测性随机源会让
// 不同站点实例生成可猜测的相同标识。
func randomUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("cursor: read random bytes for device id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx
	return fmt.Sprintf("%x-%x-%x-%x-%x%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:12], b[12:16]), nil
}

// GenerateCursorDeviceID 生成一份新的随机 deviceId（uuidv4 → hashDeviceId）。
//
// 随机性来自 randomUUIDv4 的 crypto/rand，绝不由激活码派生（B-10 核心裁定）。
func GenerateCursorDeviceID() (string, error) {
	uuid, err := randomUUIDv4()
	if err != nil {
		return "", err
	}
	return hashDeviceIDFromUUID(uuid), nil
}

// DeviceIDFor 返回指定激活码的出站 deviceId；库内尚无则生成一份并落库。
//
// 首次并发防重靠 **DB 层条件更新**（SetDeviceIDIfAbsent：WHERE device_id IS NULL OR 空串）：
// 并发首次请求中只有把"未生成"改成非空的那一个事务会命中（受影响行数=1），其余
// 拿到 0 行后重读，取到赢家写入的值。因此同一激活码恒定只有一份 deviceId，
// 不会出现"两个请求各生成一个不同值、后写覆盖先写"的分裂。
//
// deviceId 落库后**永久复用**：这是插件语义（固化到 ~/.cursor_free_data），
// 也是上游能把同一份设备标识视为同一客户端的前提，故本方法在已有值时绝不重算。
//
// §25.14-C「设备解析与依赖」：本方法保持原契约（"无值就首写一份"），实现委托给同包的
// 低层本体 cursorDeviceIDFirstWrite；上层的三分支解析（持久值 → 存量派生 → 真新码首写）
// 见 cursorDeviceIdentityOf，不得把三分支语义塞回本方法。
func (s *QuotaKeyService) DeviceIDFor(ctx context.Context, quotaKeyID int64) (string, error) {
	if s == nil || s.repo == nil {
		return "", fmt.Errorf("%w: quota key service not configured", ErrCursorDeviceIDUnavailable)
	}
	return cursorDeviceIDFirstWrite(ctx, s.repo, quotaKeyID)
}

// cursorDeviceIDFirstWrite 是"首写 + 重读赢家值"的**低层本体**（接收 repo，不依赖
// QuotaKeyService，避免 AutoEntry 反向依赖服务造成 DI 环）。
//
// 只有"真新码"才允许走到这里（随机生成只出现在这一处）；存量码走派生值、已有持久
// device 直接用，都不进入本函数。首写发生在每 key 的 advisory/行锁事务**之前**，
// 保留 O01 修复（事务回滚不会丢掉已提交的设备身份）。
func cursorDeviceIDFirstWrite(ctx context.Context, repo QuotaKeyRepository, quotaKeyID int64) (string, error) {
	if repo == nil {
		return "", fmt.Errorf("%w: quota key repository not configured", ErrCursorDeviceIDUnavailable)
	}
	qk, err := repo.GetByID(ctx, quotaKeyID)
	if err != nil {
		// 双 %w：既保留底层原因（可判具体错误），也归入"鉴权材料不可得"这一
		// 失败关闭语义（出站侧只需认这个哨兵）。
		return "", fmt.Errorf("%w: load quota key %d: %w", ErrCursorDeviceIDUnavailable, quotaKeyID, err)
	}
	if qk == nil {
		return "", fmt.Errorf("%w: quota key %d not found", ErrCursorDeviceIDUnavailable, quotaKeyID)
	}
	// 已有值直接复用（稳态路径，不写库）。"已有"的判定与 SetDeviceIDIfAbsent 的
	// WHERE 条件必须逐字一致（NULL 或空串），否则会出现"这边认为已有、那边仍可写"
	// 的错配，进而在竞态分支读到刚被本请求自己写入的值。
	if qk.DeviceID != nil && *qk.DeviceID != "" {
		return *qk.DeviceID, nil
	}

	candidate, err := GenerateCursorDeviceID()
	if err != nil {
		return "", err
	}
	written, err := repo.SetDeviceIDIfAbsent(ctx, quotaKeyID, candidate)
	if err != nil {
		return "", fmt.Errorf("cursor: persist device id for quota key %d: %w", quotaKeyID, err)
	}
	if written == 1 {
		return candidate, nil
	}

	// 0 行 = 已有并发请求抢先写入（条件不满足）。重读取赢家的值，
	// 绝不返回本请求的候选值（那会造成同一激活码出现两份 deviceId）。
	settled, err := repo.GetByID(ctx, quotaKeyID)
	if err != nil {
		return "", fmt.Errorf("%w: re-read quota key %d after device id race: %v", ErrCursorDeviceIDUnavailable, quotaKeyID, err)
	}
	if settled == nil || settled.DeviceID == nil || *settled.DeviceID == "" {
		// 0 行却读不到值：说明条件更新的 WHERE 未被满足且库内仍为空，属异常态，
		// 失败关闭，绝不返回候选值让两个请求各持一份。
		return "", fmt.Errorf("%w: quota key %d device id still unset after conditional write", ErrCursorDeviceIDUnavailable, quotaKeyID)
	}
	return *settled.DeviceID, nil
}

// ---------------------------------------------------------------------------
// §25.14-C「设备解析与依赖」：三分支来源设备身份解析的**唯一实现**。
//
// 取号（pull 首写边界）、出站（cursor_upstream_client.resolveDeviceID）与
// Probe（CursorAutoEntryService 观测入口）共用本函数，不得各自再写一份分支：
//
//	① 已有持久 device_id → 直接复用（全链同值，不写库）；
//	② 无持久 device 但该码已有账号（存量）→ 复用原 deviceID(code)，**不写新身份**、
//	   不迁移（§16 红线：绝不自动改存量设备身份）；
//	③ 真新码（无设备、无账号）→ 沿既有 SetDeviceIDIfAbsent 条件首写、重读赢家值
//	   （随机生成只出现在这里）。
//
// 解析同时记下来源 MAC 指纹：调用方进锁（每 key advisory/行锁事务）后重读来源，
// MAC 不一致即"来源已被换码"，必须明确冲突并**零 lookup**，不拿旧明文继续观察。
// ---------------------------------------------------------------------------

// cursorDeviceSource 标识本次设备身份的来源分支（只用于日志/断言，不落库）。
type cursorDeviceSource string

const (
	// cursorDeviceSourcePersisted：库内已有持久 device_id（稳态路径）。
	cursorDeviceSourcePersisted cursorDeviceSource = "persisted"
	// cursorDeviceSourceDerived：存量码的确定性派生值 deviceID(code)，不写库。
	cursorDeviceSourceDerived cursorDeviceSource = "derived"
	// cursorDeviceSourceFirstWrite：真新码的首写路径（随机生成 + 条件写 + 重读赢家）。
	cursorDeviceSourceFirstWrite cursorDeviceSource = "first-write"
)

// cursorDeviceIdentity 是一次设备解析的结果。
type cursorDeviceIdentity struct {
	// DeviceID 是本次解析出的设备身份（取号/出站/lookup 全链同值）。
	DeviceID string
	// Source 是命中的分支（persisted / derived / first-write）。
	Source cursorDeviceSource
	// CodeMAC 是**解析时刻**来源凭据的 MAC 指纹；调用方进锁后重读来源并比对，
	// 不一致即换码冲突（零 lookup）。空串表示调用方未提供明文，不参与比对。
	CodeMAC string
}

// resolveCursorDeviceIdentity 是三分支解析的唯一入口（repo 版本）。
//
// code 为该来源的**明文**激活码：分支②的派生值需要它，分支③不需要（首写与明文无关）。
// 调用方必须传入明文；未解密成功不得进入本函数（否则分支②会拿到错误派生值）。
func resolveCursorDeviceIdentity(ctx context.Context, repo QuotaKeyRepository, qk *ent.QuotaKey, code string) (cursorDeviceIdentity, error) {
	if repo == nil {
		return cursorDeviceIdentity{}, fmt.Errorf("%w: quota key repository not configured", ErrCursorDeviceIDUnavailable)
	}
	return resolveCursorDeviceIdentityWith(ctx, qk, code,
		repositoryHasAccount(repo),
		func(ctx context.Context, id int64) (string, error) {
			return cursorDeviceIDFirstWrite(ctx, repo, id)
		},
	)
}

// resolveCursorDeviceIdentityWith 是三分支解析的本体：只依赖"是否已有账号"与
// "首写"两个窄回调，因此 QuotaKeyService 的窄面（出站客户端持有的 deviceResolver）
// 与 QuotaKeyRepository 都能接入同一份分支逻辑，不再各自复制一遍。
func resolveCursorDeviceIdentityWith(
	ctx context.Context,
	qk *ent.QuotaKey,
	code string,
	hasAccount func(context.Context, int64) (bool, error),
	firstWrite func(context.Context, int64) (string, error),
) (cursorDeviceIdentity, error) {
	branch, err := cursorDeviceBranch(ctx, qk, hasAccount)
	if err != nil {
		return cursorDeviceIdentity{}, err
	}
	identity := cursorDeviceIdentity{Source: branch, CodeMAC: quotaKeyMACOfPlaintext(code)}
	switch branch {
	case cursorDeviceSourcePersisted:
		// ① 已持久化 device_id → 全链复用（出站侧读同一列，恒同值）。
		identity.DeviceID = *qk.DeviceID
		return identity, nil
	case cursorDeviceSourceDerived:
		// ② 存量（已有账号）→ 保持既有派生值，不写库、不迁移。
		identity.DeviceID = deviceID(code)
		return identity, nil
	default:
		// ③ 真新码（无账号）→ 首写 + 重读赢家值（随机生成只在此分支）。
		if firstWrite == nil {
			return cursorDeviceIdentity{}, fmt.Errorf("%w: device first-write dependency not configured", ErrCursorDeviceIDUnavailable)
		}
		generated, ferr := firstWrite(ctx, qk.ID)
		if ferr != nil {
			return cursorDeviceIdentity{}, ferr
		}
		identity.DeviceID = generated
		return identity, nil
	}
}

// cursorDeviceBranch 是三分支**判定**的唯一实现（① 持久值 / ② 存量派生 / ③ 真新码首写）。
//
// 把它单独抽出来，是为了让不持有明文的调用方（取号链的设备首写边界：取号本来就不解密）
// 也能复用同一份判定，而不是各写一遍 if/else。
func cursorDeviceBranch(ctx context.Context, qk *ent.QuotaKey, hasAccount func(context.Context, int64) (bool, error)) (cursorDeviceSource, error) {
	if qk == nil {
		return "", fmt.Errorf("%w: quota key not loaded", ErrCursorDeviceIDUnavailable)
	}
	if qk.DeviceID != nil && *qk.DeviceID != "" {
		return cursorDeviceSourcePersisted, nil
	}
	if hasAccount == nil {
		return "", fmt.Errorf("%w: device resolution dependencies not configured", ErrCursorDeviceIDUnavailable)
	}
	has, err := hasAccount(ctx, qk.ID)
	if err != nil {
		return "", fmt.Errorf("shareai: resolve quota key account: %w", err)
	}
	if has {
		return cursorDeviceSourceDerived, nil
	}
	return cursorDeviceSourceFirstWrite, nil
}

// repositoryHasAccount 把"归属账号存在性"适配成分支判定的窄回调（与
// QuotaKeyService.HasAccount 同口径：一次归属账号查询，只读）。
func repositoryHasAccount(repo QuotaKeyRepository) func(context.Context, int64) (bool, error) {
	return func(ctx context.Context, quotaKeyID int64) (bool, error) {
		accs, err := repo.ListAccountsByQuotaKey(ctx, quotaKeyID)
		if err != nil {
			return false, err
		}
		return len(accs) > 0, nil
	}
}

// ensureCursorDeviceFirstWrite 是**取号链**的设备边界：与观测/出站共用同一分支判定，
// 但不需要明文（取号本就不解密），只在真新码分支执行首写。
//
// 返回命中的分支（调用方可记录），不做任何"存量改身份"动作。
func ensureCursorDeviceFirstWrite(ctx context.Context, repo QuotaKeyRepository, qk *ent.QuotaKey) (cursorDeviceSource, error) {
	if repo == nil {
		return "", fmt.Errorf("%w: quota key repository not configured", ErrCursorDeviceIDUnavailable)
	}
	branch, err := cursorDeviceBranch(ctx, qk, repositoryHasAccount(repo))
	if err != nil {
		return "", err
	}
	if branch != cursorDeviceSourceFirstWrite {
		return branch, nil
	}
	if _, err := cursorDeviceIDFirstWrite(ctx, repo, qk.ID); err != nil {
		return branch, err
	}
	return branch, nil
}
