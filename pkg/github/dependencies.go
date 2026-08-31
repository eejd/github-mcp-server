package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	ghcontext "github.com/github/github-mcp-server/pkg/context"
	"github.com/github/github-mcp-server/pkg/http/transport"
	"github.com/github/github-mcp-server/pkg/inventory"
	"github.com/github/github-mcp-server/pkg/lockdown"
	"github.com/github/github-mcp-server/pkg/observability"
	"github.com/github/github-mcp-server/pkg/observability/metrics"
	"github.com/github/github-mcp-server/pkg/raw"
	"github.com/github/github-mcp-server/pkg/translations"
	"github.com/github/github-mcp-server/pkg/utils"
	gogithub "github.com/google/go-github/v89/github"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shurcooL/githubv4"
)

// depsContextKey is the context key for ToolDependencies.
// Using a private type prevents collisions with other packages.
type depsContextKey struct{}

// ErrDepsNotInContext is returned when ToolDependencies is not found in context.
var ErrDepsNotInContext = errors.New("ToolDependencies not found in context; use ContextWithDeps to inject")

func InjectDepsMiddleware(deps ToolDependencies) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (result mcp.Result, err error) {
			return next(ContextWithDeps(ctx, deps), method, req)
		}
	}
}

// ContextWithDeps returns a new context with the ToolDependencies stored in it.
// This is used to inject dependencies at request time rather than at registration time,
// avoiding expensive closure creation during server initialization.
//
// For the local server, this is called once at startup since deps don't change.
// For the remote server, this is called per-request with request-specific deps.
func ContextWithDeps(ctx context.Context, deps ToolDependencies) context.Context {
	return context.WithValue(ctx, depsContextKey{}, deps)
}

// DepsFromContext retrieves ToolDependencies from the context.
// Returns the deps and true if found, or nil and false if not present.
// Use MustDepsFromContext if you want to panic on missing deps (for handlers
// that require deps to function).
func DepsFromContext(ctx context.Context) (ToolDependencies, bool) {
	deps, ok := ctx.Value(depsContextKey{}).(ToolDependencies)
	return deps, ok
}

// MustDepsFromContext retrieves ToolDependencies from the context.
// Panics if deps are not found - use this in handlers where deps are required.
func MustDepsFromContext(ctx context.Context) ToolDependencies {
	deps, ok := DepsFromContext(ctx)
	if !ok {
		panic(ErrDepsNotInContext)
	}
	return deps
}

// ToolDependencies defines the interface for dependencies that tool handlers need.
// This is an interface to allow different implementations:
//   - Local server: stores closures that create clients on demand
//   - Remote server: can store pre-created clients per-request for efficiency
//
// The toolsets package uses `any` for deps and tool handlers type-assert to this interface.
type ToolDependencies interface {
	// GetClient returns a GitHub REST API client
	GetClient(ctx context.Context) (*gogithub.Client, error)

	// GetGQLClient returns a GitHub GraphQL client
	GetGQLClient(ctx context.Context) (*githubv4.Client, error)

	// GetRawClient returns a raw content client for GitHub
	GetRawClient(ctx context.Context) (*raw.Client, error)

	// GetRepoAccessCache returns the lockdown mode repo access cache
	GetRepoAccessCache(ctx context.Context) (*lockdown.RepoAccessCache, error)

	// GetT returns the translation helper function
	GetT() translations.TranslationHelperFunc

	// GetFlags returns feature flags
	GetFlags(ctx context.Context) FeatureFlags

	// GetContentWindowSize returns the content window size for log truncation
	GetContentWindowSize() int

	// IsFeatureEnabled checks if a feature flag is enabled.
	IsFeatureEnabled(ctx context.Context, flagName string) bool

	// Logger returns the structured logger, optionally enriched with
	// request-scoped data from ctx. Integrators provide their own slog.Handler
	// to control where logs are sent.
	Logger(ctx context.Context) *slog.Logger

	// Metrics returns the metrics client
	Metrics(ctx context.Context) metrics.Metrics
}

// BaseDeps is the standard implementation of ToolDependencies for the local server.
// It stores pre-created clients. The remote server can create its own struct
// implementing ToolDependencies with different client creation strategies.
type BaseDeps struct {
	// Pre-created clients
	Client    *gogithub.Client
	GQLClient *githubv4.Client
	RawClient *raw.Client

	// Static dependencies
	RepoAccessCache   *lockdown.RepoAccessCache
	T                 translations.TranslationHelperFunc
	Flags             FeatureFlags
	ContentWindowSize int

	// Feature flag checker for runtime checks
	featureChecker inventory.FeatureFlagChecker

	// Observability exporters (includes logger)
	Obsv observability.Exporters

	// StateSealer protects state sent through multi-round-trip requests.
	StateSealer RequestStateSealer
}

