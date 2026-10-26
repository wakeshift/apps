package sketch

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"regexp"

	"github.com/jackc/pgx/v5"
)

// A player's watch link: /watch/<secret>, its game's page as the person
// behind the agent sees it. They can type guesses there for their agent
// (counted for it, marked as its owner's), or, while it draws, see its
// word. The secret is 128 bits derived from the app's key, the game and the
// player, so the agent gets the same link every time it asks; only its
// SHA-256 is stored, to find whose it is. A link stops working when its
// player leaves the match or the match ends (its row is deleted).

// watchPath is a watch link's secret, as routes take it: 22 base64url
// characters.
var watchSecretRE = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)

func (a *App) watchSecret(game, player string) string {
	m := hmac.New(sha256.New, a.Kit.Key.Seed())
	m.Write([]byte("sketch/watch\x00" + game + "\x00" + player))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:16])
}

func watchHash(secret string) string {
	h := sha256.Sum256([]byte("sketch/watch\x00" + secret))
	return hex.EncodeToString(h[:])
}

// watchURL is me's watch link for g ("" once it's left or the match is
// over), noted so the link finds it.
func (a *App) watchURL(ctx context.Context, g *game, me *player) (string, error) {
	if me.Left || g.Phase == over {
		return "", nil
	}
	secret := a.watchSecret(g.ID, me.Key())
	if _, err := a.Store.Pool.Exec(ctx, `INSERT INTO sketch_watch (hash, game_id, player) SELECT $1, $2, $3
		WHERE EXISTS (SELECT 1 FROM games WHERE id = $2 AND ended_at IS NULL) ON CONFLICT DO NOTHING`, watchHash(secret), g.ID, me.Key()); err != nil {
		return "", err
	}
	return a.srv.PublicURL() + "/watch/" + secret, nil
}

var errNoWatch = errors.New("this watch link isn't valid any more")

// watcher is whose a watch link is: its game and player, if it still works.
func (a *App) watcher(ctx context.Context, secret string) (game, player string, err error) {
	if !watchSecretRE.MatchString(secret) {
		return "", "", errNoWatch
	}
	err = a.Store.Pool.QueryRow(ctx, `SELECT game_id, player FROM sketch_watch WHERE hash = $1`, watchHash(secret)).Scan(&game, &player)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", errNoWatch
	}
	return game, player, err
}
