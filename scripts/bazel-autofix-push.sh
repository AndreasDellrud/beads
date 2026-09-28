#!/usr/bin/env bash
# bazel-autofix-push.sh - apply a CI-generated `make bazel-sync` patch
# (BUILD.bazel / MODULE.bazel / MODULE.bazel.lock) to a PR branch, or fall
# back to an instructive PR comment when pushing is not possible.
#
# Runs on the PRIVILEGED side of the bazel-autofix workflow_run pipeline: the
# checkout is always the base repository's default branch (trusted code), and
# the patch produced by the unprivileged PR build (scripts/ci/bazel-sync-patch.sh
# in bazel.yml) is treated as UNTRUSTED DATA. Confinement is layered:
#   * the path allowlist below pins WHICH files a patch may name (anchored
#     regex, no hidden or `..` segments, nothing under third_party/);
#   * any mode line, symlink, rename/copy or binary hunk is refused, so only
#     regular 100644 files are created, edited or deleted;
#   * `git apply --index` supplies the underlying escape guards (rejects `..`
#     paths, absolute paths, and writes through in-patch symlinks);
#   * the staged result is re-checked against the same allowlist.
# A hostile patch can therefore at most rewrite Bazel build files on its own
# PR branch, which its author could push there anyway.
#
# Usage:
#   bazel-autofix-push.sh             validate, then push or comment (env below)
#   bazel-autofix-push.sh --check P   validate patch P only; exit 0 if acceptable
#
# Inputs (environment):
#   BASE_REPO    base "owner/name" (e.g. gastownhall/beads)
#   HEAD_REPO    PR head "owner/name" (same as BASE_REPO for branch PRs;
#                may be empty if the head fork was deleted)
#   HEAD_BRANCH  PR head branch name
#   HEAD_SHA     head commit the failing run was built from
#   PATCH_FILE   path to the downloaded bazel-sync.patch
#   META_FILE    optional bazel-sync-meta.txt from the same artifact; untrusted,
#                used only to skip a patch whose head_sha disagrees with HEAD_SHA
#   RUN_ID       workflow run id that produced the patch (for comment text)
#   RUN_URL      html url of that run (for commit/comment provenance)
#   GH_TOKEN     token for gh api calls (PR lookup, comments) - needs the
#                workflow's pull-requests:write; never the PAT
#   PUSH_TOKEN   token for the git push only (optional; defaults to GH_TOKEN),
#                so the shared DOCS_AUTOFIX_TOKEN needs contents:write only
#   AUTOFIX_TOKEN_KIND  "pat" when a dedicated push token is in use, "default"
#                       for the workflow's GITHUB_TOKEN (retrigger caveat)
#
# Exit 0 on every non-actionable outcome (PR closed, head moved, no patch);
# exit 1 on a refused patch or a genuine error so the workflow surfaces them.

set -euo pipefail
export LC_ALL=C

COMMENT_MARKER="<!-- bazel-sync-autofix -->"
AUTOFIX_SUBJECT="build(bazel): auto-sync BUILD files"

# Files `make bazel-sync` may write and this bot may push - keep identical to
# scripts/ci/bazel-sync-patch.sh (scripts/ci_workflow_test.go checks). Every
# segment starts with a conservative non-dot character: no traversal, no
# .github/, no metacharacters or quoted names can slip through.
BUILD_FILE_RE='^([A-Za-z0-9_+-][A-Za-z0-9_.+-]*/)*BUILD\.bazel$'

