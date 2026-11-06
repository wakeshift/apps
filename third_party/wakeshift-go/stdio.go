package wakeshift

// Stdio without Handler. Handler strips Wakeshift's arguments from the
// HTTP body before the MCP library parses it; on stdio, Attachment.Stdin
// does the same to each line the server reads, so no middleware or hook
// of the vendor's sees them either. The values it takes wait in the
// attachment (pendingArgs) until the call's tool takes them, matched by
// the tool and its arguments without them.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Stdin is r, the stdin of a stdio server, with Wakeshift's arguments
// taken out of every tool call before the MCP library reads it, as Handler
// does on HTTP: no receiving middleware, hook or tool handler middleware
// of yours sees them, whenever it was added. On go-sdk, serve stdio with
// StdioTransport, which uses it (mcp-go: mcpgo.ServeStdio). Closing it
// closes r if r is an io.Closer. On a disabled client it reads r as it is.
func (at *Attachment) Stdin(r io.Reader) io.ReadCloser {
	if at == nil || !at.app.Enabled() {
		return readCloser{r}
	}
	return &stdinReader{at: at, src: bufio.NewReader(r), r: r}
}

// StdioTransport is go-sdk's stdio transport with Stdin in front, so no
// middleware of yours sees Wakeshift's arguments on stdio:
//
//	srv.Run(ctx, attached.StdioTransport())
//
// Like mcp.StdioTransport, it never closes the process's stdin or stdout
// when the session ends (an mcp.IOTransport given os.Stdout itself would
// close stdout). On a disabled client it's a plain stdio transport.
func (at *Attachment) StdioTransport() *mcp.IOTransport {
	return &mcp.IOTransport{Reader: at.Stdin(struct{ io.Reader }{os.Stdin}), Writer: nopWriteCloser{os.Stdout}}
}

// nopWriteCloser is a writer whose Close does nothing.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// readCloser is a reader whose Close closes it if it can.
type readCloser struct{ io.Reader }

func (r readCloser) Close() error {
	if c, ok := r.Reader.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// stdinReader is Stdin's reader: line by line (stdio's framing), each
// tool call without Wakeshift's arguments.
type stdinReader struct {
	at      *Attachment
	src     *bufio.Reader
	r       io.Reader
	pending []byte
	err     error
}

func (s *stdinReader) Read(p []byte) (int, error) {
	for len(s.pending) == 0 {
		if s.err != nil {
			return 0, s.err
		}
		line, err := s.src.ReadBytes('\n')
		s.err = err
		if len(line) > 0 {
			s.pending = s.at.stripLine(line)
		}
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

func (s *stdinReader) Close() error { return readCloser{s.r}.Close() }

// stripLine is one stdio line with Wakeshift's arguments taken out of its
// tool calls, kept for the calls' tools (stdinArgs).
func (at *Attachment) stripLine(line []byte) []byte {
	body := bytes.TrimRight(line, "\r\n")
	out := stripMessages(body, at.set.wait, func(tool string, args json.RawMessage, w wakeArgs) {
		at.stdinArgs.put(callKey(tool, args), w)
	})
	if len(out) == len(body) && bytes.Equal(out, body) {
		return line
	}
	return append(out, '\n')
}

// pendingArgs are Wakeshift's arguments taken from tool calls before
// their tools ran, by callKey, oldest first. A call's tool takes its own
// by exact match, never another call's. At most maxPending wait: a call
// whose tool never asks (an unknown tool) doesn't hold them forever.
type pendingArgs struct {
	mu   sync.Mutex
	m    map[string][]wakeArgs
	keys []string // in order put, for the cap
}

const maxPending = 256

func (p *pendingArgs) put(key string, w wakeArgs) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string][]wakeArgs{}
	}
	if len(p.keys) >= maxPending {
		old := p.keys[0]
		p.keys = p.keys[1:]
		if l := p.m[old]; len(l) > 1 {
			p.m[old] = l[1:]
		} else {
			delete(p.m, old)
		}
	}
	p.m[key] = append(p.m[key], w)
	p.keys = append(p.keys, key)
}

// take is the oldest arguments put under key, if any.
func (p *pendingArgs) take(key string) (wakeArgs, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	l := p.m[key]
	if len(l) == 0 {
		return wakeArgs{}, false
	}
	w := l[0]
	if len(l) > 1 {
		p.m[key] = l[1:]
	} else {
		delete(p.m, key)
	}
	for i, k := range p.keys {
		if k == key {
			p.keys = append(p.keys[:i:i], p.keys[i+1:]...)
			break
		}
	}
	return w, true
}
