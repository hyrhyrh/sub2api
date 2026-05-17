package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// latencyRepo 是 service.LatencyRepository 的纯 SQL 实现。
// 单独建一个 struct（不复用 usageLogRepository）是为了把延迟查询的演化和
// 既有 usage_log 写入路径解耦：阈值/聚合 SQL 后续可能频繁调整，但不应该
// 触发 usage_log 主写路径回归。
type latencyRepo struct {
	db *sql.DB
}

// NewLatencyRepository 由 wire 注入。
func NewLatencyRepository(db *sql.DB) service.LatencyRepository {
	return &latencyRepo{db: db}
}

func (r *latencyRepo) whereClause(f service.LatencyAggFilters, baseIdx int) (string, []any) {
	var conds []string
	var args []any
	idx := baseIdx
	conds = append(conds, fmt.Sprintf("created_at >= $%d", idx))
	args = append(args, f.StartTime)
	idx++
	conds = append(conds, fmt.Sprintf("created_at < $%d", idx))
	args = append(args, f.EndTime)
	idx++
	if f.AccessType != "" {
		conds = append(conds, fmt.Sprintf("access_type = $%d", idx))
		args = append(args, f.AccessType)
		idx++
	}
	if f.Model != "" {
		conds = append(conds, fmt.Sprintf("model = $%d", idx))
		args = append(args, f.Model)
		idx++
	}
	return strings.Join(conds, " AND "), args
}

