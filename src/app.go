package main

// The forge's HTTP server: one process answering git smart HTTP, REST v3,
// GraphQL and MCP from a single in-memory store. CreateServer() builds the
// server; main.go owns the harness lifecycle. (Port of entry/app.ts +
// entry/ctx.ts.)

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Request context (port of entry/ctx.ts)

// Sender is the response callback the git smart-HTTP handlers write through.
type Sender func(status int, contentType string, body []byte)

// Ctx is the request context every surface handler shares, built once by the
// server handler below.
type Ctx struct {
	Method string
	// Path is the URL pathname, still percent-encoded per segment.
	Path string
	// Segments is the pathname split into decoded segments (empties dropped).
	Segments []string
	Query    url.Values
	// Body is the raw request body.
	Body []byte
	// JSON is the parsed JSON body when the request carried one; nil when the
	// request carried none.
	JSON interface{}
	// JSONErr is true when a body was present but malformed (TS
	// JSON_PARSE_ERROR).
	JSONErr bool
	// Origin is the absolute origin of this server as the client addressed
	// it, for Link headers: "http://<host>".
	Origin string
	Req    *http.Request

	w       http.ResponseWriter
	reqID   string
	respond bool
}

// Send writes a complete response: status, content type, content length and
// the request id header, then the body.
func (c *Ctx) Send(status int, contentType string, body []byte) {
	c.SendH(status, contentType, body, nil)
}

// SendH is Send with extra response headers (pagination Link, OAuth scopes).
func (c *Ctx) SendH(status int, contentType string, body []byte, headers map[string]string) {
	if c.w == nil || c.respond {
		return
	}
	c.respond = true
	h := c.w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("X-GitHub-Request-Id", c.reqID)
	for k, v := range headers {
		h.Set(k, v)
	}
	c.w.WriteHeader(status)
	_, _ = c.w.Write(body)
}

// SendJSON serializes v the way ctx.sendJson does in TS: JSON with HTML
// escaping off, plus a trailing newline.
func (c *Ctx) SendJSON(status int, v interface{}) {
	c.SendJSONH(status, v, nil)
}

// SendJSONH is SendJSON with extra response headers.
func (c *Ctx) SendJSONH(status int, v interface{}, headers map[string]string) {
	body := encodeJSONValue(v)
	body = append(body, '\n')
	c.SendH(status, "application/json; charset=utf-8", body, headers)
}

