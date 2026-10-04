package xianguanjia

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// 商品目录被调接口（D6c）：查询平台信息 / 查询商户信息 / 查询商品列表 / 查询商品详情。
//
// 数据源映射（本仓库无独立「商品」表，商品由分组聚合，详见证据 d6c.md）：
//
//	groups.id ──(0/1)──> subscription_plans.group_id （分组售价，price_cny）
//
//   - goods_no     = groups.id 的十进制字符串
//   - goods_type   = 商品类型，我方货源全为卡密，恒为 2
//   - goods_name   = groups.name
//   - price        = 该分组在售套餐价 subscription_plans.price_cny 换算为「分」（无则 0）
//   - stock        = 无限生成库存固定 99999（用户裁定无限库存；官方 stock 为 int32，
//     99999 为官方文档示例值，库存告警按裁定作废）
//   - status       = 1 在架（分组 active 且未软删）/ 2 下架（官方枚举）
//   - update_time  = groups.updated_at 的 Unix 秒
//
// 本单元按「分组」聚合为一个商品（与总单 D6c 描述一致）。成员过滤仅纳入有在售订阅套餐
// 的分组，单纯卡密（无套餐分组）不进入目录。

// SupplyMerchantBalance 是「查询商户信息」固定返回的余额（自研系统无真实商户账户）。
// 官方要求 balance 为大于 0 的整数。出处：总单「官方返回要求」。
const SupplyMerchantBalance int64 = 999999

// supplyPlatformFeatures 是「查询平台信息」固定返回的 features 能力开关。
// 出处：方案 §3 决策 2（支持同步撤单退款；商品订阅通知未实现如实声明 false；
// loss_purchase 按官方可选语义处理故 true）。
var supplyPlatformFeatures = SupplyPlatformFeatures{
	IsSupportOrderRefund:  true,
	IsSupportGoodsNotify:  false,
	IsSupportLossPurchase: true,
}

// SupplyCatalogService 编排四个目录类被调接口。
type SupplyCatalogService struct {
	cfg   SupplyConfigReader
	goods SupplyGoodsSource
}

// NewSupplyCatalogService 构造目录服务。cfg/goods 均必填（fail-closed）。
func NewSupplyCatalogService(cfg SupplyConfigReader, goods SupplyGoodsSource) *SupplyCatalogService {
	return &SupplyCatalogService{cfg: cfg, goods: goods}
}

// PlatformInfo 查询平台信息：返回当前对接的应用概况 AppKey（整数）。
// 官方原话「app_id 为当前对接的应用概况 AppKey」——即 supply app_id
// （如 1783283558647493 量级），不是货源授权 mch_id。无 active 配置时 fail-closed。
func (s *SupplyCatalogService) PlatformInfo(ctx context.Context) (*SupplyPlatformInfo, error) {
	if s == nil || s.cfg == nil {
		return nil, fmt.Errorf("xianguanjia supply catalog: config reader unavailable")
	}
	cfg, err := s.cfg.GetSupplyConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("xianguanjia supply config: %w", err)
	}
	if cfg == nil {
		return nil, ErrNoActiveConfig
	}
	appID, err := parseSupplyInt64(cfg.SupplyAppID, "supply_app_id")
	if err != nil {
		return nil, err
	}
	return &SupplyPlatformInfo{AppID: appID, Features: supplyPlatformFeatures}, nil
}

// MerchantInfo 查询商户信息：返回固定余额（> 0 的整数）。
// 官方 schema：data 仅含 balance，不再返回 mch_id。
func (s *SupplyCatalogService) MerchantInfo(ctx context.Context) (*SupplyMerchantInfo, error) {
	if s == nil || s.cfg == nil {
		return nil, fmt.Errorf("xianguanjia supply catalog: config reader unavailable")
	}
	cfg, err := s.cfg.GetSupplyConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("xianguanjia supply config: %w", err)
	}
	if cfg == nil {
		return nil, ErrNoActiveConfig
	}
	// 官方约束：balance 必须为大于 0 的整数。
	if SupplyMerchantBalance <= 0 {
		return nil, fmt.Errorf("xianguanjia supply merchant balance must be > 0")
	}
	return &SupplyMerchantInfo{Balance: SupplyMerchantBalance}, nil
}

