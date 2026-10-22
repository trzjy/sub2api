//go:build unit

package service

// 本文件是 §25.14-D「摘要算法」中上下文摘要引擎的定向验证。
//
// 覆盖（对应派发单 CR-B 的验收条款）：
//   - JSON 键序/空白不误拒（同值同摘要）；
//   - 大整数不失真（9007199254740993 与 9007199254740992 摘要不同，且都不等于 float64 近似值）；
//   - 文本逐字节保留（仅换行/尾部空白差异必须被拒，即摘要不同）；
//   - 不同分帧方式摘要相同 / 不同分片边界摘要相同；
//   - 重复键、无效 JSON 明确拒绝；
//   - 链式公式与类型命名空间符合 Hn = SHA256(H(n-1) || type || length8 || canonical)。

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
)

// cursorDigestManualChain 用与实现无关的手写步骤复算链式公式，用于反证实现真的按公式算。
func cursorDigestManualChain(t *testing.T, steps []struct {
	itemType  string
	canonical []byte
}) [sha256.Size]byte {
	t.Helper()
	prev := sha256.Sum256([]byte("sub2api/cursor/session-history-digest/v1"))
	for _, step := range steps {
		var length [8]byte
		n := uint64(len(step.canonical))
		for i := 7; i >= 0; i-- {
			length[i] = byte(n)
			n >>= 8
		}
		h := sha256.New()
		h.Write(prev[:])
		h.Write([]byte(step.itemType))
		h.Write(length[:])
		h.Write(step.canonical)
		var next [sha256.Size]byte
		copy(next[:], h.Sum(nil))
		prev = next
	}
	return prev
}

// TestCursorSessionDigestChainFormula 校验固定起点 H0、长度 8 字节 big-endian、
// 以及 type 参与哈希（直接对照手写公式）。
func TestCursorSessionDigestChainFormula(t *testing.T) {
	t.Run("空历史等于固定起点H0", func(t *testing.T) {
		d := NewCursorHistoryDigest()
		want := sha256.Sum256([]byte("sub2api/cursor/session-history-digest/v1"))
		if d.Sum() != want {
			t.Fatalf("空历史摘要应等于 H0\n got=%x\nwant=%x", d.Sum(), want)
		}
		if d.ItemCount() != 0 {
			t.Fatalf("空历史项数应为 0，实际 %d", d.ItemCount())
		}
	})

	t.Run("零值等价于构造器", func(t *testing.T) {
		var zero CursorHistoryDigest
		if zero.Sum() != NewCursorHistoryDigest().Sum() {
			t.Fatal("零值 Sum 应等于 H0")
		}
		if err := zero.AppendText("chat.message", "hi"); err != nil {
			t.Fatalf("零值 AppendText 失败: %v", err)
		}
		want := cursorDigestManualChain(t, []struct {
			itemType  string
			canonical []byte
		}{{"chat.message", []byte("hi")}})
		if zero.Sum() != want {
			t.Fatalf("零值链值不符\n got=%x\nwant=%x", zero.Sum(), want)
		}
	})

	t.Run("三项链值与手写公式一致", func(t *testing.T) {
		d := NewCursorHistoryDigest()
		steps := []struct {
			itemType  string
			canonical []byte
		}{
			{"chat.message", []byte("hello \r\nworld ")},
			{"responses.item.function_call", []byte(`{"a":1e0}`)},
			{"gemini.part.functionResponse", []byte("")},
		}
		for _, s := range steps {
			if err := d.AppendItem(s.itemType, s.canonical); err != nil {
				t.Fatalf("AppendItem(%s) 失败: %v", s.itemType, err)
			}
		}
		if d.ItemCount() != len(steps) {
			t.Fatalf("项数应为 %d，实际 %d", len(steps), d.ItemCount())
		}
		if want := cursorDigestManualChain(t, steps); d.Sum() != want {
			t.Fatalf("链值不符\n got=%x\nwant=%x", d.Sum(), want)
		}
	})

	t.Run("类型命名空间参与哈希", func(t *testing.T) {
		a := NewCursorHistoryDigest()
		b := NewCursorHistoryDigest()
		if err := a.AppendItem("chat.message", []byte("x")); err != nil {
			t.Fatalf("append a: %v", err)
		}
		if err := b.AppendItem("responses.item.message", []byte("x")); err != nil {
			t.Fatalf("append b: %v", err)
		}
		if a.Sum() == b.Sum() {
			t.Fatal("仅类型不同的同一 canonical 内容必须产生不同摘要")
		}
	})

	t.Run("顺序参与哈希", func(t *testing.T) {
		a := NewCursorHistoryDigest()
		b := NewCursorHistoryDigest()
		_ = a.AppendText("chat.message", "one")
		_ = a.AppendText("chat.message", "two")
		_ = b.AppendText("chat.message", "two")
		_ = b.AppendText("chat.message", "one")
		if a.Sum() == b.Sum() {
			t.Fatal("项顺序变化必须产生不同摘要")
		}
	})

	t.Run("非法项类型被拒绝", func(t *testing.T) {
		cases := []string{"", "chat message", "chat\x00message", strings.Repeat("a", 129)}
		for _, itemType := range cases {
			d := NewCursorHistoryDigest()
			err := d.AppendItem(itemType, []byte("x"))
			if !errors.Is(err, ErrCursorDigestItemType) {
				t.Fatalf("itemType=%q 期望 ErrCursorDigestItemType，实际 %v", itemType, err)
			}
			if d.ItemCount() != 0 {
				t.Fatalf("itemType=%q 失败后不得推进链，实际项数 %d", itemType, d.ItemCount())
			}
		}
	})
}

