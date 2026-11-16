package storetest

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/store"
)

func testWebUsers(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	u := store.User{ID: "human", Email: " Alice@Example.COM ", Role: store.RoleUser, PasswordHash: "hash", CreatedAt: t0}
	must(t, s.CreateUser(ctx, u))
	got, err := s.GetUserByEmail(ctx, " ALICE@example.com ")
	must(t, err)
	if got.Email != "alice@example.com" || got.Role != store.RoleUser || got.PasswordHash != "hash" {
		t.Fatalf("user = %+v", got)
	}
	u.ID = "dup"
	mustIs(t, s.CreateUser(ctx, u), store.ErrExists)
	u.Email = "bad"
	mustIs(t, s.CreateUser(ctx, u), store.ErrInvalid)
	u.Email = "root@example.com"
	u.Role = store.RoleSuper
	mustIs(t, s.CreateUser(ctx, u), store.ErrInvalid)
	has, err := s.HasUserIdentityAtIssuer(ctx, got.ID, "https://idp")
	must(t, err)
	if has {
		t.Fatal("new user has an identity")
	}

	identity := store.UserIdentity{Issuer: "https://idp", Subject: "alice", UserID: got.ID, CreatedAt: t0}
	must(t, s.CreateUserIdentity(ctx, identity))
	mustIs(t, s.CreateUserIdentity(ctx, identity), store.ErrExists)
	has, err = s.HasUserIdentityAtIssuer(ctx, got.ID, "https://idp")
	must(t, err)
	if !has {
		t.Fatal("bound issuer was not found")
	}
	has, err = s.HasUserIdentityAtIssuer(ctx, got.ID, "https://other-idp")
	must(t, err)
	if has {
		t.Fatal("another issuer matched")
	}

	id, err := s.GetUserIdentity(ctx, identity.Issuer, identity.Subject)
	must(t, err)
	mustEqual(t, "identity", id, identity)
	identity.Subject = "missing"
	identity.UserID = "absent"
	mustIs(t, s.CreateUserIdentity(ctx, identity), store.ErrNotFound)
	_, err = s.GetUserIdentity(ctx, "https://idp", "absent")
	mustIs(t, err, store.ErrNotFound)

	// An identity conflict must roll the new super user back, even if a caller
	// ignores the operation error and commits its surrounding transaction.
	u.ID = "super"
	u.Email = "super@example.com"
	identity.Subject = "alice"
	identity.UserID = u.ID
	must(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		mustIs(t, tx.CreateSuperUser(ctx, u, &identity), store.ErrExists)
		return nil
	}))
	_, err = s.GetUser(ctx, u.ID)
	mustIs(t, err, store.ErrNotFound)
	_, err = s.GetSuperUser(ctx)
	mustIs(t, err, store.ErrNotFound)
	identity.Subject = "root"
	must(t, s.CreateSuperUser(ctx, u, &identity))
	super, err := s.GetSuperUser(ctx)
	must(t, err)
	if super.ID != u.ID || super.Role != store.RoleSuper {
		t.Fatalf("super = %+v", super)
	}
	id, err = s.GetUserIdentity(ctx, identity.Issuer, identity.Subject)
	must(t, err)
	mustEqual(t, "super identity", id, identity)
	u.ID = "other"
	u.Email = "other@example.com"
	mustIs(t, s.CreateSuperUser(ctx, u, nil), store.ErrExists)
	_, err = s.GetUser(ctx, u.ID)
	mustIs(t, err, store.ErrNotFound)

	sess := store.WebSession{TokenHash: webHash("cookie"), UserID: got.ID, CreatedAt: t0, LastUsedAt: t0, ExpiresAt: t0 + store.WebSessionIdleTimeout.Milliseconds()}
	must(t, s.CreateWebSession(ctx, sess))
	must(t, s.DeleteUser(ctx, got.ID))
	_, err = s.GetUserIdentity(ctx, "https://idp", "alice")
	mustIs(t, err, store.ErrNotFound)
	_, err = s.UseWebSession(ctx, sess.TokenHash, t0+1)
	mustIs(t, err, store.ErrNotFound)
	must(t, s.DeleteUser(ctx, got.ID))
}

