# credmigrate — 凭证存量迁移命令

执行凭证静态加密的第三阶段（A3-E3，见 `docs/security-ban-prevention-plan.md`）：
把 `accounts.credentials` 中明文形态的敏感子键批量加密为 `enc:v1:` 密文
（AES-256-GCM），并为全部账号回填 `credentials_mac` / `credentials_api_key_mac`
指纹列（迁移 246 增加，供 SQL 级 CAS 守卫与 Ollama 用量分组使用）。

## 前置条件

1. 后端已升级到包含 E2（写路径加密 + 指纹列）的版本；
2. 环境已配置 `CRED_ENCRYPTION_KEY`（32 字节 base64，`openssl rand -base64 32` 生成），
   **与网关进程使用同一密钥**；未配置时本命令拒绝执行。

## 用法

```bash
# 先 dry-run 看待迁移规模（只读，不写库）
go run ./cmd/credmigrate -dry-run

# 执行迁移（可安全重跑：已加密且指纹一致的行自动跳过）
go run ./cmd/credmigrate -batch 500
```

## 行为与注意

- 幂等：重复执行第二次起全部报告"无变化"；
- 迁移期间网关可继续运行（E2 写路径产出相同形态，读取双形态兼容）；
  建议低峰执行；
- 已配置 `CRED_ENCRYPTION_KEY_OLD` 的部署：全部行重写完成后（重新跑本命令
  至无变化），旧密钥即可按方案移除；
- 完成后按方案抽验业务：账号 CRUD、token 刷新、网关转发。
