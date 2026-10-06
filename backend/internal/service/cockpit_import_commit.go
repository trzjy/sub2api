// cockpit_import_commit.go —— cockpit 导入 service 层提交入口（方案 v16 §1.5/§1.2）。
//
// 本文件只定义提交编排契约与薄入口：凭证校验 → 同版本解析 → enabled-零载荷闸 →
// 构造载荷 → 仓储原子写入 → 计数汇总。平台映射器（entry→payload 转换）因 §1.2 门控
// 为 pending，不在本单（enabled 为空）。当前 enabled 平台为空，无 entry 能转成载荷，
// 天然不写入；映射器上线后在本服务内做 entry→payload 转换再调用仓储，接口边界保持不变。
// 提交边界结构强制（v16 §1.5）：载荷写入仅收敛为私有 commitPayloads，
// 仅凭证校验通过后的 CommitCockpitImport 可达，无导出旁路。

package service

import (
	"context"
	"crypto/hmac"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// cockpitImportParseTimeout 解析时限（10 秒）。
// 取值依据：backup.json ≤ 50MB、ZIP 解压总量 ≤ 60MB，且账户条目 ≤ 200；在单实例 CPU 预算内，
// 10 秒足以覆盖最坏情况的严格 JSON 解析 + 同键分组。超出即视为异常滥用/卡死，失败关闭返回
// PREVIEW_PARSE_TIMEOUT，避免长耗时请求占用管理面 worker。父 context 若更早 deadline 则父 deadline 胜出。
const cockpitImportParseTimeout = 10 * time.Second

// CockpitPreviewResult 是 preview 的响应摘要（零凭证材料：仅计数、slug 与 raw_sha256 摘要）。
type CockpitPreviewResult struct {
	RawSHA256      string         `json:"raw_sha256"`
	ParserVersion  int            `json:"parser_version"`
	TotalEntries   int            `json:"total_entries"`
	PendingAccounts map[string]int `json:"pending_accounts"`
	UnknownAccounts int           `json:"unknown_accounts"`
	EnabledAccounts map[string]int `json:"enabled_accounts"`
	DedupedEntries int            `json:"deduped_entries"`
	InvalidEntries int            `json:"invalid_entries"`
	InvalidEntryReasons map[string]int `json:"invalid_entry_reasons"`
	Receipt        string         `json:"receipt"`
}

// CockpitImportAccountPayload 是一条已构造好的账号终态载荷。
// 由未来平台映射器产出（B1b 不做 entry→payload 转换，§1.3 门控）。
type CockpitImportAccountPayload struct {
	Platform    string         // 站点平台名，如 "claude"、"codebuddy"（与 accounts.platform 对齐）
	UID         string         // 平台侧唯一标识，与 platform 共同构成部分唯一索引冲突键（非空）
	Name        string         // 账户显示名称
	AccountType string         // 认证类型，如 "oauth"、"api_key"（非空，对应 accounts.type）
	Credentials map[string]any // 凭证材料（终态字段；明文由仓储写路径加密 + MAC）
	Extra       map[string]any // 平台特定扩展数据（终态字段）
}

// CockpitImportCommitResult 是提交结果摘要。
type CockpitImportCommitResult struct {
	Created         int // 实际新增账号数
	SkippedExisting int // 存量已存在被 DO NOTHING 跳过的键数
}

// CockpitImportCommitRepository 是 cockpit 导入整文件原子提交的仓储契约。
// 仅由真实账号仓储实现；service 层通过接口依赖，不反向 import repository。
type CockpitImportCommitRepository interface {
	CommitCockpitImport(ctx context.Context, payloads []CockpitImportAccountPayload) (CockpitImportCommitResult, error)
}

// ErrCockpitCommitPayloadMissing 提交链装载失败（v16 §1.2）：解析结果存在 enabled 平台
// 条目而提交链无可用载荷 → 显式失败关闭，禁止静默 created=0（假完成面）。
const ErrCockpitCommitPayloadMissing = "COMMIT_PAYLOAD_MISSING"

// CockpitImportCommitService 封装 cockpit 导入提交编排（§1.5/§1.6）与 preview/commit
// 凭证校验（§1.4-1.8）。唯一构造形态带凭证签名器——不存在无凭证旁路形态（v16 §1.5）。
type CockpitImportCommitService struct {
	repo          CockpitImportCommitRepository
	receiptSigner *CockpitPreviewReceiptSigner
}

// NewCockpitImportCommitServiceWithReceipt 构造带 HMAC 凭证签名的提交服务（唯一构造入口）。
func NewCockpitImportCommitServiceWithReceipt(repo CockpitImportCommitRepository, signer *CockpitPreviewReceiptSigner) *CockpitImportCommitService {
	return &CockpitImportCommitService{repo: repo, receiptSigner: signer}
}

// commitPayloads 将已构造的账号载荷整批原子提交到仓储（私有——v16 §1.5 提交边界结构强制：
// 载荷写入路径不暴露为可绕过凭证校验的导出方法，仅凭证校验通过后的 CommitCockpitImport 可达）。
// 映射器上线后由 CommitCockpitImport 在调用本方法前完成 entry→payload 转换，接口边界不变。
// 空载荷直接返回零计数，不触发仓储调用（与 repo 层空值守门一致）。
func (s *CockpitImportCommitService) commitPayloads(ctx context.Context, payloads []CockpitImportAccountPayload) (CockpitImportCommitResult, error) {
	if len(payloads) == 0 {
		return CockpitImportCommitResult{}, nil
	}
	return s.repo.CommitCockpitImport(ctx, payloads)
}

// cockpitEnabledAccountsTotal 汇总解析结果中 enabled 平台条目总数。
func cockpitEnabledAccountsTotal(r *CockpitImportResult) int {
	if r == nil {
		return 0
	}
	total := 0
	for _, n := range r.EnabledPlatformAccounts {
		total += n
	}
	return total
}

// checkCockpitCommitPayloadGate enabled-零载荷失败关闭闸（v16 §1.2，纯函数）：
// 解析结果存在 enabled 平台条目而提交链无可用载荷 → *CockpitImportError
// {Code: ErrCockpitCommitPayloadMissing} 失败关闭，禁止静默 created=0。
func checkCockpitCommitPayloadGate(r *CockpitImportResult, payloads []CockpitImportAccountPayload) error {
	if cockpitEnabledAccountsTotal(r) > 0 && len(payloads) == 0 {
		return &CockpitImportError{
			Code: ErrCockpitCommitPayloadMissing,
			Err:  fmt.Errorf("enabled platform entries parsed but no payloads available"),
		}
	}
	return nil
}

// PreviewCockpitImport 解析上传字节流并返回计数摘要 + 签名凭证；零副作用（不写库不落盘）。
// 解析在 cockpitImportParseTimeout 内未完成 → *CockpitImportError{Code: ErrCockpitPreviewParseTimeout} 失败关闭。
func (s *CockpitImportCommitService) PreviewCockpitImport(ctx context.Context, operator string, raw []byte) (*CockpitPreviewResult, error) {
	if !s.receiptSigner.Configured() {
		return nil, &CockpitImportError{Code: ErrCockpitPreviewReceiptInvalid, Err: errors.New("preview receipt signer not configured")}
	}
	result, err := runCockpitParseWithTimeout(ctx, raw)
	if err != nil {
		return nil, err
	}
	rawSha := cockpitSHA256Hex(raw)
	token, err := s.receiptSigner.Issue(operator, rawSha)
	if err != nil {
		return nil, &CockpitImportError{Code: ErrCockpitPreviewReceiptInvalid, Err: err}
	}
	total := sumCockpitEntries(result)
	return &CockpitPreviewResult{
		RawSHA256:           rawSha,
		ParserVersion:       cockpitImportParserVersion,
		TotalEntries:        total,
		PendingAccounts:     result.PendingPlatformAccounts,
		UnknownAccounts:     result.UnknownPlatformAccounts,
		EnabledAccounts:     result.EnabledPlatformAccounts,
		DedupedEntries:      result.DedupedSameEntries,
		InvalidEntries:      result.InvalidEntries,
		InvalidEntryReasons: result.InvalidEntryReasons,
		Receipt:             token,
	}, nil
}

// CommitCockpitImport 校验凭证 → 重算 SHA 比对 → 同版本解析 → 调既有 B1b 写入器。
// 无凭证/无效凭证 → *CockpitImportError{Code: ErrCockpitPreviewReceiptInvalid} 失败关闭；
// 字节流与凭证内 raw_sha256 不一致 → ErrCockpitPreviewManifestMismatch 失败关闭。
func (s *CockpitImportCommitService) CommitCockpitImport(ctx context.Context, operator, receipt string, raw []byte) (*CockpitImportCommitResult, error) {
	if !s.receiptSigner.Configured() {
		return nil, &CockpitImportError{Code: ErrCockpitPreviewReceiptInvalid, Err: errors.New("preview receipt signer not configured")}
	}
	if receipt == "" {
		return nil, &CockpitImportError{Code: ErrCockpitPreviewReceiptInvalid, Err: errors.New("missing preview receipt")}
	}
	verified, err := s.receiptSigner.Verify(receipt, operator)
	if err != nil {
		return nil, &CockpitImportError{Code: ErrCockpitPreviewReceiptInvalid, Err: err}
	}
	// 重算原始字节流 SHA-256 与凭证内比对（不一致视为清单被篡改，失败关闭）。
	rawSha := cockpitSHA256Hex(raw)
	if !subtleEqualHex(rawSha, verified.RawSHA256) {
		return nil, &CockpitImportError{Code: ErrCockpitPreviewManifestMismatch, Err: fmt.Errorf("raw sha256 mismatch")}
	}
	// 同版本解析（与 preview 一致；版本变更后凭证已因 Verify 版本校验失败）。
	result, err := runCockpitParseWithTimeout(ctx, raw)
	if err != nil {
		return nil, err
	}
	// 平台映射器 pending（§1.2 门控）：当前无 entry→payload 转换，可用载荷恒为空；
	// enabled 表亦为空，闸门天然通过。映射器上线后在此装载载荷，闸门语义不变。
	var payloads []CockpitImportAccountPayload
	// enabled-零载荷失败关闭闸（v16 §1.2）：enabled 条目存在而载荷为空 → 显式失败关闭。
	if err := checkCockpitCommitPayloadGate(result, payloads); err != nil {
		return nil, err
	}
	res, err := s.commitPayloads(ctx, payloads)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// cockpitParseOutcome 是解析协程的产出；done 通道用于佐证超时返回后解析协程
// 在检查点退出（测试观测用，生产路径不消费）。
type cockpitParseOutcome struct {
	res *CockpitImportResult
	err error
}

// runCockpitParseWithTimeout 在 cockpitImportParseTimeout 时限下解析（父 deadline 更早则父胜出）；
// 超时 → *CockpitImportError{Code: ErrCockpitPreviewParseTimeout} 失败关闭。
func runCockpitParseWithTimeout(ctx context.Context, raw []byte) (*CockpitImportResult, error) {
	res, err, _ := runCockpitParseWithLimit(ctx, raw, cockpitImportParseTimeout)
	return res, err
}

// runCockpitParseWithLimit 与 runCockpitParseWithTimeout 同语义，时限可注入；
// 返回的 done 在解析协程退出后必有产出可读（缓冲 1，不阻塞协程）。
// 超时即终止（v16 §1.5）：select 超时分支返回即触发 cancel，解析器在 zip 条目/账户条目
// 循环等天然检查点响应 parseCtx 取消而退出并释放输入字节，不留下持有完整上传的失控解析协程；
// goroutine + select 是时限失败关闭的外层保证（超时不等待协程即返回）。
func runCockpitParseWithLimit(ctx context.Context, raw []byte, timeout time.Duration) (*CockpitImportResult, error, <-chan cockpitParseOutcome) {
	parseCtx, cancel := context.WithTimeout(ctx, timeout)
	ch := make(chan cockpitParseOutcome, 1)
	go func() {
		res, perr := ParseCockpitImport(parseCtx, raw)
		ch <- cockpitParseOutcome{res, perr}
		cancel()
	}()
	select {
	case <-parseCtx.Done():
		cancel()
		return nil, &CockpitImportError{Code: ErrCockpitPreviewParseTimeout, Err: parseCtx.Err()}, ch
	case o := <-ch:
		cancel()
		return o.res, o.err, ch
	}
}

// sumCockpitEntries 汇总解析结果的原始条目总数（去重冗余 + 各类计数）。
func sumCockpitEntries(r *CockpitImportResult) int {
	if r == nil {
		return 0
	}
	total := r.DedupedSameEntries + r.InvalidEntries + r.UnknownPlatformAccounts
	for _, n := range r.PendingPlatformAccounts {
		total += n
	}
	for _, n := range r.EnabledPlatformAccounts {
		total += n
	}
	return total
}

// subtleEqualHex 对两个十六进制摘要做恒定时间比较，避免计时侧信道。
func subtleEqualHex(a, b string) bool {
	ab, errA := hex.DecodeString(a)
	bb, errB := hex.DecodeString(b)
	if errA != nil || errB != nil {
		return false
	}
	return hmac.Equal(ab, bb)
}
