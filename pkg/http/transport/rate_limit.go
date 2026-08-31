package transport

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/github/github-mcp-server/pkg/http/headers"
)

const (
	// defaultRateLimitMaxTokens bounds how many distinct tokens the transport
	// tracks rate-limit state for. The key is derived from a caller-supplied
	// Authorization header, so this map MUST be bounded: an unbounded one is a
	// memory-exhaustion vector on a multi-tenant server.
	defaultRateLimitMaxTokens = 256

	// defaultRateLimitMaxRetries bounds how many times a single request is
	// replayed after a throttling response. Zero disables replay entirely.
	defaultRateLimitMaxRetries = 2

	// defaultRateLimitMaxWait bounds how long the transport will block. GitHub's
	// primary window is an hour and Retry-After can name a delay far beyond any
	// reasonable tool-call timeout, so past this bound the transport stops
	// waiting and lets the caller see the real throttling response — a clear
	// error beats an invisible multi-minute stall.
	defaultRateLimitMaxWait = 60 * time.Second

	// maxDrainBytes bounds how much of a superseded response body is read before
	// Close in order to make its connection reusable. GitHub's throttling bodies
	// are a few hundred bytes; this is generous for a legitimate one and refuses
	// to be held open by anything larger.
	maxDrainBytes = 64 << 10 // 64 KiB
)

// Rate-limit response headers. GitHub documents these on every REST and GraphQL
// response; see "Rate limits for the REST API".
const (
	rateLimitRemainingHeader = "X-RateLimit-Remaining"
	rateLimitResetHeader     = "X-RateLimit-Reset"
	retryAfterHeader         = "Retry-After"
)

// tokenState is the last observed rate-limit state for one token.
type tokenState struct {
	// remaining is the value of X-RateLimit-Remaining on the most recent
	// response, or -1 when it has not been observed.
	remaining int
	// reset is when the primary window rolls over, from X-RateLimit-Reset.
	reset time.Time
}

type rateLimitItem struct {
	key   string
	state tokenState
}

// RateLimitTransport is an http.RoundTripper that keeps a request from being
// sent when the token it carries is known to be out of primary rate-limit
// budget, and that replays a request GitHub answered with a throttling response
// once the named Retry-After has elapsed.
//
// It exists because the two GitHub limits fail differently and only one of them
// is visible. The *primary* limit (5,000 REST requests or GraphQL points per
// hour) is reported on every response, so it can be anticipated: this transport
// records X-RateLimit-Remaining / X-RateLimit-Reset per token and holds the next
// request until the window rolls over rather than spending a call to be told no.
// The *secondary* limits key on concurrency, points per minute and CPU-seconds;
// GitHub states there is no way to query their status, so they can only be
// handled reactively, on the 429 or secondary-403 that reports them. Both paths
// converge here.
//
// State is per token, because quota is per token. Every field below is a bound,
// and each is load-bearing:
//
//   - the token map is an LRU capped at MaxTokens, because its key derives from
//     a caller-supplied Authorization header;
//   - replay is capped at MaxRetries, so a persistently throttled request fails
//     instead of looping;
//   - every wait is capped at MaxWait and is context-aware, so a cancelled
//     tool call returns promptly rather than sleeping out a reset an hour away.
//
// Requests are only ever replayed when GitHub reports the request was throttled
// rather than performed, and only when the body can be rewound via
// Request.GetBody. A request with an unrewindable body is returned as-is.
//
// Safe for concurrent use, and intended to be shared for the lifetime of the
// process: a transport allocated per request observes exactly one response and
// can anticipate nothing.
type RateLimitTransport struct {
	Transport http.RoundTripper

	// MaxTokens bounds the number of tokens tracked. Zero means
	// defaultRateLimitMaxTokens.
	MaxTokens int

	// MaxRetries bounds replays of a throttled request. Negative disables
	// replay; zero means defaultRateLimitMaxRetries.
	MaxRetries int

	// MaxWait bounds any single wait. Zero means defaultRateLimitMaxWait.
	MaxWait time.Duration

	// now and after are injection points for tests. Both default to the
	// corresponding time package function.
	now   func() time.Time
	after func(time.Duration) <-chan time.Time

	mu    sync.Mutex
	ll    *list.List
	items map[string]*list.Element
}

