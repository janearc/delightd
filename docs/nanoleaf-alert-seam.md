# The Nanoleaf alert seam

The contract between this repo and `janearc/lights`. delightd decides what is
worth a light and delivers it. lights owns the devices, the colours, and the
receiver. Neither reaches into the other.

## What earns a light

Two conditions, ruled by the operator on 2026-08-21 -- OOM kills, and disk
pressure. Disk is expressed as a three-rung ladder rather than one rule, for the
reason given under "what the light does":

| Alert | Fires when | `nanoleaf_effect` |
|---|---|---|
| `ContainerOOMKilled` | a container was killed for exceeding its memory limit in the last 30m | `oom-bounce` |
| `NodeDiskFillingUp` | node disk above 80% for 10m | `disk-throb-slow` |
| `NodeDiskFillingFast` | node disk above 90% for 5m | `disk-throb-medium` |
| `NodeUnderDiskPressure` | the kubelet reports `DiskPressure` for 2m | `disk-throb-fast` |

Nothing else, and this is the load-bearing decision rather than a starting
point to expand from. **If everything lights it up, nothing does.** A light that
means "something, somewhere, maybe" is one you learn to ignore, and then it is
worse than no light, because you believe you would have noticed.

Routing is **opt-in per rule**, not severity-based. An alert reaches lights only
if it carries the label `nanoleaf: "true"`. Adding a new alert rule does not
add a light; someone has to decide it earned one. Everything else routes to a
receiver named `blackhole` that does nothing — the alert is still visible in
Prometheus and Grafana. Not routed is not the same as not recorded.

## What the light does

Two behaviours, one per condition, named by the `nanoleaf_effect` label. delightd
says **which effect**; lights owns what the effect *is* -- which panels, which
colours, the timing. The name is the whole contract. That split is deliberate:
if delightd carried RGB values, changing a colour would mean an image rebuild, a
holden ruling and a control-plane restart, and the colour would never get
changed.

**`oom-bounce`** -- OOM kills. The operator's words: *scary as fuck*. Chosen
panels alternate between red and their colour in the current scene, so it reads
as bouncing. Something is actively broken and wants a human.

**`disk-throb-slow` / `-medium` / `-fast`** -- the disk ladder. The current scene
throbbing slowly down to dark and back, **faster as the disk fills**. Less
alarming than the bounce on purpose: disk fills over hours and you have time.

### Why three disk effects instead of one with a speed parameter

Because an Alertmanager label is a static string from the rule. A value that
changes cannot ride in a label without minting a new alert for every new value,
which would re-notify constantly and blow up cardinality. So the ramp is
expressed as buckets: three rules, three effect names, rising urgency.

**Exactly one lights at a time.** A node at 95% under DiskPressure satisfies all
three rules, so `inhibit_rules` in alertmanager.yaml suppress the lower rungs --
`fast` silences `medium` and `slow`, `medium` silences `slow`, matched on `node`
so one node's crisis never silences another's warning. Three effects arriving
together would fight over the same panels and the light would show whichever
webhook landed last, which is worse than showing nothing because it looks
deliberate.

### Both effects are RELATIVE, and that is the hard part

"alternate with the current scene" and "throb down from the current scene" are
not expressible as stored Nanoleaf effects. The device's `animData` is a flat
string of **absolute** RGBW values per panel per frame -- `numPanels panelId
numFrames R G B W transTime ...` -- addressed by hardware panel ID. There is no
"whatever is playing right now" primitive.

So these cannot be stored on the device and triggered by name. lights has to
**compose** them at alert time against whatever scene is active, then restore
that scene on resolve. That is why the effect name is a contract and not a
pointer to a device-stored effect.

lights already has the vocabulary for this: `src/animation.ts` defines
`AnimationIntent`, a device-independent description of what a light is trying to
do over time, with per-device adapters. And `src/indicators.ts` already binds a
named indicator to a chosen panel with operator-editable config persisted in
`~/.config/lights/`. An alert is another indicator source -- pushed rather than
polled.

## What lights actually receives

