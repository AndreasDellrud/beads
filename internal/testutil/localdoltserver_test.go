//go:build !windows

package testutil

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// serverFingerprint is what a test can observe about "the Dolt test server"
// over SQL. Both backends must produce wantServerFingerprint: the container
// because that is what CI has always run against, the local backend because
// it has to be indistinguishable from it. A Dolt or image bump that changes
// any of these defaults fails here first (design D6 §2).
type serverFingerprint struct {
	Users        []string // user@host, sorted
	TestGrants   []string // SHOW GRANTS FOR 'test'@'%'
	Databases    []string // SHOW DATABASES, sorted
	CurrentUser  string   // CURRENT_USER() when connecting as root
	Version      string
	MaxConns     string
	SQLMode      string
	SystemTZ     string
	TimeZone     string
	Autocommit   string
	SecureFilePv string
}

var wantServerFingerprint = serverFingerprint{
	Users: []string{
		"__dolt_local_user__@localhost",
		"event_scheduler@localhost",
		"root@%",
		"test@%",
	},
	TestGrants: []string{
		"GRANT USAGE ON *.* TO `test`@`%`",
		"GRANT SELECT, INSERT, UPDATE, DELETE, CREATE, DROP, REFERENCES, INDEX, ALTER, CREATE TEMPORARY TABLES, LOCK TABLES, EXECUTE, CREATE VIEW, SHOW VIEW, CREATE ROUTINE, ALTER ROUTINE, EVENT, TRIGGER ON `beads_test`.* TO `test`@`%`",
	},
	Databases:    []string{"beads_test", "information_schema", "mysql"},
	CurrentUser:  "root@%",
	Version:      "8.0.31",
	MaxConns:     "151",
	SQLMode:      "NO_ENGINE_SUBSTITUTION,ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES",
	SystemTZ:     "UTC",
	TimeZone:     "SYSTEM",
	Autocommit:   "1",
	SecureFilePv: "",
}

func queryStrings(ctx context.Context, db *sql.DB, q string) ([]string, error) {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", q, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s sql.NullString
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("%s: %w", q, err)
		}
		out = append(out, s.String)
	}
	return out, rows.Err()
}

func fingerprintServer(t *testing.T, port string) serverFingerprint {
	t.Helper()
	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%s)/", port))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var fp serverFingerprint
	must := func(v []string, err error) []string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	fp.Users = must(queryStrings(ctx, db, "SELECT CONCAT(user, '@', host) FROM mysql.user ORDER BY 1"))
	fp.TestGrants = must(queryStrings(ctx, db, "SHOW GRANTS FOR 'test'@'%'"))
	fp.Databases = must(queryStrings(ctx, db, "SHOW DATABASES"))
	sort.Strings(fp.Databases)
	row := db.QueryRowContext(ctx, "SELECT CURRENT_USER(), @@version, @@max_connections, @@sql_mode, "+
		"@@system_time_zone, @@time_zone, @@autocommit, @@secure_file_priv")
	var secure sql.NullString
	if err := row.Scan(&fp.CurrentUser, &fp.Version, &fp.MaxConns, &fp.SQLMode,
		&fp.SystemTZ, &fp.TimeZone, &fp.Autocommit, &secure); err != nil {
		t.Fatalf("server variables: %v", err)
	}
	fp.SecureFilePv = secure.String
	return fp
}

func checkFingerprint(t *testing.T, got serverFingerprint) {
	t.Helper()
	if g, w := fmt.Sprintf("%+q", got), fmt.Sprintf("%+q", wantServerFingerprint); g != w {
		t.Errorf("Dolt test server fingerprint changed\n got: %s\nwant: %s", g, w)
	}
}

// TestDoltServerFingerprint checks every backend available on this host
// against the same checked-in fingerprint, so container and local servers
// are compared with each other through it. The backend BEADS_TEST_DOLT_SERVER
// selects obeys the usual skip/fail rules; the other one is checked only when
// it happens to be available.
func TestDoltServerFingerprint(t *testing.T) {
	if hasTestSkip("dolt") {
		t.Skip("skipping: Dolt tests skipped (BEADS_TEST_SKIP=dolt)")
	}
	selectedLocal := useLocalDoltServer()

	t.Run("container", func(t *testing.T) {
		if !selectedLocal {
			if state := checkDolt(); state != doltReady {
				skipOrFailDoltUnavailable(t, state)
			}
		} else if !isDockerAvailable() || !isDoltImageCached() {
			t.Skipf("container backend not available on this host (docker + %s)", DoltDockerImage)
		}
		c := startIsolatedDoltContainer(t)
		checkFingerprint(t, fingerprintServer(t, c.Port))
	})

	t.Run("local", func(t *testing.T) {
		if selectedLocal {
			if state := checkDolt(); state != doltReady {
				skipOrFailDoltUnavailable(t, state)
			}
		} else if _, err := resolveLocalDoltBinary(); err != nil {
			t.Skipf("local backend not available on this host: %v", err)
		}
		c := startIsolatedLocalDoltServer(t)
		checkFingerprint(t, fingerprintServer(t, c.Port))
	})
}

