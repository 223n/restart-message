// Package notify sends messages to a Discord channel via an incoming webhook.
package notify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Message is a Discord webhook payload (subset of the documented fields).
type Message struct {
	Username  string  `json:"username,omitempty"`
	AvatarURL string  `json:"avatar_url,omitempty"`
	Content   string  `json:"content,omitempty"`
	Embeds    []Embed `json:"embeds,omitempty"`
}

// Embed is a Discord rich embed.
type Embed struct {
	Title       string       `json:"title,omitempty"`
	Description string       `json:"description,omitempty"`
	Color       int          `json:"color,omitempty"`
	Fields      []EmbedField `json:"fields,omitempty"`
	Footer      *EmbedFooter `json:"footer,omitempty"`
	Timestamp   string       `json:"timestamp,omitempty"`
}

// EmbedField is one name/value row in an embed.
type EmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

// EmbedFooter is the small footer line of an embed.
type EmbedFooter struct {
	Text string `json:"text"`
}

// HTTPError is returned when the webhook responds with a non-2xx status. It
// never includes the webhook URL: the status and the response body are kept,
// but any secret path segment echoed back inside that body is redacted.
type HTTPError struct {
	StatusCode int
	RetryAfter time.Duration // server-requested wait (0 if none)
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("discord webhook returned %d: %s", e.StatusCode, e.Body)
}

// Permanent reports whether retrying cannot help (4xx other than 429).
func (e *HTTPError) Permanent() bool {
	return e.StatusCode >= 400 && e.StatusCode < 500 && e.StatusCode != http.StatusTooManyRequests
}

// Send posts msg to the webhook once. Discord returns 204 on success.
//
// On a transport-level failure the returned error is scrubbed of the webhook
// URL, because Go's *url.Error stringifies the full URL — and the Discord
// webhook URL embeds a secret token in its path.
func Send(webhookURL string, msg *Message, timeout time.Duration) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return scrubURLError(err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return scrubURLError(err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{
			StatusCode: resp.StatusCode,
			RetryAfter: parseRetryAfter(resp, respBody),
			Body:       redactToken(string(respBody), webhookURL),
		}
	}
	return nil
}

// SendWithRetry retries Send to tolerate the network not being ready yet at
// boot. It stops immediately on a permanent error (e.g. 401/404 bad webhook,
// 400 invalid payload) and honors a 429 Retry-After delay.
func SendWithRetry(webhookURL string, msg *Message, attempts int, wait time.Duration) error {
	var last error
	for i := 0; i < attempts; i++ {
		last = Send(webhookURL, msg, 20*time.Second)
		if last == nil {
			return nil
		}
		var he *HTTPError
		if errors.As(last, &he) && he.Permanent() {
			return last // retrying cannot help
		}
		if i == attempts-1 {
			break
		}
		sleep := wait
		if errors.As(last, &he) && he.RetryAfter > sleep {
			sleep = he.RetryAfter
		}
		time.Sleep(sleep)
	}
	return last
}

// scrubURLError strips every *url.Error layer (each of whose message includes
// the full request URL) down to the underlying cause, so the secret token in
// the webhook URL can never leak — even through nested redirect-error chains.
func scrubURLError(err error) error {
	for {
		var ue *url.Error
		if !errors.As(err, &ue) {
			break
		}
		if ue.Err == nil {
			return fmt.Errorf("discord request failed")
		}
		err = ue.Err
	}
	return fmt.Errorf("discord request failed: %w", err)
}

// minSecretSegment is the shortest webhook path segment treated as secret.
// Discord's webhook id (17-19 digits) and token (~68 characters) both clear it,
// while the structural "api" and "webhooks" segments do not.
const minSecretSegment = 16

// redactToken removes the webhook URL's secret path segments from a response
// body before that body is stored in an error.
//
// Discord itself does not echo the request URL back, but whoever answers is not
// necessarily Discord. A TLS-terminating proxy on the path can return its own
// error page with the full request URL embedded, and the service writes that
// body to %ProgramData%\restart-message\service.log. README states without
// qualification that the token never reaches logs or error output, so the
// guarantee is enforced here rather than left to depend on who replied.
func redactToken(body, webhookURL string) string {
	u, err := url.Parse(webhookURL)
	if err != nil {
		return body
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if len(seg) >= minSecretSegment {
			body = strings.ReplaceAll(body, seg, "[REDACTED]")
		}
	}
	return body
}

// maxRetryAfter caps how long a server-provided 429 delay may make us sleep.
const maxRetryAfter = 60 * time.Second

// parseRetryAfter reads the Retry-After header (seconds) or the JSON body's
// retry_after field (seconds) from a 429 response, clamped to a sane maximum.
func parseRetryAfter(resp *http.Response, body []byte) time.Duration {
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.ParseFloat(v, 64); err == nil {
			return clampRetryAfter(secs)
		}
	}
	var parsed struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if json.Unmarshal(body, &parsed) == nil {
		return clampRetryAfter(parsed.RetryAfter)
	}
	return 0
}

// clampRetryAfter bounds a server-provided delay (in seconds) to [0, maxRetryAfter],
// so a hostile or buggy response can't overflow the conversion or make the
// boot-time notifier sleep for an absurd duration.
func clampRetryAfter(secs float64) time.Duration {
	if secs <= 0 {
		return 0
	}
	if secs > maxRetryAfter.Seconds() {
		return maxRetryAfter
	}
	return time.Duration(secs * float64(time.Second))
}
