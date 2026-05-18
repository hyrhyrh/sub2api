package kiroerrors

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestParseRetryAfter_Seconds(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "30")
	d, ok := ParseRetryAfter(h)
	if !ok || d != 30*time.Second {
		t.Errorf("ParseRetryAfter(30) = %v, %v, want 30s, true", d, ok)
	}
}

func TestParseRetryAfter_Float(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "1.5")
	d, ok := ParseRetryAfter(h)
	if !ok || d != 1500*time.Millisecond {
		t.Errorf("ParseRetryAfter(1.5) = %v, %v, want 1.5s, true", d, ok)
	}
}

func TestParseRetryAfter_HTTPDate(t *testing.T) {
	future := time.Now().Add(2 * time.Minute).UTC()
	h := http.Header{}
	h.Set("Retry-After", future.Format(http.TimeFormat))
	d, ok := ParseRetryAfter(h)
	if !ok {
		t.Fatalf("ParseRetryAfter(http-date) failed")
	}
	// 允许 5s 误差(时间已流逝)
	if d < 110*time.Second || d > 125*time.Second {
		t.Errorf("ParseRetryAfter(http-date) = %v, want ~120s", d)
	}
}

func TestParseRetryAfter_Missing(t *testing.T) {
	if _, ok := ParseRetryAfter(http.Header{}); ok {
		t.Error("missing header should return false")
	}
	if _, ok := ParseRetryAfter(nil); ok {
		t.Error("nil header should return false")
	}
}

func TestParseRetryAfter_RetryAfterMs(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After-Ms", "1500")
	d, ok := ParseRetryAfter(h)
	if !ok || d != 1500*time.Millisecond {
		t.Errorf("ParseRetryAfter(Retry-After-Ms=1500) = %v, %v", d, ok)
	}
}

func TestParseRetryAfter_XRateLimitReset(t *testing.T) {
	future := time.Now().Add(45 * time.Second).Unix()
	h := http.Header{}
	h.Set("X-RateLimit-Reset", strconv.FormatInt(future, 10))
	d, ok := ParseRetryAfter(h)
	if !ok {
		t.Fatal("ParseRetryAfter(X-RateLimit-Reset) failed")
	}
	if d < 40*time.Second || d > 50*time.Second {
		t.Errorf("d = %v, want ~45s", d)
	}
}

func TestParseRetryAfterFromBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want time.Duration
	}{
		{"retry_after_seconds", `{"retry_after_seconds":15}`, 15 * time.Second},
		{"retry_after_ms", `{"retry_after_ms":2500}`, 2500 * time.Millisecond},
		{"retry_after", `{"retry_after":3}`, 3 * time.Second},
		{"nested", `{"error":{"retry_after_seconds":7}}`, 7 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, ok := ParseRetryAfterFromBody([]byte(tc.body))
			if !ok || d != tc.want {
				t.Errorf("ParseRetryAfterFromBody(%s) = %v, %v, want %v, true", tc.body, d, ok, tc.want)
			}
		})
	}
}

func TestApplyRetryAfterBounds(t *testing.T) {
	cases := []struct {
		in    time.Duration
		minMS int64
		maxS  int64
		want  time.Duration
	}{
		{50 * time.Millisecond, 200, 7 * 86400, 200 * time.Millisecond},
		{5 * time.Second, 200, 7 * 86400, 5 * time.Second},
		{30 * 24 * time.Hour, 200, 7 * 86400, 7 * 24 * time.Hour},
		{0, 200, 7 * 86400, 200 * time.Millisecond}, // 也提到 min
	}
	for _, tc := range cases {
		got := ApplyRetryAfterBounds(tc.in, tc.minMS, tc.maxS)
		if got != tc.want {
			t.Errorf("ApplyRetryAfterBounds(%v, %d, %d) = %v, want %v", tc.in, tc.minMS, tc.maxS, got, tc.want)
		}
	}
}
