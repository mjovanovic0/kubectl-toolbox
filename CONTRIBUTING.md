# Contributing

## Commit messages

This repository uses [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/).
The commit history is the source of the release notes, so the subject line should
read as something a user of the tool would recognise.

```
<type>[optional scope][!]: <description>

[optional body]

[optional footer]
```

Types in use:

| Type | Meaning | Version effect |
| --- | --- | --- |
| `feat` | new capability the user can see | MINOR |
| `fix` | corrected behaviour | PATCH |
| `perf` | faster or fewer API calls, same behaviour | PATCH |
| `refactor` | internal change, no behaviour change | none |
| `test` | tests only | none |
| `docs` | documentation only | none |
| `ci` | workflows and release tooling | none |
| `build` | build system, dependencies | none |
| `chore` | everything else | none |

A breaking change is marked with `!` after the type (`feat!:`) or a
`BREAKING CHANGE:` footer, and forces a MAJOR bump.

Useful scopes: `snapshot`, `disks`, `array`, `events`, `runtime`, `source`, `tree`,
`cli`, `update`, `version`.

A new tool in the toolbox is a `feat` with its own scope, added as a subcommand of
the root so the top-level help stays a list of nouns rather than loose verbs.

Examples from this project's history:

```
fix(snapshot): read persistentVolumeClaim, not persistentVolumeClaimName
feat(events): promote CSI failure events to the problem list
fix(array): stop claiming a chain was shown elsewhere when it was not
```

## Versioning

[Semantic Versioning](https://semver.org/). While the major version is `0` the
public surface is the CLI — command names, flags and exit codes — and it may still
change between minor versions.

Exit codes are part of that surface and are relied upon by scripts:

| Code | Meaning |
| --- | --- |
| 0 | chain resolved; at most warnings |
| 1 | a reference is missing or errored |
| 2 | the tool itself failed |

## Releasing

Releases are cut by pushing a semver tag; the `Release` workflow builds every
platform, attaches the archives and checksums, and publishes the GitHub release.

```sh
git tag -a v0.1.0 -m "release: v0.1.0"
git push origin v0.1.0
```

The workflow re-runs `gofmt`, `go vet` and the full test suite before it publishes,
so a tag on a broken tree fails instead of shipping.

## Development

Go 1.26 or newer, as required by the Kubernetes client libraries; the `go`
directive in `go.mod` is the source of truth and CI reads it via
`setup-go`'s `go-version-file`.

```sh
make test     # runs entirely against testdata fixtures; no cluster needed
make vet
make build
make text     # the pasteable self-extracting installer
```

### Testing without a cluster

Every behaviour is pinned by a fixture under `testdata/`, resolved through the
same code path as a live cluster via `--dump`:

| Fixture | Covers |
| --- | --- |
| `fixture` | a healthy chain plus a dangling VolumeSnapshot and an un-snapshottable disk |
| `broken` | inconsistent back-references and an unmatched array handle |
| `prodshape` | a real Portworx/FlashArray topology: restore-created disks, a failed restore, a stray PVC |
| `provisionfail` | a stalled restore whose CSI provisioning fails because the array-side snapshot is gone |
| `restorepending` | an incomplete restore that has not yet raised a Failure condition |

When fixing a bug, add the shape that produced it to a fixture first. Several
fixtures here exist because a real cluster disagreed with an assumption in the
code, and the fixture is what stops it regressing.

## Adding checks

The tool is used when something is already broken, so a new check should:

- **State facts, not guesses.** If the answer is readable from the API, read it.
  Hints that offer two possibilities send the reader down the wrong path half the
  time.
- **Distinguish "broken" from "not what you expected".** Provenance that has
  legitimately rotated away is lineage, not a fault, and must not drive a non-zero
  exit.
- **Say what was checked when a lookup fails**, so a negative result is
  falsifiable rather than merely empty.
- **Escalate, never downgrade.** Use `Node.Escalate`, not `SetState`, when
  flagging a fault, so a milder later finding cannot mask an error.
