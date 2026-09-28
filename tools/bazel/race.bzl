"""go_test_race_off: an existing go_test built without the race detector.

The docker lane (--config=docker) sets --@rules_go//go/config:race for every
target, because pr.yml's container jobs run `go test -race`, except the
Contract corpus job, which does not. A go_test_variant.sh sh_test over this
rule's output runs such a package the way that job does, without a second
go_test rule (whose srcs gazelle would not maintain). The go_test keeps its own
`race = "auto"` and so follows the setting this transition gives it.
"""

def _race_off_impl(_settings, _attr):
    return {"@rules_go//go/config:race": False}

_race_off = transition(
    implementation = _race_off_impl,
    inputs = [],
    outputs = ["@rules_go//go/config:race"],
)

def _go_test_race_off_impl(ctx):
    target = ctx.attr.test[0]
    src = target[DefaultInfo].files_to_run.executable
    out = ctx.actions.declare_file(ctx.label.name)
    ctx.actions.symlink(output = out, target_file = src, is_executable = True)
    runfiles = ctx.runfiles(files = [out]).merge(target[DefaultInfo].default_runfiles)
    return [DefaultInfo(files = depset([out]), runfiles = runfiles, executable = out)]

_go_test_race_off = rule(
    implementation = _go_test_race_off_impl,
    doc = "The executable of `test`, built with --@rules_go//go/config:race=false.",
    attrs = {
        "test": attr.label(
            cfg = _race_off,
            executable = True,
            mandatory = True,
        ),
    },
    executable = True,
)

def go_test_race_off(name, test, **kwargs):
    """Declares `name`, the executable of go_test `test` built without race."""
    _go_test_race_off(name = name, test = test, testonly = True, **kwargs)
