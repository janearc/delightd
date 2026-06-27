# delightd: model hosting

> **Revision:** v2 — forward-ported from `model-svc/DESIGN.md` (v1, 2026-06-19).
> Model hosting folds **into delightd**: the model-svc project is retired and its
> responsibilities — the deployment descriptor, the `up`/`down`/`health` control
> surface, and the LiteLLM render — become a subsystem of delightd, in Go. (One more
> Python service retired in the process.)

delightd is the fleet's reconciliation engine and source of truth: it holds a roster,
polls, and **checks what it sees against what it knows** (project git-state, missing
paths). A model is just another asset class under that same loop. As in the fossil,
delightd manages **declarations, not weights** — the model files are sacred, read-only
Critical Assets in the HF cache; delightd references them in place and never moves,
copies, or re-downloads them.

## 1. The contract is `model.v1`; transports sit under it

The fleet speaks one model contract — `model.v1` (the `ModelDescriptor`, the
`Modality` / `Architecture` / `Role` / `Provider` enums, and the `Invoke` operation).
A caller **enumerates and reaches any model through that one contract**; the transport
behind a given model is an implementation detail selected by its `Provider`.

| Transport          | Job                                            | Runs as            |
|--------------------|------------------------------------------------|--------------------|
| LiteLLM            | OpenAI-compatible gateway (chat/completion/embedding) | container   |
| Xinference         | serving + registry (embeddings, custom)        | container          |
| ollama             | decoder/chat GGUF (llama.cpp)                   | bare-metal (Metal) |
| transformers / MLX | seq2seq (flan)                                 | bare-metal (Metal) |
| apple-on-device    | the Swift capability provider: transcription, synthesis, on-device text | bare-metal (host) |

**This is the v2 correction to the fossil.** v1 made *"LiteLLM always the gateway,
never a backend."* That held when every model was OpenAI-shaped text. It no longer
does: the apple-on-device provider serves roles OpenAI cannot express (audio→text
transcription, text→audio synthesis), and image generation rides a workflow engine.
So LiteLLM is **demoted to one transport among several, under `model.v1`** — the
umbrella is the contract, not any one server. `Provider` already enumerates the
transports (`OLLAMA`, `OPENAI_COMPATIBLE`, `COMFYUI`, `ANTHROPIC`,
`APPLE_ON_DEVICE`); `Role` already spans chat/completion/embedding, transcription,
synthesis, and image-generation — so text-image and text-video host the same way when
their workflows land.

Container transports (LiteLLM, Xinference) join the traefik network and are reached
in-mesh. Bare-metal transports (ollama, the seq2seq shim, the apple-on-device
provider) run on the host because they need direct hardware (Metal, the Neural
Engine); containers reach them via `host.docker.internal`. The policy exception holds:
containerize unless the software requires bare-metal hardware access. LiteLLM's
`model_list` stays **derived** from the canonical deployment set, so there is one
source of truth and no hand-maintained drift.

## 2. The unit: a deployment as a "model availability signifier"

A delightd **deployment** is a running instantiation of a project; a model deployment
is exactly that — a declaration that a given model, at a given location, served by a
given transport, is reachable behind a traefik route, with a known architecture and
role. The descriptor carries two dimensions beyond a bare endpoint, both already in
`model.v1`:

| Field          | Why it exists                                                              |
|----------------|---------------------------------------------------------------------------|
| `architecture` | encoder \| encoder-decoder \| decoder — determines which transport can host the model at all |
| `role`         | chat \| completion \| embedding \| transcription \| synthesis \| … — the contract the caller binds to |

The validator **fails loud on incoherence**: an encoder-decoder (seq2seq) model
declared on ollama is rejected, because ollama is decoder-only (llama.cpp). This is
the exact failure that blocked paling stage 4 — flan-t5 cannot ride an ollama
`/api/generate` endpoint. The schema makes that a config-time error, not a runtime
surprise. (Example set: the 24b mistral — decode/chat, ollama; flan-t5-large —
encode-decode/completion, transformers.)

## 3. delightd owns the control surface (the fold)

What was a separate project presenting *to* delightd is now a subsystem *of* it. The
control surface is a delightd subcommand and an agent skill, JSON-by-default, the
wrapper CLI **is the contract**:

| Command                              | Meaning                                              |
|--------------------------------------|------------------------------------------------------|
| `model list`                         | list declared deployments (JSON)                     |
| `model up <dep> [--dry-run]`         | bring up, idempotently (`--dry-run` = plan only)     |
| `model down <dep> [--dry-run]`       | take down, idempotently                              |
| `model health [dep]`                 | report the ladder (§4); exit 1 if any unhealthy      |
| `model render`                       | emit the derived LiteLLM config (JSON)               |

`up`/`down` are real and idempotent. Per transport:

