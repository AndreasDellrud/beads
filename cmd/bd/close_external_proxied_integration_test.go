//go:build cgo

package main

import (
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// TestProxiedServerCloseHonorsExternalBlockers drives `bd close` over a proxied
// server, which reaches the provider's BatchCloser accessor — the same policy
// batch closer the store arm uses. An unsatisfied `external:` blocker refuses
// the close without --force, --claim-next walks past the held issue, --force
// bypasses the guard, and re-closing the now-closed issue is the idempotent
// no-op (ga-ktn9pe.4.8).
func TestProxiedServerCloseHonorsExternalBlockers(t *testing.T) {
	requireSharedProxiedServer(t)
	t.Parallel()
	bd := buildEmbeddedBD(t)
	p := newSharedProxiedProject(t, bd, "cxb")

	held := bdProxiedCreate(t, bd, p.dir, "Waits for payments", "--priority", "0")
	free := bdProxiedCreate(t, bd, p.dir, "Free work", "--priority", "2")
	done := bdProxiedCreate(t, bd, p.dir, "Finished work", "--priority", "3")
	if stdout, stderr, err := bdProxiedRunBuffers(t, bd, p.dir, "dep", "add", held.ID, "external:remote:payments"); err != nil {
		t.Fatalf("bd dep add: %v\n%s\n%s", err, stdout, stderr)
	}

	bdProxiedClose(t, bd, p.dir, done.ID, "--claim-next")
	if got := bdProxiedShow(t, bd, p.dir, held.ID); got.Assignee != "" || got.Status != types.StatusOpen {
		t.Errorf("--claim-next claimed externally blocked %s (status=%s assignee=%q)", held.ID, got.Status, got.Assignee)
	}
	if got := bdProxiedShow(t, bd, p.dir, free.ID); got.Status != types.StatusInProgress {
		t.Errorf("--claim-next left %s %s, want it claimed", free.ID, got.Status)
	}

	if out := bdProxiedCloseFail(t, bd, p.dir, held.ID); !strings.Contains(out, "external:remote:payments") {
		t.Errorf("refusal does not name the blocker:\n%s", out)
	}
	if got := bdProxiedShow(t, bd, p.dir, held.ID); got.Status != types.StatusOpen {
		t.Errorf("%s status after a refused close = %s, want open", held.ID, got.Status)
	}
	bdProxiedClose(t, bd, p.dir, held.ID, "--force")
	bdProxiedClose(t, bd, p.dir, held.ID)
}
