package kiroerrors

import (
	"net/http"
	"testing"
)

func TestClassify_FatalContextLength(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"upper", `{"reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`},
		{"prose", `Error: Input is too long for context window`},
		{"expected_limit", `EXPECTED_LENGTH_LIMIT exceeded`},
		{"max_tokens", `max_tokens_exceeded for model`},
		{"exceeds_max", `request exceeds the maximum allowed length`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, reason := Classify(http.StatusBadRequest, []byte(tc.body))
			if class != ClassFatal {
				t.Errorf("Classify(%q) class = %s, want Fatal", tc.body, class)
			}
			if reason != "context_length_exceeded" {
				t.Errorf("reason = %q, want context_length_exceeded", reason)
			}
		})
	}
}

func TestClassify_UnclassifiedBadRequest(t *testing.T) {
	// 一般 400(invalid model 等)留给现有 kiro_error_classifier 走子分类,
	// 在调度决策层返 Unknown。
	class, _ := Classify(http.StatusBadRequest, []byte(`{"error":"INVALID_MODEL_ID"}`))
	if class != ClassUnknown {
		t.Errorf("Classify(invalid_model) = %s, want Unknown", class)
	}
}

func TestClassify_Fatal422(t *testing.T) {
	class, _ := Classify(http.StatusUnprocessableEntity, []byte(`{"error":"bad schema"}`))
	if class != ClassFatal {
		t.Errorf("class = %s, want Fatal", class)
	}
}

func TestClassify_AccountDead(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrorClass
	}{
		{401, ``, ClassAccountDead},
		{401, `{"error":"unauthorized"}`, ClassAccountDead},
		{403, `Account has been disabled`, ClassAccountDead},
		{403, `account suspended`, ClassAccountDead},
		{403, `account terminated`, ClassAccountDead},
		{403, `Forbidden`, ClassRecoverable}, // 普通 403 → Recoverable
	}
	for _, tc := range cases {
		class, _ := Classify(tc.status, []byte(tc.body))
		if class != tc.want {
			t.Errorf("Classify(%d, %q) = %s, want %s", tc.status, tc.body, class, tc.want)
		}
	}
}

func TestClassify_RateLimited(t *testing.T) {
	class, _ := Classify(http.StatusTooManyRequests, nil)
	if class != ClassRateLimited {
		t.Errorf("429 class = %s, want RateLimited", class)
	}

	class, reason := Classify(http.StatusPaymentRequired, []byte(`{"reason":"MONTHLY_REQUEST_COUNT"}`))
	if class != ClassRateLimited {
		t.Errorf("402+monthly class = %s, want RateLimited", class)
	}
	if reason != "monthly_request_count" {
		t.Errorf("reason = %q, want monthly_request_count", reason)
	}

	class, _ = Classify(http.StatusPaymentRequired, []byte(`{"error":"payment failed"}`))
	if class != ClassRecoverable {
		t.Errorf("402+other class = %s, want Recoverable", class)
	}
}

func TestClassify_Recoverable5xx(t *testing.T) {
	for _, code := range []int{500, 502, 503, 504, 599} {
		class, _ := Classify(code, nil)
		if class != ClassRecoverable {
			t.Errorf("%d class = %s, want Recoverable", code, class)
		}
	}
}

func TestClassify_Unknown(t *testing.T) {
	for _, code := range []int{100, 200, 300, 304} {
		class, _ := Classify(code, nil)
		if class != ClassUnknown {
			t.Errorf("%d class = %s, want Unknown", code, class)
		}
	}
}