func testSuperConcurrent(t *testing.T, b Backend) {
	s, dir := open(t, b)
	second := b.Open(t, dir)
	defer second.Close()
	var wg sync.WaitGroup
	outcomes := make(chan error, 16)
	start := make(chan struct{})
	for i := range 16 {
		wg.Go(func() {
			<-start
			st := s
			if i%2 == 1 {
				st = second
			}
			id := fmt.Sprintf("u%d", i)
			outcomes <- st.CreateSuperUser(t.Context(), store.User{ID: id, Email: id + "@example.com", Role: store.RoleSuper, CreatedAt: t0}, nil)
		})
	}
	close(start)
	wg.Wait()
	close(outcomes)
	winners := 0
	for err := range outcomes {
		if err == nil {
			winners++
		} else if !errors.Is(err, store.ErrExists) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("super winners = %d", winners)
	}
}

func testWebSessions(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	must(t, s.CreateUser(ctx, store.User{ID: "human", Email: "a@b", Role: store.RoleUser, CreatedAt: t0}))
	sess := store.WebSession{TokenHash: webHash("cookie"), UserID: "human", CreatedAt: t0, LastUsedAt: t0, ExpiresAt: t0 + store.WebSessionIdleTimeout.Milliseconds()}
	must(t, s.CreateWebSession(ctx, sess))
	mustIs(t, s.CreateWebSession(ctx, sess), store.ErrExists)
	bad := sess
	bad.TokenHash = []byte("plaintext")
	mustIs(t, s.CreateWebSession(ctx, bad), store.ErrInvalid)
	bad = sess
	bad.TokenHash = webHash("absent")
	bad.UserID = "absent"
	mustIs(t, s.CreateWebSession(ctx, bad), store.ErrNotFound)
	got, err := s.UseWebSession(ctx, sess.TokenHash, t0+time.Minute.Milliseconds())
	must(t, err)
	mustEqual(t, "not touched", got, sess)
	got, err = s.UseWebSession(ctx, sess.TokenHash, t0+store.WebSessionTouchInterval.Milliseconds())
	must(t, err)
	if got.LastUsedAt != t0+store.WebSessionTouchInterval.Milliseconds() || got.ExpiresAt != got.LastUsedAt+store.WebSessionIdleTimeout.Milliseconds() {
		t.Fatalf("touch = %+v", got)
	}
	_, err = s.UseWebSession(ctx, sess.TokenHash, got.ExpiresAt)
	mustIs(t, err, store.ErrNotFound)
	must(t, s.DeleteWebSession(ctx, sess.TokenHash))
	must(t, s.DeleteWebSession(ctx, sess.TokenHash))
	_, err = s.UseWebSession(ctx, sess.TokenHash, t0+1)
	mustIs(t, err, store.ErrNotFound)

	// Frequent use extends idle expiry, but never past the hard deadline.
	sess.TokenHash = webHash("active")
	must(t, s.CreateWebSession(ctx, sess))
	for day := int64(1); day < 30; day++ {
		got, err = s.UseWebSession(ctx, sess.TokenHash, t0+day*(24*time.Hour).Milliseconds())
		must(t, err)
		if got.ExpiresAt > t0+store.WebSessionMaxLifetime.Milliseconds() {
			t.Fatalf("past hard deadline: %+v", got)
		}
	}
	_, err = s.UseWebSession(ctx, sess.TokenHash, t0+store.WebSessionMaxLifetime.Milliseconds())
	mustIs(t, err, store.ErrNotFound)
	// Purge must physically remove expired rows and retain unexpired ones.
	sess.TokenHash = webHash("purge-expired")
	must(t, s.CreateWebSession(ctx, sess))
	active := sess
	active.TokenHash = webHash("purge-live")
	active.CreatedAt = t0 + 1
	active.LastUsedAt = t0 + 1
	active.ExpiresAt = sess.ExpiresAt + 1
	must(t, s.CreateWebSession(ctx, active))
	n, err := s.PurgeWebSessions(ctx, sess.ExpiresAt)
	must(t, err)
	// The original long-lived session was touched up to its max lifetime and
	// is still live at this earlier time; only purge-expired is due here.
	if n != 1 {
		t.Fatalf("purged %d, want 1", n)
	}
	must(t, s.CreateWebSession(ctx, sess)) // same hash can be inserted only after deletion
	mustIs(t, s.CreateWebSession(ctx, active), store.ErrExists)
	n, err = s.PurgeWebSessions(ctx, sess.ExpiresAt)
	must(t, err)
	if n != 1 {
		t.Fatalf("purged %d", n)
	}
	n, err = s.PurgeWebSessions(ctx, sess.ExpiresAt)
	must(t, err)
	if n != 0 {
		t.Fatal("purge not idempotent")
	}

}

func webHash(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }
