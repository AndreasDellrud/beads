package externaldeps

import (
	"context"
	"fmt"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/workapi/storereader"
	"github.com/steveyegge/beads/issueops"
)

// IssueReader narrows Ready through the shared role wrapper and hands the
// request to the INNER store's own reader, so the backend's reader (and the
// telemetry layer beneath this one) answers it with ExcludeIDs set. List and
// Get stay on a reader built over this decorator: List(ReadyFlag) still runs
// the store-level GetReadyWorkWithCounts override until it moves to a role.
func (s *Store) IssueReader() (issueops.Reader, error) {
	rest, err := storereader.New(s)
	if err != nil {
		return nil, err
	}
	inner, err := s.inner.IssueReader()
	if err != nil {
		return nil, err
	}
	return newPolicyReader(rest, inner, s.readyPolicy()), nil
}

// ReadyCounter narrows the count exactly as IssueReader narrows Ready, and
// delegates to the inner store's counter — so the documented
// storage.ReadyCounter.CountReady span still comes from the telemetry layer's
// own accessor rather than being grafted on here.
func (s *Store) ReadyCounter() (issueops.ReadyCounter, error) {
	inner, err := s.inner.ReadyCounter()
	if err != nil {
		return nil, err
	}
	return newPolicyReadyCounter(inner, s.readyPolicy()), nil
}

// ReadyLister narrows the listing exactly as ReadyCounter narrows the count and
// delegates to the inner store's lister, so the page and its total still come
// from the backend's single pass and the storage.ReadyLister.ListReady span
// from the telemetry layer's own accessor. Without this override the accessor
// would PROMOTE to the inner lister and list externally blocked work.
func (s *Store) ReadyLister() (issueops.ReadyLister, error) {
	inner, err := s.inner.ReadyLister()
	if err != nil {
		return nil, err
	}
	return newPolicyReadyLister(inner, s.readyPolicy()), nil
}

// readyPolicy reads this store's edges from beneath every decorator, the same
// source the store-level overrides use.
func (s *Store) readyPolicy() readyPolicy {
	return readyPolicy{policy: s.Policy, edges: s.edgeSource()}
}

// IssueClaimer rejects a direct claim of externally blocked work before the
// backend's atomic claim operation. ReadyClaimer below handles selection among
// candidates; this method covers callers that already name an issue.
func (s *Store) IssueClaimer() (issueops.Claimer, error) {
	inner, err := s.inner.IssueClaimer()
	if err != nil {
		return nil, err
	}
	return &issueClaimer{inner: inner, policy: s}, nil
}

type issueClaimer struct {
	inner  issueops.Claimer
	policy *Store
}

func (c *issueClaimer) Claim(ctx context.Context, req issueops.ClaimRequest) (issueops.ClaimResult, error) {
	blocked, blockers, err := c.policy.IsBlocked(ctx, req.IssueID)
	if err != nil {
		return issueops.ClaimResult{}, err
	}
	if blocked {
		return issueops.ClaimResult{}, fmt.Errorf("%w: %s is blocked by %v", storage.ErrCloseBlocked, req.IssueID, blockers)
	}
	return c.inner.Claim(ctx, req)
}

// BatchCloser guards every item against unsatisfied external blockers and
// narrows the claim the batch earns, then delegates to the inner store's own
// closer. Without it the accessor promoted straight to the inner closer, so
// the direct `bd close` route and serve's store arm closed externally blocked
// issues without --force and could claim one with --claim-next.
func (s *Store) BatchCloser() (issueops.BatchCloser, error) {
	inner, err := s.inner.BatchCloser()
	if err != nil {
		return nil, err
	}
	return newPolicyBatchCloser(inner, s.readyPolicy()), nil
}

// ReadyClaimer narrows the claim's filter and delegates to the inner store's
// own atomic ClaimNext, which wakes expired defers, selects, claims and
// hydrates in one transaction. Cross-project state cannot be atomic with the
// local claim, but local claim ownership remains race-safe.
func (s *Store) ReadyClaimer() (issueops.ReadyClaimer, error) {
	inner, err := s.inner.ReadyClaimer()
	if err != nil {
		return nil, err
	}
	return newPolicyReadyClaimer(inner, s.readyPolicy()), nil
}

// BlockingAnnotator augments the backend's derived local answer with the
// external blockers that the policy resolves for the same ids.
func (s *Store) BlockingAnnotator() (issueops.BlockingAnnotator, error) {
	inner, err := s.inner.BlockingAnnotator()
	if err != nil {
		return nil, err
	}
	return &blockingAnnotator{inner: inner, policy: s}, nil
}

type blockingAnnotator struct {
	inner  issueops.BlockingAnnotator
	policy *Store
}

func (a *blockingAnnotator) AnnotateBlocking(ctx context.Context, req issueops.BlockingRequest) (issueops.BlockingResult, error) {
	result, err := a.inner.AnnotateBlocking(ctx, req)
	if err != nil {
		return issueops.BlockingResult{}, err
	}
	for i := range result.Items {
		blocked, blockers, err := a.policy.IsBlocked(ctx, result.Items[i].ID)
		if err != nil {
			return issueops.BlockingResult{}, err
		}
		if blocked {
			result.Items[i].BlockedBy = blockers
		}
	}
	return result, nil
}

// TreeWalker preserves synthetic external leaves for the normal down-tree
// request. Reverse walks do not follow a source's dependencies, and the
// combined/up variants retain the backend's existing traversal semantics.
func (s *Store) TreeWalker() (issueops.TreeWalker, error) {
	inner, err := s.inner.TreeWalker()
	if err != nil {
		return nil, err
	}
	return &treeWalker{inner: inner, policy: s}, nil
}

type treeWalker struct {
	inner  issueops.TreeWalker
	policy *Store
}

func (t *treeWalker) WalkTree(ctx context.Context, req issueops.WalkTreeRequest) (issueops.TreeResult, error) {
	if (req.Direction != "" && req.Direction != issueops.TreeDown) || req.Status != "" || req.MaxRows != 0 {
		return t.inner.WalkTree(ctx, req)
	}
	nodes, err := t.policy.GetDependencyTree(ctx, req.RootID, req.MaxDepth, false, false)
	if err != nil {
		return issueops.TreeResult{}, err
	}
	return issueops.TreeResult{Nodes: nodes}, nil
}

var (
	_ issueops.Claimer           = (*issueClaimer)(nil)
	_ issueops.BlockingAnnotator = (*blockingAnnotator)(nil)
	_ issueops.TreeWalker        = (*treeWalker)(nil)
)
