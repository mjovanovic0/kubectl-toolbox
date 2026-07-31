# nodecheck

Given a pod, report which of its scheduling **label** requirements no node in the
cluster satisfies — the ones that keep it `Pending`.

```sh
oc toolbox nodecheck <pod> -n <namespace>    # alias: label-check
```

## How the verdict is reached

The AND/OR structure decides the answer, so it is evaluated faithfully rather than
flattened:

- **`nodeSelector`** entries must *all* match (AND).
- **required `nodeAffinity`** needs at least *one* `nodeSelectorTerm` to match (OR).
- Within a term, every `matchExpression` must match (AND).

Operators supported: `In`, `NotIn`, `Exists`, `DoesNotExist`, `Gt`, `Lt`. An absent
label counts as *not in* a set, so `NotIn` is satisfied by a node that lacks the key
entirely — which is what the scheduler does.

The output gives a per-node breakdown showing exactly which requirement failed and
what the node's actual label value was, then a cluster-wide summary. A
`nodeSelector` label that *no* node carries is reported separately, because that one
will never be satisfied by scaling: something has to be labelled.

## What is deliberately not evaluated

**`preferredDuringSchedulingIgnoredDuringExecution`** — soft affinity never blocks
scheduling, so including it would only add noise.

**`matchFields`** selects on node fields such as `metadata.name`, not labels. A term
containing one is reported as **undecidable** rather than being dropped. Dropping it
would narrow the OR and could report a schedulable pod as stuck; a pod whose only
terms are undecidable is therefore a warning, not a hard finding.

**Everything that is not a label.** Taints and tolerations, resource requests,
topology spread constraints and volume topology can all keep a pod `Pending`. The
report states this, so a clean result is not mistaken for "scheduling is fine".

A pod already assigned to a node is noted as such — its label requirements are no
longer what holds it up.

## Exit codes

`1` when no node can satisfy the requirements, `0` otherwise, which makes it usable
as a check in scripts. An undecidable verdict exits `0`: the tool will not claim a
pod is stuck on evidence it does not have.

## Offline

Like every tool here it reads through `--dump`, so you can ask why a pod was
`Pending` from a must-gather after the pod is gone. The dump must include
cluster-scoped nodes; without them no verdict is possible, and the report says so
rather than concluding anything.
