package appkit

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRPCMethodAndFirstMessage(t *testing.T) {
	for body, want := range map[string]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"post","arguments":{"text":"secret"}}}`: "tools/call/post",
		`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`:                                                            "ping/",
		`{"jsonrpc":"2.0","id":7,"result":{}}`:                                                                  "(response)/",
		`not json`:                                                                                              "?/",
	} {
		if m, tool := rpcMethod([]byte(body)); m+"/"+tool != want {
			t.Errorf("%s: %s/%s", body, m, tool)
		}
	}
	for chunk, want := range map[string]string{
		"event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"url\":\"wss://secret\"}}\n\n": "result",
		"event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":0,\"method\":\"roots/list\"}\n\n":             "roots/list",
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"x"}}`:                                 "error",
	} {
		if got := firstMessage([]byte(chunk)); got != want {
			t.Errorf("%q: %s", chunk, got)
		}
	}
}

// Logging peeks at the body but passes the whole of it on, however large.
func TestLogMCPKeepsTheBody(t *testing.T) {
	big := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"post","arguments":{"text":"` + strings.Repeat("x", 3<<20) + `"}}}`
	var got int
	h := LogMCP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = len(b)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/mcp", strings.NewReader(big)))
	if got != len(big) {
		t.Fatalf("the handler got %d of %d bytes", got, len(big))
	}
}

// Logging runs before authentication, so it reads only the start of a
// body, and still finds the method and tool of a long call.
func TestLogMCPPeeksOnlyTheStart(t *testing.T) {
	big := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"post","arguments":{"text":"` + strings.Repeat("x", 1<<20) + `"}}}`
	if m, tool := rpcMethod([]byte(big[:peekBytes])); m != "tools/call" || tool != "post" {
		t.Fatalf("cut short: %s/%s", m, tool)
	}
	if m, tool := rpcMethod([]byte(`{"jsonrpc":"2.0","id":1,"params":{"arguments":{"text":"xx`)); m != "?" || tool != "" {
		t.Fatalf("cut before the method: %s/%s", m, tool)
	}
	body := &countingReader{r: strings.NewReader(big)}
	var before int
	h := LogMCP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { before = body.n }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/mcp", body))
	if before > peekBytes {
		t.Fatalf("read %d bytes before the handler", before)
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}
