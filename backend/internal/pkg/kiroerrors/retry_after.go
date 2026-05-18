// retry_after.go: 解析上游 Retry-After 头(以及 body 内嵌的 retry_after 字段),
// 并对结果应用最小 / 最大兜底,避免上游返回 15-50ms 这种瞬时值导致刚解锁又撞墙,
// 或异常的超长值占用账号过久。

package kiroerrors

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// ParseRetryAfter 优先从 response.Header 解析 Retry-After。
//
// 解析顺序:
//  1. headers.Get("Retry-After") 秒数(整数 / 浮点)
//  2. headers.Get("Retry-After") HTTP-date 格式 (RFC 1123 / 1123Z)
//  3. headers.Get("X-RateLimit-Reset") Unix 时间戳(秒)
//  4. headers.Get("Retry-After-Ms") 毫秒(airgate-core 等自定义头)
//
// 返回 (duration, true) 表示成功解析;(0, false) 表示无 / 解析失败。
// 调用方应配合 ApplyRetryAfterBounds 应用 min/max 兜底。
func ParseRetryAfter(headers http.Header) (time.Duration, bool) {
	if headers == nil {
		return 0, false
	}

	// 1. Retry-After: 整数/浮点秒
	if raw := strings.TrimSpace(headers.Get("Retry-After")); raw != "" {
		if d, ok := parseRetryAfterValue(raw); ok {
			return d, true
		}
	}

	// 2. X-RateLimit-Reset: Unix 时间戳
	if raw := strings.TrimSpace(headers.Get("X-RateLimit-Reset")); raw != "" {
		if ts, err := strconv.ParseInt(raw, 10, 64); err == nil && ts > 0 {
			until := time.Unix(ts, 0)
			if d := time.Until(until); d > 0 {
				return d, true
			}
		}
	}

	// 3. Retry-After-Ms: 自定义毫秒头
	if raw := strings.TrimSpace(headers.Get("Retry-After-Ms")); raw != "" {
		if ms, err := strconv.ParseInt(raw, 10, 64); err == nil && ms > 0 {
			return time.Duration(ms) * time.Millisecond, true
		}
	}

	return 0, false
}

// ParseRetryAfterFromBody 在 Retry-After 头缺失 / 解析失败时,
// 尝试从 JSON body 提取 retry_after_seconds / retry_after_ms 字段。
// 部分上游(包括 Anthropic / OpenAI variant)会在 error JSON 里给出建议。
func ParseRetryAfterFromBody(body []byte) (time.Duration, bool) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return 0, false
	}
	// 顺序尝试常见字段名;返回第一个有效值。
	candidates := []struct {
		path string
		unit time.Duration
	}{
		{"retry_after_ms", time.Millisecond},
		{"retry_after_seconds", time.Second},
		{"retry_after", time.Second},
		{"error.retry_after_ms", time.Millisecond},
		{"error.retry_after_seconds", time.Second},
		{"error.retry_after", time.Second},
	}
	for _, c := range candidates {
		v := gjson.GetBytes(body, c.path)
		if !v.Exists() {
			continue
		}
		switch v.Type {
		case gjson.Number:
			n := v.Float()
			if n > 0 {
				return time.Duration(n * float64(c.unit)), true
			}
		case gjson.String:
			if d, ok := parseRetryAfterValue(v.Str); ok {
				return d, true
			}
		default:
			// 其他类型(null/object)忽略
		}
	}
	return 0, false
}

// parseRetryAfterValue 解析单个 Retry-After 字符串值。
// 接受:整数秒、浮点秒、HTTP-date(RFC 1123 / 1123Z / RFC850 / ANSIC)。
// 返回 (duration, true) 表示成功;duration 可能 = 0(表示"立即重试"),保留语义。
func parseRetryAfterValue(raw string) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}

	// 整数秒
	if secs, err := strconv.Atoi(raw); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}

	// 浮点秒(部分实现会给 "1.5" 这种)
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		if f < 0 {
			return 0, false
		}
		return time.Duration(f * float64(time.Second)), true
	}

	// HTTP-date: 依次尝试常见格式
	layouts := []string{
		http.TimeFormat, // RFC 1123 with GMT
		time.RFC1123,    // RFC 1123
		time.RFC1123Z,   // RFC 1123 with numeric zone
		time.RFC850,
		time.ANSIC,
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			d := time.Until(t)
			if d < 0 {
				// 已经过去:返 0 让调用方走默认兜底
				return 0, true
			}
			return d, true
		}
	}

	return 0, false
}

// ApplyRetryAfterBounds 把解析出的 Retry-After 应用最小/最大兜底。
//
//   - d <= 0: 调用方应使用自己的默认 cooldown,不应该再走 min/max 兜底;
//     这里仍返回 minMS(让调用方可以选 max(default, ApplyBounds(0,...)) 模式)。
//   - d < minMS: 提到 minMS,防止 15-50ms 瞬时值。
//   - d > maxS: 截断到 maxS,防异常超长值。
//
// minMS 默认 200, maxS 默认 7 天,由 caller 从 env 传入。
func ApplyRetryAfterBounds(d time.Duration, minMS int64, maxS int64) time.Duration {
	if minMS <= 0 {
		minMS = 200
	}
	if maxS <= 0 {
		maxS = 7 * 24 * 60 * 60
	}
	minDur := time.Duration(minMS) * time.Millisecond
	maxDur := time.Duration(maxS) * time.Second

	if d < minDur {
		d = minDur
	}
	if d > maxDur {
		d = maxDur
	}
	return d
}
