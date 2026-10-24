package service

// 本文件实现 Cursor 跨 HTTP 会话的上下文摘要引擎（方案 §25.14-D「摘要算法」）。
//
// 设计约束（硬性，逐条对应方案）：
//   - 链式摘要：固定起点 H0，每项 Hn = SHA256(H(n-1) || type || length || canonical_item)；
//     length 使用固定宽度编码（8 字节 big-endian）。
//   - type 由调用方传入（如 "chat.message"、"responses.item.function_call"），引擎不硬编码协议清单。
//   - 文本逐字节保留：不 trim、不改换行、不做任何 Unicode 归一。
//   - 流式分片：增量累加器先把同一逻辑文本项的增量累计为 hash/长度，项结束（Finish）
//     才产出该项的 canonical 字节并加入历史链；同一逻辑内容不同分帧方式必须得到相同历史摘要。
//   - JSON 规范化：对象按键排序、数组保序、数值保精度（json.Decoder.UseNumber + 精确十进制
//     符号/系数/指数表示，绝不经 float64）；重复键与无效 JSON 明确拒绝，不静默取首值。
//
// 不新增通用规范化框架、不引入第三方依赖（只用标准库 crypto/sha256、encoding/json、
// math/big、sort、bytes、strings、io）。

import (
	"os"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/big"
	"sort"
	"strings"
)

// cursorDigestLengthWidth 是链式摘要中 length 字段的固定宽度（字节，big-endian）。
const cursorDigestLengthWidth = 8

// cursorDigestMaxJSONDepth 限制 JSON 规范化递归深度，避免恶意深嵌套输入耗尽栈。
const cursorDigestMaxJSONDepth = 256

// cursorDigestH0 是固定起点。用域分隔常量派生，保证跨进程/跨版本稳定且不与其它哈希用途混淆。
var cursorDigestH0 = sha256.Sum256([]byte("sub2api/cursor/session-history-digest/v1"))

// 摘要层错误。
var (
	// ErrCursorDigestItemType 表示传入的项类型不合法（空或含非命名空间字符）。
	ErrCursorDigestItemType = errors.New("cursor session digest: invalid item type")
	// ErrCursorDigestInvalidJSON 表示输入不是合法 JSON（含重复键、尾随垃圾、空输入）。
	ErrCursorDigestInvalidJSON = errors.New("cursor session digest: invalid json")
	// ErrCursorDigestStreamClosed 表示已在 Finish 之后继续写入流式累加器。
	ErrCursorDigestStreamClosed = errors.New("cursor session digest: stream already finished")
)

// CursorHistoryDigest 是链式历史摘要状态。
//
// 零值不可直接用猜的语义：请使用 NewCursorHistoryDigest；零值在首次 AppendItem 时
// 会自动初始化为 H0（这样 &CursorHistoryDigest{} 的行为与构造器一致）。
type CursorHistoryDigest struct {
	state       [sha256.Size]byte
	initialized bool
	items       int
}

// NewCursorHistoryDigest 返回位于固定起点 H0 的空历史摘要。
func NewCursorHistoryDigest() *CursorHistoryDigest {
	d := &CursorHistoryDigest{}
	d.reset()
	return d
}

// reset 把状态恢复到 H0 并清空项数。
func (d *CursorHistoryDigest) reset() {
	d.state = cursorDigestH0
	d.initialized = true
	d.items = 0
}

// ItemCount 返回已进入历史链的逻辑项数。
func (d *CursorHistoryDigest) ItemCount() int {
	if d == nil {
		return 0
	}
	return d.items
}

// Sum 返回当前链摘要（Hn）。
func (d *CursorHistoryDigest) Sum() [sha256.Size]byte {
	if d == nil {
		return cursorDigestH0
	}
	if !d.initialized {
		return cursorDigestH0
	}
	return d.state
}

// HexString 返回当前链摘要的小写十六进制表示（仅用于比较/日志，不进入上下文语义）。
func (d *CursorHistoryDigest) HexString() string {
	sum := d.Sum()
	return hex.EncodeToString(sum[:])
}

// Equal 比较两个历史摘要的链值与项数。
func (d *CursorHistoryDigest) Equal(other *CursorHistoryDigest) bool {
	if d == nil || other == nil {
		return d == other
	}
	return d.ItemCount() == other.ItemCount() && d.Sum() == other.Sum()
}

// Clone 返回当前状态的深拷贝，便于在不推进原链的前提下试算前缀摘要。
func (d *CursorHistoryDigest) Clone() *CursorHistoryDigest {
	if d == nil {
		return nil
	}
	c := &CursorHistoryDigest{state: d.Sum(), initialized: true, items: d.ItemCount()}
	return c
}

