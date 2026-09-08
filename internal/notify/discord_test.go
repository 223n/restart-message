package notify

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSend_PostsValidPayload(t *testing.T) {
	var got Message
	var contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusNoContent) // Discord replies 204
	}))
	defer srv.Close()

	msg := &Message{
		Username: "Restart Notifier",
		Embeds: []Embed{{
			Title:  "🔄 PC が再起動しました",
			Color:  0x3498DB,
			Fields: []EmbedField{{Name: "種別", Value: "Windows Update", Inline: true}},
		}},
	}
	if err := Send(srv.URL, msg, 5*time.Second); err != nil {
		t.Fatalf("Send returned error: %v", err)
	}
	if contentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", contentType)
	}
	if got.Username != "Restart Notifier" {
		t.Fatalf("username = %q", got.Username)
	}
	if len(got.Embeds) != 1 || got.Embeds[0].Title != "🔄 PC が再起動しました" {
		t.Fatalf("embed not round-tripped: %+v", got.Embeds)
	}
}

func TestSend_Non2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"message":"bad"}`)
	}))
	defer srv.Close()

	if err := Send(srv.URL, &Message{Content: "x"}, 5*time.Second); err == nil {
		t.Fatal("expected error on 400 response")
	}
}

func TestSendWithRetry_PermanentErrorNotRetried(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNotFound) // 404: bad/deleted webhook — permanent
	}))
	defer srv.Close()

	if err := SendWithRetry(srv.URL, &Message{Content: "x"}, 5, time.Millisecond); err == nil {
		t.Fatal("expected error on 404")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (permanent 4xx must not be retried)", calls)
	}
}

func TestSendWithRetry_429IsRetried(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0") // retryable, no real wait
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := SendWithRetry(srv.URL, &Message{Content: "x"}, 3, time.Millisecond); err != nil {
		t.Fatalf("expected success after 429 retry, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestSend_TransportErrorDoesNotLeakWebhookURL(t *testing.T) {
	const secret = "SUPERSECRETTOKEN"
	webhookURL := "http://nonexistent.invalid.example/api/webhooks/123456789/" + secret
	err := Send(webhookURL, &Message{Content: "x"}, 2*time.Second)
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked the webhook secret: %v", err)
	}
}

func TestSendWithRetry_EventuallySucceeds(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := SendWithRetry(srv.URL, &Message{Content: "x"}, 3, 10*time.Millisecond); err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

// A webhook URL shaped like Discord's: a 19-digit id and a 68-character token.
const (
	testWebhookID    = "1234567890123456789"
	testWebhookToken = "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ012345"
)

func TestSend_ResponseBodyEchoingTheURLDoesNotLeakTheToken(t *testing.T) {
	// A TLS-terminating proxy answering instead of Discord typically puts the
	// whole request URL into its error page. That body ends up in service.log.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, "upstream failed for "+r.URL.Path)
	}))
	defer srv.Close()

	webhookURL := srv.URL + "/api/webhooks/" + testWebhookID + "/" + testWebhookToken
	err := Send(webhookURL, &Message{Content: "x"}, 5*time.Second)
	if err == nil {
		t.Fatal("expected an error on 502")
	}
	if strings.Contains(err.Error(), testWebhookToken) {
		t.Fatalf("error leaked the webhook token: %v", err)
	}
	if strings.Contains(err.Error(), testWebhookID) {
		t.Fatalf("error leaked the webhook id: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("expected the echoed segments to be redacted: %v", err)
	}
}

func TestSend_DiscordErrorBodyIsKeptIntact(t *testing.T) {
	// Redaction must not cost us the diagnostic Discord actually returns.
	const body = `{"message":"Unknown Webhook","code":10015}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, body)
	}))
	defer srv.Close()

	webhookURL := srv.URL + "/api/webhooks/" + testWebhookID + "/" + testWebhookToken
	err := Send(webhookURL, &Message{Content: "x"}, 5*time.Second)
	if err == nil {
		t.Fatal("expected an error on 404")
	}
	if !strings.Contains(err.Error(), body) {
		t.Fatalf("Discord's error body was altered: %v", err)
	}
}

