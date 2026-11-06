package wakeshift

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wakeshift/wakeshift-go/internal/appkey"
)

// testKeyS is app-keys.json's S key, a current key (it names no app).
const testKeyS = "wst_1116qJFWMMHFy3xDdLmvUeyc2S6FrWRhJP51HsvDYdz9fWXzSV"

// oldKey is an older test key naming app id (the S key's seed).
func oldKey(id string) string {
	k, err := appkey.Parse(testKeyS)
	if err != nil {
		panic(err)
	}
	return appkey.FormatOld(appkey.Test, id, k.Seed)
}

// testApp is an enabled client for app "app-test" against h, with its own
// HTTP client (so a test sees its own connections).
func testApp(t *testing.T, h http.Handler, opts ...Option) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	all := append([]Option{WithKey(oldKey("app-test")), WithCore(srv.URL), WithHTTPClient(&http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}})}, opts...)
	c, err := New(all...)
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

// clientAt is an enabled client for app "app-test" at core.
func clientAt(t *testing.T, core string, opts ...Option) *Client {
	t.Helper()
	c, err := New(append([]Option{WithKey(oldKey("app-test")), WithCore(core)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
