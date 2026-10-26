// Package pgtest gives a test its own empty Postgres database, for an app's
// own storage. It depends on nothing but pgx, so the apps' tests can use it
// where they're published without the core (opensource/export.sh); dbtest
// builds the core's migrated databases on it.
//
// Tests need WAKESHIFT_TEST_DATABASE_URL: a Postgres URL for a role that can
// create roles and databases (`make test` starts one with Docker).
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	envURL   = "WAKESHIFT_TEST_DATABASE_URL"
	envPool  = "WAKESHIFT_TEST_POOL" // pool_max_conns for the test databases (optional)
	appRole  = "wakeshift_test_app"
	appPass  = "wakeshift_test_app"
	setupTip = "set " + envURL + " (e.g. run tests with `make test`, which starts Postgres in Docker)"
)

// EmptyURL returns the URL of a new, empty database owned by an ordinary
// role (NOSUPERUSER, NOBYPASSRLS; CREATEROLE, as Cloud SQL's users have).
// It's dropped when the test ends.
func EmptyURL(t testing.TB) string {
	t.Helper()
	adminURL := os.Getenv(envURL)
	if adminURL == "" {
		t.Fatal(setupTip)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connecting to %s: %v", envURL, err)
	}
	defer admin.Close(context.Background())

	// The role is shared by all test databases, so it may exist already, or
	// another test package may be creating it at the same moment (a unique
	// violation on pg_authid). Anything else is a setup problem. It may
	// create roles, as the database's owner does on Cloud SQL: the core's
	// migrations make the admin service's read role (080_admin.sql).
	if _, err := admin.Exec(ctx, `CREATE ROLE `+appRole+` LOGIN PASSWORD '`+appPass+`' NOSUPERUSER NOBYPASSRLS CREATEROLE`); err != nil {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || (pgErr.Code != "42710" && pgErr.Code != "23505") { // duplicate_object, unique_violation
			t.Fatalf("creating the test role: %v", err)
		}
		// It exists: it must be the ordinary login role the tests rely on
		// (row-level security is enforced only for such a role).
		var login, super, bypass bool
		if err := admin.QueryRow(ctx, `SELECT rolcanlogin, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = $1`, appRole).Scan(&login, &super, &bypass); err != nil {
			t.Fatalf("reading the test role: %v", err)
		}
		if !login || super || bypass {
			t.Fatalf("the test role %s exists but isn't an ordinary login role (login %v, superuser %v, bypassrls %v): drop it and run again", appRole, login, super, bypass)
		}
		// Made before it needed CREATEROLE. Another package may be doing
		// the same at once ("tuple concurrently updated"): check again.
		var createRole bool
		if err := admin.QueryRow(ctx, `SELECT rolcreaterole FROM pg_roles WHERE rolname = $1`, appRole).Scan(&createRole); err != nil {
			t.Fatalf("reading the test role: %v", err)
		}
		if !createRole {
			if _, err := admin.Exec(ctx, `ALTER ROLE `+appRole+` CREATEROLE`); err != nil {
				if err := admin.QueryRow(ctx, `SELECT rolcreaterole FROM pg_roles WHERE rolname = $1`, appRole).Scan(&createRole); err != nil || !createRole {
					t.Fatalf("letting the test role create roles: %v", err)
				}
			}
		}
	}
	b := make([]byte, 6)
	rand.Read(b)
	name := "wakeshift_test_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name+` OWNER `+appRole); err != nil {
		t.Fatalf("creating test database: %v", err)
	}
	// Registered first, so it runs after the cleanups that close pools.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if admin, err := pgx.Connect(ctx, adminURL); err == nil {
			admin.Exec(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
			admin.Close(ctx)
		}
	})
	u, _ := url.Parse(adminURL)
	u.User = url.UserPassword(appRole, appPass)
	u.Path = "/" + name
	if n := os.Getenv(envPool); n != "" { // a load test's core needs more
		q := u.Query()
		q.Set("pool_max_conns", n)
		u.RawQuery = q.Encode()
	}
	return u.String()
}