func TestClampRetryAfter(t *testing.T) {
	tests := []struct {
		name string
		secs float64
		want time.Duration
	}{
		// Every comparison against NaN is false, so NaN clears both bounds and
		// reaches an undefined float-to-int conversion unless rejected by name.
		{"NaN is not a delay", math.NaN(), 0},
		{"positive infinity is capped", math.Inf(1), maxRetryAfter},
		{"negative infinity is floored", math.Inf(-1), 0},
		{"zero", 0, 0},
		{"negative", -5, 0},
		{"exactly the maximum is kept", maxRetryAfter.Seconds(), maxRetryAfter},
		{"just above the maximum is capped", maxRetryAfter.Seconds() + 0.5, maxRetryAfter},
		{"absurd value is capped", 1e9, maxRetryAfter},
		{"fractional seconds keep their precision", 1.25, 1250 * time.Millisecond},
		{"sub-second", 0.05, 50 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clampRetryAfter(tt.secs)
			// The concrete harm of the NaN conversion was a hugely negative
			// duration, so check that invariant first: behind the equality
			// check it could never fire, and it is the more telling
			// diagnostic for exactly the regression this test guards.
			if got < 0 {
				t.Fatalf("clampRetryAfter(%v) = %v: a delay must never be negative", tt.secs, got)
			}
			if got != tt.want {
				t.Fatalf("clampRetryAfter(%v) = %v, want %v", tt.secs, got, tt.want)
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name   string
		header string // "" means the header is absent
		body   string
		want   time.Duration
	}{
		{"header is preferred over the body", "1.5", `{"retry_after":9}`, 1500 * time.Millisecond},
		{"body is used when the header is absent", "", `{"retry_after":2}`, 2 * time.Second},
		{"unparseable header falls through to the body", "in a while", `{"retry_after":3}`, 3 * time.Second},
		{"neither present", "", "", 0},
		{"non-JSON body and no header", "", "429 Too Many Requests", 0},
		{"JSON body without the field", "", `{"message":"slow down"}`, 0},
		{"header above the cap", "99999", "", maxRetryAfter},
		{"body above the cap", "", `{"retry_after":99999}`, maxRetryAfter},
		// strconv.ParseFloat accepts "NaN" with a nil error, so this reaches
		// clampRetryAfter as a real NaN rather than as a parse failure.
		{"NaN header is not a delay", "NaN", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{}}
			if tt.header != "" {
				resp.Header.Set("Retry-After", tt.header)
			}
			if got := parseRetryAfter(resp, []byte(tt.body)); got != tt.want {
				t.Fatalf("parseRetryAfter(header=%q, body=%q) = %v, want %v", tt.header, tt.body, got, tt.want)
			}
		})
	}
}

func TestParseRetryAfter_HTTPDateForm(t *testing.T) {
	// RFC 9110 §10.2.3 allows Retry-After to be an HTTP-date as well as
	// delta-seconds, and the date form is the one CDNs and reverse proxies most
	// often emit — i.e. the 503-in-front-of-Discord responder that is the whole
	// justification for reading this header from non-429 responses. Until it was
	// implemented every header below parsed as nothing and came out 0s.
	//
	// A date carries only whole seconds and is resolved against the clock at parse
	// time, so the near-future case is asserted as a range. The extremes are exact
	// because there the clamp decides the answer, not the arithmetic.
	now := time.Now()
	tests := []struct {
		name             string
		header           string
		body             string
		wantMin, wantMax time.Duration
	}{
		{
			name:    "a date in the near future becomes that delay",
			header:  now.Add(10 * time.Second).UTC().Format(http.TimeFormat),
			wantMin: 5 * time.Second, // wide: the runner may stall between here and the parse
			wantMax: 10 * time.Second,
		},
		{
			name:    "a date beyond the cap is clamped rather than trusted",
			header:  now.Add(time.Hour).UTC().Format(http.TimeFormat),
			wantMin: maxRetryAfter,
			wantMax: maxRetryAfter,
		},
		{
			// Also the shape a clock skewed forward produces, which is the case
			// that matters here: this runs a minute into boot, possibly before
			// w32time has synced. It has to floor, not go negative.
			name:    "a date already past is not a delay",
			header:  now.Add(-time.Hour).UTC().Format(http.TimeFormat),
			wantMin: 0,
			wantMax: 0,
		},
		{
			name:    "the RFC 850 form http.ParseTime also accepts",
			header:  now.Add(time.Hour).UTC().Format(time.RFC850),
			wantMin: maxRetryAfter,
			wantMax: maxRetryAfter,
		},
		{
			name:    "the asctime form http.ParseTime also accepts",
			header:  now.Add(time.Hour).UTC().Format(time.ANSIC),
			wantMin: maxRetryAfter,
			wantMax: maxRetryAfter,
		},
		{
			// Same header-beats-body precedence the numeric form already had:
			// a date the server actually sent is not overridden by the body.
			name:    "a date wins over a retry_after in the body",
			header:  now.Add(-time.Hour).UTC().Format(http.TimeFormat),
			body:    `{"retry_after":9}`,
			wantMin: 0,
			wantMax: 0,
		},
		{
			// Neither form, so the existing fall-through must survive the change.
			name:    "prose is neither form and still falls through to the body",
			header:  "in a while",
			body:    `{"retry_after":3}`,
			wantMin: 3 * time.Second,
			wantMax: 3 * time.Second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{}}
			resp.Header.Set("Retry-After", tt.header)
			got := parseRetryAfter(resp, []byte(tt.body))
			if got < tt.wantMin || got > tt.wantMax {
				t.Fatalf("parseRetryAfter(header=%q, body=%q) = %v, want within [%v, %v]",
					tt.header, tt.body, got, tt.wantMin, tt.wantMax)
			}
			// The budget in maxRetryAfter's comment holds only because every path
			// out of this function is clamped; a new one must not be the exception.
			if got > maxRetryAfter {
				t.Fatalf("parseRetryAfter(header=%q) = %v, above the %v cap", tt.header, got, maxRetryAfter)
			}
		})
	}
}

