package httpapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/storage/externaldeps"
	"github.com/steveyegge/beads/internal/storage/uow"
	"github.com/steveyegge/beads/internal/types"
)

// externalEdgesProvider is the claim fake with an `external:` dependency plane,
// so the external-capability policy has something to hold an issue back with.
type externalEdgesProvider struct {
	*fakeProvider
	edges map[string][]*types.Dependency
	reads *int
}

func (p externalEdgesProvider) NewUOW(ctx context.Context) (uow.UnitOfWork, error) {
	u, err := p.fakeProvider.NewUOW(ctx)
	if err != nil {
		return nil, err
	}
	return externalEdgesUOW{UnitOfWork: u, p: p}, nil
}

type externalEdgesUOW struct {
	uow.UnitOfWork
	p externalEdgesProvider
}

func (u externalEdgesUOW) DependencyUseCase() domain.DependencyUseCase {
	return externalEdgesDeps{DependencyUseCase: u.UnitOfWork.DependencyUseCase(), p: u.p}
}

type externalEdgesDeps struct {
	domain.DependencyUseCase
	p externalEdgesProvider
}

func (d externalEdgesDeps) GetExternalBlockingDependencyRecords(context.Context) (map[string][]*types.Dependency, error) {
	*d.p.reads++
	return d.p.edges, nil
}

// servedPolicyProvider wraps the fake in the external-capability policy exactly
// as `bd serve` wraps its provider (wireExternalDependencyUOWProvider). The
// locator resolves no project, so every `external:` blocker is unsatisfied.
func servedPolicyProvider(issues *fakeIssues, edges map[string][]*types.Dependency, reads *int) uow.UnitOfWorkProvider {
	inner := externalEdgesProvider{fakeProvider: &fakeProvider{issues: issues}, edges: edges, reads: reads}
	return externaldeps.WrapUOWProvider(inner, func(externaldeps.ProjectName) (string, bool) { return "", false }, nil)
}

// TestServedClaimRefusesExternallyBlockedWork pins the claim endpoint on serve's
// PROVIDER arm: an issue held back by an unsatisfied `external:` blocker is
// refused before the compare-and-set runs, as the store arm and `bd update
// --claim` refuse it, and the policy reads the edges once for the request.
func TestServedClaimRefusesExternallyBlockedWork(t *testing.T) {
	issues := &fakeIssues{issue: seededIssue("bd-1", "", types.StatusOpen)}
	reads := 0
	provider := servedPolicyProvider(issues, map[string][]*types.Dependency{
		"bd-1": {{IssueID: "bd-1", DependsOnID: "external:remote:payments", Type: types.DepBlocks}},
	}, &reads)
	ts := newTestServer(t, Config{Provider: provider})

	resp := ts.claim(t, claimPath, `{"actor":"alice"}`)
	body := readAll(t, resp)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("served claim of an externally blocked issue succeeded: %s", body)
	}
	if n := len(issues.claimed()); n != 0 {
		t.Fatalf("the compare-and-set ran %d times for an externally blocked issue, want 0", n)
	}
	if reads != 1 {
		t.Errorf("the request read the external edges %d times, want exactly 1", reads)
	}

	// The same server claims the issue once the blocker is gone, so the
	// refusal above is the policy's and not the fake's.
	unblocked := &fakeIssues{issue: seededIssue("bd-1", "", types.StatusOpen)}
	ts = newTestServer(t, Config{Provider: servedPolicyProvider(unblocked, nil, &reads)})
	if resp := ts.claim(t, claimPath, `{"actor":"alice"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("served claim of an unblocked issue: status %d: %s", resp.StatusCode, readAll(t, resp))
	}
}
