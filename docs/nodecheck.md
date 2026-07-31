# nodecheck

Given a pod, reports which of its scheduling **label** requirements no node in the
cluster satisfies. Those are the ones keeping it `Pending`.

```sh
oc toolbox nodecheck <pod> -n <namespace>    # alias: label-check
```

## How the verdict is reached

The AND/OR structure decides the answer, so it is evaluated as the scheduler does it
rather than flattened:

- `nodeSelector` entries must all match (AND).
- Required `nodeAffinity` needs at least one `nodeSelectorTerm` to match (OR).
- Within a term, every `matchExpression` must match (AND).

Supported operators: `In`, `NotIn`, `Exists`, `DoesNotExist`, `Gt`, `Lt`. An absent
label counts as not being in a set, so `NotIn` is satisfied by a node that lacks the
key entirely, which is how the scheduler treats it.

The output gives a per-node breakdown showing which requirement failed and what the
node's actual label value was, followed by a cluster-wide summary. A `nodeSelector`
label that no node carries is reported separately, because scaling will never satisfy
it. Something has to be labelled.

## What is not evaluated

`preferredDuringSchedulingIgnoredDuringExecution` is ignored. Soft affinity never
blocks scheduling, so including it would only add noise.

`matchFields` selects on node fields such as `metadata.name`, not on labels. A term
containing one is reported as undecidable instead of being dropped. Dropping it would
narrow the OR and could report a schedulable pod as stuck, so a pod whose only terms
are undecidable is a warning rather than a hard finding.

Anything that is not a label. Taints and tolerations, resource requests, topology
spread constraints and volume topology can all keep a pod `Pending`. The report states
this, so a clean result is not read as "scheduling is fine".

A pod already assigned to a node is flagged as such, since its label requirements are
no longer what holds it up.

## Exit codes

`1` when no node can satisfy the requirements, `0` otherwise, which makes it usable
as a check in scripts. An undecidable verdict exits `0`, because the tool will not
claim a pod is stuck on evidence it does not have.

## Offline

Like the other tools it reads through `--dump`, so you can ask why a pod was
`Pending` from a must-gather after the pod is gone. The dump has to include
cluster-scoped nodes. Without them no verdict is possible, and the report says that
instead of concluding anything.
