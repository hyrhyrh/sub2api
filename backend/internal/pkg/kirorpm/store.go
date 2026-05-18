// Package kirorpm 提供 Kiro 平台账号级别的 RPM 滑动窗口限流。
//
// 设计动机:
//   - sub2api 当前对 Kiro 账号完全没有 proactive RPM 限流,完全靠上游 429 反馈,
//     事后才反应,流量集中容易雪崩。
//   - airgate-core/backend/internal/scheduler/rpm.go 提供了成熟的实现:
//     Lua 原子脚本 GET → 比对 max → INCR → 自愈 TTL,fail-open。
//   - 这里照搬该实现,但限定服务于 Kiro 账号(其他平台已有自己的 RPM 机制)。
//
// 关键设计:
//   - key: kirorpm:<acc_id>:<unix/window_sec>,TTL = 2 × window_sec
//   - 用 Redis 服务器时间 (rdb.Time) 避免分布式时钟漂移
//   - Lua 原子脚本一次完成"读 + 比对 + 递增 + 自愈 TTL"
//   - 回退脚本 decrementScript 仅在 key 存在时 DECR,避免跨窗口建出 -1 无 TTL key
//   - Redis 不可用 fail-open (Check 返回 true 放行),不能因为 Redis 抖动让 Kiro 整池不可用
package kirorpm

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// defaultRedisTimeout 单次 Redis 调用超时。
// 与 kirocooldown 包保持一致(3s),不会影响热路径主链路:Check 超时 fail-open 后请求照常放行。
const defaultRedisTimeout = 3 * time.Second

// Store 维护 Kiro 账号的 RPM 滑动窗口计数。
//
// Redis 不可用时所有方法 fail-open:Check 返回 (true, nil) 放行,
// Increment/Decrement 静默忽略。宁可短暂超出限制,也不能阻断主链路。
type Store struct {
	rdb       *redis.Client
	windowSec int // 窗口秒数,通常 60
}

// NewStore 构造 RPM 计数器。rdb=nil 时所有方法 no-op(fail-open)。
// windowSec <= 0 时回退到 60 秒。
func NewStore(rdb *redis.Client, windowSec int) *Store {
	if windowSec <= 0 {
		windowSec = 60
	}
	return &Store{rdb: rdb, windowSec: windowSec}
}

// WindowSec 返回窗口长度,供调用方做 TTL/抖动等决策。
func (s *Store) WindowSec() int {
	if s == nil {
		return 0
	}
	return s.windowSec
}

// minuteKey 用 Redis 服务器时间生成窗口 key,避免多副本时钟漂移。
// Redis Time 失败时回退本地时间,误差可接受(误差 = 副本间时钟漂移)。
func (s *Store) minuteKey(ctx context.Context, accountID int64) string {
	t, err := s.rdb.Time(ctx).Result()
	if err != nil {
		t = time.Now()
	}
	bucket := t.Unix() / int64(s.windowSec)
	return fmt.Sprintf("kirorpm:%d:%d", accountID, bucket)
}

// tryIncrementScript 原子检查 RPM 上限并递增。
// ARGV[1]=maxRPM, ARGV[2]=ttlSec
// 返回:-1 = 已达上限(拒绝),>= 0 = 递增后的值(允许)
var tryIncrementScript = redis.NewScript(`
local key = KEYS[1]
local maxRPM = tonumber(ARGV[1])
local ttlSec = tonumber(ARGV[2])
local current = tonumber(redis.call('GET', key) or '0')
if current >= maxRPM then
  return -1
end
local newVal = redis.call('INCR', key)
if redis.call('TTL', key) < 0 then
  redis.call('EXPIRE', key, ttlSec)
end
return newVal
`)

// decrementScript 仅在 key 存在时 DECR,避免跨窗口边界建出值为 -1 的无 TTL key。
var decrementScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then
  return redis.call('DECR', KEYS[1])
end
return 0
`)

// Check 原子检查并预递增 RPM 计数。
//   - maxRPM <= 0:不限制,直接递增并返回 (true, nil)
//   - 已达 maxRPM:返回 (false, nil) 拒绝
//   - 未达 maxRPM:递增并返回 (true, nil)
//   - Redis 不可用 / 报错:fail-open 返回 (true, nil),不阻断主链路
//
// 调用方约定:Check 返回 true 后必须真正发起请求;若发起前判定要换号(因为别的原因)
// 或请求最终失败,应调用 Decrement 回退计数,避免占用窗口名额。
func (s *Store) Check(ctx context.Context, accountID int64, maxRPM int) (bool, error) {
	if s == nil || s.rdb == nil {
		return true, nil
	}
	if maxRPM <= 0 {
		// 不限制时仍递增计数,便于运维观察实际 RPM(无 -1 拒绝路径)
		_ = s.Increment(ctx, accountID)
		return true, nil
	}

	cacheCtx, cancel := context.WithTimeout(ctx, defaultRedisTimeout)
	defer cancel()

	key := s.minuteKey(cacheCtx, accountID)
	ttlSec := s.windowSec * 2
	result, err := tryIncrementScript.Run(cacheCtx, s.rdb, []string{key}, maxRPM, ttlSec).Int()
	if err != nil {
		// fail-open:Redis 抖动时放行,不阻断主链路。
		// 不重试 Increment:Lua 脚本失败后再追加一次 INCR 可能放大问题。
		return true, nil
	}
	return result >= 0, nil
}

// Increment 直接递增计数(不做限制检查),用于初始化或不限制路径。
// 失败时静默忽略(fail-open)。
func (s *Store) Increment(ctx context.Context, accountID int64) error {
	if s == nil || s.rdb == nil {
		return nil
	}
	cacheCtx, cancel := context.WithTimeout(ctx, defaultRedisTimeout)
	defer cancel()
	key := s.minuteKey(cacheCtx, accountID)
	pipe := s.rdb.TxPipeline()
	pipe.Incr(cacheCtx, key)
	pipe.Expire(cacheCtx, key, time.Duration(s.windowSec*2)*time.Second)
	_, _ = pipe.Exec(cacheCtx) // 失败 fail-open
	return nil
}

// Decrement 回退计数(请求失败/换号时撤销 Check 的预递增)。
// 仅当 key 存在时 DECR,避免窗口切换后建出 -1 的无 TTL key。
// 失败时静默忽略(fail-open)。
func (s *Store) Decrement(ctx context.Context, accountID int64) {
	if s == nil || s.rdb == nil {
		return
	}
	cacheCtx, cancel := context.WithTimeout(ctx, defaultRedisTimeout)
	defer cancel()
	key := s.minuteKey(cacheCtx, accountID)
	_, _ = decrementScript.Run(cacheCtx, s.rdb, []string{key}).Result()
}

// Get 读取当前窗口计数。失败返回 0(fail-open 语义)。
// 供运维 / metrics 使用,热路径不需要。
func (s *Store) Get(ctx context.Context, accountID int64) (int, error) {
	if s == nil || s.rdb == nil {
		return 0, nil
	}
	cacheCtx, cancel := context.WithTimeout(ctx, defaultRedisTimeout)
	defer cancel()
	key := s.minuteKey(cacheCtx, accountID)
	val, err := s.rdb.Get(cacheCtx, key).Int()
	if err == redis.Nil {
		return 0, nil
	}
	return val, err
}
