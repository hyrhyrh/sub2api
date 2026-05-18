package kirorpm

import (
	"context"
	"testing"
)

// Redis 不可用时所有方法必须 fail-open(Check 放行,Increment/Decrement/Get 静默)。
// Get 返回 (0, error) 是合理的:调度端只在 Get 成功时使用,失败时退回 maxRPM 检查。
func TestNilRDBFailOpen(t *testing.T) {
	s := NewStore(nil, 60)

	t.Run("Check fail-open returns true with no error", func(t *testing.T) {
		ok, err := s.Check(context.Background(), 1, 5)
		if err != nil {
			t.Fatalf("Check nil rdb err = %v, want nil", err)
		}
		if !ok {
			t.Fatal("Check nil rdb returned false, want true (fail-open)")
		}
	})

	t.Run("Increment silently ignores", func(t *testing.T) {
		if err := s.Increment(context.Background(), 1); err != nil {
			t.Fatalf("Increment nil rdb err = %v, want nil", err)
		}
	})

	t.Run("Decrement silently ignores", func(t *testing.T) {
		s.Decrement(context.Background(), 1)
	})

	t.Run("Get returns 0 with no error", func(t *testing.T) {
		n, err := s.Get(context.Background(), 1)
		if err != nil {
			t.Fatalf("Get nil rdb err = %v, want nil", err)
		}
		if n != 0 {
			t.Fatalf("Get nil rdb n = %d, want 0", n)
		}
	})
}

// 配置默认值:windowSec<=0 → 60s,WindowSec() 返回 60。
func TestWindowSecDefault(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{0, 60},
		{-1, 60},
		{30, 30},
		{120, 120},
	}
	for _, tc := range cases {
		s := NewStore(nil, tc.in)
		if got := s.WindowSec(); got != tc.want {
			t.Errorf("NewStore(nil, %d).WindowSec() = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// maxRPM<=0 表示不限制,Check 不论 Redis 状态都应返回 true。
func TestCheckUnlimited(t *testing.T) {
	s := NewStore(nil, 60)
	for _, max := range []int{0, -5} {
		ok, err := s.Check(context.Background(), 42, max)
		if err != nil {
			t.Fatalf("Check max=%d err = %v", max, err)
		}
		if !ok {
			t.Fatalf("Check max=%d returned false, want true (unlimited)", max)
		}
	}
}

// 调用 nil receiver 不能 panic,WindowSec 应有合理默认值。
func TestNilReceiver(t *testing.T) {
	var s *Store
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil receiver panicked: %v", r)
		}
	}()
	_ = s.WindowSec()
}
