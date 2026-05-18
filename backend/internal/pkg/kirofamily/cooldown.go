// Package kirofamily 提供 Kiro 账号 × 模型家族 (family) 维度的限流冷却。
//
// 设计动机:
//   - 当前 sub2api 的 cooldown 是账号粒度的:一次 Sonnet 触发 429,所有模型
//     (Haiku/Opus) 都被锁 1-5min。Kiro 后端实际按模型/token 维度限流,
//     账号级冷却浪费容量。
//   - airgate-core/backend/internal/scheduler/family.go 提供成熟实现:
//     Redis SETEX,TTL = until - now,SCAN 遍历清理,fail-open。
//   - 这里照搬该实现,但限定服务于 Kiro 平台账号。
//
// 关键设计:
//   - Redis key 格式: kiro-family-cooldown:v1:<acc_id>:<family>
//   - family = 请求的 model name (sub2api 内部统一名,例如 "claude-sonnet-4-5"),
//     不用 Kiro 后端 mapped 名,保证调度阶段不需要做映射,且对 model routing 透明。
//   - 写入用 SETEX 原子操作;旧 cooldown 直接被覆盖(每次 429 都视为最新建议)。
//   - Redis 不可用时所有方法 fail-open(IsInCooldown 返回 false 放行),
//     不能因为 Redis 抖动让 Kiro 整池不可用。
//
// 与 P0 #2 (kirorpm) / 已有 kirocooldown 的关系:
//   - kirocooldown: 账号级冷却,与 family 级 cooldown 同时存在;family cooldown 命中时
//     该账号在该 family 下不可调度,但其他 family 仍可。
//   - kirorpm: 账号级 RPM 滑动窗口,与 family 正交;先 family cooldown 检查,
//     再 RPM 检查,任一命中即不可调度。
package kirofamily

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	defaultRedisTimeout = 3 * time.Second
	keyPrefix           = "kiro-family-cooldown:v1:"
)

// Store 维护 (Kiro account, model family) 维度的限流冷却。
//
// rdb=nil 时所有方法 no-op,IsInCooldown 返回 (false, zero) (fail-open)。
type Store struct {
	rdb *redis.Client
}

// NewStore 构造 family cooldown 管理器。rdb=nil 时退化为 no-op。
func NewStore(rdb *redis.Client) *Store {
	return &Store{rdb: rdb}
}

