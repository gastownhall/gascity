"""Differential and crafted-index checks for the MCP mail GitPython cache."""

from io import BytesIO
import os
import random
import struct
import sys

import git.index.fun as index_fun
import sitecustomize


def outcome(function, *args):
    try:
        function(*args)
        return ("ok",)
    except Exception as exc:  # Compare public behavior, including error text.
        return (type(exc).__name__, str(exc))


def index_bytes(path, mode):
    encoded = path.encode("utf-8")
    if len(encoded) >= 4095:
        raise ValueError("test path is too long")
    entry = struct.pack(
        ">8s8sLLLLLL20sH",
        b"\0" * 8,
        b"\0" * 8,
        0,
        0,
        mode,
        0,
        0,
        0,
        b"\0" * 20,
        len(encoded),
    )
    entry += encoded + b"\0"
    entry += b"\0" * ((-len(entry)) % 8)
    return b"DIRC" + struct.pack(">LL", 2, 1) + entry + b"\0" * 20


corpus = [
    "",
    ".",
    "..",
    "/absolute",
    "a//b",
    "a/../b",
    "a/.git/config",
    ".GIT",
    "git~1/foo",
    "foo/.git:stream",
    "foo/.git. ",
    "foo/.gitmodules",
    "foo/gitmod~1",
    "foo/gi7eba~1",
    "foo/.gitmodules:stream",
    "foo/.gitmodules. ",
    "foo/.g\u200cit",
    "foo/.g\u200citmodules",
    "foo\\.git\\config",
    "foo\0bar",
    "normal/file",
    "space name/new\nline",
    "x" * 257,
]
random.seed(281013)
alphabet = "abcdefGIT.~0123/\\ :\u200c\u200d\ufeff"
corpus.extend(
    "".join(random.choices(alphabet, k=random.randrange(1, 90)))
    for _ in range(10_000)
)

checks = 0
for platform in ("posix", "nt"):
    saved = os.name
    os.name = platform
    try:
        for path in corpus:
            for mode in (None, 0o100644, 0o120000):
                expected = outcome(sitecustomize._ORIGINAL, path, mode)
                assert outcome(sitecustomize.validate_repo_path, path, mode) == expected, (
                    platform,
                    path,
                    mode,
                    "first",
                )
                assert outcome(sitecustomize.validate_repo_path, path, mode) == expected, (
                    platform,
                    path,
                    mode,
                    "repeat",
                )
                checks += 2
    finally:
        os.name = saved

# Mode and platform change validation results and therefore remain cache keys.
assert outcome(sitecustomize.validate_repo_path, "foo/.gitmodules", 0o100644) == ("ok",)
assert outcome(sitecustomize.validate_repo_path, "foo/.gitmodules", 0o120000)[0] == "ValueError"
saved = os.name
try:
    os.name = "posix"
    assert outcome(sitecustomize.validate_repo_path, "foo\\bar") == ("ok",)
    os.name = "nt"
    assert outcome(sitecustomize.validate_repo_path, "foo\\bar")[0] == "ValueError"
finally:
    os.name = saved

# All call sites loaded at startup must reference the wrapper.
for name in sitecustomize._PATCHED_MODULES:
    module = sys.modules[name]
    assert module._validate_repo_path is sitecustomize.validate_repo_path, name

# read_cache must agree with the original validator on crafted index entries.
index_checks = 0
for path in corpus[:23]:
    for mode in (0o100644, 0o120000):
        raw = index_bytes(path, mode)
        index_fun._validate_repo_path = sitecustomize._ORIGINAL
        expected = outcome(index_fun.read_cache, BytesIO(raw))
        index_fun._validate_repo_path = sitecustomize.validate_repo_path
        actual = outcome(index_fun.read_cache, BytesIO(raw))
        repeated = outcome(index_fun.read_cache, BytesIO(raw))
        assert expected == actual == repeated, (path, mode, expected, actual, repeated)
        index_checks += 2

# Failed checks are not memoized; repeated safe checks are.
before = sitecustomize.cache_info()
for _ in range(2):
    assert outcome(sitecustomize.validate_repo_path, "../invalid")[0] == "ValueError"
after_invalid = sitecustomize.cache_info()
assert after_invalid.misses >= before.misses + 2
before_hit = sitecustomize.cache_info()
sitecustomize.validate_repo_path("cache-hit/path", 0o100644)
sitecustomize.validate_repo_path("cache-hit/path", 0o100644)
after_hit = sitecustomize.cache_info()
assert after_hit.hits > before_hit.hits

print(
    "GITPYTHON_PATH_CACHE PASS "
    f"version={sitecustomize._EXPECTED_GITPYTHON} direct_checks={checks} "
    f"index_read_checks={index_checks} cache_info={sitecustomize.cache_info()}",
    flush=True,
)