func (t *RateLimitTransport) maxTokens() int {
	if t.MaxTokens > 0 {
		return t.MaxTokens
	}
	return defaultRateLimitMaxTokens
}

func (t *RateLimitTransport) maxRetries() int {
	if t.MaxRetries < 0 {
		return 0
	}
	if t.MaxRetries > 0 {
		return t.MaxRetries
	}
	return defaultRateLimitMaxRetries
}

func (t *RateLimitTransport) maxWait() time.Duration {
	if t.MaxWait > 0 {
		return t.MaxWait
	}
	return defaultRateLimitMaxWait
}

func (t *RateLimitTransport) timeNow() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

func (t *RateLimitTransport) timeAfter(d time.Duration) <-chan time.Time {
	if t.after != nil {
		return t.after(d)
	}
	return time.After(d)
}

func (t *RateLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt := t.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}

	key := rateLimitKey(req)

	// Proactive: the previous response for this token said the primary window
	// was exhausted. Hold until it rolls over rather than spending a call.
	if wait, ok := t.pendingWait(key); ok {
		if err := t.sleep(req, wait); err != nil {
			return nil, err
		}
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	t.observe(key, resp)

	// Reactive: GitHub reported this request as throttled rather than
	// performed. Replay it once the named delay has elapsed.
	for attempt := 0; attempt < t.maxRetries(); attempt++ {
		if !isThrottled(resp) {
			return resp, nil
		}
		wait, ok := t.retryAfter(resp)
		if !ok {
			return resp, nil
		}
		replay, ok := rewind(req)
		if !ok {
			return resp, nil
		}
		drain(resp)
		if err := t.sleep(req, wait); err != nil {
			return nil, err
		}
		resp, err = rt.RoundTrip(replay)
		if err != nil {
			return nil, err
		}
		t.observe(key, resp)
		req = replay
	}
	return resp, nil
}

// sleep waits for d, or returns the context's error if the request is cancelled
// first. A tool call that has been abandoned must not keep a slot warm.
func (t *RateLimitTransport) sleep(req *http.Request, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	ctx := req.Context()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.timeAfter(d):
		return nil
	}
}

// pendingWait reports how long to hold a request for this token, if the last
// observed response left the primary window exhausted. A wait longer than
// MaxWait is declined: the caller is better served by GitHub's own error than
// by an invisible stall.
func (t *RateLimitTransport) pendingWait(key string) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	el, ok := t.items[key]
	if !ok {
		return 0, false
	}
	t.ll.MoveToFront(el)
	state := el.Value.(*rateLimitItem).state
	if state.remaining != 0 || state.reset.IsZero() {
		return 0, false
	}
	d := state.reset.Sub(t.timeNow())
	if d <= 0 || d > t.maxWait() {
		return 0, false
	}
	return d, true
}

// observe records the rate-limit headers from a response. Responses that carry
// no rate-limit headers leave the recorded state untouched.
func (t *RateLimitTransport) observe(key string, resp *http.Response) {
	remainingRaw := resp.Header.Get(rateLimitRemainingHeader)
	resetRaw := resp.Header.Get(rateLimitResetHeader)
	if remainingRaw == "" && resetRaw == "" {
		return
	}

	state := tokenState{remaining: -1}
	if remainingRaw != "" {
		if n, err := strconv.Atoi(remainingRaw); err == nil {
			state.remaining = n
		}
	}
	if resetRaw != "" {
		if sec, err := strconv.ParseInt(resetRaw, 10, 64); err == nil {
			state.reset = time.Unix(sec, 0)
		}
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.items == nil {
		t.items = make(map[string]*list.Element)
		t.ll = list.New()
	}
	if el, ok := t.items[key]; ok {
		el.Value.(*rateLimitItem).state = state
		t.ll.MoveToFront(el)
		return
	}
	t.items[key] = t.ll.PushFront(&rateLimitItem{key: key, state: state})
	for t.ll.Len() > t.maxTokens() {
		oldest := t.ll.Back()
		if oldest == nil {
			break
		}
		t.ll.Remove(oldest)
		delete(t.items, oldest.Value.(*rateLimitItem).key)
	}
}

// retryAfter reports how long to wait before replaying, from Retry-After or
// from X-RateLimit-Reset. A delay beyond MaxWait declines the replay.
func (t *RateLimitTransport) retryAfter(resp *http.Response) (time.Duration, bool) {
	var d time.Duration
	if raw := resp.Header.Get(retryAfterHeader); raw != "" {
		// Retry-After is delta-seconds or an HTTP-date (RFC 9110 10.2.3). GitHub
		// sends delta-seconds today, but a value we cannot parse must not be
		// silently treated as "no delay named" — that is indistinguishable from a
		// MaxWait decline and would hide a format change as unexplained errors.
		//
		// Negative delta-seconds and a past HTTP-date are handled differently, on
		// purpose. RFC 9110 defines delta-seconds as non-negative, so a negative
		// one is malformed and falls through to be declined; a date in the past is
		// well-formed and says the delay has already elapsed, so it is clamped to
		// zero and the replay proceeds at once.
		switch sec, err := strconv.Atoi(strings.TrimSpace(raw)); {
		case err == nil && sec >= 0:
			d = time.Duration(sec) * time.Second
		default:
			at, dateErr := http.ParseTime(strings.TrimSpace(raw))
			if dateErr != nil {
				return 0, false
			}
			d = at.Sub(t.timeNow())
		}
	} else if raw := resp.Header.Get(rateLimitResetHeader); raw != "" && resp.Header.Get(rateLimitRemainingHeader) == "0" {
		sec, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return 0, false
		}
		d = time.Unix(sec, 0).Sub(t.timeNow())
	} else {
		return 0, false
	}

	if d < 0 {
		d = 0
	}
	if d > t.maxWait() {
		return 0, false
	}
	return d, true
}

