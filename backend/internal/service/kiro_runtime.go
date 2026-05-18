package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand"
	"net/http"
	"strings"
	"time"

	kiropkg "github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/Wei-Shaw/sub2api/internal/pkg/kiroerrors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

type kiroEndpointConfig struct {
	URL       string
	AmzTarget string
	Name      string
}

const kiroInvalidModelTempUnschedDuration = time.Minute

const (
	kiroRetryBaseDelay = 200 * time.Millisecond
	kiroRetryMaxDelay  = 2 * time.Second
)

var kiroRetrySleep = sleepWithContext

func kiroRetryBackoffDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	delay := kiroRetryBaseDelay * time.Duration(1<<attempt)
	if delay > kiroRetryMaxDelay {
		delay = kiroRetryMaxDelay
	}
	jitterMax := delay / 4
	if jitterMax <= 0 {
		return delay
	}
	return delay + time.Duration(mathrand.Int63n(int64(jitterMax)+1))
}

func sleepKiroRetry(ctx context.Context, attempt int) error {
	return kiroRetrySleep(ctx, kiroRetryBackoffDelay(attempt))
}

func (s *GatewayService) forwardKiroMessages(ctx context.Context, c *gin.Context, account *Account, parsed *ParsedRequest, startTime time.Time) (*ForwardResult, error) {
	if account == nil || parsed == nil {
		return nil, fmt.Errorf("kiro forward: missing account or request")
	}

	originalModel := parsed.Model
	mappedModel := originalModel
	if next := account.GetMappedModel(originalModel); next != "" {
		mappedModel = next
	}
	body := parsed.Body
	if mappedModel != originalModel {
		body = s.replaceModelInBody(body, mappedModel)
	}
	logger.L().Debug("gateway forward_kiro_messages: request prepared",
		zap.Int64("account_id", account.ID),
		zap.String("auth_method", strings.TrimSpace(account.GetCredential("auth_method"))),
		zap.String("requested_model", originalModel),
		zap.String("mapped_model", mappedModel),
		zap.Bool("has_profile_arn", strings.TrimSpace(account.GetCredential("profile_arn")) != ""),
	)

	if s.shouldEmulateWebSearch(ctx, account, parsed.GroupID, body) {
		parsedForEmulation := *parsed
		parsedForEmulation.Body = body
		parsedForEmulation.Model = mappedModel
		return s.handleWebSearchEmulation(ctx, c, account, &parsedForEmulation)
	}

	if parsed.Stream {
		resp, _, err := s.openKiroAnthropicStreamResponse(ctx, account, parsed, body, mappedModel, originalModel, c.Request.Header, parsed.Group)
		if err != nil {
			var failoverErr *UpstreamFailoverError
			if errors.As(err, &failoverErr) {
				appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
					Platform:           account.Platform,
					AccountID:          account.ID,
					AccountName:        account.Name,
					UpstreamStatusCode: failoverErr.StatusCode,
					Kind:               "failover",
					Message:            sanitizeUpstreamErrorMessage(err.Error()),
				})
				return nil, failoverErr
			}
			safeErr := sanitizeUpstreamErrorMessage(err.Error())
			setOpsUpstreamError(c, 0, safeErr, "")
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: 0,
				Kind:               "request_error",
				Message:            safeErr,
			})
			c.JSON(http.StatusBadGateway, gin.H{
				"type": "error",
				"error": gin.H{
					"type":    "upstream_error",
					"message": "Upstream request failed",
				},
			})
			return nil, fmt.Errorf("kiro upstream request failed: %s", safeErr)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode >= 400 {
			return nil, s.handleKiroHTTPError(ctx, resp, c, account, mappedModel, body)
		}
		upstreamModel := normalizeModelNameForPricing(kiropkg.MapModel(mappedModel))
		streamResult, err := s.handleStreamingResponse(ctx, resp, c, account, startTime, originalModel, mappedModel, false)
		if err != nil {
			return nil, err
		}
		if streamResult.usage == nil {
			streamResult.usage = &ClaudeUsage{}
		}
		return &ForwardResult{
			RequestID:        resp.Header.Get("x-request-id"),
			Usage:            *streamResult.usage,
			Model:            originalModel,
			UpstreamModel:    upstreamModel,
			Stream:           true,
			Duration:         time.Since(startTime),
			FirstTokenMs:     streamResult.firstTokenMs,
			ClientDisconnect: streamResult.clientDisconnect,
		}, nil
	}

	token, tokenType, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	if tokenType != "oauth" {
		return nil, fmt.Errorf("kiro requires oauth token, got %s", tokenType)
	}
	if isOnlyWebSearchToolInBody(body) {
		webSearchResult, webSearchErr := s.executeKiroWebSearch(ctx, account, parsed.Group, body, mappedModel, originalModel, token, c.Request.Header)
		switch {
		case errors.Is(webSearchErr, errKiroWebSearchFallback):
		case webSearchErr == nil:
			upstreamModel := normalizeModelNameForPricing(kiropkg.MapModel(mappedModel))
			c.Header("Content-Type", "application/json")
			if webSearchResult.RequestID != "" {
				c.Header("x-request-id", webSearchResult.RequestID)
			}
			c.Data(http.StatusOK, "application/json", webSearchResult.ResponseBody)
			return &ForwardResult{
				RequestID:     webSearchResult.RequestID,
				Usage:         webSearchResult.Usage,
				Model:         originalModel,
				UpstreamModel: upstreamModel,
				Stream:        false,
				Duration:      time.Since(startTime),
			}, nil
		default:
			var httpErr *kiroWebSearchHTTPError
			if errors.As(webSearchErr, &httpErr) && httpErr.Response != nil {
				return nil, s.handleKiroHTTPError(ctx, httpErr.Response, c, account, mappedModel, body)
			}
			var failoverErr *UpstreamFailoverError
			if errors.As(webSearchErr, &failoverErr) {
				appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
					Platform:           account.Platform,
					AccountID:          account.ID,
					AccountName:        account.Name,
					UpstreamStatusCode: failoverErr.StatusCode,
					Kind:               "failover",
					Message:            sanitizeUpstreamErrorMessage(webSearchErr.Error()),
				})
				return nil, failoverErr
			}
			safeErr := sanitizeUpstreamErrorMessage(webSearchErr.Error())
			c.JSON(http.StatusBadGateway, gin.H{
				"type": "error",
				"error": gin.H{
					"type":    "upstream_error",
					"message": "Upstream request failed",
				},
			})
			return nil, fmt.Errorf("kiro upstream request failed: %s", safeErr)
		}
	}

	inputTokens := estimateKiroInputTokens(body)
	// P0 #3: service 层 429 内 failover —— wrapper 内部最多重试 KIRO_SERVICE_RETRY_MAX 次,
	// 每次切到新账号都会重新 GetAccessToken。返回的 effectiveAccount 可能与传入的 account 不同。
	effectiveAccount, resp, requestCtx, err := s.executeKiroUpstreamWithServiceFailover(ctx, account, parsed, body, mappedModel, originalModel, token, c.Request.Header)
	if effectiveAccount != nil {
		account = effectiveAccount
	}
	if err != nil {
		var failoverErr *UpstreamFailoverError
		if errors.As(err, &failoverErr) {
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: failoverErr.StatusCode,
				Kind:               "failover",
				Message:            sanitizeUpstreamErrorMessage(err.Error()),
			})
			return nil, failoverErr
		}
		safeErr := sanitizeUpstreamErrorMessage(err.Error())
		c.JSON(http.StatusBadGateway, gin.H{
			"type": "error",
			"error": gin.H{
				"type":    "upstream_error",
				"message": "Upstream request failed",
			},
		})
		return nil, fmt.Errorf("kiro upstream request failed: %s", safeErr)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return nil, s.handleKiroHTTPError(ctx, resp, c, account, mappedModel, body)
	}

	cacheUsage := s.buildKiroCacheEmulationUsage(account, parsed.Group, body, mappedModel, inputTokens)
	requestCtx.CacheEmulationUsage = cacheUsage.toKiroUsage()
	parseResult, err := kiropkg.ParseNonStreamingEventStreamWithContext(resp.Body, mappedModel, requestCtx)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"type": "error",
			"error": gin.H{
				"type":    "upstream_error",
				"message": "Failed to parse Kiro upstream response",
			},
		})
		return nil, err
	}

	c.Header("Content-Type", "application/json")
	if requestID := resp.Header.Get("x-request-id"); requestID != "" {
		c.Header("x-request-id", requestID)
	}
	c.Data(http.StatusOK, "application/json", parseResult.ResponseBody)

	upstreamModel := normalizeModelNameForPricing(kiropkg.MapModel(mappedModel))

	return &ForwardResult{
		RequestID:     resp.Header.Get("x-request-id"),
		Usage:         kiroUsageToClaude(parseResult.Usage, inputTokens),
		Model:         originalModel,
		UpstreamModel: upstreamModel,
		Stream:        false,
		Duration:      time.Since(startTime),
	}, nil
}

