package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock drives the transport's waits without spending real time. after()
// records the requested duration and fires immediately.
type fakeClock struct {
	now   time.Time
	waits []time.Duration
}

func (c *fakeClock) after(d time.Duration) <-chan time.Time {
	c.waits = append(c.waits, d)
	ch := make(chan time.Time, 1)
	c.now = c.now.Add(d)
	ch <- c.now
	return ch
}

func newTestRateLimitTransport(t *testing.T, clock *fakeClock) *RateLimitTransport {
	t.Helper()
	return &RateLimitTransport{
		Transport: newIsolatedTransport(t),
		now:       func() time.Time { return clock.now },
		after:     clock.after,
	}
}

func doGet(t *testing.T, rt http.RoundTripper, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// TestRateLimitTransportWaitsForPrimaryReset is the proactive path: a response
// that leaves the primary window exhausted must hold the *next* request until
// the window rolls over, rather than spending a call to be told no.
func TestRateLimitTransportWaitsForPrimaryReset(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_000_000, 0)}
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			w.Header().Set(rateLimitRemainingHeader, "0")
			w.Header().Set(rateLimitResetHeader, strconv.FormatInt(clock.now.Add(30*time.Second).Unix(), 10))
		} else {
			w.Header().Set(rateLimitRemainingHeader, "4999")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	tr := newTestRateLimitTransport(t, clock)
	doGet(t, tr, server.URL)
	if len(clock.waits) != 0 {
		t.Fatalf("first request must not wait, waited %v", clock.waits)
	}

	doGet(t, tr, server.URL)
	if len(clock.waits) != 1 || clock.waits[0] != 30*time.Second {
		t.Fatalf("want one 30s wait before the second request, got %v", clock.waits)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("want 2 upstream hits, got %d", got)
	}
}

// TestRateLimitTransportDeclinesLongPrimaryWait: the primary window is an hour
// wide. Blocking a tool call for most of one is worse than surfacing GitHub's
// own error, so a wait beyond MaxWait is declined and the request is sent.
func TestRateLimitTransportDeclinesLongPrimaryWait(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_000_000, 0)}
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set(rateLimitRemainingHeader, "0")
		w.Header().Set(rateLimitResetHeader, strconv.FormatInt(clock.now.Add(45*time.Minute).Unix(), 10))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	tr := newTestRateLimitTransport(t, clock)
	doGet(t, tr, server.URL)
	doGet(t, tr, server.URL)

	if len(clock.waits) != 0 {
		t.Fatalf("a 45m reset is beyond MaxWait and must not be waited out, got %v", clock.waits)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("want 2 upstream hits, got %d", got)
	}
}

// TestRateLimitTransportRetriesSecondaryLimit covers both shapes GitHub uses to
// report a secondary limit: a 429, and a 403 carrying Retry-After.
func TestRateLimitTransportRetriesSecondaryLimit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"429", http.StatusTooManyRequests},
		{"secondary 403", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &fakeClock{now: time.Unix(1_000_000, 0)}
			var hits int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if atomic.AddInt32(&hits, 1) == 1 {
					w.Header().Set(retryAfterHeader, "1")
					w.WriteHeader(tc.status)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			tr := newTestRateLimitTransport(t, clock)
			resp := doGet(t, tr, server.URL)

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("want the replay to succeed, got %d", resp.StatusCode)
			}
			if got := atomic.LoadInt32(&hits); got != 2 {
				t.Fatalf("want exactly 2 upstream hits, got %d", got)
			}
			if len(clock.waits) != 1 || clock.waits[0] != time.Second {
				t.Fatalf("want one 1s wait honouring Retry-After, got %v", clock.waits)
			}
		})
	}
}

// TestRateLimitTransportDoesNotRetryOrdinaryForbidden: a permission failure is
// not a throttling response. Replaying it would double a non-idempotent call
// for nothing.
func TestRateLimitTransportDoesNotRetryOrdinaryForbidden(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_000_000, 0)}
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set(rateLimitRemainingHeader, "4999")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	tr := newTestRateLimitTransport(t, clock)
	resp := doGet(t, tr, server.URL)

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want the 403 surfaced, got %d", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("an ordinary 403 must not be replayed; got %d hits", got)
	}
}

// TestRateLimitTransportRetriesAreBounded: a server that throttles forever must
// produce an error response, not an unbounded loop.
func TestRateLimitTransportRetriesAreBounded(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_000_000, 0)}
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set(retryAfterHeader, "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	tr := newTestRateLimitTransport(t, clock)
	tr.MaxRetries = 2
	resp := doGet(t, tr, server.URL)

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("want the throttling response returned, got %d", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("want 1 initial + 2 replays = 3 hits, got %d", got)
	}
}

// TestRateLimitTransportHonoursContextCancellation: an abandoned tool call must
// not sleep out its Retry-After.
func TestRateLimitTransportHonoursContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(retryAfterHeader, "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	// A real (never-firing) timer, so the only way out is the context.
	tr := &RateLimitTransport{
		Transport: newIsolatedTransport(t),
		after:     func(time.Duration) <-chan time.Time { return make(chan time.Time) },
	}

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-token")

	done := make(chan error, 1)
	go func() {
		resp, err := tr.RoundTrip(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		done <- err
	}()

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RoundTrip did not return after the context was cancelled")
	}
}

// TestRateLimitTransportRewindsBody: a replayed POST must carry its body again,
// not an already-drained reader.
func TestRateLimitTransportRewindsBody(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_000_000, 0)}
	var hits int32
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if atomic.AddInt32(&hits, 1) == 1 {
			w.Header().Set(retryAfterHeader, "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(`{"query":"x"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-token")

	tr := newTestRateLimitTransport(t, clock)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if len(bodies) != 2 || bodies[0] != bodies[1] || bodies[1] != `{"query":"x"}` {
		t.Fatalf("replay must resend the same body, got %q", bodies)
	}
}

