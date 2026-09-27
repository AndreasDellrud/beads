#!/usr/bin/env bash
# Runs an existing go_test binary as another test target, so a package can be
# tested a second way (different env, tags, or lane) without compiling it
# again. The first argument is the binary's runfiles path ($(rootpath ...),
# relative to the workspace runfiles directory, which is the test's working
# directory); the rest, plus any --test_arg, go to the binary unchanged.
# Sharding, XML output and timeouts pass through: the go_test binary reads
# the same TEST_* environment it would as its own target.
set -euo pipefail
bin="$1"
shift
exec "./$bin" "$@"
