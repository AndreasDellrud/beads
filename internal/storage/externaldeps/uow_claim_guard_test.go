package externaldeps

import (
	"context"
	"errors"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/storage/uow"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

func (u *fakeIssueUseCase) ClaimIssue(_ context.Context, id, actor string) (domain.ClaimResult, error) {
	for _, issue := range u.ready {
		if issue.ID == id {
			issue.Assignee = actor
			issue.Status = types.StatusInProgress
			return domain.ClaimResult{}, nil
		}
	}
	return domain.ClaimResult{}, publicops.ErrNotFound
}

// TestUOWIssueClaimerRefusesExternallyBlockedWork pins claim-by-id on the
// unit-of-work arm (`bd update --claim` over a proxied server, and the served
// claim endpoint on serve's provider arm) to what the store arm has always
// done: an issue held back by an unsatisfied external blocker refuses with
// ErrCloseBlocked, and an unblocked one claims.
func TestUOWIssueClaimerRefusesExternallyBlockedWork(t *testing.T) {
	blocked, ready := issue("be-blocked"), issue("be-ready")
	inner := &fakeUOW{
		issues: &fakeIssueUseCase{ready: []*types.Issue{blocked, ready}},
		deps: &fakeDependencyUseCase{external: map[string][]*types.Dependency{
			blocked.ID: {externalDep(blocked.ID, "external:remote:payments", types.DepBlocks)},
		}},
	}
	provider := WrapUOWProvider(&fakeUOWProvider{uw: inner}, func(ProjectName) (string, bool) { return "", false }, nil)
	claimer, err := provider.(uow.IssueClaimerSource).IssueClaimer()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := claimer.Claim(t.Context(), publicops.ClaimRequest{IssueID: blocked.ID, Actor: "w"}); !errors.Is(err, storage.ErrCloseBlocked) {
		t.Fatalf("claim of externally blocked %s: err = %v, want ErrCloseBlocked", blocked.ID, err)
	}
	if blocked.Assignee != "" {
		t.Fatalf("externally blocked %s was claimed by %q", blocked.ID, blocked.Assignee)
	}
	if _, err := claimer.Claim(t.Context(), publicops.ClaimRequest{IssueID: ready.ID, Actor: "w"}); err != nil {
		t.Fatalf("claim of unblocked %s: %v", ready.ID, err)
	}
	if ready.Assignee != "w" {
		t.Fatalf("unblocked %s assignee = %q, want w", ready.ID, ready.Assignee)
	}
}
