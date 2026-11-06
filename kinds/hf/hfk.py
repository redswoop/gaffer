import argparse, glob, json, os, shutil, sys

PLAN = os.path.join(os.environ.get("W_JOBDIR", "."), "hf-plan.json")


def partials(local):
    """In-flight downloads. huggingface_hub keeps them under .cache/huggingface/download/ mirroring the repo's
    folder layout (a file in MTP/ has its .incomplete in download/MTP/), hence the recursive glob."""
    return glob.glob(os.path.join(local, ".cache/huggingface/download/**/*.incomplete"), recursive=True)


def plan(argv):
    ap = argparse.ArgumentParser(prog="hfk plan")
    ap.add_argument("repo_id")
    ap.add_argument("filenames", nargs="*")
    ap.add_argument("--type", "--repo-type", dest="repo_type", default="model")
    ap.add_argument("--revision")
    ap.add_argument("--include", action="append")
    ap.add_argument("--exclude", action="append")
    ap.add_argument("--local-dir")
    ap.add_argument("--token")
    ap.add_argument("--force-download", action="store_true")
    ap.add_argument("--max-workers", type=int)
    ap.add_argument("--cache-dir")
    a, _ = ap.parse_known_args(argv)
    if not a.local_dir:
        sys.exit("hf kind: --local-dir is required (progress is measured in that directory)")
    local = os.path.abspath(os.path.expanduser(a.local_dir))

    from huggingface_hub import snapshot_download
    allow = (a.include or []) + a.filenames or None
    rows = snapshot_download(a.repo_id, repo_type=a.repo_type, revision=a.revision, token=a.token,
                             allow_patterns=allow, ignore_patterns=a.exclude, local_dir=local,
                             force_download=a.force_download, dry_run=True)
    files = [{"name": r.filename, "size": r.file_size, "path": r.local_path, "todo": r.will_download} for r in rows]
    total = sum(f["size"] for f in files)
    todo = sum(f["size"] for f in files if f["todo"])
    if not files:
        sys.exit("hf kind: nothing matches that repo/filter")

    probe = local
    while not os.path.exists(probe):
        probe = os.path.dirname(probe)
    free = shutil.disk_usage(probe).free
    # partial files already on disk count toward what is still needed
    have = sum(os.path.getsize(p) for p in partials(local))
    need = max(0, todo - have)
    spare = float(os.environ.get("HF_MIN_FREE_GB", "2")) * 2**30
    if need + spare > free:
        sys.exit(f"hf kind: needs {need/2**30:.1f} GiB (+{spare/2**30:.0f} spare), {probe} has {free/2**30:.1f} GiB free")

    json.dump({"local": local, "total": total, "files": files}, open(PLAN, "w"))
    print(f"::phase plan")
    print(f"::msg {len(files)} file(s), {todo/2**30:.2f} GiB to fetch into {local} ({free/2**30:.0f} GiB free)")
    print(f"::progress done={total-todo} total={total} unit=bytes")


def poll(_):
    try:
        p = json.load(open(PLAN))
    except (OSError, ValueError):
        return  # plan hasn't run yet: say nothing (a done=0 would overwrite pushed progress)
    done = 0
    for f in p["files"]:
        try:
            if os.path.getsize(f["path"]) == f["size"]:
                done += f["size"]
        except OSError:
            pass
    for g in partials(p["local"]):
        try:
            done += os.path.getsize(g)
        except OSError:
            pass
    print(f"::progress done={min(done, p['total'])} total={p['total']} unit=bytes")


if __name__ == "__main__":
    {"plan": plan, "poll": poll}[sys.argv[1]](sys.argv[2:])