// TestCursorSessionDigestJSONKeyOrderAndWhitespace 校验对象键序/空白差异不误拒。
func TestCursorSessionDigestJSONKeyOrderAndWhitespace(t *testing.T) {
	t.Run("顶层键序与空白不改变摘要", func(t *testing.T) {
		variants := []string{
			`{"a":1,"b":"x"}`,
			`{ "b" : "x" , "a" : 1 }`,
			"\n{\n\t\"a\":\t1,\n\"b\": \"x\"\n}\n",
			`{"b":"x","a":1}`,
		}
		base, err := CursorCanonicalJSON([]byte(variants[0]))
		if err != nil {
			t.Fatalf("基线规范化失败: %v", err)
		}
		for i, v := range variants {
			got, err := CursorCanonicalJSON([]byte(v))
			if err != nil {
				t.Fatalf("变体 %d 被误拒: %v (输入 %s)", i, err, v)
			}
			if string(got) != string(base) {
				t.Fatalf("变体 %d canonical 不同\n got=%s\nbase=%s", i, got, base)
			}
			d1 := NewCursorHistoryDigest()
			d2 := NewCursorHistoryDigest()
			if err := d1.AppendToolArgsJSON("chat.tool_call.args", []byte(variants[0])); err != nil {
				t.Fatalf("append base: %v", err)
			}
			if err := d2.AppendToolArgsJSON("chat.tool_call.args", []byte(v)); err != nil {
				t.Fatalf("append variant %d: %v", i, err)
			}
			if d1.Sum() != d2.Sum() {
				t.Fatalf("变体 %d 摘要不同", i)
			}
		}
	})

	t.Run("嵌套对象与数组保序", func(t *testing.T) {
		x, err := CursorCanonicalJSON([]byte(`{"z":[1,2,3],"a":{"y":true,"b":null}}`))
		if err != nil {
			t.Fatalf("规范化失败: %v", err)
		}
		y, err := CursorCanonicalJSON([]byte(`{ "a" : { "b" : null , "y" : true } , "z" : [1, 2, 3] }`))
		if err != nil {
			t.Fatalf("规范化失败: %v", err)
		}
		if string(x) != string(y) {
			t.Fatalf("嵌套键序差异应同值\n x=%s\n y=%s", x, y)
		}
		if string(x) != `{"a":{"b":null,"y":true},"z":[1e0,2e0,3e0]}` {
			t.Fatalf("规范化输出形状不符: %s", x)
		}
		z, err := CursorCanonicalJSON([]byte(`{"z":[3,2,1],"a":{"y":true,"b":null}}`))
		if err != nil {
			t.Fatalf("规范化失败: %v", err)
		}
		if string(x) == string(z) {
			t.Fatal("数组顺序变化必须改变摘要（数组保序）")
		}
	})

	t.Run("字符串内容差异不是空白差异", func(t *testing.T) {
		a, err := CursorCanonicalJSON([]byte(`{"s":"value"}`))
		if err != nil {
			t.Fatalf("规范化失败: %v", err)
		}
		b, err := CursorCanonicalJSON([]byte(`{"s":"value "}`))
		if err != nil {
			t.Fatalf("规范化失败: %v", err)
		}
		if string(a) == string(b) {
			t.Fatal("字符串内容（含尾部空格）变化必须改变摘要")
		}
	})
}

