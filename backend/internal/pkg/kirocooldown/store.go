package kirocooldown

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	MinRequestInterval = time.Second
	MaxRequestInterval = 2 * time.Second

	CooldownReason429       = "rate_limit_exceeded"
	CooldownReasonSuspended = "account_suspended"

	ShortCooldown = time.Minute
	MaxCooldown   = 5 * time.Minute
	LongCooldown  = 24 * time.Hour

	redisTimeout = 3 * time.Second
	activeTTL    = 10 * time.Second
	stateTTL     = 25 * time.Hour
	keyPrefix    = "kiro:cooldown:"

	// P0 #4 配置默认值(可被 env 覆盖):
	// KIRO_FAIL_COUNT_DECAY_MIN: 距上次 429 超过该分钟数则 fail_count 重置为 1,
	//   打破"在 cooldown 中拿不到 success → fail_count 永远不清零 → 反复回锁"死循环。
	// KIRO_COOLDOWN_JITTER_PCT: cooldown 时长 ±15% 抖动,防多账号同步解锁雪崩。
	DefaultFailCountDecayMinutes = 10
	DefaultCooldownJitterPct     = 0.15
)

// 从 env 读取 P0 #4 参数。kirocooldown 是 pkg 包,不直接依赖 service 包的 config;
// 用 os.Getenv 兼顾简洁(env 由 systemd 注入,启动后不变)。
const (
	envFailCountDecayMinutes = "KIRO_FAIL_COUNT_DECAY_MIN"
	envCooldownJitterPct     = "KIRO_COOLDOWN_JITTER_PCT"
)

// failCountDecayThresholdMs 返回 fail_count 衰减阈值(毫秒)。
// 非数字 / 缺失 / 负值时回退默认 10 分钟。<=0 表示禁用衰减(仍按递增累计)。
func failCountDecayThresholdMs() int64 {
	raw := strings.TrimSpace(os.Getenv(envFailCountDecayMinutes))
	if raw == "" {
		return int64(DefaultFailCountDecayMinutes) * 60 * 1000
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return int64(DefaultFailCountDecayMinutes) * 60 * 1000
	}
	return int64(v) * 60 * 1000
}

// cooldownJitterPct 返回 cooldown 抖动百分比 (0~1)。
// 非数字 / 缺失时回退默认 0.15;<=0 关闭抖动;>=1 截断为 0.99 防止 cooldown 归零。
func cooldownJitterPct() float64 {
	raw := strings.TrimSpace(os.Getenv(envCooldownJitterPct))
	if raw == "" {
		return DefaultCooldownJitterPct
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return DefaultCooldownJitterPct
	}
	if v < 0 {
		return 0
	}
	if v > 0.99 {
		return 0.99
	}
	return v
}

