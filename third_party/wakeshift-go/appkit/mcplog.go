package appkit

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// LogMCP wraps an MCP endpoint to log each request: the JSON-RPC method (and
// tool name), the client, the status, how long it took, how much it sent,
// and what kind of message the response began with ("result", "error", or
// a server-to-client method). Never the content: tool results can hold
// stream links, which are credentials. A request still open after a while
// is logged then too, so one that never finishes shows what it's waiting
// on.
func LogMCP(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, tool := "", ""
		if r.Method == http.MethodPost && r.Body != nil {
			// Peek at the start for the method (this runs before the
			// handler authenticates, so it reads little); the handler
			// still gets the whole body.
			body, _ := io.ReadAll(io.LimitReader(r.Body, peekBytes))
			r.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(body), r.Body), r.Body}
			method, tool = rpcMethod(body)
		}
		lw := &logWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		attrs := func() []any {
			lw.mu.Lock()
			defer lw.mu.Unlock()
			return []any{"http_method", r.Method, "rpc", method, "tool", tool, "client", r.Header.Get("User-Agent"),
				"status", lw.status, "bytes", lw.n, "first", lw.first, "ms", time.Since(start).Milliseconds()}
		}
		slow := time.AfterFunc(15*time.Second, func() { slog.Warn("mcp: request still open", attrs()...) })
		defer slow.Stop()
		h.ServeHTTP(lw, r)
		slog.Info("mcp: request", attrs()...)
	})
}

// peekBytes is how much of a request LogMCP reads to find its method.
const peekBytes = 16 << 10

// rpcMethod is a JSON-RPC message's method, and for tools/call the tool.
// A batch reports its first message's. body may be cut short (LogMCP reads
// only its start): what came before the cut is still reported.
func rpcMethod(body []byte) (method, tool string) {
	dec := json.NewDecoder(bytes.NewReader(body))
	t, err := dec.Token()
	if err == nil && t == json.Delim('[') {
		t, err = dec.Token()
	}
	if err != nil || t != json.Delim('{') {
		return "?", ""
	}
	var result, isError bool
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			break
		}
		var v any = new(json.RawMessage)
		switch k {
		case "method":
			v = &method
		case "params":
			// Its name usually comes before the (possibly long, cut off)
			// arguments: read it key by key.
			if t, err := dec.Token(); err != nil || t != json.Delim('{') {
				return orUnknown(method), tool
			}
			for dec.More() {
				pk, err := dec.Token()
				if err != nil {
					return orUnknown(method), tool
				}
				pv := any(new(json.RawMessage))
				if pk == "name" {
					pv = &tool
				}
				if dec.Decode(pv) != nil {
					return orUnknown(method), tool
				}
			}
			if _, err := dec.Token(); err != nil {
				return orUnknown(method), tool
			}
			continue
		case "result":
			result = true
		case "error":
			isError = true
		}
		if dec.Decode(v) != nil {
			break
		}
	}
	switch {
	case method != "":
		return method, tool
	case result:
		return "(response)", ""
	case isError:
		return "(error response)", ""
	}
	return "?", ""
}

func orUnknown(method string) string {
	if method == "" {
		return "?"
	}
	return method
}

type logWriter struct {
	http.ResponseWriter
	mu     sync.Mutex
	status int
	n      int
	first  string // what the first message sent was
}

func (w *logWriter) WriteHeader(code int) {
	w.mu.Lock()
	w.status = code
	w.mu.Unlock()
	w.ResponseWriter.WriteHeader(code)
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.n += len(p)
	if w.first == "" {
		w.first = firstMessage(p)
	}
	w.mu.Unlock()
	return w.ResponseWriter.Write(p)
}

func (w *logWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// firstMessage is the kind of the first JSON-RPC message in a response
// chunk (plain JSON or an SSE data line).
func firstMessage(p []byte) string {
	s := string(p)
	if i := strings.Index(s, "data:"); i >= 0 {
		s = strings.TrimSpace(s[i+5:])
		if j := strings.IndexByte(s, '\n'); j >= 0 {
			s = s[:j]
		}
	}
	var m struct {
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(s), &m) != nil {
		return "?"
	}
	switch {
	case m.Method != "":
		return m.Method
	case m.Error != nil:
		return "error"
	case m.Result != nil:
		return "result"
	}
	return "?"
}