func TestSend_HTTPDateRetryAfterOnA503IsHonoured(t *testing.T) {
	// The exact shape parseRetryAfter's comment names, end to end: a proxy in
	// front of Discord answering 503 with an HTML body and a date-form
	// Retry-After. End-to-end because two separate decisions have to hold for the
	// delay to be worth anything — the status must stay non-permanent so
	// SendWithRetry reaches a sleep at all, and the header must parse. This
	// response previously yielded 0s and the loop fell back to its base wait.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", time.Now().Add(10*time.Second).UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, "<html><body>503 Service Unavailable</body></html>")
	}))
	defer srv.Close()

	err := Send(srv.URL, &Message{Content: "x"}, 5*time.Second)
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("expected an *HTTPError on 503, got %v", err)
	}
	if he.Permanent() {
		t.Fatal("a 503 must stay retryable, or its Retry-After is never used")
	}
	if he.RetryAfter < 5*time.Second || he.RetryAfter > maxRetryAfter {
		t.Fatalf("RetryAfter = %v, want the ~10s the date-form header asked for (capped at %v)", he.RetryAfter, maxRetryAfter)
	}
}

func TestRedactToken(t *testing.T) {
	webhook := "https://discord.com/api/webhooks/" + testWebhookID + "/" + testWebhookToken

	tests := []struct {
		name       string
		body       string
		webhookURL string
		want       string
	}{
		{
			name:       "both the id and the token are redacted",
			body:       "upstream failed for /api/webhooks/" + testWebhookID + "/" + testWebhookToken,
			webhookURL: webhook,
			want:       "upstream failed for /api/webhooks/[REDACTED]/[REDACTED]",
		},
		{
			name:       "the token is redacted outside a URL too",
			body:       "denied (token=" + testWebhookToken + ")",
			webhookURL: webhook,
			want:       "denied (token=[REDACTED])",
		},
		{
			name:       "structural segments are left alone",
			body:       "GET /api/webhooks/ failed",
			webhookURL: webhook,
			want:       "GET /api/webhooks/ failed",
		},
		{
			// Nothing is lost by giving up here: a URL this malformed never got
			// past http.NewRequest, so no secret was ever put on the wire.
			name:       "an unparseable webhook URL leaves the body untouched",
			body:       "boom",
			webhookURL: "://nope",
			want:       "boom",
		},
		{
			name:       "no path segment is long enough to be a secret",
			body:       "no secrets here: /hook/abc",
			webhookURL: "https://example.test/hook/abc",
			want:       "no secrets here: /hook/abc",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactToken(tt.body, tt.webhookURL); got != tt.want {
				t.Fatalf("redactToken() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestScrubURLError(t *testing.T) {
	secretURL := "https://discord.com/api/webhooks/" + testWebhookID + "/" + testWebhookToken

	assertNoLeak := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("expected an error")
		}
		if strings.Contains(err.Error(), testWebhookToken) || strings.Contains(err.Error(), testWebhookID) {
			t.Fatalf("scrubbed error still carries the webhook URL: %v", err)
		}
	}

	t.Run("every nested layer is stripped", func(t *testing.T) {
		// A redirect that fails wraps one *url.Error inside another, so peeling
		// off a single layer would still leave the URL in the message.
		inner := &url.Error{Op: "Get", URL: secretURL, Err: errors.New("connection refused")}
		err := scrubURLError(&url.Error{Op: "Post", URL: secretURL, Err: inner})
		assertNoLeak(t, err)
		if !strings.Contains(err.Error(), "connection refused") {
			t.Fatalf("the underlying cause was lost: %v", err)
		}
	})

	t.Run("a url.Error with no cause", func(t *testing.T) {
		// A nil Err cannot be wrapped: %w would render it as the formatting
		// artifact "%!w(<nil>)", and that string is what would land in
		// service.log. The leak check alone passes either way, so assert the
		// exact message to pin what the guard is actually for.
		err := scrubURLError(&url.Error{Op: "Post", URL: secretURL})
		assertNoLeak(t, err)
		if err.Error() != "discord request failed" {
			t.Fatalf("err = %q, want a clean message with no formatting artifact", err)
		}
	})

	t.Run("a plain error is wrapped, not swallowed", func(t *testing.T) {
		cause := errors.New("dial tcp: i/o timeout")
		err := scrubURLError(cause)
		if !errors.Is(err, cause) {
			t.Fatalf("scrubURLError dropped the cause: %v", err)
		}
	})
}

func TestSend_RequestIsAPOSTOfTheMarshalledMessage(t *testing.T) {
	var raw []byte
	var method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		raw, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	msg := &Message{Username: "Restart Notifier", Content: "PC が再起動しました"}
	if err := Send(srv.URL, msg, 5*time.Second); err != nil {
		t.Fatalf("Send returned error: %v", err)
	}
	if method != http.MethodPost {
		t.Fatalf("method = %q, want POST", method)
	}
	// omitempty must keep unset fields out of the payload entirely rather than
	// sending them as empty values.
	const want = `{"username":"Restart Notifier","content":"PC が再起動しました"}`
	if string(raw) != want {
		t.Fatalf("request body = %s, want %s", raw, want)
	}
}

func TestSend_MalformedURLIsRejectedWithoutLeaking(t *testing.T) {
	// http.NewRequest fails at url.Parse and hands back a *url.Error carrying
	// the raw URL, so even this pre-flight failure has to go through scrubbing.
	err := Send("://"+testWebhookToken, &Message{Content: "x"}, time.Second)
	if err == nil {
		t.Fatal("expected an error for a malformed webhook URL")
	}
	if strings.Contains(err.Error(), testWebhookToken) {
		t.Fatalf("error leaked the webhook token: %v", err)
	}
}

func TestSendWithRetry_GivesUpAfterTheLastAttempt(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError) // 5xx: transient, always retried
	}))
	defer srv.Close()

	err := SendWithRetry(srv.URL, &Message{Content: "x"}, 3, time.Millisecond)
	if err == nil {
		t.Fatal("expected the last error to be returned")
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	var he *HTTPError
	if !errors.As(err, &he) || he.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected the final 500 to surface, got %v", err)
	}
}

