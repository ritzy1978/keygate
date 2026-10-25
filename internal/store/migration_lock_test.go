package store

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scratchMigrationStore opens a store on a database of its own, created
// for the test and dropped after it: the migrations these tests run are
// not the real ones, and must not be recorded where the real ones are.
// params are added to the DSN's query.
func scratchMigrationStore(t *testing.T, params url.Values) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	admin, err := New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	ctx := context.Background()
	name := fmt.Sprintf("keygate_miglock_test_%d", time.Now().UnixNano())
	if _, err := admin.DB.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Skipf("cannot create a scratch database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.DB.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	q := u.Query()
	for k, vs := range params {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	u.RawQuery = q.Encode()
	s, err := New(u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// writeMigrations writes each body as a migration, in order, to a new
// directory.
func writeMigrations(t *testing.T, bodies ...string) string {
	t.Helper()
	dir := t.TempDir()
	for i, body := range bodies {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("202601%02d_test.up.sql", i+1)), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// migrationLockHeld reports whether this database's migration lock is
// held. Advisory locks are per database: other packages' tests migrate
// theirs meanwhile, so the count looks at this one only.
const migrationLockHeld = `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND objid = 7367616 AND granted
	AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`

func assertMigrationLockReleased(t *testing.T, s *Store) {
	t.Helper()
	var held int
	if err := s.DB.NewRaw(migrationLockHeld).Scan(context.Background(), &held); err != nil {
		t.Fatal(err)
	}
	if held != 0 {
		t.Fatalf("migration lock still held after RunMigrations returned")
	}
}

// The migration lock is held for the whole run. Taken through the pool,
// it lived on whichever connection ran the statement, and the pool closes
// a connection once it outlives ConnMaxLifetime: a run longer than that
// lost the lock partway, letting a second instance migrate at once.
func TestRunMigrationsHoldsItsLockThroughout(t *testing.T) {
	s := scratchMigrationStore(t, nil)
	// Each migration checks, from its own connection, that the lock is
	// still held, and every connection is past its lifetime as soon as it
	// is made.
	check := `DO $$ BEGIN
		IF (` + migrationLockHeld + `) = 0 THEN
			RAISE EXCEPTION 'migration lock not held';
		END IF;
	END $$;`
	s.DB.SetConnMaxLifetime(time.Nanosecond)
	if err := s.RunMigrations(writeMigrations(t, check, check, check)); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	assertMigrationLockReleased(t, s)
}

// Migrations run on the session that holds the lock, so the lock lasts
// as long as they do: a session the server ends takes the migration
// with it, and a migration whose client gave up keeps the lock until it
// is done. On a session of their own, an idle lock session could be
// ended (idle_session_timeout) while they ran on unlocked.
func TestMigrationsRunInTheLockSession(t *testing.T) {
	s := scratchMigrationStore(t, nil)
	check := `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND objid = 7367616 AND granted
			AND pid = pg_backend_pid()) THEN
			RAISE EXCEPTION 'migration not on the lock session';
		END IF;
	END $$;`
	if err := s.RunMigrations(writeMigrations(t, check, check)); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	assertMigrationLockReleased(t, s)
}

// A migration may take longer than the driver's read timeout, which
// bounds every ordinary query, and so may the wait for another instance
// still migrating. Both used to fail with an i/o timeout after 10
// seconds; here the DSN lowers that timeout to one second.
func TestRunMigrationsOutlastTheReadTimeout(t *testing.T) {
	s := scratchMigrationStore(t, url.Values{"read_timeout": {"1s"}})
	ctx := context.Background()
	if _, err := s.DB.ExecContext(ctx, "SELECT pg_sleep(2)"); err == nil {
		t.Fatal("an ordinary query outlived the read timeout; the test proves nothing")
	}

	// Another instance holds the lock for two seconds.
	other, err := s.DB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.ExecContext(ctx, "SELECT pg_advisory_lock(7367616)"); err != nil {
		t.Fatal(err)
	}
	released := make(chan error, 1)
	go func() {
		time.Sleep(2 * time.Second)
		_, err := other.ExecContext(ctx, "SELECT pg_advisory_unlock(7367616)")
		released <- err
	}()

	start := time.Now()
	if err := s.RunMigrations(writeMigrations(t, "SELECT pg_sleep(2)")); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 4*time.Second {
		t.Fatalf("RunMigrations returned after %v: it did not wait for the lock and the migration", d)
	}
	assertMigrationLockReleased(t, s)
}

// shortMigrationLockWaits makes a migration give up on a table lock, and
// try again, within the time a test can wait.
func shortMigrationLockWaits(t *testing.T, attempts int) {
	t.Helper()
	wait, backoff, tries := migrationTableLockWait, migrationLockBackoff, migrationLockAttempts
	migrationTableLockWait = 300 * time.Millisecond
	migrationLockBackoff = func(int) time.Duration { return 100 * time.Millisecond }
	migrationLockAttempts = attempts
	t.Cleanup(func() { migrationTableLockWait, migrationLockBackoff, migrationLockAttempts = wait, backoff, tries })
}

// holdTableLock keeps a transaction that has read the table open on a
// connection of its own, as a long export or a session idle in a
// transaction does, until release is called.
func holdTableLock(t *testing.T, s *Store, table string) (release func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "SELECT count(*) FROM "+table); err != nil {
		t.Fatal(err)
	}
	return func() { _ = tx.Rollback() }
}

// A migration that cannot lock a table because a long transaction holds
// it does not wait in the queue, where every later query on the table
// would wait behind it: it gives up after a short wait, and is tried
// again until the transaction ends.
func TestMigrationBacksOffFromATableLock(t *testing.T) {
	s := scratchMigrationStore(t, nil)
	ctx := context.Background()
	if _, err := s.DB.ExecContext(ctx, "CREATE TABLE busy (id int)"); err != nil {
		t.Fatal(err)
	}
	shortMigrationLockWaits(t, 50)
	release := holdTableLock(t, s, "busy")
	time.AfterFunc(2*time.Second, release)

	// Traffic on the table meanwhile: no query waits behind the migration
	// for much longer than its short lock wait.
	stop := make(chan struct{})
	slowest := make(chan time.Duration, 1)
	go func() {
		var worst time.Duration
		for {
			select {
			case <-stop:
				slowest <- worst
				return
			default:
			}
			start := time.Now()
			_, _ = s.DB.ExecContext(ctx, "SELECT count(*) FROM busy")
			worst = max(worst, time.Since(start))
			time.Sleep(20 * time.Millisecond)
		}
	}()

	start := time.Now()
	err := s.RunMigrations(writeMigrations(t, "ALTER TABLE busy ADD COLUMN extra int"))
	close(stop)
	worst := <-slowest
	if err != nil {
		t.Fatalf("migrations: %v", err)
	}
	if d := time.Since(start); d < 2*time.Second {
		t.Fatalf("migration done after %v, before the lock was released", d)
	}
	if worst > time.Second {
		t.Fatalf("a query on the table waited %v behind the migration", worst)
	}
	var has bool
	if err := s.DB.NewRaw("SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'busy' AND column_name = 'extra')").Scan(ctx, &has); err != nil || !has {
		t.Fatalf("column added = %v (%v)", has, err)
	}
	assertMigrationLockReleased(t, s)
}

// A lock held through every attempt fails the run, leaving nothing of
// the migration behind and saying why.
func TestMigrationGivesUpOnAHeldTableLock(t *testing.T) {
	s := scratchMigrationStore(t, nil)
	ctx := context.Background()
	if _, err := s.DB.ExecContext(ctx, "CREATE TABLE busy (id int)"); err != nil {
		t.Fatal(err)
	}
	shortMigrationLockWaits(t, 3)
	release := holdTableLock(t, s, "busy")
	defer release()

	err := s.RunMigrations(writeMigrations(t, "ALTER TABLE busy ADD COLUMN extra int"))
	if err == nil || !isLockTimeout(err) {
		t.Fatalf("migrations under a held lock = %v, want a lock timeout", err)
	}
	// The error names the open transaction, for whoever has to end it.
	if !strings.Contains(err.Error(), "SELECT count(*) FROM busy") {
		t.Fatalf("the error does not name the transaction holding the table: %v", err)
	}
	var recorded int
	if err := s.DB.NewRaw("SELECT count(*) FROM schema_migrations").Scan(ctx, &recorded); err != nil || recorded != 0 {
		t.Fatalf("%d migrations recorded (%v), want none", recorded, err)
	}
	assertMigrationLockReleased(t, s)
}

// A migration the server picks as the victim of a deadlock is tried
// again, as one that waited too long for a lock is: its transaction was
// rolled back whole.
func TestMigrationRetriesADeadlock(t *testing.T) {
	s := scratchMigrationStore(t, nil)
	ctx := context.Background()
	// A sequence is not rolled back with the transaction, so it counts
	// the attempts: the first fails as a deadlock would.
	if _, err := s.DB.ExecContext(ctx, "CREATE SEQUENCE attempts"); err != nil {
		t.Fatal(err)
	}
	shortMigrationLockWaits(t, 5)
	err := s.RunMigrations(writeMigrations(t, `DO $$ BEGIN
		IF nextval('attempts') = 1 THEN
			RAISE EXCEPTION 'deadlock detected' USING ERRCODE = '40P01';
		END IF;
	END $$;
	CREATE TABLE after_deadlock (id int);`))
	if err != nil {
		t.Fatalf("migrations: %v", err)
	}
	var tries int
	if err := s.DB.NewRaw("SELECT last_value FROM attempts").Scan(ctx, &tries); err != nil || tries != 2 {
		t.Fatalf("attempts = %d (%v), want 2", tries, err)
	}
	assertMigrationLockReleased(t, s)
}
