package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

const (
	defaultServerProcessingThresholdMs = 100
	defaultUpstreamTTFBThresholdMs     = 3000
	defaultResponseDeliveryThresholdMs = 1000

	latencyCacheTTL = 30 * time.Second

	// 瓶颈分类
	BottleneckNormal         = "normal"
	BottleneckServerInternal = "server_internal"
	BottleneckUpstreamSlow   = "upstream_slow"
	BottleneckDeliverySlow   = "delivery_slow"
)

// LatencyService 是 admin/latency API 的业务层入口。
// 它负责：
//   - 从环境变量解析阈值（运维可不重启进程改 cfg 后通过 reload-thresholds）
//   - 调 LatencyRepository 拿原始统计
//   - 计算每行的瓶颈分类
//   - Redis 缓存聚合结果 30s（避免管理员页面频繁刷新打爆 DB）
type LatencyService struct {
	repo  LatencyRepository
	cache *redis.Client
}

// NewLatencyService 由 wire 注入。
func NewLatencyService(repo LatencyRepository, cache *redis.Client) *LatencyService {
	return &LatencyService{repo: repo, cache: cache}
}

// Thresholds 从环境变量读阈值。每次调用都读一次以支持热生效（无需重启）。
// 单次调用开销 3 次 Getenv + 3 次 Atoi，可忽略。
func (s *LatencyService) Thresholds() LatencyThresholds {
	return LatencyThresholds{
		ServerProcessingMs: envIntDefault("LATENCY_THRESHOLD_SERVER_PROCESSING_MS", defaultServerProcessingThresholdMs),
		UpstreamTTFBMs:     envIntDefault("LATENCY_THRESHOLD_UPSTREAM_TTFB_MS", defaultUpstreamTTFBThresholdMs),
		ResponseDeliveryMs: envIntDefault("LATENCY_THRESHOLD_RESPONSE_DELIVERY_MS", defaultResponseDeliveryThresholdMs),
	}
}

