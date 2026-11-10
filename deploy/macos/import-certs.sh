#!/bin/sh
# Imports the Developer ID certificates for build-pkg.sh into a throwaway
# keychain, for CI runners. Reads base64 .p12 files from
# MACOS_SIGN_P12_BASE64 (Developer ID Application) and
# MACOS_INSTALLER_P12_BASE64 (Developer ID Installer), both protected by
# MACOS_P12_PASSWORD. With MACOS_NOTARY_APPLE_ID, MACOS_NOTARY_TEAM_ID and
# MACOS_NOTARY_PASSWORD it also stores them in the keychain as the notary
# profile named by MACOS_NOTARY_PROFILE (default overload-ci), so build-pkg.sh
# never needs the password. Prints the keychain path; delete it with
# `security delete-keychain PATH` when done.
set -eu

: "${MACOS_SIGN_P12_BASE64:?}" "${MACOS_INSTALLER_P12_BASE64:?}" "${MACOS_P12_PASSWORD:?}"
umask 077
parent=${RUNNER_TEMP:-${TMPDIR:-/tmp}}
# A fresh directory per run, since a reused runner may still hold an older
# keychain. The decoded certificates never outlive the script, and on failure
# neither does the keychain, as the caller never learns its path.
dir=$(mktemp -d "${parent%/}/overload-signing.XXXXXX")
keychain=$dir/overload-signing.keychain-db
cleanup() {
	status=$?
	rm -f "$dir"/*.p12
	if [ "$status" -ne 0 ]; then
		security delete-keychain "$keychain" 2>/dev/null || true
		rm -rf "$dir"
	fi
}
trap cleanup EXIT
password=$(openssl rand -hex 24)

security create-keychain -p "$password" "$keychain" >&2
security set-keychain-settings -lut 3600 "$keychain" >&2
security unlock-keychain -p "$password" "$keychain" >&2
import_p12() {
	printf '%s' "$1" | base64 --decode >"$dir/$2.p12"
	# security import only takes the password as -P; on a short-lived runner
	# only this job's processes can see it.
	security import "$dir/$2.p12" -k "$keychain" -P "$MACOS_P12_PASSWORD" -f pkcs12 \
		-T /usr/bin/codesign -T /usr/bin/productsign >&2
	rm -f "$dir/$2.p12"
}
import_p12 "$MACOS_SIGN_P12_BASE64" application
import_p12 "$MACOS_INSTALLER_P12_BASE64" installer
security set-key-partition-list -S apple-tool:,apple: -s -k "$password" "$keychain" >/dev/null
# shellcheck disable=SC2046
security list-keychains -d user -s "$keychain" $(security list-keychains -d user | tr -d '"')

if [ -n "${MACOS_NOTARY_APPLE_ID:-}" ]; then
	: "${MACOS_NOTARY_TEAM_ID:?}" "${MACOS_NOTARY_PASSWORD:?}"
	xcrun notarytool store-credentials "${MACOS_NOTARY_PROFILE:-overload-ci}" \
		--apple-id "$MACOS_NOTARY_APPLE_ID" --team-id "$MACOS_NOTARY_TEAM_ID" \
		--password "$MACOS_NOTARY_PASSWORD" --keychain "$keychain" >&2
fi
echo "$keychain"
