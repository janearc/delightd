# How delightd talks to Kubernetes: client-go, not the kubectl binary

## Decision

delightd talks to the Kubernetes API through `client-go` -- the Go client compiled
into the delightd binary -- instead of shelling out to the `kubectl` binary. The
current code (`furnish`, and the `/readyz` probe) forks `kubectl` and parses its
stdout. The note in `operations.md` ("delightd does not call the Kubernetes API -- no
client-go") was descriptive of that state, not a constraint; this reverses it.

## Why, especially in a container

- **No binary to ship.** The runtime image is `FROM scratch` -- no shell, no package
  manager, no `kubectl`. Shelling out means shipping and version-pinning a second
  binary in that image. client-go is already in the delightd binary, so there is
  nothing external to ship or keep current. scratch stays minimal (it still carries
  the baked config -- the `kube/` + `meubilair.yaml` tree from the config bake -- so
  the image has the manifests it applies; what it lacked was the *applier*).
- **Typed objects and typed errors kill a class of bug.** Shelling makes `furnish
  health` parse text: `kubectlRunner` uses `CombinedOutput`, so a benign kubectl
  stderr warning folds into the `-o json` it then parses, and there is no way to tell
  *indeterminate* (apiserver unreachable / RBAC / timeout) from *unhealthy*. client-go
  returns typed objects, and `apierrors.IsNotFound` / `IsUnauthorized` / a transport
  error are distinguishable -- the fail-loud taxonomy becomes trivial.
- It is the idiomatic way a Go control plane speaks to Kubernetes.

## Approach

- **Auth / config.** delightd is a host-level container (the operator, outside the
  fleet it drives), not a pod -- so it uses `clientcmd` to load a kubeconfig, not
  `rest.InClusterConfig()`. The kubeconfig is a runtime credential: mounted read-only
  or referenced by `KUBECONFIG`, never baked into the image.
- **Applying manifests.** Replace `kubectl apply -f` with **server-side apply** via the
  dynamic client: read the baked YAML, decode to `unstructured`, `Patch` with
  `types.ApplyPatchType` and a stable field manager. SSA gives the 3-way-merge
  behaviour `kubectl apply` has, without a second binary or a diff-and-merge of our
  own.
- **Reads / health.** Typed or unstructured `Get`/`List`, read `.status`. No text
  parsing; typed errors drive the indeterminate-vs-unhealthy decision directly.
- **Reachability.** The container still needs a network route to the apiserver (and,
  separately, to in-cluster services). This is placement/networking, not a client
  choice -- it is the "operator outside the fleet, reaching in" question, and it is the
  same route that lets delightd resolve in-cluster DNS names.

## What this move touches (honest about how strongly)

- **Resolves directly:**
  - **#107** -- no kubectl in the image. Gone: the client is in the binary; scratch
    needs nothing added.
  - **#96** -- furnish health's error taxonomy. The stderr-fold and
    indeterminate-vs-unhealthy problems are artifacts of shelling; typed errors remove
    both.
- **Resolves via the containerization (placement, not client-go alone):**
  - **#88** -- production `/register` fail-closes 503 because bare-metal delightd can't
    resolve the in-cluster schema-registry DNS (`schema-registry:8081`). A delightd
    that runs where in-cluster names resolve (the same route it needs for the
    apiserver) reaches the schema registry. The reachability work for the API and for
    the schema registry is one problem.
- **Unlocks (client-go gives delightd programmatic cluster access it did not have):**
  - **#85** -- verify meubilair vs k3s. delightd can `List` what the cluster actually
    runs and compare it to what it furnishes, instead of a manual audit.
  - **#24** -- registry drifts from the roster. delightd can reconcile its registry
    against real cluster state.
- **Becomes the substrate for:**
  - **#34** (model-svc folded into delightd) and **#35** (fleet-svc folded in): their
    deploy/reconcile-against-the-cluster operations are native client-go once this
    lands, rather than more shelling. **#38** (model descriptor vs `model.v1`) rides in
    with #34.
- **Forced / obsoleted:**
  - **#52** -- vendor external protos by git submodule, not `cp -R ../sibling`. An
    in-container build has no `../big-little-mesh` to copy from, so containerizing the
    build makes the relative-fs-copy mechanism unworkable and forces the submodule (or
    registry) sourcing #52 asks for. (Confirm the intended mechanism -- submodule vs
    buf registry.)
- **Mentions / eases:**
  - **#87** -- workstation layout (`~/mesh`). Config-by-mount instead of `$HOME`-rooted
    paths eases the relayout; it does not resolve it (87 is the host move itself).

## Plan on the branch

1. Config loading (`clientcmd` + kubeconfig) and a shared client + dynamic client.
2. Rewrite `furnish`'s kubectl calls: SSA for apply, typed reads for health, typed
   errors for the taxonomy (closes #96).
3. Point `/readyz`'s `kubectl_reachable` check at a client-go apiserver ping.
4. Drop the kubectl-in-image question (#107) entirely; the image needs a kubeconfig
   mount + apiserver reachability, not a binary.
5. Integration-test the whole: clone -> build -> run comes up `/readyz`-green and
   drives k3s through the API. Lands to main as one merge only then.
