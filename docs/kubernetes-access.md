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
  `rest.InClusterConfig()`. The credential model is the light fake-EKS one below: a
  credential-less kubeconfig plus an externally-delivered token. Nothing kube-credential
  is baked.
- **Applying manifests.** Replace `kubectl apply -f` with **server-side apply** via the
  dynamic client: read the baked YAML, decode to `unstructured`, `Patch` with
  `types.ApplyPatchType` and a stable field manager. SSA gives the 3-way-merge
  behaviour `kubectl apply` has, without a second binary or a diff-and-merge of our
  own.
- **Reads / health.** Typed or unstructured `Get`/`List`, read `.status`. No text
  parsing; typed errors drive the indeterminate-vs-unhealthy decision directly.
- **Reachability.** The container needs a network route to the apiserver. That route is
  ring0 (below): a network holding only the apiserver and delightd, with delightd the sole
  ingress. This is placement/networking, not a client choice.

## Credentials and network: the light fake-EKS model

delightd operates the cluster from *outside* it (a host-level container, not a pod -- an
operator supervised by the thing it operates is a precedence loop). That creates a
bootstrap problem for its credential: it must authenticate to the apiserver to do
anything, and the credential to do so cannot come *from* the cluster it is reaching. You
cannot keep the key to a room inside the locked room. In-cluster pods dodge this because
the kubelet mounts a ServiceAccount token for them; delightd, deliberately not a pod, does
not get one.

The answer is the pattern EKS uses for out-of-cluster clients, minus AWS: an **external
credential**. The kubeconfig holds no secret -- only the apiserver address, the cluster
CA (public), and a pointer to where the credential lives (a `tokenFile`). client-go
re-reads that token file on every request, so rotation is transparent and no credential
plugin ships in the image.

**What lives where:**

| Thing | Where | Secret? |
|-------|-------|---------|
| meubilair manifests, `delight.yaml`, `mcp.json` | baked into the image | no |
| kubeconfig (server + CA + `tokenFile`) | assembled by the host wrapper, mounted read-only | no |
| the ServiceAccount token | 1Password; the wrapper reads it and writes it to the mounted token file | **yes** |

Nothing kube-credential is baked. The kubeconfig is cluster-specific (its CA and server
change when k3d is recreated), so the wrapper assembles it at `delightd start` from k3d's
own output. The token is the only secret, and it never touches an image layer, a `.env`,
or argv.

**The token is a scoped ServiceAccount, not admin.** It is bound by RBAC to exactly what
`furnish` does -- server-side apply and reads on the `fleet` namespace via a namespaced
Role, plus the /readyz-style non-resource URLs, the only cluster-scoped rule the
ClusterRole carries -- so even the bootstrap key is least privilege
(deploy/delightd-operator-rbac.yaml). It lives in 1Password; the host `delightd` wrapper reads
it (`op read`, authenticated host-side by your own 1Password, never in the container) and
writes it to the transient token mount. The wrapper is delightd's local "IMDS": the
trusted identity bridge, the role instance metadata plays on EKS.

**A live biometric gates each fetch.** `op read` goes through the 1Password desktop-app
integration, so it prompts for Touch ID -- a human authorizes every credential fetch, and
delightd cannot obtain cluster access without a live biometric (do not set a 1Password
service-account token, which is silent and would bypass the gate). `delightd creds`
resolves the credential chain on its own -- no container start -- so this path, Touch ID
and all, can be exercised before a full `start` and confirmed in the runbook. The
trade-off: `delightd start` is a human-in-the-loop action, which suits the operator-driven
recovery it exists for; fully-unattended boot survival (a launchd auto-start with no one to
tap Touch ID) would need a silent credential and is a separate decision, not wired here.

**Why the token is static-scoped, not short-lived-minted.** Minting bound tokens requires
apiserver access, and the apiserver lives on ring0 where delightd is the sole ingress --
the host cannot reach it to mint. So the token is a scoped, long-lived SA token,
provisioned once and delivered from 1Password. Truly short-lived, cluster-minted tokens
would need a token-authenticator webhook the apiserver trusts (the "full fake EKS"): a
separate service, noted as a follow-on, not built here.

**ring0.** The apiserver and delightd share one network, ring0, and *only* those two
things live there. delightd is the sole ingress on `:8088`; the apiserver is not published
to the host at all. Every mesh service is a k3s pod reached *through* the apiserver, so
nothing else belongs on ring0. A consequence, and an intended one: there is no raw
`kubectl` from a shell, ever -- the apiserver is unreachable except through delightd, and
the runbook uses `delightd furnish` and the control port for everything.

### Bootstrap: the concrete steps