// openKiroAnthropicStreamResponse 打开 Kiro upstream 的流式响应。
// 注意:parsed 用于 P0 #3 service 层 429 内 failover 时调用 SelectAccountForModelWithExclusions;
// parsed=nil 时退化为不切号(只针对当前 account 发起请求,完全兼容旧调用方)。
// group 字段仍保留为入参,在 P0 改动前由调用方独立计算,这里继续从 parsed 取或用入参兜底。
func (s *GatewayService) openKiroAnthropicStreamResponse(ctx context.Context, account *Account, parsed *ParsedRequest, anthropicBody []byte, mappedModel, requestModel string, headers http.Header, group *Group) (*http.Response, int, error) {
	token, tokenType, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, 0, err
	}
	if tokenType != "oauth" {
		return nil, 0, fmt.Errorf("kiro requires oauth token, got %s", tokenType)
	}

	inputTokens := estimateKiroInputTokens(anthropicBody)
	if isOnlyWebSearchToolInBody(anthropicBody) {
		cacheUsage := s.buildKiroCacheEmulationUsage(account, group, anthropicBody, mappedModel, inputTokens)
		pr, pw := io.Pipe()
		headers := make(http.Header)
		headers.Set("Content-Type", "text/event-stream")
		go func() {
			streamErr := s.streamKiroWebSearchAsAnthropic(ctx, account, anthropicBody, mappedModel, requestModel, token, inputTokens, headers, pw, cacheUsage)
			if streamErr != nil {
				_ = pw.CloseWithError(streamErr)
				return
			}
			_ = pw.Close()
		}()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     headers,
			Body:       pr,
		}, inputTokens, nil
	}

	// P0 #3: service 层 429 内 failover —— wrapper 内部最多 KIRO_SERVICE_RETRY_MAX 次切号 retry。
	// 流式分支同样适用:此处尚未开始向客户端写 SSE,切号是安全的(切完后才开始 stream)。
	effectiveAccount, resp, requestCtx, err := s.executeKiroUpstreamWithServiceFailover(ctx, account, parsed, anthropicBody, mappedModel, requestModel, token, headers)
	if effectiveAccount != nil {
		account = effectiveAccount
	}
	if err != nil {
		var failoverErr *UpstreamFailoverError
		if errors.As(err, &failoverErr) {
			return nil, inputTokens, err
		}
		return nil, inputTokens, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, inputTokens, nil
	}
	cacheUsage := s.buildKiroCacheEmulationUsage(account, group, anthropicBody, mappedModel, inputTokens)
	requestCtx.CacheEmulationUsage = cacheUsage.toKiroUsage()

	pr, pw := io.Pipe()
	wrappedHeaders := resp.Header.Clone()
	wrappedHeaders.Set("Content-Type", "text/event-stream")
	if requestID := buildKiroRequestID(resp); requestID != "" {
		wrappedHeaders.Set("x-request-id", requestID)
	}

	go func() {
		defer func() { _ = resp.Body.Close() }()
		_, streamErr := kiropkg.StreamEventStreamAsAnthropicWithContext(ctx, resp.Body, pw, mappedModel, inputTokens, requestCtx)
		if streamErr != nil {
			_ = pw.CloseWithError(streamErr)
			return
		}
		_ = pw.Close()
	}()

	return &http.Response{
		StatusCode: resp.StatusCode,
		Header:     wrappedHeaders,
		Body:       pr,
	}, inputTokens, nil
}

