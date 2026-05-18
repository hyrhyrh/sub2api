package service

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kirocooldown"
	"github.com/Wei-Shaw/sub2api/internal/pkg/kiroerrors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/kirofamily"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

var errKiroCooldownStoreUnavailable = errors.New("kiro cooldown store unavailable")

type KiroCooldownStore interface {
	ReserveRequest(ctx context.Context, tokenKey string) (time.Duration, error)
	MarkSuccess(ctx context.Context, tokenKey string) error
	Mark429(ctx context.Context, tokenKey string) (time.Duration, error)
	MarkSuspended(ctx context.Context, tokenKey string) (time.Duration, error)
	GetState(ctx context.Context, tokenKey string) (*kirocooldown.State, error)
	ClearEarliestTransientCooldown(ctx context.Context, tokenKeys []string) (bool, error)
}

func asKiroCooldownFailoverError(err error) *UpstreamFailoverError {
	if err == nil {
		return nil
	}
	var cooldownErr *kirocooldown.Error
	if !errors.As(err, &cooldownErr) {
		return nil
	}
	return &UpstreamFailoverError{
		StatusCode:   http.StatusTooManyRequests,
		ResponseBody: []byte(cooldownErr.Error()),
	}
}

func (s *GatewayService) checkAndWaitKiroCooldown(ctx context.Context, tokenKey string) error {
	if s == nil || s.kiroCooldownStore == nil {
		return errKiroCooldownStoreUnavailable
	}
	waitFor, err := s.kiroCooldownStore.ReserveRequest(ctx, tokenKey)
	if err != nil {
		return err
	}
	if waitFor <= 0 {
		return nil
	}
	timer := time.NewTimer(waitFor)
	select {
	case <-ctx.Done():
		if !timer.Stop() {
			<-timer.C
		}
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *GatewayService) markKiroSuccess(ctx context.Context, tokenKey string) error {
	if s == nil || s.kiroCooldownStore == nil {
		return errKiroCooldownStoreUnavailable
	}
	return s.kiroCooldownStore.MarkSuccess(ctx, tokenKey)
}

func (s *GatewayService) markKiro429(ctx context.Context, tokenKey string) (time.Duration, error) {
	if s == nil || s.kiroCooldownStore == nil {
		return 0, errKiroCooldownStoreUnavailable
	}
	return s.kiroCooldownStore.Mark429(ctx, tokenKey)
}

// markKiro429WithFamily P1 #5+#8: 同时 mark 账号级 cooldown 和 family 级 cooldown。
//
// 流程:
//  1. 账号级 mark(沿用 P0 #4 的衰减 + 抖动 Lua) —— 让该账号短期不再被调度
//  2. 解析上游 Retry-After(优先)/ 默认 KIRO_FAMILY_COOLDOWN_DEFAULT_S
//  3. 应用 min/max 兜底(P1 #8)
//  4. 标 family cooldown(P1 #5) —— 只锁住对应 model,其他 model 仍可用
//
// 即使 family cooldown 失败 / 关闭,账号级仍然生效,行为退化到 P0。
// account 为 nil 时仅做账号级 mark,保持向后兼容。
func (s *GatewayService) markKiro429WithFamily(
	ctx context.Context,
	account *Account,
	tokenKey string,
	mappedModel string,
	respHeader http.Header,
	respBody []byte,
) (time.Duration, error) {
	// 1. 账号级 (P0)
	accountCD, err := s.markKiro429(ctx, tokenKey)
	if err != nil {
		return 0, err
	}

	// 2. family 级 (P1 #5)
	if account == nil || s.kiroFamilyCooldown == nil {
		return accountCD, nil
	}
	family := kirofamily.FamilyKey(mappedModel)
	if family == "" {
		return accountCD, nil
	}

	// 3. Retry-After 解析(P1 #8)
	var familyDur time.Duration
	if d, ok := kiroerrors.ParseRetryAfter(respHeader); ok {
		familyDur = d
	} else if d, ok := kiroerrors.ParseRetryAfterFromBody(respBody); ok {
		familyDur = d
	} else {
		familyDur = kiroFamilyCooldownDefault()
	}
	familyDur = kiroerrors.ApplyRetryAfterBounds(familyDur, kiroRetryAfterMinMS(), kiroRetryAfterMaxS())

	s.kiroFamilyCooldown.Mark(ctx, account.ID, family, familyDur, kirocooldown.CooldownReason429)
	logger.L().Info("kiro family cooldown marked",
		zap.Int64("account_id", account.ID),
		zap.String("family", family),
		zap.Duration("family_cooldown", familyDur),
		zap.Duration("account_cooldown", accountCD),
	)
	return accountCD, nil
}

func (s *GatewayService) markKiroSuspended(ctx context.Context, tokenKey string) (time.Duration, error) {
	if s == nil || s.kiroCooldownStore == nil {
		return 0, errKiroCooldownStoreUnavailable
	}
	return s.kiroCooldownStore.MarkSuspended(ctx, tokenKey)
}

func (s *GatewayService) getKiroCooldownState(ctx context.Context, tokenKey string) (*kirocooldown.State, error) {
	if s == nil || s.kiroCooldownStore == nil {
		return nil, errKiroCooldownStoreUnavailable
	}
	return s.kiroCooldownStore.GetState(ctx, tokenKey)
}

func kiroRuntimeStateSnapshot(state *kirocooldown.State) (string, string, *time.Time) {
	if state == nil || !state.Active {
		return "", "", nil
	}
	resetAt := state.CooldownUntil
	switch state.Reason {
	case kirocooldown.CooldownReasonSuspended:
		return "suspended", state.Reason, &resetAt
	default:
		return "cooldown", state.Reason, &resetAt
	}
}