// TestCursorSessionDigestBigIntegerPrecision 是「未走 float64」的关键证据。
func TestCursorSessionDigestBigIntegerPrecision(t *testing.T) {
	const (
		exact      = "9007199254740993" // 2^53 + 1，float64 无法精确表示
		rounded    = "9007199254740992" // 2^53，exact 的 float64 近似值
		hugeInt    = "123456789012345678901234567890"
		hugeIntAlt = "123456789012345678901234567891"
	)

	t.Run("2^53+1 的 canonical 保留全部数字", func(t *testing.T) {
		canonical, err := CursorCanonicalJSON([]byte(`{"n":` + exact + `}`))
		if err != nil {
			t.Fatalf("规范化失败: %v", err)
		}
		if !strings.Contains(string(canonical), exact) {
			t.Fatalf("canonical 丢失原始数字精度: %s", canonical)
		}
		if string(canonical) != `{"n":`+exact+`e0}` {
			t.Fatalf("canonical 形状不符: %s", canonical)
		}
	})

	t.Run("float64 路径会塌陷而本实现不会", func(t *testing.T) {
		exactCanonical, err := CursorCanonicalJSON([]byte(`{"n":` + exact + `}`))
		if err != nil {
			t.Fatalf("规范化失败: %v", err)
		}

		// 证明测试用例本身是有效的：Go 的 float64 解析确实把 2^53+1 塌陷成 2^53。
		f, convErr := strconv.ParseFloat(exact, 64)
		if convErr != nil {
			t.Fatalf("ParseFloat 失败: %v", convErr)
		}
		floatLiteral := strconv.FormatFloat(f, 'g', -1, 64)
		floatCanonical, err := CursorCanonicalJSON([]byte(`{"n":` + floatLiteral + `}`))
		if err != nil {
			t.Fatalf("float64 近似值规范化失败: %v", err)
		}
		roundedCanonical, err := CursorCanonicalJSON([]byte(`{"n":` + rounded + `}`))
		if err != nil {
			t.Fatalf("规范化失败: %v", err)
		}

		if string(floatCanonical) != string(roundedCanonical) {
			t.Fatalf("前置断言失败：float64 近似值应等于 2^53\n float=%s\nrounded=%s",
				floatCanonical, roundedCanonical)
		}
		if string(exactCanonical) == string(floatCanonical) {
			t.Fatalf("实现疑似经 float64：2^53+1 与 float64 近似值 summary 相同 (%s)", exactCanonical)
		}

		dExact := NewCursorHistoryDigest()
		dRounded := NewCursorHistoryDigest()
		if err := dExact.AppendJSON("tools.schema", []byte(`{"n":`+exact+`}`)); err != nil {
			t.Fatalf("append exact: %v", err)
		}
		if err := dRounded.AppendJSON("tools.schema", []byte(`{"n":`+rounded+`}`)); err != nil {
			t.Fatalf("append rounded: %v", err)
		}
		if dExact.Sum() == dRounded.Sum() {
			t.Fatal("9007199254740993 与 9007199254740992 必须产生不同摘要")
		}
		if dExact.HexString() == "" || dRounded.HexString() == "" {
			t.Fatal("HexString 不应为空")
		}
	})

	t.Run("超长整数逐位保留", func(t *testing.T) {
		a, err := CursorCanonicalJSON([]byte(`{"n":` + hugeInt + `}`))
		if err != nil {
			t.Fatalf("规范化失败: %v", err)
		}
		b, err := CursorCanonicalJSON([]byte(`{"n":` + hugeIntAlt + `}`))
		if err != nil {
			t.Fatalf("规范化失败: %v", err)
		}
		if string(a) == string(b) {
			t.Fatal("超长整数最后一位不同必须产生不同 canonical")
		}
		if string(a) != `{"n":`+hugeInt+`e0}` {
			t.Fatalf("超长整数 canonical 形状不符: %s", a)
		}
	})

	t.Run("带指数与大指数的精确十进制表示", func(t *testing.T) {
		cases := []struct {
			in   string
			want string
		}{
			{`1`, "1e0"},
			{`-0`, "-0e0"},
			{`1.5`, "15e-1"},
			{`-1.50`, "-150e-2"},
			{`1e3`, "1e3"},
			{`1.5e2`, "15e1"},
			{`2.5E-3`, "25e-4"},
			{`1e999999999999999999`, "1e999999999999999999"},
			{`0.0001`, "1e-4"},
			{`0.10`, "10e-2"},
		}
		for _, c := range cases {
			got, err := cursorCanonicalJSONNumber(c.in)
			if err != nil {
				t.Fatalf("number %q 失败: %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("number %q => %q，期望 %q", c.in, got, c.want)
			}
			// 输出必须是合法 JSON 数字字面量（用 UseNumber 解析，避免探针自己走 float64）。
			probeDec := json.NewDecoder(strings.NewReader(got))
			probeDec.UseNumber()
			var probe interface{}
			if err := probeDec.Decode(&probe); err != nil {
				t.Fatalf("canonical 数字 %q 不是合法 JSON: %v", got, err)
			}
			if _, ok := probe.(json.Number); !ok {
				t.Fatalf("canonical 数字 %q 解析类型应为 json.Number，实际 %T", got, probe)
			}
			if err := probeDec.Decode(new(interface{})); err != io.EOF {
				t.Fatalf("canonical 数字 %q 存在尾随内容", got)
			}
		}
		for _, bad := range []string{"", "+1", "1.", ".1", "1e", "1e+", "0x1", "1..2", "NaN"} {
			if got, err := cursorCanonicalJSONNumber(bad); err == nil {
				t.Fatalf("非法数字 %q 应被拒绝，实际得到 %q", bad, got)
			}
		}
	})
}

// TestCursorSessionDigestTextByteExact 校验文本逐字节保留（不 trim、不改换行）。
func TestCursorSessionDigestTextByteExact(t *testing.T) {
	base := "line1\nline2"
	variants := map[string]string{
		"尾部换行":    base + "\n",
		"尾部空格":    base + " ",
		"尾部制表符":   base + "\t",
		"头部空格":    " " + base,
		"CRLF换行":  strings.ReplaceAll(base, "\n", "\r\n"),
		"大小写":     "Line1\nline2",
		"Unicode": "line1\nline２",
	}

	dBase := NewCursorHistoryDigest()
	if err := dBase.AppendText("chat.message", base); err != nil {
		t.Fatalf("append base: %v", err)
	}
	if got, want := string(CursorCanonicalText(base)), base; got != want {
		t.Fatalf("CursorCanonicalText 必须逐字节原样返回\n got=%q\nwant=%q", got, want)
	}

	seen := map[string]string{dBase.HexString(): "base"}
	for name, variant := range variants {
		t.Run(name, func(t *testing.T) {
			d := NewCursorHistoryDigest()
			if err := d.AppendText("chat.message", variant); err != nil {
				t.Fatalf("append %s: %v", name, err)
			}
			if d.Sum() == dBase.Sum() {
				t.Fatalf("%s 与基线摘要相同，说明文本被裁剪或归一 (variant=%q)", name, variant)
			}
			key := d.HexString()
			if prev, dup := seen[key]; dup {
				t.Fatalf("%s 与 %s 摘要碰撞（不同逻辑文本必须不同）", name, prev)
			}
			seen[key] = name
		})
	}

	t.Run("完全相同的文本摘要相同", func(t *testing.T) {
		dup := NewCursorHistoryDigest()
		if err := dup.AppendText("chat.message", base); err != nil {
			t.Fatalf("append dup: %v", err)
		}
		if dup.Sum() != dBase.Sum() {
			t.Fatal("相同文本必须得到相同摘要")
		}
	})
}

// TestCursorSessionDigestStreamFramingInvariance 校验流式分片：不同分帧方式摘要相同。
func TestCursorSessionDigestStreamFramingInvariance(t *testing.T) {
	const content = "第一段\r\nsecond chunk with trailing space \n第三段  "
	perRune := make([]string, 0, len(content))
	for _, r := range content {
		perRune = append(perRune, string(r))
	}
	framings := [][]string{
		{content},
		{"第一段", "\r\nsecond chunk with trailing space \n第三段  "},
		{"第一段\r\n", "second", " chunk with trailing space ", "\n第三段  "},
		perRune,
	}

	digests := make([]string, 0, len(framings))
	for i, chunks := range framings {
		t.Run(fmt.Sprintf("分帧方式_%d", i), func(t *testing.T) {
			stream := NewCursorTextStream()
			for _, chunk := range chunks {
				if _, err := stream.WriteString(chunk); err != nil {
					t.Fatalf("WriteString 失败: %v", err)
				}
			}
			d := NewCursorHistoryDigest()
			if err := stream.Finish(d, "chat.message"); err != nil {
				t.Fatalf("Finish 失败: %v", err)
			}
			if d.ItemCount() != 1 {
				t.Fatalf("Finish 应只产生一个逻辑项，实际 %d", d.ItemCount())
			}
			digests = append(digests, d.HexString())
		})
	}
	for i := 1; i < len(digests); i++ {
		if digests[i] != digests[0] {
			t.Fatalf("分帧方式 %d 摘要与基准不同\n got=%s\nbase=%s", i, digests[i], digests[0])
		}
	}

	t.Run("分帧不影响后续项链", func(t *testing.T) {
		d1 := NewCursorHistoryDigest()
		s1 := NewCursorTextStream()
		_, _ = s1.WriteString("abc")
		if err := s1.Finish(d1, "chat.message"); err != nil {
			t.Fatalf("finish d1: %v", err)
		}
		if err := d1.AppendToolArgsJSON("chat.tool_call.args", []byte(`{"k":1}`)); err != nil {
			t.Fatalf("append args d1: %v", err)
		}

		d2 := NewCursorHistoryDigest()
		s2 := NewCursorTextStream()
		_, _ = s2.WriteString("a")
		_, _ = s2.WriteString("b")
		_, _ = s2.WriteString("c")
		if err := s2.Finish(d2, "chat.message"); err != nil {
			t.Fatalf("finish d2: %v", err)
		}
		if err := d2.AppendToolArgsJSON("chat.tool_call.args", []byte(`{ "k" : 1 }`)); err != nil {
			t.Fatalf("append args d2: %v", err)
		}

		if !d1.Equal(d2) {
			t.Fatalf("分帧与JSON空白差异不应改变多项链摘要\n d1=%s\nd2=%s", d1.HexString(), d2.HexString())
		}
	})

	t.Run("文本长度与字节相同但内容不同摘要不同", func(t *testing.T) {
		a := NewCursorTextStream()
		_, _ = a.WriteString("abc")
		b := NewCursorTextStream()
		_, _ = b.WriteString("abd")
		if a.ByteLen() != b.ByteLen() {
			t.Fatalf("前置断言失败：长度应相同 %d vs %d", a.ByteLen(), b.ByteLen())
		}
		if string(a.Canonical()) == string(b.Canonical()) {
			t.Fatal("同长度不同内容必须产生不同 canonical")
		}
	})

	t.Run("Finish 后禁写且禁重复Finish", func(t *testing.T) {
		stream := NewCursorTextStream()
		_, _ = stream.WriteString("x")
		d := NewCursorHistoryDigest()
		if err := stream.Finish(d, "chat.message"); err != nil {
			t.Fatalf("首次 Finish 失败: %v", err)
		}
		if _, err := stream.WriteString("y"); !errors.Is(err, ErrCursorDigestStreamClosed) {
			t.Fatalf("Finish 后写入应返回 ErrCursorDigestStreamClosed，实际 %v", err)
		}
		if err := stream.Finish(d, "chat.message"); !errors.Is(err, ErrCursorDigestStreamClosed) {
			t.Fatalf("重复 Finish 应返回 ErrCursorDigestStreamClosed，实际 %v", err)
		}
		if d.ItemCount() != 1 {
			t.Fatalf("重复 Finish 不得推进链，实际项数 %d", d.ItemCount())
		}
	})
}

// TestCursorSessionDigestChunkBoundaryInvariance 校验 JSON 项在不同分片边界下摘要相同。
func TestCursorSessionDigestChunkBoundaryInvariance(t *testing.T) {
	const doc = `{"alpha":"值一","beta":[1,2,{"gamma":9007199254740993}],"delta":null}`

	t.Run("任意切分点摘要相同", func(t *testing.T) {
		base := NewCursorHistoryDigest()
		s := NewCursorJSONStream()
		_, _ = s.WriteString(doc)
		if err := s.Finish(base, "responses.item.function_call"); err != nil {
			t.Fatalf("基线 Finish 失败: %v", err)
		}

		for cut := 0; cut <= len(doc); cut++ {
			stream := NewCursorJSONStream()
			// 逐字节写入，切分点覆盖字符串内部、多字节 UTF-8 内部、数字内部。
			if _, err := stream.WriteString(doc[:cut]); err != nil {
				t.Fatalf("cut=%d 写入失败: %v", cut, err)
			}
			if _, err := stream.WriteString(doc[cut:]); err != nil {
				t.Fatalf("cut=%d 写入失败: %v", cut, err)
			}
			d := NewCursorHistoryDigest()
			if err := stream.Finish(d, "responses.item.function_call"); err != nil {
				t.Fatalf("cut=%d Finish 失败: %v", cut, err)
			}
			if d.Sum() != base.Sum() {
				t.Fatalf("切分点 %d 摘要与基线不同", cut)
			}
		}
	})

	t.Run("同内容不同键序的分片流摘要相同", func(t *testing.T) {
		a := NewCursorJSONStream()
		_, _ = a.WriteString(`{"b":1,`)
		_, _ = a.WriteString(`"a":2}`)
		da := NewCursorHistoryDigest()
		if err := a.Finish(da, "tools.declaration"); err != nil {
			t.Fatalf("finish a: %v", err)
		}

		b := NewCursorJSONStream()
		_, _ = b.WriteString(`{ "a" : 2 , "b" : 1 }`)
		db := NewCursorHistoryDigest()
		if err := b.Finish(db, "tools.declaration"); err != nil {
			t.Fatalf("finish b: %v", err)
		}
		if da.Sum() != db.Sum() {
			t.Fatal("分片方式与对象键序差异不应改变摘要")
		}
	})
}

// TestCursorSessionDigestRejectsInvalidJSON 校验重复键与无效 JSON 明确拒绝。
func TestCursorSessionDigestRejectsInvalidJSON(t *testing.T) {
	cases := map[string]string{
		"顶层重复键":    `{"a":1,"a":1}`,
		"重复键不同值":   `{"a":1,"a":2}`,
		"嵌套重复键":    `{"o":{"k":1,"k":2}}`,
		"数组内对象重复键": `[{"x":1,"x":2}]`,
		"无效JSON":   `{"a":}`,
		"尾随垃圾":     `{"a":1} {"b":2}`,
		"未闭合对象":    `{"a":1`,
		"未闭合数组":    `[1,2`,
		"空输入":      ``,
		"仅空白":      "  \n\t ",
		"非JSON字面量": `nul`,
		"非法数字":     `{"a":01}`,
		"深度超限":     strings.Repeat("[", 300) + strings.Repeat("]", 300),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := CursorCanonicalJSON([]byte(in))
			if err == nil {
				t.Fatalf("输入 %q 应被拒绝，实际得到 %s", in, got)
			}
			if !errors.Is(err, ErrCursorDigestInvalidJSON) {
				t.Fatalf("输入 %q 期望 ErrCursorDigestInvalidJSON，实际 %v", in, err)
			}
			// 拒绝时不得推进历史链。
			d := NewCursorHistoryDigest()
			before := d.Sum()
			if err := d.AppendJSON("chat.message", []byte(in)); err == nil {
				t.Fatalf("AppendJSON 对非法输入 %q 不应成功", in)
			}
			if d.Sum() != before || d.ItemCount() != 0 {
				t.Fatalf("拒绝后链状态被改动: items=%d", d.ItemCount())
			}
			// 流式累加器同样必须拒绝。
			stream := NewCursorJSONStream()
			_, _ = stream.WriteString(in)
			if err := stream.Finish(d, "chat.message"); err == nil {
				t.Fatalf("CursorJSONStream 对非法输入 %q 不应成功", in)
			}
		})
	}

	t.Run("重复键不被静默取首值", func(t *testing.T) {
		first, err := CursorCanonicalJSON([]byte(`{"a":1,"a":1}`))
		if err == nil {
			t.Fatalf("重复键应报错而不是返回 %s", first)
		}
	})
}

