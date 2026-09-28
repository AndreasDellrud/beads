#!/usr/bin/env python3
"""Check that `bazel test --config=ci` ran the same Go tests `go test` would.

The Bazel lane is only a safe replacement for the `go test` lanes in pr.yml
(PR Core runs `go test -race -short -skip '^TestEmbedded' ./...`) if it runs
the same tests. A tag filter, a stray `manual` tag, a package gazelle never
picked up, or a sharding bug that makes a shard run zero tests all shrink the
Bazel set silently: every target Bazel did run is still green. This script
makes that shrinkage a failure.

Two checks:

1. Package set. Every package `go list` reports with test files must have a
   go_test target (`bazel query 'kind("go_test rule", //...)'`).
2. Test-name set. For every such package, each top-level `func TestXxx(t
   *testing.T)` in the files `go list` selects (the same -race and
   gms_pure_go build context as PR Core, minus the `^TestEmbedded` skip) must
   appear as a <testcase> in the test.xml of a go_test target that this Bazel
   invocation tested (read from --build_event_json_file). Runtime skips count
   as run: `go test` skips them the same way. Bazel testcases that `go test`
   would not run are reported too.

Divergences listed in the allowlist (tools/bazel/equivalence_allowlist.txt)
are expected; anything else fails. The allowlist format is one entry per line:

    <package dir> <TestName or glob>   # why this is expected

`*` as the test name matches the whole package (including the package-set
check).

Requires: go (with the module cache the packages need), bazel for the query
unless --no-query, and the BEP JSON plus bazel-testlogs of a `--config=ci`
run (which sets GO_TEST_WRAP_TESTV=1 and downloads test.xml).
"""
import argparse
import fnmatch
import json
import os
import re
import subprocess
import sys
import xml.etree.ElementTree as ET

GO_LIST_ARGS = ["list", "-e", "-race", "-tags", "gms_pure_go", "-json", "./..."]
# pr.yml's go test lanes pass -skip '^TestEmbedded' (as does test:prcore).
SKIP_RE = re.compile(r"^TestEmbedded")
TEST_FUNC_RE = re.compile(r"^func\s+(Test\w*)\s*\(\s*\w+\s+\*testing\.T\s*\)", re.M)
STATUS_RANK = {"skipped": 0, "passed": 1, "failed": 2}
DEFAULT_ALLOWLIST = os.path.join(os.path.dirname(os.path.abspath(__file__)), "equivalence_allowlist.txt")


def is_go_test_name(name):
    # cmd/go: "TestXxx" where Xxx does not start with a lower-case letter.
    return name == "Test" or not name[4].islower()


def go_expected(root, go_json_path=None):
    """Return {pkg_dir: set(test names)} for packages with test files."""
    if go_json_path:
        with open(go_json_path, encoding="utf-8") as f:
            raw = f.read()
    else:
        raw = subprocess.run(["go", *GO_LIST_ARGS], cwd=root, check=True, capture_output=True, text=True).stdout
    decoder = json.JSONDecoder()
    pkgs, idx = [], 0
    while idx < len(raw):
        while idx < len(raw) and raw[idx].isspace():
            idx += 1
        if idx >= len(raw):
            break
        obj, idx = decoder.raw_decode(raw, idx)
        pkgs.append(obj)

    expected = {}
    for p in pkgs:
        files = (p.get("TestGoFiles") or []) + (p.get("XTestGoFiles") or [])
        if not files:
            continue
        rel = os.path.relpath(p["Dir"], root)
        rel = "" if rel == "." else rel.replace(os.sep, "/")
        names = set()
        for name in files:
            with open(os.path.join(p["Dir"], name), encoding="utf-8", errors="replace") as f:
                for m in TEST_FUNC_RE.finditer(f.read()):
                    t = m.group(1)
                    if is_go_test_name(t) and not SKIP_RE.search(t):
                        names.add(t)
        expected[rel] = names
    return expected


def label_pkg(label):
    # "//cmd/bd:bd_test" -> "cmd/bd", "@@//:x" -> ""
    label = label.lstrip("@")
    return label[2:].split(":", 1)[0] if label.startswith("//") else label.split(":", 1)[0]


def query_go_test_pkgs(root, bazel):
    out = subprocess.run(
        [bazel, "query", 'kind("go_test rule", //...)', "--output=label"],
        cwd=root, check=True, capture_output=True, text=True,
    ).stdout
    return {label_pkg(l) for l in out.split()}


