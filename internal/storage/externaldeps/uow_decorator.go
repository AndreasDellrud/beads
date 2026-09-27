package externaldeps

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/storage/uow"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
	"github.com/steveyegge/beads/memoryops"
)

// WrapUOWProvider installs the external capability policy on the server UOW
// path. Proxied commands otherwise bypass storage decorators entirely.
func WrapUOWProvider(inner uow.UnitOfWorkProvider, locate ProjectLocator, open StoreOpener) uow.UnitOfWorkProvider {
	if inner == nil {
		return nil
	}
	return &uowProvider{UnitOfWorkProvider: inner, policy: NewPolicy(locate, open)}
}

type uowProvider struct {
	uow.UnitOfWorkProvider
	policy *Policy
}

var _ uow.UnitOfWorkProvider = (*uowProvider)(nil)
var _ uow.MaintenanceProvider = (*uowProvider)(nil)
var _ uow.ProviderUnwrapper = (*uowProvider)(nil)
var _ uow.ProviderRewrapper = (*uowProvider)(nil)

// Unwrap lets callers deliberately peel policy decorators. In particular,
// bd serve must get beneath the notifying provider before handing a provider
// to HTTP handlers, which must never run workspace hooks.
func (p *uowProvider) Unwrap() uow.UnitOfWorkProvider { return p.UnitOfWorkProvider }

// Rewrap puts this policy, unchanged, over a different inner provider. bd
// serve's HTTP layer uses it to slide its per-request timing provider BENEATH
// the policy, so served requests reach their roles through the accessors below
// — the same role-level policy the CLI uses — instead of around them.
func (p *uowProvider) Rewrap(inner uow.UnitOfWorkProvider) uow.UnitOfWorkProvider {
	if inner == nil {
		return nil
	}
	return &uowProvider{UnitOfWorkProvider: inner, policy: p.policy}
}

// RunNonTx preserves the optional maintenance capability exposed by the
// proxied provider. Wrapping the provider must not make unrelated commands
// such as compact lose access to their pinned connection.
func (p *uowProvider) RunNonTx(ctx context.Context, fn func(context.Context, *sql.Conn) error) error {
	provider, ok := p.UnitOfWorkProvider.(uow.MaintenanceProvider)
	if !ok {
		return fmt.Errorf("external dependency UOW wrapper: maintenance operations unsupported")
	}
	return provider.RunNonTx(ctx, fn)
}

func (p *uowProvider) NewUOW(ctx context.Context) (uow.UnitOfWork, error) {
	inner, err := p.UnitOfWorkProvider.NewUOW(ctx)
	if err != nil {
		return nil, err
	}
	return &unitOfWork{UnitOfWork: inner, policy: p.policy}, nil
}

// Provider capability accessors build roles on this wrapper. Delegating them
// to the inner provider would silently discard external-dependency policy for
// every command that reaches the proxied seam through an optional source.
func (p *uowProvider) IssueLifecycle() (publicops.Lifecycle, error) { return uow.NewIssueOperations(p) }

// IssueReader, ReadyCounter, ReadyLister and ReadyClaimer use the SAME role wrappers as the
// store decorator (policy_roles.go): the exclusions are read once, in a
// read-only unit of work of the undecorated provider, and the request is
// delegated to a role over that undecorated provider. A role built over this
// wrapper instead would apply the policy a second time through the use-case
// overrides below, which remain for raw-UOW callers and List(ReadyFlag).
func (p *uowProvider) IssueReader() (publicops.Reader, error) {
	rest, err := uow.NewIssueReader(p)
	if err != nil {
		return nil, err
	}
	inner, err := uow.NewIssueReader(p.UnitOfWorkProvider)
	if err != nil {
		return nil, err
	}
	return newPolicyReader(rest, inner, p.readyPolicy()), nil
}

