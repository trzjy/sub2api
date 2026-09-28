// cockpit_import_parser.go —— 平台无关 Cockpit 备份导入解析引擎（方案 §1.1–1.5、§1.6）。
//
// 唯一权威来源：docs/codebuddy-cockpit-fusion-plan.md（v16）。
// v16 终审整改（D-10a）：单一 JSON 值边界与 decoder 错误统一入 *CockpitImportError（§1.4）；
// 类型契约钉死——token/uid/domain/enterprise_id 仅接受 JSON 字符串（禁数值强转），expires_at
// 单独按有限数值解析；domain 非空字符集校验（§1.3）；解析响应 context 取消（§1.5）。
// 本文件只实现"通用解析器"部分；codebuddy 系字段映射器因 §1.2 取证现状为 pending，不合入（enabled 前不实现）。
//
// 设计原则（contract §5）：生产代码零兜底、零 fallback——任何越界/非法输入一律失败关闭（fail close）返回带错误码的
// *CockpitImportError，绝不猜测或静默放行。需要但缺失的能力一律由调用方（B1 阶段）以 fixture 门控补齐。

package service

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// —— 失败关闭错误码（方案 §1.4 点名）——
const (
	// ErrCockpitUnarchiveResourceLimit ZIP 解压前资源硬上限越界（条目数/单条目/总量/放大比/路径穿越/符号链接/重名）。
	ErrCockpitUnarchiveResourceLimit = "UNARCHIVE_RESOURCE_LIMIT"
	// ErrCockpitPayloadStructureLimit JSON 结构上限越界（嵌套深度/单字段长度/账户条目数）。
	ErrCockpitPayloadStructureLimit = "PAYLOAD_STRUCTURE_LIMIT"
	// ErrCockpitSchemaVersionUnsupported schema/version 白名单未命中（0/负/缺/2+）。
	ErrCockpitSchemaVersionUnsupported = "SCHEMA_VERSION_UNSUPPORTED"
	// ErrCockpitDuplicateJSONKey 同一 JSON 对象内重复键。
	ErrCockpitDuplicateJSONKey = "DUPLICATE_JSON_KEY"
	// ErrCockpitAliasConflict 同一逻辑字段的多别名规范化后不一致。
	ErrCockpitAliasConflict = "ALIAS_CONFLICT"
	// ErrCockpitFieldInvalid 必填/字符集/数值类非法（token/uid/domain/expires_at）。
	ErrCockpitFieldInvalid = "INVALID_ENTRY"
	// ErrCockpitConflictingDuplicateUID 同文件同键凭证冲突组（组内任一字段不一致，整组失败关闭）。
	ErrCockpitConflictingDuplicateUID = "conflicting_duplicate_uid"
)

// —— 资源硬上限常量（方案 §1.4）——
const (
	maxZipEntries            = 1000
	maxZipEntryUncompressed  = 50 << 20 // 单条目未压缩 ≤ 50 MB
	maxZipTotalUncompressed  = 60 << 20 // 解压总未压缩 ≤ 60 MB
	maxZipAmplificationRatio = 100      // 放大比硬上限（小包高放大比解压炸弹防御，与字节上限并行生效）
	maxBackupJSONBytes       = 50 << 20 // backup.json 解析后 ≤ 50 MB
	maxJSONNestingDepth      = 32       // JSON 最大嵌套深度 ≤ 32
	maxJSONFieldBytes        = 64 << 10 // 单字段值长度 ≤ 64 KB
	maxAccountEntries        = 200      // 账户条目数 ≤ 200
	maxTokenBytes            = 8192     // token/refresh_token ≤ 8192，仅 ASCII 可打印 0x21–0x7E
	maxUIDBytes              = 128      // uid ≤ 128，仅 [0-9a-zA-Z_-]
	maxDomainBytes           = 253      // domain 非空时 ≤ 253，仅 [0-9a-zA-Z.-]
)

// CockpitImportError 是带错误码的失败关闭错误。
type CockpitImportError struct {
	Code string
	Err  error
}

func (e *CockpitImportError) Error() string {
	if e.Err == nil {
		return e.Code
	}
	return e.Code + ": " + e.Err.Error()
}

