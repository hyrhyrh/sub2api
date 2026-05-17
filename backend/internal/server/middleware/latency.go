// Package middleware - latency tracker
package middleware

import (
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	// CtxKeyLatencyTracker 在 gin.Context 中保存 *LatencyTracker 指针的 key。
	CtxKeyLatencyTracker = "latency_tracker"

	// AccessTypeDomain 域名进站。
	AccessTypeDomain = "domain"
	// AccessTypeDirectIP IP 直连进站（nginx default_server 兜底）。
	AccessTypeDirectIP = "direct_ip"
)

// LatencyTracker 在请求生命周期内累积 5 个时间点，由各层埋点调用 MarkXxx() 填充。
//
// 字段含义见 plan/latency-tracking-final.md：
//
//	T1 ServerReceivedAt    中间件入口
//	T2 UpstreamSentAt      开始向上游发请求
//	T3 UpstreamFirstByteAt 收到上游首字节
//	T4 UpstreamCompletedAt 上游响应读完
//	T5 ResponseCompletedAt c.Next() 返回（最外层填）
type LatencyTracker struct {
	ServerReceivedAt    time.Time
	UpstreamSentAt      time.Time
	UpstreamFirstByteAt time.Time
	UpstreamCompletedAt time.Time
	ResponseCompletedAt time.Time

	AccessType string

	upstreamSentSet      atomic.Bool
	upstreamFirstByteSet atomic.Bool
	upstreamCompletedSet atomic.Bool
	responseCompletedSet atomic.Bool
}

// LatencyMetrics 是 5 段派生延迟，nil 表示该段无法计算（埋点未触发或异常）。
type LatencyMetrics struct {
	ServerProcessingMs *int
	UpstreamTTFBMs     *int
	UpstreamStreamMs   *int
	ResponseDeliveryMs *int
	TotalLatencyMs     *int
}

// Latency 注册 Tracker 到 gin.Context。注册位置应在最外层（RequestLogger 之后、其他业务中间件之前）。
//
// 入口类型从 nginx 注入的 X-Access-Type header 读取，仅信任 127.0.0.1/::1 来源
// (nginx 与 sub2api 同机)，避免外部客户端伪造。
func Latency() gin.HandlerFunc {
	return func(c *gin.Context) {
		t := &LatencyTracker{
			ServerReceivedAt: time.Now(),
			AccessType:       AccessTypeDomain,
		}
		if isLocalProxyClient(c.RemoteIP()) {
			if v := c.GetHeader("X-Access-Type"); v == AccessTypeDomain || v == AccessTypeDirectIP {
				t.AccessType = v
			}
		}
		c.Set(CtxKeyLatencyTracker, t)
		c.Next()
		t.MarkResponseCompleted()
	}
}

func isLocalProxyClient(ip string) bool {
	return ip == "127.0.0.1" || ip == "::1"
}

// GetLatencyTracker 从 gin.Context 取出 Tracker，未注册时返回 nil。
// 调用者必须做 nil 检查后再调 MarkXxx() / DerivedMetrics()。
func GetLatencyTracker(c *gin.Context) *LatencyTracker {
	if c == nil {
		return nil
	}
	if v, ok := c.Get(CtxKeyLatencyTracker); ok {
		if t, ok := v.(*LatencyTracker); ok {
			return t
		}
	}
	return nil
}

// MarkUpstreamSent 标记 T2，幂等（仅首次生效）。
func (t *LatencyTracker) MarkUpstreamSent() {
	if t == nil {
		return
	}
	if !t.upstreamSentSet.Swap(true) {
		t.UpstreamSentAt = time.Now()
	}
}

// MarkUpstreamFirstByte 标记 T3，幂等。
// 流式场景下应在收到首个有效 chunk 时调用。
func (t *LatencyTracker) MarkUpstreamFirstByte() {
	if t == nil {
		return
	}
	if !t.upstreamFirstByteSet.Swap(true) {
		t.UpstreamFirstByteAt = time.Now()
	}
}