def read_bep(path):
    """Return ({label: shard_count} for tested go_test targets, set of go_test
    labels configured, this invocation's testlogs dir or None)."""
    go_tests, shards = set(), {}
    exec_root, testlogs_rel = None, None
    with open(path, encoding="utf-8") as f:
        for line in f:
            if not line.strip():
                continue
            ev = json.loads(line)
            eid = ev.get("id", {})
            if "workspace" in eid:
                exec_root = ev.get("workspaceInfo", {}).get("localExecRoot")
            elif "convenienceSymlinksIdentified" in eid:
                for link in ev.get("convenienceSymlinksIdentified", {}).get("convenienceSymlinks", []):
                    if link.get("path", "").endswith("testlogs") and link.get("target"):
                        testlogs_rel = link["target"]
            elif "targetConfigured" in eid:
                if ev.get("configured", {}).get("targetKind") == "go_test rule":
                    go_tests.add(eid["targetConfigured"]["label"])
            elif "testResult" in eid:
                tr = eid["testResult"]
                shards[tr["label"]] = max(shards.get(tr["label"], 0), int(tr.get("shard", 1) or 1))
    tested = {l: n for l, n in shards.items() if l in go_tests}
    testlogs = None
    if exec_root and testlogs_rel:
        # The symlink target is relative to the output base, two levels above
        # the exec root (<output_base>/execroot/<workspace>).
        testlogs = os.path.join(os.path.dirname(os.path.dirname(exec_root)), testlogs_rel)
    return tested, go_tests, testlogs


def testlog_xmls(testlogs, label, shard_count):
    pkg = label_pkg(label)
    name = label.split(":", 1)[1]
    base = os.path.join(testlogs, pkg, name)
    if shard_count <= 1:
        return [os.path.join(base, "test.xml")]
    return [os.path.join(base, f"shard_{i}_of_{shard_count}", "test.xml") for i in range(1, shard_count + 1)]


def bazel_observed(tested, testlogs):
    """Return ({pkg: {test: status}}, [problems])."""
    observed, problems = {}, []
    for label, n in sorted(tested.items()):
        pkg = label_pkg(label)
        per = observed.setdefault(pkg, {})
        for xml_path in testlog_xmls(testlogs, label, n):
            if not os.path.exists(xml_path):
                problems.append(f"{label}: {xml_path} missing (was test.xml downloaded? run with --config=ci)")
                continue
            try:
                tree = ET.parse(xml_path)
            except ET.ParseError as e:
                problems.append(f"{label}: cannot parse {xml_path}: {e}")
                continue
            cases = tree.getroot().iter("testcase")
            count = 0
            for tc in cases:
                name = tc.get("name", "")
                if "/" in name or not name.startswith("Test"):
                    continue
                count += 1
                if tc.find("failure") is not None or tc.find("error") is not None:
                    status = "failed"
                elif tc.find("skipped") is not None:
                    status = "skipped"
                else:
                    status = "passed"
                # A test seen in several targets keeps its worst status.
                if name not in per or STATUS_RANK[status] > STATUS_RANK[per[name]]:
                    per[name] = status
            if count == 0 and os.path.getsize(xml_path) < 64:
                # An empty <testsuites/> means the binary ran no tests at all
                # (or GO_TEST_WRAP_TESTV was off). The test-name diff reports
                # which tests are missing; note it here for the log.
                problems.append(f"{label}: {os.path.relpath(xml_path, testlogs)} lists no testcases")
    return observed, problems


def load_allowlist(path):
    entries, errors = [], []
    if not path or not os.path.exists(path):
        return entries, errors
    with open(path, encoding="utf-8") as f:
        for n, line in enumerate(f, 1):
            body, _, why = line.partition("#")
            body = body.strip()
            if not body:
                continue
            fields = body.split()
            if len(fields) != 2 or not why.strip():
                errors.append(f"{path}:{n}: want '<package dir> <test glob>  # justification', got {line.strip()!r}")
                continue
            entries.append({"pkg": fields[0], "test": fields[1], "why": why.strip(), "line": n, "used": False})
    return entries, errors


