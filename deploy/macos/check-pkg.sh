#!/bin/sh
# Checks a package built by build-pkg.sh without installing it: the payload
# holds exactly the expected files, the binary is universal and runs, and
# reports the expected version. With REQUIRE_SIGNED=1 it also requires a
# Developer ID signature, a hardened runtime binary and a stapled ticket.
#
#   deploy/macos/check-pkg.sh dist/pkg/overload_darwin_universal.pkg [version]
set -eu

pkg=${1:?usage: check-pkg.sh PKG [VERSION]}
want_version=${2:-}
want_version=${want_version#v}

fail() {
	echo "check-pkg: $*" >&2
	exit 1
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

pkgutil --expand-full "$pkg" "$work/expanded"
payload=$work/expanded/Payload
[ -d "$payload" ] || fail "no payload in the package"
# AppleDouble entries carry extended attributes (com.apple.provenance on
# recent macOS); the installer turns them back into attributes, not files.
files=$(cd "$payload" && find . -type f ! -name '._*' | sed 's|^\./||' | sort | tr '\n' ' ')
want_files="usr/local/bin/overload usr/local/share/doc/overload/LICENSE "
[ "$files" = "$want_files" ] || fail "payload is [$files], want [$want_files]"
binary=$payload/usr/local/bin/overload
archs=$(lipo -archs "$binary")
for arch in arm64 x86_64; do
	case " $archs " in
	*" $arch "*) ;;
	*) fail "binary architectures are [$archs], want arm64 and x86_64" ;;
	esac
done
codesign --verify --strict "$binary" || fail "binary signature does not verify"

got_version=$("$binary" version)
if [ -n "$want_version" ] && [ "$got_version" != "$want_version" ] && [ "$got_version" != "v$want_version" ]; then
	fail "binary reports version $got_version, want $want_version"
fi

if [ "${REQUIRE_SIGNED:-}" = 1 ]; then
	pkgutil --check-signature "$pkg" | grep -q 'Developer ID Installer' || fail "package is not signed with a Developer ID Installer certificate"
	codesign --display --verbose=2 "$binary" 2>&1 | grep -q 'Authority=Developer ID Application' || fail "binary is not signed with a Developer ID Application certificate"
	codesign --display --verbose=2 "$binary" 2>&1 | grep -q 'flags=.*runtime' || fail "binary does not use the hardened runtime"
	xcrun stapler validate "$pkg" || fail "package has no stapled notarization ticket"
	spctl --assess --type install "$pkg" || fail "Gatekeeper rejects the package"
fi

echo "check-pkg: $pkg ok ($archs, version $got_version)"
