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

// D4d: 闲管家 admin 卡种管理 + 批量推仓 API。

export interface XianguanjiaKindCreateInput {
  name: string
  /** 可选分类 ID；不传或 0 表示不指定 */
  category_id?: number
}

export interface XianguanjiaCardPair {
  card_no: string
  card_pwd: string
}

export interface XianguanjiaPushResult {
  total: number
  succeeded: number
  failed: number
  failures?: string[]
}

/** 建卡种，返回新 kind_id */
export async function createPoolKind(input: XianguanjiaKindCreateInput): Promise<{ kind_id: number }> {
  const { data } = await apiClient.post<{ kind_id: number }>('/admin/xianguanjia/pool/kind', input)
  return data
}

/** 返回当前 kind_id（未设置时为 0） */
export async function getPoolKind(): Promise<{ kind_id: number }> {
  const { data } = await apiClient.get<{ kind_id: number }>('/admin/xianguanjia/pool/kind')
  return data
}

/** 批量推仓：往指定 kind_id 的卡仓推入卡密 */
export async function pushPoolCards(kindId: number, cards: XianguanjiaCardPair[]): Promise<XianguanjiaPushResult> {
  const { data } = await apiClient.post<XianguanjiaPushResult>('/admin/xianguanjia/pool/push', {
    kind_id: kindId,
    cards
  })
  return data
}

export default { getConfig, putConfig, healthCheck, createPoolKind, getPoolKind, pushPoolCards }
