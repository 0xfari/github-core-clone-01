// app.go — the HTTP server: one Store, one router, one error path.
//
// Routing:
//   GET  /healthz                       -> 200 {"ok":true} (no auth)
//   /api/v3/...                         -> REST v3 (Bearer required)
//   POST /api/graphql                   -> GraphQL (Bearer required)
//   POST /mcp                           -> MCP (Bearer required)
//   /{owner}/{repo}.git/info/refs       -> git advertisement (no auth)
//   /{owner}/{repo}.git/git-upload-pack -> git fetch/clone (no auth)
//   /{owner}/{repo}.git/git-receive-pack-> git push (no auth)
//   anything else                       -> 404 JSON {message}
//
// Every reply carries a non-empty X-GitHub-Request-Id (an incrementing
// counter, deterministic per request sequence) and X-OAuth-Scopes. One
// mutex serializes all requests, matching Node's sequential semantics.

package main

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
)

type appServer struct {
	store *Store
	mu    sync.Mutex
}

// trackedWriter records whether the status line has gone out, so the
// panic recovery knows whether an error reply is still possible.
type trackedWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (t *trackedWriter) WriteHeader(status int) {
	t.wroteHeader = true
	t.ResponseWriter.WriteHeader(status)
}

func (t *trackedWriter) Write(b []byte) (int, error) {
	t.wroteHeader = true
	return t.ResponseWriter.Write(b)
}

func parseSegments(pathname string) ([]string, error) {
	parts := strings.Split(pathname, "/")
	segs := make([]string, 0, len(parts))
	for _, seg := range parts {
		if seg == "" {
			continue
		}
		// Most segments carry no '%' at all; decoding those is identity work.
		if strings.Contains(seg, "%") {
			decoded, err := url.PathUnescape(seg)
			if err != nil {
				return nil, apiErr(400, "bad path encoding")
			}
			seg = decoded
		}
		segs = append(segs, seg)
	}
	return segs, nil
}

// mayHaveBody reports whether the request can carry a body (a length, or
// chunked encoding).
func mayHaveBody(r *http.Request) bool {
	if r.ContentLength != 0 {
		return true
	}
	return r.Header.Get("Transfer-Encoding") != ""
}

func (s *appServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tw := &trackedWriter{ResponseWriter: w}
	defer func() {
		if rec := recover(); rec != nil {
			if tw.wroteHeader {
				return
			}
			if apiErr, ok := rec.(*ApiError); ok {
				sendJSON(tw, apiErr.Status, Obj("message", JStrVal(apiErr.Message)), nil)
				return
			}
			fmt.Fprintf(os.Stderr, "[entry] unhandled error: %v\n", rec)
			sendJSON(tw, 500, Obj("message", JStrVal("Internal Server Error")), nil)
		}
	}()
	s.handle(tw, r)
}

func (s *appServer) handle(w http.ResponseWriter, r *http.Request) {
	store := s.store
	requestID := store.NextRequestID()
	w.Header().Set("x-github-request-id", requestID)
	w.Header().Set("x-oauth-scopes", "repo, read:org")

	// Skip the URL constructor: the suite's paths need only the pathname
	// and the raw query string. GET/HEAD requests from the suite never
	// carry a body, so do not wait for a body that adds nothing.
	rawURL := r.URL.RequestURI()
	q := strings.Index(rawURL, "?")
	pathname := rawURL
	queryStr := ""
	if q >= 0 {
		pathname = rawURL[:q]
		queryStr = rawURL[q+1:]
	}
	query := ParseQuery(queryStr)
	segments, err := parseSegments(pathname)
	if err != nil {
		panic(err)
	}
	method := r.Method
	var body []byte
	if (method == "GET" || method == "HEAD") && !mayHaveBody(r) {
		body = []byte{}
	} else {
		body, err = readBody(r)
		if err != nil {
			panic(err)
		}
	}
	ctx := &Ctx{
		Store:     store,
		Method:    method,
		Path:      pathname,
		Segments:  segments,
		Query:     query,
		Headers:   r.Header,
		Host:      r.Host,
		Body:      body,
		RequestID: requestID,
	}

	if len(segments) == 1 && segments[0] == "healthz" {
		sendJSON(w, 200, Obj("ok", JBoolVal(true)), nil)
		return
	}

	if len(segments) >= 2 && segments[0] == "api" && segments[1] == "v3" {
		if !isAuthorized(ctx) {
			panic(apiErr(401, "Bad credentials"))
		}
		ctx.Segments = segments[2:]
		handleRest(ctx, w)
		return
	}

	if len(segments) == 2 && segments[0] == "api" && segments[1] == "graphql" {
		if !isAuthorized(ctx) {
			panic(apiErr(401, "Bad credentials"))
		}
		if ctx.Method != "POST" {
			panic(apiErr(404, "Not Found"))
		}
		handleGraphQL(ctx, w)
		return
	}

	if len(segments) == 1 && segments[0] == "mcp" {
		if !isAuthorized(ctx) {
			panic(apiErr(401, "Bad credentials"))
		}
		handleMcp(ctx, w)
		return
	}

	if len(segments) >= 2 && strings.HasSuffix(segments[1], ".git") {
		gitPath := segments[2:]
		if len(gitPath) == 2 && gitPath[0] == "info" && gitPath[1] == "refs" && ctx.Method == "GET" {
			handleInfoRefs(ctx, w)
			return
		}
		if len(gitPath) == 1 && gitPath[0] == "git-upload-pack" && ctx.Method == "POST" {
			handleUploadPack(ctx, w)
			return
		}
		if len(gitPath) == 1 && gitPath[0] == "git-receive-pack" && ctx.Method == "POST" {
			handleReceivePack(ctx, w)
			return
		}
		panic(apiErr(404, "Not Found"))
	}

	panic(apiErr(404, "Not Found"))
}