// ValidateCursorDigestItemType 校验项类型是否是可安全拼入链的类型命名空间字节串。
//
// 允许 ASCII 字母、数字、'.'、'_'、'-'（如 "chat.message"、"responses.item.function_call"）。
// 之所以限制字符集：Hn = SHA256(H(n-1) || type || length || item) 中 type 未做长度前缀，
// 只允许定长字母表可消除 type 与 length 边界上的歧义。
func ValidateCursorDigestItemType(itemType string) error {
	if itemType == "" {
		return fmt.Errorf("%w: empty", ErrCursorDigestItemType)
	}
	if len(itemType) > 128 {
		return fmt.Errorf("%w: too long (%d bytes)", ErrCursorDigestItemType, len(itemType))
	}
	for i := 0; i < len(itemType); i++ {
		c := itemType[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '.' || c == '_' || c == '-'
		if !ok {
			return fmt.Errorf("%w: illegal byte 0x%02x at offset %d", ErrCursorDigestItemType, c, i)
		}
	}
	return nil
}

// AppendItem 把一项的 canonical 字节按链式公式并入历史摘要。
//
// canonical 必须是调用方按 §25.14-D 生成的确定表示（文本原样字节、规范化 JSON 等），
// 本方法不做任何再加工。
func (d *CursorHistoryDigest) AppendItem(itemType string, canonical []byte) error {
	if d == nil {
		return errors.New("cursor session digest: nil digest")
	}
	if err := ValidateCursorDigestItemType(itemType); err != nil {
		return err
	}
	// TEMP-DIAG：逐项打印类型/长度/载荷指纹前 4 字节（无正文），定位后移除。
	{
		sum := sha256.Sum256(canonical)
		fmt.Fprintf(os.Stderr, "DIGESTDIAG type=%s len=%d sha=%x\n", itemType, len(canonical), sum[:4])
	}
	if !d.initialized {
		d.state = cursorDigestH0
		d.initialized = true
		d.items = 0
	}
	var length [cursorDigestLengthWidth]byte
	n := uint64(len(canonical))
	for i := cursorDigestLengthWidth - 1; i >= 0; i-- {
		length[i] = byte(n)
		n >>= 8
	}
	h := sha256.New()
	h.Write(d.state[:])
	h.Write([]byte(itemType))
	h.Write(length[:])
	h.Write(canonical)
	var next [sha256.Size]byte
	copy(next[:], h.Sum(nil))
	d.state = next
	d.items++
	return nil
}

// AppendText 以逐字节文本项并入历史链（不 trim、不改换行）。
func (d *CursorHistoryDigest) AppendText(itemType, text string) error {
	return d.AppendItem(itemType, CursorCanonicalText(text))
}

// AppendJSON 以规范化 JSON 项并入历史链；重复键/无效 JSON 返回错误且不改变链状态。
func (d *CursorHistoryDigest) AppendJSON(itemType string, raw []byte) error {
	canonical, err := CursorCanonicalJSON(raw)
	if err != nil {
		return err
	}
	return d.AppendItem(itemType, canonical)
}

// AppendToolArgsJSON 以规范化工具参数 JSON 并入历史链。
//
// 附加规则（§25.14-D「工具参数 JSON 仅空白或对象键序不同视为同值」）：
// 仅含空白或空的工具参数视为空对象；其余按 CursorCanonicalJSON 严格规范化。
func (d *CursorHistoryDigest) AppendToolArgsJSON(itemType string, raw []byte) error {
	canonical, err := CursorCanonicalToolArgsJSON(raw)
	if err != nil {
		return err
	}
	return d.AppendItem(itemType, canonical)
}

// CursorCanonicalText 返回文本项的 canonical 字节：逐字节原样返回。
//
// 明确不做：trim、换行统一（\r\n→\n）、Unicode 归一、大小写折叠。
// 因此仅尾部空白/换行差异的文本必须是不同摘要。
func CursorCanonicalText(text string) []byte {
	return []byte(text)
}

// CursorCanonicalJSON 返回 JSON 的规范化 canonical 字节（§25.14-D）。
//
// 规范化规则：
//   - 对象：键按 UTF-8 字节序排序；重复键一律报错（不静默取首值）。
//   - 数组：保持顺序（顺序变化即不同值）。
//   - 字符串：用 encoding/json 的标准转义重新编码（内容不变）。
//   - 数值：解析为精确十进制的 (符号, 系数, 指数) 三元组并输出为
//     "<-?><coefficient>e<exponent>"。系数按字面数字串保留（含尾随零、仅去掉前导零，
//     因 JSON 数字字面量不允许前导零），指数为
//     小数点位移后的整数。绝不经过 float64，因此 9007199254740993 与 9007199254740992
//     保持不同；同时 1.1 与 1.10 也保持不同（不把精度变化当同值）。
//   - 输出无任何多余空白；非法 JSON、尾随垃圾、空输入均返回错误。
func CursorCanonicalJSON(raw []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("%w: empty input", ErrCursorDigestInvalidJSON)
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()

	var sb strings.Builder
	sb.Grow(len(trimmed))
	if err := cursorNormalizeJSONValue(dec, &sb, 0); err != nil {
		return nil, err
	}
	if tok, err := dec.Token(); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrCursorDigestInvalidJSON, err)
		}
		return nil, fmt.Errorf("%w: trailing data %v", ErrCursorDigestInvalidJSON, tok)
	}
	return []byte(sb.String()), nil
}

