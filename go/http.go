// http.go — small shared helpers for the HTTP layer: the request context,
// JSON/text/buffer replies, and the bearer-token check. No business logic.

package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Ctx carries one request's parsed inputs plus the store.
type Ctx struct {
	Store    *Store
	Method   string
	Path     string
	Segments []string
	Query    *OrderedQuery
	Headers  http.Header
	// Host is the request's Host header (with port). Go's net/http keeps
	// it in r.Host, not in r.Header, so it is carried separately.
	Host      string
	Body      []byte
	RequestID string
}

// OrderedQuery is a query string parsed in order (like URLSearchParams):
// Get returns the first value for a key; Entries preserves order.
type OrderedQuery struct {
	Pairs []QPair
}

type QPair struct {
	K string
	V string
}

// ParseQuery parses a raw query string in order, decoding %XX and + as space.
func ParseQuery(raw string) *OrderedQuery {
	q := &OrderedQuery{}
	if raw == "" {
		return q
	}
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		k, v := part, ""
		if i := strings.Index(part, "="); i >= 0 {
			k, v = part[:i], part[i+1:]
		}
		q.Pairs = append(q.Pairs, QPair{K: urlDecode(k), V: urlDecode(v)})
	}
	return q
}

// urlDecode decodes application/x-www-form-urlencoded pieces (+ -> space).
func urlDecode(s string) string {
	var buf bytes.Buffer
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '+':
			buf.WriteByte(' ')
		case '%':
			if i+2 < len(s) {
				if n, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
					buf.WriteByte(byte(n))
					i += 2
					continue
				}
			}
			buf.WriteByte(s[i])
		default:
			buf.WriteByte(s[i])
		}
	}
	return buf.String()
}

func (q *OrderedQuery) Get(key string) string {
	for _, p := range q.Pairs {
		if p.K == key {
			return p.V
		}
	}
	return ""
}

// Has reports whether the key is present at all.
func (q *OrderedQuery) Has(key string) bool {
	for _, p := range q.Pairs {
		if p.K == key {
			return true
		}
	}
	return false
}

// Entries returns all pairs in order.
func (q *OrderedQuery) Entries() []QPair { return q.Pairs }

// ApiError is an error with an HTTP status, rendered as {"message": ...}.
type ApiError struct {
	Status  int
	Message string
}

func (e *ApiError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message) }

func apiErr(status int, msg string) *ApiError { return &ApiError{Status: status, Message: msg} }

// writeJSON writes a JSON body with the forge's standard headers.
func writeJSON(w http.ResponseWriter, status int, body []byte, extra map[string]string) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	for k, v := range extra {
		h.Set(k, v)
	}
	w.WriteHeader(status)
	w.Write(body)
}

// sendJSON serializes a JVal and writes it.
func sendJSON(w http.ResponseWriter, status int, obj *JVal, extra ...map[string]string) {
	writeJSON(w, status, obj.Marshal(), firstExtra(extra))
}

// sendJSONString sends an already-serialized JSON string (a cached issue
// body, byte-identical).
func sendJSONString(w http.ResponseWriter, status int, body string, extra ...map[string]string) {
	writeJSON(w, status, []byte(body), firstExtra(extra))
}

// sendBuffer writes raw bytes with an explicit Content-Length (Go's
// net/http would otherwise chunk large bodies and drop the length).
func sendBuffer(w http.ResponseWriter, status int, buf []byte, contentType string) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.Itoa(len(buf)))
	w.WriteHeader(status)
	w.Write(buf)
}

// sendEmpty writes a header-only reply.
func sendEmpty(w http.ResponseWriter, status int, extra ...map[string]string) {
	h := w.Header()
	h.Set("Content-Length", "0")
	for k, v := range firstExtra(extra) {
		h.Set(k, v)
	}
	w.WriteHeader(status)
}

// firstExtra returns the first extra-header map, or nil.
func firstExtra(extra []map[string]string) map[string]string {
	if len(extra) > 0 {
		return extra[0]
	}
	return nil
}

// bearerToken extracts the bearer token on the request, or "".
func bearerToken(ctx *Ctx) string {
	header := ctx.Headers.Get("Authorization")
	if header == "" {
		return ""
	}
	// v2: /^Bearer\s+(.+)$/ — one or more spaces, then the token.
	if !strings.HasPrefix(header, "Bearer") {
		return ""
	}
	rest := header[len("Bearer"):]
	if len(rest) == 0 || (rest[0] != ' ' && rest[0] != '\t') {
		return ""
	}
	return strings.TrimSpace(rest)
}

// isAuthorized reports whether the request carries the suite's shop token.
func isAuthorized(ctx *Ctx) bool {
	token := bearerToken(ctx)
	return token != "" && ctx.Store.Token != "" && token == ctx.Store.Token
}

// parseJSONBody parses the request body as JSON; throws ApiError(400) when
// malformed. An empty body parses as an empty object, like JSON.parse(”).
func parseJSONBody(ctx *Ctx) (*JVal, error) {
	if len(ctx.Body) == 0 {
		return JObjVal(), nil
	}
	v, err := ParseJSON(ctx.Body)
	if err != nil {
		return nil, apiErr(400, "Problems parsing JSON")
	}
	return v, nil
}

// readBody reads the full request body into a buffer, capped so a garbage
// request cannot grow memory without bound.
func readBody(r *http.Request) ([]byte, error) {
	const maxBodyBytes = 64 * 1024 * 1024
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBodyBytes {
		return nil, apiErr(413, "request body too large")
	}
	return body, nil
}
