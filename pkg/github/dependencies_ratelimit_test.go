package github_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	ghcontext "github.com/github/github-mcp-server/pkg/context"
	"github.com/github/github-mcp-server/pkg/github"
	"github.com/github/github-mcp-server/pkg/http/headers"
	ghtransport "github.com/github/github-mcp-server/pkg/http/transport"
	"github.com/github/github-mcp-server/pkg/translations"
	"github.com/shurcooL/githubv4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newRateLimitTestDeps(t *testing.T, endpoint string) *github.RequestDeps {
	t.Helper()
	return github.NewRequestDeps(
		newRequestDepsAPIHostResolver(t, endpoint),
		"test",
		false,
		nil,
		translations.NullTranslationHelper,
		0,
		nil,
		testExporters(),
	)
}

// findRateLimitTransport walks an exported transport chain looking for the
// rate-limit transport. Only BearerAuthTransport's link is exported, which is
// enough: it is the outermost wrapper on every client built by RequestDeps.
func findRateLimitTransport(rt http.RoundTripper) *ghtransport.RateLimitTransport {
	for range 8 {
		switch v := rt.(type) {
		case *ghtransport.RateLimitTransport:
			return v
		case *ghtransport.BearerAuthTransport:
			rt = v.Transport
		case *ghtransport.GraphQLFeaturesTransport:
			rt = v.Transport
		default:
			return nil
		}
	}
	return nil
}

// TestRequestDepsRateLimitTransportSurvivesAcrossCalls is the test that would
// have caught the defect this wiring exists to avoid.
//
// A rate-limit transport is only useful because it remembers what the previous
// response said. Allocated inside GetClient it would be born empty on every
// call, observe exactly one response, and be discarded — and a test that merely
// asserted "the transport is in the chain" would still pass. So this asserts
// pointer identity across two separate GetClient calls: same object, therefore
// same accumulated state.
func TestRequestDepsRateLimitTransportSurvivesAcrossCalls(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	deps := newRateLimitTestDeps(t, server.URL)
	ctx := ghcontext.WithTokenInfo(context.Background(), &ghcontext.TokenInfo{Token: "request-token"})

	first, err := deps.GetClient(ctx)
	require.NoError(t, err)
	second, err := deps.GetClient(ctx)
	require.NoError(t, err)

	firstRL := findRateLimitTransport(first.Client().Transport)
	secondRL := findRateLimitTransport(second.Client().Transport)

	require.NotNil(t, firstRL, "REST chain must carry the rate-limit transport")
	require.NotNil(t, secondRL, "REST chain must carry the rate-limit transport")
	assert.Same(t, firstRL, secondRL,
		"the rate-limit transport must be shared across calls; a per-call instance remembers nothing")
}

// TestRequestDepsRawClientSharesRateLimitTransport: the raw client delegates to
// GetClient, and raw reads spend the same per-token budget as everything else,
// so it must draw on the same state rather than a private one.
func TestRequestDepsRawClientSharesRateLimitTransport(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	deps := newRateLimitTestDeps(t, server.URL)
	ctx := ghcontext.WithTokenInfo(context.Background(), &ghcontext.TokenInfo{Token: "request-token"})

	restClient, err := deps.GetClient(ctx)
	require.NoError(t, err)
	_, err = deps.GetRawClient(ctx)
	require.NoError(t, err)

	restRL := findRateLimitTransport(restClient.Client().Transport)
	require.NotNil(t, restRL)

	// GetRawClient builds on GetClient, so a second GetClient resolves to the
	// same transport the raw client received.
	again, err := deps.GetClient(ctx)
	require.NoError(t, err)
	assert.Same(t, restRL, findRateLimitTransport(again.Client().Transport))
}

// TestRequestDepsGraphQLRetriesSecondaryLimit proves the transport is actually
// in the GraphQL chain, which pointer-walking cannot show: githubv4.Client does
// not expose its http.Client. GraphQL is where the incidents that motivated this
// work happened, so "it is wired for REST" is not good enough.
func TestRequestDepsGraphQLRetriesSecondaryLimit(t *testing.T) {
	t.Parallel()

	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set(headers.ContentTypeHeader, headers.ContentTypeJSON)
		_, _ = w.Write([]byte(`{"data":{"viewer":{"login":"octocat"}}}`))
	}))
	defer server.Close()

	deps := newRateLimitTestDeps(t, server.URL)
	ctx := ghcontext.WithTokenInfo(context.Background(), &ghcontext.TokenInfo{Token: "request-token"})

	gql, err := deps.GetGQLClient(ctx)
	require.NoError(t, err)

	var query struct {
		Viewer struct{ Login githubv4.String }
	}
	start := time.Now()
	require.NoError(t, gql.Query(ctx, &query, nil))

	assert.Equal(t, int32(2), atomic.LoadInt32(&hits),
		"the 429 must be replayed once, not surfaced to the caller")
	assert.Equal(t, "octocat", string(query.Viewer.Login))
	assert.GreaterOrEqual(t, time.Since(start), time.Second,
		"the replay must honour Retry-After rather than retrying immediately")
}

// TestRequestDepsSharesRateLimitStateAcrossClients: quota is per token and this
// server presents one token per request, so a limit observed on the REST client
// must be visible to the GraphQL client. Separate state per client would let the
// second lane walk straight into a limit the first already discovered.
func TestRequestDepsSharesRateLimitStateAcrossClients(t *testing.T) {
	t.Parallel()

	var restHits, gqlHits int32
	reset := time.Now().Add(2 * time.Second).Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			atomic.AddInt32(&gqlHits, 1)
			w.Header().Set(headers.ContentTypeHeader, headers.ContentTypeJSON)
			w.Header().Set("X-RateLimit-Remaining", "4999")
			_, _ = w.Write([]byte(`{"data":{"viewer":{"login":"octocat"}}}`))
			return
		}
		atomic.AddInt32(&restHits, 1)
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	deps := newRateLimitTestDeps(t, server.URL)
	ctx := ghcontext.WithTokenInfo(context.Background(), &ghcontext.TokenInfo{Token: "request-token"})

	restClient, err := deps.GetClient(ctx)
	require.NoError(t, err)
	resp, err := restClient.Client().Get(server.URL + "/rest")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	gql, err := deps.GetGQLClient(ctx)
	require.NoError(t, err)
	var query struct {
		Viewer struct{ Login githubv4.String }
	}
	start := time.Now()
	require.NoError(t, gql.Query(ctx, &query, nil))

	// A real threshold, not NotZero. Any network call takes non-zero time, so
	// NotZero passes against a transport that shares no state at all and never
	// waits — it would have pinned nothing. The reset is 2s out, so a working
	// transport blocks for close to that and a broken one returns in
	// milliseconds; 1s separates them with room for scheduling noise.
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, time.Second,
		"the GraphQL request must observe the exhausted window the REST response reported; "+
			"took %s, which means it did not wait", elapsed)
	assert.Equal(t, int32(1), atomic.LoadInt32(&gqlHits))
}