func (e *CockpitImportError) Unwrap() error { return e.Err }

// IsCockpitErrorCode 判断 err 是否携带指定失败关闭错误码。
func IsCockpitErrorCode(err error, code string) bool {
	var ce *CockpitImportError
	if errorsAsCockpit(err, &ce) {
		return ce.Code == code
	}
	return false
}

func errorsAsCockpit(err error, target **CockpitImportError) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(*CockpitImportError); ok {
		*target = e
		return true
	}
	if u, ok := err.(interface{ Unwrap() error }); ok {
		return errorsAsCockpit(u.Unwrap(), target)
	}
	return false
}

// CockpitNormalizedEntry 是一条经规范化后的候选导入账号（唯一索引输入 = 规范化后的值）。
type CockpitNormalizedEntry struct {
	Platform     string
	UID          string
	AccessToken  string
	RefreshToken string
	ExpiresAt    float64
	HasExpiresAt bool
	Domain       string
	EnterpriseID string
}

// CockpitImportResult 是解析结果摘要（不含任何凭证材料）。
type CockpitImportResult struct {
	RawSHA256 string // 原始上传字节流 SHA-256（覆盖含凭证的完整文件内容）

	Schema  string
	Version int64

	// PendingPlatformAccounts: pending slug → 识别到的账号数（不导入、不报错）。
	PendingPlatformAccounts map[string]int
	// PendingPlatformMessages: pending slug → 人类可读说明。
	PendingPlatformMessages map[string]string

	// UnknownPlatformAccounts: unsupported + 未知 slug 的账号总数（单独计数上报）。
	UnknownPlatformAccounts     int
	UnknownPlatformAccountSlug map[string]int // 按 slug 拆分的未知账号计数（含 unsupported 与未知）

	// EnabledPlatformAccounts: enabled slug → 识别到的账号数（当前为空）。
	EnabledPlatformAccounts map[string]int

	// DedupedSameEntries: 同文件同键组内完全一致被确定性去重的冗余条目数（= Σ(len(group)-1)）。
	DedupedSameEntries int
	// InvalidEntries: 失败关闭未写入的条目总数（含 ALIAS_CONFLICT / INVALID_ENTRY / conflicting_duplicate_uid）。
	InvalidEntries      int
	InvalidEntryReasons map[string]int
}

// ParseCockpitImport 解析 Cockpit 备份导入字节流（JSON 单文件或 ZIP 导出包）。
// 任何越界/非法输入失败关闭并返回 *CockpitImportError；result 始终带回 RawSHA256 供审计。
// 解析在 zip 条目循环与账户条目循环等天然边界响应 ctx 取消（v16 §1.5）：
// ctx.Err() 非 nil → *CockpitImportError{Code: ErrCockpitPreviewParseTimeout} 失败关闭。
func ParseCockpitImport(ctx context.Context, raw []byte) (*CockpitImportResult, error) {
	result := &CockpitImportResult{
		RawSHA256:                cockpitSHA256Hex(raw),
		PendingPlatformAccounts:  map[string]int{},
		PendingPlatformMessages:  map[string]string{},
		UnknownPlatformAccountSlug: map[string]int{},
		EnabledPlatformAccounts:  map[string]int{},
		InvalidEntryReasons:      map[string]int{},
	}

	// ② ZIP 流式解压（资源硬上限先于任何解析/内存分配检查）；否则视为 JSON 单文件。
	var jsonBytes []byte
	if isCockpitZipBytes(raw) {
		jb, err := extractCockpitBackupJSON(ctx, raw)
		if err != nil {
			return result, err
		}
		jsonBytes = jb
	} else {
		if len(raw) > maxBackupJSONBytes {
			return result, &CockpitImportError{
				Code: ErrCockpitPayloadStructureLimit,
				Err:  fmt.Errorf("backup.json %d bytes exceeds %d", len(raw), maxBackupJSONBytes),
			}
		}
		jsonBytes = raw
	}

	// ③ JSON 解析：重复键拒绝 + 嵌套深度/单字段长度上限（超限立即失败关闭）。
	v, err := strictDecodeCockpitJSON(jsonBytes)
	if err != nil {
		return result, err
	}

	// ③ 账户条目数 ≤ 200（超限立即失败关闭）。
	totalEntries, err := countCockpitAccountEntries(v)
	if err != nil {
		return result, err
	}
	if totalEntries > maxAccountEntries {
		return result, &CockpitImportError{
			Code: ErrCockpitPayloadStructureLimit,
			Err:  fmt.Errorf("account entries %d exceeds %d", totalEntries, maxAccountEntries),
		}
	}

	// ④ 信封校验：schema/version 白名单（仅 1）。
	top, ok := v.(map[string]interface{})
	if !ok {
		return result, &CockpitImportError{
			Code: ErrCockpitPayloadStructureLimit,
			Err:  fmt.Errorf("envelope is not a JSON object"),
		}
	}
	if err := validateCockpitEnvelope(top, result); err != nil {
		return result, err
	}

	// ⑤ 逐 slug 三态分类计数 + ⑦ 同文件同键分组（仅 enabled 平台进入写入分组）。
	enabledEntries, err := classifyAndCountCockpitAccounts(ctx, top, result)
	if err != nil {
		return result, err
	}
	deduped, invalid, reasons := groupCockpitEntries(enabledEntries)
	result.DedupedSameEntries += deduped
	result.InvalidEntries += invalid
	for k, n := range reasons {
		result.InvalidEntryReasons[k] += n
	}

	return result, nil
}

