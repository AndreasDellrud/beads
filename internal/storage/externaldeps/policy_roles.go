package externaldeps

import (
	"context"
	"fmt"

	"github.com/steveyegge/beads/internal/storage"
	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/workapi"
	"github.com/steveyegge/beads/issueops"
)

// The role wrappers below are the ONE place the external capability policy
// meets the ready vocabulary. Each computes the exclusions once per call,
// unions them into a CLONE of the caller's ReadyRequest.ExcludeIDs, and
// delegates to a role that applies no policy of its own: the inner store's
// role on the store arm, a role over the undecorated provider on the
// unit-of-work arm. Delegating to a role rebuilt over a policy decorator would
// run the exclusion query twice; delegating to one that ignores ExcludeIDs
// would drop the policy. Both are pinned by tests in this package.
//
// Validation runs first and without IO, so a request the role would refuse
// costs no edge read.

// readyPolicy binds the policy to the edge source of one seam.
type readyPolicy struct {
	policy *Policy
	edges  EdgeSource
}

// narrow returns req with every externally held-back issue excluded. req is a
// value and ExcludeIDs is freshly allocated, so the caller's request is never
// written through.
func (p readyPolicy) narrow(ctx context.Context, req issueops.ReadyRequest) (issueops.ReadyRequest, error) {
	refs, err := p.policy.Exclusions(ctx, p.edges)
	if err != nil {
		return issueops.ReadyRequest{}, err
	}
	req.ExcludeIDs = unionExcludedIDs(req.ExcludeIDs, refs)
	return req, nil
}

// policyReader applies the policy to Ready. List and Get are answered by the
// embedded reader, which the accessor chooses.
type policyReader struct {
	issueops.Reader
	ready  issueops.Reader
	policy readyPolicy
}

func newPolicyReader(rest, ready issueops.Reader, policy readyPolicy) issueops.Reader {
	return &policyReader{Reader: rest, ready: ready, policy: policy}
}

func (r *policyReader) Ready(ctx context.Context, req issueops.ReadyRequest) (issueops.IssuePage, error) {
	if _, err := workapi.BuildReadyFilter(req); err != nil {
		return issueops.IssuePage{}, err
	}
	narrowed, err := r.policy.narrow(ctx, req)
	if err != nil {
		return issueops.IssuePage{}, err
	}
	return r.ready.Ready(ctx, narrowed)
}

type policyReadyCounter struct {
	inner  issueops.ReadyCounter
	policy readyPolicy
}

func newPolicyReadyCounter(inner issueops.ReadyCounter, policy readyPolicy) issueops.ReadyCounter {
	return &policyReadyCounter{inner: inner, policy: policy}
}

func (c *policyReadyCounter) CountReady(ctx context.Context, req issueops.ReadyRequest) (issueops.ReadyCountResult, error) {
	if _, err := workapi.BuildReadyCountFilter(req); err != nil {
		return issueops.ReadyCountResult{}, err
	}
	narrowed, err := c.policy.narrow(ctx, req)
	if err != nil {
		return issueops.ReadyCountResult{}, err
	}
	return c.inner.CountReady(ctx, narrowed)
}

type policyReadyClaimer struct {
	inner  issueops.ReadyClaimer
	policy readyPolicy
}

func newPolicyReadyClaimer(inner issueops.ReadyClaimer, policy readyPolicy) issueops.ReadyClaimer {
	return &policyReadyClaimer{inner: inner, policy: policy}
}

func (c *policyReadyClaimer) ClaimNext(ctx context.Context, req issueops.ClaimNextRequest) (issueops.ClaimNextResult, error) {
	if err := storageissueops.ValidateClaimNextRequest(req); err != nil {
		return issueops.ClaimNextResult{}, err
	}
	if _, err := workapi.BuildReadyFilter(req.Filter); err != nil {
		return issueops.ClaimNextResult{}, err
	}
	filter, err := c.policy.narrow(ctx, req.Filter)
	if err != nil {
		return issueops.ClaimNextResult{}, err
	}
	req.Filter = filter
	return c.inner.ClaimNext(ctx, req)
}

var (
	_ issueops.Reader       = (*policyReader)(nil)
	_ issueops.ReadyCounter = (*policyReadyCounter)(nil)
	_ issueops.ReadyClaimer = (*policyReadyClaimer)(nil)
)

