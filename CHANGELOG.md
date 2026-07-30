# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project adheres to
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Fixed

- The array-side parent-volume check silently did nothing when a Portworx snapshot
  name carried no parent (`px_<cluster>-snapshot-<uuid>` rather than
  `<volume>.<suffix>`). Silence read as "checked and fine"; it now states that the
  name cannot be used and points at the origin-volume lookup instead.
- CDI volume populators are resolved to the object they wrap, so a PVC populated
  through a `VolumeCloneSource` is no longer reported as disagreeing with its
  DataVolume when both name the same source. A genuine disagreement is still
  reported.
- `source PVC still exists` compared names only, so after an `InPlace` restore --
  which recreates the PVC under the same name -- a different, newer volume was
  reported as the captured disk. Creation time is now compared against the
  snapshot.
- `spec.volumeRestorePolicy` is shown on a restore, since `InPlace` is what
  explains a reused PVC name.

### Changed

- Upgraded the Kubernetes client libraries to 0.36.3, `spf13/cobra` to 1.10.2 and
  `sigs.k8s.io/yaml` to 1.6.0. The Kubernetes libraries require Go 1.26, so the
  `go` directive moved from 1.24 to 1.26.0 and building from source now needs that
  toolchain. No API changes were needed and behaviour is unchanged.
- CI now uses `actions/checkout@v7` and `actions/setup-go@v7`.

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