// executeKiroUpstreamWithServiceFailover 在 executeKiroUpstream 基础上叠加 service 层 429
// 内 failover。当原 executeKiroUpstream 返回 UpstreamFailoverError(429) 时,
// 函数在 service 内部调 SelectAccountForModelWithExclusions 选下一个账号、重取 token、
// 重建 payload 再发,最多 KIRO_SERVICE_RETRY_MAX 次。
//
// 与 handler 层 failover 相比的优势:
//   - 不需要走完整个 forwardKiroMessages 重启,延迟更低。
//   - handler 层 MaxSwitches 是全局额度;service 层切号不消耗它,留给真正
//     不同类型错误(401/403/5xx)使用。
//
// 切号时的副作用:
//   - 上一账号已经在 executeKiroUpstream 内做了 markKiro429 + RPM 已被 Increment
//     (前者由原函数完成,后者也是),都视为占用消耗,不回退。
//   - 新账号若也 429,继续 retry 直到 maxRetry 耗尽,最后透出最后一次 429 给 handler。
//
// excludedIDs 包含已经撞过 429 的账号 + caller 传入的初始 excludedIDs(用于跨次
// request failover);第一次调用时只含 initialAccount.ID。
//
// 当 parsed=nil 或 parsed.GroupID=nil 时退化为单次调用(不切号),保证不破坏
// 现有 web search / 测试等无 parsed 信息的调用路径。
func (s *GatewayService) executeKiroUpstreamWithServiceFailover(
	ctx context.Context,
	initialAccount *Account,
	parsed *ParsedRequest,
	anthropicBody []byte,
	mappedModel, requestModel, initialToken string,
	headers http.Header,
) (*Account, *http.Response, kiropkg.KiroRequestContext, error) {
	currentAccount := initialAccount
	currentToken := initialToken
	maxRetry := kiroServiceRetryMax()
	// 单次调用时(maxRetry=0 或 parsed 信息不足)直接退化为 executeKiroUpstream。
	canFailover := maxRetry > 0 && parsed != nil && parsed.GroupID != nil && initialAccount != nil

	excluded := map[int64]struct{}{}
	if initialAccount != nil {
		excluded[initialAccount.ID] = struct{}{}
	}

	for attempt := 0; ; attempt++ {
		resp, requestCtx, err := s.executeKiroUpstream(ctx, currentAccount, anthropicBody, mappedModel, requestModel, currentToken, headers)
		// 仅当返回的是 service 内部识别的 UpstreamFailoverError(429) 时才考虑切号。
		// executeKiroUpstream 返回 resp.StatusCode=429 + nil err 的兜底分支(endpoint 用完)
		// 也算 429,但此时 resp.Body 已被读取/封装,处理更复杂——暂不在 service 层做切号,
		// 让 handler 层去走整个 failover,保持现有兜底行为不破。
		if err != nil {
			var failoverErr *UpstreamFailoverError
			if canFailover && errors.As(err, &failoverErr) && failoverErr.StatusCode == http.StatusTooManyRequests && attempt < maxRetry {
				next, selectErr := s.SelectAccountForModelWithExclusions(ctx, parsed.GroupID, "" /*sessionHash 不复用*/, requestModel, excluded)
				if selectErr != nil || next == nil {
					return currentAccount, resp, requestCtx, err
				}
				// 重选号后重新取 token 并继续 retry。
				nextToken, nextTokenType, tokenErr := s.GetAccessToken(ctx, next)
				if tokenErr != nil || nextTokenType != "oauth" {
					// 取 token 失败,把当前账号加入 excluded 让后续 retry 继续,
					// 但本次不消耗 attempt(避免 token 抖动消耗 retry quota)。
					excluded[next.ID] = struct{}{}
					continue
				}
				excluded[next.ID] = struct{}{}
				currentAccount = next
				currentToken = nextToken
				continue
			}
		}
		return currentAccount, resp, requestCtx, err
	}
}

