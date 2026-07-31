# vmsnapshot

Resolves the reference chain behind a KubeVirt / OpenShift Virtualization VM's
snapshots and disks, down to the objects you can look up on the storage array. Aimed
at debugging missing PVCs and missing snapshots.

```sh
oc toolbox vmsnapshot vm <vm-name> -n <namespace>
```

`vm` is the usual starting point. It resolves both chains in one pass and merges
every finding into a single problem list. Use `snapshot`, `disks`, `list`, `pvc` or
`volumesnapshot` when you already know what you are chasing.

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

The two interlink rather than running in parallel. A `DataVolume` can be sourced from
a `VolumeSnapshot`, and a restored `PersistentVolumeClaim` records its origin
snapshot in `spec.dataSourceRef`. Both links are followed. CDI volume populators such
as `VolumeCloneSource` are resolved to the object they wrap, so a populated PVC is not
reported as disagreeing with its own DataVolume.

`spec.volumeBackups[]` carries more than it appears to: the VM disk name, the
VolumeSnapshot name, and the captured spec of the source PVC. Once that PVC is
deleted, this is the only remaining record of its size and storage class.

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

## Beyond following references

**Kubernetes events** are attached to any object already flagged, since they explain
why rather than what. The newest warning is promoted into the problem list, because a
CSI driver's own account of a failure is usually the diagnosis.

**The live runtime view.** `VirtualMachineInstance.status.volumeStatus` reports what
is actually attached, with a per-disk phase and reason. The `virt-launcher` pod
underneath it is where mount and scheduling failures appear. Neither is reachable
from the spec side of the chain, and both matter when every storage object looks
healthy but the VM will not run. A failed live migration is reported too, since it
can leave volumes attached to the wrong node.

**Storage class facts rather than assumptions.** `volumeBindingMode` is read, so a
Pending PVC gets the right explanation: under `WaitForFirstConsumer` Pending is
expected, under `Immediate` it means provisioning is failing. Whether a
`VolumeSnapshotClass` exists for the provisioner is checked, which answers why a
snapshot skipped a disk. That is reported as `CONFIRMED cause` or `CONFIRMED NOT the
cause`.

**Where the volume is attached.** `VolumeAttachment` gives the node holding each PV
and any attach or detach error. This is the source of `Multi-Attach` failures and
what you correlate against array-side host connections.

**Who else holds the PVC.** Every mounting pod with its phase and node. More than one
consumer of a `ReadWriteOnce` volume is flagged, as is a PVC terminating while still
mounted, along with the finalizers holding it.

**Consistency checks** that plain traversal would miss:

- `spec.volumeBackups[]` against `status.volumeSnapshotStatus[]`, in both directions.
  A stale status entry and an un-snapshotted disk look nothing alike.
- Crossed back-references. `VolumeSnapshotContent.spec.volumeSnapshotRef` and
  `PersistentVolume.spec.claimRef` must point back at whatever claims them.
- Whether each captured source PVC still exists, compared by creation time rather
  than name alone, because an `InPlace` restore reuses the name for a new volume.
- `dataVolumeTemplates` declared by the VM with no corresponding `DataVolume`.
- A snapshot larger than the PVC it is being restored into, which cannot bind.
- Restored PVCs that are missing, or orphaned by a restore that never completed.

Stale provenance is not treated as a failure. A `Bound` PVC whose origin golden-image
snapshot has rotated away is normal, and is reported as lineage instead of driving a
non-zero exit.

## Storage array CRs

The array layer defaults to Pure/Portworx CSI (`PureSnapshot`, `PureVolume`).
Portworx documents these CRs' fields but not their API group, so the group is resolved
from the Kind through API discovery instead of being hardcoded. That is also why
other vendors' CRs work by passing a different kind.

CSI does not specify how a handle maps to a vendor object, so the CR is located by
matching the handle against several candidate fields: `metadata.name`,
`spec.snapshotName`, `spec.snapshotID`, and for volumes `spec.volumeName`,
`spec.volumeID` and `spec.serial`. It tries the whole handle and each trailing
segment after `::`, `:` and `/`. The matched field is reported as `matchedOn=` so the
result can be checked, and a failed match reports how many objects were scanned
rather than showing nothing. The CSI handle is always printed even when no CR
matches, since that string is what you search for on the array.

A snapshot can be unrestorable while Kubernetes still reports it ready. On FlashArray
a snapshot is a child of its source volume: destroying the volume destroys its
snapshots, and after the eradication window they are gone. Nothing re-verifies
`readyToUse`, so it stays `true` indefinitely. Where the driver encodes the parent in
its snapshot name, that parent is checked and its absence reported. Where it does
not, the tool says the name cannot be used and points at the origin volume lookup
instead of staying silent.

## What sits above a VirtualMachineSnapshot

Unlike `DataVolume` to `PersistentVolumeClaim`, a `VirtualMachineSnapshot` has no
single fixed parent kind. It may be created directly, by a `VirtualMachineClone`
(which records it in `status.snapshotName` and is built on Snapshot and Restore), or
by backup tooling such as OADP/Velero via the KubeVirt plugin. `--owners` walks
`metadata.ownerReferences` upward and also scans `VirtualMachineClone` status, so
whatever produced the snapshot is found without enumerating kinds.

`VirtualMachineRestore` objects consuming a snapshot are always listed, since a
restore creates new PVCs and is therefore usually the thing that failed. Its
`volumeRestorePolicy` is shown because `InPlace` explains a reused PVC name.