// isThrottled reports whether GitHub answered with a throttling response, which
// means the request was rejected rather than performed — so replaying it is
// safe even when the method is not idempotent.
//
// 429 is the documented secondary-limit status. 403 is used for the primary
// limit (with X-RateLimit-Remaining: 0) and historically for secondary limits
// too, so a 403 counts only when it carries one of those markers; a 403 from an
// ordinary permission failure must not be replayed.
func isThrottled(resp *http.Response) bool {
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return true
	case http.StatusForbidden:
		return resp.Header.Get(retryAfterHeader) != "" ||
			resp.Header.Get(rateLimitRemainingHeader) == "0"
	default:
		return false
	}
}

// rewind returns a copy of req with a fresh body. A request whose body cannot be
// replayed (GetBody unset, as for an opaque stream) is not retried.
func rewind(req *http.Request) (*http.Request, bool) {
	clone := req.Clone(req.Context())
	if req.Body == nil || req.Body == http.NoBody {
		return clone, true
	}
	if req.GetBody == nil {
		return nil, false
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, false
	}
	clone.Body = body
	return clone, true
}

// drain releases a superseded response so its connection returns to the pool.
//
// Reading before Close is not optional. net/http only reuses a connection when
// the body reached EOF before Close; closing an unread body tells the transport
// to discard the connection instead. Under sustained throttling every replay
// would then burn a fresh TCP connection — the opposite of what a transport
// that exists to reduce pressure should do.
//
// The read is capped. An unbounded copy hands a hostile or malfunctioning server
// a way to stall the replay loop for as long as it cares to keep streaming, and
// drain runs before the context-aware wait, so a cancelled call could not escape
// it. A throttling body is a short JSON error; anything past maxDrainBytes is
// misbehaviour, and abandoning that connection is the right outcome rather than a
// cost. Note the boundary is inclusive: at exactly maxDrainBytes, io.LimitedReader
// returns its own EOF without ever issuing the read that would have let the body
// report EOF, so net/http does not pool that connection either.
func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.CopyN(io.Discard, resp.Body, maxDrainBytes)
	_ = resp.Body.Close()
}

// rateLimitKey derives a stable, non-reversible key from the request's
// Authorization header. Quota is per token, so state must be too. Mirrors
// cacheKey in etag.go, including its 8-byte prefix.
//
// A collision costs correctness, not confidentiality: two tokens would share
// one bucket, so one's exhaustion could hold the other back for a bounded
// wait. No credential and no response body crosses between them. At the
// default bound of 256 tracked tokens the birthday probability over 64 bits is
// around 1.8e-15.
func rateLimitKey(req *http.Request) string {
	sum := sha256.Sum256([]byte(req.Header.Get(headers.AuthorizationHeader)))
	return hex.EncodeToString(sum[:8])
}