// Overview 一次性返回总览所需的 6 类数据：
//   1. 总请求 / 慢请求 / 主因分布
//   2. 5 段延迟分布（avg/p50/p95/p99）
//   3. domain vs direct_ip 对比
//
// 用单次 query + 多个 CTE 替代 6 次往返，减小 DB 压力。
// 注意：percentile_cont 是聚合中相对昂贵的操作，配合 idx_usage_logs_created_total_latency 走 created_at 索引扫描。
func (r *latencyRepo) Overview(ctx context.Context, t service.LatencyThresholds, f service.LatencyAggFilters) (*service.LatencyOverview, error) {
	where, args := r.whereClause(f, 1)
	thresholdArgs := []any{t.ServerProcessingMs, t.UpstreamTTFBMs, t.ResponseDeliveryMs}
	// where 用 $1..$N, threshold 用紧接的占位符
	tBase := len(args) + 1
	args = append(args, thresholdArgs...)

	query := fmt.Sprintf(`
WITH base AS (
    SELECT
        server_processing_ms,
        upstream_ttfb_ms,
        upstream_stream_ms,
        response_delivery_ms,
        total_latency_ms,
        access_type
    FROM usage_logs
    WHERE %s
),
flagged AS (
    SELECT
        *,
        (
            COALESCE(server_processing_ms, 0) > $%d
            OR COALESCE(upstream_ttfb_ms, 0) > $%d
            OR COALESCE(response_delivery_ms, 0) > $%d
        ) AS is_slow,
        CASE
            WHEN COALESCE(upstream_ttfb_ms, 0) > $%d THEN 'upstream_slow'
            WHEN COALESCE(server_processing_ms, 0) > $%d THEN 'server_internal'
            WHEN COALESCE(response_delivery_ms, 0) > $%d THEN 'delivery_slow'
            ELSE 'normal'
        END AS bottleneck
    FROM base
)
SELECT
    COUNT(*)::bigint AS total_requests,
    COUNT(*) FILTER (WHERE is_slow)::bigint AS slow_count,
    COALESCE(SUM(CASE WHEN bottleneck = 'upstream_slow'   THEN 1 ELSE 0 END), 0)::bigint AS bn_upstream,
    COALESCE(SUM(CASE WHEN bottleneck = 'server_internal' THEN 1 ELSE 0 END), 0)::bigint AS bn_server,
    COALESCE(SUM(CASE WHEN bottleneck = 'delivery_slow'   THEN 1 ELSE 0 END), 0)::bigint AS bn_delivery,
    COALESCE(SUM(CASE WHEN bottleneck = 'normal'          THEN 1 ELSE 0 END), 0)::bigint AS bn_normal,
    COALESCE(AVG(server_processing_ms), 0)::bigint AS avg_server,
    COALESCE(PERCENTILE_CONT(0.5)  WITHIN GROUP (ORDER BY server_processing_ms), 0)::bigint AS p50_server,
    COALESCE(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY server_processing_ms), 0)::bigint AS p95_server,
    COALESCE(PERCENTILE_CONT(0.99) WITHIN GROUP (ORDER BY server_processing_ms), 0)::bigint AS p99_server,
    COALESCE(AVG(upstream_ttfb_ms), 0)::bigint AS avg_ttfb,
    COALESCE(PERCENTILE_CONT(0.5)  WITHIN GROUP (ORDER BY upstream_ttfb_ms), 0)::bigint AS p50_ttfb,
    COALESCE(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY upstream_ttfb_ms), 0)::bigint AS p95_ttfb,
    COALESCE(PERCENTILE_CONT(0.99) WITHIN GROUP (ORDER BY upstream_ttfb_ms), 0)::bigint AS p99_ttfb,
    COALESCE(AVG(upstream_stream_ms), 0)::bigint AS avg_stream,
    COALESCE(PERCENTILE_CONT(0.5)  WITHIN GROUP (ORDER BY upstream_stream_ms), 0)::bigint AS p50_stream,
    COALESCE(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY upstream_stream_ms), 0)::bigint AS p95_stream,
    COALESCE(PERCENTILE_CONT(0.99) WITHIN GROUP (ORDER BY upstream_stream_ms), 0)::bigint AS p99_stream,
    COALESCE(AVG(response_delivery_ms), 0)::bigint AS avg_delivery,
    COALESCE(PERCENTILE_CONT(0.5)  WITHIN GROUP (ORDER BY response_delivery_ms), 0)::bigint AS p50_delivery,
    COALESCE(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY response_delivery_ms), 0)::bigint AS p95_delivery,
    COALESCE(PERCENTILE_CONT(0.99) WITHIN GROUP (ORDER BY response_delivery_ms), 0)::bigint AS p99_delivery,
    COALESCE(AVG(total_latency_ms), 0)::bigint AS avg_total,
    COALESCE(PERCENTILE_CONT(0.5)  WITHIN GROUP (ORDER BY total_latency_ms), 0)::bigint AS p50_total,
    COALESCE(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY total_latency_ms), 0)::bigint AS p95_total,
    COALESCE(PERCENTILE_CONT(0.99) WITHIN GROUP (ORDER BY total_latency_ms), 0)::bigint AS p99_total
FROM flagged
	`, where, tBase, tBase+1, tBase+2, tBase+1, tBase, tBase+2)

	out := &service.LatencyOverview{
		BottleneckBreakdown: map[string]int64{},
	}
	var (
		bnUpstream, bnServer, bnDelivery, bnNormal int64
		avgSrv, p50Srv, p95Srv, p99Srv             int64
		avgTtfb, p50Ttfb, p95Ttfb, p99Ttfb         int64
		avgStr, p50Str, p95Str, p99Str             int64
		avgDel, p50Del, p95Del, p99Del             int64
		avgTot, p50Tot, p95Tot, p99Tot             int64
	)
	if err := r.db.QueryRowContext(ctx, query, args...).Scan(
		&out.TotalRequests, &out.SlowCount,
		&bnUpstream, &bnServer, &bnDelivery, &bnNormal,
		&avgSrv, &p50Srv, &p95Srv, &p99Srv,
		&avgTtfb, &p50Ttfb, &p95Ttfb, &p99Ttfb,
		&avgStr, &p50Str, &p95Str, &p99Str,
		&avgDel, &p50Del, &p95Del, &p99Del,
		&avgTot, &p50Tot, &p95Tot, &p99Tot,
	); err != nil {
		return nil, fmt.Errorf("latency overview query: %w", err)
	}
	out.BottleneckBreakdown[service.BottleneckUpstreamSlow] = bnUpstream
	out.BottleneckBreakdown[service.BottleneckServerInternal] = bnServer
	out.BottleneckBreakdown[service.BottleneckDeliverySlow] = bnDelivery
	out.BottleneckBreakdown[service.BottleneckNormal] = bnNormal

	out.Distribution = []service.LatencyDimensionStats{
		{Dimension: "server_processing", Avg: avgSrv, P50: p50Srv, P95: p95Srv, P99: p99Srv},
		{Dimension: "upstream_ttfb", Avg: avgTtfb, P50: p50Ttfb, P95: p95Ttfb, P99: p99Ttfb},
		{Dimension: "upstream_stream", Avg: avgStr, P50: p50Str, P95: p95Str, P99: p99Str},
		{Dimension: "response_delivery", Avg: avgDel, P50: p50Del, P95: p95Del, P99: p99Del},
		{Dimension: "total_latency", Avg: avgTot, P50: p50Tot, P95: p95Tot, P99: p99Tot},
	}

	atBreakdown, err := r.accessTypeBreakdown(ctx, t, f)
	if err != nil {
		return nil, err
	}
	out.AccessTypeBreakdown = atBreakdown

	return out, nil
}

