# kubectl-toolbox

[![CI](https://github.com/mjovanovic0/kubectl-toolbox/actions/workflows/ci.yml/badge.svg)](https://github.com/mjovanovic0/kubectl-toolbox/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/mjovanovic0/kubectl-toolbox?sort=semver)](https://github.com/mjovanovic0/kubectl-toolbox/releases/latest)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

Debugging tools for OpenShift and Kubernetes in one kubectl plugin.

| Tool | Answers | Docs |
| --- | --- | --- |
| `vmsnapshot` | where a KubeVirt VM's disks and snapshots live, down to the storage array | [docs](docs/vmsnapshot.md) |
| `nodecheck` | which of a pod's scheduling label requirements no node satisfies | [docs](docs/nodecheck.md) |

## Install

Download the archive for your platform from the
[latest release](https://github.com/mjovanovic0/kubectl-toolbox/releases/latest):

```sh
tar -xzf kubectl-toolbox_v0.3.0_linux_amd64.tar.gz   # or a darwin/windows asset
install -m 0755 kubectl-toolbox ~/bin/               # any directory on your PATH
oc toolbox vmsnapshot vm <vm-name> -n <namespace>
```

`oc` and `kubectl` both discover the plugin from the single `kubectl-toolbox` binary
on PATH, so `oc toolbox ...`, `kubectl toolbox ...` and `kubectl-toolbox ...` are
equivalent. Check a download against `checksums.txt` from the same release.

From source, with Go 1.26 or newer (required by the Kubernetes client libraries):

```sh
go install github.com/mjovanovic0/kubectl-toolbox@latest
```

### On a host you can only paste text into

Some clusters are administered from a host with no file transfer and no outbound
route. `make text` packages the binary as one self-extracting shell script: gzipped,
base64-encoded, with its SHA-256 embedded. Every release publishes one as
`kubectl-toolbox-linux-amd64.sh`.

```sh
sh kubectl-toolbox-linux-amd64.sh    # writes ./kubectl-toolbox and checks the hash
```

It decodes to a staging path and moves the binary into place only once the checksum
matches. A truncated paste fails with a clear error and leaves nothing runnable.

## Tools

### vmsnapshot

```sh
oc toolbox vmsnapshot vm <vm-name> -n <namespace>   # disks and snapshots in one pass
oc toolbox vmsnapshot list [vm]                     # which snapshots exist and which look suspect
oc toolbox vmsnapshot snapshot <vm> <snapshot>      # one snapshot, through to the array
oc toolbox vmsnapshot disks <vm>                    # the live disk chain only
oc toolbox vmsnapshot pvc <name>                    # one PVC, down and back to its origin
oc toolbox vmsnapshot volumesnapshot <name>         # start from a name in CSI logs
```

It follows `VirtualMachineSnapshot` to its content, then to the `VolumeSnapshot`,
`VolumeSnapshotContent` and the array object, and separately `DataVolume` to `PVC`,
`PV` and the array volume. Each reference is checked, and a broken one is reported
with the reason that hop tends to break.

The report also includes what you would look up next: events, the live VMI and
virt-launcher state, storage class and snapshot class facts, volume attachments and
PVC consumers. See [docs/vmsnapshot.md](docs/vmsnapshot.md).

### nodecheck

```sh
oc toolbox nodecheck <pod> -n <namespace>    # alias: label-check
```

Reports which of a pod's `nodeSelector` and required `nodeAffinity` requirements no
node satisfies, which is what keeps it `Pending`. Only labels are considered. Taints,
resource requests and topology constraints also block scheduling, and the report says
so. See [docs/nodecheck.md](docs/nodecheck.md).

## Shared behaviour

The standard `kubectl` flags (`-n`, `--context`, `--kubeconfig`) work with every
tool, as do the following.

### Offline and post-mortem

`--dump <dir>` reads from a directory of YAML instead of a cluster. Use it to
investigate objects that are already deleted, or when you have no cluster access. It
accepts an `oc adm must-gather` tree or a hand-rolled capture, and unwraps both
`kind: List` wrappers and multi-document files.

```sh
oc adm must-gather --image=registry.redhat.io/container-native-virtualization/cnv-must-gather-rhel9
oc toolbox vmsnapshot vm win-db-01 --dump ./must-gather.local.123 -n virt-prod
```

### Exit codes

| Code | Meaning |
| --- | --- |
| 0 | resolved, with at most warnings |
| 1 | something is missing or errored, which is a successful diagnosis |
| 2 | the tool itself failed, for example a bad kubeconfig or unreadable dump |

Use `--no-color` in scripts and CI.

### Staying up to date

```sh
oc toolbox version [--check]    # build stamp and install path, optionally query GitHub
oc toolbox update [--yes]       # download the newest release and replace this binary
```

`update` checks the download against the release's `checksums.txt`, then replaces the
binary in one step: the new file is staged next to the old one, run once to confirm it
works, and renamed into place. A failed or interrupted update leaves the working
binary alone.

Every command also starts a background check and prints a one-line notice on stderr
after the output when a newer release exists. Using stderr keeps it out of the tree
and out of pipes. The result is cached for 12 hours, the check gives up after 3
seconds, and local builds skip it. On a host with no route to GitHub it fails
silently. To turn it off:

```sh
export TOOLBOX_NO_UPDATE_CHECK=1
```

## Contributing

[CONTRIBUTING.md](CONTRIBUTING.md) covers the commit conventions, the release
process and the rules new checks follow. `make test` runs the whole suite against
fixtures under `testdata/`, so no cluster is needed. Notable changes are listed in
[CHANGELOG.md](CHANGELOG.md).

## Licence

[Apache-2.0](LICENSE). Copyright 2026 Milan Jovanović.

`kubectl-toolbox` is an independent tool, not affiliated with or endorsed by Red Hat,
the KubeVirt project, Pure Storage or Portworx. Those names appear only to describe
the APIs and resources it reads.
