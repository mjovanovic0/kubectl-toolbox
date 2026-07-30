#!/usr/bin/env bash
# Package the plugin as pasteable text: cross-build, compress, base64, and wrap
# the result in a self-extracting shell script that verifies its own checksum.
#
# The generated artifact is decoded and checksummed here before it is reported as
# ready, so a corrupt payload is never handed over.
#
# Env:
#   TEXT_OS    target GOOS   (default linux)
#   TEXT_ARCH  target GOARCH (default amd64)
#   COMP       gzip | xz     (default gzip; xz is ~33% smaller, needs unxz on the host)
#   CHUNK      split payload into N-MiB .b64 parts as well (default 0 = off)
#   VERSION    version string stamped into the binary
set -euo pipefail

TEXT_OS="${TEXT_OS:-linux}"
TEXT_ARCH="${TEXT_ARCH:-amd64}"
COMP="${COMP:-gzip}"
CHUNK="${CHUNK:-0}"
VERSION="${VERSION:-dev}"
MODULE=github.com/mjovanovic0/kubectl-vmstorage

case "$COMP" in
gzip) comp_cmd=(gzip -9 -c); decomp=gunzip ;;
xz) comp_cmd=(xz -9e -c); decomp=unxz ;;
*)
	echo "unsupported COMP=$COMP (use gzip or xz)" >&2
	exit 2
	;;
esac

target="${TEXT_OS}-${TEXT_ARCH}"
outdir=dist
bin="$outdir/kubectl-vmstorage-$target"
script="$outdir/kubectl-vmstorage-$target.sh"
mkdir -p "$outdir"

echo "building $target ..."
GOOS="$TEXT_OS" GOARCH="$TEXT_ARCH" CGO_ENABLED=0 \
	go build -trimpath -ldflags "-s -w -X $MODULE/cmd.Version=$VERSION" -o "$bin" .

# sha256 of the binary the host must end up with.
sum_of() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}
want="$(sum_of "$bin")"

echo "compressing with $COMP and encoding ..."
payload="$(mktemp)"
trap 'rm -f "$payload"' EXIT
"${comp_cmd[@]}" "$bin" | base64 | fold -w 76 >"$payload"

# The extractor is POSIX sh and copes with both GNU and BSD base64/sha tools,
# since the host may be either.
cat >"$script" <<EOF
#!/bin/sh
# kubectl-vmstorage  version=$VERSION  target=$TEXT_OS/$TEXT_ARCH  compressor=$COMP
# Self-extracting. Copy this whole file to the host, then:
#     sh $(basename "$script") [output-path]
# Decoded binary sha256: $want
set -eu

OUT="\${1:-kubectl-vmstorage}"
SELF="\$0"
if [ ! -r "\$SELF" ]; then
	echo "error: run this as a file (sh $(basename "$script")), not from a pipe" >&2
	exit 2
fi

if printf 'eA==' | base64 -d >/dev/null 2>&1; then B64=-d; else B64=-D; fi
command -v $decomp >/dev/null 2>&1 || { echo "error: $decomp not found on this host" >&2; exit 2; }

# Decode to a staging path and only move it into place once verified, so a
# truncated paste can never leave behind something that looks runnable.
STAGE="\$OUT.incomplete.\$\$"
trap 'rm -f "\$STAGE"' EXIT HUP INT TERM
if ! sed -e '1,/^__VMSTORAGE_PAYLOAD__\$/d' "\$SELF" | base64 \$B64 | $decomp > "\$STAGE"; then
	echo "error: decode failed -- the pasted file is truncated or corrupt" >&2
	echo "       expected the payload to end with a complete $COMP stream" >&2
	exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
	got=\$(sha256sum "\$STAGE" | cut -d' ' -f1)
elif command -v shasum >/dev/null 2>&1; then
	got=\$(shasum -a 256 "\$STAGE" | cut -d' ' -f1)
else
	got=""
fi
if [ -n "\$got" ] && [ "\$got" != "$want" ]; then
	echo "CHECKSUM MISMATCH -- the paste was truncated or altered" >&2
	echo "  expected $want" >&2
	echo "  got      \$got" >&2
	exit 1
fi

chmod +x "\$STAGE"
mv -f "\$STAGE" "\$OUT"
trap - EXIT HUP INT TERM

echo "wrote \$OUT (\$( [ -n "\$got" ] && echo 'sha256 verified' || echo 'sha256 tool unavailable, NOT verified' ))"
echo "install with: install -m 0755 \$OUT ~/bin/kubectl-vmstorage   # then: oc vmstorage vm <name> -n <ns>"
exit 0
__VMSTORAGE_PAYLOAD__
EOF
cat "$payload" >>"$script"

# Prove the artifact actually round-trips before calling it done.
echo "verifying round-trip ..."
check="$(mktemp -d)"
trap 'rm -f "$payload"; rm -rf "$check"' EXIT
if printf 'eA==' | base64 -d >/dev/null 2>&1; then b64dec=-d; else b64dec=-D; fi
sed -e '1,/^__VMSTORAGE_PAYLOAD__$/d' "$script" | base64 "$b64dec" | $decomp >"$check/out"
got="$(sum_of "$check/out")"
if [ "$got" != "$want" ]; then
	echo "FAILED: round-trip checksum mismatch (want $want, got $got)" >&2
	exit 1
fi

if [ "$CHUNK" != "0" ]; then
	rm -f "$outdir/${target}.part-"*
	split -b "$((CHUNK * 1024 * 1024))" -a 2 "$payload" "$outdir/${target}.part-"
	echo "split payload into $(ls "$outdir/${target}.part-"* | wc -l | tr -d ' ') parts of ${CHUNK}MiB"
	echo "  reassemble: cat ${target}.part-* | base64 -d | $decomp > kubectl-vmstorage && chmod +x kubectl-vmstorage"
fi

bytes=$(wc -c <"$script" | tr -d ' ')
lines=$(wc -l <"$script" | tr -d ' ')
printf 'ok  %s\n' "$script"
printf '    %s MiB of text, %s lines, decoded sha256 %s\n' \
	"$(awk -v b="$bytes" 'BEGIN{printf "%.1f", b/1048576}')" "$lines" "$want"
