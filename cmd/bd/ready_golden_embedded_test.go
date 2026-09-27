//go:build cgo

package main

import (
	"bytes"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var updateReadyGolden = flag.Bool("update-ready-golden", false, "rewrite cmd/bd/testdata/ready_golden/*.golden from the bd binary under test")

// readyGoldenTimestamp matches every RFC 3339 timestamp bd prints, which is the
// only nondeterminism in a seeded workspace's ready output besides tips.
var readyGoldenTimestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)

// readyGoldenTip matches the rotating tip the text renderings may append.
var readyGoldenTip = regexp.MustCompile(`\n💡 Tip: [^\n]*\n`)

// TestEmbeddedReadyGoldenOutput pins `bd ready`'s direct-route output byte for
// byte — JSON, text, pretty, brief, the truncation hint and the pagination
// envelope — across representative invocations, including gc's exact
// `bd ready --json --include-ephemeral --limit 0` (a plain array).
//
// The golden files were generated from the binary BEFORE `bd ready`'s listing
// moved onto issueops.ReadyLister (-update-ready-golden with
// BEADS_TEST_BD_BINARY pointing at it), so this test is the before/after
// equivalence for that move: the listing changed roles, the output did not.
func TestEmbeddedReadyGoldenOutput(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "gd")

	run := func(args ...string) {
		t.Helper()
		if out, err := bdRunWithFlockRetry(t, bd, dir, args...); err != nil {
			t.Fatalf("bd %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	// Distinct priorities where the hybrid order would otherwise fall back to
	// creation time, so the order is a property of the data.
	run("create", "--silent", "--id", "gd-alpha", "Alpha task", "--type", "task", "--priority", "1", "--labels", "team-a", "--estimate", "30")
	run("create", "--silent", "--id", "gd-bug", "Crash on start", "--type", "bug", "--priority", "0", "--description", "long text that --brief drops")
	run("create", "--silent", "--id", "gd-feat", "Shiny feature", "--type", "feature", "--priority", "2", "--assignee", "alice", "--labels", "team-a,team-b")
	run("create", "--silent", "--id", "gd-blocked", "Blocked task", "--type", "task", "--priority", "0", "--deps", "blocked-by:gd-alpha")
	run("create", "--silent", "--id", "gd-epic", "Big epic", "--type", "epic", "--priority", "3")
	run("create", "--silent", "Epic child", "--type", "task", "--priority", "4", "--parent", "gd-epic")
	run("create", "--silent", "--id", "gd-wisp", "Ephemeral step", "--type", "task", "--priority", "2", "--ephemeral")

	cases := []struct {
		name string
		args []string
		env  []string
	}{
		{"json_default", []string{"ready", "--json"}, nil},
		{"json_limit2", []string{"ready", "--json", "--limit", "2"}, nil},
		{"json_limit2_envelope", []string{"ready", "--json", "--limit", "2"}, []string{"BD_JSON_ENVELOPE=1"}},
		{"json_gc_include_ephemeral_limit0", []string{"ready", "--json", "--include-ephemeral", "--limit", "0"}, nil},
		{"json_brief", []string{"ready", "--json", "--brief", "--limit", "0"}, nil},
		{"json_label", []string{"ready", "--json", "--label", "team-a"}, nil},
		{"json_assignee_priority_sort", []string{"ready", "--json", "--assignee", "alice", "--sort", "priority"}, nil},
		{"json_empty", []string{"ready", "--json", "--label", "nobody"}, nil},
		{"text_default", []string{"ready"}, nil},
		{"text_limit2", []string{"ready", "--limit", "2"}, nil},
		{"text_pretty", []string{"ready", "--pretty"}, nil},
		{"text_pretty_limit2", []string{"ready", "--pretty", "--limit", "2"}, nil},
		{"text_include_ephemeral", []string{"ready", "--include-ephemeral", "--limit", "0"}, nil},
		{"text_empty", []string{"ready", "--label", "nobody"}, nil},
		{"text_max_rows_refused", []string{"ready", "--limit", "0", "--max-rows", "2"}, nil},
		{"json_max_rows_refused", []string{"ready", "--json", "--limit", "0", "--max-rows", "2"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(bd, tc.args...)
			cmd.Dir = dir
			cmd.Env = append(bdEnv(dir), tc.env...)
			stdout, stderr, err := runCommandBuffers(t, cmd)
			exit := "0"
			if err != nil {
				exit = "nonzero"
			}
			got := "$ bd " + strings.Join(tc.args, " ") + "\n" +
				"--- exit: " + exit + "\n" +
				"--- stdout\n" + normalizeReadyGolden(stdout.String()) +
				"--- stderr\n" + normalizeReadyGolden(stderr.String())
			path := filepath.Join("testdata", "ready_golden", tc.name+".golden")
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

func normalizeReadyGolden(s string) string {
	s = readyGoldenTimestamp.ReplaceAllString(s, "<TS>")
	s = readyGoldenTip.ReplaceAllString(s, "")
	return s
}