func (s *GatewayService) executeKiroUpstream(ctx context.Context, account *Account, anthropicBody []byte, mappedModel, requestModel, token string, headers http.Header) (*http.Response, kiropkg.KiroRequestContext, error) {
	var requestCtx kiropkg.KiroRequestContext
	if err := s.checkAndWaitKiroCooldown(ctx, buildKiroAccountKey(account)); err != nil {
		if failoverErr := asKiroCooldownFailoverError(err); failoverErr != nil {
			return nil, requestCtx, failoverErr
		}
		return nil, requestCtx, err
	}

	// P0 #2: Kiro 账号级 RPM 滑动窗口的原子检查 + 预递增。
	// 选号阶段已用只读 Get 做过初筛,这里做权威检查;返回 false 表示窗口名额耗尽,
	// 立即返回 UpstreamFailoverError(429) 让 handler 层切到下一个账号。
	// 没有递增计数 → 不需要 Decrement。
	if account != nil && account.Platform == PlatformKiro && account.Type == AccountTypeOAuth {
		if !s.TryIncrementKiroRPM(ctx, account.ID) {
			return nil, requestCtx, &UpstreamFailoverError{
				StatusCode:   http.StatusTooManyRequests,
				ResponseBody: []byte("kiro account rpm limit reached, please try again later"),
			}
		}
	}

	modelID := kiropkg.MapModel(mappedModel)
	currentToken := token
	buildResult, err := buildKiroPayloadForAccountWithRepo(ctx, s.accountRepo, account, anthropicBody, modelID, currentToken, requestModel, headers)
	if err != nil {
		return nil, requestCtx, err
	}
	payload := buildResult.Payload
	requestCtx = buildResult.Context

	endpoints := buildKiroEndpoints(account)
	proxyURL := kiroProxyURL(account)
	tlsProfile := s.tlsFPProfileService.ResolveTLSProfile(account)
	accountKey := buildKiroAccountKey(account)
	maxRetries := 2

	for idx, endpoint := range endpoints {
		for attempt := 0; attempt <= maxRetries; attempt++ {
			req, err := newKiroJSONRequest(ctx, endpoint.URL, payload, currentToken, accountKey, buildKiroMachineID(account), endpoint.AmzTarget, account)
			if err != nil {
				return nil, requestCtx, err
			}

			resp, err := s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, tlsProfile)
			if err != nil {
				if attempt < maxRetries {
					if sleepErr := sleepKiroRetry(ctx, attempt); sleepErr != nil {
						return nil, requestCtx, sleepErr
					}
					continue
				}
				return nil, requestCtx, err
			}

			if resp.StatusCode == http.StatusTooManyRequests {
				// P1 #5+#8: 同时 mark 账号级 + (account, model family) 级冷却,
				// family 时长优先用上游 Retry-After,缺失则用 KIRO_FAMILY_COOLDOWN_DEFAULT_S。
				// 先读 body(用于 Retry-After body fallback 和后续 resp 返回),
				// 再 mark,最后用 resetHTTPResponseBody 还原 body 给调用方。
				respBody, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil {
					return nil, requestCtx, readErr
				}
				cooldown, err := s.markKiro429WithFamily(ctx, account, accountKey, mappedModel, resp.Header, respBody)
				if err != nil {
					return nil, requestCtx, err
				}
				if idx+1 < len(endpoints) {
					if sleepErr := sleepKiroRetry(ctx, attempt); sleepErr != nil {
						return nil, requestCtx, sleepErr
					}
					break
				}
				resp.Header.Set("x-kiro-cooldown", cooldown.String())
				resetHTTPResponseBody(resp, respBody)
				return resp, requestCtx, nil
			}

			if resp.StatusCode == http.StatusRequestTimeout || (resp.StatusCode >= 500 && resp.StatusCode < 600) {
				if attempt < maxRetries {
					_ = resp.Body.Close()
					if sleepErr := sleepKiroRetry(ctx, attempt); sleepErr != nil {
						return nil, requestCtx, sleepErr
					}
					continue
				}
				if idx+1 < len(endpoints) {
					_ = resp.Body.Close()
					if sleepErr := sleepKiroRetry(ctx, attempt); sleepErr != nil {
						return nil, requestCtx, sleepErr
					}
					break
				}
				return resp, requestCtx, nil
			}

			if resp.StatusCode == http.StatusPaymentRequired {
				respBody, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil {
					return nil, requestCtx, readErr
				}
				classification := classifyKiroHTTPError(resp.StatusCode, string(respBody))
				if classification.Category == kiroErrorMonthlyRequest {
					s.markKiroMonthlyRequestCountRateLimited(ctx, account, string(respBody))
				}
				return nil, requestCtx, &UpstreamFailoverError{
					StatusCode:      resp.StatusCode,
					ResponseBody:    respBody,
					ResponseHeaders: resp.Header.Clone(),
				}
			}

			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				respBody, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil {
					return nil, requestCtx, readErr
				}

				if resp.StatusCode == http.StatusForbidden && isKiroSuspendedBody(respBody) {
					if _, err := s.markKiroSuspended(ctx, accountKey); err != nil {
						return nil, requestCtx, err
					}
					resetHTTPResponseBody(resp, respBody)
					return resp, requestCtx, nil
				}

				if s.kiroTokenProvider != nil && (resp.StatusCode == http.StatusUnauthorized || isKiroTokenErrorBody(respBody)) && attempt < maxRetries {
					refreshedToken, refreshErr := s.kiroTokenProvider.ForceRefreshAccessToken(ctx, account)
					if refreshErr == nil && strings.TrimSpace(refreshedToken) != "" {
						currentToken = refreshedToken
						accountKey = buildKiroAccountKey(account)
						buildResult, err = buildKiroPayloadForAccountWithRepo(ctx, s.accountRepo, account, anthropicBody, modelID, currentToken, requestModel, headers)
						if err != nil {
							return nil, requestCtx, err
						}
						payload = buildResult.Payload
						requestCtx = buildResult.Context
						if sleepErr := sleepKiroRetry(ctx, attempt); sleepErr != nil {
							return nil, requestCtx, sleepErr
						}
						continue
					}
					if refreshErr != nil && isNonRetryableRefreshError(refreshErr) {
						resetHTTPResponseBody(resp, respBody)
						return resp, requestCtx, nil
					}
				}

				if classifyKiroHTTPError(resp.StatusCode, string(respBody)).Category == kiroErrorAuthError {
					s.markKiroAuthTemporarilyUnavailable(ctx, account, resp.StatusCode, string(respBody))
				}

				resetHTTPResponseBody(resp, respBody)
				return resp, requestCtx, nil
			}

			if resp.StatusCode == http.StatusBadRequest {
				respBody, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil {
					return nil, requestCtx, readErr
				}
				classification := classifyKiroHTTPError(resp.StatusCode, string(respBody))
				logKiroBadRequestClassification(classification, account, mappedModel, resp.Header, respBody)
				resetHTTPResponseBody(resp, respBody)
				return resp, requestCtx, nil
			}

			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				if err := s.markKiroSuccess(ctx, accountKey); err != nil {
					_ = resp.Body.Close()
					return nil, requestCtx, err
				}
			}
			return resp, requestCtx, nil
		}
	}
	return nil, requestCtx, fmt.Errorf("kiro upstream endpoints exhausted")
}