func TestSendWithRetry_429WaitsForRetryAfter(t *testing.T) {
	const retryAfter = 60 * time.Millisecond
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0.06")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	// The base wait is two orders of magnitude shorter than Retry-After, so a
	// run that finishes fast is proof the server-requested delay was ignored.
	// The threshold keeps a wide margin below Retry-After because timer
	// granularity, not correctness, is what would otherwise make this flaky.
	start := time.Now()
	if err := SendWithRetry(srv.URL, &Message{Content: "x"}, 3, time.Millisecond); err != nil {
		t.Fatalf("expected success after the 429, got %v", err)
	}
	if elapsed := time.Since(start); elapsed < retryAfter/2 {
		t.Fatalf("elapsed = %v, want at least %v: Retry-After must win over the base wait", elapsed, retryAfter/2)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestSend_NaNRetryAfterHeaderIsNotADelay(t *testing.T) {
	// End-to-end guard for the header path, which is the only way a NaN can get
	// in: strconv.ParseFloat("NaN", 64) succeeds with a nil error, so before the
	// explicit check this reached time.Duration(NaN * 1e9) and surfaced here as
	// a hugely negative duration that SendWithRetry then compared against its
	// base wait.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "NaN")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	err := Send(srv.URL, &Message{Content: "x"}, 5*time.Second)
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("expected an *HTTPError on 429, got %v", err)
	}
	if he.RetryAfter != 0 {
		t.Fatalf("RetryAfter = %v, want 0", he.RetryAfter)
	}
}

func TestSend_ForbiddenResponseNeverCarriesTheCredentials(t *testing.T) {
	// A proxy may name the token outside a URL path; redaction is a substring
	// replacement precisely so that shape is covered too.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, "denied: id="+testWebhookID+" token="+testWebhookToken)
	}))
	defer srv.Close()

	err := Send(srv.URL+"/api/webhooks/"+testWebhookID+"/"+testWebhookToken, &Message{Content: "x"}, 5*time.Second)
	if err == nil {
		t.Fatal("expected an error on 403")
	}
	if strings.Contains(err.Error(), testWebhookToken) || strings.Contains(err.Error(), testWebhookID) {
		t.Fatalf("error leaked the webhook credentials: %v", err)
	}
	var he *HTTPError
	if !errors.As(err, &he) || !he.Permanent() {
		t.Fatalf("403 must be a permanent HTTPError, got %v", err)
	}
}

