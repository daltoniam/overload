#!/bin/sh
# Builds a macOS installer package that puts a universal (arm64 and amd64)
# overload binary in /usr/local/overload/bin and links /usr/local/bin/overload
# to it. The payload only holds /usr/local/overload, so installing never
# changes the owner or mode of /usr/local or /usr/local/bin (Homebrew on Intel
# Macs owns them), and the link is left alone when something else, such as a
# Homebrew install, already has that name.
#
# Without credentials the package is unsigned, which is fine for CI and for
# installing with `sudo installer -pkg`. A downloadable package needs:
#
#   MACOS_SIGN_IDENTITY       "Developer ID Application: Name (TEAMID)"
#   MACOS_INSTALLER_IDENTITY  "Developer ID Installer: Name (TEAMID)"
#   MACOS_NOTARY_PROFILE      a `xcrun notarytool store-credentials` profile
#
# MACOS_KEYCHAIN selects the keychain that holds the identities and the
# notary profile. VERSION (default: git describe) and OUTDIR (default:
# dist/pkg) are optional.
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
for tool in go lipo codesign pkgbuild productsign pkgutil ditto cpio lsbom mkbom; do
	command -v "$tool" >/dev/null 2>&1 || fail "$tool is required"
done

# Notarizing reads the credentials from a keychain profile so the password
# is not on the command line of a submission that can run for many minutes.
notary_profile=${MACOS_NOTARY_PROFILE:-}
if [ -n "${MACOS_NOTARY_APPLE_ID:-}${MACOS_NOTARY_PASSWORD:-}" ] && [ -z "$notary_profile" ]; then
	fail "store the notary credentials with xcrun notarytool store-credentials and set MACOS_NOTARY_PROFILE"
fi
if [ -n "$notary_profile" ] && { [ -z "$sign_identity" ] || [ -z "$installer_identity" ]; }; then
	fail "notarizing needs MACOS_SIGN_IDENTITY and MACOS_INSTALLER_IDENTITY"
fi
if [ -n "$installer_identity" ] && [ -z "$sign_identity" ]; then
	fail "MACOS_INSTALLER_IDENTITY needs MACOS_SIGN_IDENTITY so the binary inside is signed too"
fi

# notary_log prints Apple's log for a rejected submission, which names the
# files and the reasons.
notary_log() {
	submission=$(sed -n 's/.*"id" *: *"\([^"]*\)".*/\1/p' "$work/notary.json" 2>/dev/null | head -n 1)
	if [ -n "$submission" ]; then
		xcrun notarytool log "$submission" "$@" >&2 || true
	fi
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

for arch in arm64 amd64; do
	echo "build-pkg: building darwin/$arch"
	(cd "$root" && CGO_ENABLED=0 GOOS=darwin GOARCH=$arch go build -trimpath \
		-ldflags "-s -w -X main.version=$version" -o "$work/overload-$arch" ./cmd/overload)
done
staging=$work/staging
mkdir -p "$staging/bin"
lipo -create -output "$staging/bin/overload" "$work/overload-arm64" "$work/overload-amd64"
cp "$root/LICENSE" "$staging/LICENSE"
binary=$staging/bin/overload

if [ -n "$sign_identity" ]; then
	echo "build-pkg: signing the binary as $sign_identity"
	set -- --force --options runtime --timestamp --identifier dev.overload --sign "$sign_identity"
	if [ -n "$keychain" ]; then
		set -- "$@" --keychain "$keychain"
	fi
	codesign "$@" "$binary"
	codesign --verify --strict --verbose=2 "$binary"
else
	codesign --force --identifier dev.overload --sign - "$binary"
fi

# ditto drops resource forks, ACLs and extended attributes from the files.
payload=$work/payload
ditto --norsrc --noextattr --noacl "$staging" "$payload"
scripts=$work/scripts
mkdir -p "$scripts"
cp "$root/deploy/macos/postinstall" "$scripts/postinstall"
chmod 755 "$scripts/postinstall"

mkdir -p "$outdir"
pkg=$outdir/overload_darwin_universal.pkg
rm -f "$pkg" "$pkg.sha256"
pkgbuild --root "$payload" --identifier dev.overload.pkg --version "$version" \
	--install-location /usr/local/overload --scripts "$scripts" --ownership recommended \
	"$work/component.pkg"

# Recent macOS puts com.apple.provenance on every file this build creates,
# and pkgbuild ships it as AppleDouble ._ entries in the payload, the bill of
# materials and the scripts. Rebuild those three without them; the payload
# is a root-owned cpio archive as pkgbuild writes it, and pkgutil --flatten
# archives the scripts directory without attributes.
flat=$work/flat
pkgutil --expand "$work/component.pkg" "$flat"
(cd "$payload" && find . | LC_ALL=C sort | cpio -o --format odc -R 0:0 --quiet) >"$work/payload.cpio"
gzip -9 -c "$work/payload.cpio" >"$flat/Payload"
rm -rf "$flat/Scripts"
ditto --norsrc --noextattr --noacl "$scripts" "$flat/Scripts"
lsbom "$flat/Bom" | grep -v '/\._' >"$work/bom.txt"
mkbom -i "$work/bom.txt" "$flat/Bom"
# The payload holds only /usr/local/overload; overwrite-permissions off also
# keeps an existing directory's owner and mode.
sed -i '' -e "s/numberOfFiles=\"[0-9]*\"/numberOfFiles=\"$(wc -l <"$work/bom.txt" | tr -d ' ')\"/" \
	-e 's/overwrite-permissions="true"/overwrite-permissions="false"/' "$flat/PackageInfo"
pkgutil --flatten "$flat" "$work/unsigned.pkg"

if [ -n "$installer_identity" ]; then
	echo "build-pkg: signing the package as $installer_identity"
	set -- --sign "$installer_identity" --timestamp
	if [ -n "$keychain" ]; then
		set -- "$@" --keychain "$keychain"
	fi
	productsign "$@" "$work/unsigned.pkg" "$pkg"
	pkgutil --check-signature "$pkg"
else
	mv "$work/unsigned.pkg" "$pkg"
fi

if [ -n "$notary_profile" ]; then
	echo "build-pkg: notarizing"
	set -- --keychain-profile "$notary_profile"
	if [ -n "$keychain" ]; then
		set -- "$@" --keychain "$keychain"
	fi
	if ! xcrun notarytool submit "$pkg" "$@" --wait --timeout 30m --output-format json >"$work/notary.json"; then
		notary_log "$@"
		fail "notarization request failed: $(cat "$work/notary.json" 2>/dev/null)"
	fi
	if ! grep -q '"status" *: *"Accepted"' "$work/notary.json"; then
		notary_log "$@"
		fail "notarization was not accepted: $(cat "$work/notary.json")"
	fi
	xcrun stapler staple "$pkg"
	xcrun stapler validate "$pkg"
	spctl --assess --type install --verbose=2 "$pkg"
fi

(cd "$outdir" && shasum -a 256 "$(basename "$pkg")" >"$(basename "$pkg").sha256")
echo "build-pkg: wrote $pkg"
