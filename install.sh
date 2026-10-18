#!/bin/sh
# Install the amf binary from a GitHub release.
#
#   curl -fsSL https://raw.githubusercontent.com/vicgupta/auto-mini-factory/main/install.sh | sh
#
# Environment:
#   AMF_VERSION      release tag to install (default: latest), e.g. v0.0.2
#   AMF_INSTALL_DIR  where to put amf (default: /usr/local/bin if writable, else ~/.local/bin)
set -eu

repo="vicgupta/auto-mini-factory"

fail() {
	echo "amf install: $*" >&2
	exit 1
}

for cmd in curl tar uname mktemp; do
	command -v "$cmd" >/dev/null 2>&1 || fail "$cmd is required"
done

case "$(uname -s)" in
Darwin) os=darwin ;;
Linux) os=linux ;;
*) fail "unsupported OS $(uname -s); use: go install github.com/$repo/cmd/amf@latest" ;;
esac

case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) fail "unsupported architecture $(uname -m); use: go install github.com/$repo/cmd/amf@latest" ;;
esac

version="${AMF_VERSION:-}"
if [ -z "$version" ]; then
	# /releases/latest redirects to /releases/tag/<version>.
	latest=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest") ||
		fail "could not find the latest release"
	version="${latest##*/}"
	case "$version" in
	v*) ;;
	*) fail "could not find the latest release" ;;
	esac
fi

name="amf_${version}_${os}_${arch}"
base="https://github.com/$repo/releases/download/$version"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "Downloading amf $version for $os/$arch"
curl -fsSL -o "$tmp/$name.tar.gz" "$base/$name.tar.gz" || fail "download failed: $base/$name.tar.gz"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || fail "download failed: $base/checksums.txt"

want=$(awk -v f="$name.tar.gz" '$2 == f { print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || fail "$name.tar.gz is not listed in checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
	got=$(sha256sum "$tmp/$name.tar.gz" | awk '{ print $1 }')
else
	got=$(shasum -a 256 "$tmp/$name.tar.gz" | awk '{ print $1 }')
fi
[ "$got" = "$want" ] || fail "checksum mismatch for $name.tar.gz"

tar -xzf "$tmp/$name.tar.gz" -C "$tmp"

dir="${AMF_INSTALL_DIR:-}"
if [ -z "$dir" ]; then
	if [ -w /usr/local/bin ]; then
		dir=/usr/local/bin
	else
		dir="$HOME/.local/bin"
	fi
fi
mkdir -p "$dir"
# Install via a temp name and rename, so a running amf isn't overwritten in place.
cp "$tmp/$name/amf" "$dir/.amf.new"
chmod 755 "$dir/.amf.new"
mv -f "$dir/.amf.new" "$dir/amf"

echo "Installed $("$dir/amf" version) to $dir/amf"
case ":$PATH:" in
*":$dir:"*) ;;
*) echo "Note: $dir is not on your PATH. Add it, e.g.: export PATH=\"$dir:\$PATH\"" ;;
esac
