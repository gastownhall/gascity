"""Minimal semver parsing and constraint matching for gc import.

Supports:
  - Parsing tags like "v1.2.3", "1.2.3", "v1.2.3-rc.1"
  - Constraints: "^1.2", "~1.2.3", ">=1.0,<2.0", "1.2.3" (exact)
  - Picking the highest matching version from a list

Pre-releases (e.g. "1.2.3-rc.1") are excluded from constraint matches
unless the constraint itself is a pre-release.
"""

import re
from dataclasses import dataclass, field
from typing import Optional


_TAG_RE = re.compile(r"^v?(\d+)\.(\d+)(?:\.(\d+))?(?:-([0-9A-Za-z.-]+))?(?:\+([0-9A-Za-z.-]+))?$")


@dataclass
class Version:
    major: int
    minor: int
    patch: int
    pre: tuple = field(default=())  # Comparable pre-release identifiers
    raw: str = field(default="", compare=False)

    def __str__(self) -> str:
        s = f"{self.major}.{self.minor}.{self.patch}"
        if self.pre:
            s += "-" + ".".join(str(p) for p in self.pre)
        return s

    @property
    def is_prerelease(self) -> bool:
        return bool(self.pre)

    @classmethod
    def parse(cls, s: str) -> Optional["Version"]:
        m = _TAG_RE.match(s.strip())
        if not m:
            return None
        major = int(m.group(1))
        minor = int(m.group(2))
        patch = int(m.group(3)) if m.group(3) else 0
        pre_str = m.group(4) or ""
        pre = ()
        if pre_str:
            parts = []
            for p in pre_str.split("."):
                try:
                    parts.append((0, int(p)))  # numeric ids sort before alpha
                except ValueError:
                    parts.append((1, p))
            pre = tuple(parts)
        return cls(major, minor, patch, pre, raw=s)

    def __lt__(self, other: "Version") -> bool:
        # Per semver: a version without pre-release is greater than one with
        if (self.major, self.minor, self.patch) != (other.major, other.minor, other.patch):
            return (self.major, self.minor, self.patch) < (other.major, other.minor, other.patch)
        if self.pre and not other.pre:
            return True
        if not self.pre and other.pre:
            return False
        return self.pre < other.pre


# A constraint is a list of (op, version) pairs that must all be satisfied.
@dataclass
class Constraint:
    raw: str
    clauses: list  # list of (op, Version) where op in {"=", ">=", ">", "<=", "<"}

    def matches(self, v: Version) -> bool:
        # Pre-releases excluded unless the constraint itself is on the same
        # major.minor.patch with a pre-release component.
        if v.is_prerelease:
            allowed = any(
                cv.is_prerelease
                and (cv.major, cv.minor, cv.patch) == (v.major, v.minor, v.patch)
                for _, cv in self.clauses
            )
            if not allowed:
                return False
        for op, cv in self.clauses:
            if op == "=" and not (v.major == cv.major and v.minor == cv.minor and v.patch == cv.patch and v.pre == cv.pre):
                return False
            if op == ">=" and v < cv:
                return False
            if op == ">" and not (cv < v):
                return False
            if op == "<=" and cv < v:
                return False
            if op == "<" and not (v < cv):
                return False
        return True


def parse_constraint(s: str) -> Constraint:
    s = s.strip()
    if not s:
        raise ValueError("empty constraint")

    # ^X.Y or ^X.Y.Z → >=X.Y.Z, <(X+1).0.0   (or <(0.X+1).0 if X==0 — caret special)
    if s.startswith("^"):
        v = Version.parse(s[1:])
        if v is None:
            raise ValueError(f"invalid version in caret constraint: {s}")
        if v.major > 0:
            upper = Version(v.major + 1, 0, 0)
        elif v.minor > 0:
            upper = Version(0, v.minor + 1, 0)
        else:
            upper = Version(0, 0, v.patch + 1)
        return Constraint(s, [(">=", v), ("<", upper)])

    # ~X.Y.Z → >=X.Y.Z, <X.(Y+1).0
    if s.startswith("~"):
        v = Version.parse(s[1:])
        if v is None:
            raise ValueError(f"invalid version in tilde constraint: {s}")
        upper = Version(v.major, v.minor + 1, 0)
        return Constraint(s, [(">=", v), ("<", upper)])

    # Comma-separated list of comparators
    clauses = []
    for part in s.split(","):
        part = part.strip()
        op = "="
        rest = part
        for o in (">=", "<=", ">", "<", "="):
            if part.startswith(o):
                op = o
                rest = part[len(o):].strip()
                break
        v = Version.parse(rest)
        if v is None:
            raise ValueError(f"invalid version in constraint: {part!r}")
        clauses.append((op, v))
    return Constraint(s, clauses)


def parse_tag(tag: str) -> Optional[Version]:
    """Parse a git tag string into a Version, or None if it's not semver."""
    return Version.parse(tag)


def pick_highest(versions: list[Version], constraint: Constraint) -> Optional[Version]:
    """Pick the highest version from `versions` that satisfies `constraint`."""
    matching = [v for v in versions if constraint.matches(v)]
    if not matching:
        return None
    return max(matching)


def default_constraint_for(v: Version) -> Constraint:
    """Default constraint for `gc import add` without --version: ^<major>.<minor>."""
    return parse_constraint(f"^{v.major}.{v.minor}")


def same_major(a: Version, b: Version) -> bool:
    if a.major == 0 or b.major == 0:
        # 0.x.y is special: each minor is a major
        return a.major == b.major and a.minor == b.minor
    return a.major == b.major