| Transport            | `up`                                                | `down`                              |
|----------------------|-----------------------------------------------------|-------------------------------------|
| ollama (mistral-24b) | register the cached GGUF (`ollama create -f`), then warm it into RAM | `ollama stop <tag>` — unload from RAM, registration intact |
| transformers (flan)  | verify weights present on disk for the in-process consumer | no process to stop; flan's RAM is the consumer's |
| apple-on-device      | the host provider is already up; `up` confirms it answers `/health` | host-managed; not delightd's to stop |
| xinference / llama-cpp | plan-only today (emit the steps, say so)          | same                                |

`down` only frees RAM — it never deletes a registration or a weight. So a heavy model
(the 24b) can be turned down to free RAM for kafka without losing its declaration.

## 4. Health is a ladder, not a bit (the new part)

A model's "green" is not one signal. delightd defines a ladder, reported in the
fleet's `observability.v1` `GREEN`/`YELLOW`/`RED`, cheap → expensive:

1. **Declared + weights on disk** — the manifest says it should exist and the files
   are in the HF cache. Free, always-on. This tier is where drift lives (§5).
2. **Reachable** — registered behind its traefik route / its endpoint answers
   `/health`. Free, the poll.
3. **Loadable** — the transport admits it can load it. Backend-dependent: where a
   transport has no "instantiate without committing RAM," this tier collapses into 4.
4. **Integrity** — *not* a bare liveness ping. For a shipped, fine-tuned model, verify it
   is the right checkpoint, intact, and behaving as the specific model it is meant to be
   — the standard supply-chain + eval-as-CI pattern, in two parts:
   - a **weight checksum + signature** verified before first load (catches corruption, a
     swapped checkpoint, bad shard reassembly) — cheap and exact; and
   - a **golden set** the model ships with: a small, versioned battery of
     `{prompt, expected fuzzy output}` spanning capability across domains (a broken MoE
     expert passes some and fails others), identity, and the model's **intended
     refuse-or-answer behavior**. Integrity is **model-relative** — a safety-tuned model
     refuses what an uncensored one answers; the expected behavior *defines* the model.
     Scored fuzzily against the model's own expected outputs, summoned not continuous.

   **Reserved, not built.** It depends on the producer (paling) emitting that integrity
   kit — checksum + golden set — alongside the weights (janearc/paling#43), and on stable
   paling output; punted until then. Today the ladder stops at *reachable* (tiers 1–2).

Steady green is tiers 1–2; tiers 3–4 are requested.

## 5. Reconciliation: the same loop as git-state

delightd already reconciles project git-state — it knows the roster, polls, and flags
drift (a project whose path vanished). Models reconcile the same way:

- walk the declared model dirs / HF cache; a model **present but not in the registry**
  is an `INFO` ("found a model not registered"), not an error;
- a model configured-to-be-served that has **gone missing** from disk or stopped
  answering flips its tier-1/2 state — delightd reports it red, the same drift signal.

This is continuous (tiers 1–2). It is what makes *"tell me what you know about the
world, and whether it still matches what you see"* true for models, not just code.

## 6. Invariants

- **Idempotent plans.** `up`/`down` emit steps guarded by existence checks, safe to
  re-run; `--dry-run` is plan-only.
- **Sacred assets are read-only.** GGUF and HF snapshots are referenced in place,
  never copied or mutated.
- **Telemetry never blocks.** `health` is read-only; a down sink reports down, it does
  not error the caller's intent.
- **Upstream sanctity.** Off-the-shelf images are configured via override only; base
  images stay pull-clean.

## 7. Future direction (explicit, not built today)

delightd routes by hand-writing traefik config files. Adequate for a laptop fleet,
inadequate for where this is going. The intended graduation is **xDS via
go-control-plane** (`github.com/envoyproxy/go-control-plane`): delightd becomes an xDS
management server and the data plane (Envoy) pulls dynamic config, instead of delightd
templating static traefik files. The deployment set already carries host, port, and
route per deployment — the right shape to feed an xDS snapshot cache. (Service Weaver
is not an option: it entered maintenance mode in December 2024.)

## 8. Built vs. not (honest)

- **Built (in delightd, Go):** the `model.v1` contract; the apple-on-device provider
  (serves `model.v1` over loopback HTTP, `/health`); and the `model` subsystem ported from
  model-svc — the descriptor + validator, `list` / `render`, `up` / `down` / `health`, and
  the health ladder's always-on tiers (declared + reachable).
- **To build:** tier 3 (**loadable**); tier 4 (**integrity** — weight checksum + golden
  set; reserved, blocked on paling#43 and stable paling output); the reconciliation walk
  over model dirs; LiteLLM-as-a-transport-under-`model.v1`.
- **Not today:** the xDS graduation; xinference / llama-cpp executed paths.
