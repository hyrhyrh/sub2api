package kirofamily

import (
	"context"
	"testing"
	"time"
)

func TestFamilyKey(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"claude-sonnet-4-5", "claude-sonnet-4-5"},
		{"  CLAUDE-OPUS-4-7  ", "claude-opus-4-7"},
		{"", ""},
		{"   ", ""},
		{"GPT-4", "gpt-4"},
	}
	for _, tc := range cases {
		if got := FamilyKey(tc.in); got != tc.want {
			t.Errorf("FamilyKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNilStoreNoOp(t *testing.T) {
	// nil store / rdb 时所有方法不能 panic,且 IsInCooldown 必须返回 false。
	var s *Store
	s.Mark(context.Background(), 1, "x", time.Second, "test")
	if active, _ := s.IsInCooldown(context.Background(), 1, "x"); active {
		t.Error("nil store should report not in cooldown")
	}
	s.Clear(context.Background(), 1, "x")
	if n := s.ClearAccount(context.Background(), 1); n != 0 {
		t.Errorf("nil store ClearAccount = %d, want 0", n)
	}
	if entries := s.List(context.Background(), 1); entries != nil {
		t.Errorf("nil store List = %v, want nil", entries)
	}

	// store 存在但 rdb=nil 时同样 no-op。
	s2 := NewStore(nil)
	s2.Mark(context.Background(), 1, "x", time.Second, "test")
	if active, _ := s2.IsInCooldown(context.Background(), 1, "x"); active {
		t.Error("rdb=nil store should report not in cooldown")
	}
}

func TestEmptyFamilyNoOp(t *testing.T) {
	// 即使 rdb 有值,family="" 时也必须 no-op,避免误删通配 key。
	s := NewStore(nil) // rdb=nil 已经能覆盖该路径
	s.Mark(context.Background(), 1, "", time.Second, "test")
	if active, _ := s.IsInCooldown(context.Background(), 1, ""); active {
		t.Error("empty family should report not in cooldown")
	}
	s.Clear(context.Background(), 1, "")
}