**This payload is Alertmanager's, not ours.** It is not a schema we chose and
not one we can change — inventing a custom shape here would mean writing a
translator nobody asked for. Alertmanager POSTs its standard webhook v4 body:

```json
{
  "version": "4",
  "groupKey": "{}:{alertname=\"ContainerOOMKilled\"}",
  "truncatedAlerts": 0,
  "status": "firing",
  "receiver": "nanoleaf",
  "groupLabels":      { "alertname": "...", "namespace": "...", "pod": "..." },
  "commonLabels":     { "severity": "warning", "nanoleaf": "true" },
  "commonAnnotations":{ "summary": "..." },
  "externalURL": "http://alertmanager.fleet.svc:9093",
  "alerts": [
    {
      "status": "firing",
      "labels":      { "alertname": "ContainerOOMKilled", "namespace": "fleet",
                       "pod": "logstash-0", "severity": "warning",
                       "nanoleaf": "true" },
      "annotations": { "summary": "fleet/logstash-0 was OOMKilled ..." },
      "startsAt": "2026-08-21T03:14:00Z",
      "endsAt":   "0001-01-01T00:00:00Z",
      "generatorURL": "http://prometheus.localhost:8800/graph?...",
      "fingerprint": "a1b2c3d4e5f6"
    }
  ]
}
```

### The three fields that matter

- **`status`** — `"firing"` or `"resolved"`. `send_resolved: true` is set, so
  lights gets told when it is over. **A light that only knows how to turn on is
  half a light.**
- **`alerts[].labels.nanoleaf_effect`** — the effect to play. This is the field
  to key on, not `alertname`: it is the contract, and it is what lets the disk
  ladder add a rung without lights changing.
- **`alerts[].status`** — the per-alert state. Do not read the top-level
  `status` alone: one notification can carry a mix, where some alerts in the
  group have resolved and others have not.

`fingerprint` identifies a specific alert across notifications and is the right
key if lights tracks state per alert rather than per group.

### Things that will surprise you

- **`endsAt` is `0001-01-01T00:00:00Z` while firing.** It is not a real date.
  Treat any year below 1971 as "still going".
- **Notifications repeat.** `repeat_interval` is 12h, so a condition that stays
  true re-notifies twice a day. The receiver must be idempotent — re-lighting an
  already-lit light must be a no-op, not a queued animation.
- **Grouping means one POST can carry several alerts.** `group_by` is
  `alertname, namespace, pod, node`, so ten OOM kills in one pod arrive as one
  notification, but two different pods arrive as two.
- **`groupKey` is opaque.** Do not parse it. Use labels.

## The endpoint

    http://lights.mesh/nanoleaf/alert

lights owns this. It does not exist yet, and that ordering is deliberate: the
contract lands first so the receiver can be built against something fixed.

Until it exists, **delivery fails and Alertmanager logs it.** That is correct.
A seam that silently swallowed undeliverable alerts would look identical to a
working one — which is precisely the failure this estate found in four separate
places on 2026-08-20: a liveness probe 404ing against a healthy process, alert
rules evaluating over gapped data, a rule written as `== 0` that can never
match, and a backup daemon that could not see any of its projects while
reporting healthy. Loud beats tidy.

The hostname resolves through the cluster's CoreDNS wildcard: every `*.mesh`
name answers with traefik's address, and traefik routes on the Host header. So
lights needs a Host rule for `lights.mesh`, not a DNS entry.

## Testing it without waiting for a real fault

Post the payload above straight at the receiver — that exercises lights without
involving Prometheus at all:

    curl -X POST http://lights.mesh/nanoleaf/alert \
         -H 'content-type: application/json' -d @sample.json

To test the delightd half, check Alertmanager saw the rule and routed it:

    kubectl -n fleet port-forward svc/alertmanager 9093:9093
    curl -s localhost:9093/api/v2/alerts | jq '.[].labels'

An alert present there but never delivered means the receiver is unreachable.
An alert absent there means Prometheus never fired it, which is a rules
question and not a lights one.
