package xianguanjia

import (
	"bytes"
	"crypto/md5"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// D6b: 货源提卡方向（闲管家 → 我方）被调接口的六段签名验签中间件。
//
// 与 ERP 出站/推送方向（sign.go 的四段签名、handler.XianguanjiaSignatureVerifier）
// 相反：货源方向我方是被调方，闲管家在 query 带 mch_id/timestamp/sign（**三参**；
// app_id 不随 query 传输，只作为签名输入六段之一），
// body 为压缩 JSON，我方用**六段**公式重算并比对后放行。
//
// 官方契约（总单 /root/dispatch-D6.md「官方契约要点」；doc-4985015.md 已坐实）：
//
//	bodyMd5 = md5(raw body)          // 无 body 用 md5("{}")
//	sign    = md5("{app_id},{app_secret},{bodyMd5},{timestamp},{mch_id},{mch_secret}")
//
// query 三参（mch_id/timestamp/sign；app_id 不随 query 传输，只参与签名——
// 2026-10-04 生产 nginx 取证，闲管家 go-resty 客户端实测，与官方文档 query 四参
// 表述不符，以行为为准）：app_id 位验签时取库内已配置的 cfg.SupplyAppID
// （双方共知的管家应用 ID，与签名输入同源），库内 app_id 配错即 401 fail-closed。
// 两个密钥：app_secret（管家下发）与 mch_secret（我方自造），由 SupplyConfigReader
// 提供（D6a 的 SupplyConfigStore 交付实现）。
//
// 【编译期依赖说明 / 集成点】本单元需独立编译，故在本文件定义 SupplyConfig 与
// SupplyConfigReader 作为「内向契约」。D6a 的 supply_config.go 与 D6c 的
// supply_types.go 会各自给出同名类型，集成时以能编译为准保留一份：
//   - 本单元验签需要**明文** app_secret/mch_secret，故字段语义采用 D6a 的
//     SupplyAppSecret/MchSecret（Get 解密后返回明文）；D6c 的密文字段版
//     需让位（见证据 d6b.md「集成点」）。
//   - SupplyConfigStore.Get 的方法名是 Get，非 GetSupplyConfig；D6a 只需一个
//     极小适配器或别名方法即可满足 SupplyConfigReader。

// ---- 官方全局错误码（货源接口信封 {code,msg,data}，与 ERP result=success 不同） ----
//
// SupplyCodeSignError/SupplyCodeTimestampExpired 与 D6c 的 supply_types.go 同名同值；
// 集成去重时保留一份即可（若保留 D6c 版本，本处删除即可）。

// supplyTimestampWindowSec 是货源接口的 timestamp 新鲜度窗口（秒）。
// 参照 ERP 推送方向 5 分钟（api-93586387.md:36），官方货源文档未坐实数值，
// 保守取 300s，联调时以管家实际行为校正（记入证据 d6b.md 联调校正清单）。
const supplyTimestampWindowSec = 300

// supplyTimestampMaxFutureSkewSec 允许的最大时钟前移（防双方时钟轻微不一致误杀）。
const supplyTimestampMaxFutureSkewSec = 5

// supplyMaxBodyBytes 限制被调接口请求 body 大小（防超大 body 拖垮验签）。
const supplyMaxBodyBytes = 1 << 20

// ErrSupplySignMismatch 是六段签名比对不通过的哨兵错误（供 errors.Is 判定）。
var ErrSupplySignMismatch = errors.New("xianguanjia: supply sign mismatch")

// SupplySign 按官方六段公式计算货源被调接口签名。
//
// sign = md5("{app_id},{app_secret},{bodyMd5},{timestamp},{mch_id},{mch_secret}")
//
// 六段、英文逗号分隔、固定顺序、无 key 排序。secret 以参数传入，本函数绝不记录日志。
func SupplySign(appID, appSecret, bodyMd5, timestamp, mchID, mchSecret string) string {
	var b strings.Builder
	b.Grow(len(appID) + len(appSecret) + len(bodyMd5) + len(timestamp) + len(mchID) + len(mchSecret) + 5)
	b.WriteString(appID)
	b.WriteByte(',')
	b.WriteString(appSecret)
	b.WriteByte(',')
	b.WriteString(bodyMd5)
	b.WriteByte(',')
	b.WriteString(timestamp)
	b.WriteByte(',')
	b.WriteString(mchID)
	b.WriteByte(',')
	b.WriteString(mchSecret)
	sum := md5.Sum([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// VerifySupplySign 是验签纯函数核：用给定明文密钥重算六段签名并与 querySign 恒时比对。
//
// 返回 nil 表示签名一致；不一致返回 ErrSupplySignMismatch。
//
// 说明：dispatch 原型列出的 5 参（rawBody + query 四参）无法完成「重算比对」
// ——重算必须知道 app_secret/mch_secret，故实现补充 appSecret/mchSecret 两参，
// 参数顺序保持 query 语义在前。bodyMd5 复用 BodyMd5（无 body 用 md5("{}")）。
func VerifySupplySign(
	rawBody []byte,
	queryAppID, queryTimestamp, queryMchID, querySign string,
	appSecret, mchSecret string,
) error {
	bodyMd5 := BodyMd5(rawBody)
	expected := SupplySign(queryAppID, appSecret, bodyMd5, queryTimestamp, queryMchID, mchSecret)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(querySign)) != 1 {
		return ErrSupplySignMismatch
	}
	return nil
}

// supplyRespond 输出官方货源接口错误信封 {code,msg,data}。
// HTTP 状态恒为 200：官方以 body 内 code 判定业务结果（全局错误码表）。
func supplyRespond(c *gin.Context, code int, msg string) {
	c.AbortWithStatusJSON(http.StatusOK, gin.H{
		"code": code,
		"msg":  msg,
		"data": nil,
	})
}

// SupplySignMiddleware 返回六段验签 gin 中间件。
//
// 流程（顺序即 fail-closed 边界）：
//  1. 读 raw body（限长）后**还原** c.Request.Body，供后续 handler 正常读取；
//  2. 提取 query mch_id/timestamp/sign（app_id 不随 query 传输，验签取自库内
//     cfg.SupplyAppID），校验 timestamp 在 300s 窗口内，
//     超窗 → {code:408,msg:"时间戳已超过有效期"} 中止；
//  3. 读货源配置，无配置/读取失败 → {code:1,msg:"货源未配置"} 中止；
//  4. 六段重算比对，不符 → {code:401,msg:"签名错误"} 中止；
//  5. 通过 → c.Next()。
//
// cfgReader 为 nil 时全程 fail-closed（第 3 步返回未配置）。
func SupplySignMiddleware(cfgReader SupplyConfigReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 1. 读原始 body 并还原，保证签名用的是未经解析的字节，且后续 handler 可再读。
		rawBody, err := io.ReadAll(io.LimitReader(c.Request.Body, supplyMaxBodyBytes))
		if err != nil {
			supplyRespond(c, SupplyCodeSignError, "签名错误")
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(rawBody))

		// 2. timestamp 新鲜度。
		tsStr := c.Query("timestamp")
		ts, err := strconv.ParseInt(tsStr, 10, 64)
		if err != nil {
			supplyRespond(c, SupplyCodeTimestampExpired, "时间戳已超过有效期")
			return
		}
		now := time.Now().Unix()
		if ts > now+int64(supplyTimestampMaxFutureSkewSec) || now-ts > int64(supplyTimestampWindowSec) {
			supplyRespond(c, SupplyCodeTimestampExpired, "时间戳已超过有效期")
			return
		}

		// 3. 读配置（fail-closed）。
		if cfgReader == nil {
			supplyRespond(c, SupplyCodeNoConfig, "货源未配置")
			return
		}
		cfg, err := cfgReader.GetSupplyConfig(c.Request.Context())
		if err != nil || cfg == nil || cfg.SupplyAppSecret == "" || cfg.MchSecret == "" {
			// 无配置、读取失败、或密钥缺失（D6a 的 Get 对部分配置已归一为未配置；
			// 此处再兜一层，确保任何缺密钥路径都 fail-closed 到「货源未配置」）。
			supplyRespond(c, SupplyCodeNoConfig, "货源未配置")
			return
		}

		// 4. 六段重算比对（app_id 位取库内 cfg.SupplyAppID，与签名输入同源；
		//    query 中的 app_id 被完全忽略、不参与验签。库内 SupplyAppID 与
		//    签名所用 app_id 不一致 → 恒失配 401 fail-closed。mch_id 仍来自 query）。
		if err := VerifySupplySign(
			rawBody,
			cfg.SupplyAppID, tsStr, c.Query("mch_id"), c.Query("sign"),
			cfg.SupplyAppSecret, cfg.MchSecret,
		); err != nil {
			supplyRespond(c, SupplyCodeSignError, "签名错误")
			return
		}

		// 5. 放行。
		c.Next()
	}
}
