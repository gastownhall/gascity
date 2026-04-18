#!/usr/bin/env python3.11
"""gc import install — restore the city to the exact state in pack.lock.

Usage:
    gc import install

In the common case, reads pack.lock and fetches every entry from its
recorded source at the recorded commit, materializes each pack into
.gc/cache/packs/, and verifies the content hash.

If pack.lock doesn't exist, OR is missing entries that the resolver
would compute (e.g. the user just ran `gc init` and never invoked
`gc import add`, so pack.lock has no entries but the implicit list
has the maintenance pack), this command does a full resolve+lock as
a self-healing first-run flow. This is the load-bearing trigger that
makes implicit imports work for users who never type `gc import add`.

In all cases, when there's nothing missing, install verifies hashes
and does NOT modify city.toml or pack.lock — it's a pure restore.
"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from lib import cache, citytoml, git as gitlib, implicit, lockfile, manifest, resolver, ui  # noqa: E402


def main(argv: list[str]) -> int:
    if argv:
        ui.die("gc import install takes no arguments")

    city_root = ui.find_city_root()
    city_toml_path = city_root / "city.toml"
    lock_path = city_root / "pack.lock"

    # Compute what the lock SHOULD contain by reading the city's imports
    # and splicing in the implicit list.
    m = manifest.read(city_toml_path)
    opt_out = implicit.read_opt_out_flag(city_toml_path)
    if opt_out:
        implicit_imports = {}
    else:
        implicit.ensure_default_file()
        implicit_imports = implicit.read_implicit_imports()

    # Build the spliced manifest (what the resolver should see).
    spliced = manifest.Manifest()
    for h, s in implicit_imports.items():
        spliced.imports[h] = s
    for h, s in m.imports.items():
        spliced.imports[h] = s
    implicit_handles = {h for h in implicit_imports if h not in m.imports}

    # If the spliced set is empty, there's truly nothing to install.
    if not spliced.imports:
        ui.info("Nothing to install — no imports declared in city.toml and the implicit list is empty.")
        return 0

    # Read the existing lock (if any). Decide whether it's "complete"
    # for the spliced set.
    lf = lockfile.read(lock_path)

    # Heuristic for completeness: every direct entry in the spliced
    # manifest should appear in the lock with a matching source. (We
    # can't check the full transitive closure without resolving;
    # that's fine — partial completeness is enough to short-circuit
    # the common case.)
    needs_resolve = False
    for handle, spec in spliced.imports.items():
        if spec.is_path():
            continue  # directory-backed imports don't have lock entries
        locked = lf.packs.get(handle)
        if locked is None:
            needs_resolve = True
            break
        if locked.url != spec.source:
            needs_resolve = True
            break

    if needs_resolve:
        # Self-healing first-run / drift-recovery path: do a full
        # resolve and rewrite the lock and the city.toml mirror.
        ui.info("Lock file is missing entries; doing a full resolve...")
        return _full_install(
            city_root, city_toml_path, m, spliced, implicit_handles
        )

    # Common case: pure restore from the lock. The lock is the truth.
    accelerator = cache.user_accelerator_root()
    accelerator.mkdir(parents=True, exist_ok=True)
    pack_cache_root = cache.city_pack_cache(city_root)
    pack_cache_root.mkdir(parents=True, exist_ok=True)

    ui.info(f"Installing from pack.lock ({len(lf.packs)} entries)...")
    failed = []
    for handle in sorted(lf.packs.keys()):
        p = lf.packs[handle]

        # Fetch into the accelerator (no-op if already present)
        repo_url, _ = gitlib.split_url_and_subpath(p.url)
        try:
            accel_path = gitlib.fetch_to_accelerator(repo_url, p.commit, accelerator)
        except gitlib.GitError as e:
            ui.warn(f"failed to fetch {handle}: {e}")
            failed.append(handle)
            continue

        # Materialize into the city cache
        target = pack_cache_root / handle
        gitlib.materialize(accel_path, target, subpath=p.subpath)

        # Verify hash
        actual = cache.hash_directory(target)
        if p.hash and actual != p.hash:
            ui.warn(f"hash mismatch for {handle}: lock says {p.hash}, got {actual}")
            failed.append(handle)
            continue

        marker = ""
        if p.parent == "(implicit)":
            marker = " (implicit)"
        elif p.parent:
            marker = f" (transitive: {p.parent})"
        ui.step(f"{handle} v{p.version} ✓{marker}", indent=1)

    if failed:
        ui.die(f"install failed for: {', '.join(failed)}", code=2)
    return 0


def _full_install(city_root, city_toml_path, m, spliced, implicit_handles) -> int:
    """First-run / drift-recovery path: do a full resolve, write the lock,
    materialize, and mirror into city.toml.

    This is structurally the same as the tail end of `gc import add`,
    just without adding a new direct import.
    """
    accelerator = cache.user_accelerator_root()
    accelerator.mkdir(parents=True, exist_ok=True)

    direct = resolver.pending_from_manifest(spliced)
    try:
        closure = resolver.resolve(direct, accelerator)
    except resolver.ResolveError as e:
        ui.die(str(e))

    pack_cache_root = cache.city_pack_cache(city_root)
    pack_cache_root.mkdir(parents=True, exist_ok=True)

    for h, rp in closure.items():
        target = pack_cache_root / h
        gitlib.materialize(rp.accelerator_path, target, subpath=rp.subpath)
        if h in implicit_handles:
            marker = "(implicit)"
        elif rp.parent:
            marker = f"(transitive: {rp.parent})"
        else:
            marker = ""
        ui.step(f"Materialized {h} v{rp.version} {marker}", indent=1)

    # Write the lock
    lock_path = city_root / "pack.lock"
    lf = lockfile.read(lock_path)
    reachable = set(closure.keys())
    for h in list(lf.packs.keys()):
        if h not in reachable:
            del lf.packs[h]
    for h, rp in closure.items():
        target = pack_cache_root / h
        content_hash = cache.hash_directory(target)
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
    city_data = citytoml.read(city_toml_path)
    existing_includes = citytoml.get_includes(city_data)
    managed_handles = set(closure.keys())
    user_includes = [
        e for e in existing_includes
        if e not in managed_handles
    ]
    new_includes = list(user_includes)
    for h in sorted(closure.keys()):
        if h not in new_includes:
            new_includes.append(h)

    new_packs = {}
    for h, rp in closure.items():
        repo_url, _ = gitlib.split_url_and_subpath(rp.url)
        ref = f"v{rp.version}" if rp.subpath == "" else f"{rp.subpath}/v{rp.version}"
        new_packs[h] = citytoml.PacksBlock(
            name=h,
            source=repo_url,
            ref=ref,
            path=rp.subpath,
        )

    existing_packs = citytoml.get_packs(city_data)
    to_remove = set(existing_packs.keys()) - set(new_packs.keys())

    citytoml.update_includes_and_packs(
        city_toml_path,
        new_includes=new_includes,
        new_packs=new_packs,
        removed_packs=to_remove,
    )

    ui.info(f"Updated city.toml ([packs]: {len(new_packs)}, includes: {len(new_includes)})")
    ui.info(f"Wrote pack.lock ({len(lf.packs)} entries)")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
