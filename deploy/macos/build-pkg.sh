#!/bin/sh
# Builds a macOS installer package that puts a universal (arm64 and amd64)
# overload binary in /usr/local/bin.
#
# Without credentials the package is unsigned, which is fine for CI and for
# installing with `sudo installer -pkg`. A downloadable package needs all of:
#
#   MACOS_SIGN_IDENTITY       "Developer ID Application: Name (TEAMID)"
#   MACOS_INSTALLER_IDENTITY  "Developer ID Installer: Name (TEAMID)"
#
# and, to notarize, either MACOS_NOTARY_PROFILE (a `notarytool
# store-credentials` keychain profile) or MACOS_NOTARY_APPLE_ID,
# MACOS_NOTARY_TEAM_ID and MACOS_NOTARY_PASSWORD (an app-specific password).
# MACOS_KEYCHAIN selects the keychain that holds the identities.
#
# VERSION (default: git describe) and OUTDIR (default: dist/pkg) are optional.
set -eu

root=$(cd "$(dirname "$0")/../.." && pwd)
version=${VERSION:-$(git -C "$root" describe --tags --always --dirty 2>/dev/null || echo dev)}
version=${version#v}
outdir=${OUTDIR:-$root/dist/pkg}
sign_identity=${MACOS_SIGN_IDENTITY:-}
installer_identity=${MACOS_INSTALLER_IDENTITY:-}
keychain=${MACOS_KEYCHAIN:-}

fail() {
	echo "build-pkg: $*" >&2
	exit 1
}

[ "$(uname -s)" = Darwin ] || fail "macOS is required (pkgbuild, lipo and codesign)"
for tool in go lipo codesign pkgbuild xattr; do
	command -v "$tool" >/dev/null 2>&1 || fail "$tool is required"
done

notarize=
if [ -n "${MACOS_NOTARY_PROFILE:-}" ] || [ -n "${MACOS_NOTARY_APPLE_ID:-}" ]; then
	notarize=1
	if [ -z "${MACOS_NOTARY_PROFILE:-}" ]; then
		[ -n "${MACOS_NOTARY_TEAM_ID:-}" ] && [ -n "${MACOS_NOTARY_PASSWORD:-}" ] ||
			fail "MACOS_NOTARY_APPLE_ID also needs MACOS_NOTARY_TEAM_ID and MACOS_NOTARY_PASSWORD"
	fi
	[ -n "$sign_identity" ] && [ -n "$installer_identity" ] ||
		fail "notarizing needs MACOS_SIGN_IDENTITY and MACOS_INSTALLER_IDENTITY"
fi
if [ -n "$installer_identity" ] && [ -z "$sign_identity" ]; then
	fail "MACOS_INSTALLER_IDENTITY needs MACOS_SIGN_IDENTITY so the binary inside is signed too"
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

for arch in arm64 amd64; do
	echo "build-pkg: building darwin/$arch"
	(cd "$root" && CGO_ENABLED=0 GOOS=darwin GOARCH=$arch go build -trimpath \
		-ldflags "-s -w -X main.version=$version" -o "$work/overload-$arch" ./cmd/overload)
done
payload=$work/payload
mkdir -p "$payload/usr/local/bin" "$payload/usr/local/share/doc/overload"
lipo -create -output "$payload/usr/local/bin/overload" "$work/overload-arm64" "$work/overload-amd64"
cp "$root/LICENSE" "$payload/usr/local/share/doc/overload/LICENSE"

if [ -n "$sign_identity" ]; then
	echo "build-pkg: signing the binary as $sign_identity"
	set -- --force --options runtime --timestamp --identifier dev.overload --sign "$sign_identity"
	if [ -n "$keychain" ]; then
		set -- "$@" --keychain "$keychain"
	fi
	codesign "$@" "$payload/usr/local/bin/overload"
	codesign --verify --strict --verbose=2 "$payload/usr/local/bin/overload"
else
	codesign --force --identifier dev.overload --sign - "$payload/usr/local/bin/overload"
fi

# Extended attributes would otherwise ship as AppleDouble ._ files.
xattr -cr "$payload" 2>/dev/null || true
export COPYFILE_DISABLE=1

mkdir -p "$outdir"
pkg=$outdir/overload_darwin_universal.pkg
rm -f "$pkg" "$pkg.sha256"
set -- --root "$payload" --identifier dev.overload.pkg --version "$version" \
	--install-location / --ownership recommended
if [ -n "$installer_identity" ]; then
	echo "build-pkg: signing the package as $installer_identity"
	set -- "$@" --sign "$installer_identity" --timestamp
	if [ -n "$keychain" ]; then
		set -- "$@" --keychain "$keychain"
	fi
fi
pkgbuild "$@" "$pkg"
if [ -n "$installer_identity" ]; then
	pkgutil --check-signature "$pkg"
fi

if [ -n "$notarize" ]; then
	echo "build-pkg: notarizing"
	if [ -n "${MACOS_NOTARY_PROFILE:-}" ]; then
		set -- --keychain-profile "$MACOS_NOTARY_PROFILE"
		if [ -n "$keychain" ]; then
			set -- "$@" --keychain "$keychain"
		fi
	else
		set -- --apple-id "$MACOS_NOTARY_APPLE_ID" --team-id "$MACOS_NOTARY_TEAM_ID" --password "$MACOS_NOTARY_PASSWORD"
	fi
	xcrun notarytool submit "$pkg" "$@" --wait --timeout 30m --output-format json >"$work/notary.json" ||
		fail "notarization request failed: $(cat "$work/notary.json" 2>/dev/null)"
	grep -q '"status" *: *"Accepted"' "$work/notary.json" ||
		fail "notarization was not accepted: $(cat "$work/notary.json")"
	xcrun stapler staple "$pkg"
	xcrun stapler validate "$pkg"
	spctl --assess --type install --verbose=2 "$pkg"
fi

(cd "$outdir" && shasum -a 256 "$(basename "$pkg")" >"$(basename "$pkg").sha256")
echo "build-pkg: wrote $pkg"
