package github

import (
	"net/http"
	"testing"

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
