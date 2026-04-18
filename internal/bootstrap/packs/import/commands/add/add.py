#!/usr/bin/env python3.11
"""gc import add — add a pack to the city's imports.

Usage:
    gc import add <source> [--version <constraint>] [--name <handle>]

The argument shape selects the form:
  - Directory source (/, ., ~ prefix) → write [imports.X] source = "..."
    to city.toml. No fetching, no lock entry, no recursion.
  - Git source → fetch the repo, recurse into its [imports], write the
    full closure to pack.lock, materialize every pack into .gc/cache/packs/,
    record the user's direct intent in the [imports] section of city.toml,
    and mirror the [packs] entries into city.toml as well.

--name lets the user override the default local handle when the default
collides with an existing import.
"""

import argparse
import os
import re
import sys
from pathlib import Path

# Make `lib` importable when invoked as a [[commands]] script
sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from lib import cache, citytoml, lockfile, manifest, resolver, ui  # noqa: E402


def _looks_like_url(s: str) -> bool:
    return bool(re.match(r"^(https?://|git@|ssh://|git://)", s))


def _looks_like_path(s: str) -> bool:
    return s.startswith(("/", ".", "~"))


def _derive_local_handle(source: str) -> str:
    """Derive a default local handle from a source string."""
    if _looks_like_url(source):
        # Last path segment, .git stripped
        last = source.rstrip("/").split("/")[-1]
        if last.endswith(".git"):
            last = last[:-4]
        return last
    else:
        # Last directory name
        return Path(source).expanduser().resolve().name


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(prog="gc import add", description="Add a pack to the city's imports.")
    parser.add_argument("target", help="A source string for a pack import")
    parser.add_argument("--version", help="Semver constraint (e.g. ^1.2). Default: ^<major>.<minor> of latest tag.")
    parser.add_argument("--name", help="Override the local handle (default: derived from source)")
    args = parser.parse_args(argv)

    city_root = ui.find_city_root()
    city_toml_path = city_root / "city.toml"
    handle = args.name or _derive_local_handle(args.target)

    # Distinguish directory source vs git source.
    is_path = _looks_like_path(args.target)

    # Read existing manifest from city.toml [imports]
    m = manifest.read(city_toml_path)

    if handle in m.imports:
        ui.die(
            f"import {handle!r} already exists in [imports] in city.toml.\n"
            f"  Remove it first with `gc import remove {handle}`,\n"
            f"  or retry with --name <alias> to register this one under a different local handle."
        )

    if is_path:
        # Path import — no fetching, no lock entry
        spec = manifest.ImportSpec(handle=handle, source=args.target)
        m.imports[handle] = spec
        manifest.write(m, city_toml_path)
        ui.info(f"Added [imports.{handle}] source = \"{args.target}\" to city.toml")
        return 0

    # Git import — full resolution and materialization.
    spec = manifest.ImportSpec(handle=handle, source=args.target, version=args.version)
    m.imports[handle] = spec

    ui.info(f"Resolving {args.target}...")

    # Splice in the implicit-imports list so the resolver sees both
    # the city's direct imports and the implicit baseline (e.g.
    # maintenance). Honors implicit_imports = false in city.toml.
    # Build a temporary manifest for resolution; the original 'm' is
    # what gets written back to city.toml so the user sees only their
    # direct imports there.
    from lib import implicit as implicit_lib
    opt_out = implicit_lib.read_opt_out_flag(city_toml_path)
    if opt_out:
        implicit_imports = {}
    else:
        implicit_lib.ensure_default_file()
        implicit_imports = implicit_lib.read_implicit_imports()

    spliced = manifest.Manifest()
    for h, s in implicit_imports.items():
        spliced.imports[h] = s
    for h, s in m.imports.items():
        spliced.imports[h] = s  # city wins on collision
    implicit_handles = {h for h in implicit_imports if h not in m.imports}

    direct = resolver.pending_from_manifest(spliced)
    accelerator = cache.user_accelerator_root()
    accelerator.mkdir(parents=True, exist_ok=True)

    try:
        closure = resolver.resolve(direct, accelerator)
    except resolver.ResolveError as e:
        ui.die(str(e))

    # If the user didn't specify --version, the resolver picked a default
    # constraint based on the latest tag (e.g. ^1.4 for v1.4.0). Persist
    # that default back into the manifest spec so it shows up in city.toml.
    if not args.version and handle in closure:
        spec.version = closure[handle].constraint

    # Materialize every pack in the closure into the city cache
    pack_cache_root = cache.city_pack_cache(city_root)
    pack_cache_root.mkdir(parents=True, exist_ok=True)
    for h, rp in closure.items():
        target = pack_cache_root / h
        from lib import git as gitlib
        gitlib.materialize(rp.accelerator_path, target, subpath=rp.subpath)
        marker = "(transitive)" if rp.parent else ""
        ui.step(f"Materialized {h} v{rp.version} {marker}", indent=1)

    # Write pack.lock
    lock_path = city_root / "pack.lock"
    lf = lockfile.read(lock_path)
    reachable = set(closure.keys())
    # Garbage-collect old entries that are no longer reachable
    for h in list(lf.packs.keys()):
        if h not in reachable:
            del lf.packs[h]
    # Update lock from the closure
    for h, rp in closure.items():
        target = pack_cache_root / h
        content_hash = cache.hash_directory(target)
        # Mark implicit-origin entries with parent = "(implicit)" so
        # gc import list and gc import remove can recognize them.
        # A handle that's in the implicit set AND has no transitive
        # parent is a top-level implicit import.
        parent = rp.parent
        if parent is None and h in implicit_handles:
            parent = "(implicit)"
        lf.packs[h] = lockfile.LockedPack(
            handle=h,
            url=rp.url,
            version=str(rp.version),
            constraint=rp.constraint,
            commit=rp.commit,
            hash=content_hash,
            parent=parent,
            subpath=rp.subpath,
        )
    lockfile.write(lf, lock_path)

    # Mirror into city.toml: [packs.X] entries + [workspace].includes
    city_data = citytoml.read(city_root / "city.toml")
    existing_includes = citytoml.get_includes(city_data)

    # Build the new includes list: preserve any user entries that aren't
    # pack handles managed by us, then add a handle for every pack in
    # the closure.
    managed_handles = set(closure.keys())
    user_includes = [e for e in existing_includes if e not in managed_handles]
    new_includes = list(user_includes)
    for h in sorted(closure.keys()):
        if h not in new_includes:
            new_includes.append(h)

    from lib import git as gitlib
    new_packs = {}
    for h, rp in closure.items():
        # The v1 loader's PackSource expects a bare repo source in `source`,
        # the git tag in `ref`, and an optional subpath in `path`.
        repo_url, _ = gitlib.split_url_and_subpath(rp.url)
        ref = f"v{rp.version}" if rp.subpath == "" else f"{rp.subpath}/v{rp.version}"
        new_packs[h] = citytoml.PacksBlock(
            name=h,
            source=repo_url,
            ref=ref,
            path=rp.subpath,
        )

    # Anything previously in city.toml that's no longer in the closure
    # gets removed
    existing_packs = citytoml.get_packs(city_data)
    to_remove = set(existing_packs.keys()) - set(new_packs.keys())

    citytoml.update_includes_and_packs(
        city_root / "city.toml",
        new_includes=new_includes,
        new_packs=new_packs,
        removed_packs=to_remove,
    )

    # Write back the updated manifest into city.toml [imports]
    manifest.write(m, city_toml_path)

    ui.info(f"Added [imports.{handle}] to city.toml")
    ui.info(f"Updated city.toml ([imports]: {len(m.imports)}, [packs]: {len(new_packs)}, includes: {len(new_includes)})")
    ui.info(f"Updated pack.lock ({len(lf.packs)} entries)")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