// TestRateLimitTransportDoesNotRetryUnrewindableBody: without GetBody the body
// cannot be resent, so the throttling response is surfaced rather than replaying
// a truncated request.
func TestRateLimitTransportDoesNotRetryUnrewindableBody(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_000_000, 0)}
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		atomic.AddInt32(&hits, 1)
		w.Header().Set(retryAfterHeader, "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL, io.NopCloser(strings.NewReader("opaque")))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.GetBody = nil
	req.Header.Set("Authorization", "Bearer test-token")

	tr := newTestRateLimitTransport(t, clock)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("an unrewindable body must not be replayed; got %d hits", got)
	}
}

// TestRateLimitTransportStateIsPerToken: quota is per token, so one token's
// exhaustion must not hold up another's requests.
func TestRateLimitTransportStateIsPerToken(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_000_000, 0)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer exhausted" {
			w.Header().Set(rateLimitRemainingHeader, "0")
			w.Header().Set(rateLimitResetHeader, strconv.FormatInt(clock.now.Add(10*time.Second).Unix(), 10))
		} else {
			w.Header().Set(rateLimitRemainingHeader, "4999")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	tr := newTestRateLimitTransport(t, clock)

	send := func(token string) {
		req, err := http.NewRequest(http.MethodGet, server.URL, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatalf("round trip: %v", err)
		}
		_ = resp.Body.Close()
	}

	send("exhausted")
	send("healthy")
	if len(clock.waits) != 0 {
		t.Fatalf("a different token must not inherit the exhausted state, waited %v", clock.waits)
	}
	send("exhausted")
	if len(clock.waits) != 1 {
		t.Fatalf("the exhausted token must wait, got %v", clock.waits)
	}
}

// TestRateLimitTransportBoundsTokenMap: the key derives from a caller-supplied
// header, so the map must be bounded or it is a memory-exhaustion vector.
func TestRateLimitTransportBoundsTokenMap(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_000_000, 0)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(rateLimitRemainingHeader, "4999")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	tr := newTestRateLimitTransport(t, clock)
	tr.MaxTokens = 8

	for i := 0; i < 100; i++ {
		req, err := http.NewRequest(http.MethodGet, server.URL, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", fmt.Sprintf("Bearer token-%d", i))
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatalf("round trip: %v", err)
		}
		_ = resp.Body.Close()
	}

	tr.mu.Lock()
	n := len(tr.items)
	llLen := tr.ll.Len()
	tr.mu.Unlock()
	if n != 8 || llLen != 8 {
		t.Fatalf("want the map bounded at 8, got map=%d list=%d", n, llLen)
	}
}

// TestRateLimitTransportConcurrentUse: the transport is shared for the lifetime
// of the process, so it must be race-free under -race.
func TestRateLimitTransportConcurrentUse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(rateLimitRemainingHeader, "4999")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	tr := &RateLimitTransport{Transport: newIsolatedTransport(t)}

	done := make(chan struct{})
	for i := 0; i < 50; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			req, err := http.NewRequest(http.MethodGet, server.URL, nil)
			if err != nil {
				return
			}
			req.Header.Set("Authorization", fmt.Sprintf("Bearer token-%d", i%5))
			resp, err := tr.RoundTrip(req)
			if err != nil {
				return
			}
			_ = resp.Body.Close()
		}(i)
	}
	for i := 0; i < 50; i++ {
		<-done
	}
}

// hdr builds an http.Header via Set so keys are canonicalised the way net/http
// canonicalises them off the wire. A raw map literal is not equivalent:
// "X-RateLimit-Remaining" is not the canonical form of itself
// (textproto lowercases after each dash), so Header.Get would never find it and
// the test would assert against a header the code cannot see.
func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

// TestIsThrottled pins the throttling-response classification directly.
//
// The end-to-end test above cannot pin it on its own: an ordinary 403 carries
// neither Retry-After nor X-RateLimit-Remaining: 0, so retryAfter declines the
// replay regardless of how isThrottled classifies it, and the test passes even
// when isThrottled is wrong. Classifying a 403 is the security-relevant half —
// replaying a permission failure would double a non-idempotent call — so it gets
// its own assertions.
func TestIsThrottled(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		header http.Header
		want   bool
	}{
		{
			name:   "429 is always a secondary limit",
			status: http.StatusTooManyRequests,
			header: hdr(),
			want:   true,
		},
		{
			name:   "403 with Retry-After is a secondary limit",
			status: http.StatusForbidden,
			header: hdr(retryAfterHeader, "60"),
			want:   true,
		},
		{
			name:   "403 with no budget left is the primary limit",
			status: http.StatusForbidden,
			header: hdr(rateLimitRemainingHeader, "0"),
			want:   true,
		},
		{
			name:   "403 from a permission failure is not throttling",
			status: http.StatusForbidden,
			header: hdr(rateLimitRemainingHeader, "4999"),
			want:   false,
		},
		{
			name:   "bare 403 is not throttling",
			status: http.StatusForbidden,
			header: hdr(),
			want:   false,
		},
		{
			name:   "404 is not throttling",
			status: http.StatusNotFound,
			header: hdr(retryAfterHeader, "60"),
			want:   false,
		},
		{
			name:   "200 is not throttling",
			status: http.StatusOK,
			header: hdr(rateLimitRemainingHeader, "0"),
			want:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := isThrottled(&http.Response{StatusCode: tc.status, Header: tc.header})
			if got != tc.want {
				t.Fatalf("isThrottled(%d, %v) = %v, want %v", tc.status, tc.header, got, tc.want)
			}
		})
	}
}
