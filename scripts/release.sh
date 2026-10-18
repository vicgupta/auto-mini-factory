#!/bin/sh
# Build amf for every supported platform and publish a GitHub release.
# For the maintainer only: publishing requires gh to be logged in as the
# repo owner. End users get new versions with `amf update`.
#
#   scripts/release.sh v0.1.2 notes.md      test, build, check, tag and publish
#   scripts/release.sh --dry-run v0.1.2     test, build and check only
#
# Each archive is laid out the way install.sh and `amf update` expect:
#
#   amf_<tag>_<os>_<arch>.tar.gz
#     amf_<tag>_<os>_<arch>/amf
#     amf_<tag>_<os>_<arch>/README.md
#     amf_<tag>_<os>_<arch>/SPECS.md
#
# and checksums.txt lists each archive's SHA-256.
set -eu

repo="vicgupta/auto-mini-factory"
platforms="darwin/arm64 darwin/amd64 linux/arm64 linux/amd64"

fail() {
	echo "release: $*" >&2
	exit 1
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$@"
	else
		shasum -a 256 "$@"
	fi
}

dry=false
if [ "${1:-}" = "--dry-run" ]; then
	dry=true
	shift
fi
tag="${1:-}"
notes="${2:-}"
echo "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$' || fail "usage: scripts/release.sh [--dry-run] vX.Y.Z [notes.md]"

if ! $dry; then
	[ -s "$notes" ] || fail "release notes file is missing or empty: ${notes:-<none>}"
	# Releases are credited to the maintainer alone.
	if grep -Eiq 'generated with \[?claude code|co-authored-by' "$notes"; then
		fail "$notes contains Claude attribution; remove it"
	fi
	notes="$(cd "$(dirname "$notes")" && pwd)/$(basename "$notes")"
	command -v gh >/dev/null 2>&1 || fail "gh is required"
	login=$(gh api user -q .login 2>/dev/null) || fail "log in to gh first: gh auth login"
	[ "$login" = "${repo%%/*}" ] || fail "only ${repo%%/*} can publish releases (gh is logged in as $login); end users update with: amf update"
fi

cd "$(dirname "$0")/.."

if ! $dry; then
	[ -z "$(git status --porcelain --untracked-files=no)" ] || fail "commit or stash your changes first"
	[ "$(git rev-parse --abbrev-ref HEAD)" = main ] || fail "release from main"
	git fetch -q origin main
	[ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ] || fail "main is not the same as origin/main; push or pull first"
	if git rev-parse -q --verify "refs/tags/$tag" >/dev/null || git ls-remote --exit-code --tags origin "refs/tags/$tag" >/dev/null; then
		fail "$tag already exists"
	fi
fi

go vet ./...
go test ./...

out=$(mktemp -d)
if ! $dry; then
	trap 'rm -rf "$out"' EXIT
fi

for p in $platforms; do
	os=${p%/*}
	arch=${p#*/}
	name="amf_${tag}_${os}_${arch}"
	echo "Building $name"
	mkdir "$out/$name"
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=$tag" -o "$out/$name/amf" ./cmd/amf
	cp README.md SPECS.md "$out/$name/"
	# COPYFILE_DISABLE keeps macOS tar from adding ._ metadata files.
	(cd "$out" && COPYFILE_DISABLE=1 tar -czf "$name.tar.gz" "$name")
	tar -tzf "$out/$name.tar.gz" | grep -qx "$name/amf" || fail "$name.tar.gz has no $name/amf"
done
(cd "$out" && sha256 amf_*.tar.gz >checksums.txt)

# Run this machine's build from its archive, as install.sh would.
case "$(uname -s)" in
Darwin) host_os=darwin ;;
Linux) host_os=linux ;;
*) host_os= ;;
esac
case "$(uname -m)" in
x86_64 | amd64) host_arch=amd64 ;;
arm64 | aarch64) host_arch=arm64 ;;
*) host_arch= ;;
esac
host="amf_${tag}_${host_os}_${host_arch}"
if [ -f "$out/$host.tar.gz" ]; then
	mkdir "$out/check"
	tar -xzf "$out/$host.tar.gz" -C "$out/check"
	got=$("$out/check/$host/amf" version)
	[ "$got" = "amf $tag" ] || fail "$host/amf version printed \"$got\", want \"amf $tag\""
	rm -rf "$out/check"
fi

if $dry; then
	echo "Dry run: built and checked $tag in $out"
	exit 0
fi

gh release create "$tag" --repo "$repo" --target "$(git rev-parse HEAD)" --title "amf $tag" \
	--notes-file "$notes" "$out"/amf_*.tar.gz "$out/checksums.txt"
git fetch -q --tags origin

# Check what GitHub serves. Its download cache can lag for a few minutes.
if [ -f "$out/$host.tar.gz" ]; then
	want=$(awk -v f="$host.tar.gz" '$2 == f { print $1 }' "$out/checksums.txt")
	url="https://github.com/$repo/releases/download/$tag/$host.tar.gz"
	served=
	for _ in 1 2 3 4 5 6; do
		if curl -fsSL -o "$out/served.tar.gz" "$url" 2>/dev/null; then
			served=$(sha256 "$out/served.tar.gz" | awk '{ print $1 }')
			[ "$served" = "$want" ] && break
		fi
		sleep 10
	done
	if [ "$served" = "$want" ]; then
		echo "Verified: GitHub serves $host.tar.gz with the published checksum"
	else
		echo "Warning: GitHub isn't serving $host.tar.gz as published yet; check again with: amf update --check" >&2
	fi
fi

echo "Released https://github.com/$repo/releases/tag/$tag"