func buildKiroEndpoints(account *Account) []kiroEndpointConfig {
	region := kiroAPIRegion(account)
	return []kiroEndpointConfig{
		{
			URL:  fmt.Sprintf("https://q.%s.amazonaws.com/generateAssistantResponse", region),
			Name: "AmazonQ",
		},
	}
}

func buildKiroPayloadForAccountWithRepo(ctx context.Context, repo AccountRepository, account *Account, anthropicBody []byte, modelID, token, requestModel string, headers http.Header) (*kiropkg.KiroBuildResult, error) {
	profileArn := resolveKiroPayloadProfileArn(account)
	anthropicBody = prepareKiroPayloadBodyForRequestModel(anthropicBody, requestModel)
	return kiropkg.BuildKiroPayloadWithContext(anthropicBody, modelID, profileArn, "AI_EDITOR", headers)
}

func prepareKiroPayloadBodyForRequestModel(anthropicBody []byte, requestModel string) []byte {
	requestModel = strings.TrimSpace(requestModel)
	if requestModel == "" || !strings.Contains(strings.ToLower(requestModel), "thinking") {
		return anthropicBody
	}
	bodyModel := strings.TrimSpace(gjson.GetBytes(anthropicBody, "model").String())
	if bodyModel == "" || strings.EqualFold(bodyModel, requestModel) || strings.Contains(strings.ToLower(bodyModel), "thinking") {
		return anthropicBody
	}
	if next, ok := setJSONValueBytes(anthropicBody, "model", requestModel); ok {
		return next
	}
	return anthropicBody
}