// ListGoods 查询商品列表（分页）。keyword 同时支持商品名模糊与商品编码精准；
// goods_type 仅支持卡密(2)，其它类型按空结果返回（本系统只供货卡密）。
// page_no/page_size 越界归一到安全范围（官方 page_size 上限 100）。
func (s *SupplyCatalogService) ListGoods(ctx context.Context, req ListGoodsRequest) (*ListGoodsResult, error) {
	if s == nil || s.goods == nil {
		return nil, fmt.Errorf("xianguanjia supply catalog: goods source unavailable")
	}
	pageNo, pageSize := req.normalize()
	// 非卡密类型：本系统无对应商品，返回空列表（code==0，不报错）。
	if req.GoodsType != 0 && req.GoodsType != SupplyGoodsTypeKami {
		return &ListGoodsResult{List: []SupplyGoods{}, Count: 0}, nil
	}
	offset := (pageNo - 1) * pageSize
	list, total, err := s.goods.ListGoods(ctx, strings.TrimSpace(req.Keyword), offset, pageSize)
	if err != nil {
		return nil, fmt.Errorf("xianguanjia supply list goods: %w", err)
	}
	if list == nil {
		list = []SupplyGoods{}
	}
	return &ListGoodsResult{List: list, Count: total}, nil
}

// GoodsDetail 查询商品详情。货物不存在时返回 ErrSupplyGoodsNotFound（code=1100），
// handler 据此输出官方错误信封。
func (s *SupplyCatalogService) GoodsDetail(ctx context.Context, goodsNo string) (*SupplyGoods, error) {
	if s == nil || s.goods == nil {
		return nil, fmt.Errorf("xianguanjia supply catalog: goods source unavailable")
	}
	no := strings.TrimSpace(goodsNo)
	if no == "" {
		return nil, ErrSupplyGoodsNotFound
	}
	g, err := s.goods.GetGoods(ctx, no)
	if err != nil {
		return nil, fmt.Errorf("xianguanjia supply get goods: %w", err)
	}
	if g == nil {
		return nil, ErrSupplyGoodsNotFound
	}
	return g, nil
}

// parseSupplyInt64 把配置中的数字字符串解析为 int64（类型纪律：对外整数不得为字符串）。
func parseSupplyInt64(raw, field string) (int64, error) {
	v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("xianguanjia supply config %s is not an integer: %q", field, raw)
	}
	return v, nil
}

// ---- DB 数据源：group 聚合 ----

type supplyGoodsSourceDB struct {
	db *sql.DB
}

// NewSupplyGoodsSource 返回基于 PostgreSQL 的商品聚合数据源。
func NewSupplyGoodsSource(db *sql.DB) *supplyGoodsSourceDB {
	return &supplyGoodsSourceDB{db: db}
}

// supplyGoodsColumns 与 scanSupplyGoods 保持列序一致。
const supplyGoodsSelect = `
	SELECT
		g.id,
		g.name,
		g.status,
		COALESCE((
			SELECT sp.price_cny FROM subscription_plans sp
			WHERE sp.group_id = g.id
			ORDER BY sp.sort_order, sp.id LIMIT 1
		), 0),
		99999,
		g.updated_at
	FROM "groups" g
	WHERE g.deleted_at IS NULL`

