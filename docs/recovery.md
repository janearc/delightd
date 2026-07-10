# delightd — machine-recovery runbook

The fleet does not come back from a reboot by itself. After a restart, an OS
upgrade, or a power event, walk this document top to bottom: each step is a
command to run and the output that proves the step is done. Nothing here is
an emergency, because nothing is an emergency — the daemon fails closed and
the state on disk keeps.

Scope: bringing THIS machine's fleet back to serving. Cold bring-up of a new
machine follows `scripts/install.sh`, validated 2026-07-10 (findings in the
table below); its full prerequisite set is listed under known gaps.

The one rule, learned the hard way: **a thing listed is not a thing alive.**
Ask the serving surface, not the inventory.

## 1. Toolchain

```bash
go version          # prints go1.x
task --version      # prints 3.x
buf --version       # prints 1.x
which protoc-gen-go # prints a path under ~/go/bin
```

Failure modes seen in the field:

- `task --version` prints `Cannot proceed without rc file.` — that is
  **Taskwarrior**, not go-task; both install a binary named `task` and
  Homebrew's formula named `task` is the wrong one. Fix:
  `brew uninstall task && brew install go-task`.
- `protoc-gen-go` (or `buf generate`) not found while the files sit intact in
  `~/go/bin` — the OS upgrade dropped `~/go/bin` from PATH by resetting shell
  init. The tools did not go anywhere. The fix is one line, in `~/.zprofile`
  (macOS login shells source it; `~/.zshrc` works too if PATH lives there):

  ```bash
  export PATH="$HOME/go/bin:$PATH"
  ```

  Then open a fresh shell and re-run the checks above. As of this writing
  the permanent line is NOT in any rc file on this machine — the 2026-07-05
  recovery used per-command overrides. Until it lands, prefix any building
  step with it: `PATH="$HOME/go/bin:$PATH" task build`.

## 2. Container runtime (colima)

```bash
colima status   # "colima is not running" after any reboot is normal
colima list     # the default profile should exist, likely Stopped
colima start
docker info --format '{{.ServerVersion}}'   # prints a version, not an error
```

If `colima list` shows no profile at all, the VM is gone and this stops being
a runbook problem — do not improvise a re-provision mid-recovery; that is a
deliberate operation with its own downloads.

## 3. Cluster (k3d)

```bash
k3d cluster list        # shows "fleet" -- BUT LIST IS NOT LIVENESS
kubectl get nodes       # the real check: STATUS "Ready"
```

Seen in the field: the k3d containers run but the API server answers EOF
after a reboot into a new OS. The fix is a full stop/start cycle, not a wait:

```bash
k3d cluster stop fleet && k3d cluster start fleet
kubectl get nodes       # Ready, within about a minute
```

## 4. The daemon

```bash
curl -s http://127.0.0.1:8088/health
# {"status":"ok","active_projects":N,"dry_run":false,"degraded":false}
```

Connection refused means delightd is down. Docker restarts a *crashed*
daemon on its own (`restart: unless-stopped`), but a reboot takes colima
with it and nothing brings colima back — so after a boot, delightd is down
until a human runs the wrapper. Bring it back through the wrapper, never by
hand-running the container's internals:

```bash
delightd start
delightd status
curl -s http://127.0.0.1:8088/health   # ok, degraded:false
```

`delightd start` is `colima start` (if needed) + resolving credentials from
1Password (Touch ID) + `docker compose build` (image, commit-stamped) +
`docker compose up -d` — see `scripts/delightd`. There is no separate build
step and no on-disk `bin/delightd` to `nohup`: the daemon runs as a container,
and the wrapper is the only front door the runbook should use. `delightd
status` folds runtime + container + `/readyz` into one exit code — 0 only
when all three are up — so it is the one command to gate recovery on, here
and everywhere else in this document.

This restart path had its live bounce 2026-07-10 (the sprint-15 capstone
run): `stop`/`start` through the wrapper, readyz 200 with both checks green
on return. Proven procedure, not just a documented one.

Seen in the field: after 1Password self-updates, the still-running app
serves no CLI socket and every `op read` dies with "couldn't connect to the
desktop app" — even though the app looks fine and unlocks fine. The tell is
`--just-updated --should-restart` on the 1Password process. Quit the app
fully (Cmd+Q, not the window), relaunch, re-run `delightd start`.

`degraded: true` in the health body means the config half-loaded; the
`warnings` field says why. The daemon serves anyway — read the warnings, fix
the config, bounce it.

## 5. Enablement state

```bash
curl -s http://127.0.0.1:8088/state
```

Reads fail closed, so interpret exactly:

- Every project listed, some `"recorded": false` — normal. An absent record
  reads `disabled` by doctrine; it is a default, not data loss.
- `503` with `"degraded": true` — the state store did not open. Check disk
  and permissions on `~/var/state/enablement.db`, then bounce the daemon.
  The store survives restarts; a record you wrote before the reboot is
  still there once the store opens.

## 6. Furnishings

```bash
delightd furnish list     # the declared pieces
delightd furnish health   # ladder; non-zero exit if anything is RED
```

A RED deployment right after cluster start is usually an image pull still in
flight — check, do not guess:

```bash
kubectl get pods -n fleet
kubectl get events -n fleet --sort-by=.lastTimestamp | tail
```

A piece that stays RED after the pull settles: `delightd furnish up <piece>`
re-converges it (idempotent; a no-op on a healthy piece). Still RED after a
re-converge means the piece's manifests or image are broken, not the
reconcile — read the pod events and go to that piece's directory under
`kube/`.

Two more failure modes seen in the field (2026-07-10):

- `furnish health` can abort the whole estate report on one piece's error
  (issue 96 is the taxonomy fix); `delightd furnish health <piece>` still
  answers for every other piece, so interrogate piece-by-piece when the
  full report blanks.
- A pod stuck `ImagePullBackOff` on an image that was only ever built
  locally (the `good-citizen-dummy` class) is not broken manifests: the
  node cache dropped the image and the cluster has no registry to re-pull
  from. `k3d image import <image>:<tag> -c fleet` restores it.

Furniture whose manifests still live in other repos (kafka, the elk stack)
is inventoried in `meubilair.yaml` at the repo root. That file is a
declarative index — nothing executes its probes yet — so turning an entry
into a live check is on you, and the recipe is mechanical:

- an `httpGet` probe: port-forward the service, curl the path —

  ```bash
  kubectl port-forward -n fleet svc/schema-registry 18081:8081 &
  curl -s http://127.0.0.1:18081/subjects   # 200 + a JSON list = healthy
  kill %1
  ```

- an `exec` probe: run the entry's command inside the pod —

  ```bash
  kubectl exec -n fleet kafka-0 -- sh -c \
    'kafka-broker-api-versions --bootstrap-server localhost:9092' > /dev/null \
    && echo kafka OK
  ```

Close the loop on the whole namespace, because furnish and meubilair
together do not cover everything that runs:

```bash
kubectl get pods -n fleet   # everything should be Running
```

Pods here that neither surface names (today: good-citizen-dummy,
obs-svc-agg, paling-sidecar) are workloads deployed from their own repos;
Running status in this listing is their recovery check, and anything deeper
belongs to those repos' docs.