func envIntDefault(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// ClassifyBottleneck 应用 plan/latency-tracking-final.md 的优先级判定：
//   1. upstream_ttfb_ms 超阈值 → upstream_slow（上游问题）
//   2. server_processing_ms 超阈值 → server_internal（服务器内部）
//   3. response_delivery_ms 超阈值 → delivery_slow（推测客户端网络）
//   4. 否则 → normal
//
// 优先级反映"对运维更有价值的归因"：上游慢可联系上游/切账号；
// server_internal 是我们的责任；delivery_slow 只是推测，所以放最后。
func ClassifyBottleneck(t LatencyThresholds, server, ttfb, delivery *int) string {
	if ttfb != nil && *ttfb > t.UpstreamTTFBMs {
		return BottleneckUpstreamSlow
	}
	if server != nil && *server > t.ServerProcessingMs {
		return BottleneckServerInternal
	}
	if delivery != nil && *delivery > t.ResponseDeliveryMs {
		return BottleneckDeliverySlow
	}
	return BottleneckNormal
}

// Overview 拉总览数据。带 30s Redis 缓存。
func (s *LatencyService) Overview(ctx context.Context, f LatencyAggFilters) (*LatencyOverview, error) {
	t := s.Thresholds()
	cacheKey := s.cacheKey("overview", f, t)
	if cached := s.cacheGet(ctx, cacheKey); cached != nil {
		var out LatencyOverview
		if err := json.Unmarshal(cached, &out); err == nil {
			return &out, nil
		}
	}
	out, err := s.repo.Overview(ctx, t, f)
	if err != nil {
		return nil, err
	}
	if out.TotalRequests > 0 {
		out.SlowRatio = float64(out.SlowCount) / float64(out.TotalRequests)
	}
	if out.BottleneckBreakdown == nil {
		out.BottleneckBreakdown = map[string]int64{}
	}
	s.cacheSet(ctx, cacheKey, out)
	return out, nil
}

// ByRegion 按 country/region 聚合，按 avg_delivery 降序。limit ≤ 0 时用 20 默认。
func (s *LatencyService) ByRegion(ctx context.Context, f LatencyAggFilters, limit int) ([]LatencyRegionStats, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	cacheKey := s.cacheKey(fmt.Sprintf("by-region-%d", limit), f, LatencyThresholds{})
	if cached := s.cacheGet(ctx, cacheKey); cached != nil {
		var out []LatencyRegionStats
		if err := json.Unmarshal(cached, &out); err == nil {
			return out, nil
		}
	}
	out, err := s.repo.ByRegion(ctx, f, limit)
	if err != nil {
		return nil, err
	}
	s.cacheSet(ctx, cacheKey, out)
	return out, nil
}

// Trend 趋势数据。bucket: "hour" | "day"。
func (s *LatencyService) Trend(ctx context.Context, f LatencyAggFilters, bucket string) ([]LatencyTrendPoint, error) {
	if bucket != "hour" && bucket != "day" {
		bucket = "hour"
	}
	cacheKey := s.cacheKey("trend-"+bucket, f, LatencyThresholds{})
	if cached := s.cacheGet(ctx, cacheKey); cached != nil {
		var out []LatencyTrendPoint
		if err := json.Unmarshal(cached, &out); err == nil {
			return out, nil
		}
	}
	out, err := s.repo.Trend(ctx, f, bucket)
	if err != nil {
		return nil, err
	}
	s.cacheSet(ctx, cacheKey, out)
	return out, nil
}

// SlowRequests 慢请求或 Top-N 兜底。
//
// 默认 mode=top：直接按 total_latency_ms 倒序取 Top N，无视阈值——
// 这样小流量场景（用户视图常见）也能看到代表性请求；瓶颈分类仍由 service 层计算。
// mode=slow：保留原"超过阈值才出列"的逻辑（运维筛 SLA 违规时用）。
func (s *LatencyService) SlowRequests(ctx context.Context, f LatencyAggFilters, mode string, limit int) ([]SlowRequestRow, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	t := s.Thresholds()
	var (
		rows []SlowRequestRow
		err  error
	)
	if mode == "slow" {
		rows, err = s.repo.SlowRequests(ctx, t, f, limit)
	} else {
		rows, err = s.repo.TopRequests(ctx, f, limit)
	}
	if err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].Bottleneck = ClassifyBottleneck(t, rows[i].ServerProcessingMs, rows[i].UpstreamTTFBMs, rows[i].ResponseDeliveryMs)
	}
	return rows, nil
}

// ByUser 按 user_id 聚合的延迟排行。带 30s 缓存。
func (s *LatencyService) ByUser(ctx context.Context, f LatencyAggFilters, limit int) ([]LatencyUserStats, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	t := s.Thresholds()
	cacheKey := s.cacheKey(fmt.Sprintf("by-user-%d", limit), f, t)
	if cached := s.cacheGet(ctx, cacheKey); cached != nil {
		var out []LatencyUserStats
		if err := json.Unmarshal(cached, &out); err == nil {
			return out, nil
		}
	}
	out, err := s.repo.ByUser(ctx, t, f, limit)
	if err != nil {
		return nil, err
	}
	s.cacheSet(ctx, cacheKey, out)
	return out, nil
}

func (s *LatencyService) cacheKey(prefix string, f LatencyAggFilters, t LatencyThresholds) string {
	return fmt.Sprintf("admin:latency:%s:%d:%d:%s:%d:%d:%d",
		prefix,
		f.StartTime.Unix(), f.EndTime.Unix(), f.AccessType,
		t.ServerProcessingMs, t.UpstreamTTFBMs, t.ResponseDeliveryMs,
	)
}

func (s *LatencyService) cacheGet(ctx context.Context, key string) []byte {
	if s.cache == nil {
		return nil
	}
	v, err := s.cache.Get(ctx, key).Bytes()
	if err != nil {
		return nil
	}
	return v
}

func (s *LatencyService) cacheSet(ctx context.Context, key string, value any) {
	if s.cache == nil {
		return
	}
	data, err := json.Marshal(value)
	if err != nil {
		logger.L().Warn("latency cache marshal failed", zap.String("key", key), zap.Error(err))
		return
	}
	if err := s.cache.Set(ctx, key, data, latencyCacheTTL).Err(); err != nil {
		logger.L().Warn("latency cache set failed", zap.String("key", key), zap.Error(err))
	}
}
