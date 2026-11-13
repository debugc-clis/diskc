#!/bin/sh
set -eu

if [ "$#" -gt 1 ]; then
    printf 'Usage: sh scripts/build-macos.sh [output-directory]\n' >&2
    exit 2
fi

script_dir=$(CDPATH= cd "$(dirname "$0")" && pwd -P)
repo_root=$(CDPATH= cd "$script_dir/.." && pwd -P)
output_dir=${1:-"$repo_root/dist/macos"}
case "$output_dir" in
    /*) ;;
    *) output_dir="$PWD/$output_dir" ;;
esac

if ! command -v go >/dev/null 2>&1; then
    printf 'Go is required to build the macOS packages.\n' >&2
    exit 1
fi
if command -v shasum >/dev/null 2>&1; then
    checksum_command=shasum
elif command -v sha256sum >/dev/null 2>&1; then
    checksum_command=sha256sum
else
    printf 'A SHA256 tool is required: shasum or sha256sum.\n' >&2
    exit 1
fi

mkdir -p "$output_dir"
output_dir=$(CDPATH= cd "$output_dir" && pwd -P)
artifacts='diskc_macos_arm64.tar.gz diskc_macos_amd64.tar.gz checksums.txt'
for artifact in $artifacts; do
    if [ -e "$output_dir/$artifact" ] || [ -L "$output_dir/$artifact" ]; then
        printf 'Refusing to overwrite %s; choose a new output directory.\n' "$output_dir/$artifact" >&2
        exit 1
    fi
done

stage_dir=$(mktemp -d "$output_dir/.diskc-macos.XXXXXX")
trap 'rm -rf "$stage_dir"' 0
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

commit=$(git -C "$repo_root" rev-parse HEAD 2>/dev/null || printf 'none')
built=$(date -u +%Y-%m-%dT%H:%M:%SZ)
buildinfo=github.com/debugc-clis/diskc/internal/buildinfo
ldflags="-s -w -X $buildinfo.Version=dev -X $buildinfo.Commit=$commit -X $buildinfo.Date=$built"

mkdir -p "$stage_dir/package/docs"
cp "$repo_root/LICENSE" "$repo_root/README.md" "$repo_root/DISCLAIMER.md" "$stage_dir/package/"
cp "$repo_root/docs/diskc.1" "$stage_dir/package/docs/diskc.1"
cd "$repo_root"
for arch in arm64 amd64; do
    printf 'Building macOS/%s...\n' "$arch"
    GOOS=darwin GOARCH="$arch" CGO_ENABLED=0 go build -trimpath -ldflags "$ldflags" -o "$stage_dir/package/diskc" ./cmd/diskc
    COPYFILE_DISABLE=1 tar -czf "$stage_dir/diskc_macos_$arch.tar.gz" -C "$stage_dir/package" diskc LICENSE README.md DISCLAIMER.md docs/diskc.1
done

(
    cd "$stage_dir"
    if [ "$checksum_command" = shasum ]; then
        shasum -a 256 diskc_macos_arm64.tar.gz diskc_macos_amd64.tar.gz > checksums.txt
    else
        sha256sum diskc_macos_arm64.tar.gz diskc_macos_amd64.tar.gz > checksums.txt
    fi
)

for artifact in $artifacts; do
    # Hard links publish completed files without replacing a concurrently created file.
    if ! ln "$stage_dir/$artifact" "$output_dir/$artifact"; then
        printf 'Could not publish %s; existing files were preserved.\n' "$output_dir/$artifact" >&2
        exit 1
    fi
done
printf 'macOS packages and SHA256 checksums are ready in %s\n' "$output_dir"