// IssueClaimer builds the claim-by-id role over THIS wrapper, so its unit of
// work's IssueUseCase is the policy's and ClaimIssue below refuses externally
// blocked work inside the claim's own transaction. That override is what
// covers every caller — this accessor, `bd update --claim` over a proxied
// server, and any raw-UOW caller — rather than a guard layered on the role.
func (p *uowProvider) IssueClaimer() (publicops.Claimer, error)     { return uow.NewIssueClaimer(p) }
func (p *uowProvider) IssueRelations() (publicops.Relations, error) { return uow.NewIssueRelations(p) }
func (p *uowProvider) EdgeReader() (publicops.EdgeReader, error)    { return uow.NewEdgeReader(p) }
func (p *uowProvider) BlockingAnnotator() (publicops.BlockingAnnotator, error) {
	return uow.NewBlockingAnnotator(p)
}
func (p *uowProvider) TreeWalker() (publicops.TreeWalker, error) { return uow.NewTreeWalker(p) }
func (p *uowProvider) GraphCounter() (publicops.GraphCounter, error) {
	return uow.NewGraphCounter(p)
}
func (p *uowProvider) Counter() (publicops.Counter, error) { return uow.NewCounter(p) }
func (p *uowProvider) ReadyCounter() (publicops.ReadyCounter, error) {
	inner, err := uow.NewReadyCounter(p.UnitOfWorkProvider)
	if err != nil {
		return nil, err
	}
	return newPolicyReadyCounter(inner, p.readyPolicy()), nil
}
func (p *uowProvider) ReadyLister() (publicops.ReadyLister, error) {
	inner, err := uow.NewReadyLister(p.UnitOfWorkProvider)
	if err != nil {
		return nil, err
	}
	return newPolicyReadyLister(inner, p.readyPolicy()), nil
}
func (p *uowProvider) ReadyClaimer() (publicops.ReadyClaimer, error) {
	inner, err := uow.NewReadyClaimer(p.UnitOfWorkProvider)
	if err != nil {
		return nil, err
	}
	return newPolicyReadyClaimer(inner, p.readyPolicy()), nil
}

// readyPolicy reads the edges in a read-only unit of work of the undecorated
// provider, one per call.
func (p *uowProvider) readyPolicy() readyPolicy {
	return readyPolicy{policy: p.policy, edges: p.externalEdges, closed: p.issueClosed}
}

func (p *uowProvider) issueClosed(ctx context.Context, id string) (bool, error) {
	return uow.RunTxRead(ctx, p.UnitOfWorkProvider, func(ctx context.Context, uw uow.UnitOfWork) (bool, error) {
		return closedInUOW(ctx, uw.IssueUseCase(), id)
	})
}

func (p *uowProvider) externalEdges(ctx context.Context) (map[string][]*types.Dependency, error) {
	return uow.RunTxRead(ctx, p.UnitOfWorkProvider, func(ctx context.Context, uw uow.UnitOfWork) (map[string][]*types.Dependency, error) {
		return uw.DependencyUseCase().GetExternalBlockingDependencyRecords(ctx)
	})
}
func (p *uowProvider) Querier() (publicops.Querier, error) { return uow.NewQuerier(p) }
func (p *uowProvider) StatsReporter() (publicops.StatsReporter, error) {
	return uow.NewStatsReporter(p)
}
func (p *uowProvider) CycleDetector() (publicops.CycleDetector, error) {
	return uow.NewCycleDetector(p)
}
func (p *uowProvider) Commenter() (publicops.Commenter, error) { return uow.NewCommenter(p) }