// requireLocalDoltCLI skips (or, under BEADS_TEST_REQUIRE_DOLT_CONTAINER=1,
// fails) a test of the local backend itself when the pinned dolt CLI is
// missing. These tests run whichever backend is selected.
func requireLocalDoltCLI(t *testing.T) {
	t.Helper()
	if hasTestSkip("dolt") {
		t.Skip("skipping: Dolt tests skipped (BEADS_TEST_SKIP=dolt)")
	}
	if _, err := resolveLocalDoltBinary(); err != nil {
		if os.Getenv(EnvRequireDoltContainer) == "1" && useLocalDoltServer() {
			t.Fatalf("local Dolt CLI unavailable (%v) but %s=1", err, EnvRequireDoltContainer)
		}
		t.Skipf("local Dolt CLI unavailable: %v", err)
	}
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func TestLocalDoltServer_BindRaceRetriesOnAnotherPort(t *testing.T) {
	requireLocalDoltCLI(t)
	// Hold a port the way a concurrent action would between FindFreePort and
	// dolt's own bind.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	taken := l.Addr().(*net.TCPAddr).Port

	s, err := newLocalDoltServer()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.terminate)
	if err := s.start(taken); err != nil {
		t.Fatalf("start with port %d taken: %v", taken, err)
	}
	if s.Port() == taken {
		t.Fatalf("server reports the taken port %d", taken)
	}
	if !strings.Contains(s.logSince(0), "already in use") {
		t.Errorf("first attempt did not hit the taken port; log: %s", s.logSince(0))
	}
	if err := pingDoltOnce(fmt.Sprintf("root@tcp(127.0.0.1:%d)/", s.Port())); err != nil {
		t.Errorf("ping after retry: %v", err)
	}
}

func TestLocalDoltServer_NeverDefaultPort(t *testing.T) {
	requireLocalDoltCLI(t)
	s, err := newLocalDoltServer()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.terminate)
	if err := s.start(3307); err != nil {
		t.Fatal(err)
	}
	if s.Port() == 3307 {
		t.Fatal("local test server listens on 3307, the production default port")
	}
}

