package notify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	url := "http://nonexistent.invalid.example/api/webhooks/123456789/" + secret
	err := Send(url, &Message{Content: "x"}, 2*time.Second)
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
