// The server: one Forge, one HTTP server. Routing, auth and the request-id
// header live here; the four surfaces live in git.go, rest.go, graphql.go
// and mcp.go. Every reply carries X-GitHub-Request-Id, including 401s and 404s.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const maxBodyBytes = 64 * 1024 * 1024

var requestCounter atomic.Int64

func nextRequestID() string {
	n := requestCounter.Add(1)
	return fmt.Sprintf("req-%08x", n)
}

var tokenOnce sync.Once
var cachedToken string

// expectedToken is the bearer token this entry accepts, or "" when no token
// is configured. Read once: the token is fixed before the server starts and
// never changes during a trial.
func expectedToken() string {
	tokenOnce.Do(func() {
		cachedToken = os.Getenv("MAKEFUN_SHOP_TOKEN")
	})
	return cachedToken
}

func authorized(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if auth == "" || !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	token := auth[len("Bearer "):]
	if token == "" {
		return false
	}
	expected := expectedToken()
	if expected == "" {
		return true
	}
	return token == expected
}

var errBodyTooLarge = io.ErrShortBuffer

func readBodyCapped(rc io.ReadCloser, limit int) ([]byte, error) {
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, errBodyTooLarge
	}
	return data, nil
}

// splitPath splits a pathname on '/', drops empties, and decodes each
// segment containing '%' (keeping it raw when decoding fails).
func splitPath(pathname string) []string {
	var out []string
	for _, s := range strings.Split(pathname, "/") {
		if s == "" {
			continue
		}
		if strings.Contains(s, "%") {
			if decoded, err := url.PathUnescape(s); err == nil {
				s = decoded
			}
		}
		out = append(out, s)
	}
	return out
}

type app struct {
	forge *Forge
}

func writeResult(w http.ResponseWriter, result *HandlerResult) {
	for k, v := range result.Headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("X-GitHub-Request-Id", nextRequestID())
	w.Header().Set("Content-Length", strconv.Itoa(len(result.Body)))
	w.WriteHeader(result.Status)
	if len(result.Body) > 0 {
		_, _ = w.Write(result.Body)
	}
}

func (a *app) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	result := func() (res *HandlerResult) {
		defer func() {
			if rec := recover(); rec != nil {
				res = errorResult(500, "Internal Server Error")
			}
		}()
		return a.dispatch(r)
	}()
	writeResult(w, result)
}

func (a *app) dispatch(r *http.Request) *HandlerResult {
	escapedPath := r.URL.EscapedPath()
	segments := splitPath(escapedPath)
	method := r.Method

	// The env's boot check calls this twice: 200 {"ok":true}. No auth here.
	if method == "GET" && escapedPath == "/healthz" {
		return jsonResult(200, map[string]any{"ok": true})
	}

	// Everything else needs a bearer token (any non-empty one when the env
	// did not set MAKEFUN_SHOP_TOKEN, the exact one when it did).
	if !authorized(r) {
		return jsonResult(401, map[string]any{"message": "Bad credentials"})
	}

	var body []byte
	if method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE" {
		b, err := readBodyCapped(r.Body, maxBodyBytes)
		if err != nil {
			return errorResult(413, "Request Entity Too Large")
		}
		body = b
	}

	headers := map[string]string{}
	for k, v := range r.Header {
		if len(v) > 0 {
			headers[strings.ToLower(k)] = v[0]
		}
	}
	query := map[string]string{}
	for k, v := range r.URL.Query() {
		if len(v) > 0 {
			query[k] = v[0]
		}
	}
	host := r.Host
	if host == "" {
		host = "127.0.0.1"
	}

	// MCP: a body that is not JSON is a JSON-RPC parse error.
	if len(segments) == 1 && segments[0] == "mcp" {
		var js any
		if len(body) > 0 {
			if err := json.Unmarshal(body, &js); err != nil {
				return jsonResult(400, map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "Parse error: request body is not valid JSON"}})
			}
		}
		return handleMcp(a.forge, mcpRequest{method: method, json: js})
	}

	// GraphQL v4.
	if len(segments) == 2 && segments[0] == "api" && segments[1] == "graphql" {
		var js any
		if len(body) > 0 {
			if err := json.Unmarshal(body, &js); err != nil {
				return jsonResult(400, map[string]any{"errors": []any{map[string]any{"message": "Problems parsing JSON"}}})
			}
		}
		return handleGraphql(a.forge, graphqlRequest{method: method, json: js})
	}

	// REST v3.
	if len(segments) >= 2 && segments[0] == "api" && segments[1] == "v3" {
		var js any
		if len(body) > 0 {
			if err := json.Unmarshal(body, &js); err != nil {
				return errorResult(400, "Problems parsing JSON")
			}
		}
		return handleRest(a.forge, restRequest{method: method, segments: segments[2:], query: query, json: js, host: host})
	}

	// git smart HTTP: /{owner}/{repo}.git/...
	if len(segments) >= 2 && strings.HasSuffix(segments[1], ".git") {
		return handleGit(a.forge, gitRequest{method: method, segments: segments, query: query, headers: headers, body: body})
	}

	return jsonResult(404, map[string]any{"message": "Not Found"})
}
