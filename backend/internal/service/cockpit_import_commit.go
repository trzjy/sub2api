// cockpit_import_commit.go —— cockpit 导入 service 层提交入口（方案 v15 §1.6，B1b）。
//
// 本文件只定义提交编排契约与薄入口：构造载荷 → 仓储原子写入 → 计数汇总。
// 平台映射器（entry→payload 转换）因 §1.2 门控为 pending，不在本单（B1c 之外、enabled 为空）。
// 当前 enabled 平台为空，无 entry 能转成载荷，天然不写入；映射器上线后在本服务内
// 做 entry→payload 转换再调用仓储，接口边界保持不变。

package service

import "context"

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

// CockpitImportCommitService 封装 cockpit 导入提交编排（§1.6）。
type CockpitImportCommitService struct {
	repo CockpitImportCommitRepository
}

// NewCockpitImportCommitService 构造提交服务。
func NewCockpitImportCommitService(repo CockpitImportCommitRepository) *CockpitImportCommitService {
	return &CockpitImportCommitService{repo: repo}
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
