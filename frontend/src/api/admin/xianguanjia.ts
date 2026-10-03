import { apiClient } from '@/api/client'

// D3: 闲管家（开放平台 ERP 方向）admin 凭证配置 API。
// AppSecret 永不回显：GET 只返回是否已设置 + 末 4 位。

export interface XianguanjiaConfig {
  configured: boolean
  base_url: string
  app_id: string
  app_secret_set: boolean
  app_secret_tail: string
  push_url: string
  status: string
  health_status: string
}

export interface XianguanjiaConfigPutInput {
  app_id: string
  /** 留空表示保留已存密钥（仅首次保存必填） */
  app_secret?: string
  push_url?: string
  base_url?: string
}

export async function getConfig(): Promise<XianguanjiaConfig> {
  const { data } = await apiClient.get<XianguanjiaConfig>('/admin/xianguanjia/config')
  return data
}

export async function putConfig(input: XianguanjiaConfigPutInput): Promise<void> {
  await apiClient.put('/admin/xianguanjia/config', input)
}

export async function healthCheck(): Promise<{ health_status: string }> {
  const { data } = await apiClient.post<{ health_status: string }>('/admin/xianguanjia/health-check')
  return data
}

export default { getConfig, putConfig, healthCheck }
