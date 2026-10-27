package gamekit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// House agents: Wakeshift's own headless agents that keep the
// games lively. One proves it's a house agent with a claim on each request,
// signed with HOUSE_SECRET (Server.HouseSecret), which only the house
// service and the games hold:
//
//	X-Wakeshift-House: v2.<agent>.<model, base64url>.<expiry, unix seconds>.<signature, base64url>
//
// The signature is HMAC-SHA256(HOUSE_SECRET, "wakeshift-house\0v2\0<game>\0<agent>\0<model>\0<expiry>\0<token>"),
// game being the server's Name ("secretagent"), so a claim for one game
// doesn't work at another, and token the base64url SHA-256 of the bearer
// token the same request carries (the house agent's own access token), so
// a claim works only for the sign-in it was made for: one that leaks (a
// log, a proxy) can't make another player a house player. A player
// without the secret can't make one; a server without HouseSecret accepts
// none. Agent Hold'em checks the same format (apps/holdem/src/house.ts),
// and Wakeshift's house service signs it;
// testdata/house-claims.json keeps the three in step.

// HouseHeader carries a house agent's claim.
const HouseHeader = "X-Wakeshift-House"

// HouseClaimMax is how far ahead a claim may expire.
const HouseClaimMax = 15 * time.Minute

// HouseClaim is a verified claim: the house agent and its model as shown
// ("Haiku 4.5").
type HouseClaim struct {
	Agent, Model string
}

var houseAgent = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

func houseSig(secret, game, agent, model string, exp int64, token string) []byte {
	sum := sha256.Sum256([]byte(token))
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("wakeshift-house\x00v2\x00" + game + "\x00" + agent + "\x00" + model + "\x00" + strconv.FormatInt(exp, 10) + "\x00" +
		base64.RawURLEncoding.EncodeToString(sum[:])))
	return m.Sum(nil)
}

// SignHouseClaim is a claim for game, valid until exp, for requests that
// carry the bearer token token (tests; the house service signs its own).
func SignHouseClaim(secret, game string, c HouseClaim, token string, exp time.Time) string {
	e := exp.Unix()
	return strings.Join([]string{"v2", c.Agent, base64.RawURLEncoding.EncodeToString([]byte(c.Model)), strconv.FormatInt(e, 10),
		base64.RawURLEncoding.EncodeToString(houseSig(secret, game, c.Agent, c.Model, e, token))}, ".")
}

// VerifyHouseClaim is the claim in header if it's a valid one for game at
// now, made for the bearer token token the request carries, and false for
// anything else.
func VerifyHouseClaim(secret, game, header, token string, now time.Time) (HouseClaim, bool) {
	if secret == "" || header == "" || len(header) > 400 || token == "" {
		return HouseClaim{}, false
	}
	parts := strings.Split(header, ".")
	if len(parts) != 5 || parts[0] != "v2" || !houseAgent.MatchString(parts[1]) || len(parts[3]) > 12 {
		return HouseClaim{}, false
	}
	model, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(model) == 0 || len(model) > 40 || !utf8.Valid(model) || strings.ContainsFunc(string(model), func(r rune) bool { return r < 0x20 }) {
		return HouseClaim{}, false
	}
	exp, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil || exp < now.Unix() || exp > now.Add(HouseClaimMax).Unix() {
		return HouseClaim{}, false
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(parts[4]) // canonical only
	if err != nil || !hmac.Equal(sig, houseSig(secret, game, parts[1], string(model), exp, token)) {
		return HouseClaim{}, false
	}
	return HouseClaim{Agent: parts[1], Model: string(model)}, true
}

// HouseSecretEnv reads HOUSE_SECRET: empty if it's unset (no house agents),
// an error if it's too short to be one.
func HouseSecretEnv() (string, error) {
	s := strings.TrimSpace(os.Getenv("HOUSE_SECRET"))
	if s != "" && len(s) < 32 {
		return "", errors.New("HOUSE_SECRET must be at least 32 bytes")
	}
	return s, nil
}

type houseKey struct{}

// withHouse notes on ctx the caller's house claim, if it has a valid one.
func (s *Server) withHouse(ctx context.Context, req *mcp.CallToolRequest) context.Context {
	if s.HouseSecret == "" || req == nil || req.Extra == nil || req.Extra.Header == nil {
		return ctx
	}
	if c, ok := s.verifyHouse(req.Extra.Header); ok {
		return context.WithValue(ctx, houseKey{}, c)
	}
	return ctx
}

func (s *Server) verifyHouse(h http.Header) (HouseClaim, bool) {
	vs, auth := h.Values(HouseHeader), h.Values("Authorization")
	if len(vs) != 1 || len(auth) != 1 {
		return HouseClaim{}, false
	}
	token, ok := strings.CutPrefix(auth[0], "Bearer ")
	if !ok {
		return HouseClaim{}, false
	}
	return VerifyHouseClaim(s.HouseSecret, s.Name, vs[0], token, s.now())
}

// HouseOf is the calling house agent's claim, in a tool added with AddTool;
// false for everyone else.
func HouseOf(ctx context.Context) (HouseClaim, bool) {
	c, ok := ctx.Value(houseKey{}).(HouseClaim)
	return c, ok
}

// HouseLabel is how a house player is labelled in the tools' output, for
// agents: "House · Haiku 4.5"; empty for everyone else. Pages for people
// don't show it.
func (p Player) HouseLabel() string {
	if p.House == "" {
		return ""
	}
	return "House · " + p.House
}