// —— ② ZIP 提取 ——

func isCockpitZipBytes(b []byte) bool {
	return len(b) >= 4 && b[0] == 0x50 && b[1] == 0x4B && b[2] == 0x03 && b[3] == 0x04
}

// cockpitCtxError 在天然循环边界检查 context 取消（v16 §1.5）：
// 已取消/超时 → *CockpitImportError{Code: ErrCockpitPreviewParseTimeout}；否则 nil。
func cockpitCtxError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return &CockpitImportError{Code: ErrCockpitPreviewParseTimeout, Err: err}
	}
	return nil
}

// extractCockpitBackupJSON 从 ZIP 导出包中提取首个名为 backup.json 的条目内容，
// 全程流式并在任何解析/内存分配前执行资源硬上限检查（防解压炸弹）。
// 逐条目循环边界响应 ctx 取消。
func extractCockpitBackupJSON(ctx context.Context, raw []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, &CockpitImportError{
			Code: ErrCockpitUnarchiveResourceLimit,
			Err:  fmt.Errorf("open zip: %w", err),
		}
	}
	if len(zr.File) > maxZipEntries {
		return nil, &CockpitImportError{
			Code: ErrCockpitUnarchiveResourceLimit,
			Err:  fmt.Errorf("zip entry count %d exceeds %d", len(zr.File), maxZipEntries),
		}
	}

	seenNames := map[string]bool{}
	var backup *zip.File
	var totalUncompressed int64

	for _, f := range zr.File {
		if err := cockpitCtxError(ctx); err != nil {
			return nil, err
		}
		name := f.Name
		// 拒绝路径穿越与绝对路径。
		if strings.Contains(name, "..") || filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
			return nil, &CockpitImportError{
				Code: ErrCockpitUnarchiveResourceLimit,
				Err:  fmt.Errorf("path traversal/symlink rejected in entry %q", name),
			}
		}
		// 拒绝符号链接。
		if f.FileInfo().Mode()&os.ModeSymlink != 0 {
			return nil, &CockpitImportError{
				Code: ErrCockpitUnarchiveResourceLimit,
				Err:  fmt.Errorf("symlink rejected in entry %q", name),
			}
		}
		// 拒绝多条目同名。
		if seenNames[name] {
			return nil, &CockpitImportError{
				Code: ErrCockpitUnarchiveResourceLimit,
				Err:  fmt.Errorf("duplicate entry name %q", name),
			}
		}
		seenNames[name] = true

		// 单条目未压缩字节上限。
		if f.UncompressedSize64 > maxZipEntryUncompressed {
			return nil, &CockpitImportError{
				Code: ErrCockpitUnarchiveResourceLimit,
				Err:  fmt.Errorf("entry %q uncompressed %d exceeds %d", name, f.UncompressedSize64, maxZipEntryUncompressed),
			}
		}
		// 放大比硬上限（小包高解压放大比解压炸弹）。
		if f.CompressedSize64 > 0 {
			ratio := float64(f.UncompressedSize64) / float64(f.CompressedSize64)
			if ratio > maxZipAmplificationRatio {
				return nil, &CockpitImportError{
					Code: ErrCockpitUnarchiveResourceLimit,
					Err:  fmt.Errorf("entry %q amplification ratio %.2f exceeds %d", name, ratio, maxZipAmplificationRatio),
				}
			}
		}
		// 解压总量上限。
		totalUncompressed += int64(f.UncompressedSize64)
		if totalUncompressed > maxZipTotalUncompressed {
			return nil, &CockpitImportError{
				Code: ErrCockpitUnarchiveResourceLimit,
				Err:  fmt.Errorf("total uncompressed %d exceeds %d", totalUncompressed, maxZipTotalUncompressed),
			}
		}

		if name == "backup.json" && backup == nil {
			backup = f
		}
	}

	if backup == nil {
		return nil, &CockpitImportError{
			Code: ErrCockpitUnarchiveResourceLimit,
			Err:  fmt.Errorf("zip contains no backup.json entry"),
		}
	}

	rc, err := backup.Open()
	if err != nil {
		return nil, &CockpitImportError{
			Code: ErrCockpitUnarchiveResourceLimit,
			Err:  fmt.Errorf("open backup.json: %w", err),
		}
	}
	defer rc.Close()

	// 流式读取，硬上限由 LimitReader 兜底（真实字节数而非头声称值）。
	limited := io.LimitReader(rc, maxZipEntryUncompressed+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, &CockpitImportError{
			Code: ErrCockpitUnarchiveResourceLimit,
			Err:  fmt.Errorf("read backup.json: %w", err),
		}
	}
	if int64(len(data)) > maxZipEntryUncompressed {
		return nil, &CockpitImportError{
			Code: ErrCockpitUnarchiveResourceLimit,
			Err:  fmt.Errorf("backup.json uncompressed %d exceeds %d", len(data), maxZipEntryUncompressed),
		}
	}
	return data, nil
}