// BatchCloser uses the SAME policy wrapper as the store decorator: one edge
// read guards every item (re-closing an already-closed item stays a no-op) and
// narrows the claim the batch earns, and the batch itself runs on a closer over
// the undecorated provider, so the use-case overrides do not apply the policy
// a second time — once per item — beneath it.
func (p *uowProvider) BatchCloser() (publicops.BatchCloser, error) {
	inner, err := uow.NewBatchCloser(p.UnitOfWorkProvider)
	if err != nil {
		return nil, err
	}
	return newPolicyBatchCloser(inner, p.readyPolicy()), nil
}
func (p *uowProvider) BatchCreator() (publicops.BatchCreator, error) {
	return uow.NewBatchCreator(p)
}
func (p *uowProvider) DependencyEditor() (publicops.DependencyEditor, error) {
	return uow.NewDependencyEditor(p)
}
func (p *uowProvider) BatchApplier() (publicops.BatchApplier, error) {
	return uow.NewBatchApplier(p)
}
func (p *uowProvider) Deleter() (publicops.Deleter, error)   { return uow.NewDeleter(p) }
func (p *uowProvider) Sweeper() (publicops.Sweeper, error)   { return uow.NewSweeper(p) }
func (p *uowProvider) Importer() (publicops.Importer, error) { return uow.NewImporter(p) }
func (p *uowProvider) Bootstrapper() (publicops.Bootstrapper, error) {
	return uow.NewBootstrapper(p)
}
func (p *uowProvider) InitVerifier() (publicops.InitVerifier, error) {
	return uow.NewInitVerifier(p)
}
func (p *uowProvider) WorkspaceConfig() (publicops.WorkspaceConfig, error) {
	return uow.NewWorkspaceConfig(p)
}
func (p *uowProvider) VersionReconciler() (publicops.VersionReconciler, error) {
	return uow.NewVersionReconciler(p)
}
func (p *uowProvider) MetadataCAS() (publicops.MetadataCAS, error) { return uow.NewMetadataCAS(p) }
func (p *uowProvider) Releaser() (publicops.Releaser, error)       { return uow.NewReleaser(p) }
func (p *uowProvider) Memories() (memoryops.Memories, error)       { return uow.NewMemories(p) }
func (p *uowProvider) EventsJournalCursor() (storage.EventsJournalCursor, error) {
	return uow.NewEventsJournalCursor(p)
}

func (p *uowProvider) SetPoolLimits(limits uow.PoolLimits) {
	if tuner, ok := p.UnitOfWorkProvider.(uow.PoolTuner); ok {
		tuner.SetPoolLimits(limits)
	}
}

func (p *uowProvider) SetEventsJournalEnabled(enabled bool) {
	if configurer, ok := p.UnitOfWorkProvider.(storage.EventsJournalConfigurer); ok {
		configurer.SetEventsJournalEnabled(enabled)
	}
}

// SetVersionedHistoryEnabled forwards dual-write issue-version activation
// inward, for the same reason SetEventsJournalEnabled does.
//
// This wrapper is the OUTERMOST provider on both real chains — cmd/bd/main.go
// and cmd/bd/serve.go each build wireExternalDependencyUOWProvider(...) around
// everything else — and activation in this family works by type-asserting the
// outermost provider value for the configurer interface. Without this method
// the assertion fails on exactly the chains that matter, so activation would
// silently enable nothing on uow-backed processes while the direct-store plane
// turned on: split-brain, with no error and no log. The inner provider's own
// forwarder cannot be reached from those call sites.
func (p *uowProvider) SetVersionedHistoryEnabled(enabled bool) {
	if configurer, ok := p.UnitOfWorkProvider.(storage.VersionedHistoryConfigurer); ok {
		configurer.SetVersionedHistoryEnabled(enabled)
	}
}

func (p *uowProvider) RunEventsMaintenanceTx(ctx context.Context, fn func(context.Context, issueops.DBTX) error) error {
	runner, ok := p.UnitOfWorkProvider.(issueops.EventsMaintenanceRunner)
	if !ok {
		return fmt.Errorf("external dependency UOW wrapper: events-journal maintenance unsupported")
	}
	return runner.RunEventsMaintenanceTx(ctx, fn)
}

