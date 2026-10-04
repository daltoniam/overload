#!/bin/sh
# One-command overload install:
#   curl -fsSL https://raw.githubusercontent.com/daltoniam/overload/main/install.sh | sh
#
# With Homebrew it installs the daltoniam/tap/overload formula (which brings
# Postgres); otherwise it downloads the release binary into ~/.local/bin.
# Then it runs `overload install`, which starts a private Postgres and the
# overload service and prints the UI address and password.
#
# Environment overrides:
#   OVERLOAD_VERSION       release tag to install (default: latest)
#   OVERLOAD_NO_BREW=1     skip Homebrew even if present
#   OVERLOAD_INSTALL_DIR   binary directory without Homebrew (default ~/.local/bin)
#   OVERLOAD_DOWNLOAD_URL  archive URL (for testing or mirrors)
#   OVERLOAD_INSTALL_ARGS  extra arguments for `overload install`
set -eu

repo="daltoniam/overload"
version="${OVERLOAD_VERSION:-latest}"

say() { printf '%s\n' "$*"; }
fail() { say "error: $*" >&2; exit 1; }

fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1" -o "$2"
	elif command -v wget >/dev/null 2>&1; then
		wget -q "$1" -O "$2"
	else
		fail "curl or wget is required"
	fi
}

install_with_brew() {
	say "Installing overload with Homebrew..."
	brew install daltoniam/tap/overload
	bin="$(brew --prefix)/bin/overload"
}

install_from_release() {
	os="$(uname -s | tr '[:upper:]' '[:lower:]')"
	case "$os" in darwin | linux) ;; *) fail "unsupported OS: $os" ;; esac
	arch="$(uname -m)"
	case "$arch" in
	arm64 | aarch64) arch=arm64 ;;
	x86_64 | amd64) arch=amd64 ;;
	*) fail "unsupported architecture: $arch" ;;
	esac
	asset="overload_${os}_${arch}.tar.gz"
	if [ -n "${OVERLOAD_DOWNLOAD_URL:-}" ]; then
		url="$OVERLOAD_DOWNLOAD_URL"
		sums=""
	elif [ "$version" = latest ]; then
		url="https://github.com/$repo/releases/latest/download/$asset"
		sums="https://github.com/$repo/releases/latest/download/checksums.txt"
	else
		url="https://github.com/$repo/releases/download/$version/$asset"
		sums="https://github.com/$repo/releases/download/$version/checksums.txt"
	fi
	tmp="$(mktemp -d)"
	trap 'rm -rf "$tmp"' EXIT
	say "Downloading $url"
	fetch "$url" "$tmp/$asset"
	if [ -n "$sums" ]; then
		fetch "$sums" "$tmp/checksums.txt"
		expected="$(grep " $asset\$" "$tmp/checksums.txt" | cut -d' ' -f1)"
		[ -n "$expected" ] || fail "no checksum for $asset"
		actual="$(shasum -a 256 "$tmp/$asset" 2>/dev/null || sha256sum "$tmp/$asset")"
		[ "${actual%% *}" = "$expected" ] || fail "checksum mismatch for $asset"
	fi
	tar -xzf "$tmp/$asset" -C "$tmp" overload
	dir="${OVERLOAD_INSTALL_DIR:-$HOME/.local/bin}"
	mkdir -p "$dir"
	install -m 0755 "$tmp/overload" "$dir/overload"
	bin="$dir/overload"
	case ":$PATH:" in
	*":$dir:"*) ;;
	*) say "Note: add $dir to your PATH to run overload directly." ;;
	esac
}

if [ -z "${OVERLOAD_NO_BREW:-}" ] && command -v brew >/dev/null 2>&1 && [ "$version" = latest ] && [ -z "${OVERLOAD_DOWNLOAD_URL:-}" ]; then
	install_with_brew
else
	install_from_release
fi

say "Installed $("$bin" version) at $bin"
# shellcheck disable=SC2086
exec "$bin" install ${OVERLOAD_INSTALL_ARGS:-}
