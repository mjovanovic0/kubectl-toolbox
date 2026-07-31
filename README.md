# kubectl-toolbox

[![CI](https://github.com/mjovanovic0/kubectl-toolbox/actions/workflows/ci.yml/badge.svg)](https://github.com/mjovanovic0/kubectl-toolbox/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/mjovanovic0/kubectl-toolbox?sort=semver)](https://github.com/mjovanovic0/kubectl-toolbox/releases/latest)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

Debugging tools for OpenShift and Kubernetes, in one kubectl plugin. Each tool
answers a question you would otherwise chase by hand across a dozen `oc get`
commands, and each says plainly what it could *not* determine.

| Tool | Answers | Docs |
| --- | --- | --- |
| `vmsnapshot` | where a KubeVirt VM's disks and snapshots actually live, down to the storage array | [docs](docs/vmsnapshot.md) |
| `nodecheck` | which of a pod's scheduling label requirements no node satisfies | [docs](docs/nodecheck.md) |

## Install

Download the archive for your platform from the
[latest release](https://github.com/mjovanovic0/kubectl-toolbox/releases/latest):

```sh
tar -xzf kubectl-toolbox_v0.3.0_linux_amd64.tar.gz   # or a darwin/windows asset
install -m 0755 kubectl-toolbox ~/bin/               # any directory on your PATH
oc toolbox vmsnapshot vm <vm-name> -n <namespace>
```

Both `oc` and `kubectl` discover the plugin from the single `kubectl-toolbox`
binary on PATH, so `oc toolbox …`, `kubectl toolbox …` and `kubectl-toolbox …` are
equivalent. Verify a download against `checksums.txt` from the same release.

From source (needs Go 1.26 or newer, which the Kubernetes client libraries
require):

```sh
go install github.com/mjovanovic0/kubectl-toolbox@latest
```

### On a host you can only paste text into

Some clusters are administered from a host with no file transfer and no route out.
`make text` packages the binary as a single self-extracting shell script — gzipped,
base64-encoded, with its SHA-256 embedded — and every release publishes one as
`kubectl-toolbox-linux-amd64.sh`.

```sh
sh kubectl-toolbox-linux-amd64.sh    # writes ./kubectl-toolbox, verifies the checksum
```

It decodes to a staging path and only moves the binary into place once the checksum
matches, so a truncated paste fails loudly and leaves nothing runnable behind.

## Tools

### vmsnapshot

```sh
oc toolbox vmsnapshot vm <vm-name> -n <namespace>       # disks and snapshots, in one pass
oc toolbox vmsnapshot list [vm]                         # which snapshots exist, and which look suspect
oc toolbox vmsnapshot snapshot <vm> <snapshot>          # one snapshot, all the way to the array
oc toolbox vmsnapshot disks <vm>                        # the live disk chain only
oc toolbox vmsnapshot pvc <name>                        # one PVC, down and back to its origin
oc toolbox vmsnapshot volumesnapshot <name>             # start from a name in CSI logs
```

Resolves `VirtualMachineSnapshot` → content → `VolumeSnapshot` → `VolumeSnapshotContent`
→ the array object, and `DataVolume` → `PVC` → `PV` → the array volume. Every hop is
verified, and a broken one is reported with the reason that hop usually breaks. It
also pulls in the things you would check next: events, the live VMI and
virt-launcher state, storage-class and snapshot-class facts, volume attachments and
PVC consumers. See [docs/vmsnapshot.md](docs/vmsnapshot.md).

### nodecheck

```sh
oc toolbox nodecheck <pod> -n <namespace>    # alias: label-check
```

Reports which of a pod's `nodeSelector` and required `nodeAffinity` requirements no
node satisfies — the ones that keep it `Pending`. Only labels are considered;
the report says so, because taints, resource requests and topology can equally
block scheduling. See [docs/nodecheck.md](docs/nodecheck.md).

## Shared behaviour

Standard `kubectl` flags (`-n`, `--context`, `--kubeconfig`) apply to every tool,
as do these.

### Offline and post-mortem

`--dump <dir>` resolves from a directory of YAML instead of the cluster, so you can
investigate a chain whose objects are already deleted, or work with no cluster
access at all. It reads an `oc adm must-gather` tree, or a hand-rolled capture —
`kind: List` wrappers and multi-document files are both unwrapped.

```sh
oc adm must-gather --image=registry.redhat.io/container-native-virtualization/cnv-must-gather-rhel9
oc toolbox vmsnapshot vm win-db-01 --dump ./must-gather.local.123 -n virt-prod
```

### Exit codes

| Code | Meaning |
| --- | --- |
| 0 | resolved; at most warnings |
| 1 | something is missing or errored — a successful diagnosis |
| 2 | the tool itself failed (bad kubeconfig, unreadable dump) |

Use `--no-color` in scripts and CI.

### Staying up to date

```sh
oc toolbox version [--check]    # build stamp, install path, optionally query GitHub
oc toolbox update [--yes]       # download the newest release and replace this binary
```

`update` verifies the download against the release's `checksums.txt`, then replaces
the binary atomically: the new one is staged alongside the old, executed once to
prove it runs, and moved into place. A failed or interrupted update leaves the
working binary untouched.

Every command also starts a background check and prints a one-line notice on
**stderr** after the output if a newer release exists — stderr so it can never
interleave with the tree or corrupt a pipe. It is cached for 12 hours, gives up
after 3 seconds, and is skipped for local builds. On a host with no route to GitHub
it fails silently. To disable it entirely:

```sh
export TOOLBOX_NO_UPDATE_CHECK=1
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the commit conventions, the release
process, and the design rules new checks are expected to follow. `make test` runs
the whole suite against fixtures in `testdata/`, so no cluster is needed. Notable
changes are in [CHANGELOG.md](CHANGELOG.md).

## Licence

[Apache-2.0](LICENSE). Copyright 2026 Milan Jovanović.

`kubectl-toolbox` is an independent tool. It is not affiliated with or endorsed by
Red Hat, the KubeVirt project, Pure Storage or Portworx; those names are used only
to describe the APIs and resources it reads.
