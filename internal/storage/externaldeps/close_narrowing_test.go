package externaldeps

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/storage/uow"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// closeNarrowingWorld is a workspace where closing one issue must not look at
// another issue's `external:` edges: be-free has none, be-other is held by a
// project that is not configured ("nowhere", which warns when resolved), and
// be-held by a configured one ("remote") whose provider issue is still open.
type closeNarrowingWorld struct {
	edges    map[string][]*types.Dependency
	warnings []ProjectName
	opened   []string
	locate   ProjectLocator
	open     StoreOpener
}

func newCloseNarrowingWorld() *closeNarrowingWorld {
	w := &closeNarrowingWorld{edges: map[string][]*types.Dependency{
		"be-other": {externalDep("be-other", "external:nowhere:cap", types.DepBlocks)},
		"be-held":  {externalDep("be-held", "external:remote:payments", types.DepBlocks)},
	}}
	foreign := &fakeStore{labels: map[string][]*types.Issue{
		"provides:payments": {{ID: "fx-1", Status: types.StatusOpen}},
	}}
	w.locate = func(project ProjectName) (string, bool) { return "/projects/remote", project == "remote" }
	w.open = func(_ context.Context, path string) (storage.DoltStorage, error) {
		w.opened = append(w.opened, path)
		return foreign, nil
	}
	return w
}

func (w *closeNarrowingWorld) watch(p *Policy) {
	p.warnProject = func(project ProjectName) { w.warnings = append(w.warnings, project) }
}

// check asserts what one close did to the foreign side: no warning ever (the
// only unavailable project is be-other's), and exactly the foreign opens the
// closed issue's own refs need.
func (w *closeNarrowingWorld) check(t *testing.T, op string, wantOpens int) {
	t.Helper()
	if len(w.warnings) != 0 {
		t.Errorf("%s warned about %v, a project only an unrelated issue references", op, w.warnings)
	}
	if len(w.opened) != wantOpens {
		t.Errorf("%s opened foreign stores %v, want %d open(s)", op, w.opened, wantOpens)
	}
	w.warnings, w.opened = nil, nil
}