// Compile-time assertion to verify that BaseDeps implements the ToolDependencies interface.
var _ ToolDependencies = (*BaseDeps)(nil)

// NewBaseDeps creates a BaseDeps with the provided clients and configuration.
func NewBaseDeps(
	client *gogithub.Client,
	gqlClient *githubv4.Client,
	rawClient *raw.Client,
	repoAccessCache *lockdown.RepoAccessCache,
	t translations.TranslationHelperFunc,
	flags FeatureFlags,
	contentWindowSize int,
	featureChecker inventory.FeatureFlagChecker,
	obsv observability.Exporters,
) *BaseDeps {
	return &BaseDeps{
		Client:            client,
		GQLClient:         gqlClient,
		RawClient:         rawClient,
		RepoAccessCache:   repoAccessCache,
		T:                 t,
		Flags:             flags,
		ContentWindowSize: contentWindowSize,
		featureChecker:    featureChecker,
		Obsv:              obsv,
	}
}

// GetClient implements ToolDependencies.
func (d BaseDeps) GetClient(_ context.Context) (*gogithub.Client, error) {
	return d.Client, nil
}

// GetGQLClient implements ToolDependencies.
func (d BaseDeps) GetGQLClient(_ context.Context) (*githubv4.Client, error) {
	return d.GQLClient, nil
}

// GetRawClient implements ToolDependencies.
func (d BaseDeps) GetRawClient(_ context.Context) (*raw.Client, error) {
	return d.RawClient, nil
}

// GetRepoAccessCache implements ToolDependencies.
func (d BaseDeps) GetRepoAccessCache(_ context.Context) (*lockdown.RepoAccessCache, error) {
	return d.RepoAccessCache, nil
}

// GetT implements ToolDependencies.
func (d BaseDeps) GetT() translations.TranslationHelperFunc { return d.T }

// GetFlags implements ToolDependencies.
func (d BaseDeps) GetFlags(_ context.Context) FeatureFlags { return d.Flags }

// GetContentWindowSize implements ToolDependencies.
func (d BaseDeps) GetContentWindowSize() int { return d.ContentWindowSize }

// Logger implements ToolDependencies.
func (d BaseDeps) Logger(_ context.Context) *slog.Logger {
	return d.Obsv.Logger()
}

// Metrics implements ToolDependencies.
func (d BaseDeps) Metrics(ctx context.Context) metrics.Metrics {
	if d.Obsv == nil {
		return metrics.NewNoopMetrics()
	}
	return d.Obsv.Metrics(ctx)
}

// GetRequestStateSealer implements RequestStateSealerProvider.
func (d BaseDeps) GetRequestStateSealer() RequestStateSealer { return d.StateSealer }

// IsFeatureEnabled checks if a feature flag is enabled.
// Returns false if the feature checker is nil, flag name is empty, or an error occurs.
// This allows tools to conditionally change behavior based on feature flags.
func (d BaseDeps) IsFeatureEnabled(ctx context.Context, flagName string) bool {
	if d.featureChecker == nil || flagName == "" {
		return false
	}

	enabled, err := d.featureChecker(ctx, flagName)
	if err != nil {
		// Log error but don't fail the tool - treat as disabled
		fmt.Fprintf(os.Stderr, "Feature flag check error for %q: %v\n", flagName, err)
		return false
	}

	return enabled
}

// NewTool creates a ServerTool that retrieves ToolDependencies from context at call time.
// This avoids creating closures at registration time, which is important for performance
// in servers that create a new server instance per request (like the remote server).
//
// The handler function receives deps extracted from context via MustDepsFromContext.
// Ensure ContextWithDeps is called to inject deps before any tool handlers are invoked.
//
// scopeAccess controls fixed-token visibility and per-call OAuth challenges.
func NewTool[In, Out any](
	toolset inventory.ToolsetMetadata,
	tool mcp.Tool,
	scopeAccess inventory.ScopeAccess,
	handler func(ctx context.Context, deps ToolDependencies, req *mcp.CallToolRequest, args In) (*mcp.CallToolResult, Out, error),
) inventory.ServerTool {
	st := inventory.NewServerToolWithContextHandler(tool, toolset, func(ctx context.Context, req *mcp.CallToolRequest, args In) (*mcp.CallToolResult, Out, error) {
		deps := MustDepsFromContext(ctx)
		return handler(ctx, deps, req, args)
	})
	st.ScopeAccess = scopeAccess
	return st
}