// encodeJSONValue renders one JSON value with HTML escaping disabled and no
// trailing newline (json.Encoder.Encode adds one; it is trimmed).
func encodeJSONValue(v interface{}) []byte {
	if v == nil {
		return []byte("null")
	}
	if raw, ok := v.(json.RawMessage); ok {
		return []byte(raw)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte("null")
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// ---------------------------------------------------------------------------
// Ordered JSON objects
//
// TS serializers build object literals whose key order JSON.stringify
// preserves. The suite parses JSON so order is not load-bearing, but the Go
// port mirrors it anyway: an orderedObj marshals its keys in insertion
// order.

// OrderedObj is a JSON object with insertion-ordered keys.
type OrderedObj struct {
	keys []string
	vals []interface{}
	idx  map[string]int
}

// NewOrderedObj builds an OrderedObj from alternating key, value pairs.
func NewOrderedObj(pairs ...interface{}) *OrderedObj {
	o := &OrderedObj{idx: make(map[string]int)}
	for i := 0; i+1 < len(pairs); i += 2 {
		key, _ := pairs[i].(string)
		o.Set(key, pairs[i+1])
	}
	return o
}

// Set inserts key, or replaces its value in place when already present
// (matching JS object assignment / spread-override semantics).
func (o *OrderedObj) Set(key string, value interface{}) *OrderedObj {
	if o.idx == nil {
		o.idx = make(map[string]int)
	}
	if pos, ok := o.idx[key]; ok {
		o.vals[pos] = value
		return o
	}
	o.idx[key] = len(o.keys)
	o.keys = append(o.keys, key)
	o.vals = append(o.vals, value)
	return o
}

// Get returns the value stored under key.
func (o *OrderedObj) Get(key string) (interface{}, bool) {
	pos, ok := o.idx[key]
	if !ok {
		return nil, false
	}
	return o.vals[pos], true
}

// MarshalJSON renders the object with keys in insertion order. Scalars take
// a fast path straight into the buffer (the previous version built a fresh
// json.Encoder per key and per value — dozens of allocations per response).
func (o *OrderedObj) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, key := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		appendJSONScalar(&buf, key)
		buf.WriteByte(':')
		appendJSONValue(&buf, o.vals[i])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// appendJSONValue writes v into buf: scalars inline, composites via the
// general encoder (which recurses into MarshalJSON for nested OrderedObjs).
func appendJSONValue(buf *bytes.Buffer, v interface{}) {
	switch v.(type) {
	case nil, string, bool, int, int64, json.Number:
		appendJSONScalar(buf, v)
	case float64:
		t := v.(float64)
		// Integral floats render as integers, like JSON.stringify.
		if t == float64(int64(t)) && t < 9e15 && t > -9e15 {
			var tmp [20]byte
			buf.Write(strconv.AppendInt(tmp[:0], int64(t), 10))
		} else {
			buf.Write(encodeJSONValue(v))
		}
	default:
		buf.Write(encodeJSONValue(v))
	}
}

// appendJSONScalar writes a string/bool/int/json.Number/nil without an
// Encoder. Strings with exotic characters fall back to the encoder so the
// escaping matches encoding/json exactly.
func appendJSONScalar(buf *bytes.Buffer, v interface{}) {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case int:
		var tmp [20]byte
		buf.Write(strconv.AppendInt(tmp[:0], int64(t), 10))
	case int64:
		var tmp [20]byte
		buf.Write(strconv.AppendInt(tmp[:0], t, 10))
	case json.Number:
		buf.WriteString(string(t))
	case string:
		if jsonStringIsPlain(t) {
			buf.WriteByte('"')
			buf.WriteString(t)
			buf.WriteByte('"')
		} else {
			buf.Write(encodeJSONValue(t))
		}
	default:
		buf.Write(encodeJSONValue(v))
	}
}

// jsonStringIsPlain reports whether s needs no JSON escaping beyond the
// surrounding quotes (printable ASCII, no quote/backslash, no DEL, no
// non-ASCII — those fall back to the encoder).
func jsonStringIsPlain(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == '"' || c == '\\' || c == 0x7f || c >= 0x80 {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// ctx.ts field helpers

// IsRecord reports whether v is a JSON object, returning it as a map.
func IsRecord(v interface{}) (map[string]interface{}, bool) {
	m, ok := v.(map[string]interface{})
	return m, ok
}

// StrField returns obj[key] when it is a string.
func StrField(obj map[string]interface{}, key string) (string, bool) {
	s, ok := obj[key].(string)
	return s, ok
}

// BoolField returns obj[key] when it is a boolean.
func BoolField(obj map[string]interface{}, key string) (bool, bool) {
	b, ok := obj[key].(bool)
	return b, ok
}

// ---------------------------------------------------------------------------
// Server (port of entry/app.ts)

var forgeShopToken = os.Getenv("MAKEFUN_SHOP_TOKEN")

var forgeRequestCounter atomic.Uint64

var forgeGitPathRe = regexp.MustCompile(`^/([^/]+)/([^/]+)\.git/(info/refs|git-upload-pack|git-receive-pack)$`)

// CreateServer builds the forge's HTTP server. It binds nothing itself;
// main.go listens on :0.
//
// Node's keepAliveTimeout (30s) maps to IdleTimeout, its headersTimeout
// (35s) to ReadHeaderTimeout. Go's net/http enables TCP_NODELAY on accepted
// connections by default, as Node does, so no listener wrapping is needed.
func CreateServer() *http.Server {
	return &http.Server{
		Handler:           http.HandlerFunc(serveForge),
		ReadHeaderTimeout: 35 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
}

func serveForge(w http.ResponseWriter, r *http.Request) {
	reqID := "forge-" + padHex8(forgeRequestCounter.Add(1))
	ctx := &Ctx{
		Method: strings.ToUpper(r.Method),
		Req:    r,
		w:      w,
		reqID:  reqID,
	}
	defer func() {
		if recover() != nil {
			if !ctx.respond {
				// Mirrors app.ts's catch: only a content type, no request id,
				// and no trailing newline on the body.
				ctx.respond = true
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(500)
				_, _ = w.Write([]byte(`{"message":"Internal error"}`))
			}
		}
	}()

	rawURL := r.RequestURI
	if rawURL == "" && r.URL != nil {
		rawURL = r.URL.RequestURI()
	}
	path := rawURL
	rawQuery := ""
	if i := strings.IndexByte(rawURL, '?'); i >= 0 {
		path = rawURL[:i]
		rawQuery = rawURL[i+1:]
	}
	ctx.Path = path
	if rawQuery != "" {
		query, _ := url.ParseQuery(rawQuery)
		ctx.Query = query
	}
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	ctx.Origin = "http://" + host

	if path == "/healthz" {
		ctx.Send(200, "text/plain", []byte("ok\n"))
		return
	}

	authorized := forgeShopToken != "" && r.Header.Get("Authorization") == "Bearer "+forgeShopToken

	var body []byte
	if ctx.Method == "GET" || ctx.Method == "HEAD" {
		body = []byte{}
	} else {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			panic(err)
		}
		body = data
	}
	ctx.Body = body

	ctx.Segments = decodeSegments(path)

	// JSON body parsing is shared: a malformed body is a 400 on REST/GraphQL,
	// a -32700 on MCP.
	contentType := r.Header.Get("Content-Type")
	if len(body) > 0 && (strings.Contains(contentType, "json") || strings.HasPrefix(path, "/api/") || path == "/mcp") {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		var v interface{}
		if err := dec.Decode(&v); err != nil {
			ctx.JSONErr = true
		} else if !onlyJSONWhitespace(body[dec.InputOffset():]) {
			// Trailing data after the first value: JSON.parse would reject.
			// (Was a second Decode; scanning the tail is equivalent and
			// allocation-free.)
			ctx.JSONErr = true
			ctx.JSON = nil
		} else {
			ctx.JSON = v
		}
	}

	// Serialize store access the way Node's single thread did, while letting
	// read-only traffic (notably concurrent shallow clones through
	// git-upload-pack) run in parallel.
	if dispatchNeedsWrite(ctx) {
		StoreMu.Lock()
		defer StoreMu.Unlock()
	} else {
		StoreMu.RLock()
		defer StoreMu.RUnlock()
	}

	dispatch(ctx, authorized)
}

// dispatchNeedsWrite decides the StoreMu mode for a request: write lock for
// POST/PUT/PATCH/DELETE, except git-upload-pack (read), /api/graphql (write
// iff the body mentions "mutation") and /mcp (write iff the body mentions a
// create_/merge_ tool or "tools/call"). GET/HEAD and everything else read.
func dispatchNeedsWrite(c *Ctx) bool {
	switch c.Method {
	case "POST", "PUT", "PATCH", "DELETE":
	default:
		return false
	}
	if c.Path == "/mcp" {
		return bytes.Contains(c.Body, []byte("create_")) ||
			bytes.Contains(c.Body, []byte("merge_")) ||
			bytes.Contains(c.Body, []byte("tools/call"))
	}
	if c.Path == "/api/graphql" {
		return bytes.Contains(c.Body, []byte("mutation"))
	}
	if m := forgeGitPathRe.FindStringSubmatch(c.Path); m != nil && m[3] == "git-upload-pack" {
		return false
	}
	return true
}

func dispatch(c *Ctx, authorized bool) {
	path := c.Path

	// MCP answers its own parse error before auth, so it gets the raw
	// context first.
	if path == "/mcp" {
		if c.Method == "POST" {
			HandleMcp(c, authorized)
		} else {
			c.SendJSON(404, NewOrderedObj("message", "Not Found"))
		}
		return
	}

	// Everything else requires the bearer token.
	if !authorized {
		if strings.HasPrefix(path, "/api/") {
			c.SendJSON(401, NewOrderedObj("message", "Bad credentials"))
		} else {
			c.Send(401, "text/plain", []byte("unauthorized\n"))
		}
		return
	}

	if path == "/api/graphql" {
		if c.Method == "POST" {
			HandleGraphQL(c)
		} else {
			c.SendJSON(404, NewOrderedObj("message", "Not Found"))
		}
		return
	}

	if strings.HasPrefix(path, "/api/v3") {
		if !HandleRest(c) {
			c.SendJSON(404, NewOrderedObj("message", "Not Found"))
		}
		return
	}

	// Git smart HTTP: /{owner}/{repo}.git/...
	if m := forgeGitPathRe.FindStringSubmatch(path); m != nil {
		owner := percentDecodeOrRaw(m[1])
		name := percentDecodeOrRaw(m[2])
		action := m[3]
		var repo *RepoRec
		if owner == ViewerLogin {
			repo = S.GetRepo(owner, name)
		}
		send := Sender(func(status int, contentType string, body []byte) {
			c.Send(status, contentType, body)
		})
		switch {
		case action == "info/refs" && c.Method == "GET":
			service, _ := queryFirst(c.Query, "service")
			protocolV2 := c.Req.Header.Get("Git-Protocol") == "version=2"
			InfoRefs(repo, service, protocolV2, send)
		case action == "git-upload-pack" && c.Method == "POST":
			UploadPack(repo, c.Body, send)
		case action == "git-receive-pack" && c.Method == "POST":
			ReceivePack(repo, c.Body, send)
		default:
			c.Send(404, "text/plain", []byte("not found\n"))
		}
		return
	}

	if strings.HasPrefix(path, "/api/") {
		c.SendJSON(404, NewOrderedObj("message", "Not Found"))
		return
	}
	c.Send(404, "text/plain", []byte("not found\n"))
}

// decodeSegments splits a raw pathname into decoded segments, dropping
// empties; a segment is decoded only when it contains '%', and a failed
// decode keeps the raw segment (as app.ts does).
func decodeSegments(path string) []string {
	parts := strings.Split(path, "/")
	segments := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		if strings.Contains(part, "%") {
			if decoded, err := url.PathUnescape(part); err == nil {
				part = decoded
			}
		}
		segments = append(segments, part)
	}
	return segments
}

func percentDecodeOrRaw(s string) string {
	if decoded, err := url.PathUnescape(s); err == nil {
		return decoded
	}
	return s
}

// queryFirst mirrors URLSearchParams.get: the first value, and whether the
// key was present at all.
func queryFirst(q url.Values, key string) (string, bool) {
	vals, ok := q[key]
	if !ok || len(vals) == 0 {
		return "", false
	}
	return vals[0], true
}

// onlyJSONWhitespace reports whether b is empty or all JSON whitespace
// (space, tab, LF, CR) — the only bytes JSON.parse tolerates after a value.
func onlyJSONWhitespace(b []byte) bool {
	for _, c := range b {
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			return false
		}
	}
	return true
}

// padHex8 renders n in lowercase hex, zero-padded to at least 8 digits
// (TS: counter.toString(16).padStart(8, '0')).
func padHex8(n uint64) string {
	s := strconv.FormatUint(n, 16)
	for len(s) < 8 {
		s = "0" + s
	}
	return s
}