// CursorCanonicalToolArgsJSON 返回工具参数 JSON 的 canonical 字节。
//
// 空或仅空白的工具参数按空对象处理（"仅空白...视为同值"）；其它输入等价于 CursorCanonicalJSON。
// 工具 custom 文本输入不走本函数（不得当作 JSON 强行改写）。
func CursorCanonicalToolArgsJSON(raw []byte) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []byte("{}"), nil
	}
	return CursorCanonicalJSON(raw)
}

// cursorNormalizeJSONValue 用 token 流递归规范化一个 JSON 值并写入 sb。
func cursorNormalizeJSONValue(dec *json.Decoder, sb *strings.Builder, depth int) error {
	if depth > cursorDigestMaxJSONDepth {
		return fmt.Errorf("%w: nesting deeper than %d", ErrCursorDigestInvalidJSON, cursorDigestMaxJSONDepth)
	}
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCursorDigestInvalidJSON, err)
	}
	switch typed := tok.(type) {
	case json.Delim:
		switch typed {
		case '{':
			return cursorNormalizeJSONObject(dec, sb, depth)
		case '[':
			return cursorNormalizeJSONArray(dec, sb, depth)
		default:
			return fmt.Errorf("%w: unexpected delimiter %q", ErrCursorDigestInvalidJSON, typed.String())
		}
	case string:
		encoded, mErr := json.Marshal(typed)
		if mErr != nil {
			return fmt.Errorf("%w: %v", ErrCursorDigestInvalidJSON, mErr)
		}
		sb.Write(encoded)
		return nil
	case json.Number:
		canonical, nErr := cursorCanonicalJSONNumber(string(typed))
		if nErr != nil {
			return nErr
		}
		sb.WriteString(canonical)
		return nil
	case bool:
		if typed {
			sb.WriteString("true")
		} else {
			sb.WriteString("false")
		}
		return nil
	case nil:
		sb.WriteString("null")
		return nil
	default:
		return fmt.Errorf("%w: unsupported token %T", ErrCursorDigestInvalidJSON, tok)
	}
}

// cursorNormalizeJSONObject 规范化对象：键排序 + 重复键拒绝。
func cursorNormalizeJSONObject(dec *json.Decoder, sb *strings.Builder, depth int) error {
	type member struct {
		key   string
		value string
	}
	members := make([]member, 0, 8)
	seen := make(map[string]struct{}, 8)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("%w: %v", ErrCursorDigestInvalidJSON, err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return fmt.Errorf("%w: object key is %T", ErrCursorDigestInvalidJSON, keyTok)
		}
		if _, dup := seen[key]; dup {
			return fmt.Errorf("%w: duplicate object key %q", ErrCursorDigestInvalidJSON, key)
		}
		seen[key] = struct{}{}
		var valueSB strings.Builder
		if err := cursorNormalizeJSONValue(dec, &valueSB, depth+1); err != nil {
			return err
		}
		members = append(members, member{key: key, value: valueSB.String()})
	}
	if _, err := cursorExpectDelim(dec, '}'); err != nil {
		return err
	}
	sort.Slice(members, func(i, j int) bool { return members[i].key < members[j].key })
	sb.WriteByte('{')
	for i, m := range members {
		if i > 0 {
			sb.WriteByte(',')
		}
		encodedKey, mErr := json.Marshal(m.key)
		if mErr != nil {
			return fmt.Errorf("%w: %v", ErrCursorDigestInvalidJSON, mErr)
		}
		sb.Write(encodedKey)
		sb.WriteByte(':')
		sb.WriteString(m.value)
	}
	sb.WriteByte('}')
	return nil
}