// NewToolFromHandler creates a ServerTool that retrieves ToolDependencies from context at call time.
// Use this when you have a handler that conforms to mcp.ToolHandler directly.
//
// The handler function receives deps extracted from context via MustDepsFromContext.
// Ensure ContextWithDeps is called to inject deps before any tool handlers are invoked.
//
// scopeAccess controls fixed-token visibility and per-call OAuth challenges.
func NewToolFromHandler(
	toolset inventory.ToolsetMetadata,
	tool mcp.Tool,
	scopeAccess inventory.ScopeAccess,
	handler func(ctx context.Context, deps ToolDependencies, req *mcp.CallToolRequest) (*mcp.CallToolResult, error),
) inventory.ServerTool {
	st := inventory.NewServerTool(tool, toolset, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		deps := MustDepsFromContext(ctx)
		return handler(ctx, deps, req)
	})
	st.ScopeAccess = scopeAccess
	return st
}

type RequestDeps struct {
	// Static dependencies
	apiHosts          utils.APIHostResolver
	version           string
	lockdownMode      bool
	RepoAccessOpts    []lockdown.RepoAccessOption
	T                 translations.TranslationHelperFunc
	ContentWindowSize int

	// Feature flag checker for runtime checks
	featureChecker inventory.FeatureFlagChecker

	// Observability exporters (includes logger)
	obsv observability.Exporters

	// StateSealer protects state sent through multi-round-trip requests.
	StateSealer RequestStateSealer

	// rateLimit is shared by every client this RequestDeps hands out, and must
	// outlive any single request.
	//
	// This is the whole point of putting it here rather than inside the getters
	// below. A rate-limit transport works by remembering what the *previous*
	// response said - X-RateLimit-Remaining, X-RateLimit-Reset, Retry-After - so
	// the next request can be held back instead of spent. Allocated per call it
	// would be born empty, observe exactly one response, and be discarded: the
	// chain would look correct in a test that asserts the transport is present,
	// and anticipate nothing at all in production.
	//
	// In http mode NewRequestDeps is called once, from RunHTTPServer, and the
	// result is stored on the handler and reused for every request, so a field
	// here is process-lifetime state. The transport is safe for concurrent use.
	rateLimit *transport.RateLimitTransport

	// etag is the conditional-request cache for the REST client, and like
	// rateLimit above it must be shared rather than rebuilt per request — a cache
	// with a lifetime of one request is not a cache.
	//
	// Upstream wires this into the stdio server only, on the reasoning that the
	// hosted deployment builds a fresh client per request. That reasoning is about
	// where the transport is *allocated*, not about whether an in-process cache is
	// useful here; with the allocation moved onto RequestDeps it applies to this
	// server too. It is enabled here because this is a single replica, so a
	// process-local cache is the whole cache. It would be the wrong call for a
	// horizontally-scaled deployment, where each replica would hold a partial one.
	//
	// Deliberately NOT given to the raw-content client: raw bodies are whole files
	// and would evict everything worth keeping. See rawTransport.
	etag *transport.ETagTransport
}

// NewRequestDeps creates a RequestDeps with the provided clients and configuration.
func NewRequestDeps(
	apiHosts utils.APIHostResolver,
	version string,
	lockdownMode bool,
	repoAccessOpts []lockdown.RepoAccessOption,
	t translations.TranslationHelperFunc,
	contentWindowSize int,
	featureChecker inventory.FeatureFlagChecker,
	obsv observability.Exporters,
) *RequestDeps {
	rateLimit := &transport.RateLimitTransport{Transport: http.DefaultTransport}
	return &RequestDeps{
		apiHosts:          apiHosts,
		version:           version,
		lockdownMode:      lockdownMode,
		RepoAccessOpts:    repoAccessOpts,
		T:                 t,
		ContentWindowSize: contentWindowSize,
		featureChecker:    featureChecker,
		obsv:              obsv,
		rateLimit:         rateLimit,
		// Chained here, not lazily in restTransport: that accessor is called
		// concurrently by every request, and assigning through it would race.
		// 8 MiB rather than the 32 MiB default — this server runs under a 256 MiB
		// container limit, and a cache is not worth an OOM.
		etag: &transport.ETagTransport{Transport: rateLimit, MaxTotalBytes: 8 << 20},
	}
}

