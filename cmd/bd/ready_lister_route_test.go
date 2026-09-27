package main

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/hooks"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// runDirectReady drives readyCmd's RunE on the DIRECT route against chain, with
// the command line args, and returns what it printed to stdout.
func runDirectReady(t *testing.T, chain storage.DoltStorage, asJSON bool, args ...string) string {
	t.Helper()
	oldStore, oldCtx, oldJSON, oldProxied := store, rootCtx, jsonOutput, proxiedServerMode
	t.Cleanup(func() { store, rootCtx, jsonOutput, proxiedServerMode = oldStore, oldCtx, oldJSON, oldProxied })
	store = chain
	rootCtx = t.Context()
	jsonOutput = asJSON
	proxiedServerMode = false

	cmd := newReadyFlagsCommand(t, args...)
	return captureStdout(t, func() error { return readyCmd.RunE(cmd, nil) })
}

// TestReadyDirectRouteForwardsTheRequestToAServerEnforcedStore drives `bd
// ready` through the whole cmd/bd storage chain against a client of a
// policy-enforcing bd server (storage.ServerEnforcedPolicy): the listing must
// reach that store's OWN ReadyLister exactly once, carrying every flag the
// command line set on the matching request field and nothing else — no
// client-side ExcludeIDs, no rewritten labels — and must never read dependency
// records (the stub fails the test if the external-dependency policy runs).
// The output is the role's answer: the page, and its total in the pagination
// envelope.
func TestReadyDirectRouteForwardsTheRequestToAServerEnforcedStore(t *testing.T) {
	clearTelemetryEnv(t)
	t.Setenv("BD_OTEL_STDOUT", "true")
	t.Setenv("BEADS_MAX_ROWS", "")
	t.Setenv("BD_JSON_ENVELOPE", "1")
	stub := newPolicyChainStub(t)
	stub.forbidEdges = true
	stub.listing = issueops.ReadyListing{
		Items: []*types.IssueWithCounts{
			{Issue: &types.Issue{ID: "bd-1", Title: "one", Status: types.StatusOpen}},
			{Issue: &types.Issue{ID: "bd-2", Title: "two", Status: types.StatusOpen}},
		},
		HasMore: true,
		Total:   5,
	}
	chain := wireStorageDecorators(serverEnforcedChainStub{stub}, hooks.NewRunner(t.TempDir()), false)

	out := runDirectReady(t, chain, true,
		"--limit", "2",
		"--assignee", "bob",
		"--sort", "oldest",
		"--label", "a,b",
		"--label-any", "c",
		"--exclude-label", "d",
		"--label-pattern", "x*",
		"--label-regex", "y.*",
		"--type", "bug",
		"--parent", "bd-p",
		"--include-deferred",
		"--include-ephemeral",
		"--exclude-type", "epic",
		"--priority", "0",
		"--mol-type", "swarm",
		"--metadata-field", "team=core",
		"--has-metadata-key", "team",
		"--brief",
		"--max-rows", "50",
	)

	if len(stub.listed) != 1 {
		t.Fatalf("the server-enforced store's ReadyLister was called %d times, want exactly 1 (reader=%d counter=%d)",
			len(stub.listed), len(stub.ready), len(stub.counted))
	}
	if len(stub.ready) != 0 || len(stub.counted) != 0 {
		t.Errorf("the listing also reached Reader.Ready %d and CountReady %d times; it is one role call", len(stub.ready), len(stub.counted))
	}
	limit, priority, molType := 2, 0, types.MolType("swarm")
	want := issueops.ReadyListRequest{
		ReadyRequest: issueops.ReadyRequest{
			IssueType:        "bug",
			Assignee:         "bob",
			Labels:           []string{"a", "b"},
			LabelsAny:        []string{"c"},
			ExcludeLabels:    []string{"d"},
			LabelPattern:     "x*",
			LabelRegex:       "y.*",
			Priority:         &priority,
			ParentID:         "bd-p",
			MolType:          &molType,
			IncludeDeferred:  true,
			IncludeEphemeral: true,
			ExcludeTypes:     []string{"epic"},
			MetadataFields:   map[string]string{"team": "core"},
			HasMetadataKey:   "team",
			Sort:             "oldest",
			Limit:            &limit,
			Brief:            true,
		},
		MaxRows:       50,
		MaxRowsSource: "--max-rows",
	}
	if got := stub.listed[0]; !reflect.DeepEqual(got, want) {
		t.Errorf("forwarded request:\n got %+v\nwant %+v", got, want)
	}

	var envelope struct {
		Data       []types.IssueWithCounts `json:"data"`
		Pagination *PaginationMeta         `json:"pagination"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &envelope); err != nil {
		t.Fatalf("parse ready JSON: %v\n%s", err, out)
	}
	if len(envelope.Data) != 2 || envelope.Pagination == nil || envelope.Pagination.Total != 5 || !envelope.Pagination.Truncated {
		t.Errorf("output = %d rows, pagination %+v; want 2 rows and total 5 from the role's listing", len(envelope.Data), envelope.Pagination)
	}
}

// TestReadyDirectRouteClaimIsUnaffected pins that --claim still goes to the
// ReadyClaimer role — not to the listing — with the page cleared off the
// request and the cap left behind.
func TestReadyDirectRouteClaimIsUnaffected(t *testing.T) {
	clearTelemetryEnv(t)
	t.Setenv("BEADS_MAX_ROWS", "")
	stub := newPolicyChainStub(t)
	stub.forbidEdges = true
	chain := wireStorageDecorators(serverEnforcedChainStub{stub}, hooks.NewRunner(t.TempDir()), false)

	runDirectReady(t, chain, true, "--claim", "--label", "a", "--limit", "3", "--max-rows", "9")

	if len(stub.listed) != 0 {
		t.Errorf("--claim listed %d times; a claim is the ReadyClaimer's alone", len(stub.listed))
	}
	if len(stub.claims) != 1 {
		t.Fatalf("ReadyClaimer called %d times, want 1", len(stub.claims))
	}
	got := stub.claims[0].Filter
	if got.Limit != nil || got.Offset != 0 || !reflect.DeepEqual(got.Labels, []string{"a"}) {
		t.Errorf("claim request = %+v; want the labels and no page", got)
	}
}

// The routed-read probe `bd ready` runs before listing reads workspace config;
// an empty config means "no contributor routing", so the listing stays on the
// store under test.
func (s *policyChainStub) GetAllConfig(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}

func (s *policyChainStub) GetConfig(context.Context, string) (string, error) { return "", nil }
