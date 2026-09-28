// cockpit_import_commit.go —— cockpit 导入 service 层提交入口（方案 v15 §1.6，B1b）。
//
// 本文件只定义提交编排契约与薄入口：构造载荷 → 仓储原子写入 → 计数汇总。
// 平台映射器（entry→payload 转换）因 §1.2 门控为 pending，不在本单（B1c 之外、enabled 为空）。
// 当前 enabled 平台为空，无 entry 能转成载荷，天然不写入；映射器上线后在本服务内
// 做 entry→payload 转换再调用仓储，接口边界保持不变。

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

// CockpitImportCommitService 封装 cockpit 导入提交编排（§1.6）与 preview/commit 凭证校验（§1.4-1.8，B1c）。
type CockpitImportCommitService struct {
	repo          CockpitImportCommitRepository
	receiptSigner *CockpitPreviewReceiptSigner
}

// NewCockpitImportCommitService 构造提交服务（仅 B1b 写入编排；不含 preview 凭证，B1c 之前既有入口）。
func NewCockpitImportCommitService(repo CockpitImportCommitRepository) *CockpitImportCommitService {
	return &CockpitImportCommitService{repo: repo}
}

// NewCockpitImportCommitServiceWithReceipt 构造带 HMAC 凭证签名的提交服务（B1c）。
func NewCockpitImportCommitServiceWithReceipt(repo CockpitImportCommitRepository, signer *CockpitPreviewReceiptSigner) *CockpitImportCommitService {
	return &CockpitImportCommitService{repo: repo, receiptSigner: signer}
}

// Commit 将已构造的账号载荷整批原子提交到仓储。
// 映射器上线前由调用方（测试或 B1c 端点）直接注入载荷；
// 上线后此处会在调用 repo 前完成 entry→payload 转换，接口边界不变。
// 空载荷直接返回零计数，不触发仓储调用（与 repo 层空值守门一致）。
func (s *CockpitImportCommitService) Commit(ctx context.Context, payloads []CockpitImportAccountPayload) (CockpitImportCommitResult, error) {
	if len(payloads) == 0 {
		return CockpitImportCommitResult{}, nil
	}
	return s.repo.CommitCockpitImport(ctx, payloads)
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
	if _, err := runCockpitParseWithTimeout(ctx, raw); err != nil {
		return nil, err
	}
	// enabled 为空，当前无 entry 可转载荷；成功路径 created=0，调既有 B1b 写入器。
	res, err := s.Commit(ctx, []CockpitImportAccountPayload{})
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// runCockpitParseWithTimeout 在 cockpitImportParseTimeout 派生子 context（父 deadline 更早则父胜出）下
// 运行解析；超时 → *CockpitImportError{Code: ErrCockpitPreviewParseTimeout} 失败关闭。
// 解析为 CPU 密集型、不响应取消，故以 goroutine + select 实现时限失败关闭：超时时立即返回，
// 解析协程在完成后自然退出（结果被丢弃）。
func runCockpitParseWithTimeout(ctx context.Context, raw []byte) (*CockpitImportResult, error) {
	parseCtx, cancel := context.WithTimeout(ctx, cockpitImportParseTimeout)
	defer cancel()
	type out struct {
		res *CockpitImportResult
		err error
	}
	ch := make(chan out, 1)
	go func() {
		res, perr := ParseCockpitImport(raw)
		ch <- out{res, perr}
	}()
	select {
	case <-parseCtx.Done():
		return nil, &CockpitImportError{Code: ErrCockpitPreviewParseTimeout, Err: parseCtx.Err()}
	case o := <-ch:
		return o.res, o.err
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
