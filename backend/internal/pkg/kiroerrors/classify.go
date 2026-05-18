// Package kiroerrors 提供 Kiro 上游响应的粗粒度错误二分类,以及 Retry-After 解析。
//
// 设计动机:
//   - 当前 sub2api 收到 4xx/5xx 一律往切号 / 冷却走,
//     但 400 上下文超限 / 422 请求格式错切到下一个账号也救不了,
//     白白浪费配额且误标账号 fail_count。
//   - kiro-gateway (Python) 的 account_errors.py 已有成熟的 FATAL / RECOVERABLE 二分,
//     这里照搬其规则到 Go,并叠加 sub2api 现有的 kiro_error_classifier 子分类(本包不替代,
//     做更粗粒度的"调度决策"层)。
//   - airgate-core 的 OutcomeKind (AccountRateLimited / AccountDead / UpstreamTransient / Success
//     / ClientError) 在概念上对应本包的 RateLimited / AccountDead / Recoverable / / Fatal。
//
// 调用方:kiro_runtime.go 在 executeKiroUpstream 收到 status >= 400 时调 Classify,
// 根据 ErrorClass 决定:
//   - Fatal:直接返客户端 4xx,不切号、不冷却(只算客户端错误)
//   - AccountDead:走 markKiroSuspended / SetTempUnschedulable(已有逻辑)
//   - RateLimited:走 P0 已有的 cooldown 流程 + P1 #5 family 冷却
//   - Recoverable:触发 service 层 / handler 层 failover
//   - Unknown:保持现有兜底逻辑(按 Recoverable 处理,避免破坏现有兜底)
package kiroerrors

import (
	"strings"

	"github.com/tidwall/gjson"
)

// ErrorClass 上游响应的粗粒度调度决策类别。
type ErrorClass int

const (
	// ClassUnknown 无法识别。调用方应保持现有兜底逻辑(通常按 Recoverable 处理)。
	ClassUnknown ErrorClass = iota
	// ClassFatal 客户端错误或请求本身有问题(上下文超限、请求格式错),
	// 切到下一个账号也救不了 — 直接返客户端,不切号、不冷却账号。
	ClassFatal
	// ClassRecoverable 上游瞬时错误(5xx、连接超时),切到下一个账号可能成功。
	// 触发 handler / service 层 failover,但不长期冷却账号(账号本身没问题)。
	ClassRecoverable
	// ClassAccountDead 账号永久不可用(401 凭证失效 / 403 disabled/suspended),
	// 不应再选中该账号,直到管理员干预。
	ClassAccountDead
	// ClassRateLimited 上游临时限流(429,402+MONTHLY_REQUEST_COUNT)。
	// 走 cooldown + family 冷却 + 切换下一个账号。
	ClassRateLimited
)

// String 用于日志输出。
func (c ErrorClass) String() string {
	switch c {
	case ClassFatal:
		return "fatal"
	case ClassRecoverable:
		return "recoverable"
	case ClassAccountDead:
		return "account_dead"
	case ClassRateLimited:
		return "rate_limited"
	default:
		return "unknown"
	}
}