// FamilyKey 把请求中的 model 名归一化为 family 键。
//
// 当前规则简单直接:小写 + trim,即每个 model 一个独立 family。
// 这样能精确按上游限流维度隔离 cooldown,且对 sub2api 的 model routing 透明。
//
// 后续若发现 Kiro 后端存在跨 model 共享池(例如 sonnet-thinking 与 sonnet 共享配额),
// 可在此扩展折叠规则;目前保守"每 model 一个 family"。
func FamilyKey(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

// key 生成完整的 Redis key,与 airgate-core 命名风格一致。
// 空 family 不应该出现(调用方应早 return),这里仍兜底以防 panic。
func key(accountID int64, family string) string {
	return fmt.Sprintf("%s%d:%s", keyPrefix, accountID, family)
}

// Mark 把 (account, family) 写入 cooldown,TTL = cooldownDur(最少 1ms)。
// reason 写入 value 供后台 inspect。
//
// 旧 cooldown 直接被覆盖:每次 429 都视为上游最新建议,无须保留历史。
// Redis 不可用时静默忽略,不阻断主链路。
func (s *Store) Mark(ctx context.Context, accountID int64, family string, cooldownDur time.Duration, reason string) {
	if s == nil || s.rdb == nil || family == "" {
		return
	}
	ttl := cooldownDur
	if ttl <= 0 {
		ttl = time.Millisecond
	}
	cacheCtx, cancel := context.WithTimeout(ctx, defaultRedisTimeout)
	defer cancel()
	// Set 失败 fail-open:返回错误也不影响主链路,下次撞墙再重新落库。
	_ = s.rdb.Set(cacheCtx, key(accountID, family), reason, ttl).Err()
}

// IsInCooldown 返回 (account, family) 是否在冷却中。
//
// 返回:
//   - (true, until): 仍在冷却中,until = 现在 + 剩余 TTL
//   - (false, zero): 不在冷却 / 已过期 / 没记录 / Redis 不可用 / family 为空
//
// 失败 fail-open:Redis 抖动时让请求过去试,撞了再重新落库,比阻断整池好。
func (s *Store) IsInCooldown(ctx context.Context, accountID int64, family string) (bool, time.Time) {
	if s == nil || s.rdb == nil || family == "" {
		return false, time.Time{}
	}
	cacheCtx, cancel := context.WithTimeout(ctx, defaultRedisTimeout)
	defer cancel()
	ttl, err := s.rdb.TTL(cacheCtx, key(accountID, family)).Result()
	if err != nil || ttl <= 0 {
		return false, time.Time{}
	}
	return true, time.Now().Add(ttl)
}

// Clear 清除指定 family 的 cooldown(管理员强制解封 / 测试)。
// 业务路径不需要主动调,TTL 到期自动失效。
func (s *Store) Clear(ctx context.Context, accountID int64, family string) {
	if s == nil || s.rdb == nil || family == "" {
		return
	}
	cacheCtx, cancel := context.WithTimeout(ctx, defaultRedisTimeout)
	defer cancel()
	_ = s.rdb.Del(cacheCtx, key(accountID, family)).Err()
}

// ClearAccount 清除账号下所有 family cooldown(管理员手动恢复 / 重启)。
// 用 SCAN 走一遍 keyPrefix:<acc_id>:* 模式,返回清掉的条数。
// 一个账号通常只有 0~3 个 family 在冷却,COUNT=32 一轮就回。
func (s *Store) ClearAccount(ctx context.Context, accountID int64) int {
	if s == nil || s.rdb == nil {
		return 0
	}
	cacheCtx, cancel := context.WithTimeout(ctx, defaultRedisTimeout)
	defer cancel()

	pattern := fmt.Sprintf("%s%d:*", keyPrefix, accountID)
	cleared := 0
	var cursor uint64
	for {
		keys, next, err := s.rdb.Scan(cacheCtx, cursor, pattern, 32).Result()
		if err != nil {
			return cleared
		}
		if len(keys) > 0 {
			if n, err := s.rdb.Del(cacheCtx, keys...).Result(); err == nil {
				cleared += int(n)
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	return cleared
}

// Entry 描述一条仍在生效的 family cooldown,供后台展示。
type Entry struct {
	Family string
	Until  time.Time
	Reason string
}

// List 列出指定账号当前所有生效中的 family cooldown。
// 后台展示用,Redis 不可用时返回 nil/空,不报错。
func (s *Store) List(ctx context.Context, accountID int64) []Entry {
	if s == nil || s.rdb == nil {
		return nil
	}
	cacheCtx, cancel := context.WithTimeout(ctx, defaultRedisTimeout)
	defer cancel()

	prefix := fmt.Sprintf("%s%d:", keyPrefix, accountID)
	pattern := prefix + "*"
	var entries []Entry
	var cursor uint64
	for {
		keys, next, err := s.rdb.Scan(cacheCtx, cursor, pattern, 32).Result()
		if err != nil {
			return entries
		}
		for _, k := range keys {
			ttl, err := s.rdb.TTL(cacheCtx, k).Result()
			if err != nil || ttl <= 0 {
				continue
			}
			reason, _ := s.rdb.Get(cacheCtx, k).Result()
			entries = append(entries, Entry{
				Family: strings.TrimPrefix(k, prefix),
				Until:  time.Now().Add(ttl),
				Reason: reason,
			})
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	return entries
}