// cursorNormalizeJSONArray 规范化数组：保序。
func cursorNormalizeJSONArray(dec *json.Decoder, sb *strings.Builder, depth int) error {
	sb.WriteByte('[')
	first := true
	for dec.More() {
		if !first {
			sb.WriteByte(',')
		}
		first = false
		if err := cursorNormalizeJSONValue(dec, sb, depth+1); err != nil {
			return err
		}
	}
	if _, err := cursorExpectDelim(dec, ']'); err != nil {
		return err
	}
	sb.WriteByte(']')
	return nil
}

// cursorExpectDelim 读取并校验容器闭合分隔符。
func cursorExpectDelim(dec *json.Decoder, want json.Delim) (json.Token, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCursorDigestInvalidJSON, err)
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != want {
		return nil, fmt.Errorf("%w: expected %q, got %v", ErrCursorDigestInvalidJSON, want.String(), tok)
	}
	return tok, nil
}

// cursorCanonicalJSONNumber 把 JSON 数字字面量转成精确十进制的 "<-?><coefficient>e<exponent>"。
//
// 全程字符串 + big.Int，不经过 float64/float32：
//   - 符号保留（"-0" 与 "0" 不同）。
//   - 系数 = 去掉小数点后的全部数字字符，保留尾随零（故 1.1 与 1.10 不同），
//     仅去掉无意义的前导零（保持至少一位）。
//   - 指数 = 字面指数 − 小数位数（用 big.Int 表示，允许极大指数而不溢出）。
func cursorCanonicalJSONNumber(literal string) (string, error) {
	s := literal
	if s == "" {
		return "", fmt.Errorf("%w: empty number", ErrCursorDigestInvalidJSON)
	}
	neg := false
	switch s[0] {
	case '-':
		neg = true
		s = s[1:]
	case '+':
		return "", fmt.Errorf("%w: illegal leading '+' in number %q", ErrCursorDigestInvalidJSON, literal)
	}
	if s == "" {
		return "", fmt.Errorf("%w: number without digits %q", ErrCursorDigestInvalidJSON, literal)
	}

	mantissa := s
	expLiteral := ""
	if idx := strings.IndexAny(s, "eE"); idx >= 0 {
		mantissa = s[:idx]
		expLiteral = s[idx+1:]
		if expLiteral == "" {
			return "", fmt.Errorf("%w: number without exponent digits %q", ErrCursorDigestInvalidJSON, literal)
		}
	}

	intPart := mantissa
	fracPart := ""
	if idx := strings.IndexByte(mantissa, '.'); idx >= 0 {
		intPart = mantissa[:idx]
		fracPart = mantissa[idx+1:]
		if fracPart == "" {
			return "", fmt.Errorf("%w: number with empty fraction %q", ErrCursorDigestInvalidJSON, literal)
		}
	}
	if intPart == "" {
		return "", fmt.Errorf("%w: number without integer part %q", ErrCursorDigestInvalidJSON, literal)
	}
	if !cursorAllDigits(intPart) {
		return "", fmt.Errorf("%w: illegal integer digits in %q", ErrCursorDigestInvalidJSON, literal)
	}
	if fracPart != "" && !cursorAllDigits(fracPart) {
		return "", fmt.Errorf("%w: illegal fraction digits in %q", ErrCursorDigestInvalidJSON, literal)
	}

	exp := new(big.Int)
	if expLiteral != "" {
		parsed, ok := new(big.Int).SetString(cursorNormalizeExponentLiteral(expLiteral), 10)
		if !ok {
			return "", fmt.Errorf("%w: illegal exponent in %q", ErrCursorDigestInvalidJSON, literal)
		}
		exp.Set(parsed)
	}
	// 小数点右移 len(fracPart) 位 ⇒ 指数减去该位数。
	exp.Sub(exp, new(big.Int).SetInt64(int64(len(fracPart))))

	coefficient := intPart + fracPart
	if !cursorAllDigits(coefficient) {
		return "", fmt.Errorf("%w: illegal coefficient in %q", ErrCursorDigestInvalidJSON, literal)
	}
	// 去掉无意义的前导零（保持至少一位）：JSON 数字字面量不允许前导零，
	// 而去掉前导零不改变值与精度；尾随零必须保留（精度变化不是同值）。
	coefficient = strings.TrimLeft(coefficient, "0")
	if coefficient == "" {
		coefficient = "0"
	}

	var sb strings.Builder
	sb.Grow(len(coefficient) + 8)
	if neg {
		sb.WriteByte('-')
	}
	sb.WriteString(coefficient)
	sb.WriteByte('e')
	sb.WriteString(exp.String())
	return sb.String(), nil
}

