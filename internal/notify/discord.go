// Package notify sends messages to a Discord channel via an incoming webhook.
package notify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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

// Permanent reports whether retrying cannot help: a 4xx other than 429, or any
// 3xx.
//
// The 3xx case is load-bearing together with the CheckRedirect hook in Send,
// and neither half works alone. Send refuses to follow redirects, so a 3xx now
// surfaces here as an HTTPError instead of being silently followed; without
// this clause it would count as retryable and SendWithRetry would burn its
// whole budget, on every boot, against a webhook URL that is misconfigured
// permanently (wrong scheme, a proxy bouncing to a captive portal). A redirect
// is the server saying "not here", which no amount of retrying changes.
func (e *HTTPError) Permanent() bool {
	if e.StatusCode >= 300 && e.StatusCode < 400 {
		return true
	}
	return e.StatusCode >= 400 && e.StatusCode < 500 && e.StatusCode != http.StatusTooManyRequests
}

// ErrInvalidRequest marks a failure that happened before anything went on the
// wire — the webhook URL could not even be turned into a request (malformed,
// unsupported scheme, a control character in it). Such an input is fatal
// deterministically: the boot-time retry loop would otherwise sleep through its
// entire budget re-parsing the same broken string, which is exactly the wait it
// exists to spend on a network that is not up yet.
//
// It is a sentinel rather than another Permanent()-style type because the
// condition carries nothing worth inspecting: unlike *HTTPError there is no
// status, no Retry-After, no body — only "this can never succeed" — and
// errors.Is keeps the check next to the existing *HTTPError one in
// SendWithRetry without giving callers a second error shape to learn.
var ErrInvalidRequest = errors.New("invalid webhook request")

// userAgentURL identifies this project to Discord, per its documented
// "DiscordBot ($url, $versionNumber)" format.
const userAgentURL = "https://github.com/223n/restart-message"

// UserAgent is sent with every webhook request. Discord's developer
// documentation requires clients to identify themselves this way and warns that
// unidentified ones may be blocked with a Cloudflare error — a 403, which
// Permanent() treats as unretryable, so both notification paths would die after
// a single attempt on every machine at once.
//
// The version component defaults to a placeholder rather than a literal release
// number on purpose: the version lives in exactly one place in this repository
// (main.Version), and main is Windows-only, so a copy here would be a second
// source of truth that silently goes stale. SetUserAgentVersion exists so that
// main — the one caller that knows the number — can fill it in; until it does,
// the default is still a valid identification on its own.
var UserAgent = "DiscordBot (" + userAgentURL + ", 0)"

// SetUserAgentVersion refines UserAgent with the running binary's version. It is
// meant to be called once at startup, before any Send.
func SetUserAgentVersion(version string) {
	if version == "" {
		return
	}
	UserAgent = "DiscordBot (" + userAgentURL + ", " + version + ")"
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
		// Nothing was sent, so no retry can change the outcome. Still scrubbed:
		// url.Parse's error stringifies the URL it choked on, token included.
		return fmt.Errorf("%w: %w", ErrInvalidRequest, scrubURLError(err))
	}
	if (req.URL.Scheme != "http" && req.URL.Scheme != "https") || req.URL.Hostname() == "" {
		// http.NewRequest accepts anything url.Parse accepts, so a webhook URL
		// written without "https://" gets this far and is only rejected deep inside
		// client.Do ("unsupported protocol scheme", "no Host in request URL").
		// There it looks like an ordinary transport error and the retry loop spends
		// its whole budget on it. It is as deterministic as the parse failure above
		// and no byte ever leaves the machine, so it stops the loop the same way.
		//
		// config.Load screens the same shapes before this package is reached, so
		// for the binary this is the second of two gates rather than the only one.
		// It is still this package's job: notify is importable on its own, and a
		// caller that skips config must not be handed a 3-minute stall instead of
		// an immediate answer. Hostname() rather than Host, to agree with config —
		// "https://:443/…" has a non-empty Host and no host at all.
		// The URL is not quoted back: it carries the token.
		return fmt.Errorf("%w: webhook URL needs an http:// or https:// scheme and a host", ErrInvalidRequest)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)

	client := &http.Client{
		Timeout: timeout,
		// Never follow a redirect; hand the 3xx back as the response instead.
		//
		// net/http otherwise follows up to 10 hops, and its redirect handling
		// rewrites a 301/302/303 POST into a GET and DROPS the body. The shape
		// that made this concrete: a webhook URL written as http://, to which
		// Discord answers 301 for the https one — and a GET on that path is its
		// documented "Get Webhook with Token" endpoint, returning 200 with the
		// webhook object. Send saw 2xx, reported success and posted nothing,
		// while main wrote state.json and suppressed that boot forever.
		//
		// config.Load now rejects a non-https webhook URL, so that particular
		// route no longer starts here. What remains is every redirect this tool
		// does not control: a TLS-terminating proxy answering 200 with a block
		// page, a captive portal 302, a middlebox in front of an https endpoint.
		// Surfacing the 3xx turns all of them into a visible permanent error
		// (see HTTPError.Permanent) instead of a success that delivered nothing.
		//
		// 307 and 308 are refused as well, and that half is a deliberate trade
		// rather than a bug fix: they are the two redirects net/http does not
		// rewrite, so the POST and its body survive the hop and a webhook behind
		// a 308-issuing proxy used to be delivered correctly. Following one
		// would forward a request carrying the webhook token to a host the
		// configuration never named, on the say-so of whoever answered.
		// Refusing every hop keeps the token inside the configured origin, and
		// the error names the status so the operator can repoint the config at
		// the redirect target themselves.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
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
// boot. It stops immediately on a permanent error (an unusable webhook URL, a
// redirect, 401/404 bad webhook, 400 invalid payload) and honors a 429
// Retry-After delay.
func SendWithRetry(webhookURL string, msg *Message, attempts int, wait time.Duration) error {
	var last error
	for i := 0; i < attempts; i++ {
		last = Send(webhookURL, msg, 20*time.Second)
		if last == nil {
			return nil
		}
		if errors.Is(last, ErrInvalidRequest) {
			return last // never reached the network; the input itself is unusable
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
//
// NaN must be rejected explicitly, before the bounds: it compares false against
// both of them, so it would otherwise reach time.Duration(NaN * 1e9) — a
// float-to-integer conversion the Go spec leaves undefined, which on amd64
// yields the most negative int64. It is reachable from the wire rather than
// merely theoretical, because strconv.ParseFloat("NaN", 64) succeeds with a nil
// error: a hostile responder or a broken TLS-terminating proxy need only send
// "Retry-After: NaN". Only the header can carry it — the JSON body path is safe
// because encoding/json rejects NaN, which is not valid JSON.
//
// The infinities need no case of their own: -Inf is caught by the lower bound
// and +Inf by the upper one, so both already clamp to 0 and maxRetryAfter.
func clampRetryAfter(secs float64) time.Duration {
	if math.IsNaN(secs) || secs <= 0 {
		return 0
	}
	if secs > maxRetryAfter.Seconds() {
		return maxRetryAfter
	}
	return time.Duration(secs * float64(time.Second))
}