path_allowed() {
    case "$1" in
        MODULE.bazel | MODULE.bazel.lock) return 0 ;;
        third_party/*) return 1 ;;
    esac
    [[ "$1" =~ $BUILD_FILE_RE ]]
}

# validate_patch FILE: refuse anything but plain text edits to allowlisted
# regular files.
validate_patch() {
    local file="$1" path bad=""
    # Only 100644 files: no symlink (120000), executable, gitlink or bare mode
    # change; no rename/copy (which would name a second path); no binary hunk.
    if grep -qE '^(old mode|new mode|similarity index|dissimilarity index|rename (from|to)|copy (from|to)|GIT binary patch|Binary files )' "$file"; then
        echo "REFUSED: patch contains a mode change, rename/copy or binary hunk."
        return 1
    fi
    if grep -E '^(new file mode|deleted file mode) ' "$file" | grep -qvE '^(new file mode|deleted file mode) 100644$'; then
        echo "REFUSED: patch creates or deletes a non-regular (symlink/executable/submodule) file."
        return 1
    fi
    # "index a..b MODE" appears on edits of an existing file.
    if grep -E '^index [0-9a-f]+\.\.[0-9a-f]+ ' "$file" | grep -qvE ' 100644$'; then
        echo "REFUSED: patch edits a file whose mode is not 100644."
        return 1
    fi
    # --numstat prints "added<TAB>deleted<TAB>path"; unusual names come back
    # quoted and renames as "old => new", both of which the allowlist rejects.
    local numstat
    if ! numstat="$(git apply --numstat "$file")"; then
        echo "REFUSED: git apply cannot parse the patch."
        return 1
    fi
    if [ -z "$numstat" ]; then
        echo "REFUSED: patch names no files."
        return 1
    fi
    while IFS=$'\t' read -r _ _ path; do
        [ -n "$path" ] || continue
        path_allowed "$path" || bad="${bad}  ${path}\n"
    done <<<"$numstat"
    if [ -n "$bad" ]; then
        printf 'REFUSED: patch touches paths outside the Bazel build-file allowlist:\n%b' "$bad"
        return 1
    fi
}

if [ "${1:-}" = "--check" ]; then
    [ -s "${2:-}" ] || { echo "usage: $0 --check <patch>" >&2; exit 2; }
    validate_patch "$2"
    echo "OK: patch touches only allowlisted Bazel build files."
    exit 0
fi

if [ -z "${HEAD_REPO:-}" ] || [ -z "${HEAD_BRANCH:-}" ]; then
    echo "Head repository/branch unavailable (deleted fork?); nothing to do."
    exit 0
fi
: "${BASE_REPO:?}" "${HEAD_SHA:?}"
: "${PATCH_FILE:?}" "${RUN_ID:?}" "${RUN_URL:?}" "${GH_TOKEN:?}"
AUTOFIX_TOKEN_KIND="${AUTOFIX_TOKEN_KIND:-default}"
PUSH_TOKEN="${PUSH_TOKEN:-$GH_TOKEN}"

if [ ! -s "$PATCH_FILE" ]; then
    echo "No patch content; nothing to do."
    exit 0
fi
PATCH_FILE="$(readlink -f "$PATCH_FILE")"

# --- Validate the untrusted patch --------------------------------------------

validate_patch "$PATCH_FILE"

# The metadata can only veto: a patch built for another head is not ours.
if [ -n "${META_FILE:-}" ] && [ -f "$META_FILE" ]; then
    meta_sha="$(sed -n 's/^head_sha=\([0-9a-f]\{40\}\)$/\1/p' "$META_FILE" | head -1)"
    if [ -n "$meta_sha" ] && [ "$meta_sha" != "$HEAD_SHA" ]; then
        echo "Patch was built for head $meta_sha, not $HEAD_SHA; skipping."
        exit 0
    fi
fi

# --- Resolve the PR and confirm the patch is still current -------------------

# List-and-filter client side: branch names with URL metacharacters would
# corrupt a ?head= query string, and jq --arg needs no encoding.
PULLS_JSON="$(gh api --paginate "repos/$BASE_REPO/pulls?state=open&per_page=100")"
PR_MATCH="$(printf '%s' "$PULLS_JSON" | jq -r -s --arg repo "$HEAD_REPO" --arg branch "$HEAD_BRANCH" \
    'add | [ .[] | select(.head.ref == $branch and (.head.repo.full_name // "") == $repo) ]
     | .[0] | if . == null then "" else "\(.number) \(.head.sha)" end')"
PR_NUMBER="${PR_MATCH%% *}"
PR_HEAD_NOW="${PR_MATCH##* }"

if [ -z "$PR_NUMBER" ]; then
    echo "No open PR for $HEAD_REPO:$HEAD_BRANCH; nothing to do."
    exit 0
fi
if [ "$PR_HEAD_NOW" != "$HEAD_SHA" ]; then
    echo "PR #$PR_NUMBER head moved ($HEAD_SHA -> $PR_HEAD_NOW); a newer run owns the fix."
    exit 0
fi

# Circuit breaker: if the failing head is already one of our autofix commits,
# the sync is not converging - stacking more bot commits would loop. Fail safe
# to the recipe comment.
HEAD_MSG="$(gh api "repos/$BASE_REPO/commits/$HEAD_SHA" --jq '.commit.message' 2>/dev/null || true)"
case "$HEAD_MSG" in
    "$AUTOFIX_SUBJECT"*) NONCONVERGENT=1 ;;
    *) NONCONVERGENT=0 ;;
esac

post_or_update_comment() {
    local body_file="$1"
    # Capture fully before taking the first id: head -1 on a live --paginate
    # stream SIGPIPEs gh under pipefail.
    local ids existing
    ids="$(gh api --paginate "repos/$BASE_REPO/issues/$PR_NUMBER/comments" \
        --jq ".[] | select(.body | startswith(\"$COMMENT_MARKER\")) | .id")"
    existing="$(printf '%s\n' "$ids" | head -1)"
    if [ -n "$existing" ]; then
        gh api --method PATCH "repos/$BASE_REPO/issues/comments/$existing" \
            -F body=@"$body_file" >/dev/null
        echo "Updated autofix comment $existing on PR #$PR_NUMBER."
    else
        gh api --method POST "repos/$BASE_REPO/issues/$PR_NUMBER/comments" \
            -F body=@"$body_file" >/dev/null
        echo "Posted autofix comment on PR #$PR_NUMBER."
    fi
}

comment_fallback() {
    local reason="$1"
    local body
    body="$(mktemp)"
    cat > "$body" <<EOF
$COMMENT_MARKER
**Bazel BUILD files are out of sync on this PR** (${reason}).

Go files, imports or go.mod changed without \`make bazel-sync\`. CI already produced the fix; apply it locally:

\`\`\`bash
gh run download $RUN_ID -R $BASE_REPO -n bazel-sync-patch
git apply --index bazel-sync.patch
git commit -m "build(bazel): sync BUILD files"
git push
\`\`\`

Or regenerate with Bazel installed: \`make bazel-sync\`, then commit the result. Drift outside BUILD.bazel / MODULE.bazel / MODULE.bazel.lock (e.g. third_party/patches) is never in the patch; the failing run's log lists it.

_Automated by the [bazel-autofix workflow]($RUN_URL); this comment is updated in place on each failing run._
EOF
    post_or_update_comment "$body"
    rm -f "$body"
}

if [ "$NONCONVERGENT" = "1" ]; then
    echo "Head $HEAD_SHA is already an autofix commit; refusing to stack another."
    comment_fallback "an earlier auto-fix did not converge - please run make bazel-sync"
    exit 0
fi

# --- Fork PRs: no token we hold can push there, leave the recipe --------------

if [ "$HEAD_REPO" != "$BASE_REPO" ]; then
    comment_fallback "fork PR - CI cannot push the fix to your branch"
    exit 0
fi

# Never push to a long-lived branch, even if a PR happens to use one as head.
case "$HEAD_BRANCH" in
    main | release/* | gh-readonly-queue/*)
        comment_fallback "the head branch $HEAD_BRANCH is protected from bot pushes"
        exit 0
        ;;
esac

# --- Same-repo PRs: push the sync commit --------------------------------------

# Keep the token out of on-disk .git/config: pass the auth header per command.
# Uses PUSH_TOKEN (the optional contents:write PAT), not the API token.
AUTH_CONFIG="http.https://github.com/.extraheader=AUTHORIZATION: basic $(printf 'x-access-token:%s' "$PUSH_TOKEN" | base64 -w0)"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

git -c "$AUTH_CONFIG" clone --quiet --no-checkout --filter=blob:none \
    "https://github.com/${BASE_REPO}.git" "$WORK/repo"
cd "$WORK/repo"
git -c "$AUTH_CONFIG" fetch --quiet origin "$HEAD_BRANCH"
git checkout --quiet "$HEAD_SHA" 2>/dev/null || {
    echo "Head $HEAD_SHA no longer reachable on $BASE_REPO/$HEAD_BRANCH; skipping."
    exit 0
}

if ! git apply --index "$PATCH_FILE" 2>/dev/null; then
    cd - >/dev/null
    comment_fallback "the sync patch no longer applies cleanly to the PR head"
    exit 0
fi

# Belt and braces: what actually got staged must pass the same rules.
# --raw lines: ":SRCMODE DSTMODE SRCSHA DSTSHA STATUS<TAB>PATH".
while IFS=$'\t' read -r meta path; do
    read -r src_mode dst_mode _ <<<"$meta"
    case "$src_mode $dst_mode" in
        ":100644 100644" | ":000000 100644" | ":100644 000000") ;;
        *) path="" ;;
    esac
    if ! path_allowed "$path"; then
        echo "REFUSED: staged change outside the allowlist: $meta $path"
        exit 1
    fi
done < <(git -c core.quotePath=true diff --cached --raw --no-renames)

git -c user.name="github-actions[bot]" \
    -c user.email="41898282+github-actions[bot]@users.noreply.github.com" \
    commit --quiet -m "$AUTOFIX_SUBJECT

Applied from the bazel-sync-patch artifact of $RUN_URL
(\`make bazel-sync\`: gazelle, tools/bazel/go_srcs.py, bazel mod tidy).
Only BUILD.bazel, MODULE.bazel and MODULE.bazel.lock are ever pushed; see
scripts/bazel-autofix-push.sh."

if ! git -c "$AUTH_CONFIG" push --quiet origin "HEAD:refs/heads/$HEAD_BRANCH"; then
    cd - >/dev/null
    comment_fallback "pushing the fix to $HEAD_BRANCH failed (branch protection or a concurrent push)"
    exit 0
fi
NEW_SHA="$(git rev-parse HEAD)"
cd - >/dev/null

echo "Pushed sync commit $NEW_SHA to $BASE_REPO/$HEAD_BRANCH."

BODY="$(mktemp)"
cat > "$BODY" <<EOF
$COMMENT_MARKER
**Pushed \`${NEW_SHA:0:12}\` syncing the Bazel BUILD files** (\`make bazel-sync\` output from the [failing run]($RUN_URL)). Pull before pushing again.
EOF
if [ "$AUTOFIX_TOKEN_KIND" = "default" ]; then
    cat >> "$BODY" <<'EOF'

Note: this commit was pushed with the default workflow token, which does **not** retrigger PR checks - re-run them (or push any commit) to refresh the gate. Configuring the `DOCS_AUTOFIX_TOKEN` repo secret (shared with the docs autofix) removes this step.
EOF
fi
post_or_update_comment "$BODY"
rm -f "$BODY"