var (
	ErrStoreUnavailable = errors.New("kiro cooldown store unavailable")

	reserveRequestScript = redis.NewScript(`
local t = redis.call('TIME')
local now_ms = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local last_request_ms = tonumber(redis.call('HGET', KEYS[1], 'last_request_ms') or '0')
local fail_count = tonumber(redis.call('HGET', KEYS[1], 'fail_count') or '0')
local cooldown_until_ms = tonumber(redis.call('HGET', KEYS[1], 'cooldown_until_ms') or '0')
local cooldown_reason = redis.call('HGET', KEYS[1], 'cooldown_reason') or ''
local interval_ms = tonumber(ARGV[1])
local active_ttl_ms = tonumber(ARGV[2])
local state_ttl_ms = tonumber(ARGV[3])

if cooldown_until_ms > now_ms then
  return {1, cooldown_until_ms - now_ms, cooldown_reason}
end

if cooldown_until_ms > 0 then
  redis.call('HDEL', KEYS[1], 'cooldown_until_ms', 'cooldown_reason')
end

local next_slot_ms = now_ms
if last_request_ms > 0 then
  local candidate_ms = last_request_ms + interval_ms
  if candidate_ms > now_ms then
    next_slot_ms = candidate_ms
  end
end

redis.call('HSET', KEYS[1], 'last_request_ms', next_slot_ms)
if fail_count > 0 or cooldown_until_ms > now_ms then
  redis.call('PEXPIRE', KEYS[1], state_ttl_ms)
else
  redis.call('PEXPIRE', KEYS[1], active_ttl_ms)
end
return {0, next_slot_ms - now_ms, ''}
`)

	// mark429Script:在 P0 #4 之前 fail_count 单调递增(只有 MarkSuccess 才重置),
	// 但被冷却的账号不会被选中 → 没机会 success → fail_count 永远不清零,
	// 形成"5min 上限反复回锁"死循环。
	//
	// 改造:
	//   ARGV[5] = decay_threshold_ms:若距上次失败 > 该阈值,fail_count 重置为 1。
	//   ARGV[6] = jitter_numerator / 1e6:乘以 cooldown_ms 得到带 ±jitter 的冷却时长。
	//     调用方在 Go 端用 rand 生成 [1-pct, 1+pct] 区间的 numerator,避免 Lua 内 math.random
	//     在共享 Redis 实例上的种子问题(Redis 自带 randomseed = key+time,但跨连接不稳)。
	mark429Script = redis.NewScript(`
local t = redis.call('TIME')
local now_ms = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local short_cooldown_ms = tonumber(ARGV[1])
local max_cooldown_ms = tonumber(ARGV[2])
local state_ttl_ms = tonumber(ARGV[3])
local decay_threshold_ms = tonumber(ARGV[5])
local jitter_num = tonumber(ARGV[6])
local last_fail_ms = tonumber(redis.call('HGET', KEYS[1], 'last_fail_ms') or '0')
local prev_count = tonumber(redis.call('HGET', KEYS[1], 'fail_count') or '0')
local fail_count
if last_fail_ms > 0 and decay_threshold_ms > 0 and (now_ms - last_fail_ms) > decay_threshold_ms then
  fail_count = 1
else
  fail_count = prev_count + 1
end
local cooldown_ms = short_cooldown_ms * (2 ^ (fail_count - 1))
if cooldown_ms > max_cooldown_ms then
  cooldown_ms = max_cooldown_ms
end
if jitter_num > 0 then
  cooldown_ms = math.floor(cooldown_ms * jitter_num / 1000000)
  if cooldown_ms < 1 then cooldown_ms = 1 end
end
redis.call('HSET', KEYS[1],
  'fail_count', fail_count,
  'last_fail_ms', now_ms,
  'cooldown_until_ms', now_ms + cooldown_ms,
  'cooldown_reason', ARGV[4]
)
redis.call('PEXPIRE', KEYS[1], state_ttl_ms)
return cooldown_ms
`)

	// markSuccessScript:Success 直接重置 fail_count + 清 last_fail_ms,
	// 让后续 Mark429 重新从 fail_count=1 开始;
	// 不清 last_fail_ms 也能工作(prev_count=0 时永远走 +1 分支),
	// 但清掉更易于运维 inspect。
	markSuccessScript = redis.NewScript(`
redis.call('HSET', KEYS[1],
  'fail_count', 0,
  'last_fail_ms', 0,
  'cooldown_until_ms', 0,
  'cooldown_reason', ''
)
redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[1]))
return 1
`)

	markSuspendedScript = redis.NewScript(`
local t = redis.call('TIME')
local now_ms = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local cooldown_ms = tonumber(ARGV[1])
local state_ttl_ms = tonumber(ARGV[2])
redis.call('HSET', KEYS[1],
  'fail_count', 0,
  'cooldown_until_ms', now_ms + cooldown_ms,
  'cooldown_reason', ARGV[3]
)
redis.call('PEXPIRE', KEYS[1], state_ttl_ms)
return cooldown_ms
`)
)

type Error struct {
	remaining time.Duration
	reason    string
}

type State struct {
	Active        bool
	Reason        string
	CooldownUntil time.Time
	Remaining     time.Duration
	FailCount     int
}

func NewError(remaining time.Duration, reason string) error {
	return &Error{remaining: remaining, reason: reason}
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.reason == "" {
		return fmt.Sprintf("kiro token is in cooldown for %v", e.remaining.Round(time.Second))
	}
	return fmt.Sprintf("kiro token is in cooldown for %v (reason: %s)", e.remaining.Round(time.Second), e.reason)
}

