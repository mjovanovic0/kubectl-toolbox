# kubectl-vmsnap

[![CI](https://github.com/mjovanovic0/kubectl-vmsnap/actions/workflows/ci.yml/badge.svg)](https://github.com/mjovanovic0/kubectl-vmsnap/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/mjovanovic0/kubectl-vmsnap?sort=semver)](https://github.com/mjovanovic0/kubectl-vmsnap/releases/latest)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

Resolve the full reference chain behind a KubeVirt / OpenShift Virtualization VM's
snapshots and disks, down to the objects you can actually look up on the storage
array. Built for debugging missing PVCs and missing snapshots.

Every hop is verified. A broken reference is reported with the reason that hop
usually breaks, and the process exits non-zero.

## Quick start

Download the archive for your platform from the
[latest release](https://github.com/mjovanovic0/kubectl-vmsnap/releases/latest):

```sh
tar -xzf kubectl-vmsnap_v0.0.1_linux_amd64.tar.gz    # or a darwin/windows asset
install -m 0755 kubectl-vmsnap ~/bin/                # any directory on your PATH
oc vmsnap vm <vm-name> -n <namespace>
```

Or build from source (needs Go 1.26 or newer, which the Kubernetes client
libraries require):

```sh
go install github.com/mjovanovic0/kubectl-vmsnap@latest
# or, from a clone:
make install PREFIX=~/bin
```

Verify a download against `checksums.txt` from the same release.

`oc vmsnap vm <vm-name>` is the main entry point: it walks the VM's disks down to
their PVCs/PVs and the array, then resolves every snapshot of that VM down to the
array, and merges all findings into one problem list.

The plugin is discovered by both `oc` and `kubectl` from the single
`kubectl-vmsnap` binary — `oc` looks for `kubectl-*` as well as `oc-*`, so
`oc vmsnap ...`, `kubectl vmsnap ...` and `kubectl-vmsnap ...` are equivalent.

## The two chains

Snapshot chain — `kubectl vmsnap snapshot <vm> <snapshot>`:

| Object | Reference followed |
| --- | --- |
| `VirtualMachine` | `VirtualMachineSnapshot.spec.source` points back at it |
| `VirtualMachineSnapshot` | `status.virtualMachineSnapshotContentName` |
| `VirtualMachineSnapshotContent` | `spec.volumeBackups[].volumeSnapshotName` |
| `VolumeSnapshot` | `status.boundVolumeSnapshotContentName` |
| `VolumeSnapshotContent` | `status.snapshotHandle` |
| `PureSnapshot` | matched on the CSI handle |

Disk chain — `kubectl vmsnap disks <vm>`:

| Object | Reference followed |
| --- | --- |
| `DataImportCron` | `spec.managedDataSource` |
| `DataSource` | `status.source.pvc` / `status.source.snapshot` |
| `DataVolume` | `spec.sourceRef`, `spec.source`, `status.claimName` |
| `PersistentVolumeClaim` | `spec.volumeName`, `spec.dataSourceRef` |
| `PersistentVolume` | `spec.csi.volumeHandle` |
| `PureVolume` | matched on the CSI handle |

The two chains interlink: a `DataVolume` can be sourced from a `VolumeSnapshot`,
and a restored `PersistentVolumeClaim` records its origin snapshot in
`spec.dataSourceRef`. Both links are followed.

## Install

```sh
make install PREFIX=~/bin      # anywhere on PATH
oc plugin list                 # should show kubectl-vmsnap
```

Cross-compile release binaries with `make cross`.

### Installing on a host you can only paste text into

`make text` packages the plugin as a single self-extracting shell script — the
binary, gzipped and base64-encoded, with its sha256 embedded — for hosts reachable
only by copying text into a terminal.

```sh
make text                     # -> dist/kubectl-vmsnap-linux-amd64.sh  (~16 MiB of text)
make text TEXT_ARCH=arm64
make text COMP=xz             # ~33% less text, needs unxz on the host
make text CHUNK=4             # also split the payload into 4 MiB .b64 parts
make text-all                 # linux amd64 + arm64
```

Then on the host:

```sh
sh kubectl-vmsnap-linux-amd64.sh          # writes ./kubectl-vmsnap, verifies sha256
install -m 0755 kubectl-vmsnap ~/bin/
oc vmsnap vm <name> -n <namespace>
```

The script decodes to a staging path and only moves the binary into place after
the checksum matches, so a truncated or mangled paste fails loudly and leaves
nothing runnable behind. `make text` verifies its own round-trip before reporting
success. If the paste is too large for one go, `CHUNK=<MiB>` also emits numbered
parts to reassemble with
`cat *.part-* | base64 -d | gunzip > kubectl-vmsnap`.

## Usage

```sh
# everything about one VM: disks and all its snapshots
oc vmsnap vm win-db-01 -n virt-prod

# a VM with a long snapshot history: only the newest few
oc vmsnap vm win-db-01 -n virt-prod --latest 3
oc vmsnap vm win-db-01 -n virt-prod --no-snapshots   # disks only
oc vmsnap vm win-db-01 -n virt-prod --no-disks       # snapshots only

# what snapshots exist, and which ones are already suspect
kubectl vmsnap list -n virt-prod
kubectl vmsnap list win-db-01 -n virt-prod

# narrow to one snapshot, or to the disks only
kubectl vmsnap snapshot win-db-01 snap-2026-07-20 -n virt-prod
kubectl vmsnap disks win-db-01 -n virt-prod

# start from a name you found in CSI or storage-operator logs
kubectl vmsnap volumesnapshot vmsnapshot-8f3a-vol-rootdisk -n virt-prod
kubectl vmsnap pvc win-db-01-root -n virt-prod
```

Standard `kubectl` flags (`-n`, `--context`, `--kubeconfig`) all apply.

Chains are always resolved in full, including where the same VolumeSnapshot is
reached from more than one branch — a branch you are following should never dead-end
in a pointer to somewhere else in the output. When that repetition is more than you
want, `--trim-repeats` collapses any sub-chain already rendered earlier in the same
report, keeping the node and its attributes but omitting the repeated subtree. The
collapse is tracked against what was actually written out, so it never claims
something was "shown above" when it wasn't.

### Offline / post-mortem

Resolve from a directory of YAML instead of the cluster. This is how you
investigate a chain whose PVCs are already deleted, or work without cluster
access:

```sh
oc adm must-gather --image=registry.redhat.io/container-native-virtualization/cnv-must-gather-rhel9
kubectl vmsnap snapshot win-db-01 snap-2026-07-20 --dump ./must-gather.local.123 -n virt-prod
```

A hand-rolled capture works too — `kind: List` wrappers and multi-document files
are both unwrapped:

```sh
mkdir dump && cd dump
for k in vm vmsnapshot vmsnapshotcontent volumesnapshot dv pvc; do oc get $k -o yaml > $k.yaml; done
oc get volumesnapshotcontent,pv,puresnapshot,purevolume -o yaml > cluster.yaml
kubectl vmsnap snapshot win-db-01 snap-2026-07-20 --dump ./dump -n virt-prod
```

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | chain resolved; at most warnings |
| 1 | a reference is missing or errored — a successful diagnosis |
| 2 | the tool itself failed (bad kubeconfig, unreadable dump) |

Suitable for `--no-color` use in scripts and CI.

## Storage-array CRs

The array layer defaults to Pure/Portworx CSI (`PureSnapshot`, `PureVolume`).
Portworx documents these CRs' fields but not their API group, so the group is
resolved from the Kind through API discovery rather than hardcoded — which also
means other vendors' CRs work by passing a different kind:

```sh
kubectl vmsnap snapshot vm snap --array-snapshot-kind MyVendorSnapshot
kubectl vmsnap disks vm --array-volume-kind MyVendorVolume
kubectl vmsnap snapshot vm snap --no-array   # stop at the CSI handle
```

Because the handle-to-CR mapping is not specified by CSI, the object is located
by matching the handle against several candidate fields (`metadata.name`,
`spec.snapshotName`, `spec.snapshotID`, and for volumes `spec.volumeName`,
`spec.volumeID`, `spec.serial`), trying the whole handle and each trailing
segment after `::`, `:` and `/`. The matched field is reported as `matchedOn=`
so the result is checkable, and a failed match says how many objects were
scanned rather than silently showing nothing. The CSI handle is always printed
even when no CR matches — that string is what you search for on the array.

## What else it surfaces when something is broken

This is a tool you reach for *when there is a problem*, so it gathers the things
you would go and check by hand next.

**Kubernetes events.** The single most useful signal and the one that says *why*
rather than *what*. Attached automatically to any object the report has already
flagged, so a clean run stays readable. `--events` attaches them to everything;
`--no-events` skips the lookup entirely.

**The live runtime view.** The `VirtualMachineInstance` is the authority on what
the VM actually has attached right now: `status.volumeStatus` gives a per-disk
phase, target device and reason, which is where hotplug failures and unmountable
volumes appear. Underneath it the `virt-launcher` pod is where mount errors and
scheduling failures surface. Neither is reachable from the spec-side chain, and
both are where you look when every storage object appears healthy but the VM will
not run. A failed live migration is reported too, since it leaves volumes attached
to the wrong node. Skip with `--no-runtime`.

**Storage class facts instead of guesses.** The `volumeBindingMode` is read, not
assumed, so a Pending PVC is explained correctly: under `WaitForFirstConsumer`
Pending is expected, under `Immediate` it means provisioning is failing. Whether a
`VolumeSnapshotClass` exists for the provisioner is checked, which definitively
answers why a VM snapshot silently skipped a disk — reported as `CONFIRMED cause`
or `CONFIRMED NOT the cause`.

**Where the volume is attached.** `VolumeAttachment` gives the node holding each
PV and any attach/detach error — this is what produces `Multi-Attach` errors and
what you correlate against array-side host connections.

**Who else holds the PVC.** Every pod mounting it, with phase and node. More than
one consumer of a `ReadWriteOnce` volume is flagged, as is a PVC terminating while
still mounted, along with the finalizers holding it.

**Capacity consistency.** A snapshot larger than the PVC it is being restored into
cannot bind; that is caught here rather than surfacing later as an opaque CSI
error.

**`--timeline`.** A chronological view of every dated fact found, deduplicated
across branches. Establishing that a restore ran *before* a PVC vanished, or that a
snapshot predates a resize, is often the whole investigation.

## What it checks beyond following references

- `spec.volumeBackups[]` vs `status.volumeSnapshotStatus[]` consistency, in both
  directions — a stale status entry and an un-snapshotted disk look nothing alike.
- Whether each captured source PVC still exists. When it doesn't, the PVC spec
  recorded inside `volumeBackups[].persistentVolumeClaim` is the only record of
  its size and storage class, and it is printed.
- `status.snapshotVolumes.excludedVolumes` — the usual reason a disk was never
  snapshotted is that its storage class has no `VolumeSnapshotClass`.
- Crossed back-references: `VolumeSnapshotContent.spec.volumeSnapshotRef` and
  `PersistentVolume.spec.claimRef` are both verified to point back at the object
  that claims them.
- `dataVolumeTemplates` declared by the VM with no corresponding `DataVolume`.
- PVCs stuck terminating on a finalizer, and DataVolumes with importer restarts.

Provenance staleness is deliberately *not* treated as a failure: a `Bound` PVC
whose origin golden-image snapshot has rotated away is normal, and is reported as
lineage rather than driving a non-zero exit.

## What sits above a VirtualMachineSnapshot

Unlike `DataVolume` → `PersistentVolumeClaim`, a `VirtualMachineSnapshot` has no
single fixed parent kind. It may be created directly, by a `VirtualMachineClone`
(which records it in `status.snapshotName` and is built on Snapshot+Restore), or
by backup tooling such as OADP/Velero via the KubeVirt plugin. `--owners` walks
`metadata.ownerReferences` upward and also scans `VirtualMachineClone` status, so
whatever produced the snapshot is found without enumerating kinds.

`VirtualMachineRestore` objects consuming a snapshot are always listed, since a
restore is what creates new PVCs and is therefore usually the thing that failed.

## Development

```sh
make test    # runs against testdata/ fixtures, no cluster needed
make vet
```

`testdata/fixture` is a healthy chain with two seeded faults; `testdata/broken`
covers inconsistent back-references and an unmatched array handle.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). The project uses
[Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/) and
[Semantic Versioning](https://semver.org/); releases are cut by pushing a `vX.Y.Z`
tag. Notable changes are recorded in [CHANGELOG.md](CHANGELOG.md).

## Licence

[Apache-2.0](LICENSE). Copyright 2026 Milan Jovanović.

`kubectl-vmsnap` is an independent tool. It is not affiliated with or endorsed by
Red Hat, the KubeVirt project, Pure Storage or Portworx; those names are used only
to describe the APIs and resources it reads.
