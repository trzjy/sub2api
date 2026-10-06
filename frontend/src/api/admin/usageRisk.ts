/**
 * Admin Usage Risk Analysis API.
 *
 * Front-end only. Field names follow the backend contract verbatim and must
 * not be renamed. The API client interceptor unwraps `{ code, message, data }`
 * into `data`, so every function returns `data` directly.
 *
 * Endpoints:
 *   GET  /admin/usage-risk/reports
 *   GET  /admin/usage-risk/reports/{report_id}
 *   POST /admin/usage-risk/reports/{report_id}/status
 *   GET  /admin/usage-risk/run-status
 */

import { apiClient } from '../client'
export type { RiskSummary } from '@/types'

// ==================== Types (backend contract, do not rename) ====================

export type RiskLevel = 'low' | 'medium' | 'high' | 'critical'
export type ReportStatus = 'open' | 'acknowledged' | 'dismissed' | 'resolved'

export interface RuleHit {
  rule: string
  scope: 'user' | 'group'
  detail: string
  points: number
}

export interface Report {
  report_id: number
  user_id: number
  username: string
  group_id: number
  group_name: string
  report_date: string
  score: number
  level: RiskLevel
  rule_hits: RuleHit[]
  status: ReportStatus
  invalidated_at: string | null
}

/**
 * Evidence JSON blob. Field names follow the frozen backend contract (R5-1)
 * verbatim and must not be renamed. Every field is optional; the renderer
 * tolerates any missing/partial section without throwing.
 *
 * Frozen contract (top-level keys):
 *   active_hours:    number
 *   ip_top:          [{ ip: string, requests: number, distinct_users: number }]
 *   ua_top:          [{ ua: string, count: number }]
 *   key_distribution: [{ key: string, count: number }]
 *   heatmap:         { "<hour>": number }   (hour strings, numeric order on render)
 *   r2_context:      { peer_count: number, filter: string, algorithm: string, p95: number }
 *   truncated:       boolean
 *   original_bytes:  number
 */
export interface Evidence {
  active_hours?: number
  heatmap?: Record<string, number>
  ip_top?: Array<{ ip: string; requests: number; distinct_users: number }>
  ua_top?: Array<{ ua: string; count: number }>
  key_distribution?: Array<{ key: string; count: number }>
  r2_context?: {
    peer_count?: number
    filter?: string
    algorithm?: string
    p95?: number
  }
  truncated?: boolean
  original_bytes?: number
  [key: string]: unknown
}

export interface ReportDetail extends Report {
  evidence: Evidence
  policy_version: string
}

export interface UsageRiskReportListResponse {
  items: Report[]
  total: number
  min_score: number
}

export interface RunStatusResponse {
  status: 'idle' | 'running' | 'completed' | 'partial' | 'error'
  window_end: string | null
  consecutive_partials: number
  failed_batches: number
  history_covered: boolean
  // E：覆盖翻转后待重评 R1 标记——true 时"历史已覆盖"不成立（翻转后全窗口重评进行中）。
  // 后端 json tag 带 omitempty，false 时字段省略，故为可选。
  r1_reeval_pending?: boolean
  // Backend returns a descriptive cursor string (e.g. cursor date / window end),
  // not a numeric percentage. Render as-is.
  recon_progress: string
  // Present when status === 'error': the reason the analysis run failed/closed.
  error?: string
}

export type UpdateReportStatusPayload = 'acknowledged' | 'dismissed' | 'resolved'

// ==================== Query / Payload ====================

export interface ListReportsParams {
  page?: number
  page_size?: number
  date?: string
  level?: RiskLevel
  user_id?: number
  group_id?: number
  rule?: string
  include_low?: boolean
}

// ==================== API Functions ====================

/**
 * List usage-risk reports (paginated, filterable, with the current entry
 * threshold `min_score` returned by the backend).
 */
export async function listReports(params: ListReportsParams): Promise<UsageRiskReportListResponse> {
  const { data } = await apiClient.get<UsageRiskReportListResponse>('/admin/usage-risk/reports', {
    params
  })
  return data
}

/**
 * Get a single report with its evidence payload.
 */
export async function getReport(id: number): Promise<ReportDetail> {
  const { data } = await apiClient.get<ReportDetail>(`/admin/usage-risk/reports/${id}`)
  return data
}

/**
 * Transition a report's status (state machine enforced server-side).
 * Returns the new status string echoed by the backend.
 */
export async function updateReportStatus(
  id: number,
  status: UpdateReportStatusPayload
): Promise<string> {
  const { data } = await apiClient.post<string>(`/admin/usage-risk/reports/${id}/status`, { status })
  return data
}

/**
 * Get the asynchronous analysis run status (freshness / coverage).
 */
export async function getRunStatus(): Promise<RunStatusResponse> {
  const { data } = await apiClient.get<RunStatusResponse>('/admin/usage-risk/run-status')
  return data
}

/**
 * Get all usage_risk_* setting keys with effective values (defaults + stored overrides).
 */
export async function getUsageRiskSettings(): Promise<Record<string, string>> {
  const { data } = await apiClient.get<{ settings: Record<string, string> }>('/admin/usage-risk/settings')
  return data.settings
}

/**
 * Persist usage risk settings (partial update; domain validation is server-side).
 */
export async function updateUsageRiskSettings(values: Record<string, string>): Promise<void> {
  await apiClient.put('/admin/usage-risk/settings', values)
}

export const adminUsageRiskAPI = {
  listReports,
  getReport,
  updateReportStatus,
  getRunStatus,
  getUsageRiskSettings,
  updateUsageRiskSettings
}

export default adminUsageRiskAPI