func (s *GatewayService) markKiroAuthTemporarilyUnavailable(ctx context.Context, account *Account, statusCode int, body string) {
	if s == nil || s.accountRepo == nil || account == nil {
		return
	}
	until := time.Now().Add(10 * time.Minute)
	reason := fmt.Sprintf("kiro auth failure (%d): %s", statusCode, strings.TrimSpace(body))
	_ = s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, reason)
}

func (s *GatewayService) markKiroMonthlyRequestCountRateLimited(ctx context.Context, account *Account, body string) {
	if s == nil || s.accountRepo == nil || account == nil {
		return
	}
	resetAt := nextKiroMonthlyResetUTC(time.Now())
	if err := s.accountRepo.SetRateLimited(ctx, account.ID, resetAt); err != nil {
		logger.L().Warn("kiro monthly request count rate-limit failed",
			zap.Int64("account_id", account.ID),
			zap.Time("reset_at", resetAt),
			zap.Error(err),
		)
		return
	}
	reason := "kiro monthly request count exhausted (402): MONTHLY_REQUEST_COUNT"
	if trimmed := strings.TrimSpace(body); trimmed != "" {
		reason = fmt.Sprintf("%s body=%s", reason, truncateForLog([]byte(trimmed), 512))
	}
	logger.L().Warn("kiro monthly request count rate-limited",
		zap.Int64("account_id", account.ID),
		zap.Time("reset_at", resetAt),
		zap.String("reason", reason),
	)
}

