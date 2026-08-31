package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	ghcontext "github.com/github/github-mcp-server/pkg/context"
	"github.com/github/github-mcp-server/pkg/http/transport"
)

// The transport chains are asserted from inside the package because the seams
// they hang off are unexported, and exporting them purely to let an external
// test look would widen an API we want upstream to take unchanged.

func newTransportTestDeps() *RequestDeps {
	return NewRequestDeps(nil, "test", false, nil, nil, 0, nil, nil)
}

// TestRESTChainOrder pins the one ordering mistake that would be a security bug
// rather than a performance one.
//
// ETagTransport's cache key hashes the Authorization header. Placed ABOVE
// BearerAuthTransport it would run before the header is set, so every entry
// would key on an empty string and one token's response bodies would be served
// to another. Placed below, as here, each token gets its own entries.
//
// It must also sit above RateLimitTransport, so a body served from cache after a
// 304 does not re-enter the throttle.
func TestRESTChainOrder(t *testing.T) {
	deps := newTransportTestDeps()

	etag, ok := deps.restTransport().(*transport.ETagTransport)
	if !ok {
		t.Fatalf("REST chain must be headed by the ETag cache, got %T", deps.restTransport())
	}
	if _, ok := etag.Transport.(*transport.RateLimitTransport); !ok {
		t.Fatalf("the throttle must sit beneath the cache, got %T", etag.Transport)
	}
}

// TestRawChainHasNoCache: raw responses are whole file bodies. Letting them into
// an LRU sized for API responses would evict everything worth keeping and then
// fail to help anyway, since a file is rarely re-read within a session.
func TestRawChainHasNoCache(t *testing.T) {
	deps := newTransportTestDeps()

	if _, ok := deps.rawTransport().(*transport.ETagTransport); ok {
		t.Fatal("the raw-content chain must not carry the conditional-request cache")
	}
	if _, ok := deps.rawTransport().(*transport.RateLimitTransport); !ok {
		t.Fatalf("the raw-content chain must still carry the throttle, got %T", deps.rawTransport())
	}
}

// TestCacheAndThrottleAreShared: both are stateful, so both must be the same
// object on every call. Rebuilt per request, a cache holds nothing and a
// throttle anticipates nothing.
func TestCacheAndThrottleAreShared(t *testing.T) {
	deps := newTransportTestDeps()

	if deps.restTransport() != deps.restTransport() {
		t.Fatal("the REST chain must be shared across calls, not rebuilt")
	}
	if deps.rawTransport() != deps.rawTransport() {
		t.Fatal("the raw chain must be shared across calls, not rebuilt")
	}
	if deps.baseTransport() != deps.rateLimit {
		t.Fatal("the base chain must be the shared throttle")
	}
}

// TestCacheBudgetFitsTheContainer: the 32 MiB default is a quarter of this
// server's whole memory limit. A cache is not worth an OOM.
func TestCacheBudgetFitsTheContainer(t *testing.T) {
	deps := newTransportTestDeps()

	if got := deps.etag.MaxTotalBytes; got != 8<<20 {
		t.Fatalf("cache budget is %d bytes, want 8 MiB", got)
	}
}

// TestTransportSeamsTolerateALiteralStruct: RequestDeps is constructed literally
// in places that do not go through NewRequestDeps, and the accessors must not
// panic there.
func TestTransportSeamsTolerateALiteralStruct(t *testing.T) {
	var deps RequestDeps

	if deps.baseTransport() != http.DefaultTransport {
		t.Fatal("a zero RequestDeps must fall back to the default transport")
	}
	if deps.restTransport() != http.DefaultTransport {
		t.Fatal("a zero RequestDeps must fall back to the default transport")
	}
	if deps.rawTransport() != http.DefaultTransport {
		t.Fatal("a zero RequestDeps must fall back to the default transport")
	}
}

// chainTestHostResolver points every GitHub URL at one test server.
type chainTestHostResolver struct{ u *url.URL }

func (r chainTestHostResolver) BaseRESTURL(context.Context) (*url.URL, error) { return r.u, nil }
func (r chainTestHostResolver) GraphqlURL(context.Context) (*url.URL, error)  { return r.u, nil }
func (r chainTestHostResolver) UploadURL(context.Context) (*url.URL, error)   { return r.u, nil }
func (r chainTestHostResolver) RawURL(context.Context) (*url.URL, error)      { return r.u, nil }
func (r chainTestHostResolver) AuthorizationServerURL(context.Context) (*url.URL, error) {
	return r.u, nil
}

func newChainTestDeps(t *testing.T, endpoint string) (*RequestDeps, context.Context) {
	t.Helper()
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	deps := NewRequestDeps(chainTestHostResolver{u: u}, "test", false, nil, nil, 0, nil, nil)
	ctx := ghcontext.WithTokenInfo(context.Background(), &ghcontext.TokenInfo{Token: "chain-test-token"})
	return deps, ctx
}

// TestBuiltRESTChainPutsAuthAboveCache pins the ordering where it is actually
// assembled, not just at the seam that supplies it.
//
// TestRESTChainOrder above checks what restTransport() returns. It cannot catch
// newRESTClient wrapping those two the other way round — writing
// ETagTransport{BearerAuthTransport{...}} would still leave restTransport()
// returning an ETagTransport over a RateLimitTransport, so that test would pass
// while the cache ran before the Authorization header was set, keyed every entry
// on an empty header, and served one token's response bodies to another.
//
// This walks the chain the client was actually built with.
func TestBuiltRESTChainPutsAuthAboveCache(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	deps, ctx := newChainTestDeps(t, server.URL)
	client, err := deps.GetClient(ctx)
	if err != nil {
		t.Fatalf("GetClient: %v", err)
	}

	bearer, ok := client.Client().Transport.(*transport.BearerAuthTransport)
	if !ok {
		t.Fatalf("auth must be the outermost layer, got %T", client.Client().Transport)
	}
	etag, ok := bearer.Transport.(*transport.ETagTransport)
	if !ok {
		t.Fatalf("the cache must sit beneath auth, got %T — above it, every entry keys on an empty Authorization header", bearer.Transport)
	}
	if etag != deps.etag {
		t.Fatal("the client must use the shared cache, not a fresh one")
	}
}

// TestRawClientKeepsTheThrottle closes the gap left by asserting only on
// rawTransport(): raw.NewClient re-wraps the go-github client, and if that ever
// dropped the transport the seam test would still pass. Behavioural, because
// raw.Client keeps its inner client unexported.
//
// The server throttles once. Without the throttle in the chain the 429 reaches
// the caller after a single request; with it, the request is replayed and the
// server sees two.
func TestRawClientKeepsTheThrottle(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			// Zero, not one: the replay path is identical either way, and a real
			// second of wall clock buys the suite nothing.
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("file body"))
	}))
	defer server.Close()

	deps, ctx := newChainTestDeps(t, server.URL)
	rawClient, err := deps.GetRawClient(ctx)
	if err != nil {
		t.Fatalf("GetRawClient: %v", err)
	}

	resp, err := rawClient.GetRawContent(ctx, "o", "r", "path", nil)
	if err != nil {
		t.Fatalf("GetRawContent: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want the replay to succeed, got %d", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("the raw chain must carry the throttle; want 2 upstream hits, got %d", got)
	}
}
