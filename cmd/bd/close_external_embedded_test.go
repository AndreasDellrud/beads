//go:build cgo

package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// TestEmbeddedExternalCapabilityGuardsDirectClose drives the direct `bd close`
// route, which reaches the BatchCloser role through the store chain. An issue
// whose `external:` blocker is unsatisfied must refuse to close without
// --force, exactly as `bd update --status closed` and the proxied route
// refuse, and --claim-next must not hand it out.
func TestEmbeddedExternalCapabilityGuardsDirectClose(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "xc")
	// No external_projects entry: the capability is unresolvable, which the
	// policy treats as unsatisfied (fail closed).
	held := bdCreate(t, bd, dir, "Waits for payments", "--type", "task", "--priority", "0")
	free := bdCreate(t, bd, dir, "Free work", "--type", "task", "--priority", "2")
	done := bdCreate(t, bd, dir, "Finished work", "--type", "task", "--priority", "3")
	run := func(args ...string) (string, error) {
		t.Helper()
		cmd := exec.Command(bd, args...)
		cmd.Dir = dir
		cmd.Env = bdEnv(dir)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run("dep", "add", held.ID, "external:remote:payments"); err != nil {
		t.Fatalf("bd dep add: %v\n%s", err, out)
	}

	if out, err := run("close", done.ID, "--claim-next"); err != nil {
		t.Fatalf("bd close --claim-next: %v\n%s", err, out)
	}
	if got := bdShow(t, bd, dir, held.ID); got.Assignee != "" || got.Status != types.StatusOpen {
		t.Errorf("--claim-next claimed externally blocked %s (status=%s assignee=%q)", held.ID, got.Status, got.Assignee)
	}
	if got := bdShow(t, bd, dir, free.ID); got.Status != types.StatusInProgress {
		t.Errorf("--claim-next left %s %s, want it claimed (the only ready issue)", free.ID, got.Status)
	}

	out, err := run("close", held.ID)
	if err == nil || !strings.Contains(out, "external:remote:payments") {
		t.Errorf("bd close of an externally blocked issue: err=%v, want a refusal naming the blocker\n%s", err, out)
	}
	if got := bdShow(t, bd, dir, held.ID); got.Status != types.StatusOpen {
		t.Errorf("%s status after a refused close = %s, want open", held.ID, got.Status)
	}

	if out, err := run("close", held.ID, "--force"); err != nil {
		t.Fatalf("bd close --force must still bypass the external guard: %v\n%s", err, out)
	}

	// The idempotent re-close (ga-ktn9pe.4.8): the issue is closed now, and
	// closing it again without --force is the no-op every close path promises,
	// external blocker or not.
	if out, err := run("close", held.ID); err != nil {
		t.Errorf("re-closing already-closed %s without --force: %v, want the idempotent no-op\n%s", held.ID, err, out)
	}
	if out, err := run("update", held.ID, "--status", "closed"); err != nil {
		t.Errorf("bd update --status closed on already-closed %s: %v, want no external refusal\n%s", held.ID, err, out)
	}
}

// TestEmbeddedExternalCapabilityGuardsUpdateClaim pins `bd update --claim` on
// the direct route: the store chain's policy lifecycle guarded only a close, so
// the claim went straight to the backend's compare-and-set and took externally
// blocked work that `bd ready --claim` would never hand out. It is refused now
// (nonzero, the blocker named, nothing written) with or without --force, and
// an unblocked issue still claims.
func TestEmbeddedExternalCapabilityGuardsUpdateClaim(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "xu")
	held := bdCreate(t, bd, dir, "Waits for payments", "--type", "task", "--priority", "0")
	free := bdCreate(t, bd, dir, "Free work", "--type", "task", "--priority", "2")
	run := func(args ...string) (string, error) {
		t.Helper()
		cmd := exec.Command(bd, args...)
		cmd.Dir = dir
		cmd.Env = bdEnv(dir)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run("dep", "add", held.ID, "external:remote:payments"); err != nil {
		t.Fatalf("bd dep add: %v\n%s", err, out)
	}

	for _, args := range [][]string{{"update", held.ID, "--claim"}, {"update", held.ID, "--claim", "--force"}} {
		out, err := run(args...)
		if err == nil || !strings.Contains(out, "external:remote:payments") {
			t.Errorf("bd %s: err=%v, want a refusal naming the blocker\n%s", strings.Join(args, " "), err, out)
		}
		if got := bdShow(t, bd, dir, held.ID); got.Assignee != "" || got.Status != types.StatusOpen {
			t.Errorf("bd %s claimed externally blocked %s (status=%s assignee=%q)", strings.Join(args, " "), held.ID, got.Status, got.Assignee)
		}
	}
	if out, err := run("update", free.ID, "--claim"); err != nil {
		t.Fatalf("bd update %s --claim: %v\n%s", free.ID, err, out)
	}
	if got := bdShow(t, bd, dir, free.ID); got.Status != types.StatusInProgress || got.Assignee == "" {
		t.Errorf("unblocked %s after --claim: status=%s assignee=%q, want claimed", free.ID, got.Status, got.Assignee)
	}
}
