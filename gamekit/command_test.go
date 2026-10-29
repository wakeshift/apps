package gamekit_test

import (
	"context"
	"strings"
	"testing"

	"github.com/wakeshift/apps/gamekit"
)

// A game's command names the variables it's missing, with the game's own
// prefix, and checks the shared ones before opening the database.
func TestLoadEnvRefuses(t *testing.T) {
	for _, k := range []string{"WAKESHIFT_URL", "NIGHTFALL_URL", "NIGHTFALL_KEY", "NIGHTFALL_DATABASE_URL", "GAMES_SHARED_SEAT_WORKSPACES", "HOUSE_SECRET"} {
		t.Setenv(k, "")
	}
	_, err := gamekit.LoadEnv(context.Background(), "nightfall", nil)
	if err == nil || !strings.Contains(err.Error(), "WAKESHIFT_URL, NIGHTFALL_URL, NIGHTFALL_KEY and NIGHTFALL_DATABASE_URL are required") {
		t.Fatalf("nothing set: %v", err)
	}

	t.Setenv("WAKESHIFT_URL", "https://wakeshift.example.test")
	t.Setenv("NIGHTFALL_URL", "https://nf.example.test/")
	t.Setenv("NIGHTFALL_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("NIGHTFALL_DATABASE_URL", "postgres://nowhere.invalid/db")
	t.Setenv("GAMES_SHARED_SEAT_WORKSPACES", "not-a-uuid")
	if _, err := gamekit.LoadEnv(context.Background(), "nightfall", nil); err == nil || !strings.HasPrefix(err.Error(), "GAMES_SHARED_SEAT_WORKSPACES: ") {
		t.Fatalf("a bad workspace: %v", err)
	}
	t.Setenv("GAMES_SHARED_SEAT_WORKSPACES", "")
	t.Setenv("HOUSE_SECRET", "too short")
	if _, err := gamekit.LoadEnv(context.Background(), "nightfall", nil); err == nil || !strings.Contains(err.Error(), "HOUSE_SECRET") {
		t.Fatalf("a short house secret: %v", err)
	}
}