// —— ③ 严格 JSON 解析 ——

// strictDecodeCockpitJSON 解析 JSON 并强制：拒绝重复键、嵌套深度 ≤ 32、单字段值 ≤ 64KB。
// 数值保留为 json.Number（expires_at 单位判定由 §1.3 规则在 B1 阶段钉死，此处不猜测）。
// 单一 JSON 值边界（v16 §1.4）：首个值解码成功后继续 Decode 并要求返回 io.EOF（允许空白）；
// 存在任何尾随内容 → ErrCockpitPayloadStructureLimit 失败关闭（拒绝尾随数据注入）。
// 所有 decoder/语法错误统一映射为 ErrCockpitPayloadStructureLimit，禁止裸返 json.Decoder 原始错误。
func strictDecodeCockpitJSON(data []byte) (interface{}, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := strictDecodeCockpitValue(dec, 0)
	if err != nil {
		return nil, cockpitJSONDecodeError(err)
	}
	// 首个值之后仅允许空白；读到任何 token 或非 EOF 错误均为尾随内容/语法错误。
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return nil, &CockpitImportError{
				Code: ErrCockpitPayloadStructureLimit,
				Err:  fmt.Errorf("unexpected trailing content after JSON value"),
			}
		}
		return nil, cockpitJSONDecodeError(err)
	}
	return v, nil
}

// cockpitJSONDecodeError 把底层 decoder/语法错误统一映射进 *CockpitImportError 契约
// （ErrCockpitPayloadStructureLimit）；已携带失败关闭错误码的错误原样透传。
func cockpitJSONDecodeError(err error) error {
	var ce *CockpitImportError
	if errorsAsCockpit(err, &ce) {
		return err
	}
	return &CockpitImportError{
		Code: ErrCockpitPayloadStructureLimit,
		Err:  fmt.Errorf("invalid JSON: %w", err),
	}
}

