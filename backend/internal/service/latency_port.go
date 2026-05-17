package service

import (
	"context"
	"time"
)

// LatencyUsageFields 是所有 RecordUsage 系列 Input 共用的延迟字段集合。
// 直接嵌入到 RecordUsageInput / OpenAIRecordUsageInput / RecordUsageLongContextInput，
// 避免每种 input 各列一份 8 字段、各自漏掉。
//
// 字段语义见 plan/latency-tracking-final.md。指针表示"未采集到"。
type LatencyUsageFields struct {
	ServerProcessingMs *int
	UpstreamTTFBMs     *int
	UpstreamStreamMs   *int
	ResponseDeliveryMs *int
	TotalLatencyMs     *int
	AccessType         string
	ClientCountry      string
	ClientRegion       string
}

// LatencyThresholds 由配置或环境变量驱动，控制慢请求 / 瓶颈判定。
// 这里集中定义而不是散落在 SQL 里，是为了让 admin 页面可以一键调整阈值并
// 立刻让历史数据展示出新的 "is_slow / bottleneck" 标签（无须重跑数据）。
type LatencyThresholds struct {
	ServerProcessingMs int
	UpstreamTTFBMs     int
	ResponseDeliveryMs int
}

// LatencyAggFilters 是聚合查询的公共过滤器。
type LatencyAggFilters struct {
	StartTime  time.Time
	EndTime    time.Time
	AccessType string // "" / "domain" / "direct_ip"
	Model      string // "" 为不过滤;精确匹配
}

// LatencyOverview 聚合结果（管理员页面"整体概览"卡片）。
type LatencyOverview struct {
	TotalRequests       int64                    `json:"total_requests"`
	SlowCount           int64                    `json:"slow_count"`
	SlowRatio           float64                  `json:"slow_ratio"` // [0, 1]
	BottleneckBreakdown map[string]int64         `json:"bottleneck_breakdown"`
	Distribution        []LatencyDimensionStats  `json:"distribution"`
	AccessTypeBreakdown []LatencyAccessTypeStats `json:"access_type_breakdown"`
}

// LatencyDimensionStats 单段延迟的统计分布。
type LatencyDimensionStats struct {
	Dimension string `json:"dimension"` // server_processing / upstream_ttfb / ...
	Avg       int64  `json:"avg"`
	P50       int64  `json:"p50"`
	P95       int64  `json:"p95"`
	P99       int64  `json:"p99"`
}

// LatencyAccessTypeStats 入口类型聚合结果。
type LatencyAccessTypeStats struct {
	AccessType    string  `json:"access_type"`
	Requests      int64   `json:"requests"`
	AvgTotal      int64   `json:"avg_total_ms"`
	P95Total      int64   `json:"p95_total_ms"`
	SlowCount     int64   `json:"slow_count"`
	SlowRatio     float64 `json:"slow_ratio"`
}

// LatencyRegionStats 按地区聚合。
type LatencyRegionStats struct {
	Country     string `json:"country"`
	Region      string `json:"region"`
	Requests    int64  `json:"requests"`
	AvgDelivery int64  `json:"avg_delivery_ms"`
	P95Delivery int64  `json:"p95_delivery_ms"`
	AvgTotal    int64  `json:"avg_total_ms"`
}

// LatencyTrendPoint 趋势图单点（按 hour/day bucket）。
type LatencyTrendPoint struct {
	Bucket             time.Time `json:"bucket"`
	Requests           int64     `json:"requests"`
	AvgServerMs        int64     `json:"avg_server_ms"`
	AvgUpstreamTTFBMs  int64     `json:"avg_upstream_ttfb_ms"`
	AvgUpstreamStream  int64     `json:"avg_upstream_stream_ms"`
	AvgDelivery        int64     `json:"avg_delivery_ms"`
	AvgTotal           int64     `json:"avg_total_ms"`
}

// SlowRequestRow 慢请求列表条目（瓶颈在 service 层算）。
type SlowRequestRow struct {
	ID                 int64     `json:"id"`
	CreatedAt          time.Time `json:"created_at"`
	UserID             int64     `json:"user_id"`
	APIKeyID           int64     `json:"api_key_id"`
	Model              string    `json:"model"`
	IPAddress          *string   `json:"ip_address"`
	AccessType         *string   `json:"access_type"`
	ClientCountry      *string   `json:"client_country"`
	ClientRegion       *string   `json:"client_region"`
	ServerProcessingMs *int      `json:"server_processing_ms"`
	UpstreamTTFBMs     *int      `json:"upstream_ttfb_ms"`
	UpstreamStreamMs   *int      `json:"upstream_stream_ms"`
	ResponseDeliveryMs *int      `json:"response_delivery_ms"`
	TotalLatencyMs     *int      `json:"total_latency_ms"`
	Bottleneck         string    `json:"bottleneck"` // 由 service 层填充
}

// LatencyUserStats 按 user_id 聚合的延迟排行行。
// 包含 username 以便前端直接渲染（避免再 JOIN）。
type LatencyUserStats struct {
	UserID         int64   `json:"user_id"`
	Username       string  `json:"username"`
	Email          string  `json:"email"`
	Requests       int64   `json:"requests"`
	AvgServerMs    int64   `json:"avg_server_ms"`
	AvgUpstreamMs  int64   `json:"avg_upstream_ms"` // ttfb + stream
	AvgDeliveryMs  int64   `json:"avg_delivery_ms"`
	AvgTotalMs     int64   `json:"avg_total_ms"`
	P95TotalMs     int64   `json:"p95_total_ms"`
	SlowCount      int64   `json:"slow_count"`
	SlowRatio      float64 `json:"slow_ratio"`
}

// LatencyRepository 是 admin/latency API 直接查 usage_logs 的端口。
// 实现位于 repository/usage_log_latency_repo.go。
type LatencyRepository interface {
	Overview(ctx context.Context, t LatencyThresholds, f LatencyAggFilters) (*LatencyOverview, error)
	ByRegion(ctx context.Context, f LatencyAggFilters, limit int) ([]LatencyRegionStats, error)
	ByUser(ctx context.Context, t LatencyThresholds, f LatencyAggFilters, limit int) ([]LatencyUserStats, error)
	Trend(ctx context.Context, f LatencyAggFilters, bucket string) ([]LatencyTrendPoint, error)
	TopRequests(ctx context.Context, f LatencyAggFilters, limit int) ([]SlowRequestRow, error)
	SlowRequests(ctx context.Context, t LatencyThresholds, f LatencyAggFilters, limit int) ([]SlowRequestRow, error)
}
