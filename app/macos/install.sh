#!/bin/sh
# Install a development build without changing the login service registration.
set -eu
fail() { printf 'hush: %s\n' "$*" >&2; exit 1; }

[ "$(uname -s)" = Darwin ] || fail "Hush.app is macOS-only"
[ "$(id -u)" != 0 ] || fail "run as the login user, not with sudo"
[ "$#" = 3 ] || fail "usage: $0 BUILT_APP INSTALLED_APP BACKEND_BUNDLE"
[ -z "${HUSH_SOCKET:-}" ] || fail "unset HUSH_SOCKET to update the local installation"
source=$1 dest=$2
bundle=$(cd "$(dirname "$3")" && printf '%s/%s\n' "$(pwd -P)" "$(basename "$3")")
new_cli=$source/Contents/MacOS/hush
old_cli=$dest/Contents/MacOS/hush
[ -x "$new_cli" ] && [ -f "$bundle" ] && [ -f "$bundle.wasm-pack" ] || fail "build Hush.app and the backend first"
case "$dest" in /*/Hush.app) ;; *) fail "expected an absolute destination ending in /Hush.app" ;; esac

# The service pins both paths. Refuse to disrupt a different checkout or an
# embedded installation: this target builds a development CLI, not a release.
set -- "$HOME/Library/LaunchAgents"/io.github.javiermolinar.hush.*.plist
hold=$HOME/.hush/supervisor-hold-v1
manage_service=no
if [ -e "$1" ]; then
    [ "$#" = 1 ] || fail "more than one Hush LaunchAgent; inspect the service definitions before installing"
    program_arg() { /usr/libexec/PlistBuddy -c "Print :ProgramArguments:$1" "$definition"; }
    definition=$1
    [ "$(program_arg 0)" = "$old_cli" ] && [ "$(program_arg 4)" = "$bundle" ] ||
        fail "the installed service does not use $dest and this checkout's backend; refusing to replace it"
    if [ ! -e "$hold" ]; then
        manage_service=yes
    fi
fi

# Stage and verify next to the destination before stopping the service. The
# final moves don't merge bundles and the old app stays available for rollback.
parent=$(dirname "$dest")
stage=$(mktemp -d "$parent/.hush-install.XXXXXX")
committed=no
cleanup() {
    result=$?
    trap - EXIT HUP INT TERM
    if [ "$manage_service" = yes ] && [ -e "$hold" ]; then
        printf 'hush: the service is paused; resume with %s service resume\n' "$old_cli" >&2
    fi
    if [ "$committed" = no ] && { [ -e "$stage/previous" ] || [ -L "$stage/previous" ]; }; then
        if [ -e "$dest" ] || [ -L "$dest" ] || ! mv "$stage/previous" "$dest"; then
            fail "restore the previous app manually from $stage/previous"
        fi
    fi
    rm -rf "$stage"
    exit "$result"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
/usr/bin/ditto "$source" "$stage/Hush.app"
/usr/bin/codesign --verify --deep --strict "$stage/Hush.app"

if [ "$manage_service" = yes ]; then
    "$new_cli" service pause
fi
if [ -e "$dest" ] || [ -L "$dest" ]; then
    mv "$dest" "$stage/previous"
fi
mv "$stage/Hush.app" "$dest"
committed=yes
printf 'Installed %s\n' "$dest"

if [ "$manage_service" = yes ]; then
    "$old_cli" service resume
    attempts=0
    until "$old_cli" service status >/dev/null 2>&1; do
        attempts=$((attempts + 1))
        [ "$attempts" -lt 10 ] || fail "the service did not become ready; run $old_cli service status"
        sleep 1
    done
fi