// TestCursorSessionDigestToolArgsEquivalence 校验工具参数 JSON 的同值/不同值边界。
func TestCursorSessionDigestToolArgsEquivalence(t *testing.T) {
	sameValue := [][2]string{
		{`{"path":"/a","n":1}`, `{"n":1,"path":"/a"}`},
		{`{"path":"/a","n":1}`, "{\n  \"n\" : 1 ,\n  \"path\" : \"/a\"\n}"},
		{``, `{}`},
		{"   \n\t ", `{ }`},
	}
	for i, pair := range sameValue {
		t.Run(fmt.Sprintf("同值_%d", i), func(t *testing.T) {
			a, err := CursorCanonicalToolArgsJSON([]byte(pair[0]))
			if err != nil {
				t.Fatalf("%q 规范化失败: %v", pair[0], err)
			}
			b, err := CursorCanonicalToolArgsJSON([]byte(pair[1]))
			if err != nil {
				t.Fatalf("%q 规范化失败: %v", pair[1], err)
			}
			if string(a) != string(b) {
				t.Fatalf("应视为同值\n a=%s\n b=%s", a, b)
			}
			da := NewCursorHistoryDigest()
			db := NewCursorHistoryDigest()
			if err := da.AppendToolArgsJSON("chat.tool_call.args", []byte(pair[0])); err != nil {
				t.Fatalf("append a: %v", err)
			}
			if err := db.AppendToolArgsJSON("chat.tool_call.args", []byte(pair[1])); err != nil {
				t.Fatalf("append b: %v", err)
			}
			if da.Sum() != db.Sum() {
				t.Fatal("同值工具参数摘要应相同")
			}
		})
	}

	differentValue := []struct {
		name string
		a    string
		b    string
	}{
		{"字符串内容", `{"q":"abc"}`, `{"q":"abd"}`},
		{"字符串尾空格", `{"q":"abc"}`, `{"q":"abc "}`},
		{"数组顺序", `{"q":[1,2]}`, `{"q":[2,1]}`},
		{"数值精度", `{"q":1.1}`, `{"q":1.10}`},
		{"数值大整数", `{"q":9007199254740993}`, `{"q":9007199254740992}`},
		{"键数量", `{"q":1}`, `{"q":1,"r":2}`},
		{"类型", `{"q":1}`, `{"q":"1"}`},
	}
	for _, c := range differentValue {
		t.Run("不同值_"+c.name, func(t *testing.T) {
			a, err := CursorCanonicalToolArgsJSON([]byte(c.a))
			if err != nil {
				t.Fatalf("%q 规范化失败: %v", c.a, err)
			}
			b, err := CursorCanonicalToolArgsJSON([]byte(c.b))
			if err != nil {
				t.Fatalf("%q 规范化失败: %v", c.b, err)
			}
			if string(a) == string(b) {
				t.Fatalf("%s 变化必须不同值，实际都是 %s", c.name, a)
			}
		})
	}
}

// TestCursorSessionDigestCloneAndEqual 校验 Clone/Equal/HexString 的语义。
func TestCursorSessionDigestCloneAndEqual(t *testing.T) {
	d := NewCursorHistoryDigest()
	_ = d.AppendText("chat.message", "hello")
	clone := d.Clone()
	if !clone.Equal(d) {
		t.Fatal("Clone 应等于原摘要")
	}
	_ = clone.AppendText("chat.message", "world")
	if d.ItemCount() != 1 || clone.ItemCount() != 2 {
		t.Fatalf("Clone 必须独立推进，实际原=%d 克隆=%d", d.ItemCount(), clone.ItemCount())
	}
	if d.Equal(clone) {
		t.Fatal("推进后的克隆不应再等于原摘要")
	}
	if len(d.HexString()) != 64 {
		t.Fatalf("HexString 长度应为 64，实际 %d", len(d.HexString()))
	}
	if d.Sum() == NewCursorHistoryDigest().Sum() {
		t.Fatal("含一项的历史不应等于空历史")
	}
}