func nextKiroMonthlyResetUTC(now time.Time) time.Time {
	utc := now.UTC()
	year, month, _ := utc.Date()
	return time.Date(year, month+1, 1, 0, 0, 0, 0, time.UTC)
}

func resetHTTPResponseBody(resp *http.Response, body []byte) {
	if resp == nil {
		return
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
}

func estimateKiroInputTokens(body []byte) int {
	if len(body) == 0 {
		return 0
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err == nil {
		return countKiroInputTokensFromPayload(payload)
	}
	tokens := len(body) / 4
	if tokens == 0 {
		return 1
	}
	return tokens
}

func kiroUsageToClaude(usage kiropkg.Usage, fallbackInput int) ClaudeUsage {
	inputTokens := usage.InputTokens
	if inputTokens == 0 {
		inputTokens = fallbackInput
	}
	return ClaudeUsage{
		InputTokens:              inputTokens,
		OutputTokens:             usage.OutputTokens,
		CacheReadInputTokens:     usage.CacheReadInputTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
		CacheCreation5mTokens:    usage.CacheCreation5mInputTokens,
		CacheCreation1hTokens:    usage.CacheCreation1hInputTokens,
	}
}

func (s *GatewayService) markKiroInvalidModelRateLimited(ctx context.Context, account *Account, mappedModel string) {
	if s == nil || s.accountRepo == nil || account == nil || account.Type != AccountTypeOAuth {
		return
	}
	resetAt := time.Now().Add(kiroInvalidModelTempUnschedDuration)
	if err := s.accountRepo.SetRateLimited(ctx, account.ID, resetAt); err != nil {
		logger.L().Warn("kiro invalid model rate-limit failed",
			zap.Int64("account_id", account.ID),
			zap.String("mapped_model", strings.TrimSpace(mappedModel)),
			zap.Time("reset_at", resetAt),
			zap.Error(err),
		)
		return
	}
	logger.L().Warn("kiro invalid model rate-limited",
		zap.Int64("account_id", account.ID),
		zap.String("mapped_model", strings.TrimSpace(mappedModel)),
		zap.Time("reset_at", resetAt),
	)
}

func (s *GatewayService) handleKiroHTTPError(ctx context.Context, resp *http.Response, c *gin.Context, account *Account, mappedModel string, requestBody []byte) error {
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(respBody))
	upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
	if upstreamMsg == "" {
		upstreamMsg = strings.TrimSpace(string(respBody))
	}
	classification := classifyKiroHTTPError(resp.StatusCode, string(respBody))
	if resp.StatusCode == http.StatusBadRequest {
		logKiroBadRequestClassification(classification, account, "", resp.Header, respBody)
	}

	// P1 #6: 错误二分类 — Fatal 早判定。
	// 400 + context length 超限切到下一个账号也救不了,直接走"返客户端"路径,
	// 不调 rateLimitService.HandleUpstreamError (避免误标账号 fail_count),
	// 不触发 service 层 / handler 层切号。
	if broadClass, broadReason := kiroerrors.Classify(resp.StatusCode, respBody); broadClass == kiroerrors.ClassFatal {
		var accID int64
		if account != nil {
			accID = account.ID
		}
		logger.L().Info("kiro upstream fatal (no failover, no cooldown)",
			zap.Int("status", resp.StatusCode),
			zap.String("reason", broadReason),
			zap.Int64("account_id", accID),
			zap.String("mapped_model", mappedModel),
		)
		setOpsUpstreamError(c, resp.StatusCode, upstreamMsg, "")
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  resp.Header.Get("x-request-id"),
			Kind:               "fatal",
			Message:            upstreamMsg,
		})
		c.JSON(mapUpstreamStatusCode(resp.StatusCode), gin.H{
			"type": "error",
			"error": gin.H{
				"type":    "upstream_error",
				"message": coalesceKiroErrorMessage(resp.StatusCode, upstreamMsg),
			},
		})
		return fmt.Errorf("kiro fatal: %d %s (%s)", resp.StatusCode, upstreamMsg, broadReason)
	}

	if classification.Category == kiroErrorMonthlyRequest {
		s.markKiroMonthlyRequestCountRateLimited(ctx, account, string(respBody))
	}
	if classification.Category == kiroErrorBadRequestInvalidModel && account != nil && account.Type == AccountTypeOAuth {
		s.markKiroInvalidModelRateLimited(ctx, account, mappedModel)
		event := s.buildKiroInvalidModelUpstreamEvent(account, resp, upstreamMsg, mappedModel, requestBody, c)
		appendOpsUpstreamError(c, event)
		return &UpstreamFailoverError{
			StatusCode:      resp.StatusCode,
			ResponseBody:    respBody,
			ResponseHeaders: resp.Header.Clone(),
		}
	}

	if resp.StatusCode == http.StatusPaymentRequired || s.shouldFailoverUpstreamError(resp.StatusCode) {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  resp.Header.Get("x-request-id"),
			Kind:               "failover",
			Message:            upstreamMsg,
		})
		if s.rateLimitService != nil {
			s.rateLimitService.HandleUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody)
		}
		return &UpstreamFailoverError{
			StatusCode:      resp.StatusCode,
			ResponseBody:    respBody,
			ResponseHeaders: resp.Header.Clone(),
		}
	}

	setOpsUpstreamError(c, resp.StatusCode, upstreamMsg, "")
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: resp.StatusCode,
		UpstreamRequestID:  resp.Header.Get("x-request-id"),
		Kind:               "http_error",
		Message:            upstreamMsg,
	})
	c.JSON(mapUpstreamStatusCode(resp.StatusCode), gin.H{
		"type": "error",
		"error": gin.H{
			"type":    "upstream_error",
			"message": coalesceKiroErrorMessage(resp.StatusCode, upstreamMsg),
		},
	})
	return fmt.Errorf("kiro upstream error: %d %s", resp.StatusCode, upstreamMsg)
}

