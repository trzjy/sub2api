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
)

// ErrCursorDeviceIDUnavailable 表示无法为激活码解析出可用的 deviceId（失败关闭）。
//
// 出站鉴权材料不可得必须失败关闭：带着空 deviceId 发出的请求必然被上游拒绝，
// 且会白耗 shareai 额度。故本卡不提供任何兜底默认值。
var ErrCursorDeviceIDUnavailable = errors.New("cursor: device id unavailable (fail closed)")

// hashDeviceIDFromUUID 逐字复刻插件 hashDeviceId（deobfuscated.js:8070-8077）。
//
// 与仓库既有 hashDeviceID（cursor_auto_entry_service.go:452）的差异**不止一处**，
// 既有实现对非 ASCII 输入与插件不等价，故本卡独立实现并单测锁定：
//
//  1. 迭代单位：插件 s.charCodeAt(i) 遍历 **UTF-16 码元**；既有实现 `for _, c := range`
//     遍历 **Unicode 码点**。ASCII 下两者一致，但 BMP 外字符（如 emoji，UTF-16 为代理对
//     2 个码元）会分叉：😀 插件得 001B0D63，既有实现得 0001F600。
//  2. 乘法形式：插件 `(a<<5) - a + c`；既有实现 `h*31 + c`。二者对 int32 环绕数学等价
//     （(x<<5)-x == 31x，且两侧都在 int32 内环绕），非差异来源，但按插件字面照抄以免歧义。
//  3. 收尾取绝对值：插件 Math.abs 作用在**双精度**上，int32 最小值 -2147483648 取绝对值
//     得 2147483648（不溢出）；Go 若用 int32 取绝对值会溢出为自身，故此处显式转 int64。
//     这是唯一的语义性溢出陷阱。
//
// 4. 格式化：插件 toString(16).padStart(8,"0").substring(0,8).toUpperCase()。对 int32
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
// 首次并发防重靠 **DB 层条件更新**（SetDeviceIDIfAbsent：WHERE device_id IS NULL OR =''）：
// 并发首次请求中只有把"未生成"改成非空的那一个事务会命中（受影响行数=1），其余
// 拿到 0 行后重读，取到赢家写入的值。因此同一激活码恒定只有一份 deviceId，
// 不会出现"两个请求各生成一个不同值、后写覆盖先写"的分裂。
//
// deviceId 落库后**永久复用**：这是插件语义（固化到 ~/.cursor_free_data），
// 也是上游能把同一份设备标识视为同一客户端的前提，故本方法在已有值时绝不重算。
func (s *QuotaKeyService) DeviceIDFor(ctx context.Context, quotaKeyID int64) (string, error) {
	if s == nil || s.repo == nil {
		return "", fmt.Errorf("%w: quota key service not configured", ErrCursorDeviceIDUnavailable)
	}
	qk, err := s.repo.GetByID(ctx, quotaKeyID)
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
	written, err := s.repo.SetDeviceIDIfAbsent(ctx, quotaKeyID, candidate)
	if err != nil {
		return "", fmt.Errorf("cursor: persist device id for quota key %d: %w", quotaKeyID, err)
	}
	if written == 1 {
		return candidate, nil
	}

	// 0 行 = 已有并发请求抢先写入（条件不满足）。重读取赢家的值，
	// 绝不返回本请求的候选值（那会造成同一激活码出现两份 deviceId）。
	settled, err := s.repo.GetByID(ctx, quotaKeyID)
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