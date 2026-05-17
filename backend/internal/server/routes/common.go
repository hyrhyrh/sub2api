package routes

import (
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/gin-gonic/gin"
)

// RegisterCommonRoutes 注册通用路由（健康检查、状态等）
func RegisterCommonRoutes(r *gin.Engine) {
	// 健康检查
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// /api/ping —— 客户端测速端点（公开，无需鉴权）。
	// 前端连续多次调用，用 (响应到达时间 - 请求发起时间) 估算客户端→sub2api 的 RTT。
	// 响应体非常小（< 200 字节）避免传输时间稀释 RTT 估计。
	// 设 no-store 避免 CDN/浏览器缓存导致后续 ping 走本地缓存。
	pingHandler := func(c *gin.Context) {
		c.Header("Cache-Control", "no-store, no-cache, must-revalidate")
		c.Header("Pragma", "no-cache")
		c.JSON(http.StatusOK, gin.H{
			"server_time_ms": time.Now().UnixMilli(),
			"client_ip":      ip.GetClientIP(c),
		})
	}
	r.GET("/api/ping", pingHandler)
	r.HEAD("/api/ping", pingHandler)

	// Claude Code 遥测日志（忽略，直接返回200）
	r.POST("/api/event_logging/batch", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	// Setup status endpoint (always returns needs_setup: false in normal mode)
	// This is used by the frontend to detect when the service has restarted after setup
	r.GET("/setup/status", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"code": 0,
			"data": gin.H{
				"needs_setup": false,
				"step":        "completed",
			},
		})
	})
}
