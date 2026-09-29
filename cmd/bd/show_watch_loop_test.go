package main

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

// scriptedWatchSource replays a fixed sequence of fetch results and counts
// renders, so the loop's redraw rule can be checked without a store.
type scriptedWatchSource struct {
	initial *types.Issue
	fetches []*types.Issue
	renders int
	// rendersAtFetch records the render count as each poll starts, i.e. the
	// result of the poll before it. Written only on the loop's goroutine and
	// read after the loop returns.
	rendersAtFetch []int
}

func (s *scriptedWatchSource) source() issueWatchSource {
	return issueWatchSource{
		render: func(context.Context) *types.Issue {
			s.renders++
			return s.initial
		},
		fetch: func(context.Context) *types.Issue {
			s.rendersAtFetch = append(s.rendersAtFetch, s.renders)
			next := s.fetches[0]
			s.fetches = s.fetches[1:]
			return next
		},
	}
}

// TestWatchIssueLoopRedrawsOnlyOnSnapshotChange pins the redraw rule both
// `bd show --watch` routes share: an unchanged snapshot or a failed read does
// not redraw, a status or updated_at change does, and stop ends the loop.
func TestWatchIssueLoopRedrawsOnlyOnSnapshotChange(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	open := &types.Issue{ID: "w-1", Status: types.StatusOpen, UpdatedAt: base}
	sameAgain := &types.Issue{ID: "w-1", Status: types.StatusOpen, UpdatedAt: base}
	inProgress := &types.Issue{ID: "w-1", Status: types.StatusInProgress, UpdatedAt: base.Add(time.Second)}
	touched := &types.Issue{ID: "w-1", Status: types.StatusInProgress, UpdatedAt: base.Add(2 * time.Second)}

	src := &scriptedWatchSource{
		initial: open,
		fetches: []*types.Issue{sameAgain, nil, inProgress, inProgress, touched},
	}
	tick := make(chan time.Time)
	stop := make(chan os.Signal, 1)
	done := make(chan bool)
	go func() { done <- watchIssueLoop(context.Background(), src.source(), tick, stop) }()

	// tick is unbuffered, so each send lands only once the loop is back in
	// its select, i.e. after the previous poll (and any redraw) finished.
	for range src.fetches {
		tick <- time.Time{}
	}
	stop <- os.Interrupt
	if !<-done {
		t.Fatal("watchIssueLoop reported nothing watched after a successful render")
	}
	// Poll order: unchanged, failed read, status change, unchanged,
	// updated_at change.
	if want := []int{1, 1, 1, 2, 2}; !slices.Equal(src.rendersAtFetch, want) {
		t.Fatalf("renders before each poll = %v, want %v", src.rendersAtFetch, want)
	}
	if src.renders != 3 {
		t.Fatalf("renders = %d, want 3 (initial, status change, updated_at change)", src.renders)
	}
}

// TestWatchIssueLoopStopsWhenNothingToWatch pins that a failed initial render
// returns at once rather than polling an issue that was never shown.
func TestWatchIssueLoopStopsWhenNothingToWatch(t *testing.T) {
	src := issueWatchSource{
		render: func(context.Context) *types.Issue { return nil },
		fetch: func(context.Context) *types.Issue {
			t.Fatal("fetch called after a failed initial render")
			return nil
		},
	}
	if watchIssueLoop(context.Background(), src, make(chan time.Time), make(chan os.Signal)) {
		t.Fatal("watchIssueLoop reported watching after a failed initial render")
	}
}
