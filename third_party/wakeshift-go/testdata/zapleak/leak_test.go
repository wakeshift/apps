package zapleak

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/wakeshift/wakeshift-go"
)

// zap's reflection encoder (zap.Any, zap.Reflect) prints none of the SDK's
// secrets: it goes through each type's MarshalJSON or MarshalText, or a
// String method for zap.Stringer.
func TestZapPrintsNoSecrets(t *testing.T) {
	const (
		key     = "wst_1116qJFWMMHFy3xDdLmvUeyc2S6FrWRhJP51HsvDYdz9fWXzSV"
		session = "wsc_canarysessioncanarysession"
		bearer  = "canary-bearer-7f3a9c2e1b"
	)
	wake, err := wakeshift.New(wakeshift.WithKey(key), wakeshift.WithCore("https://core.example"))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	enc := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
	logger := zap.New(zapcore.NewCore(enc, zapcore.AddSync(&buf), zapcore.DebugLevel))
	req := wakeshift.Request{Token: &auth.TokenInfo{UserID: "ada", Extra: map[string]any{"raw": bearer}}, Header: http.Header{"Authorization": {"Bearer " + bearer}}}
	pu := wakeshift.PublishURL{ID: "p1", Event: "e", URL: "https://app.wakeshift.example/p/wsp_canarypublishurlcanary"}
	for _, v := range []any{wakeshift.SessionKey(session), wakeshift.Subscription{Session: wakeshift.SessionKey(session)}, req,
		wakeshift.Subscriber{User: "ada", Request: req}, pu, &wakeshift.Agent{Workspace: "ws", ID: "a"}} {
		logger.Info("x", zap.Any("v", v), zap.Reflect("r", v))
	}
	logger.Info("x", zap.Stringer("wake", wake), zap.Stringer("k", wakeshift.SessionKey(session)), zap.Stringer("p", pu))
	logger.Sync()
	out := buf.String()
	for _, canary := range []string{key, key[4:20], session, bearer, "wsp_canarypublishurlcanary"} {
		if strings.Contains(out, canary) {
			t.Errorf("zap printed %q:\n%s", canary, out)
		}
	}
	if !strings.Contains(out, "[redacted]") {
		t.Fatalf("nothing logged:\n%s", out)
	}
}
