/**
 * Admin Latency Diagnostics API
 * 后端实现见 backend/internal/handler/admin/latency_handler.go
 */

import { apiClient } from '../client'

export type LatencyAccessType = '' | 'domain' | 'direct_ip'

export interface LatencyFilters {
  start?: string // RFC3339
  end?: string
  access_type?: LatencyAccessType
  model?: string
}

export interface LatencyThresholds {
  ServerProcessingMs: number
  UpstreamTTFBMs: number
  ResponseDeliveryMs: number
}

export interface LatencyDimensionStats {
  dimension: string
  avg: number
  p50: number
  p95: number
  p99: number
}

export interface LatencyAccessTypeStats {
  access_type: string
  requests: number
  avg_total_ms: number
  p95_total_ms: number
  slow_count: number
  slow_ratio: number
}

export interface LatencyOverview {
  total_requests: number
  slow_count: number
  slow_ratio: number
  bottleneck_breakdown: Record<string, number>
  distribution: LatencyDimensionStats[]
  access_type_breakdown: LatencyAccessTypeStats[]
}

export interface LatencyRegionStats {
  country: string
  region: string
  requests: number
  avg_delivery_ms: number
  p95_delivery_ms: number
  avg_total_ms: number
}

export interface LatencyTrendPoint {
  bucket: string // ISO
  requests: number
  avg_server_ms: number
  avg_upstream_ttfb_ms: number
  avg_upstream_stream_ms: number
  avg_delivery_ms: number
  avg_total_ms: number
}

export interface SlowRequestRow {
  id: number
  created_at: string
  user_id: number
  api_key_id: number
  model: string
  ip_address?: string | null
  access_type?: string | null
  client_country?: string | null
  client_region?: string | null
  server_processing_ms?: number | null
  upstream_ttfb_ms?: number | null
  upstream_stream_ms?: number | null
  response_delivery_ms?: number | null
  total_latency_ms?: number | null
  bottleneck: string
}

function toParams(f?: LatencyFilters): Record<string, string> {
  const p: Record<string, string> = {}
  if (!f) return p
  if (f.start) p.start = f.start
  if (f.end) p.end = f.end
  if (f.access_type) p.access_type = f.access_type
  if (f.model) p.model = f.model
  return p
}

// 注:apiClient 的 response 拦截器(client.ts:93)已经把 {code,message,data} 解包,
// 所以 `data` 直接是 payload。不要再写 `data.data` —— 会拿到 undefined。
export async function getLatencyOverview(f?: LatencyFilters): Promise<{ overview: LatencyOverview; thresholds: LatencyThresholds }> {
  const { data } = await apiClient.get('/admin/latency/overview', { params: toParams(f) })
  return data
}

export async function getLatencyByRegion(
  f?: LatencyFilters,
  limit = 20
): Promise<LatencyRegionStats[]> {
  const { data } = await apiClient.get('/admin/latency/by-region', { params: { ...toParams(f), limit } })
  return data ?? []
}

export async function getLatencyTrend(
  f?: LatencyFilters,
  bucket: 'hour' | 'day' = 'hour'
): Promise<LatencyTrendPoint[]> {
  const { data } = await apiClient.get('/admin/latency/trend', { params: { ...toParams(f), bucket } })
  return data ?? []
}

export interface LatencyUserStats {
  user_id: number
  username: string
  email: string
  requests: number
  avg_server_ms: number
  avg_upstream_ms: number
  avg_delivery_ms: number
  avg_total_ms: number
  p95_total_ms: number
  slow_count: number
  slow_ratio: number
}

export async function getSlowRequests(
  f?: LatencyFilters,
  limit = 100,
  mode: 'top' | 'slow' = 'top'
): Promise<SlowRequestRow[]> {
  const { data } = await apiClient.get('/admin/latency/slow-requests', {
    params: { ...toParams(f), limit, mode }
  })
  return data ?? []
}

export async function getLatencyByUser(
  f?: LatencyFilters,
  limit = 20
): Promise<LatencyUserStats[]> {
  const { data } = await apiClient.get('/admin/latency/by-user', { params: { ...toParams(f), limit } })
  return data ?? []
}
