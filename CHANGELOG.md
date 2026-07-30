# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project adheres to
[Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.0.1] - 2026-07-30

First public release. Resolves KubeVirt / OpenShift Virtualization snapshot and
disk reference chains down to storage-array objects, for debugging missing PVCs and
missing snapshots.

### Added

- `vm` resolves both chains for one VM in a single pass, and is the usual entry
  point. `snapshot`, `disks`, `pvc`, `volumesnapshot` and `list` narrow the scope.
- Snapshot chain: `VirtualMachine` → `VirtualMachineSnapshot` →
  `VirtualMachineSnapshotContent` → `VolumeSnapshot` → `VolumeSnapshotContent` →
  vendor array CR, including the `spec.volumeBackups[]` entry that preserves the
  source PVC spec after the PVC itself is gone.
- Disk chain: `DataImportCron` → `DataSource` → `DataVolume` →
  `PersistentVolumeClaim` → `PersistentVolume` → vendor array CR, with import
  provenance walked upward.
- Storage-array resolution for Pure/Portworx (`PureSnapshot`, `PureVolume`) by
  matching the CSI handle against several candidate fields, reporting `matchedOn=`
  so the match is checkable. The API group is discovered from the Kind rather than
  hardcoded, so other vendors work via `--array-snapshot-kind` /
  `--array-volume-kind`.
- Reference validation beyond traversal: crossed `volumeSnapshotRef` and
  `claimRef`, `volumeBackups[]` versus `volumeSnapshotStatus[]` in both directions,
  `dataVolumeTemplates` with no `DataVolume`, and restored PVCs that are missing or
  orphaned.
- Kubernetes events, attached to objects already flagged as unhealthy, with the
  newest warning promoted into the problem list. `--events` attaches them
  everywhere; `--no-events` skips the lookup.
- Live runtime view: `VirtualMachineInstance` per-disk `volumeStatus`, the
  `virt-launcher` pod, and live-migration state. `--no-runtime` skips it.
- Verified storage-class facts: `volumeBindingMode` is read to explain a Pending
  PVC correctly, and `VolumeSnapshotClass` support is checked to confirm or rule out
  why a disk was never snapshotted.
- `VolumeAttachment` state per PV, PVC consumers with multi-attach detection, and
  capacity-consistency checks along a restore path.
- Detection of a snapshot whose array-side parent volume no longer exists, which on
  FlashArray means the snapshot is unrestorable despite `readyToUse=true`.
- `--timeline` for a chronological view of every dated fact found.
- Offline mode via `--dump`, resolving from an `oc adm must-gather` tree or an
  `oc get -o yaml` capture, including `kind: List` wrappers.
- `--trim-repeats` to collapse a sub-chain already rendered earlier in the report.
  Off by default.
- `make text` packages the binary as a self-extracting, checksum-verified shell
  script for hosts reachable only by pasting text into a terminal.

[Unreleased]: https://github.com/mjovanovic0/kubectl-vmsnap/compare/v0.0.1...HEAD
[0.0.1]: https://github.com/mjovanovic0/kubectl-vmsnap/releases/tag/v0.0.1
