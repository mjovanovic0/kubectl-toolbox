# vmsnapshot

Resolve the reference chain behind a KubeVirt / OpenShift Virtualization VM's
snapshots and disks, down to the objects you can look up on the storage array.
Built for debugging missing PVCs and missing snapshots.

```sh
oc toolbox vmsnapshot vm <vm-name> -n <namespace>
```

`vm` is the usual entry point: it resolves both chains in one pass and merges every
finding into a single problem list. `snapshot`, `disks`, `list`, `pvc` and
`volumesnapshot` narrow the scope when you already know what you are chasing.

## The two chains

Snapshot chain:

| Object | Reference followed |
| --- | --- |
| `VirtualMachine` | `VirtualMachineSnapshot.spec.source` points back at it |
| `VirtualMachineSnapshot` | `status.virtualMachineSnapshotContentName` |
| `VirtualMachineSnapshotContent` | `spec.volumeBackups[].volumeSnapshotName` |
| `VolumeSnapshot` | `status.boundVolumeSnapshotContentName` |
| `VolumeSnapshotContent` | `status.snapshotHandle` |
| `PureSnapshot` | matched on the CSI handle |

Disk chain:

| Object | Reference followed |
| --- | --- |
| `DataImportCron` | `spec.managedDataSource` |
| `DataSource` | `status.source.pvc` / `status.source.snapshot` |
| `DataVolume` | `spec.sourceRef`, `spec.source`, `status.claimName` |
| `PersistentVolumeClaim` | `spec.volumeName`, `spec.dataSourceRef` |
| `PersistentVolume` | `spec.csi.volumeHandle` |
| `PureVolume` | matched on the CSI handle |

The two interlink rather than running in parallel: a `DataVolume` can be sourced
from a `VolumeSnapshot`, and a restored `PersistentVolumeClaim` records its origin
snapshot in `spec.dataSourceRef`. Both links are followed. CDI volume populators
(`VolumeCloneSource` and friends) are resolved to the object they wrap, so a
populated PVC is not reported as disagreeing with its own DataVolume.

`spec.volumeBackups[]` matters more than it looks: it carries the VM disk name, the
VolumeSnapshot name, *and* the captured spec of the source PVC. Once that PVC is
deleted, this is the only record of its size and storage class.

## Options

| Flag | Effect |
| --- | --- |
| `--latest N` | on `vm`, resolve only the N newest snapshots (the total is still reported) |
| `--no-disks` / `--no-snapshots` | on `vm`, one half only |
| `--no-runtime` | skip the live VMI and virt-launcher view |
| `--events` / `--no-events` | attach events everywhere / not at all |
| `--timeline` | add a chronological view of every dated fact found |
| `--owners` | walk `ownerReferences` upward to find what created a snapshot |
| `--trim-repeats` | collapse a sub-chain already shown in full earlier |
| `--no-array` | stop at the CSI handle |
| `--array-snapshot-kind` / `--array-volume-kind` | use a different vendor CR |

## What it surfaces beyond following references

**Kubernetes events**, attached to any object already flagged, because they say
*why* rather than *what*. The newest warning is promoted into the problem list —
a CSI driver's own account of a failure is usually the diagnosis.

**The live runtime view.** `VirtualMachineInstance.status.volumeStatus` is the
authority on what is actually attached, with a per-disk phase and reason; the
`virt-launcher` pod underneath is where mount and scheduling failures surface.
Neither is reachable from the spec-side chain, and both are where you look when
every storage object appears healthy but the VM will not run. A failed live
migration is reported too, since it leaves volumes attached to the wrong node.

**Storage-class facts instead of guesses.** `volumeBindingMode` is read, so a
Pending PVC is explained correctly: under `WaitForFirstConsumer` Pending is
expected, under `Immediate` it means provisioning is failing. Whether a
`VolumeSnapshotClass` exists for the provisioner is checked, which definitively
answers why a snapshot silently skipped a disk — reported as `CONFIRMED cause` or
`CONFIRMED NOT the cause`.

**Where the volume is attached.** `VolumeAttachment` gives the node holding each PV
and any attach/detach error, which is what produces `Multi-Attach` failures and what
you correlate against array-side host connections.

**Who else holds the PVC** — every mounting pod with phase and node, flagging more
than one consumer of a `ReadWriteOnce` volume, and a PVC terminating while still
mounted along with the finalizers holding it.

**Consistency checks** that traversal alone would miss:

- `spec.volumeBackups[]` against `status.volumeSnapshotStatus[]`, in both
  directions — a stale status entry and an un-snapshotted disk look nothing alike.
- Crossed back-references: `VolumeSnapshotContent.spec.volumeSnapshotRef` and
  `PersistentVolume.spec.claimRef` must point back at whatever claims them.
- Whether each captured source PVC still exists — compared by creation time, not
  just by name, because an `InPlace` restore reuses the name for a new volume.
- `dataVolumeTemplates` declared by the VM with no corresponding `DataVolume`.
- A snapshot larger than the PVC it is being restored into, which cannot bind.
- Restored PVCs that are missing, or orphaned by a restore that never completed.

Provenance staleness is deliberately *not* a failure: a `Bound` PVC whose origin
golden-image snapshot has rotated away is normal, and is reported as lineage rather
than driving a non-zero exit.

## Storage-array CRs

The array layer defaults to Pure/Portworx CSI (`PureSnapshot`, `PureVolume`).
Portworx documents these CRs' fields but not their API group, so the group is
resolved from the Kind through API discovery rather than hardcoded — which is also
why other vendors' CRs work by passing a different kind.

Because CSI does not specify how a handle maps to a vendor object, the CR is located
by matching the handle against several candidate fields (`metadata.name`,
`spec.snapshotName`, `spec.snapshotID`, and for volumes `spec.volumeName`,
`spec.volumeID`, `spec.serial`), trying the whole handle and each trailing segment
after `::`, `:` and `/`. The matched field is reported as `matchedOn=` so the result
is checkable, and a failed match says how many objects were scanned rather than
silently showing nothing. The CSI handle is always printed even when no CR matches —
that string is what you search for on the array.

**A snapshot can be unrestorable while Kubernetes still reports it ready.** On
FlashArray a snapshot is a child of its source volume: destroying the volume
destroys its snapshots, and after the eradication window they are gone. Nothing
re-verifies `readyToUse`, so it stays `true` forever. Where the driver encodes the
parent in its snapshot name, that parent is checked and its absence reported; where
it does not, the tool says the name cannot be used rather than staying silent, and
points at the origin-volume lookup instead.

## What sits above a VirtualMachineSnapshot

Unlike `DataVolume` → `PersistentVolumeClaim`, a `VirtualMachineSnapshot` has no
single fixed parent kind. It may be created directly, by a `VirtualMachineClone`
(which records it in `status.snapshotName` and is built on Snapshot+Restore), or by
backup tooling such as OADP/Velero via the KubeVirt plugin. `--owners` walks
`metadata.ownerReferences` upward and also scans `VirtualMachineClone` status, so
whatever produced the snapshot is found without enumerating kinds.

`VirtualMachineRestore` objects consuming a snapshot are always listed, since a
restore is what creates new PVCs and is therefore usually the thing that failed.
Its `volumeRestorePolicy` is shown because `InPlace` is what explains a reused PVC
name.