func TestLocalDoltServer_Lifecycle(t *testing.T) {
	requireLocalDoltCLI(t)
	c := startIsolatedLocalDoltServer(t)
	s := c.local
	ctx := context.Background()

	t.Run("ExecRunsInDataDir", func(t *testing.T) {
		code, out, err := c.Exec(ctx, []string{"pwd", "-P"})
		if err != nil || code != 0 {
			t.Fatalf("exec pwd: code=%d err=%v out=%q", code, err, out)
		}
		want, _ := filepath.EvalSymlinks(s.dataDir)
		if got := strings.TrimSpace(out); got != want {
			t.Errorf("Exec working directory = %q, want the data dir %q", got, want)
		}
		code, _, err = c.Exec(ctx, []string{"test", "-d", "beads_test"})
		if err != nil || code != 0 {
			t.Errorf("provisioned database beads_test not visible from Exec's cwd: code=%d err=%v", code, err)
		}
		code, _, err = c.Exec(ctx, []string{"false"})
		if err != nil || code != 1 {
			t.Errorf("Exec exit status: code=%d err=%v, want 1, nil", code, err)
		}
	})

	t.Run("StopStartKeepsData", func(t *testing.T) {
		dsn := func(port string) string { return fmt.Sprintf("root@tcp(127.0.0.1:%s)/beads_test", port) }
		db, err := sql.Open("mysql", dsn(c.Port))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, "CREATE TABLE kept (id INT PRIMARY KEY)"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO kept VALUES (42)"); err != nil {
			t.Fatal(err)
		}
		_ = db.Close()

		if err := c.Stop(ctx); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if gone, _ := s.exited(); !gone {
			t.Fatal("server still running after Stop")
		}
		if err := c.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}
		port, err := c.CurrentPort(ctx)
		if err != nil {
			t.Fatal(err)
		}
		db, err = sql.Open("mysql", dsn(port))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var id int
		if err := db.QueryRowContext(ctx, "SELECT id FROM kept").Scan(&id); err != nil || id != 42 {
			t.Fatalf("data after restart: id=%d err=%v", id, err)
		}
	})

	t.Run("CrashIsDetected", func(t *testing.T) {
		s.mu.Lock()
		pid := s.cmd.Process.Pid
		s.mu.Unlock()
		if gone, _ := s.exited(); gone {
			t.Fatal("server reported exited before the kill")
		}
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			if gone, _ := s.exited(); gone {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("killed server not reported as exited")
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

func TestLocalDoltServer_SingletonCrashDetection(t *testing.T) {
	requireLocalDoltCLI(t)
	s, err := startLocalDoltServer()
	if err != nil {
		t.Fatal(err)
	}
	defer s.terminate()
	// Exercise the exported crash API on a singleton that is this server,
	// without disturbing whatever singleton the package may hold.
	doltServerMu.Lock()
	saved := doltSingletonSrv
	doltSingletonSrv = &doltServer{local: s}
	doltServerMu.Unlock()
	t.Cleanup(func() {
		doltServerMu.Lock()
		doltSingletonSrv = saved
		doltServerMu.Unlock()
	})

	if DoltContainerCrashed() || DoltContainerCrashError() != nil {
		t.Fatal("running server reported as crashed")
	}
	s.kill()
	if !DoltContainerCrashed() {
		t.Error("DoltContainerCrashed() = false after the server died")
	}
	if err := DoltContainerCrashError(); err == nil {
		t.Error("DoltContainerCrashError() = nil after the server died")
	}
	if !ServerUnreachable(errors.New("anything")) {
		t.Error("ServerUnreachable ignores a dead local singleton")
	}
}

// The suites' leak sweeps (doltserver.SweepSuiteTestServers) report and kill
// dolt servers whose working directory is under the suite root. Test servers
// must live outside it even when started after PinSuiteTempRoot re-pointed
// TMPDIR.
func TestLocalDoltServer_StateOutsideSuiteRoot(t *testing.T) {
	requireLocalDoltCLI(t)
	for _, k := range []string{"TMPDIR", "GOTMPDIR", "TMP", "TEMP"} {
		t.Setenv(k, os.Getenv(k)) // restored after the test
	}
	root, err := PinSuiteTempRoot("bdt-suite-root-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	if !PathUnderSuiteRoot(filepath.Join(os.TempDir(), "x"), root) {
		t.Fatalf("PinSuiteTempRoot did not re-point os.TempDir() under %s", root)
	}

	s, err := startLocalDoltServer()
	if err != nil {
		t.Fatal(err)
	}
	defer s.terminate()
	for _, p := range []string{s.root, s.dataDir} {
		if PathUnderSuiteRoot(p, root) {
			t.Errorf("local server state %s is under the suite root %s", p, root)
		}
	}
}

func TestLocalDoltServer_TerminateLeavesNothing(t *testing.T) {
	requireLocalDoltCLI(t)
	s, err := startLocalDoltServer()
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	pid := s.cmd.Process.Pid
	s.mu.Unlock()
	s.terminate()
	if processAlive(pid) {
		t.Errorf("dolt sql-server pid %d still alive after terminate", pid)
	}
	if _, err := os.Stat(s.root); !os.IsNotExist(err) {
		t.Errorf("state dir %s not removed: %v", s.root, err)
	}
	if err := pingDoltOnce(fmt.Sprintf("root@tcp(127.0.0.1:%d)/", s.Port())); err == nil {
		t.Errorf("something still answers on port %d", s.Port())
	}
}

// The server's own dolt global config turns off the release check and usage
// events: a hermetic test action must not depend on (or phone) the network.
func TestLocalDoltServer_NoNetworkCallsConfigured(t *testing.T) {
	requireLocalDoltCLI(t)
	s, err := newLocalDoltServer()
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(s.root)
	b, err := os.ReadFile(filepath.Join(s.root, "home", ".dolt", "config_global.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"metrics.disabled":"true"`, `"versioncheck.disabled":"true"`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("server dolt global config lacks %s: %s", k, b)
		}
	}
}

func TestDoltBackendSelection(t *testing.T) {
	for _, tc := range []struct {
		val, srcdir string
		local       bool
		bad         bool
	}{
		{"", "", false, false},
		{"", "/runfiles", true, false},
		{"container", "/runfiles", false, false},
		{"local", "", true, false},
		{"docker", "", false, true},
	} {
		t.Run(tc.val+"/"+strconv.Quote(tc.srcdir), func(t *testing.T) {
			t.Setenv(EnvDoltServerBackend, tc.val)
			t.Setenv("TEST_SRCDIR", tc.srcdir)
			if got := useLocalDoltServer(); got != tc.local {
				t.Errorf("useLocalDoltServer() = %v, want %v", got, tc.local)
			}
			if got := doltBackendErr() != nil; got != tc.bad {
				t.Errorf("doltBackendErr() != nil = %v, want %v", got, tc.bad)
			}
		})
	}
}
