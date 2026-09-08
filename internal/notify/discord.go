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

// sendTimeout bounds a single attempt inside SendWithRetry — the whole exchange,
// connect through body read, since that is what http.Client.Timeout covers.
//
// It is a named constant rather than a literal because the retry loop's
// worst-case duration is computed from it (WorstCaseDuration) against a hard
// deadline set outside this package; that arithmetic has to track the real
// value, not a copy of it.
const sendTimeout = 20 * time.Second

// SendWithRetry retries Send to tolerate the network not being ready yet at
// boot. It stops immediately on a permanent error (an unusable webhook URL, a
// redirect, 401/404 bad webhook, 400 invalid payload) and honors a
// server-requested Retry-After delay, clamped to maxRetryAfter.
//
// The loop sleeps between attempts but never after the last one, so a call costs
// at most attempts*sendTimeout + (attempts-1)*max(wait, maxRetryAfter). That
// bound is not academic: see maxRetryAfter for the deadline it has to fit inside.
func SendWithRetry(webhookURL string, msg *Message, attempts int, wait time.Duration) error {
	var last error
	for i := 0; i < attempts; i++ {
		last = Send(webhookURL, msg, sendTimeout)
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

// maxRetryAfter caps how long a server-provided delay may make SendWithRetry
// sleep between attempts. It is a scheduling constraint, not a politeness knob:
// the value is derived from the budget the boot-time notifier runs inside.
//
//	worst case = attempts*sendTimeout + (attempts-1)*max(wait, maxRetryAfter)
//
// The boot path is the `run` command under the scheduled task registered by
// task_windows.go, whose <ExecutionTimeLimit>PT10M</ExecutionTimeLimit> together
// with AllowHardTerminate=true gives the whole process 600s before Windows kills
// it mid-flight — after which nothing is delivered and nothing records why. main
// calls SendWithRetry(..., 12, 15*time.Second), and cmdRun spends up to ~25s in
// collect(6) before ever reaching it. At 20s the arithmetic is:
//
//	12*20s + 11*max(15s, 20s) = 460s, plus ~25s of collect = 485s — 115s of margin.
//
// At the previous 60s the same worst case was 12*20s + 11*60s = 900s, and a
// server answering instantly with "Retry-After: 60" still cost 11*60s = 660s: over
// the limit on the sleeps alone, which is how this was found.
//
// Three of those numbers live outside this package: the attempt count and the
// base wait at main.go's SendWithRetry call site, and the task's
// ExecutionTimeLimit in task_windows.go. Raising any of them without redoing this
// arithmetic is what the budget test in discord_test.go exists to catch.
//
// That test measures against a ceiling deliberately stricter than the bare 600s:
// the limit, minus ~30s for collect(6), minus a 60s discretionary margin — 510s.
// The strictness is the point, so read the next paragraph before touching either
// subtrahend, because relaxing them is the one repair that quietly undoes this.
//
// task_windows.go works the same sum from its own end and reaches
// 25 + 240 + 11*30 = 595s, "just inside PT10M". Both figures are right; they
// answer different questions. 595 <= 600 answers "does the arithmetic fit at
// all", and by that test 30s is the largest cap admissible. This constant answers
// the stricter one: 595 of 600 leaves five seconds for six wevtapi queries that
// carry no timeout of their own, on a machine one minute into boot, plus process
// start, config load and the state file. Five seconds is rounding error, not
// margin. So a cap of 30s is arithmetically legal and still rejected here on
// purpose — and when the budget test goes red the fix is this constant, never a
// bigger bootBudgetMargin.
//
// There is deliberately no override for this cap. Anyone able to raise it could
// re-create exactly the hard termination it prevents, and nothing legitimate
// needs it: Discord's webhook bucket is 5 requests per 2 seconds and this tool
// sends one message per boot, so the retry_after values it can actually earn sit
// far below 20s. A server demanding more is asking for a wait that no longer fits
// in the task at all — the honest fix there is to raise the task's time limit and
// this cap together, which is the pair named above.
const maxRetryAfter = 20 * time.Second

// WorstCaseDuration reports the longest wall-clock time a SendWithRetry call can
// take for a given attempt count and base wait: every attempt burning its full
// timeout, every gap using the longest sleep a server is able to ask for. JSON
// marshalling and request setup are microseconds and ignored.
//
// It exists so that the budget written out in maxRetryAfter's comment is a
// checked claim rather than prose that quietly goes stale — discord_test.go
// asserts it against the scheduled task's execution limit. Keep it in step with
// the loop in SendWithRetry: the (attempts-1) is that loop's "no sleep after the
// final attempt", and the max() is its Retry-After-beats-base-wait rule.
//
// It is exported for one reason: three of the numbers that decide whether the
// bound holds live in //go:build windows files this package cannot import, so
// discord_test.go restates them by hand, and a hand-copy that drifts *smaller*
// than reality would leave that test green while a real boot run overruns and is
// hard-terminated. Nothing in this package can catch that. The windows-tagged
// TestBootRunFitsTheScheduledTaskLimit in package main does: it reads main.go's
// own bootSendAttempts/bootSendWait and task_windows.go's own
// executionTimeLimit, and CI runs it on windows-latest. This export is what lets
// that test reuse the formula instead of copying it next to the constants —
// which would put the drift back, one level down.
func WorstCaseDuration(attempts int, wait time.Duration) time.Duration {
	if attempts <= 0 {
		return 0
	}
	sleep := wait
	if maxRetryAfter > sleep {
		sleep = maxRetryAfter
	}
	return time.Duration(attempts)*sendTimeout + time.Duration(attempts-1)*sleep
}

// parseRetryAfter reads the Retry-After header — in either form RFC 9110 §10.2.3
// defines, delta-seconds or an HTTP-date — or else the JSON body's retry_after
// field (seconds), clamped to maxRetryAfter.
//
// The delay is taken from any non-2xx response rather than from a 429 alone, and
// that stays deliberate. Permanent() ends the loop for every 3xx and every 4xx
// but 429, so in practice the statuses whose delay reaches a time.Sleep are 429
// and 5xx — and RFC 9110 defines Retry-After for 503 in the same terms as for
// 429. A Cloudflare or proxy 503 in front of Discord carrying a delay is a real
// signal about when the endpoint returns; narrowing this to 429 would discard it
// and buy no safety, because the clamp rather than the status check is what
// bounds what a hostile value can do. Permanent() screens neither 1xx nor
// anything above 599, so those reach the sleep as well — and are bounded by that
// same clamp, which is the point: the ceiling does not depend on enumerating
// statuses correctly.
func parseRetryAfter(resp *http.Response, body []byte) time.Duration {
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.ParseFloat(v, 64); err == nil {
			return clampRetryAfter(secs)
		}
		// The date form, which is the one CDNs and reverse proxies most often
		// emit — i.e. exactly the 503-in-front-of-Discord responder this reads a
		// non-429 delay for at all. Without this branch that header parses as
		// nothing and the loop silently falls back to its base wait.
		//
		// The result needs no bound of its own because it goes through the same
		// clamp: a date far in the future caps at maxRetryAfter, and one already
		// past floors to 0. That floor is what makes this safe on the machine
		// this tool actually runs on — one minute into boot, quite possibly
		// before w32time has synced, so a local clock running ahead of the
		// server's turns the delay negative rather than into a wait.
		//
		// Like the delta-seconds branch it returns rather than falling through,
		// so a date the server sent wins over a retry_after in the body; that is
		// the same header-beats-body precedence the numeric form already had.
		if t, err := http.ParseTime(v); err == nil {
			return clampRetryAfter(time.Until(t).Seconds())
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
