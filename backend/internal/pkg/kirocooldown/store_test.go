package kirocooldown

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"
)

func TestClearEarliestTransientCooldownEmptyKeysIsSafe(t *testing.T) {
	store := NewStore(redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"}))

	cleared, err := store.ClearEarliestTransientCooldown(context.Background(), nil)
	if err != nil {
		t.Fatalf("ClearEarliestTransientCooldown(nil) error = %v", err)
	}
	if cleared {
		t.Fatal("ClearEarliestTransientCooldown(nil) cleared = true, want false")
	}
}

func TestClearEarliestTransientCooldownUnavailableStore(t *testing.T) {
	store := NewStore(nil)

	cleared, err := store.ClearEarliestTransientCooldown(context.Background(), []string{"token"})
	if err == nil {
		t.Fatal("ClearEarliestTransientCooldown unavailable store error = nil")
	}
	if cleared {
		t.Fatal("ClearEarliestTransientCooldown unavailable store cleared = true, want false")
	}
}

// P1 #7: 批量 API 的空键 / maxClear<=0 / nil store 退化路径。
func TestClearEarliestTransientCooldownBatchEdgeCases(t *testing.T) {
	t.Run("nil store fails closed with error", func(t *testing.T) {
		store := NewStore(nil)
		n, err := store.ClearEarliestTransientCooldownBatch(context.Background(), []string{"token"}, 3, 500)
		if err == nil {
			t.Fatal("nil store should return error, got nil")
		}
		if n != 0 {
			t.Fatalf("nil store n = %d, want 0", n)
		}
	})
	t.Run("maxClear<=0 returns 0 with no error", func(t *testing.T) {
		store := NewStore(redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"}))
		n, err := store.ClearEarliestTransientCooldownBatch(context.Background(), []string{"a", "b"}, 0, 500)
		if err != nil {
			t.Fatalf("maxClear=0 unexpected err: %v", err)
		}
		if n != 0 {
			t.Fatalf("maxClear=0 n = %d, want 0", n)
		}
	})
	t.Run("empty tokenKeys returns 0", func(t *testing.T) {
		store := NewStore(redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"}))
		n, err := store.ClearEarliestTransientCooldownBatch(context.Background(), nil, 5, 500)
		if err != nil {
			t.Fatalf("nil keys unexpected err: %v", err)
		}
		if n != 0 {
			t.Fatalf("nil keys n = %d, want 0", n)
		}
	})
}