func strictDecodeCockpitValue(dec *json.Decoder, depth int) (interface{}, error) {
	if depth > maxJSONNestingDepth {
		return nil, &CockpitImportError{
			Code: ErrCockpitPayloadStructureLimit,
			Err:  fmt.Errorf("nesting depth exceeds %d", maxJSONNestingDepth),
		}
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		// 标量：字符串需做单字段长度上限检查。
		if s, ok := tok.(string); ok {
			if len(s) > maxJSONFieldBytes {
				return nil, &CockpitImportError{
					Code: ErrCockpitPayloadStructureLimit,
					Err:  fmt.Errorf("field value length %d exceeds %d", len(s), maxJSONFieldBytes),
				}
			}
		}
		return tok, nil
	}

	switch delim {
	case '{':
		obj := map[string]interface{}{}
		seen := map[string]bool{}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyTok.(string)
			if !ok {
				return nil, fmt.Errorf("non-string object key")
			}
			if seen[key] {
				return nil, &CockpitImportError{
					Code: ErrCockpitDuplicateJSONKey,
					Err:  fmt.Errorf("duplicate key %q", key),
				}
			}
			seen[key] = true
			val, err := strictDecodeCockpitValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			obj[key] = val
		}
		if _, err := dec.Token(); err != nil { // 消费 '}'
			return nil, err
		}
		return obj, nil
	case '[':
		arr := []interface{}{}
		for dec.More() {
			val, err := strictDecodeCockpitValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			arr = append(arr, val)
		}
		if _, err := dec.Token(); err != nil { // 消费 ']'
			return nil, err
		}
		return arr, nil
	default:
		return nil, fmt.Errorf("unexpected delimiter %q", delim)
	}
}

// countCockpitAccountEntries 统计 accounts.platforms.<slug>.exported_data 的账户条目总数，并校验 sections/accounts.platforms 结构。
func countCockpitAccountEntries(v interface{}) (int, error) {
	top, ok := v.(map[string]interface{})
	if !ok {
		return 0, &CockpitImportError{
			Code: ErrCockpitPayloadStructureLimit,
			Err:  fmt.Errorf("envelope is not a JSON object"),
		}
	}
	rawAccounts, ok := top["accounts"]
	if !ok {
		return 0, nil // 无 accounts 即 0 条
	}
	accounts, ok := rawAccounts.(map[string]interface{})
	if !ok {
		return 0, &CockpitImportError{
			Code: ErrCockpitPayloadStructureLimit,
			Err:  fmt.Errorf("accounts is not an object"),
		}
	}
	rawPlatforms, ok := accounts["platforms"]
	if !ok {
		return 0, &CockpitImportError{
			Code: ErrCockpitPayloadStructureLimit,
			Err:  fmt.Errorf("accounts.platforms missing"),
		}
	}
	platforms, ok := rawPlatforms.(map[string]interface{})
	if !ok {
		return 0, &CockpitImportError{
			Code: ErrCockpitPayloadStructureLimit,
			Err:  fmt.Errorf("accounts.platforms is not an object"),
		}
	}
	total := 0
	for slug, pv := range platforms {
		pm, ok := pv.(map[string]interface{})
		if !ok {
			return 0, &CockpitImportError{
				Code: ErrCockpitPayloadStructureLimit,
				Err:  fmt.Errorf("platform %q entry is not an object", slug),
			}
		}
		ed, ok := pm["exported_data"].([]interface{})
		if !ok {
			return 0, &CockpitImportError{
				Code: ErrCockpitPayloadStructureLimit,
				Err:  fmt.Errorf("platform %q exported_data is not an array", slug),
			}
		}
		total += len(ed)
	}
	return total, nil
}

