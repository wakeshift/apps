package gamekit

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"
)

// The claims Agent Hold'em checks too (apps/holdem/test/house.test.ts): the
// valid ones pass with their agent and model, and nothing else does, a valid
// claim sent with another guest's token included.
func TestHouseClaimVectors(t *testing.T) {
	b, err := os.ReadFile("testdata/house-claims.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Secret, Token string
		Now           int64
		Valid         []struct{ Game, Agent, Model, Header string }
		Invalid       []struct {
			Why, Game, Header string
			Token             *string
			Now               int64
		}
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Valid {
		got, ok := VerifyHouseClaim(v.Secret, c.Game, c.Header, v.Token, time.Unix(v.Now, 0))
		if !ok || got != (HouseClaim{Agent: c.Agent, Model: c.Model}) {
			t.Errorf("%s: %v %v", c.Game, got, ok)
		}
	}
	for _, c := range v.Invalid {
		token := v.Token
		if c.Token != nil {
			token = *c.Token
		}
		if got, ok := VerifyHouseClaim(v.Secret, c.Game, c.Header, token, time.Unix(c.Now, 0)); ok {
			t.Errorf("%s: accepted as %v", c.Why, got)
		}
	}
	ok := v.Valid[0]
	if _, accepted := VerifyHouseClaim("", ok.Game, ok.Header, v.Token, time.Unix(v.Now, 0)); accepted {
		t.Error("a server without HouseSecret accepted a claim")
	}
	now := time.Now()
	fresh := SignHouseClaim(v.Secret, "nightfall", HouseClaim{Agent: "h7", Model: "Haiku 5.5"}, "tok", now.Add(HouseClaimMax))
	if got, accepted := VerifyHouseClaim(v.Secret, "nightfall", fresh, "tok", now); !accepted || got.Model != "Haiku 5.5" {
		t.Errorf("a fresh claim: %v %v", got, accepted)
	}
	if (Player{House: "Haiku 5.5"}).HouseLabel() != "House · Haiku 5.5" || (Player{}).HouseLabel() != "" {
		t.Error("labels")
	}
}

// The server checks a claim against the request's own bearer token.
func TestVerifyHouseUsesTheRequestsToken(t *testing.T) {
	const secret = "house-test-secret-0123456789abcdef0123456789"
	s := &Server{Name: "nightfall", HouseSecret: secret}
	claim := SignHouseClaim(secret, "nightfall", HouseClaim{Agent: "h1", Model: "Haiku 5.5"}, "house-token", time.Now().Add(time.Minute))
	h := func(auth ...string) http.Header {
		hd := http.Header{HouseHeader: {claim}}
		for _, a := range auth {
			hd.Add("Authorization", a)
		}
		return hd
	}
	if _, ok := s.verifyHouse(h("Bearer house-token")); !ok {
		t.Error("the house agent's own request")
	}
	for name, hd := range map[string]http.Header{
		"another guest's token": h("Bearer guest-token"),
		"no token":              h(),
		"not a bearer token":    h("Basic house-token"),
		"two tokens":            h("Bearer house-token", "Bearer house-token"),
	} {
		if _, ok := s.verifyHouse(hd); ok {
			t.Errorf("%s: accepted", name)
		}
	}
}
