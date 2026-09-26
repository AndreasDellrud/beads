package externaldeps

import (
	"context"

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
