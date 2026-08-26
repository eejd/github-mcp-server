package servercard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/github/github-mcp-server/pkg/http/headers"
	"github.com/go-chi/chi/v5"
)

// Handler serves the GitHub MCP Server's Server Card over HTTP.
//
// The card is public metadata: the handler requires no authentication, sets
// permissive CORS headers so browser-based clients can fetch it, and advises
// caching. It mirrors the structure of the OAuth protected-resource-metadata
// handler so the remote server repository can mount it identically and supply
// a per-environment remote URL via Config.
type Handler struct {
	cfg Config
}

// NewHandler returns a Handler that serves the card built from cfg.
func NewHandler(cfg Config) *Handler {
	return &Handler{cfg: cfg}
}

// RegisterRoutes mounts the Server Card handler at the single reserved
// `<streamable-http-url>/server-card` location. The handler is registered for
// all methods (mirroring oauth.AuthHandler) so it owns the path and answers
// non-GET requests itself rather than falling through to the auth-gated MCP
// endpoint.
//
// The card is served at exactly one canonical location — the URL the MCP/AI
// catalog links — so it is deliberately not also exposed under any alternate
// path.
func (h *Handler) RegisterRoutes(r chi.Router) {
	r.Handle(Path, h)
}

// ServeHTTP serves the Server Card as application/mcp-server-card+json.
//
// It honors GET and HEAD (with OPTIONS preflight), performs content negotiation
// against the Accept header, supports ETag conditional requests, and is safe to
// mount at <streamable-http-url>/server-card without authentication middleware.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		setCORSHeaders(w)
		w.WriteHeader(http.StatusOK)
		return
	case http.MethodGet, http.MethodHead:
		// served below
	default:
		setCORSHeaders(w)
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !acceptsCard(joinFieldLines(r.Header.Values(headers.AcceptHeader))) {
		setCORSHeaders(w)
		http.Error(w, "not acceptable: expected "+MediaType, http.StatusNotAcceptable)
		return
	}

	ServeCard(w, r, h.resolveCard(r))
}

// resolveCard builds the card for a request, applying the per-request
// RemoteURLFunc override when configured.
func (h *Handler) resolveCard(r *http.Request) *ServerCard {
	cfg := h.cfg
	if h.cfg.RemoteURLFunc != nil {
		if url := h.cfg.RemoteURLFunc(r); url != "" {
			cfg.RemoteURL = url
		}
	}
	return NewServerCard(cfg)
}

// ServeCard writes card to w as the canonical Server Card HTTP response and is
// the single source of truth for the response headers and conditional-request
// behavior. Callers that build a card per request — for example multi-tenant
// deployments that derive a per-request remote URL — can reuse it directly to
// guarantee byte-for-byte identical ETag and header handling.
//
// On a GET/HEAD it sets the read-only CORS headers, a one-hour Cache-Control,
// and a strong ETag (the lowercase-hex SHA-256 of the exact served body,
// double-quoted), plus Content-Type for the 200 response. When the request's
// If-None-Match matches that ETag (strong or weak form) or is `*`, it returns
// 304 Not Modified with the ETag and Cache-Control but no body. HEAD responses
// carry the same headers with an empty body. The caller is responsible for
// method dispatch and Accept negotiation before invoking ServeCard.
func ServeCard(w http.ResponseWriter, r *http.Request, card *ServerCard) {
	body, err := json.Marshal(card)
	if err != nil {
		setCORSHeaders(w)
		http.Error(w, "failed to encode server card", http.StatusInternalServerError)
		return
	}

	etag := computeETag(body)

	setCORSHeaders(w)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("ETag", etag)

	if ifNoneMatchSatisfied(joinFieldLines(r.Header.Values("If-None-Match")), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set(headers.ContentTypeHeader, MediaType)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

// computeETag returns a strong ETag: the lowercase-hex SHA-256 of body, wrapped
// in double quotes. It is deterministic for identical content.
func computeETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// ifNoneMatchSatisfied reports whether an If-None-Match header value matches the
// given strong ETag using RFC 9110 weak comparison: `*` always matches, and a
// listed entity-tag matches if its opaque tag equals etag's, ignoring any weak
// `W/` prefix.
func ifNoneMatchSatisfied(ifNoneMatch, etag string) bool {
	ifNoneMatch = strings.TrimSpace(ifNoneMatch)
	if ifNoneMatch == "" {
		return false
	}
	if ifNoneMatch == "*" {
		return true
	}

	target := strings.TrimPrefix(etag, "W/")
	for candidate := range strings.SplitSeq(ifNoneMatch, ",") {
		if strings.TrimPrefix(strings.TrimSpace(candidate), "W/") == target {
			return true
		}
	}
	return false
}

// setCORSHeaders applies the read-only CORS + caching headers required by the
// experimental-ext-server-card discovery spec. The card is public metadata, so
// any origin may read it; If-None-Match is allowed, ETag exposed, and Vary:
// Accept marks the negotiated body. Applied uniformly to every response path.
func setCORSHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET")
	h.Set("Access-Control-Allow-Headers", "Content-Type, If-None-Match")
	h.Set("Access-Control-Expose-Headers", "ETag")
	h.Set("Vary", "Accept")
}

// joinFieldLines merges a list-valued header's repeated field-lines, since net/http's Header.Get returns only the first.
func joinFieldLines(values []string) string {
	return strings.Join(values, ",")
}

// acceptsCard reports whether an Accept header value permits the Server Card
// media type, honoring RFC 9110 quality values. An empty header accepts
// anything. Otherwise the most specific matching media range decides the
// result — the exact card type over application/* over */* — and an explicit
// q=0 on that range rejects the representation (yielding 406).
func acceptsCard(accept string) bool {
	if strings.TrimSpace(accept) == "" {
		return true
	}

	// Among the media ranges that match the card, RFC 9110 gives precedence to
	// the most specific: exact type (2) over application/* (1) over */* (0). The
	// quality of that most specific match decides acceptability; q=0 rejects.
	bestSpecificity := -1
	var bestQuality float64
	for part := range strings.SplitSeq(accept, ",") {
		mediaRange, quality := parseMediaRange(part)
		specificity := -1
		switch mediaRange {
		case MediaType:
			specificity = 2
		case "application/*":
			specificity = 1
		case "*/*":
			specificity = 0
		}
		if specificity > bestSpecificity {
			bestSpecificity = specificity
			bestQuality = quality
		}
	}
	return bestSpecificity >= 0 && bestQuality > 0
}

// parseMediaRange splits one Accept media range into its lowercased media type
// and quality value. The quality defaults to 1.0 when no valid q parameter is
// present; only the q parameter is interpreted and other parameters are ignored.
func parseMediaRange(part string) (mediaType string, quality float64) {
	segments := strings.Split(part, ";")
	mediaType = strings.ToLower(strings.TrimSpace(segments[0]))
	quality = 1.0
	for _, param := range segments[1:] {
		param = strings.ToLower(strings.TrimSpace(param))
		if value, ok := strings.CutPrefix(param, "q="); ok {
			if q, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
				quality = q
			}
		}
	}
	return mediaType, quality
}