func TestSend_DoesNotFollowRedirects(t *testing.T) {
	// The whole failure mode this guards is silent success, so the target must
	// answer 2xx: it stands in for Discord's "Get Webhook with Token" endpoint,
	// which is what a followed redirect actually lands on once net/http has
	// rewritten the POST into a GET and dropped the body. Before CheckRedirect
	// this ran to completion with err == nil while nothing was ever posted, and
	// the caller then wrote state.json and suppressed that boot forever.
	var targetHits int
	var targetMethod, targetBody string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits++
		targetMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		targetBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	const path = "/api/webhooks/" + testWebhookID + "/" + testWebhookToken
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// http:// -> https:// on a real webhook URL is exactly this 301.
		http.Redirect(w, r, target.URL+path, http.StatusMovedPermanently)
	}))
	defer redirector.Close()

	err := Send(redirector.URL+path, &Message{Content: "x"}, 5*time.Second)
	if err == nil {
		t.Fatal("Send reported success for a 3xx response")
	}
	// The error alone would also appear if the request had been dropped for some
	// unrelated reason, so pin the redirect specifically: the hop must not have
	// happened at all.
	if targetHits != 0 {
		t.Fatalf("the redirect was followed: target hit %d time(s) with method %q and body %q",
			targetHits, targetMethod, targetBody)
	}
	var he *HTTPError
	if !errors.As(err, &he) || he.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("expected the 301 itself to surface as an *HTTPError, got %v", err)
	}
	// N1 and N2 are load-bearing for each other: without this the retry loop
	// would spend its whole budget on every boot re-issuing the same request.
	if !he.Permanent() {
		t.Fatal("a redirect must be permanent: the URL is misconfigured, not busy")
	}
	if strings.Contains(err.Error(), testWebhookToken) {
		t.Fatalf("error leaked the webhook token: %v", err)
	}
}

func TestSend_RefusesBodyPreservingRedirectsToo(t *testing.T) {
	// 307 and 308 are the redirects net/http does not rewrite: the POST and its
	// body survive the hop, so unlike the 301 above these used to be delivered
	// correctly. Refusing them is a deliberate trade, not a bug fix — the hop
	// points at a host the configuration never named, and following it would
	// forward the token-bearing POST there. Pinned so the trade is not quietly
	// undone by narrowing the hook to the body-dropping codes.
	for _, code := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var targetHits int
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				targetHits++
				w.WriteHeader(http.StatusNoContent)
			}))
			defer target.Close()
			redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL, code)
			}))
			defer redirector.Close()

			err := Send(redirector.URL, &Message{Content: "x"}, 5*time.Second)
			var he *HTTPError
			if !errors.As(err, &he) || he.StatusCode != code {
				t.Fatalf("expected the %d itself to surface as an *HTTPError, got %v", code, err)
			}
			if !he.Permanent() {
				t.Fatalf("%d must be permanent: the host said \"not here\", which retrying does not change", code)
			}
			if targetHits != 0 {
				t.Fatalf("the token-bearing POST was forwarded to the redirect target (%d hit(s))", targetHits)
			}
		})
	}
}

func TestSendWithRetry_RedirectIsNotRetried(t *testing.T) {
	// End-to-end consequence of N1+N2 at the call site main actually uses.
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Redirect(w, r, "https://example.test/elsewhere", http.StatusFound)
	}))
	defer srv.Close()

	if err := SendWithRetry(srv.URL, &Message{Content: "x"}, 5, time.Millisecond); err == nil {
		t.Fatal("expected an error on a 302")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (a redirect is permanent, not transient)", calls)
	}
}

func TestSendWithRetry_MalformedURLIsNotRetried(t *testing.T) {
	// A URL http.NewRequest rejects never reaches a server, so elapsed time is
	// the only observable for "did the loop retry": with the fix nothing sleeps,
	// without it three waits go by.
	//
	// The wait is deliberately huge and the threshold deliberately tiny. CI runs
	// on windows-latest, whose default timer granularity is ~15.6ms on a
	// contended runner, so a threshold anywhere near a few tens of milliseconds
	// is a future flaky red build. Here a regression sleeps 30s and a pass takes
	// microseconds; the 1s threshold has three orders of magnitude of headroom in
	// both directions. The test still costs nothing to run, because the passing
	// path never sleeps at all.
	const (
		wait      = 10 * time.Second
		threshold = time.Second
	)
	start := time.Now()
	err := SendWithRetry("://"+testWebhookToken, &Message{Content: "x"}, 4, wait)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error for a malformed webhook URL")
	}
	if elapsed >= threshold {
		t.Fatalf("elapsed = %v: the retry loop slept on a deterministically fatal input", elapsed)
	}
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want it to carry ErrInvalidRequest so the loop can stop", err)
	}
	// Marking the error must not undo the scrubbing that motivated it.
	if strings.Contains(err.Error(), testWebhookToken) {
		t.Fatalf("error leaked the webhook token: %v", err)
	}
	// The cause has to survive alongside the sentinel, or the log says nothing
	// about which part of the URL was wrong.
	if !strings.Contains(err.Error(), "missing protocol scheme") {
		t.Fatalf("err = %v, want the underlying parse failure to survive", err)
	}
}