// validateCockpitEnvelope 校验 schema/version 白名单（仅 cockpit-tools.data-transfer / 1）。
func validateCockpitEnvelope(top map[string]interface{}, result *CockpitImportResult) error {
	rawSchema, ok := top["schema"]
	if !ok {
		return &CockpitImportError{Code: ErrCockpitSchemaVersionUnsupported, Err: fmt.Errorf("missing schema")}
	}
	schema, ok := rawSchema.(string)
	if !ok || schema != "cockpit-tools.data-transfer" {
		return &CockpitImportError{Code: ErrCockpitSchemaVersionUnsupported, Err: fmt.Errorf("unsupported schema %v", rawSchema)}
	}
	rawVer, ok := top["version"]
	if !ok {
		return &CockpitImportError{Code: ErrCockpitSchemaVersionUnsupported, Err: fmt.Errorf("missing version")}
	}
	var f float64
	switch t := rawVer.(type) {
	case json.Number:
		vf, err := t.Float64()
		if err != nil {
			return &CockpitImportError{Code: ErrCockpitSchemaVersionUnsupported, Err: fmt.Errorf("invalid version %v", rawVer)}
		}
		f = vf
	case float64:
		f = t
	default:
		return &CockpitImportError{Code: ErrCockpitSchemaVersionUnsupported, Err: fmt.Errorf("version not numeric %v", rawVer)}
	}
	if f != 1 {
		return &CockpitImportError{Code: ErrCockpitSchemaVersionUnsupported, Err: fmt.Errorf("unsupported version %v", f)}
	}
	result.Schema = schema
	result.Version = 1
	return nil
}

// classifyAndCountCockpitAccounts 逐 slug 三态分类计数；对 enabled 平台的条目执行字段解析并收集归一化候选，供 §1.6 分组。
// 平台循环与账户条目循环边界响应 ctx 取消（v16 §1.5）。
func classifyAndCountCockpitAccounts(ctx context.Context, top map[string]interface{}, result *CockpitImportResult) ([]CockpitNormalizedEntry, error) {
	accounts, _ := top["accounts"].(map[string]interface{})
	platforms, _ := accounts["platforms"].(map[string]interface{})

	var enabledEntries []CockpitNormalizedEntry
	for slug, pv := range platforms {
		if err := cockpitCtxError(ctx); err != nil {
			return nil, err
		}
		pm, ok := pv.(map[string]interface{})
		if !ok {
			continue
		}
		ed, ok := pm["exported_data"].([]interface{})
		if !ok {
			continue
		}
		n := len(ed)
		normSlug := normalizeCockpitSlug(slug)
		mapping, known := ClassifyCockpitSlug(normSlug)
		if !known {
			// 未知 slug（Cockpit 未来新增）→ 按 unsupported 处理并单独计数。
			result.UnknownPlatformAccounts += n
			result.UnknownPlatformAccountSlug[normSlug] += n
			continue
		}
		switch mapping.State {
		case CockpitPlatformEnabled:
			result.EnabledPlatformAccounts[normSlug] += n
			for _, item := range ed {
				if err := cockpitCtxError(ctx); err != nil {
					return nil, err
				}
				obj, ok := item.(map[string]interface{})
				if !ok {
					result.InvalidEntries++
					result.InvalidEntryReasons[ErrCockpitFieldInvalid]++
					continue
				}
				res := parseCockpitAccountEntry(mapping.SitePlatform, obj)
				if res.Invalid {
					result.InvalidEntries++
					result.InvalidEntryReasons[res.Reason]++
					continue
				}
				enabledEntries = append(enabledEntries, res.Entry)
			}
		case CockpitPlatformPending:
			result.PendingPlatformAccounts[normSlug] += n
			result.PendingPlatformMessages[normSlug] = fmt.Sprintf("已识别 %d 个账号，待字段映射核对，未导入", n)
		case CockpitPlatformUnsupported:
			result.UnknownPlatformAccounts += n
			result.UnknownPlatformAccountSlug[normSlug] += n
		}
	}
	return enabledEntries, nil
}

// —— ⑥ 规范化与字段校验（方案 §1.3）——
//
// 规则：所有导入字符串字段先 trim（仅 ASCII 空白）；token/uid/domain 区分大小写原样保留；
// slug 按小写精确匹配映射表；空字符串规范化后按缺失处理；规范化值为唯一索引输入。

// normalizeCockpitField 去除首尾 ASCII 空白；空串按缺失处理（调用方据需判定）。
func normalizeCockpitField(s string) string {
	return strings.Trim(s, " \t\n\r\v\f")
}

// normalizeCockpitSlug slug 小写精确匹配。
func normalizeCockpitSlug(s string) string {
	return strings.ToLower(normalizeCockpitField(s))
}