This is what a human runs once, with cluster admin, to stand up delightd's credential --
the out-of-band step the bootstrap loop forces. It is not automated.

**0. 1Password, one time.** Enable the desktop-app CLI integration so `op read` prompts
Touch ID: 1Password app -> Settings -> Developer -> *Integrate with 1Password CLI*, and
Settings -> Security -> *Unlock using Touch ID*.

**1. The cluster on ring0.** Either create it there:

```
k3d cluster create fleet --network ring0
```

or, for an existing cluster, attach its apiserver proxy to ring0 (non-destructive):

```
docker network create ring0
docker network connect ring0 k3d-fleet-serverlb
```

**2. The scoped identity.** With admin, apply the RBAC and confirm the token Secret filled:

```
kubectl apply -f deploy/delightd-operator-rbac.yaml
kubectl -n delightd get secret delightd-operator-token -o jsonpath='{.data.token}' | wc -c   # non-zero
```

**3. The 1Password item.** Create it once, then load the real token into it. The token goes
kubectl -> op in your shell; it never lands in a file or a script:

```
op item create --category "API Credential" --title delightd-k8s-token --vault Personal
op item edit delightd-k8s-token \
  credential="$(kubectl -n delightd get secret delightd-operator-token -o jsonpath='{.data.token}' | base64 -d)"
```

Point `.env` at it: `DELIGHT_TOKEN_ITEM=op://Personal/delightd-k8s-token/credential`.

**4. The CA and address.** Extract the public cluster CA to the path `.env` names, and set the
ring0-internal apiserver URL:

```
kubectl config view --raw --minify \
  -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d > ~/.kube/k3d-fleet-ca.crt
```

`.env`: `DELIGHT_APISERVER=https://k3d-fleet-serverlb:6443`, `DELIGHT_CA_SOURCE=~/.kube/k3d-fleet-ca.crt`.

**5. Verify, then start.**

```
delightd creds     # Touch ID; the token byte count jumps from placeholder to the real JWT
delightd start     # Touch ID; brings the container up on ring0 with the creds mounted
delightd status    # readyz HTTP 200
```

Under the hood: delightd's `FromKubeconfig` loads the mounted kubeconfig by the standard
rules, and client-go re-reads the token file per request, so `/readyz` goes green once it
reaches the apiserver.

**Rotating the token.** The SA token is long-lived. If you rotate the ServiceAccount, or the
cluster is recreated (new CA), re-run step 3's `op item edit` and step 4's CA extract. Nothing
else changes -- the wrapper re-reads op and the CA on the next `delightd start`.

**Why not the alternatives** (each was considered):

- *In-cluster ServiceAccount (`rest.InClusterConfig`)* -- requires delightd to be a pod,
  which reopens the supervision precedence loop. Rejected at the containerization ruling.
- *macOS Keychain* -- works, offline, Secure-Enclave-backed, but binds the credential to
  one Mac and one store. 1Password is the fleet's secret store and is cross-device.
- *`op` inside the container* -- would need a 1Password service-account token delivered
  into the container (the bootstrap problem, relocated, with a broader blast radius), an
  `op` binary in the scratch image, and container egress to 1Password's cloud at every
  start -- a network dependency in the recovery path, exactly when the network is most
  likely degraded. The wrapper reads `op` host-side instead.
- *Admin kubeconfig in the container* -- too much authority; the scoped SA token is the
  least-privilege version.

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
  - **#52** -- vendor external protos from a **buf registry**, not `cp -R ../sibling`.
    An in-container build has no `../big-little-mesh` to copy from, so containerizing
    the build makes the relative-fs-copy mechanism unworkable; the contracts come from
    the buf registry instead (operator's call: buf registry, not a git submodule).
- **Mentions / eases:**
  - **#87** -- workstation layout (`~/mesh`). Config-by-mount instead of `$HOME`-rooted
    paths eases the relayout; it does not resolve it (87 is the host move itself).

## Plan on the branch

1. (done) Config loading (`clientcmd` + kubeconfig) and a shared client + dynamic client.
2. (done) Rewrite `furnish`'s kubectl calls: SSA for apply, typed reads for health, typed
   errors for the taxonomy (closes #96).
3. (done) `/readyz`'s cluster check (renamed `apiserver_reachable`) now pings the
   apiserver's `/readyz` via client-go, not `kubectl --raw`. This retired the last
   kubectl shell-out in the codebase.
4. Drop the kubectl-in-image question (#107) entirely; the image needs a kubeconfig
   mount + apiserver reachability, not a binary.
5. Integration-test the whole: clone -> build -> run comes up `/readyz`-green and
   drives k3s through the API. Lands to main as one merge only then.