var (
	_ uow.PoolTuner                      = (*uowProvider)(nil)
	_ storage.EventsJournalConfigurer    = (*uowProvider)(nil)
	_ storage.VersionedHistoryConfigurer = (*uowProvider)(nil)
	_ issueops.EventsMaintenanceRunner   = (*uowProvider)(nil)
	_ uow.IssueLifecycleSource           = (*uowProvider)(nil)
	_ uow.IssueReaderSource              = (*uowProvider)(nil)
	_ uow.IssueClaimerSource             = (*uowProvider)(nil)
	_ uow.RelationsSource                = (*uowProvider)(nil)
	_ uow.EdgeReaderSource               = (*uowProvider)(nil)
	_ uow.BlockingAnnotatorSource        = (*uowProvider)(nil)
	_ uow.TreeWalkerSource               = (*uowProvider)(nil)
	_ uow.GraphCounterSource             = (*uowProvider)(nil)
	_ uow.CounterSource                  = (*uowProvider)(nil)
	_ uow.ReadyCounterSource             = (*uowProvider)(nil)
	_ uow.ReadyListerSource              = (*uowProvider)(nil)
	_ uow.ReadyClaimerSource             = (*uowProvider)(nil)
	_ uow.QuerierSource                  = (*uowProvider)(nil)
	_ uow.StatsReporterSource            = (*uowProvider)(nil)
	_ uow.CycleDetectorSource            = (*uowProvider)(nil)
	_ uow.CommenterSource                = (*uowProvider)(nil)
	_ uow.BatchCloserSource              = (*uowProvider)(nil)
	_ uow.BatchCreatorSource             = (*uowProvider)(nil)
	_ uow.DependencyEditorSource         = (*uowProvider)(nil)
	_ uow.BatchApplierSource             = (*uowProvider)(nil)
	_ uow.DeleterSource                  = (*uowProvider)(nil)
	_ uow.SweeperSource                  = (*uowProvider)(nil)
	_ uow.ImporterSource                 = (*uowProvider)(nil)
	_ uow.BootstrapperSource             = (*uowProvider)(nil)
	_ uow.InitVerifierSource             = (*uowProvider)(nil)
	_ uow.WorkspaceConfigSource          = (*uowProvider)(nil)
	_ uow.VersionReconcilerSource        = (*uowProvider)(nil)
	_ uow.MetadataCASSource              = (*uowProvider)(nil)
	_ uow.ReleaserSource                 = (*uowProvider)(nil)
	_ uow.MemoriesSource                 = (*uowProvider)(nil)
	_ uow.EventsJournalCursorSource      = (*uowProvider)(nil)
)

type unitOfWork struct {
	uow.UnitOfWork
	policy *Policy
	issue  domain.IssueUseCase
	deps   domain.DependencyUseCase
}

var _ uow.UnitOfWork = (*unitOfWork)(nil)

// Unwrap keeps the transaction runner reachable to infrastructure roles such
// as import. The policy only decorates use-case methods, so peeling it does
// not bypass a mutation guard for callers that use the domain surface.
func (u *unitOfWork) Unwrap() uow.UnitOfWork { return u.UnitOfWork }

func (u *unitOfWork) IssueUseCase() domain.IssueUseCase {
	if u.issue == nil {
		u.issue = &issueUseCase{
			IssueUseCase: u.UnitOfWork.IssueUseCase(),
			deps:         u.DependencyUseCase(),
			policy:       u.policy,
		}
	}
	return u.issue
}

func (u *unitOfWork) DependencyUseCase() domain.DependencyUseCase {
	if u.deps == nil {
		u.deps = &dependencyUseCase{
			DependencyUseCase: u.UnitOfWork.DependencyUseCase(),
			policy:            u.policy,
		}
	}
	return u.deps
}

type issueUseCase struct {
	domain.IssueUseCase
	deps   domain.DependencyUseCase
	policy *Policy
}

// blockingState reads the edges in THIS unit of work, so raw-UOW callers (the
// proxied `bd ready` / `bd list --ready` paths that consume a filter rather
// than a role) see the policy inside their own transaction.
func (u *issueUseCase) blockingState(ctx context.Context) (blockingState, error) {
	return u.policy.exclusionState(ctx, u.deps.GetExternalBlockingDependencyRecords)
}