// isValidCockpitToken token/refresh_token：非空、≤ 8192、仅 ASCII 可打印 0x21–0x7E（无空白/控制字符）。
func isValidCockpitToken(s string) bool {
	if s == "" || len(s) > maxTokenBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x21 || c > 0x7E {
			return false
		}
	}
	return true
}

// isValidCockpitUID uid：非空、≤ 128、仅 [0-9a-zA-Z_-]。
func isValidCockpitUID(s string) bool {
	if s == "" || len(s) > maxUIDBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c == '_' || c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return false
		}
	}
	return true
}

// isValidCockpitDomain domain（v16 §1.3）：空允许（空 domain 占位规则唯一例外）；
// 非空时（输入值已完成 trim 规范化）长度 ≤ 253、仅含 [0-9a-zA-Z.-]（禁空白/控制字符）。
func isValidCockpitDomain(s string) bool {
	if s == "" {
		return true
	}
	if len(s) > maxDomainBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c == '.' || c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return false
		}
	}
	return true
}

// cockpitJSONFiniteNumber 仅接受 JSON 数值类型（json.Number / float64）且必须为有限值；
// 字符串/布尔/其它类型与 NaN/Inf/不可解析数值 → false（失败关闭，不经字符串路径）。
func cockpitJSONFiniteNumber(v interface{}) (float64, bool) {
	var f float64
	switch t := v.(type) {
	case json.Number:
		parsed, err := t.Float64()
		if err != nil {
			return 0, false
		}
		f = parsed
	case float64:
		f = t
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// resolveCockpitAliasedField 解析同一逻辑字段的多别名（§1.4.4），仅接受 JSON 字符串
// （v16 §1.3 类型契约：token/uid/domain/enterprise_id 禁止数值→字符串强转）。
// 多个别名同时出现时规范化后完全一致才接受；不一致 → conflict=true（ALIAS_CONFLICT）；
// 值存在但非 JSON 字符串类型 → invalid=true（INVALID_ENTRY，失败关闭，不猜测）。
func resolveCockpitAliasedField(raw map[string]interface{}, aliases []string) (value string, conflict bool, invalid bool, present bool) {
	var found string
	var has bool
	for _, a := range aliases {
		v, ok := raw[a]
		if !ok {
			continue
		}
		s, ok := v.(string)
		if !ok {
			return "", false, true, true
		}
		ns := normalizeCockpitField(s)
		if !has {
			found = ns
			has = true
		} else if ns != found {
			return "", true, false, true
		}
	}
	return found, false, false, has
}

// resolveCockpitAliasedNumericField 解析 expires_at/expiresAt 别名（§1.4.4），按有限数值解析
// （v16 §1.3：expires_at 单独按有限数值解析，不经字符串路径）。
// 多个别名同时出现时数值完全一致才接受，不一致 → conflict=true（ALIAS_CONFLICT）；
// 值存在但非 JSON 数值或非有限值 → invalid=true（INVALID_ENTRY）。
func resolveCockpitAliasedNumericField(raw map[string]interface{}, aliases []string) (value float64, conflict bool, invalid bool, present bool) {
	var found float64
	var has bool
	for _, a := range aliases {
		v, ok := raw[a]
		if !ok {
			continue
		}
		f, ok := cockpitJSONFiniteNumber(v)
		if !ok {
			return 0, false, true, true
		}
		if !has {
			found = f
			has = true
		} else if f != found {
			return 0, true, false, true
		}
	}
	return found, false, false, has
}

// cockpitEntryResult 是单条账号条目解析结果。
type cockpitEntryResult struct {
	Entry   CockpitNormalizedEntry
	Invalid bool
	Reason  string
}

// parseCockpitAccountEntry 解析单条账号条目（§1.3 必填/字符集/类型契约校验 + §1.4.4 别名冲突）。
// platform 为调用方已查表得到的站点平台名。
func parseCockpitAccountEntry(platform string, raw map[string]interface{}) cockpitEntryResult {
	accessToken, atConflict, atInvalid, _ := resolveCockpitAliasedField(raw, []string{"access_token", "accessToken"})
	refreshToken, rtConflict, rtInvalid, _ := resolveCockpitAliasedField(raw, []string{"refresh_token", "refreshToken"})
	uid, uidConflict, uidInvalid, uidPresent := resolveCockpitAliasedField(raw, []string{"uid", "account.id"})
	enterpriseID, entConflict, entInvalid, _ := resolveCockpitAliasedField(raw, []string{"enterprise_id", "enterpriseId"})
	domain, domConflict, domInvalid, _ := resolveCockpitAliasedField(raw, []string{"domain"})
	expiresAt, expConflict, expInvalid, expPresent := resolveCockpitAliasedNumericField(raw, []string{"expires_at", "expiresAt"})

	if atConflict || rtConflict || uidConflict || entConflict || domConflict || expConflict {
		return cockpitEntryResult{Invalid: true, Reason: ErrCockpitAliasConflict}
	}
	if atInvalid || rtInvalid || uidInvalid || entInvalid || domInvalid || expInvalid {
		return cockpitEntryResult{Invalid: true, Reason: ErrCockpitFieldInvalid}
	}
	if !uidPresent || !isValidCockpitUID(uid) {
		return cockpitEntryResult{Invalid: true, Reason: ErrCockpitFieldInvalid}
	}
	if !isValidCockpitToken(accessToken) {
		return cockpitEntryResult{Invalid: true, Reason: ErrCockpitFieldInvalid}
	}
	if !isValidCockpitToken(refreshToken) {
		return cockpitEntryResult{Invalid: true, Reason: ErrCockpitFieldInvalid}
	}
	if !isValidCockpitDomain(domain) {
		return cockpitEntryResult{Invalid: true, Reason: ErrCockpitFieldInvalid}
	}
	return cockpitEntryResult{
		Entry: CockpitNormalizedEntry{
			Platform:     platform,
			UID:          uid,
			AccessToken:  accessToken,
			RefreshToken: refreshToken,
			ExpiresAt:    expiresAt,
			HasExpiresAt: expPresent,
			Domain:       domain,
			EnterpriseID: enterpriseID,
		},
	}
}

// —— ⑦ 同文件同键分组校验（方案 §1.6）——
//
// 按规范化 (platform, uid) 分组：组内各条目完全一致 → 确定性去重（冗余条目数计入 deduped）；
// 组内任一字段（token/refresh_token/expires_at/domain/enterprise_id）不一致 → 整组失败关闭
// （全部条目计 invalid_entries，原因 conflicting_duplicate_uid），一个不写。

func cockpitEntryEqual(a, b CockpitNormalizedEntry) bool {
	return a.Platform == b.Platform &&
		a.UID == b.UID &&
		a.AccessToken == b.AccessToken &&
		a.RefreshToken == b.RefreshToken &&
		a.ExpiresAt == b.ExpiresAt &&
		a.HasExpiresAt == b.HasExpiresAt &&
		a.Domain == b.Domain &&
		a.EnterpriseID == b.EnterpriseID
}

// groupCockpitEntries 对候选条目执行同键分组，返回确定性去重数与冲突失败数。
func groupCockpitEntries(entries []CockpitNormalizedEntry) (deduped int, invalid int, reasons map[string]int) {
	reasons = map[string]int{}
	groups := map[string][]CockpitNormalizedEntry{}
	var order []string
	for _, e := range entries {
		k := e.Platform + "\x00" + e.UID
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], e)
	}
	for _, k := range order {
		g := groups[k]
		ref := g[0]
		allSame := true
		for i := 1; i < len(g); i++ {
			if !cockpitEntryEqual(ref, g[i]) {
				allSame = false
				break
			}
		}
		if allSame {
			// 组内完全一致：确定性去重，冗余条目数 = len(group)-1。
			deduped += len(g) - 1
		} else {
			// 组内不一致：整组失败关闭，一个不写。
			invalid += len(g)
			reasons[ErrCockpitConflictingDuplicateUID] += len(g)
		}
	}
	return deduped, invalid, reasons
}

// cockpitSHA256Hex 计算原始字节流 SHA-256 十六进制串（raw_sha256）。
func cockpitSHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