// TestStoreCloseResolvesOnlyTheClosedIssuesOwnRefs pins the store arm's close
// guards — the policy lifecycle's Close and closing Update, and the batch
// closer without a claim (the direct `bd close` route) — to the closed issue's
// OWN `external:` refs. They used to read every external edge in the
// workspace and open every foreign project those named, so `bd close be-free`
// warned about be-other's unavailable project and opened remote's store. The
// issue's own blocker is still refused.
func TestStoreCloseResolvesOnlyTheClosedIssuesOwnRefs(t *testing.T) {
	w := newCloseNarrowingWorld()
	life := &fakeLifecycle{}
	raw := &fakeStore{
		ready:     []*types.Issue{issue("be-free"), issue("be-other"), issue("be-held")},
		deps:      w.edges,
		lifecycle: life,
	}
	store := New(raw, w.locate, w.open)
	w.watch(store.Policy)
	ctx := t.Context()

	lifecycle, err := store.IssueLifecycle()
	if err != nil {
		t.Fatal(err)
	}
	closer, err := store.BatchCloser()
	if err != nil {
		t.Fatal(err)
	}
	closedUpdate := func(id string) publicops.UpdateRequest {
		req := publicops.UpdateRequest{IssueID: id, Actor: "w"}
		req.Patch.Status.Set, req.Patch.Status.Value = true, types.StatusClosed
		return req
	}

	if _, err := lifecycle.Close(ctx, publicops.CloseRequest{IssueID: "be-free", Actor: "w"}); err != nil {
		t.Fatalf("Close(be-free): %v", err)
	}
	w.check(t, "Close(be-free)", 0)
	if _, err := lifecycle.Update(ctx, closedUpdate("be-free")); err != nil {
		t.Fatalf("Update(be-free, closed): %v", err)
	}
	w.check(t, "Update(be-free, closed)", 0)
	result, err := closer.CloseBatch(ctx, publicops.CloseBatchRequest{Actor: "w", Items: []publicops.BatchCloseItem{{IssueID: "be-free"}}})
	if err != nil || result.Outcomes[0].Err != nil {
		t.Fatalf("CloseBatch(be-free): %v %+v", err, result.Outcomes)
	}
	w.check(t, "CloseBatch(be-free)", 0)
	if raw.edgeReads != 0 {
		t.Errorf("closes read the workspace's external edges %d times, want 0", raw.edgeReads)
	}

	if _, err := lifecycle.Close(ctx, publicops.CloseRequest{IssueID: "be-held", Actor: "w"}); !errors.Is(err, storage.ErrCloseBlocked) {
		t.Fatalf("Close(be-held): err = %v, want the external refusal", err)
	}
	w.check(t, "Close(be-held)", 1)
	if _, err := lifecycle.Update(ctx, closedUpdate("be-held")); !errors.Is(err, storage.ErrCloseBlocked) {
		t.Fatalf("Update(be-held, closed): err = %v, want the external refusal", err)
	}
	w.check(t, "Update(be-held, closed)", 1)
	result, err = closer.CloseBatch(ctx, publicops.CloseBatchRequest{Actor: "w", Items: []publicops.BatchCloseItem{{IssueID: "be-held"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(result.Outcomes[0].Err, storage.ErrCloseBlocked) {
		t.Fatalf("CloseBatch(be-held) outcome = %+v, want the external refusal", result.Outcomes[0])
	}
	w.check(t, "CloseBatch(be-held)", 1)
	if life.closed != 1 || life.updated != 1 {
		t.Errorf("inner lifecycle reached %d closes and %d updates, want 1 and 1 (be-free only)", life.closed, life.updated)
	}
}

// TestUOWCloseResolvesOnlyTheClosedIssuesOwnRefs is the unit-of-work arm's
// counterpart: the batch closer without a claim (proxied `bd close`) and a raw
// unit of work's close (resolving in place, with no pre-resolution) read and
// resolve only the closed issue's own refs.
func TestUOWCloseResolvesOnlyTheClosedIssuesOwnRefs(t *testing.T) {
	w := newCloseNarrowingWorld()
	issues := &fakeIssueUseCase{ready: []*types.Issue{issue("be-free"), issue("be-other"), issue("be-held")}}
	deps := &countingDependencyUseCase{fakeDependencyUseCase: &fakeDependencyUseCase{external: w.edges}}
	inner := &fakeUOW{issues: landingIssues{issues}, deps: deps}
	provider := WrapUOWProvider(&openCountingProvider{uw: inner}, w.locate, w.open)
	w.watch(provider.(*uowProvider).policy)
	ctx := t.Context()

	closer, err := provider.(uow.BatchCloserSource).BatchCloser()
	if err != nil {
		t.Fatal(err)
	}
	result, err := closer.CloseBatch(ctx, publicops.CloseBatchRequest{Actor: "w", Items: []publicops.BatchCloseItem{{IssueID: "be-free"}}})
	if err != nil || result.Outcomes[0].Err != nil {
		t.Fatalf("CloseBatch(be-free): %v %+v", err, result.Outcomes)
	}
	w.check(t, "CloseBatch(be-free)", 0)
	result, err = closer.CloseBatch(ctx, publicops.CloseBatchRequest{Actor: "w", Items: []publicops.BatchCloseItem{{IssueID: "be-held"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(result.Outcomes[0].Err, storage.ErrCloseBlocked) {
		t.Fatalf("CloseBatch(be-held) outcome = %+v, want the external refusal", result.Outcomes[0])
	}
	w.check(t, "CloseBatch(be-held)", 1)

	uw, err := provider.NewUOW(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer uw.Close(ctx)
	if _, err := uw.IssueUseCase().CloseIssueChecked(ctx, "be-free", domain.CloseIssueParams{}, "w", false); err != nil {
		t.Fatalf("raw CloseIssueChecked(be-free): %v", err)
	}
	w.check(t, "raw CloseIssueChecked(be-free)", 0)
	if _, err := uw.IssueUseCase().CloseIssueChecked(ctx, "be-held", domain.CloseIssueParams{}, "w", false); !errors.Is(err, storage.ErrCloseBlocked) {
		t.Fatalf("raw CloseIssueChecked(be-held): err = %v, want the external refusal", err)
	}
	w.check(t, "raw CloseIssueChecked(be-held)", 1)

	assertReads(t, "closes without a claim", deps, 0)
	if !slices.Contains(issues.closed, "be-free") || slices.Contains(issues.closed, "be-held") {
		t.Errorf("closes that reached the use case = %v, want be-free and never be-held", issues.closed)
	}
}