func (u *issueUseCase) GetReadyWork(ctx context.Context, filter types.WorkFilter) (domain.SearchPage, error) {
	state, err := u.blockingState(ctx)
	if err != nil {
		return domain.SearchPage{}, err
	}
	return u.IssueUseCase.GetReadyWork(ctx, withExternalExclusions(filter, state.refsByIssue))
}

func (u *issueUseCase) GetReadyWorkWithCounts(ctx context.Context, filter types.WorkFilter) (domain.SearchCountsPage, error) {
	state, err := u.blockingState(ctx)
	if err != nil {
		return domain.SearchCountsPage{}, err
	}
	return u.IssueUseCase.GetReadyWorkWithCounts(ctx, withExternalExclusions(filter, state.refsByIssue))
}

func (u *issueUseCase) ClaimReadyIssue(ctx context.Context, filter types.WorkFilter, actor string) (domain.ClaimReadyResult, error) {
	state, err := u.blockingState(ctx)
	if err != nil {
		return domain.ClaimReadyResult{}, err
	}
	return u.IssueUseCase.ClaimReadyIssue(ctx, withExternalExclusions(filter, state.refsByIssue), actor)
}

func (u *issueUseCase) GetBlockedIssues(ctx context.Context, filter types.WorkFilter) ([]*types.BlockedIssue, error) {
	base, err := u.IssueUseCase.GetBlockedIssues(ctx, unpagedBlockedFilter(filter))
	if err != nil {
		return nil, err
	}
	state, err := u.blockingState(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]*types.BlockedIssue, 0, len(base)+len(state.refsByIssue))
	byID := make(map[string]bool, len(base))
	for _, item := range base {
		if item == nil {
			continue
		}
		clone := *item
		clone.BlockedBy = slices.Clone(item.BlockedBy)
		for _, ref := range state.refsByIssue[item.ID] {
			clone.BlockedBy = appendUnique(clone.BlockedBy, ref)
		}
		clone.BlockedByCount = len(clone.BlockedBy)
		result = append(result, &clone)
		byID[item.ID] = true
	}

	missing := make([]string, 0, len(state.refsByIssue))
	for id := range state.refsByIssue {
		if !byID[id] {
			missing = append(missing, id)
		}
	}
	parentDeps := make(map[string][]*types.Dependency)
	if filter.ParentID != nil && len(missing) > 0 {
		parentDeps, err = u.deps.GetIssueDependencyRecords(ctx, missing)
		if err != nil {
			return nil, fmt.Errorf("external dependencies: load blocked parent edges: %w", err)
		}
		wispParentDeps, err := u.deps.GetWispDependencyRecords(ctx, missing)
		if err != nil {
			return nil, fmt.Errorf("external dependencies: load blocked wisp parent edges: %w", err)
		}
		for id, deps := range wispParentDeps {
			parentDeps[id] = append(parentDeps[id], deps...)
		}
	}
	issues, err := u.IssueUseCase.GetIssuesByIDs(ctx, missing)
	if err != nil {
		return nil, fmt.Errorf("external dependencies: load blocked sources: %w", err)
	}
	wisps, err := u.IssueUseCase.GetWispsByIDs(ctx, missing)
	if err != nil {
		return nil, fmt.Errorf("external dependencies: load blocked wisp sources: %w", err)
	}
	issues = append(issues, wisps...)
	for _, issue := range issues {
		if issue == nil || issue.Status == types.StatusClosed || issue.Status == types.StatusPinned {
			continue
		}
		if !matchesParentFilter(issue.ID, filter.ParentID, parentDeps) {
			continue
		}
		refs := slices.Clone(state.refsByIssue[issue.ID])
		result = append(result, &types.BlockedIssue{Issue: *issue, BlockedByCount: len(refs), BlockedBy: refs})
	}
	return finishBlockedIssues(result, filter)
}

