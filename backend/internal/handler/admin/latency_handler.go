package admin

import (
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// LatencyHandler 提供 /api/v1/admin/latency/* 系列只读接口。
// 所有 endpoint 走 admin 中间件鉴权，不在此 handler 内重复检查。
type LatencyHandler struct {
	svc *service.LatencyService
}

// NewLatencyHandler 由 wire 注入。
func NewLatencyHandler(svc *service.LatencyService) *LatencyHandler {
	return &LatencyHandler{svc: svc}
}

// parseLatencyTimeRange 从 query 解析时间窗。
// 缺省取最近 24 小时（plan/latency-tracking-final.md 默认窗口）。
//
// 接受两种参数命名：
//   - start / end : ISO8601 (RFC3339) 时间，与 admin/ops 系列对齐
//   - 缺失任意一端时回退默认窗口
func parseLatencyTimeRange(c *gin.Context) (time.Time, time.Time) {
	now := time.Now()
	defaultStart := now.Add(-24 * time.Hour)
	start := defaultStart
	end := now
	if v := c.Query("start"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			start = t
		}
	}
	if v := c.Query("end"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			end = t
		}
	}
	if !end.After(start) {
		end = start.Add(time.Hour)
	}
	return start, end
}

func parseLatencyFilters(c *gin.Context) service.LatencyAggFilters {
	start, end := parseLatencyTimeRange(c)
	at := c.Query("access_type")
	if at != "domain" && at != "direct_ip" {
		at = ""
	}
	return service.LatencyAggFilters{
		StartTime:  start,
		EndTime:    end,
		AccessType: at,
		Model:      c.Query("model"),
	}
}

// Overview GET /api/v1/admin/latency/overview
func (h *LatencyHandler) Overview(c *gin.Context) {
	out, err := h.svc.Overview(c.Request.Context(), parseLatencyFilters(c))
	if err != nil {
		response.Error(c, 500, "Failed to load latency overview: "+err.Error())
		return
	}
	thresholds := h.svc.Thresholds()
	response.Success(c, gin.H{
		"overview":   out,
		"thresholds": thresholds,
	})
}

// ByRegion GET /api/v1/admin/latency/by-region
func (h *LatencyHandler) ByRegion(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	out, err := h.svc.ByRegion(c.Request.Context(), parseLatencyFilters(c), limit)
	if err != nil {
		response.Error(c, 500, "Failed to load latency by-region: "+err.Error())
		return
	}
	response.Success(c, out)
}

// Trend GET /api/v1/admin/latency/trend?bucket=hour
func (h *LatencyHandler) Trend(c *gin.Context) {
	bucket := c.DefaultQuery("bucket", "hour")
	out, err := h.svc.Trend(c.Request.Context(), parseLatencyFilters(c), bucket)
	if err != nil {
		response.Error(c, 500, "Failed to load latency trend: "+err.Error())
		return
	}
	response.Success(c, out)
}

// SlowRequests GET /api/v1/admin/latency/slow-requests?limit=100&mode=top|slow
//
// mode 默认 "top"：按 total_latency_ms 倒序取 Top N（无视阈值）。
// mode=slow：仅返回超阈值的"违规"请求，按 created_at 倒序。
func (h *LatencyHandler) SlowRequests(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	mode := c.DefaultQuery("mode", "top")
	out, err := h.svc.SlowRequests(c.Request.Context(), parseLatencyFilters(c), mode, limit)
	if err != nil {
		response.Error(c, 500, "Failed to load latency slow-requests: "+err.Error())
		return
	}
	response.Success(c, out)
}

// ByUser GET /api/v1/admin/latency/by-user?limit=20
// 按 user_id 聚合的延迟排行（请求数倒序）。
func (h *LatencyHandler) ByUser(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	out, err := h.svc.ByUser(c.Request.Context(), parseLatencyFilters(c), limit)
	if err != nil {
		response.Error(c, 500, "Failed to load latency by-user: "+err.Error())
		return
	}
	response.Success(c, out)
}
