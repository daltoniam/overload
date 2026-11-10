#!/bin/sh
# Checks a package built by build-pkg.sh without installing it: it installs
# only into /usr/local/overload without resetting directory permissions, the
# payload and bill of materials hold exactly the expected root-owned entries
# with the expected modes (no AppleDouble ._ files), the link script is
# there, and the binary is universal and reports the expected version. With
# REQUIRE_SIGNED=1 it also requires a Developer ID signature (from team
# MACOS_TEAM_ID when set), a hardened runtime binary and a stapled ticket.
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

pkgutil --expand "$pkg" "$work/flat"
[ -f "$work/flat/Payload" ] || fail "no payload in the package"
grep -q 'install-location="/usr/local/overload"' "$work/flat/PackageInfo" ||
	fail "package does not install into /usr/local/overload"
grep -q 'overwrite-permissions="false"' "$work/flat/PackageInfo" ||
	fail "package would reset the permissions of existing directories"
listing=$(lsbom -s "$work/flat/Bom") || fail "bill of materials cannot be read"
entries=$(printf '%s\n' "$listing" | LC_ALL=C sort | tr '\n' ' ')
want_entries=". ./LICENSE ./bin ./bin/overload "
[ "$entries" = "$want_entries" ] || fail "bill of materials is [$entries], want [$want_entries]"
listing=$(lsbom -p fm "$work/flat/Bom") || fail "bill of materials cannot be read"
modes=$(printf '%s\n' "$listing" | LC_ALL=C sort | tr '\t\n' '= ')
want_modes=".=40755 ./LICENSE=100644 ./bin=40755 ./bin/overload=100755 "
[ "$modes" = "$want_modes" ] || fail "payload modes are [$modes], want [$want_modes]"
listing=$(lsbom -p U "$work/flat/Bom") || fail "bill of materials cannot be read"
owners=$(printf '%s\n' "$listing" | sort -u | tr '\n' ' ')
[ "$owners" = "root " ] || fail "payload owners are [$owners], want root"
# Decompress once and check every step's status, since a pipeline's status
# is only its last command's and a damaged payload can list fully before
# gzip reports the error.
gzip -dc "$work/flat/Payload" >"$work/payload.cpio" || fail "payload does not decompress"
listing=$(cpio -it --quiet <"$work/payload.cpio") || fail "payload archive cannot be listed"
archived=$(printf '%s\n' "$listing" | LC_ALL=C sort | tr '\n' ' ')
[ "$archived" = "$want_entries" ] || fail "payload is [$archived], want [$want_entries]"
# The installer writes files with the archive's modes, so check them there
# too, not only in the bill of materials.
listing=$(cpio -itv --quiet <"$work/payload.cpio" 2>/dev/null) || fail "payload archive cannot be listed"
archive_modes=$(printf '%s\n' "$listing" | awk '{print $1 "=" $NF}' | LC_ALL=C sort -t= -k2 | tr '\n' ' ')
want_archive_modes="drwxr-xr-x=. -rw-r--r--=./LICENSE drwxr-xr-x=./bin -rwxr-xr-x=./bin/overload "
[ "$archive_modes" = "$want_archive_modes" ] || fail "payload archive modes are [$archive_modes], want [$want_archive_modes]"
# The installer also takes ownership from the archive, not the bill of
# materials.
listing=$(cpio -itvn --quiet <"$work/payload.cpio" 2>/dev/null) || fail "payload archive cannot be listed"
archive_owners=$(printf '%s\n' "$listing" | awk '{print $3}' | sort -u | tr '\n' ' ')
[ "$archive_owners" = "0 " ] || fail "payload archive owners are [$archive_owners], want 0"
scripts=$(cd "$work/flat/Scripts" && find . -type f | LC_ALL=C sort | tr '\n' ' ')
[ "$scripts" = "./postinstall " ] || fail "scripts are [$scripts], want [./postinstall ]"
[ -x "$work/flat/Scripts/postinstall" ] || fail "postinstall is not executable"

mkdir "$work/payload"
(cd "$work/payload" && cpio -id --quiet <"$work/payload.cpio") || fail "payload does not extract"
binary=$work/payload/bin/overload
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
	if [ -n "${MACOS_TEAM_ID:-}" ]; then
		codesign --display --verbose=2 "$binary" 2>&1 | grep -qx "TeamIdentifier=$MACOS_TEAM_ID" || fail "binary is not signed by team $MACOS_TEAM_ID"
		pkgutil --check-signature "$pkg" | grep -q "Developer ID Installer: .*($MACOS_TEAM_ID)" || fail "package is not signed by team $MACOS_TEAM_ID"
	fi
	codesign --display --verbose=2 "$binary" 2>&1 | grep -q 'flags=.*runtime' || fail "binary does not use the hardened runtime"
	xcrun stapler validate "$pkg" || fail "package has no stapled notarization ticket"
	spctl --assess --type install "$pkg" || fail "Gatekeeper rejects the package"
fi

echo "check-pkg: $pkg ok ($archs, version $got_version)"