func Calculate429Cooldown(retryCount int) time.Duration {
	if retryCount < 0 {
		retryCount = 0
	}
	cooldown := ShortCooldown * time.Duration(1<<retryCount)
	if cooldown > MaxCooldown {
		return MaxCooldown
	}
	return cooldown
}

type Store struct {
	client *redis.Client
	rngMu  sync.Mutex
	rng    *rand.Rand
}

func NewStore(client *redis.Client) *Store {
	return &Store{
		client: client,
		rng:    rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (s *Store) ReserveRequest(ctx context.Context, tokenKey string) (time.Duration, error) {
	if err := s.validate(); err != nil {
		return 0, err
	}
	cacheCtx, cancel := withRedisTimeout(ctx)
	defer cancel()

	values, err := reserveRequestScript.Run(
		cacheCtx,
		s.client,
		[]string{RedisKey(tokenKey)},
		s.nextInterval().Milliseconds(),
		activeTTL.Milliseconds(),
		stateTTL.Milliseconds(),
	).Result()
	if err != nil {
		return 0, fmt.Errorf("kiro cooldown reserve request: %w", err)
	}
	parts, ok := values.([]interface{})
	if !ok || len(parts) != 3 {
		return 0, fmt.Errorf("kiro cooldown reserve request: unexpected response %T", values)
	}
	state, err := luaInt64(parts[0])
	if err != nil {
		return 0, fmt.Errorf("kiro cooldown reserve request state: %w", err)
	}
	waitMS, err := luaInt64(parts[1])
	if err != nil {
		return 0, fmt.Errorf("kiro cooldown reserve request wait: %w", err)
	}
	reason, err := luaString(parts[2])
	if err != nil {
		return 0, fmt.Errorf("kiro cooldown reserve request reason: %w", err)
	}
	if state == 1 {
		return 0, NewError(time.Duration(waitMS)*time.Millisecond, reason)
	}
	if waitMS <= 0 {
		return 0, nil
	}
	return time.Duration(waitMS) * time.Millisecond, nil
}

func (s *Store) MarkSuccess(ctx context.Context, tokenKey string) error {
	if err := s.validate(); err != nil {
		return err
	}
	cacheCtx, cancel := withRedisTimeout(ctx)
	defer cancel()
	if err := markSuccessScript.Run(
		cacheCtx,
		s.client,
		[]string{RedisKey(tokenKey)},
		activeTTL.Milliseconds(),
	).Err(); err != nil {
		return fmt.Errorf("kiro cooldown mark success: %w", err)
	}
	return nil
}

// Mark429 在 Redis 上原子记录一次 429 失败,返回本次进入冷却的时长。
//
// P0 #4:
//   - fail_count 衰减:距上次 429 超过 KIRO_FAIL_COUNT_DECAY_MIN(默认 10 分钟)
//     时把 fail_count 重置为 1,而不是无限累加。打破"5min 上限反复回锁"。
//   - cooldown jitter:Go 端用 rng 生成 ±KIRO_COOLDOWN_JITTER_PCT(默认 ±15%)
//     的乘数(以 1e6 为单位的 numerator),Lua 接收后乘除应用,防止多账号同步
//     解锁雪崩。jitterPct=0 时不抖动(保持原行为,便于回滚)。
func (s *Store) Mark429(ctx context.Context, tokenKey string) (time.Duration, error) {
	if err := s.validate(); err != nil {
		return 0, err
	}
	cacheCtx, cancel := withRedisTimeout(ctx)
	defer cancel()

	decayMs := failCountDecayThresholdMs()
	jitterPct := cooldownJitterPct()
	// jitter factor 落在 [1-pct, 1+pct]:rng.Float64 返回 [0, 1),
	// 乘以 2pct 后减 pct → [-pct, pct);加 1 → [1-pct, 1+pct)。
	jitterNumerator := int64(1_000_000) // 不抖动 → 1.0
	if jitterPct > 0 {
		s.rngMu.Lock()
		factor := 1.0 + (s.rng.Float64()*2-1)*jitterPct
		s.rngMu.Unlock()
		if factor < 0.01 {
			factor = 0.01 // 兜底:理论上 jitterPct<=0.99 已防止 factor<0.01
		}
		jitterNumerator = int64(factor * 1_000_000)
	}

	result, err := mark429Script.Run(
		cacheCtx,
		s.client,
		[]string{RedisKey(tokenKey)},
		ShortCooldown.Milliseconds(),
		MaxCooldown.Milliseconds(),
		stateTTL.Milliseconds(),
		CooldownReason429,
		decayMs,
		jitterNumerator,
	).Result()
	if err != nil {
		return 0, fmt.Errorf("kiro cooldown mark 429: %w", err)
	}
	cooldownMS, err := luaInt64(result)
	if err != nil {
		return 0, fmt.Errorf("kiro cooldown mark 429: %w", err)
	}
	return time.Duration(cooldownMS) * time.Millisecond, nil
}

func (s *Store) MarkSuspended(ctx context.Context, tokenKey string) (time.Duration, error) {
	if err := s.validate(); err != nil {
		return 0, err
	}
	cacheCtx, cancel := withRedisTimeout(ctx)
	defer cancel()
	result, err := markSuspendedScript.Run(
		cacheCtx,
		s.client,
		[]string{RedisKey(tokenKey)},
		LongCooldown.Milliseconds(),
		stateTTL.Milliseconds(),
		CooldownReasonSuspended,
	).Result()
	if err != nil {
		return 0, fmt.Errorf("kiro cooldown mark suspended: %w", err)
	}
	cooldownMS, err := luaInt64(result)
	if err != nil {
		return 0, fmt.Errorf("kiro cooldown mark suspended: %w", err)
	}
	return time.Duration(cooldownMS) * time.Millisecond, nil
}

func (s *Store) GetState(ctx context.Context, tokenKey string) (*State, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	cacheCtx, cancel := withRedisTimeout(ctx)
	defer cancel()

	values, err := s.client.HMGet(
		cacheCtx,
		RedisKey(tokenKey),
		"cooldown_until_ms",
		"cooldown_reason",
		"fail_count",
	).Result()
	if err != nil {
		return nil, fmt.Errorf("kiro cooldown get state: %w", err)
	}
	if len(values) != 3 {
		return nil, fmt.Errorf("kiro cooldown get state: unexpected response length %d", len(values))
	}

	cooldownUntilMS, err := luaInt64(values[0])
	if err != nil && values[0] != nil {
		return nil, fmt.Errorf("kiro cooldown get state cooldown_until_ms: %w", err)
	}
	reason, err := luaString(values[1])
	if err != nil {
		return nil, fmt.Errorf("kiro cooldown get state reason: %w", err)
	}
	failCount, err := luaInt64(values[2])
	if err != nil && values[2] != nil {
		return nil, fmt.Errorf("kiro cooldown get state fail_count: %w", err)
	}
	if cooldownUntilMS <= 0 {
		return nil, nil
	}

	cooldownUntil := time.UnixMilli(cooldownUntilMS)
	remaining := time.Until(cooldownUntil)
	if remaining <= 0 {
		return nil, nil
	}

	return &State{
		Active:        true,
		Reason:        reason,
		CooldownUntil: cooldownUntil,
		Remaining:     remaining,
		FailCount:     int(failCount),
	}, nil
}

// ClearEarliestTransientCooldown 仅 clear 一个最早冷却的 transient(429)账号。
// 保留是为了向后兼容老的调用方;新代码请用 ClearEarliestTransientCooldownBatch。
func (s *Store) ClearEarliestTransientCooldown(ctx context.Context, tokenKeys []string) (bool, error) {
	n, err := s.ClearEarliestTransientCooldownBatch(ctx, tokenKeys, 1, 0)
	return n > 0, err
}

// ClearEarliestTransientCooldownBatch 按 cooldown_until_ms 升序 clear / 提前到期最早冷却的
// maxClear 个 429-transient 账号。返回真正释放的数量。
//
// P1 #7: 原 ClearEarliestTransientCooldown 一次只解锁一个最早冷却,该账号被同请求 / 并发请求
// 立即雪崩重撞 429,5min 上限反复回锁。改为按比例(默认 30%)批量解锁后,
// 选号端再在这批账号里随机选,既打破"立即雪崩"也保持"优先解锁最早冷却"的公平性。
//
// 当 staggerStepMs > 0 时,按索引在每个账号上加 (i * staggerStepMs) * (1 ± 15%) 的解锁
// 延迟(同 P0 #4 的 KIRO_COOLDOWN_JITTER_PCT 抖动),让多个账号错峰解锁而不是同时回锁:
//   - 第 1 个(i=0)立即解锁:cooldown_until_ms 字段直接 HDel
//   - 第 i 个(i>=1)cooldown_until_ms 被写成 now + (i * staggerStepMs) * jitterFactor
//     而不是 HDel —— 调度逻辑会自然把它当成"还在小冷却",到点后自动可调度
//
// staggerStepMs <= 0 时退化为"全部立即解锁",jitter 不生效。
// maxClear <= 0 / 候选不足时取实际数量。pipeline 失败时返回错误(尽量不静默)。
func (s *Store) ClearEarliestTransientCooldownBatch(ctx context.Context, tokenKeys []string, maxClear int, staggerStepMs int64) (int, error) {
	if err := s.validate(); err != nil {
		return 0, err
	}
	if maxClear <= 0 {
		return 0, nil
	}
	uniqueKeys := make([]string, 0, len(tokenKeys))
	seen := make(map[string]struct{}, len(tokenKeys))
	for _, tokenKey := range tokenKeys {
		tokenKey = strings.TrimSpace(tokenKey)
		if tokenKey == "" {
			continue
		}
		redisKey := RedisKey(tokenKey)
		if _, ok := seen[redisKey]; ok {
			continue
		}
		seen[redisKey] = struct{}{}
		uniqueKeys = append(uniqueKeys, redisKey)
	}
	if len(uniqueKeys) == 0 {
		return 0, nil
	}

	cacheCtx, cancel := withRedisTimeout(ctx)
	defer cancel()

	type candidate struct {
		redisKey        string
		cooldownUntilMS int64
		failCount       int64
	}
	now := time.Now().UnixMilli()
	candidates := make([]candidate, 0, len(uniqueKeys))

	pipe := s.client.Pipeline()
	cmds := make([]*redis.SliceCmd, 0, len(uniqueKeys))
	for _, redisKey := range uniqueKeys {
		cmds = append(cmds, pipe.HMGet(cacheCtx, redisKey, "cooldown_until_ms", "cooldown_reason", "fail_count"))
	}
	if _, err := pipe.Exec(cacheCtx); err != nil {
		return 0, fmt.Errorf("kiro cooldown clear transient scan: %w", err)
	}

	for i, cmd := range cmds {
		values, err := cmd.Result()
		if err != nil {
			return 0, fmt.Errorf("kiro cooldown clear transient state: %w", err)
		}
		if len(values) != 3 {
			return 0, fmt.Errorf("kiro cooldown clear transient state: unexpected response length %d", len(values))
		}
		cooldownUntilMS, err := luaInt64(values[0])
		if err != nil && values[0] != nil {
			return 0, fmt.Errorf("kiro cooldown clear transient cooldown_until_ms: %w", err)
		}
		reason, err := luaString(values[1])
		if err != nil {
			return 0, fmt.Errorf("kiro cooldown clear transient reason: %w", err)
		}
		failCount, err := luaInt64(values[2])
		if err != nil && values[2] != nil {
			return 0, fmt.Errorf("kiro cooldown clear transient fail_count: %w", err)
		}
		if cooldownUntilMS <= now || reason != CooldownReason429 {
			continue
		}
		candidates = append(candidates, candidate{redisKey: uniqueKeys[i], cooldownUntilMS: cooldownUntilMS, failCount: failCount})
	}
	if len(candidates) == 0 {
		return 0, nil
	}

	// 按 (cooldown_until_ms ASC, fail_count ASC) 排序:先解锁最早冷却 + fail 计数最少的。
	// 不用 sort.Slice 避免 import 一长串,N 通常 <= 50,选择排序 O(N*maxClear) 即可。
	if maxClear > len(candidates) {
		maxClear = len(candidates)
	}
	for i := 0; i < maxClear; i++ {
		minIdx := i
		for j := i + 1; j < len(candidates); j++ {
			if candidates[j].cooldownUntilMS < candidates[minIdx].cooldownUntilMS ||
				(candidates[j].cooldownUntilMS == candidates[minIdx].cooldownUntilMS && candidates[j].failCount < candidates[minIdx].failCount) {
				minIdx = j
			}
		}
		if minIdx != i {
			candidates[i], candidates[minIdx] = candidates[minIdx], candidates[i]
		}
	}

	// 按索引算 stagger-with-jitter:第 i 个账号在 now + (i * staggerStepMs) * (1 ± 15%) 后解锁。
	// 第 0 个 staggerDelay=0 → HDel 立即释放;其余 HSet 一个临近未来时间(仍在 429 cooldown
	// 语义下,调度逻辑用 cooldown_until_ms <= now 判断可用)。
	// rng / rngMu 与 P0 #4 cooldown 抖动复用,无需新加同步原语。
	jitterPct := cooldownJitterPct()
	clearPipe := s.client.Pipeline()
	released := 0
	for i := 0; i < maxClear; i++ {
		var staggerMs int64
		if i > 0 && staggerStepMs > 0 {
			factor := 1.0
			if jitterPct > 0 {
				s.rngMu.Lock()
				factor = 1.0 + (s.rng.Float64()*2-1)*jitterPct
				s.rngMu.Unlock()
				if factor <= 0 {
					factor = 0.01
				}
			}
			staggerMs = int64(float64(i) * float64(staggerStepMs) * factor)
		}
		if staggerMs <= 0 {
			// 立即释放
			clearPipe.HDel(cacheCtx, candidates[i].redisKey, "cooldown_until_ms", "cooldown_reason")
			clearPipe.Expire(cacheCtx, candidates[i].redisKey, activeTTL)
		} else {
			// 错峰释放:把 cooldown_until_ms 提前到 now + staggerMs。reason 保持 429,
			// fail_count 不重置(待真正成功再 MarkSuccess 清零),仍走 P0 #4 衰减路径。
			newUntil := now + staggerMs
			clearPipe.HSet(cacheCtx, candidates[i].redisKey, "cooldown_until_ms", newUntil)
			clearPipe.Expire(cacheCtx, candidates[i].redisKey, activeTTL)
		}
		released++
	}
	if _, err := clearPipe.Exec(cacheCtx); err != nil {
		return 0, fmt.Errorf("kiro cooldown clear transient batch: %w", err)
	}
	return released, nil
}

func RedisKey(tokenKey string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(tokenKey)))
	digest := hex.EncodeToString(sum[:])
	return keyPrefix + "{" + digest + "}"
}

func ActiveTTL() time.Duration {
	return activeTTL
}

func StateTTL() time.Duration {
	return stateTTL
}

func (s *Store) validate() error {
	if s == nil || s.client == nil {
		return ErrStoreUnavailable
	}
	return nil
}

func (s *Store) nextInterval() time.Duration {
	s.rngMu.Lock()
	defer s.rngMu.Unlock()
	if MaxRequestInterval <= MinRequestInterval {
		return MinRequestInterval
	}
	return MinRequestInterval + time.Duration(s.rng.Int63n(int64(MaxRequestInterval-MinRequestInterval)))
}

func withRedisTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, redisTimeout)
}

func luaInt64(v any) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	case string:
		return strconv.ParseInt(strings.TrimSpace(n), 10, 64)
	case []byte:
		return strconv.ParseInt(strings.TrimSpace(string(n)), 10, 64)
	default:
		return 0, fmt.Errorf("unsupported lua numeric type %T", v)
	}
}

func luaString(v any) (string, error) {
	switch s := v.(type) {
	case string:
		return s, nil
	case []byte:
		return string(s), nil
	case nil:
		return "", nil
	default:
		return "", fmt.Errorf("unsupported lua string type %T", v)
	}
}