// baseTransport returns the shared rate-limit transport, or the default
// transport when this RequestDeps was built without one (tests constructing
// the struct literally). It is referenced, never constructed, so the state it
// accumulates survives across requests.
func (d *RequestDeps) baseTransport() http.RoundTripper {
	if d.rateLimit == nil {
		return http.DefaultTransport
	}
	return d.rateLimit
}

// restTransport returns the transport chain for the REST client:
//
//	BearerAuthTransport -> ETagTransport -> RateLimitTransport -> default
//
// The order is load-bearing in both directions. ETagTransport must sit BELOW
// BearerAuthTransport, because its cache key hashes the Authorization header:
// above it, every entry would key on an empty header and one token's response
// bodies would be served to another. And it must sit ABOVE RateLimitTransport,
// so that a body served from cache after a 304 does not re-enter the throttle.
func (d *RequestDeps) restTransport() http.RoundTripper {
	if d.etag == nil {
		return d.baseTransport()
	}
	return d.etag
}

// rawTransport returns the transport chain for the raw-content client: the
// same throttle, deliberately without the cache.
//
// Raw responses are whole file bodies. Letting them into an LRU sized for API
// responses would evict everything worth keeping and then fail to help anyway,
// since a file is rarely re-read within a session. Upstream's stdio server
// makes the same split with a separate rawUATransport; this is that decision
// carried over, not a new one.
func (d *RequestDeps) rawTransport() http.RoundTripper {
	return d.baseTransport()
}

// GetClient implements ToolDependencies.
func (d *RequestDeps) GetClient(ctx context.Context) (*gogithub.Client, error) {
	return d.newRESTClient(ctx, d.restTransport())
}

// newRESTClient builds a go-github client over the given transport. It exists
// so the raw-content client can be built the same way but on a chain without
// the conditional-request cache; everything else about the two is identical,
// including the host allow-list that scopes the bearer token.
func (d *RequestDeps) newRESTClient(ctx context.Context, rt http.RoundTripper) (*gogithub.Client, error) {
	// extract the token from the context
	tokenInfo, ok := ghcontext.GetTokenInfo(ctx)
	if !ok {
		return nil, fmt.Errorf("no token info in context")
	}
	token := tokenInfo.Token

	baseRestURL, err := d.apiHosts.BaseRESTURL(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get base REST URL: %w", err)
	}
	uploadURL, err := d.apiHosts.UploadURL(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get upload URL: %w", err)
	}
	graphqlURL, err := d.apiHosts.GraphqlURL(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get GraphQL URL: %w", err)
	}
	rawURL, err := d.apiHosts.RawURL(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get Raw URL: %w", err)
	}

	allowedHosts := []string{
		baseRestURL.Host,
		uploadURL.Host,
		graphqlURL.Host,
		rawURL.Host,
	}

	// Construct REST client
	restClient, err := gogithub.NewClient(
		gogithub.WithHTTPClient(&http.Client{Transport: &transport.BearerAuthTransport{
			Transport:    rt,
			Token:        token,
			AllowedHosts: allowedHosts,
		}}),
		gogithub.WithUserAgent(fmt.Sprintf("github-mcp-server/%s", d.version)),
		gogithub.WithEnterpriseURLs(baseRestURL.String(), uploadURL.String()),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create REST client: %w", err)
	}
	return restClient, nil
}

// GetRequestStateSealer implements RequestStateSealerProvider.
func (d *RequestDeps) GetRequestStateSealer() RequestStateSealer { return d.StateSealer }