// policyBatchCloser enforces the external close guard on every item and the
// ready exclusions on the claim a batch earns, then delegates to a closer that
// applies neither. One edge read serves both.
//
// A refused item is SKIPPED, not sent: the inner batch closes the survivors
// in one transaction and the refusal lands at the item's own index, which is
// the BatchCloser contract for any per-item refusal. A batch whose items were
// all refused never reaches the inner closer, so it lands nothing and earns no
// claim — exactly what the contract says a batch that closed nothing does.
type policyBatchCloser struct {
	inner  issueops.BatchCloser
	policy readyPolicy
}

func newPolicyBatchCloser(inner issueops.BatchCloser, policy readyPolicy) issueops.BatchCloser {
	return &policyBatchCloser{inner: inner, policy: policy}
}

func (c *policyBatchCloser) CloseBatch(ctx context.Context, req issueops.CloseBatchRequest) (issueops.CloseBatchResult, error) {
	if err := storageissueops.ValidateCloseBatchRequest(req); err != nil {
		return issueops.CloseBatchResult{}, err
	}
	if req.ClaimNext != nil {
		if _, err := workapi.BuildReadyFilter(*req.ClaimNext); err != nil {
			return issueops.CloseBatchResult{}, err
		}
	}
	refs, err := c.policy.policy.Exclusions(ctx, c.policy.edges)
	if err != nil {
		return issueops.CloseBatchResult{}, err
	}

	outcomes := make([]issueops.CloseOutcome, len(req.Items))
	sent := make([]int, 0, len(req.Items))
	forwarded := req
	forwarded.Items = make([]issueops.BatchCloseItem, 0, len(req.Items))
	for i, item := range req.Items {
		if blockers := refs[item.IssueID]; !req.Force && len(blockers) > 0 {
			outcomes[i] = issueops.CloseOutcome{IssueID: item.IssueID, Err: externallyBlocked(item.IssueID, blockers)}
			continue
		}
		sent = append(sent, i)
		forwarded.Items = append(forwarded.Items, item)
	}
	if len(sent) == 0 {
		return issueops.CloseBatchResult{Outcomes: outcomes}, nil
	}
	if req.ClaimNext != nil {
		claim := *req.ClaimNext
		claim.ExcludeIDs = unionExcludedIDs(claim.ExcludeIDs, refs)
		forwarded.ClaimNext = &claim
	}

	result, err := c.inner.CloseBatch(ctx, forwarded)
	if err != nil {
		return issueops.CloseBatchResult{}, err
	}
	for j, outcome := range result.Outcomes {
		outcomes[sent[j]] = outcome
	}
	return issueops.CloseBatchResult{Outcomes: outcomes, ClaimedNext: result.ClaimedNext}, nil
}

// policyIssueClaimer refuses a claim-by-id of an issue an unsatisfied
// external blocker holds back, before the backend's atomic claim.
type policyIssueClaimer struct {
	inner  issueops.Claimer
	policy readyPolicy
}

func newPolicyIssueClaimer(inner issueops.Claimer, policy readyPolicy) issueops.Claimer {
	return &policyIssueClaimer{inner: inner, policy: policy}
}

func (c *policyIssueClaimer) Claim(ctx context.Context, req issueops.ClaimRequest) (issueops.ClaimResult, error) {
	if req.Actor != "" && req.IssueID != "" {
		refs, err := c.policy.policy.Exclusions(ctx, c.policy.edges)
		if err != nil {
			return issueops.ClaimResult{}, err
		}
		if blockers := refs[req.IssueID]; len(blockers) > 0 {
			return issueops.ClaimResult{}, externallyBlocked(req.IssueID, blockers)
		}
	}
	// An empty actor or id is the inner role's validation refusal to make.
	return c.inner.Claim(ctx, req)
}

// externallyBlocked is the refusal every guard in this package returns, in
// the typed close vocabulary callers already classify with errors.Is.
func externallyBlocked(id string, blockers []string) error {
	return fmt.Errorf("%w: %s is blocked by %v", storage.ErrCloseBlocked, id, blockers)
}

var (
	_ issueops.BatchCloser = (*policyBatchCloser)(nil)
	_ issueops.Claimer     = (*policyIssueClaimer)(nil)
)
