"""go_darwin_check_test: compile and nogo-check Go targets for darwin on a Linux host.

A Go file that build constraints keep out of a Linux build (_darwin.go,
//go:build !linux) is never compiled or linted by the Linux lane, so a change
can be green there and break the macOS tier. This rule builds its deps, which
are go_library, go_binary and go_test targets, for a darwin target platform
in rules_go's pure mode (no cgo, as the release build), without linking: a
dep's nogo `_validation` outputs need its compiled package, so the compile
and every nogo analyzer run on the darwin file set, and a type error or an
analyzer finding fails the build of this test with the file and line.

The test itself only exits 0; everything it checks happens while its inputs
are built. It runs nothing darwin: runtime differences stay with the Mac tier.
"""

_PLATFORMS = "//command_line_option:platforms"
_PURE = "@rules_go//go/config:pure"
_COVERAGE = "//command_line_option:collect_code_coverage"

def _darwin_pure_impl(_settings, _attr):
    return {
        _PLATFORMS: [str(Label("@rules_go//go/toolchain:darwin_arm64"))],
        _PURE: True,
        # The nightly `bazel coverage //...` would otherwise compile and
        # analyze the whole darwin graph a second time, instrumented.
        _COVERAGE: False,
    }

_darwin_pure = transition(
    implementation = _darwin_pure_impl,
    inputs = [],
    outputs = [_PLATFORMS, _PURE, _COVERAGE],
)

def _go_darwin_check_test_impl(ctx):
    validations = []
    for dep in ctx.attr.deps:
        # go_library, go_binary and go_test attach the nogo validation of
        # every compile they own to this output group; a dep without it
        # (go_test_run, a filegroup) would be checked by nothing.
        group = getattr(dep[OutputGroupInfo], "_validation", None)
        if group == None or not group.to_list():
            fail("%s contributes no nogo validation output, so nothing would check it: only go_library, go_binary and go_test targets belong here, with nogo enabled" % dep.label)
        validations.append(group)
    outputs = depset(transitive = validations)

    script = ctx.actions.declare_file(ctx.label.name + ".sh")
    ctx.actions.write(script, "#!/bin/sh\nexit 0\n", is_executable = True)
    return [
        DefaultInfo(
            executable = script,
            runfiles = ctx.runfiles(transitive_files = outputs),
        ),
        OutputGroupInfo(_validation = outputs),
    ]

go_darwin_check_test = rule(
    implementation = _go_darwin_check_test_impl,
    test = True,
    doc = "Compiles and nogo-checks Go targets for darwin_arm64 (pure mode) without linking.",
    attrs = {
        "deps": attr.label_list(
            mandatory = True,
            cfg = _darwin_pure,
            doc = "The go_library, go_binary and go_test targets to check.",
        ),
    },
)