// GetGQLClient implements ToolDependencies.
func (d *RequestDeps) GetGQLClient(ctx context.Context) (*githubv4.Client, error) {
	// extract the token from the context
	tokenInfo, ok := ghcontext.GetTokenInfo(ctx)
	if !ok {
		return nil, fmt.Errorf("no token info in context")
	}
	token := tokenInfo.Token

	baseRestURL, err := d.apiHosts.BaseRESTURL(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get base REST URL: %w", err)
	}
	uploadURL, err := d.apiHosts.UploadURL(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get upload URL: %w", err)
	}
	graphqlURL, err := d.apiHosts.GraphqlURL(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get GraphQL URL: %w", err)
	}
	rawURL, err := d.apiHosts.RawURL(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get Raw URL: %w", err)
	}

	// allowedHosts scopes the bearer token to the configured GitHub hosts, so a
	// response that redirects off them does not carry the token to the redirect
	// target. See transport.BearerAuthTransport.
	allowedHosts := []string{
		baseRestURL.Host,
		uploadURL.Host,
		graphqlURL.Host,
		rawURL.Host,
	}

	// Construct GraphQL client
	// We use NewEnterpriseClient unconditionally since we already parsed the API host
	// Wrap transport with GraphQLFeaturesTransport to inject feature flags from context,
	// matching the transport chain used by the remote server.
	gqlHTTPClient := &http.Client{
		Transport: &transport.BearerAuthTransport{
			Transport: &transport.GraphQLFeaturesTransport{
				Transport: d.baseTransport(),
			},
			Token:        token,
			AllowedHosts: allowedHosts,
		},
	}

	gqlClient := githubv4.NewEnterpriseClient(graphqlURL.String(), gqlHTTPClient)
	return gqlClient, nil
}

// GetRawClient implements ToolDependencies.
func (d *RequestDeps) GetRawClient(ctx context.Context) (*raw.Client, error) {
	// Not GetClient: the raw client must not inherit the conditional-request
	// cache. See rawTransport.
	client, err := d.newRESTClient(ctx, d.rawTransport())
	if err != nil {
		return nil, err
	}

	rawURL, err := d.apiHosts.RawURL(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get Raw URL: %w", err)
	}

	rawClient, err := raw.NewClient(client, rawURL)
	if err != nil {
		return nil, fmt.Errorf("failed to create raw client: %w", err)
	}

	return rawClient, nil
}

// effectiveLockdownMode reports whether lockdown mode is active for the
// request. d.lockdownMode is an operator-set upper bound: the per-request
// X-MCP-Lockdown header (ghcontext.IsLockdownMode) can only enable lockdown,
// never disable one the operator already turned on.
func (d *RequestDeps) effectiveLockdownMode(ctx context.Context) bool {
	return d.lockdownMode || ghcontext.IsLockdownMode(ctx)
}

// GetRepoAccessCache implements ToolDependencies.
func (d *RequestDeps) GetRepoAccessCache(ctx context.Context) (*lockdown.RepoAccessCache, error) {
	if !d.effectiveLockdownMode(ctx) {
		return nil, nil
	}

	gqlClient, err := d.GetGQLClient(ctx)
	if err != nil {
		return nil, err
	}

	restClient, err := d.GetClient(ctx)
	if err != nil {
		return nil, err
	}

	// RepoAccessOpts is shared across requests, so copy before appending the
	// per-request identity scope.
	opts := d.RepoAccessOpts
	if tokenInfo, ok := ghcontext.GetTokenInfo(ctx); ok && tokenInfo.Token != "" {
		opts = append(append([]lockdown.RepoAccessOption{}, d.RepoAccessOpts...), lockdown.WithIdentity(tokenInfo.Token))
	}

	// Create repo access cache
	instance := lockdown.NewRepoAccessCache(gqlClient, restClient, opts...)
	return instance, nil
}

// GetT implements ToolDependencies.
func (d *RequestDeps) GetT() translations.TranslationHelperFunc { return d.T }

// GetFlags implements ToolDependencies.
func (d *RequestDeps) GetFlags(ctx context.Context) FeatureFlags {
	return FeatureFlags{
		LockdownMode: d.effectiveLockdownMode(ctx),
	}
}

// GetContentWindowSize implements ToolDependencies.
func (d *RequestDeps) GetContentWindowSize() int { return d.ContentWindowSize }

// Logger implements ToolDependencies.
func (d *RequestDeps) Logger(_ context.Context) *slog.Logger {
	return d.obsv.Logger()
}

// Metrics implements ToolDependencies.
func (d *RequestDeps) Metrics(ctx context.Context) metrics.Metrics {
	if d.obsv == nil {
		return metrics.NewNoopMetrics()
	}
	return d.obsv.Metrics(ctx)
}

// IsFeatureEnabled checks if a feature flag is enabled.
func (d *RequestDeps) IsFeatureEnabled(ctx context.Context, flagName string) bool {
	if d.featureChecker == nil || flagName == "" {
		return false
	}

	enabled, err := d.featureChecker(ctx, flagName)
	if err != nil {
		// Log error but don't fail the tool - treat as disabled
		fmt.Fprintf(os.Stderr, "Feature flag check error for %q: %v\n", flagName, err)
		return false
	}

	return enabled
}
