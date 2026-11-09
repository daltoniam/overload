#!/bin/sh
# Imports the Developer ID certificates for build-pkg.sh into a throwaway
# keychain, for CI runners. Reads base64 .p12 files from
# MACOS_SIGN_P12_BASE64 (Developer ID Application) and
# MACOS_INSTALLER_P12_BASE64 (Developer ID Installer), both protected by
# MACOS_P12_PASSWORD, and prints the keychain path. Delete the keychain with
# `security delete-keychain PATH` when done.
set -eu

: "${MACOS_SIGN_P12_BASE64:?}" "${MACOS_INSTALLER_P12_BASE64:?}" "${MACOS_P12_PASSWORD:?}"
dir=${RUNNER_TEMP:-${TMPDIR:-/tmp}}
keychain=$dir/overload-signing.keychain-db
password=$(openssl rand -hex 24)

security create-keychain -p "$password" "$keychain" >&2
security set-keychain-settings -lut 3600 "$keychain" >&2
security unlock-keychain -p "$password" "$keychain" >&2
umask 077
import_p12() {
	printf '%s' "$1" | base64 --decode >"$dir/$2.p12"
	security import "$dir/$2.p12" -k "$keychain" -P "$MACOS_P12_PASSWORD" -f pkcs12 \
		-T /usr/bin/codesign -T /usr/bin/pkgbuild -T /usr/bin/productsign >&2
	rm -f "$dir/$2.p12"
}
import_p12 "$MACOS_SIGN_P12_BASE64" application
import_p12 "$MACOS_INSTALLER_P12_BASE64" installer
security set-key-partition-list -S apple-tool:,apple: -s -k "$password" "$keychain" >/dev/null
# shellcheck disable=SC2046
security list-keychains -d user -s "$keychain" $(security list-keychains -d user | tr -d '"')
echo "$keychain"
