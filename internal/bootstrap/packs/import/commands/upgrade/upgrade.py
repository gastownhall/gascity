#!/usr/bin/env python3.11
"""gc import upgrade — re-resolve constraints and bump pack.lock.

Usage:
    gc import upgrade            # upgrade everything
    gc import upgrade <name>     # upgrade just one pack and its transitive deps

The constraint in [imports] in city.toml is NOT modified — only the resolved
version in pack.lock. Bumping a constraint requires editing city.toml by hand.

Frozen packs are skipped with a warning.
"""

import argparse
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from lib import cache, citytoml, lockfile, manifest, resolver, ui  # noqa: E402


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(prog="gc import upgrade")
    parser.add_argument("name", nargs="?", help="The local handle to upgrade (default: all)")
    args = parser.parse_args(argv)

    city_root = ui.find_city_root()
    city_toml_path = city_root / "city.toml"
    m = manifest.read(city_toml_path)

    # Splice in the implicit-imports list (e.g. maintenance) so the
    # resolver upgrades them too. Honors implicit_imports = false.
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
        spliced.imports[h] = s
    implicit_handles = {h for h in implicit_imports if h not in m.imports}

    if not spliced.imports:
        ui.info("No imports to upgrade.")
        return 0

    # If a specific name is requested, validate. Allow upgrading
    # implicit handles by name as well as city imports.
    if args.name and args.name not in spliced.imports:
        ui.die(f"no import named {args.name!r} in [imports] in city.toml or in the implicit list")

    lf = lockfile.read(city_root / "pack.lock")
    accelerator = cache.user_accelerator_root()
    accelerator.mkdir(parents=True, exist_ok=True)
    pack_cache_root = cache.city_pack_cache(city_root)
    pack_cache_root.mkdir(parents=True, exist_ok=True)

    if args.name:
        ui.info(f"Upgrading {args.name}...")
    else:
        ui.info(f"Upgrading all imports...")

    direct = resolver.pending_from_manifest(spliced)
    try:
        new_closure = resolver.resolve(direct, accelerator)
    except resolver.ResolveError as e:
        ui.die(str(e))

    from lib import git as gitlib

    bumped = []
    for h, rp in new_closure.items():
        if args.name and h != args.name and not _is_descendant_of(h, args.name, new_closure):
            # Limited upgrade: only the named pack and its transitive descendants
            # Keep the existing entry
            continue
        existing = lf.get(h)
        if existing and existing.version == str(rp.version) and existing.commit == rp.commit:
            continue  # no change
        # Materialize and update lock
        target = pack_cache_root / h
        gitlib.materialize(rp.accelerator_path, target, subpath=rp.subpath)
        content_hash = cache.hash_directory(target)
        # Mark implicit-origin entries with parent = "(implicit)".
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
        if existing:
            ui.step(f"{h}: {existing.version} → {rp.version}", indent=1)
        else:
            ui.step(f"{h}: new at {rp.version}", indent=1)
        bumped.append(h)

    if not bumped:
        ui.info("Everything is already at the latest version permitted by constraints.")
        return 0

    lockfile.write(lf, city_root / "pack.lock")

    # Mirror updated [packs.X] refs into city.toml
    new_packs = {}
    for h in bumped:
        rp = new_closure[h]
        repo_url, _ = gitlib.split_url_and_subpath(rp.url)
        ref = f"v{rp.version}" if rp.subpath == "" else f"{rp.subpath}/v{rp.version}"
        new_packs[h] = citytoml.PacksBlock(
            name=h,
            source=repo_url,
            ref=ref,
            path=rp.subpath,
        )
    if new_packs:
        # Don't change includes — they don't need updating on upgrade
        existing_includes = citytoml.get_includes(citytoml.read(city_root / "city.toml"))
        citytoml.update_includes_and_packs(
            city_root / "city.toml",
            new_includes=existing_includes,
            new_packs=new_packs,
            removed_packs=set(),
        )
    ui.info(f"Updated city.toml [packs] entries and pack.lock")
    return 0


def _is_descendant_of(handle: str, ancestor: str, closure: dict) -> bool:
    """Walk up the parent chain from `handle` to see if `ancestor` is reached."""
    cur = closure.get(handle)
    while cur is not None and cur.parent:
        if cur.parent == ancestor:
            return True
        cur = closure.get(cur.parent)
    return False


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