func TestSendWithRetry_UnusableSchemeOrHostIsNotRetried(t *testing.T) {
	// The sibling of the test above, covering the likelier typo: these URLs all
	// parse, so http.NewRequest accepts them and only client.Do rejects them —
	// where the failure arrives as an ordinary transport error and is retried
	// through the entire budget (12 attempts a 15s at the boot-time call site in
	// main). Nothing is dialled in any of these cases, so the elapsed-time
	// observable of the test above applies unchanged, threshold and all.
	const (
		wait      = 10 * time.Second
		threshold = time.Second
	)
	tests := []struct {
		name string
		url  string
	}{
		{"a webhook URL that forgot https://", "discord.com/api/webhooks/" + testWebhookID + "/" + testWebhookToken},
		{"a scheme http.Transport has no dialler for", "ftp://discord.com/api/webhooks/" + testWebhookID + "/" + testWebhookToken},
		{"a URL with no host at all", "https:///api/webhooks/" + testWebhookID + "/" + testWebhookToken},
		// url.URL.Host is ":443" here — non-empty — so a Host check would let this
		// through while config's Hostname check rejects it. The two packages must
		// agree, or a direct notify caller gets the retry storm config prevents.
		{"a port with no host in front of it", "https://:443/api/webhooks/" + testWebhookID + "/" + testWebhookToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := time.Now()
			err := SendWithRetry(tt.url, &Message{Content: "x"}, 4, wait)
			elapsed := time.Since(start)
			if err == nil {
				t.Fatal("expected an error for a webhook URL that cannot be dialled")
			}
			if elapsed >= threshold {
				t.Fatalf("elapsed = %v: the retry loop slept on a deterministically fatal input", elapsed)
			}
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err = %v, want it to carry ErrInvalidRequest so the loop can stop", err)
			}
			if strings.Contains(err.Error(), testWebhookToken) {
				t.Fatalf("error leaked the webhook token: %v", err)
			}
		})
	}
}

func TestSendWithRetry_TransportFailureIsStillRetried(t *testing.T) {
	// The counterpart to the two tests above, and the reason ErrInvalidRequest
	// marks only the pre-flight paths: a connection that fails because the
	// network is not up yet is precisely what the retry budget exists for, and
	// must keep being retried.
	//
	// Elapsed time is the observable here too, in the opposite direction: a dial
	// to a closed port fails instantly, so the only thing that can put time on
	// the clock is the loop waiting between attempts. The bound is a lower one
	// and time.Sleep never returns early, so it cannot flake. Asserting merely
	// that the error is not ErrInvalidRequest would not pin anything — a
	// SendWithRetry that had dropped retry-on-transport-failure entirely
	// satisfies that too, and this is the one property no other test covers
	// (every other retry test drives *HTTPError paths through a live server).
	const (
		attempts = 3
		wait     = 20 * time.Millisecond
	)
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	}))
	srv.Close() // nothing is listening: every attempt fails at dial

	start := time.Now()
	err := SendWithRetry(srv.URL, &Message{Content: "x"}, attempts, wait)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a dial failure must not be marked as an unusable request: %v", err)
	}
	if elapsed < (attempts-1)*wait {
		t.Fatalf("elapsed = %v, want at least %v: a dial failure is the network not being up yet at boot, which is what the retry budget exists for",
			elapsed, (attempts-1)*wait)
	}
	if calls != 0 {
		t.Fatalf("calls = %d: the closed server answered", calls)
	}
}

func TestSend_IdentifiesItselfWithADiscordBotUserAgent(t *testing.T) {
	// Discord documents "DiscordBot ($url, $versionNumber)" as required and warns
	// that unidentified clients may be blocked with a Cloudflare error — a 403,
	// which Permanent() would treat as fatal on the first attempt everywhere at
	// once. Go's default "Go-http-client/1.1" is what was being sent.
	var ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := Send(srv.URL, &Message{Content: "x"}, 5*time.Second); err != nil {
		t.Fatalf("Send returned error: %v", err)
	}
	if !strings.HasPrefix(ua, "DiscordBot (") {
		t.Fatalf("User-Agent = %q, want a Discord-documented \"DiscordBot ($url, $versionNumber)\" identity", ua)
	}
	if ua != UserAgent {
		t.Fatalf("User-Agent = %q, want the package value %q", ua, UserAgent)
	}
}

