genrule(
    name = "greet",
    outs = ["greet.out"],
    cmd = "echo 'MICROVM_S2_ACTION kernel='$$(uname -r)' marker=hello-from-s2' > $@",
)

# Distinct target/digest from :greet, used only for the go/no-go #1 AC-write
# isolation check: must never have been submitted to the oss instance before
# (so its AC entry, if any, can only have come from whichever instance ran
# it first).
genrule(
    name = "fork_check",
    outs = ["fork_check.out"],
    cmd = "echo 'MICROVM_S2_ACTION kernel='$$(uname -r)' marker=fork-isolation-check' > $@",
)