// ClaimIssue refuses a claim-by-id of an issue an unsatisfied external blocker
// holds back, reading the edges in THIS unit of work before the compare-and-set
// runs in it. The store arm has always refused this (externaldeps.Store's
// IssueClaimer); without the override the unit-of-work arm — proxied `bd
// update --claim`, and serve's provider arm, whose claim role is built over
// this provider — claimed it.
func (u *issueUseCase) ClaimIssue(ctx context.Context, id, actor string) (domain.ClaimResult, error) {
	state, err := u.blockingState(ctx)
	if err != nil {
		return domain.ClaimResult{}, err
	}
	if blockers := state.refsByIssue[id]; len(blockers) > 0 {
		return domain.ClaimResult{}, externallyBlocked(id, blockers)
	}
	return u.IssueUseCase.ClaimIssue(ctx, id, actor)
}

func (u *issueUseCase) CloseIssueChecked(ctx context.Context, id string, params domain.CloseIssueParams, actor string, force bool) (domain.CloseIssueResult, error) {
	if err := u.guardExternalClose(ctx, id, force); err != nil {
		return domain.CloseIssueResult{}, err
	}
	return u.IssueUseCase.CloseIssueChecked(ctx, id, params, actor, force)
}

func (u *issueUseCase) CloseWispChecked(ctx context.Context, id string, params domain.CloseIssueParams, actor string, force bool) (domain.CloseIssueResult, error) {
	if err := u.guardExternalClose(ctx, id, force); err != nil {
		return domain.CloseIssueResult{}, err
	}
	return u.IssueUseCase.CloseWispChecked(ctx, id, params, actor, force)
}

func (u *issueUseCase) ApplyUpdate(ctx context.Context, id string, spec domain.UpdateSpec, actor string) (*types.Issue, error) {
	if isClosedUpdate(spec.Fields) {
		if err := u.guardExternalClose(ctx, id, false); err != nil {
			return nil, err
		}
	}
	return u.IssueUseCase.ApplyUpdate(ctx, id, spec, actor)
}

func isClosedUpdate(fields map[string]any) bool {
	switch status := fields["status"].(type) {
	case string:
		return status == string(types.StatusClosed)
	case types.Status:
		return status == types.StatusClosed
	default:
		return false
	}
}

func (u *issueUseCase) guardExternalClose(ctx context.Context, id string, force bool) error {
	if force {
		return nil
	}
	state, err := u.blockingState(ctx)
	if err != nil {
		return err
	}
	blockers := state.refsByIssue[id]
	refused, err := closeRefused(ctx, u.issueClosed, id, blockers)
	if err != nil {
		return err
	}
	if refused {
		return externallyBlocked(id, blockers)
	}
	return nil
}

// issueClosed answers the re-close exemption in THIS unit of work, from either
// plane.
func (u *issueUseCase) issueClosed(ctx context.Context, id string) (bool, error) {
	return closedInUOW(ctx, u.IssueUseCase, id)
}

func closedInUOW(ctx context.Context, issues domain.IssueUseCase, id string) (bool, error) {
	issue, err := issues.GetIssue(ctx, id)
	if err != nil || issue == nil {
		// A miss on the issues plane is not an answer: the id may be a wisp.
		if wisp, werr := issues.GetWisp(ctx, id); werr == nil && wisp != nil {
			return wisp.Status == types.StatusClosed, nil
		}
		return false, nil
	}
	return issue.Status == types.StatusClosed, nil
}

type dependencyUseCase struct {
	domain.DependencyUseCase
	policy *Policy
}

func (u *dependencyUseCase) GetDependencyTree(ctx context.Context, rootID string, opts domain.DepTreeOpts) ([]*types.TreeNode, error) {
	tree, err := u.DependencyUseCase.GetDependencyTree(ctx, rootID, opts)
	if err != nil || opts.Direction == domain.DepDirectionIn || len(tree) == 0 {
		return tree, err
	}
	ids := make([]string, 0, len(tree))
	for _, node := range tree {
		if node != nil && !isExternalReference(node.ID) {
			ids = append(ids, node.ID)
		}
	}
	deps, err := u.GetIssueDependencyRecords(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("external dependencies: load tree edges: %w", err)
	}
	return u.policy.appendTreeExternalReferences(ctx, tree, deps, opts.MaxDepth, opts.ShowAllPaths)
}