// cursorNormalizeExponentLiteral 只做语法清洗（允许前导 '+'/'-' 与数字），返回可直接 SetString 的串。
func cursorNormalizeExponentLiteral(lit string) string {
	if lit == "" {
		return lit
	}
	if lit[0] == '+' {
		return lit[1:]
	}
	return lit
}

// cursorAllDigits 判断是否为非空的全 ASCII 十进制数字串。
func cursorAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// CursorTextStream 是文本项的增量累加器（§25.14-D「流式分片」）。
//
// 写入过程中只累计 SHA-256 与字节长度，不保存正文；Finish 时才产出该项 canonical 字节
// 并把它并入历史链。因此同一逻辑文本的不同分帧方式得到相同历史摘要。
type CursorTextStream struct {
	h      hash.Hash
	n      uint64
	closed bool
}

// NewCursorTextStream 返回空文本累加器。
func NewCursorTextStream() *CursorTextStream {
	return &CursorTextStream{h: sha256.New()}
}

// Write 追加一段文本增量（逐字节，不做任何裁剪或换行改写）。
func (s *CursorTextStream) Write(p []byte) (int, error) {
	if s == nil {
		return 0, errors.New("cursor session digest: nil text stream")
	}
	if s.closed {
		return 0, ErrCursorDigestStreamClosed
	}
	if s.h == nil {
		s.h = sha256.New()
	}
	n, err := s.h.Write(p)
	s.n += uint64(n)
	return n, err
}

// WriteString 追加一段文本增量。
func (s *CursorTextStream) WriteString(str string) (int, error) {
	return s.Write([]byte(str))
}

// ByteLen 返回已累计的逻辑字节数（即该项文本长度）。
func (s *CursorTextStream) ByteLen() uint64 {
	if s == nil {
		return 0
	}
	return s.n
}

// Canonical 返回该项的 canonical 字节："sha256:<hex>:<字节长度>"。
//
// 它是项内容的确定指纹，与到达方式（一次写全还是多次分帧）无关。
func (s *CursorTextStream) Canonical() []byte {
	if s == nil {
		return nil
	}
	inner := s.h
	if inner == nil {
		inner = sha256.New()
	}
	var buf [sha256.Size]byte
	copy(buf[:], inner.Sum(nil))
	return []byte(fmt.Sprintf("sha256:%s:%d", hex.EncodeToString(buf[:]), s.n))
}

// Finish 结束该项并把 canonical 字节并入历史链；重复 Finish 返回错误。
func (s *CursorTextStream) Finish(d *CursorHistoryDigest, itemType string) error {
	if s == nil {
		return errors.New("cursor session digest: nil text stream")
	}
	if s.closed {
		return ErrCursorDigestStreamClosed
	}
	if err := d.AppendItem(itemType, s.Canonical()); err != nil {
		return err
	}
	s.closed = true
	return nil
}

// CursorJSONStream 是 JSON 项的增量累加器。
//
// JSON 无法在未知边界时流式哈希（对象键需排序），因此这里累计原始字节，
// Finish 时统一规范化并入链；保证不同分片边界产生相同摘要，且重复键/无效 JSON 在 Finish 报错。
type CursorJSONStream struct {
	buf    bytes.Buffer
	closed bool
}

// NewCursorJSONStream 返回空 JSON 累加器。
func NewCursorJSONStream() *CursorJSONStream {
	return &CursorJSONStream{}
}

// Write 追加一段 JSON 原始字节。
func (s *CursorJSONStream) Write(p []byte) (int, error) {
	if s == nil {
		return 0, errors.New("cursor session digest: nil json stream")
	}
	if s.closed {
		return 0, ErrCursorDigestStreamClosed
	}
	return s.buf.Write(p)
}

// WriteString 追加一段 JSON 原始字节。
func (s *CursorJSONStream) WriteString(str string) (int, error) {
	return s.Write([]byte(str))
}

// Finish 规范化已累计的 JSON 并并入历史链；重复 Finish 返回错误。
func (s *CursorJSONStream) Finish(d *CursorHistoryDigest, itemType string) error {
	if s == nil {
		return errors.New("cursor session digest: nil json stream")
	}
	if s.closed {
		return ErrCursorDigestStreamClosed
	}
	if err := d.AppendJSON(itemType, s.buf.Bytes()); err != nil {
		return err
	}
	s.closed = true
	return nil
}