// SetUpstreamTTFBMs 用 ms 偏移推回 T3 时刻（基准为 T2）。
// 当 service 层已经度量了 first-token 延迟（例如 ForwardResult.FirstTokenMs）时，
// 在 handler 调用此方法即可避免在每个 stream handler 里再插桩。
// 若 T2 未设置则 no-op。幂等：仅首次生效。
func (t *LatencyTracker) SetUpstreamTTFBMs(ms int) {
	if t == nil || ms < 0 {
		return
	}
	if t.UpstreamSentAt.IsZero() {
		return
	}
	if !t.upstreamFirstByteSet.Swap(true) {
		t.UpstreamFirstByteAt = t.UpstreamSentAt.Add(time.Duration(ms) * time.Millisecond)
	}
}

// MarkUpstreamCompleted 标记 T4，幂等（最后一次值为准；当前实现取首次以避免乱序）。
func (t *LatencyTracker) MarkUpstreamCompleted() {
	if t == nil {
		return
	}
	if !t.upstreamCompletedSet.Swap(true) {
		t.UpstreamCompletedAt = time.Now()
	}
}

// MarkResponseCompleted 标记 T5，幂等。
// 中间件 c.Next() 返回后会自动调用，但 handler 内部的 RecordUsage 发生在 c.Next() 之内，
// 那时 T5 仍是零值，会导致 total_latency_ms/response_delivery_ms 为 NULL。
// 因此在 CollectLatencyUsageFields 之前应主动 Mark 一次（与最终中间件 Mark 相比误差仅几微秒）。
func (t *LatencyTracker) MarkResponseCompleted() {
	if t == nil {
		return
	}
	if !t.responseCompletedSet.Swap(true) {
		t.ResponseCompletedAt = time.Now()
	}
}

// 各段延迟的合理上限（毫秒），超出视为异常并丢弃（返回 nil），避免污染聚合统计。
const (
	maxServerProcessingMs = 60_000      // 1 min
	maxUpstreamTTFBMs     = 5 * 60_000  // 5 min
	maxUpstreamStreamMs   = 30 * 60_000 // 30 min
	maxResponseDeliveryMs = 5 * 60_000  // 5 min
	maxTotalLatencyMs     = 30 * 60_000 // 30 min
)

// DerivedMetrics 计算 5 段延迟。任何一段两端时间戳不全则该段为 nil。
//
// 非流式响应没有 first-token 概念，T3 不会被埋点。此时 fallback 把 T3 视为 T4：
// upstream_ttfb_ms = 整个上游耗时，upstream_stream_ms = 0。
func (t *LatencyTracker) DerivedMetrics() LatencyMetrics {
	m := LatencyMetrics{}
	if t == nil {
		return m
	}
	if !t.ServerReceivedAt.IsZero() && !t.UpstreamSentAt.IsZero() {
		m.ServerProcessingMs = diffMsCapped(t.ServerReceivedAt, t.UpstreamSentAt, maxServerProcessingMs)
	}
	firstByteAt := t.UpstreamFirstByteAt
	if firstByteAt.IsZero() && !t.UpstreamCompletedAt.IsZero() {
		firstByteAt = t.UpstreamCompletedAt
	}
	if !t.UpstreamSentAt.IsZero() && !firstByteAt.IsZero() {
		m.UpstreamTTFBMs = diffMsCapped(t.UpstreamSentAt, firstByteAt, maxUpstreamTTFBMs)
	}
	if !firstByteAt.IsZero() && !t.UpstreamCompletedAt.IsZero() {
		m.UpstreamStreamMs = diffMsCapped(firstByteAt, t.UpstreamCompletedAt, maxUpstreamStreamMs)
	}
	if !t.UpstreamCompletedAt.IsZero() && !t.ResponseCompletedAt.IsZero() {
		m.ResponseDeliveryMs = diffMsCapped(t.UpstreamCompletedAt, t.ResponseCompletedAt, maxResponseDeliveryMs)
	}
	if !t.ServerReceivedAt.IsZero() && !t.ResponseCompletedAt.IsZero() {
		m.TotalLatencyMs = diffMsCapped(t.ServerReceivedAt, t.ResponseCompletedAt, maxTotalLatencyMs)
	}
	return m
}

func diffMsCapped(a, b time.Time, capMs int) *int {
	ms := int(b.Sub(a).Milliseconds())
	if ms < 0 {
		ms = 0
	}
	if ms > capMs {
		return nil
	}
	return &ms
}
