# delightd — backups and retention

delightd checkpoints projects by writing rotating `.tgz` archives. This document
covers the pipeline, the skip/exclude rules, rotation, and the one invariant that
must never be violated: delightd only ever rotates its own archives — it never
deletes from model, weight, or cache directories.

## The hard invariant

> delightd's destructive footprint is exactly one thing: deleting old `.tgz`
> archives it previously wrote, under its own backup root. It never deletes from
> any project's working tree, and never from model/weight/cache directories.

Large regenerable trees (model weights especially) are kept out of a backup by
**exclusion from the tar**, not by deletion. delightd reads those trees only to
*skip* them. The fleet's sacred caches (`~/.cache/huggingface`,
`~/.cache/modelscope`, the comfyui models tree) are never written or deleted by
delightd under any configuration.

## Canonical paths

| Path | Contents |
|------|----------|
| `~/var/backups` | the backup root: `<root>/backups` where `system.root` is `~/var` |
| `~/var/backups/<project>/` | one project's archives |
| `~/var/backups/<project>/<project>-<YYYYMMDD-HHMMSS>.tgz` | one checkpoint |

The backup root is `system.root + "/backups"`. With the canonical
`system.root: ~/var`, that is **`~/var/backups`**.

> Path note. A separate config-fix PR addresses a double-suffix case: when
> `DELIGHT_SYSTEM_ROOT` is set to a path that already ends in `backups`, the
> `+ "/backups"` join produced `.../backups/backups`. The canonical, documented
> path is `~/var/backups`; set `system.root` (or `DELIGHT_SYSTEM_ROOT`) to
> `~/var`, not to `~/var/backups`. It is never `/var/backups/backups` and never
> `~/work/backups`.

## The pipeline

`pkg/backup.CreateCheckpoint` runs deterministically and walks the project tree
once, through a single skip predicate (`walkCheckpoint`) shared by both the
dry-run manifest and the real tar — so they cannot disagree about what is
included.

1. **Manifest** — walk `project.path`, applying the built-in skips and the
   project's `exclude` list. Directories that match a skip/exclude are pruned
   (the walk does not descend into them).
2. **Compress** — stream each surviving regular file into a gzipped tar. The
   archive path is `<root>/backups/<project>/<project>-<timestamp>.tgz`.
3. **Account** — `bytes_before` is the sum of included regular-file sizes
   (pre-compression); `bytes_after` is the written `.tgz` size. Both feed the
   `delight.v1.BackupEvent` ([events.md](events.md)).
4. **Rotate** — enforce `max_archives` for that project (below).

`--dry-run` walks the manifest and reports the file count and `bytes_before`
without writing the tar — the safe way to see what a checkpoint *would* capture.

## What gets skipped

**Built-in directory skips** (always, every project):

| Skipped dir | Skipped file extension |
|-------------|------------------------|
| `.git` | `.o` |
| `.venv` | `.so` |
| `node_modules` | `.pyc` |
| `__pycache__` | |

**Configured excludes** (`backup.exclude`, project-relative). An entry matches if
it is the path itself, a parent prefix of the path, **or a bare directory/file
name at any depth**. The name form is what "exclude the model dirs" means in
practice: comfyui keeps weights at `src/ComfyUI/models`, not at the project root,
so a config of `exclude: ["models"]` catches it wherever it sits.

```yaml
projects:
  - name: comfyui
    path: ~/work/comfyui
    backup:
      check_interval: "30m"
      exclude:
        - "models"        # matches src/ComfyUI/models, and any other "models" dir
      rotation:
        max_archives: 24
```

## Rotation

`enforceRotation` keeps at most `max_archives` `.tgz` files per project,
deleting the oldest (archives sort lexicographically, which the timestamp naming
makes chronological).

| `max_archives` | Behavior |
|----------------|----------|
| `> 0` | keep that many; delete the oldest beyond it |
| `<= 0` (unset, `0`, negative) | **unlimited — keep everything** |

The `<= 0` reading is the safe interpretation of an unset config: a missing
rotation policy must never delete the checkpoint that was just written. Rotation
only ever removes `.tgz` files from the project's own archive directory; it reads
nothing else and deletes nothing else.

## Triggers

A checkpoint runs when the per-project state machine reaches `backing_up`. That
happens by:

- the **git oracle** detecting churn on the per-project poll loop (the normal
  path; see [architecture §3](../delightd_architecture.md#3-the-git-oracle-churn-detection)),
  or
- a manual **`POST /projects/{name}/backup`** ([api.md](api.md#post-projectsnamebackup)),
  or
- the `--immediate` flag forcing an evaluation on startup.

On success the machine returns to `fallow`; on failure it enters `error` with
exponential-style backoff and retries when the backoff expires. Either outcome
emits a best-effort `BackupEvent`.