func TestSetUserAgentVersion(t *testing.T) {
	original := UserAgent
	t.Cleanup(func() { UserAgent = original })

	t.Run("the default identifies the project without a hard-coded release", func(t *testing.T) {
		// The version lives only in main.Version; a literal copy here would be a
		// second source of truth that goes stale at every release.
		if !strings.HasPrefix(original, "DiscordBot (https://github.com/223n/restart-message, ") {
			t.Fatalf("default UserAgent = %q", original)
		}
	})

	t.Run("a version supplied by main is used", func(t *testing.T) {
		SetUserAgentVersion("1.2.3")
		if UserAgent != "DiscordBot (https://github.com/223n/restart-message, 1.2.3)" {
			t.Fatalf("UserAgent = %q", UserAgent)
		}
	})

	t.Run("an empty version keeps the default", func(t *testing.T) {
		// main.Version is empty in a build that forgot the -ldflags stamp; that
		// must not produce "DiscordBot (url, )".
		UserAgent = original
		SetUserAgentVersion("")
		if UserAgent != original {
			t.Fatalf("UserAgent = %q, want the default %q", UserAgent, original)
		}
	})
}

func TestHTTPError_Permanent(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   bool
	}{
		{"301 misconfigured URL is permanent", http.StatusMovedPermanently, true},
		{"302 captive portal is permanent", http.StatusFound, true},
		{"400 invalid payload", http.StatusBadRequest, true},
		{"401 bad token", http.StatusUnauthorized, true},
		{"404 deleted webhook", http.StatusNotFound, true},
		{"429 rate limit is retryable", http.StatusTooManyRequests, false},
		{"500 is retryable", http.StatusInternalServerError, false},
		{"502 is retryable", http.StatusBadGateway, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			he := &HTTPError{StatusCode: tt.status}
			if got := he.Permanent(); got != tt.want {
				t.Fatalf("HTTPError{%d}.Permanent() = %v, want %v", tt.status, got, tt.want)
			}
		})
	}
}

// The boot-time call site and the deadline it has to fit inside. main.go calls
// SendWithRetry(..., 12, 15*time.Second); task_windows.go registers that command
// under a task carrying <ExecutionTimeLimit>PT10M</ExecutionTimeLimit> with
// AllowHardTerminate=true; and cmdRun spends up to ~25s in collect(6) before the
// first attempt is even made. All three live in //go:build windows files this
// package cannot import, so they are restated here — which is the point of the
// test below rather than a flaw in it: if one of them moves and maxRetryAfter is
// not revisited, a failure here is how that gets noticed, instead of a boot-time
// run Windows kills at the ten-minute mark having delivered nothing.
//
// The last two are a floor, not a preference. task_windows.go reaches 595s by
// this same sum and calls it "just inside PT10M", which is true and is the
// weaker claim: it asks only whether the arithmetic fits, and answers yes for a
// maxRetryAfter as large as 30s. Subtracting them here asks the stricter
// question, because 595 of 600 leaves five seconds for six wevtapi queries that
// have no timeout of their own, plus process start, config load and the state
// file — rounding error rather than margin. So when the test below goes red,
// lower maxRetryAfter; growing bootBudgetMargin or bootPreSendCost to make it
// green restores exactly the hard termination this guard exists to prevent.
const (
	bootAttempts       = 12
	bootWait           = 15 * time.Second
	taskExecutionLimit = 10 * time.Minute
	bootPreSendCost    = 30 * time.Second // collect(6): five 5s sleeps, plus the queries
	bootBudgetMargin   = 60 * time.Second // a machine one minute into boot is slow and contended
)