func (s *GatewayService) buildKiroInvalidModelUpstreamEvent(account *Account, resp *http.Response, upstreamMsg, mappedModel string, requestBody []byte, c *gin.Context) OpsUpstreamErrorEvent {
	_ = s
	requestedModel := strings.TrimSpace(gjson.GetBytes(requestBody, "model").String())
	hasTools := gjson.GetBytes(requestBody, "tools").Exists()
	hasAdaptiveThinking := strings.EqualFold(strings.TrimSpace(gjson.GetBytes(requestBody, "thinking.type").String()), "adaptive")
	hasContext1MBeta := false
	if c != nil {
		hasContext1MBeta = strings.Contains(c.GetHeader("Anthropic-Beta"), "context-1m")
	}
	return OpsUpstreamErrorEvent{
		Platform:            account.Platform,
		AccountID:           account.ID,
		AccountName:         account.Name,
		UpstreamStatusCode:  resp.StatusCode,
		UpstreamRequestID:   resp.Header.Get("x-request-id"),
		Kind:                "failover",
		Message:             upstreamMsg,
		RequestedModel:      requestedModel,
		MappedModel:         strings.TrimSpace(mappedModel),
		KiroModelID:         kiropkg.MapModel(mappedModel),
		HasTools:            hasTools,
		HasAdaptiveThinking: hasAdaptiveThinking,
		HasContext1MBeta:    hasContext1MBeta,
	}
}

func logKiroBadRequestClassification(classification kiroErrorClassification, account *Account, model string, headers http.Header, body []byte) {
	if classification.StatusCode != http.StatusBadRequest {
		return
	}
	var accountID int64
	if account != nil {
		accountID = account.ID
	}
	logger.L().Warn("kiro upstream bad request classified",
		zap.String("category", classification.Category),
		zap.Int("status", classification.StatusCode),
		zap.Int64("account_id", accountID),
		zap.String("model", strings.TrimSpace(model)),
		zap.String("request_id", headers.Get("x-request-id")),
		zap.String("body_excerpt", truncateForLog(body, 512)),
	)
}

func coalesceKiroErrorMessage(statusCode int, upstreamMsg string) string {
	if upstreamMsg != "" {
		return upstreamMsg
	}
	switch statusCode {
	case http.StatusTooManyRequests:
		return "Rate limit exceeded"
	case http.StatusForbidden:
		return "Access denied"
	case http.StatusUnauthorized:
		return "Authentication failed"
	default:
		return "Upstream request failed"
	}
}
