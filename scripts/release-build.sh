#!/usr/bin/env bash
#
# Build the release archives for every supported platform.
#
# Usage: release-build.sh <version>
#
# <version> is the release version without a leading v (semantic-release
# passes it that way). Writes to dist/release/, which is emptied first so a
# previous run can never leak archives into the checksums or the release:
#
#   code-vm_<version>_<os>_<arch>.tar.gz   code-vm, LICENSE, README.md
#   checksums.txt                          SHA-256 of every archive
set -euo pipefail

cd "$(dirname "$0")/.."

# Only the platforms the README lists as supported.
targets=(linux/amd64 darwin/arm64)

version="${1:-}"
if [[ -z "$version" ]]; then
    echo "usage: release-build.sh <version>" >&2
    exit 2
fi
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$ ]]; then
    echo "invalid version \"$version\": expected semver without leading v" >&2
    exit 2
fi

out=dist/release
rm -rf "$out"
mkdir -p "$out"

staging="$(mktemp -d dist/release-staging.XXXXXX)"
trap 'rm -rf "$staging"' EXIT

for target in "${targets[@]}"; do
    os="${target%/*}"
    arch="${target#*/}"
    dir="$staging/${os}_${arch}"
    mkdir -p "$dir"

    # Same build as `mise run build` apart from the target and the version.
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build \
        -ldflags "-X github.com/wetransform/code-vm/internal/cli.version=$version" \
        -o "$dir/code-vm" ./cmd/code-vm
    cp LICENSE README.md "$dir/"

    tar -czf "$out/code-vm_${version}_${os}_${arch}.tar.gz" -C "$dir" code-vm LICENSE README.md
done

# Bare file names, so `sha256sum -c` works next to the downloaded archives.
# macOS has no sha256sum; shasum writes the same format.
sha256=(sha256sum)
if ! command -v sha256sum > /dev/null; then
    sha256=(shasum -a 256)
fi
(cd "$out" && "${sha256[@]}" code-vm_*.tar.gz > checksums.txt)

ls -1 "$out"
