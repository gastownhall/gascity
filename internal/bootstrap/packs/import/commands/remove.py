#!/usr/bin/env python3.11
"""gc import remove — drop a pack from the city's imports.

Usage:
    gc import remove <name>

- Removes [imports.<name>] from city.toml.
- Garbage-collects transitive deps that are no longer needed.
- Removes the corresponding [packs.X] blocks from city.toml and entries
  from [workspace].includes.
- Prunes the city pack cache for everything that was removed.
- Refuses to remove implicit-list handles (set implicit_imports = false
  in city.toml to disable implicit imports for this city).
"""

import argparse
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from lib import cache, citytoml, lockfile, manifest, ui  # noqa: E402


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(prog="gc import remove")
    parser.add_argument("name", help="The local handle of the pack to remove")
    args = parser.parse_args(argv)

    city_root = ui.find_city_root()
    city_toml_path = city_root / "city.toml"
    handle = args.name

    m = manifest.read(city_toml_path)

    # Refuse to remove implicit-list handles. They aren't in [imports]
    # to drop, and the user-facing way to disable them is the per-city
    # implicit_imports = false flag, not gc import remove.
    from lib import implicit as implicit_lib
    if implicit_lib.is_implicit_handle(handle) and handle not in m.imports:
        ui.die(
            f"{handle!r} is an implicit import (every city gets it automatically). "
            f"It's not in city.toml's [imports] to remove. To disable implicit imports "
            f"in this city, set `implicit_imports = false` at the top of city.toml."
        )

    if handle not in m.imports:
        ui.die(f"no import named {handle!r} in [imports] in city.toml")

    spec = m.imports[handle]
    if spec.is_path():
        # Path imports just need to be dropped from the manifest — no lock entry, no cache
        del m.imports[handle]
        manifest.write(m, city_toml_path)
        ui.info(f"Removed [imports.{handle}] (local source) from city.toml")
        return 0

    # Git import — proceed
    lock_path = city_root / "pack.lock"
    lf = lockfile.read(lock_path)

    # Drop from manifest
    del m.imports[handle]

    # Splice in the implicit-imports list so the resolver still sees
    # them and they don't get GC'd from the lock when an unrelated
    # city import is removed.
    opt_out = implicit_lib.read_opt_out_flag(city_toml_path)
    if opt_out:
        implicit_imports_dict = {}
    else:
        implicit_lib.ensure_default_file()
        implicit_imports_dict = implicit_lib.read_implicit_imports()

    spliced = manifest.Manifest()
    for h, s in implicit_imports_dict.items():
        spliced.imports[h] = s
    for h, s in m.imports.items():
        spliced.imports[h] = s
    implicit_handles = {h for h in implicit_imports_dict if h not in m.imports}

    # Compute the new closure (everything reachable from remaining imports
    # plus the implicit list) — anything in the lock that's no longer
    # reachable is GC'd
    from lib import resolver
    direct = resolver.pending_from_manifest(spliced)
    accelerator = cache.user_accelerator_root()
    accelerator.mkdir(parents=True, exist_ok=True)
    try:
        new_closure = resolver.resolve(direct, accelerator)
    except resolver.ResolveError as e:
        ui.die(f"resolver error after removal: {e}")

    # Decide what to remove from the lock and from city.toml
    keep_handles = set(new_closure.keys())
    to_remove = set(lf.packs.keys()) - keep_handles
    to_remove.add(handle)  # explicitly remove the requested handle even if it stayed reachable somehow

    removed_from_lock = []
    for h in to_remove:
        if h in lf.packs:
            del lf.packs[h]
            cache.remove_pack_from_cache(city_root, h)
            removed_from_lock.append(h)

    # Update lock entries with the new closure (in case constraints/versions changed)
    for h, rp in new_closure.items():
        target = cache.city_pack_cache(city_root) / h
        if not target.exists():
            from lib import git as gitlib
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
    lockfile.write(lf, lock_path)

    # Update city.toml
    from lib import git as gitlib
    city_data = citytoml.read(city_root / "city.toml")
    existing_includes = citytoml.get_includes(city_data)
    managed_handles = set(new_closure.keys())
    user_includes = [
        e for e in existing_includes
        if e not in managed_handles and e not in to_remove
    ]
    new_includes = list(user_includes)
    for h in sorted(new_closure.keys()):
        if h not in new_includes:
            new_includes.append(h)

    new_packs = {}
    for h, rp in new_closure.items():
        repo_url, _ = gitlib.split_url_and_subpath(rp.url)
        ref = f"v{rp.version}" if rp.subpath == "" else f"{rp.subpath}/v{rp.version}"
        new_packs[h] = citytoml.PacksBlock(
            name=h,
            source=repo_url,
            ref=ref,
            path=rp.subpath,
        )

    citytoml.update_includes_and_packs(
        city_root / "city.toml",
        new_includes=new_includes,
        new_packs=new_packs,
        removed_packs=to_remove,
    )

    manifest.write(m, city_toml_path)

    ui.info(f"Removed [imports.{handle}] from city.toml")
    if len(removed_from_lock) > 1:
        gc = sorted(set(removed_from_lock) - {handle})
        ui.info(f"Garbage-collected transitive deps: {', '.join(gc)}")
    ui.info(f"Updated city.toml, pack.lock")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
