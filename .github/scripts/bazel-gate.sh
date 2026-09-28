#!/usr/bin/env bash
# pr.yml's ci-gate view of its bazel.yml call (the `bazel` job). bazel.yml's
# rbe job decides the execution mode once and exports it as the call's
# rbe-mode / rbe-enabled outputs; this script reads those outputs and never
# re-derives the decision (repo variable, fork, secrets) itself.
#
#   bazel-gate.sh skips      the Bazel gate ids whose job is skipped by design
#                            in this mode, for CI_GATE_SKIPPED_OK:
#                              skip:   every lane and the aggregate (BAZEL)
#                              local:  the remote-only BAZEL_EMBEDDED
#                              remote: none
#   bazel-gate.sh aggregate  the value to gate on for BAZEL, the call's
#                            aggregate result: the result itself, except
#                            that a failure is reported as success when the
#                            advisory bazel-integration lane (not gated, see
#                            bazel.yml) ran and failed or was cancelled; the
#                            gated lanes still fail the gate through their
#                            own ids. An invalid or missing mode (the rbe job
#                            failed, the call never started) gives an
#                            unexpected value, which ci-gate.sh rejects.
#
# Any other skip, a failure, a cancellation, or a lane that should run but
# reported nothing (pr.yml maps an empty output to skipped) fails the gate.
#
# Inputs (environment): BAZEL_RBE_MODE, BAZEL_RBE_ENABLED (the call's
# rbe-mode / rbe-enabled outputs), BAZEL_CALL (needs.bazel.result),
# BAZEL_INTEGRATION (the call's bazel-integration output).

set -euo pipefail

mode="${BAZEL_RBE_MODE:-}"
enabled="${BAZEL_RBE_ENABLED:-}"
valid=false
case "$mode/$enabled" in
    remote/true | local/false | skip/false) valid=true ;;
esac

case "${1:-}" in
    skips)
        skips=()
        if [[ "$valid" == true ]]; then
            case "$mode" in
                skip) skips+=(BAZEL BAZEL_TEST BAZEL_PURE BAZEL_EMBEDDED BAZEL_DOLTSERVER) ;;
                local) skips+=(BAZEL_EMBEDDED) ;;
            esac
        fi
        echo "${skips[*]-}"
        ;;
    aggregate)
        call="${BAZEL_CALL:-}"
        if [[ "$valid" != true ]]; then
            echo "invalid-rbe-mode:$(printf '%s/%s' "$mode" "$enabled" | tr -cd 'a-z/')"
        elif [[ "$call" == failure && "$mode" == remote &&
            ("${BAZEL_INTEGRATION:-}" == failure || "${BAZEL_INTEGRATION:-}" == cancelled) ]]; then
            echo "note: BAZEL=failure is the advisory bazel-integration lane's (${BAZEL_INTEGRATION}); the gated lanes are checked on their own" >&2
            echo success
        else
            echo "$call"
        fi
        ;;
    *)
        echo "usage: $0 skips|aggregate" >&2
        exit 2
        ;;
esac