func TestSendWithRetry_WorstCaseFitsTheScheduledTaskLimit(t *testing.T) {
	// Deliberately not "maxRetryAfter == 20s". The constant's value is not the
	// property worth protecting; the bound it produces is. Raising the attempt
	// count or the base wait at the call site breaks the same invariant without
	// touching the constant at all, and fails this test just as loudly.
	//
	// What no test executes is a sleep of maxRetryAfter length inside the real
	// loop: that costs 20s of suite time for a single assertion. So this bound is
	// arithmetic over pieces that are each executed elsewhere —
	// TestSendWithRetry_DoesNotSleepAfterTheFinalAttempt pins the (attempts-1)
	// sleep count and that a server-requested delay is what gets slept, while
	// TestParseRetryAfter and TestClampRetryAfter pin that no response can drive
	// that delay above maxRetryAfter.
	ceiling := taskExecutionLimit - bootPreSendCost - bootBudgetMargin

	t.Run("every attempt answered instantly with a maximal Retry-After", func(t *testing.T) {
		// The shape that was over the limit on its own, and how this was found:
		// the server costs nothing, so the whole run is sleeping.
		got := (bootAttempts - 1) * maxRetryAfter
		if got > ceiling {
			t.Fatalf("%d sleeps of %v = %v, over the %v ceiling (%v limit - %v collect - %v margin)",
				bootAttempts-1, maxRetryAfter, got, ceiling, taskExecutionLimit, bootPreSendCost, bootBudgetMargin)
		}
	})

	t.Run("every attempt hitting the per-attempt timeout instead", func(t *testing.T) {
		// A black hole rather than a rude server: no response arrives, so there is
		// no Retry-After and the gaps are the base wait. This shape does not depend
		// on maxRetryAfter — it is here because it is the other half of the answer
		// to "how long can this run", and it is already 405s.
		got := bootAttempts*sendTimeout + (bootAttempts-1)*bootWait
		if got > ceiling {
			t.Fatalf("%d timeouts of %v plus %d waits of %v = %v, over the %v ceiling",
				bootAttempts, sendTimeout, bootAttempts-1, bootWait, got, ceiling)
		}
	})

	t.Run("both at once, which is the bound the cap is chosen from", func(t *testing.T) {
		// Nothing stops a server from answering just inside the timeout and asking
		// for the maximum in the same response, so the real bound is the sum of the
		// two shapes above rather than the worse of them.
		got := WorstCaseDuration(bootAttempts, bootWait)
		if got > ceiling {
			t.Fatalf("WorstCaseDuration(%d, %v) = %v, over the %v ceiling: the boot task is hard-terminated at %v, so maxRetryAfter (%v) is too large for this call site. Lower maxRetryAfter — raising bootBudgetMargin/bootPreSendCost to fit is the repair that re-creates the hard termination",
				bootAttempts, bootWait, got, ceiling, taskExecutionLimit, maxRetryAfter)
		}
	})
}

func TestWorstCaseDuration_DegenerateAttemptCounts(t *testing.T) {
	// A caller passing 0 or 1 must not produce a negative bound out of the
	// (attempts-1) term, which would read as "this can never run long".
	tests := []struct {
		name     string
		attempts int
		wait     time.Duration
		want     time.Duration
	}{
		{"no attempts at all runs the loop body zero times", 0, time.Millisecond, 0},
		{"a negative count is the same as none", -1, time.Millisecond, 0},
		{"a single attempt is never followed by a sleep", 1, time.Millisecond, sendTimeout},
		{"the cap wins when the base wait is shorter", 2, time.Millisecond, 2*sendTimeout + maxRetryAfter},
		{"the base wait wins when it exceeds the cap", 2, time.Hour, 2*sendTimeout + time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := WorstCaseDuration(tt.attempts, tt.wait); got != tt.want {
				t.Fatalf("WorstCaseDuration(%d, %v) = %v, want %v", tt.attempts, tt.wait, got, tt.want)
			}
		})
	}
}

func TestSendWithRetry_DoesNotSleepAfterTheFinalAttempt(t *testing.T) {
	// The (attempts-1) in the worst-case arithmetic is a claim about this loop, so
	// pin it against the loop rather than trusting the formula to describe it. One
	// stray sleep after the last attempt turns the bound into attempts*sleep, which
	// at the boot call site is another 20s of a budget already being counted.
	//
	// Total elapsed time is a poor observable for that one sleep: on a contended
	// windows-latest runner (~15.6ms timer granularity) the slack accumulated over
	// the other sleeps swamps it. The gap between the LAST request and the return
	// is not — it is microseconds when the loop breaks before sleeping and a full
	// delay when it does not, with no accumulated error in it either way.
	const (
		attempts = 3
		delay    = 200 * time.Millisecond
	)
	var calls int
	var lastRequest time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		lastRequest = time.Now()
		// 429 is retryable, so the whole budget is spent — and it is the status
		// whose Retry-After the boot path can actually be handed.
		w.Header().Set("Retry-After", "0.2")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	// The base wait is two orders of magnitude below the Retry-After, so the gaps
	// below can only have come from the server-requested delay.
	start := time.Now()
	err := SendWithRetry(srv.URL, &Message{Content: "x"}, attempts, time.Millisecond)
	elapsed := time.Since(start)
	tail := time.Since(lastRequest)

	if err == nil {
		t.Fatal("expected the final 429 to be returned")
	}
	if calls != attempts {
		t.Fatalf("calls = %d, want %d", calls, attempts)
	}
	// time.Sleep never returns early, so this lower bound cannot flake. It says the
	// gaps really were the Retry-After rather than the 1ms base wait, which is the
	// half of the formula the sleep-count assertion below does not cover.
	if want := (attempts - 1) * delay; elapsed < want {
		t.Fatalf("elapsed = %v, want at least %v: %d sleeps of the server-requested delay", elapsed, want, attempts-1)
	}
	if tail > delay/2 {
		t.Fatalf("SendWithRetry returned %v after its last request: the loop slept after the final attempt, making the worst case attempts*sleep instead of (attempts-1)*sleep", tail)
	}
}
