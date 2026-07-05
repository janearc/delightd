# delightd — machine-recovery runbook

The fleet does not come back from a reboot by itself. After a restart, an OS
upgrade, or a power event, walk this document top to bottom: each step is a
command to run and the output that proves the step is done. Nothing here is
an emergency, because nothing is an emergency — the daemon fails closed and
the state on disk keeps.

Scope: bringing THIS machine's fleet back to serving. Cold bring-up of a new
machine is a different document (the install script exists at
`scripts/install.sh`; validating a true cold start is deferred until there is
hardware to be cold on).

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

Connection refused means delightd is down — and, known gap, **nothing
restarts delightd today**; a reboot silently takes it out until a human
notices. Bring it back:

```bash
cd ~/work/delightd
task build
nohup ~/work/delightd/bin/delightd >> ~/var/delightd-nohup.log 2>&1 &
disown
curl -s http://127.0.0.1:8088/health   # ok, degraded:false
```

`task build` regenerates the proto bindings first, and buf's plugins resolve
off PATH — step 1's PATH fix is a prerequisite here. A
`protoc-gen-go: executable file not found` error at this step is step 1
unfinished, not a build bug. A `nohup`'d daemon inherits PATH from the shell
that launched it, not from an rc file you have not sourced.

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
cd ~/work/delightd
./bin/delightd furnish list     # the declared pieces
./bin/delightd furnish health   # ladder; non-zero exit if anything is RED
```

A RED deployment right after cluster start is usually an image pull still in
flight — check, do not guess:

```bash
kubectl get pods -n fleet
kubectl get events -n fleet --sort-by=.lastTimestamp | tail
```

A piece that stays RED after the pull settles: `furnish up <piece>`
re-converges it (idempotent; a no-op on a healthy piece). Still RED after a
re-converge means the piece's manifests or image are broken, not the
reconcile — read the pod events and go to that piece's directory under
`kube/`.

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

## What happened when we were last there (2026-07-05, macOS 26 -> 27)

| Found | Fix | Time cost |
|-------|-----|-----------|
| colima VM present but Stopped | one `colima start`, no re-provision | seconds |
| k3d containers up, API answering EOF | `k3d cluster stop/start fleet` | ~1 min |
| delightd not running, nothing noticed | detached restart (step 4) | minutes |
| `task` eaten by the upgrade; reinstall grabbed Taskwarrior | `brew uninstall task && brew install go-task` | minutes |
| `~/go/bin` dropped from PATH; plugins intact | per-command override during the recovery; the permanent rc line (step 1) is still pending | minutes |

## Known gaps, named

- **No supervision**: delightd has no launchd/kube-managed lifecycle on this
  machine; step 4 is manual until that lands. The enablement state home
  (`/state`) exists precisely so the fleet can record intent that survives
  these gaps.
- **Cold bring-up is unvalidated**: this document recovers a machine that
  was already configured. A from-nothing machine follows
  `scripts/install.sh`, which has not yet been executed on truly cold
  hardware.