def allowed(entries, pkg, test):
    for e in entries:
        if fnmatch.fnmatchcase(pkg, e["pkg"]) and fnmatch.fnmatchcase(test, e["test"]):
            e["used"] = True
            return True
    return False


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--bep", required=True, help="--build_event_json_file of the bazel test run")
    ap.add_argument("--root", default=os.getcwd(), help="workspace root (default: cwd)")
    ap.add_argument("--testlogs", default=None, help="default: from the BEP, else <root>/bazel-testlogs")
    ap.add_argument("--allowlist", default=DEFAULT_ALLOWLIST)
    ap.add_argument("--go-list-json", default=None, help="precomputed `go list -json` output (tests)")
    ap.add_argument("--bazel", default=os.environ.get("BAZEL", "bazel"))
    ap.add_argument("--no-query", action="store_true", help="skip the bazel query package-set check")
    ap.add_argument("--details", action="store_true", help="list every divergence (nightly)")
    ap.add_argument("--summary", default=None, help="append Markdown here (default: $GITHUB_STEP_SUMMARY)")
    args = ap.parse_args(argv)

    root = os.path.abspath(args.root)
    entries, allow_errors = load_allowlist(args.allowlist)

    expected = go_expected(root, args.go_list_json)
    tested, configured, bep_testlogs = read_bep(args.bep)
    # Prefer the BEP's own testlogs dir: the bazel-testlogs symlink follows
    # whatever bazel command ran last, possibly in another output base.
    testlogs = args.testlogs or bep_testlogs or os.path.join(root, "bazel-testlogs")
    observed, problems = bazel_observed(tested, testlogs)
    target_pkgs = None if args.no_query else query_go_test_pkgs(root, args.bazel)

    no_target, missing, extra, allowlisted = [], [], [], []
    if target_pkgs is not None:
        for pkg in sorted(set(expected) - target_pkgs):
            (allowlisted if allowed(entries, pkg, "*") else no_target).append((pkg, "*"))
    for pkg, names in sorted(expected.items()):
        seen = observed.get(pkg, {})
        for t in sorted(names - set(seen)):
            (allowlisted if allowed(entries, pkg, t) else missing).append((pkg, t))
    for pkg, seen in sorted(observed.items()):
        for t in sorted(set(seen) - expected.get(pkg, set())):
            if SKIP_RE.search(t):
                continue
            (allowlisted if allowed(entries, pkg, t) else extra).append((pkg, t))
    # Dedupe allowlisted pairs (a package-level entry also covers its tests).
    allowlisted = sorted(set(allowlisted))
    unused = [e for e in entries if not e["used"]]

    statuses = {"passed": 0, "skipped": 0, "failed": 0}
    for seen in observed.values():
        for s in seen.values():
            statuses[s] += 1
    n_expected = sum(len(v) for v in expected.values())
    ok = not (no_target or missing or extra or allow_errors)

    limit = None if args.details else 40

    def listing(title, pairs):
        out = [f"{title} ({len(pairs)}):"]
        shown = pairs if limit is None else pairs[:limit]
        out += [f"  {p or '.'}  {t}" for p, t in shown]
        if limit is not None and len(pairs) > limit:
            out.append(f"  ... {len(pairs) - limit} more (run with --details)")
        return out

    lines = [
        f"go test:  {len(expected)} packages with tests, {n_expected} top-level tests (excluding ^TestEmbedded)",
        f"bazel:    {len(configured)} go_test targets configured, {len(tested)} tested; "
        f"{sum(len(v) for v in observed.values())} top-level tests seen "
        f"({statuses['passed']} passed, {statuses['skipped']} skipped, {statuses['failed']} failed)",
    ]
    if target_pkgs is not None:
        lines.append(f"query:    {len(target_pkgs)} packages with a go_test target")
    lines.append(f"allowlisted divergences: {len(allowlisted)}")
    for title, pairs in (
        ("packages with Go tests but no go_test target", no_target),
        ("tests go test runs that Bazel did not", missing),
        ("tests Bazel ran that go test would not", extra),
    ):
        if pairs:
            lines += listing(title, pairs)
    if args.details and allowlisted:
        lines += listing("allowlisted", allowlisted)
    for e in unused:
        lines.append(f"note: allowlist line {e['line']} ({e['pkg']} {e['test']}) matched nothing; remove it")
    for p in problems:
        lines.append(f"note: {p}")
    lines += allow_errors
    verdict = "EQUIVALENCE: ok" if ok else "EQUIVALENCE: FAIL (unexplained divergence; fix the BUILD tags/targets or add a justified allowlist entry)"
    lines.append(verdict)
    print("\n".join(lines))

    summary = args.summary or os.environ.get("GITHUB_STEP_SUMMARY")
    if summary:
        head = (
            f"**Equivalence**: {'ok' if ok else 'FAIL'}; go test {n_expected} tests in {len(expected)} packages, "
            f"Bazel saw {sum(len(v) for v in observed.values())} "
            f"(missing {len(missing)}, extra {len(extra)}, packages without target {len(no_target)}, "
            f"allowlisted {len(allowlisted)})\n\n"
        )
        with open(summary, "a", encoding="utf-8") as f:
            f.write(head)
            f.write("<details><summary>Equivalence detail</summary>\n\n```\n" + "\n".join(lines) + "\n```\n</details>\n\n")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
