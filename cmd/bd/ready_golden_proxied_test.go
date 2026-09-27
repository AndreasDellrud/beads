//go:build cgo

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProxiedReadyGoldenOutput pins `bd ready`'s PROXIED-route output byte for
// byte, the twin of TestEmbeddedReadyGoldenOutput over the same seeded
// workspace: JSON, text, pretty, brief, the pagination envelope, the truncation
// hint on both streams, --limit 0 (including gc's exact
// `bd ready --json --include-ephemeral --limit 0`, a plain array), --offset,
// and --claim in both renderings.
//
// Regenerate with -update-ready-golden. The claim cases mutate the workspace,
// so they run last and in a fixed order.
func TestProxiedReadyGoldenOutput(t *testing.T) {
	requireSharedProxiedServer(t)
	bd := buildEmbeddedBD(t)
	p := newSharedProxiedProject(t, bd, "gd")

	run := func(args ...string) {
		t.Helper()
		if out, err := bdProxiedRun(t, bd, p.dir, args...); err != nil {
			t.Fatalf("bd %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	seedReadyGoldenWorkspace(run)

	// The claimant is pinned: the fallback actor is the machine's user name.
	claimEnv := []string{"BEADS_ACTOR=golden-claimer"}
	cases := []struct {
		name string
		args []string
		env  []string
	}{
		{"json_default", []string{"ready", "--json"}, nil},
		{"json_limit2", []string{"ready", "--json", "--limit", "2"}, nil},
		{"json_limit2_envelope", []string{"ready", "--json", "--limit", "2"}, []string{"BD_JSON_ENVELOPE=1"}},
		{"json_limit0", []string{"ready", "--json", "--limit", "0"}, nil},
		{"json_limit0_envelope", []string{"ready", "--json", "--limit", "0"}, []string{"BD_JSON_ENVELOPE=1"}},
		{"json_gc_include_ephemeral_limit0", []string{"ready", "--json", "--include-ephemeral", "--limit", "0"}, nil},
		{"json_brief", []string{"ready", "--json", "--brief", "--limit", "0"}, nil},
		{"json_label", []string{"ready", "--json", "--label", "team-a"}, nil},
		{"json_assignee_priority_sort", []string{"ready", "--json", "--assignee", "alice", "--sort", "priority"}, nil},
		{"json_offset1_limit2", []string{"ready", "--json", "--offset", "1", "--limit", "2"}, nil},
		{"json_offset1_limit2_envelope", []string{"ready", "--json", "--offset", "1", "--limit", "2"}, []string{"BD_JSON_ENVELOPE=1"}},
		{"json_empty", []string{"ready", "--json", "--label", "nobody"}, nil},
		{"text_default", []string{"ready"}, nil},
		{"text_limit2", []string{"ready", "--limit", "2"}, nil},
		{"text_limit0", []string{"ready", "--limit", "0"}, nil},
		{"text_plain_limit2", []string{"ready", "--plain", "--limit", "2"}, nil},
		{"text_pretty", []string{"ready", "--pretty"}, nil},
		{"text_pretty_limit2", []string{"ready", "--pretty", "--limit", "2"}, nil},
		{"text_include_ephemeral", []string{"ready", "--include-ephemeral", "--limit", "0"}, nil},
		{"text_empty", []string{"ready", "--label", "nobody"}, nil},
		{"text_max_rows_refused", []string{"ready", "--limit", "0", "--max-rows", "2"}, nil},
		{"json_claim", []string{"ready", "--claim", "--json"}, claimEnv},
		{"text_claim", []string{"ready", "--claim"}, claimEnv},
		{"json_claim_none", []string{"ready", "--claim", "--json", "--label", "nobody"}, claimEnv},
		{"text_claim_none", []string{"ready", "--claim", "--label", "nobody"}, claimEnv},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, err := bdProxiedRunBuffersWithEnv(t, bd, p.dir, tc.env, tc.args...)
			exit := "0"
			if err != nil {
				exit = "nonzero"
			}
			got := "$ bd " + strings.Join(tc.args, " ") + "\n" +
				"--- exit: " + exit + "\n" +
				"--- stdout\n" + normalizeReadyGolden(stdout) +
				"--- stderr\n" + normalizeReadyGolden(stderr)
			path := filepath.Join("testdata", "ready_golden_proxied", tc.name+".golden")
			if *updateReadyGolden {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden (regenerate with -update-ready-golden): %v", err)
			}
			if !bytes.Equal([]byte(got), want) {
				t.Errorf("bd %s output changed.\n--- got\n%s\n--- want\n%s", strings.Join(tc.args, " "), got, want)
			}
		})
	}
}