// Classify 根据 HTTP 状态码 + body 内容,返回二分类 + 简要原因。
//
// 优先级(从上到下):
//  1. 5xx + 网关 timeout (504) → Recoverable (上游抖动,切号试)
//  2. 401 → AccountDead (凭证失效,管理员介入)
//  3. 403 + body 含 disabled/deactivated/suspended → AccountDead
//  4. 403 + 其他 → Recoverable (可能是 sticky session 失效,切号试)
//  5. 402 + body 含 MONTHLY_REQUEST_COUNT → RateLimited (月度配额耗尽走特殊冷却)
//  6. 402 + 其他 → Recoverable (可能是临时计费问题)
//  7. 429 → RateLimited
//  8. 408 → Recoverable (请求超时,切号)
//  9. 400 + body 含 context length 关键词 → Fatal (上下文超限,切号没用)
// 10. 400 + 其他 → 走 sub-classification (现有 kiro_error_classifier 处理细节)
// 11. 422 → Fatal (请求格式错)
// 12. 其他 4xx → Fatal (客户端错误)
// 13. 其他 → Unknown
func Classify(statusCode int, body []byte) (ErrorClass, string) {
	bodyStr := string(body)
	lower := strings.ToLower(strings.TrimSpace(bodyStr))

	switch {
	case statusCode >= 500 && statusCode < 600:
		return ClassRecoverable, "upstream_5xx"

	case statusCode == 401:
		return ClassAccountDead, "unauthorized"

	case statusCode == 403:
		if isAccountDeadBody(lower) {
			return ClassAccountDead, "forbidden_account_dead"
		}
		return ClassRecoverable, "forbidden"

	case statusCode == 402:
		if isMonthlyRequestCountBody(bodyStr) {
			return ClassRateLimited, "monthly_request_count"
		}
		return ClassRecoverable, "payment_required"

	case statusCode == 429:
		return ClassRateLimited, "too_many_requests"

	case statusCode == 408:
		return ClassRecoverable, "request_timeout"

	case statusCode == 400:
		if isContextLengthBody(lower) {
			return ClassFatal, "context_length_exceeded"
		}
		// 其他 400 留给现有 kiro_error_classifier 做细分判断;
		// 在调度决策层,部分 400 (invalid_model_id 等) 实际上是 Recoverable
		// (切到支持该 model 的账号可能成功),所以这里返 Unknown 让调用方走现有逻辑。
		return ClassUnknown, "bad_request_unclassified"

	case statusCode == 422:
		return ClassFatal, "unprocessable_entity"

	case statusCode >= 400 && statusCode < 500:
		// 405/406/411/413/415/418... 都是请求本身问题。
		return ClassFatal, "client_error"

	default:
		return ClassUnknown, "unhandled"
	}
}

// isContextLengthBody 检测 400 响应是否是上下文超限。
// Kiro 后端实际错误片段(已观察到):
//   - "CONTENT_LENGTH_EXCEEDS_THRESHOLD" / "Content length exceeds"
//   - "EXPECTED_LENGTH_LIMIT"
//   - "context length exceeded" / "max tokens exceeded"
//   - "input is too long"
func isContextLengthBody(lower string) bool {
	if lower == "" {
		return false
	}
	return strings.Contains(lower, "content_length_exceeds_threshold") ||
		strings.Contains(lower, "expected_length_limit") ||
		strings.Contains(lower, "content length exceeds") ||
		strings.Contains(lower, "context length exceeded") ||
		strings.Contains(lower, "context_length_exceeded") ||
		strings.Contains(lower, "max tokens exceeded") ||
		strings.Contains(lower, "max_tokens_exceeded") ||
		strings.Contains(lower, "input is too long") ||
		strings.Contains(lower, "prompt is too long") ||
		strings.Contains(lower, "exceeds the maximum")
}

// isAccountDeadBody 检测 403 响应是否表示账号永久禁用(suspended/deactivated/disabled)。
func isAccountDeadBody(lower string) bool {
	if lower == "" {
		return false
	}
	return strings.Contains(lower, "disabled") ||
		strings.Contains(lower, "deactivated") ||
		strings.Contains(lower, "suspended") ||
		strings.Contains(lower, "account closed") ||
		strings.Contains(lower, "terminated")
}

// isMonthlyRequestCountBody 检测 402 响应是否是 Kiro 月度配额耗尽。
// 与现有 looksLikeKiroMonthlyRequestCountError 逻辑一致,但抽出来避免跨包依赖。
func isMonthlyRequestCountBody(body string) bool {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return false
	}
	if strings.Contains(trimmed, "MONTHLY_REQUEST_COUNT") {
		return true
	}
	if !gjson.Valid(trimmed) {
		return false
	}
	return gjson.Get(trimmed, "reason").String() == "MONTHLY_REQUEST_COUNT" ||
		gjson.Get(trimmed, "error.reason").String() == "MONTHLY_REQUEST_COUNT"
}