// ListGoods 列出分组聚合商品。成员过滤仅纳入有在售订阅套餐的分组（EXISTS subscription_plans）；
// 单纯卡密（无套餐分组）不进入目录。
func (r *supplyGoodsSourceDB) ListGoods(ctx context.Context, keyword string, offset, limit int) ([]SupplyGoods, int, error) {
	if r == nil || r.db == nil {
		return nil, 0, fmt.Errorf("xianguanjia supply goods source unavailable")
	}
	args := []any{}
	filter := `
		AND EXISTS (SELECT 1 FROM subscription_plans sp WHERE sp.group_id = g.id)`
	if keyword != "" {
		kw := strings.TrimSpace(keyword)
		args = append(args, "%"+kw+"%")
		nameCond := fmt.Sprintf("g.name ILIKE $%d", len(args))
		// 商品编码（goods_no = groups.id 十进制串）精准命中：kw 为数字时叠加 OR。
		if _, err := strconv.ParseInt(kw, 10, 64); err == nil {
			args = append(args, kw)
			filter += fmt.Sprintf(" AND (%s OR g.id = $%d)", nameCond, len(args))
		} else {
			filter += fmt.Sprintf(" AND %s", nameCond)
		}
	}

	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (`+supplyGoodsSelect+filter+`) t`, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("xianguanjia supply list goods count: %w", err)
	}

	query := supplyGoodsSelect + filter + ` ORDER BY g.id`
	if limit > 0 {
		args = append(args, limit, offset)
		query += fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)-1, len(args))
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("xianguanjia supply list goods: %w", err)
	}
	defer rows.Close()
	out := make([]SupplyGoods, 0)
	for rows.Next() {
		g, err := scanSupplyGoods(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *g)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("xianguanjia supply iterate goods: %w", err)
	}
	return out, total, nil
}

// GetGoods 按 goods_no（分组 ID）查询单个商品；不存在返回 (nil, nil)。
func (r *supplyGoodsSourceDB) GetGoods(ctx context.Context, goodsNo string) (*SupplyGoods, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("xianguanjia supply goods source unavailable")
	}
	id, err := strconv.ParseInt(goodsNo, 10, 64)
	if err != nil {
		// 非数字 goods_no 不可能是分组 ID：归一为「不存在」。
		return nil, nil
	}
	row := r.db.QueryRowContext(ctx, supplyGoodsSelect+` AND g.id = $1`, id)
	g, err := scanSupplyGoods(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return g, nil
}

// rowScanner 抽象 *sql.Row 与 *sql.Rows 的共同 Scan。
type rowScanner interface {
	Scan(dest ...any) error
}

func scanSupplyGoods(row rowScanner) (*SupplyGoods, error) {
	var (
		id        int64
		name      string
		status    string
		price     float64
		stock     int
		updatedAt time.Time
	)
	if err := row.Scan(&id, &name, &status, &price, &stock, &updatedAt); err != nil {
		return nil, err
	}
	return &SupplyGoods{
		GoodsNo:    strconv.FormatInt(id, 10),
		GoodsType:  SupplyGoodsTypeKami,
		GoodsName:  name,
		Price:      int64(math.Round(price * 100)), // DB 元 → 分
		Stock:      stock,
		Status:     supplyGoodsStatusFromGroupStatus(status),
		UpdateTime: updatedAt.Unix(),
	}, nil
}

// supplyGoodsStatusFromGroupStatus 把分组状态映射为对外商品状态整数（1=在架 2=下架）。
func supplyGoodsStatusFromGroupStatus(status string) int {
	if status == "active" {
		return SupplyGoodsStatusOnSale
	}
	return SupplyGoodsStatusOffSale
}

// ---- 内存数据源（单测 / handler 测试用） ----

type supplyGoodsSourceMem struct {
	goods []SupplyGoods
}

// NewSupplyGoodsSourceMemory 返回内存版商品数据源。传入切片会被复制，避免外部修改。
func NewSupplyGoodsSourceMemory(goods []SupplyGoods) *supplyGoodsSourceMem {
	cp := make([]SupplyGoods, len(goods))
	copy(cp, goods)
	return &supplyGoodsSourceMem{goods: cp}
}

func (s *supplyGoodsSourceMem) ListGoods(ctx context.Context, keyword string, offset, limit int) ([]SupplyGoods, int, error) {
	if s == nil {
		return nil, 0, fmt.Errorf("xianguanjia supply goods source unavailable")
	}
	kw := strings.TrimSpace(keyword)
	matched := make([]SupplyGoods, 0, len(s.goods))
	for _, g := range s.goods {
		if kw == "" {
			matched = append(matched, g)
			continue
		}
		if strings.Contains(g.GoodsName, kw) {
			matched = append(matched, g)
			continue
		}
		// 商品编码精准命中：kw 为数字且等于 goods_no。
		if _, err := strconv.ParseInt(kw, 10, 64); err == nil {
			if g.GoodsNo == kw {
				matched = append(matched, g)
			}
		}
	}
	total := len(matched)
	if offset >= total {
		return []SupplyGoods{}, total, nil
	}
	matched = matched[offset:]
	if limit > 0 && limit < len(matched) {
		matched = matched[:limit]
	}
	return matched, total, nil
}

func (s *supplyGoodsSourceMem) GetGoods(ctx context.Context, goodsNo string) (*SupplyGoods, error) {
	if s == nil {
		return nil, fmt.Errorf("xianguanjia supply goods source unavailable")
	}
	for i := range s.goods {
		if s.goods[i].GoodsNo == goodsNo {
			g := s.goods[i]
			return &g, nil
		}
	}
	return nil, nil
}

// 确保内存实现满足接口（编译期断言）。
var _ SupplyGoodsSource = (*supplyGoodsSourceMem)(nil)
var _ SupplyGoodsSource = (*supplyGoodsSourceDB)(nil)

// ---- 内存配置读取器（单测 / handler 测试用） ----

type supplyConfigReaderMem struct {
	cfg *SupplyConfig
}

// NewSupplyConfigReaderMemory 返回内存版货源配置读取器；cfg 为 nil 模拟「无 active 配置」。
// D6a 提供生产 DB 实现，此实现供单测与 D6b/D6d 复用。
func NewSupplyConfigReaderMemory(cfg *SupplyConfig) SupplyConfigReader {
	return &supplyConfigReaderMem{cfg: cfg}
}

func (r *supplyConfigReaderMem) GetSupplyConfig(ctx context.Context) (*SupplyConfig, error) {
	return r.cfg, nil
}