func (r *latencyRepo) accessTypeBreakdown(ctx context.Context, t service.LatencyThresholds, f service.LatencyAggFilters) ([]service.LatencyAccessTypeStats, error) {
	// 仅按 created_at + 可选 model 过滤；access_type 自身是 GROUP BY 键，
	// 无论 filter 是否传，这里都要列出全部分类。
	conds := []string{"created_at >= $1", "created_at < $2"}
	args := []any{f.StartTime, f.EndTime, t.ServerProcessingMs, t.UpstreamTTFBMs, t.ResponseDeliveryMs}
	if f.Model != "" {
		conds = append(conds, fmt.Sprintf("model = $%d", len(args)+1))
		args = append(args, f.Model)
	}

	query := fmt.Sprintf(`
SELECT
    COALESCE(access_type, '') AS access_type,
    COUNT(*)::bigint AS requests,
    COALESCE(AVG(total_latency_ms), 0)::bigint AS avg_total,
    COALESCE(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY total_latency_ms), 0)::bigint AS p95_total,
    COUNT(*) FILTER (
        WHERE COALESCE(server_processing_ms, 0) > $3
           OR COALESCE(upstream_ttfb_ms, 0) > $4
           OR COALESCE(response_delivery_ms, 0) > $5
    )::bigint AS slow_count
FROM usage_logs
WHERE %s
GROUP BY access_type
ORDER BY requests DESC
	`, strings.Join(conds, " AND "))

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("latency access_type breakdown: %w", err)
	}
	defer rows.Close()

	var out []service.LatencyAccessTypeStats
	for rows.Next() {
		var s service.LatencyAccessTypeStats
		if err := rows.Scan(&s.AccessType, &s.Requests, &s.AvgTotal, &s.P95Total, &s.SlowCount); err != nil {
			return nil, err
		}
		if s.Requests > 0 {
			s.SlowRatio = float64(s.SlowCount) / float64(s.Requests)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ByRegion 按 country/region 聚合，按 avg_delivery 降序。
// 过滤 NULL country（GeoIP 解析失败的）以避免大块"未知"挤掉真实地区。
func (r *latencyRepo) ByRegion(ctx context.Context, f service.LatencyAggFilters, limit int) ([]service.LatencyRegionStats, error) {
	where, args := r.whereClause(f, 1)
	args = append(args, limit)
	limitIdx := len(args)

	query := fmt.Sprintf(`
SELECT
    COALESCE(client_country, '') AS country,
    COALESCE(client_region, '') AS region,
    COUNT(*)::bigint AS requests,
    COALESCE(AVG(response_delivery_ms), 0)::bigint AS avg_delivery,
    COALESCE(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY response_delivery_ms), 0)::bigint AS p95_delivery,
    COALESCE(AVG(total_latency_ms), 0)::bigint AS avg_total
FROM usage_logs
WHERE %s
  AND client_country IS NOT NULL
GROUP BY client_country, client_region
ORDER BY avg_delivery DESC
LIMIT $%d
	`, where, limitIdx)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("latency by-region: %w", err)
	}
	defer rows.Close()

	var out []service.LatencyRegionStats
	for rows.Next() {
		var s service.LatencyRegionStats
		if err := rows.Scan(&s.Country, &s.Region, &s.Requests, &s.AvgDelivery, &s.P95Delivery, &s.AvgTotal); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Trend 按 bucket（hour/day）截断时间，输出 5 段平均延迟时间序列。
func (r *latencyRepo) Trend(ctx context.Context, f service.LatencyAggFilters, bucket string) ([]service.LatencyTrendPoint, error) {
	truncUnit := "hour"
	if bucket == "day" {
		truncUnit = "day"
	}
	where, args := r.whereClause(f, 1)

	query := fmt.Sprintf(`
SELECT
    date_trunc('%s', created_at) AS bucket,
    COUNT(*)::bigint AS requests,
    COALESCE(AVG(server_processing_ms), 0)::bigint AS avg_server,
    COALESCE(AVG(upstream_ttfb_ms), 0)::bigint AS avg_ttfb,
    COALESCE(AVG(upstream_stream_ms), 0)::bigint AS avg_stream,
    COALESCE(AVG(response_delivery_ms), 0)::bigint AS avg_delivery,
    COALESCE(AVG(total_latency_ms), 0)::bigint AS avg_total
FROM usage_logs
WHERE %s
GROUP BY bucket
ORDER BY bucket
	`, truncUnit, where)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("latency trend: %w", err)
	}
	defer rows.Close()

	var out []service.LatencyTrendPoint
	for rows.Next() {
		var p service.LatencyTrendPoint
		if err := rows.Scan(&p.Bucket, &p.Requests, &p.AvgServerMs, &p.AvgUpstreamTTFBMs, &p.AvgUpstreamStream, &p.AvgDelivery, &p.AvgTotal); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ByUser 按 user_id 聚合，返回每个用户的请求数与延迟统计。
// JOIN users 取 username/email 直接返回，避免前端二次查询。
// 按 requests 降序（活跃用户优先）。
//
// where 子句这里手写（不复用 whereClause），因为 JOIN users 后 created_at
// 在两个表都存在，必须用 ul.created_at 显式限定避免歧义。
func (r *latencyRepo) ByUser(ctx context.Context, t service.LatencyThresholds, f service.LatencyAggFilters, limit int) ([]service.LatencyUserStats, error) {
	conds := []string{"ul.created_at >= $1", "ul.created_at < $2"}
	args := []any{f.StartTime, f.EndTime}
	if f.AccessType != "" {
		conds = append(conds, fmt.Sprintf("ul.access_type = $%d", len(args)+1))
		args = append(args, f.AccessType)
	}
	if f.Model != "" {
		conds = append(conds, fmt.Sprintf("ul.model = $%d", len(args)+1))
		args = append(args, f.Model)
	}
	thresholdBase := len(args) + 1
	args = append(args, t.ServerProcessingMs, t.UpstreamTTFBMs, t.ResponseDeliveryMs, limit)
	limitIdx := len(args)

	query := fmt.Sprintf(`
SELECT
    ul.user_id,
    COALESCE(u.username, '') AS username,
    COALESCE(u.email, '') AS email,
    COUNT(*)::bigint AS requests,
    COALESCE(AVG(server_processing_ms), 0)::bigint AS avg_server,
    COALESCE(AVG(COALESCE(upstream_ttfb_ms, 0) + COALESCE(upstream_stream_ms, 0)), 0)::bigint AS avg_upstream,
    COALESCE(AVG(response_delivery_ms), 0)::bigint AS avg_delivery,
    COALESCE(AVG(total_latency_ms), 0)::bigint AS avg_total,
    COALESCE(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY total_latency_ms), 0)::bigint AS p95_total,
    COUNT(*) FILTER (
        WHERE COALESCE(server_processing_ms, 0) > $%d
           OR COALESCE(upstream_ttfb_ms, 0) > $%d
           OR COALESCE(response_delivery_ms, 0) > $%d
    )::bigint AS slow_count
FROM usage_logs ul
LEFT JOIN users u ON u.id = ul.user_id
WHERE %s
GROUP BY ul.user_id, u.username, u.email
ORDER BY requests DESC
LIMIT $%d
	`, thresholdBase, thresholdBase+1, thresholdBase+2, strings.Join(conds, " AND "), limitIdx)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("latency by-user: %w", err)
	}
	defer rows.Close()

	var out []service.LatencyUserStats
	for rows.Next() {
		var s service.LatencyUserStats
		if err := rows.Scan(&s.UserID, &s.Username, &s.Email, &s.Requests, &s.AvgServerMs, &s.AvgUpstreamMs, &s.AvgDeliveryMs, &s.AvgTotalMs, &s.P95TotalMs, &s.SlowCount); err != nil {
			return nil, err
		}
		if s.Requests > 0 {
			s.SlowRatio = float64(s.SlowCount) / float64(s.Requests)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// TopRequests 取耗时最长的 N 条请求（不按阈值过滤）。
// 用于代替 SlowRequests 在数据量小、阈值未触发时仍能展示代表性请求。
func (r *latencyRepo) TopRequests(ctx context.Context, f service.LatencyAggFilters, limit int) ([]service.SlowRequestRow, error) {
	where, args := r.whereClause(f, 1)
	args = append(args, limit)
	limitIdx := len(args)

	query := fmt.Sprintf(`
SELECT
    id, created_at, user_id, api_key_id, model,
    ip_address, access_type, client_country, client_region,
    server_processing_ms, upstream_ttfb_ms, upstream_stream_ms, response_delivery_ms, total_latency_ms
FROM usage_logs
WHERE %s
  AND total_latency_ms IS NOT NULL
ORDER BY total_latency_ms DESC NULLS LAST
LIMIT $%d
	`, where, limitIdx)

	return r.scanRequestRows(ctx, query, args)
}

func (r *latencyRepo) scanRequestRows(ctx context.Context, query string, args []any) ([]service.SlowRequestRow, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("latency request rows: %w", err)
	}
	defer rows.Close()
	var out []service.SlowRequestRow
	for rows.Next() {
		var (
			row                                             service.SlowRequestRow
			ipAddress, accessType, country, region          sql.NullString
			serverMs, ttfbMs, streamMs, deliveryMs, totalMs sql.NullInt64
		)
		if err := rows.Scan(
			&row.ID, &row.CreatedAt, &row.UserID, &row.APIKeyID, &row.Model,
			&ipAddress, &accessType, &country, &region,
			&serverMs, &ttfbMs, &streamMs, &deliveryMs, &totalMs,
		); err != nil {
			return nil, err
		}
		row.IPAddress = nullStringPtr(ipAddress)
		row.AccessType = nullStringPtr(accessType)
		row.ClientCountry = nullStringPtr(country)
		row.ClientRegion = nullStringPtr(region)
		row.ServerProcessingMs = nullInt64ToIntPtr(serverMs)
		row.UpstreamTTFBMs = nullInt64ToIntPtr(ttfbMs)
		row.UpstreamStreamMs = nullInt64ToIntPtr(streamMs)
		row.ResponseDeliveryMs = nullInt64ToIntPtr(deliveryMs)
		row.TotalLatencyMs = nullInt64ToIntPtr(totalMs)
		out = append(out, row)
	}
	return out, rows.Err()
}

// SlowRequests 慢请求列表，按 created_at 倒序。
// 不展开 user/api_key/group 详情（避免大 JOIN）；前端有需要可以再点详情。
func (r *latencyRepo) SlowRequests(ctx context.Context, t service.LatencyThresholds, f service.LatencyAggFilters, limit int) ([]service.SlowRequestRow, error) {
	where, args := r.whereClause(f, 1)
	thresholdBase := len(args) + 1
	args = append(args, t.ServerProcessingMs, t.UpstreamTTFBMs, t.ResponseDeliveryMs, limit)
	limitIdx := len(args)

	query := fmt.Sprintf(`
SELECT
    id, created_at, user_id, api_key_id, model,
    ip_address, access_type, client_country, client_region,
    server_processing_ms, upstream_ttfb_ms, upstream_stream_ms, response_delivery_ms, total_latency_ms
FROM usage_logs
WHERE %s
  AND (
        COALESCE(server_processing_ms, 0) > $%d
     OR COALESCE(upstream_ttfb_ms, 0) > $%d
     OR COALESCE(response_delivery_ms, 0) > $%d
  )
ORDER BY created_at DESC
LIMIT $%d
	`, where, thresholdBase, thresholdBase+1, thresholdBase+2, limitIdx)

	return r.scanRequestRows(ctx, query, args)
}

func nullStringPtr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

func nullInt64ToIntPtr(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int64)
	return &n
}
