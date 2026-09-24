#!/bin/bash
#
# Offline lifecycle tests for the installer's service handling.
#
# Covered failure modes:
#   - upgrade must not stop a running service before the release has passed the
#     fail-closed checks (an image-only or missing-asset release must fail with
#     the service still running);
#   - when a stop did happen and verification failed afterwards, the previous
#     binary must be started again instead of leaving the host without service;
#   - after a swap on a running service the installer must restart it, because a
#     plain start is a no-op and the old process would keep running;
#   - a failed start must be reported (non-zero, no success banner);
#   - a `deploy/sub2api` member must never overwrite the verified binary;
#   - asset downloads must ignore a hostile curlrc (-q --globoff);
#   - `rollback` without a version must detect the platform before listing;
#   - a failed public-IP lookup is advisory and must not abort a fresh install
#     before the service is started;
#   - a release that never passes verification must not replace the rollback
#     backup of the binary that is still installed;
#   - a chown that fails must abort before the swap, so the service is restored
#     around the binary that is actually installed, with no staging left behind;
#   - the rollback backup must be read from a path the service account cannot
#     write: an entry swapped for a link inside the service-writable INSTALL_DIR
#     between the installer's check and the root copy must not redirect that copy
#     onto a root-only file;
#   - the executable parked out of INSTALL_DIR while the swap is prepared must be
#     put back on every abort, including one that goes through set -e;
#   - when it cannot be put back (a directory planted at the install path), it must
#     be preserved rather than deleted with the staging directory, and no restored
#     service may be claimed for a start that never happened;
#   - the installed executable, which the service account can replace at will, must
#     never be executed to report its version: the version comes from the root-owned
#     stamp beside INSTALL_DIR, and a planted executable (one that records being run)
#     must stay unexecuted through `upgrade`, `install_version` and
#     `get_current_version`;
#   - that stamp is evidence only about the executable it was written for: a binary
#     replaced out of band, as the in-app updater replaces it without rewriting the
#     stamp, reports an unknown version, so `install_version` of the version the
#     stale stamp still names installs that release instead of exiting successfully
#     without installing anything, and the rollback backup it takes is not named
#     after the version the replaced binary is not - while a stamp that still matches
#     the executable it recorded keeps short-circuiting an install of that version.
#
# Safety: systemctl, id, uname, useradd/usermod/userdel and chown are replaced by
# offline stand-ins that record calls and change nothing, curl is replaced by a
# fixture server, and INSTALL_DIR is an isolated temporary directory. No service
# is started or stopped, no user or file outside the temp directory is touched,
# and no request leaves the machine.
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
# The installer under test. INSTALLER_UNDER_TEST is a verification seam for the
# injected-swap case at the end of this file: pointed at a build without the
# parked-entry fix, that case must fail (green here, red there).
INSTALLER="${INSTALLER_UNDER_TEST:-$ROOT_DIR/deploy/install.sh}"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

fail() {
    echo "install-service-lifecycle: $1" >&2
    exit 1
}

INSTALL_DIR="$WORK_DIR/install"
# The version stamp the installer keeps beside INSTALL_DIR. It is named here, at the
# top, because cases on both sides of this file read it.
VERSION_STAMP="$(dirname "$INSTALL_DIR")/.sub2api.version"
MOCKBIN="$WORK_DIR/bin"
FIXTURES="$WORK_DIR/fixtures"
ASSETS="$FIXTURES/assets"
SYSTEMCTL_LOG="$WORK_DIR/systemctl-calls"
SYSTEMCTL_STATE="$WORK_DIR/systemctl-state"
# Counts the start calls the stand-in has served. It lets a case fail only the first
# start - the one the installer itself performs - while the later start the EXIT trap
# performs against the binary the swap installed still succeeds, which is the only way
# to observe what that repair start reports.
SYSTEMCTL_START_COUNT="$WORK_DIR/systemctl-start-count"
CURL_LOG="$WORK_DIR/curl-calls"
STUB_LOG="$WORK_DIR/stub-calls"
# Records the injected attacker swap, so the case that uses it cannot pass without
# the injection having really run.
SWAP_LOG="$WORK_DIR/swap-calls"
for file in "$SYSTEMCTL_LOG" "$CURL_LOG" "$STUB_LOG" "$SWAP_LOG"; do : > "$file"; done
mkdir -p "$MOCKBIN" "$FIXTURES/tags" "$ASSETS"

# --------------------------------------------------------------- stand-ins ----
cat > "$MOCKBIN/systemctl" <<'MOCK'
#!/bin/bash
set -uo pipefail
sub="${1:-}"
shift || true
printf '%s %s\n' "$sub" "$*" >> "$SYSTEMCTL_LOG"
active="inactive"
[ -f "$SYSTEMCTL_STATE" ] && active=$(cat "$SYSTEMCTL_STATE")
case "$sub" in
    is-active|is-enabled)
        [ "$active" = "active" ]
        ;;
    stop)
        printf 'inactive\n' > "$SYSTEMCTL_STATE"
        exit 0
        ;;
    start)
        if [ "${SYSTEMCTL_START_FAILS:-}" = "true" ]; then
            printf 'inactive\n' > "$SYSTEMCTL_STATE"
            echo "Job for sub2api.service failed" >&2
            exit 1
        fi
        # Fail only the first SYSTEMCTL_START_FAIL_TIMES starts (when set), so the
        # repair start that follows can still succeed and be observed.
        if [ -n "${SYSTEMCTL_START_FAIL_TIMES:-}" ]; then
            served=0
            [ -f "${SYSTEMCTL_START_COUNT:-}" ] && served=$(cat "$SYSTEMCTL_START_COUNT")
            served=$((served + 1))
            printf '%s\n' "$served" > "$SYSTEMCTL_START_COUNT"
            if [ "$served" -le "$SYSTEMCTL_START_FAIL_TIMES" ]; then
                printf 'inactive\n' > "$SYSTEMCTL_STATE"
                echo "Job for sub2api.service failed" >&2
                exit 1
            fi
        fi
        printf 'active\n' > "$SYSTEMCTL_STATE"
        exit 0
        ;;
    restart)
        if [ "${SYSTEMCTL_RESTART_FAILS:-}" = "true" ]; then
            printf 'inactive\n' > "$SYSTEMCTL_STATE"
            echo "Job for sub2api.service failed" >&2
            exit 1
        fi
        printf 'active\n' > "$SYSTEMCTL_STATE"
        exit 0
        ;;
    *)
        exit 0
        ;;
esac
MOCK
chmod +x "$MOCKBIN/systemctl"
printf 'active\n' > "$SYSTEMCTL_STATE"

# Recorded no-ops: these must never reach the real host tools during a test.
for stub in id uname useradd userdel usermod chown groupadd; do
    cat > "$MOCKBIN/$stub" <<MOCK
#!/bin/bash
printf '%s %s\n' "$stub" "\$*" >> "$STUB_LOG"
case "$stub" in
    id) [ "\${1:-}" = "-u" ] && { echo 0; exit 0; }; echo 0; exit 0 ;;
    uname) case "\${1:-}" in -s) echo Linux ;; -m) echo x86_64 ;; *) echo Linux ;; esac; exit 0 ;;
    chown) [ "\${CHOWN_FAILS:-}" = "true" ] && exit 1; exit 0 ;;
    *) exit 0 ;;
esac
MOCK
    chmod +x "$MOCKBIN/$stub"
done

# cp delegates to the real tool but can be told to fail on the binary swap, to
# exercise an abort that has no explicit error branch (set -e). The staged binary
# is the copy whose destination is "sub2api" (the backup lands as
# "sub2api.backup", deploy members under their own names), so the pattern matches
# exactly that one copy.
#
# It can also play the account that can write INSTALL_DIR, which is what the
# backup copy has to be defended against: on the copy whose destination is the
# rollback backup, and thus immediately before the real cp opens its source, the
# installed executable is replaced by the attacker's entry - a link where the host
# can make one, and the sentinel's content otherwise, which models the same thing,
# a path that no longer resolves to the binary the installer validated. A real
# host is the reason this is a link: the account cannot read a root-only file, it
# can only point the path at it, and the root copy then lands it where the account
# can read it.
REAL_CP=$(command -v cp)
cat > "$MOCKBIN/cp" <<MOCK
#!/bin/bash
last="\${@: -1}"
if [ "\${CP_FAILS:-}" = "true" ]; then
    case "\$last" in
        */sub2api|*.sub2api.new*) echo "cp: simulated failure" >&2; exit 1 ;;
    esac
fi
if [ "\${CP_SWAPS_SOURCE:-}" = "true" ]; then
    case "\$last" in
        */sub2api.backup)
            printf 'planted %s\n' "\$last" >> "\$SWAP_LOG"
            rm -f "\$INSTALL_DIR/sub2api"
            MSYS=winsymlinks:nativestrict ln -s "\$ROOT_ONLY_SENTINEL" "\$INSTALL_DIR/sub2api" 2>/dev/null || true
            if [ ! -L "\$INSTALL_DIR/sub2api" ]; then
                "$REAL_CP" "\$ROOT_ONLY_SENTINEL" "\$INSTALL_DIR/sub2api"
            fi
            ;;
    esac
fi
exec "$REAL_CP" "\$@"
MOCK
chmod +x "$MOCKBIN/cp"

# mv delegates to the real tool, but can play the local attacker winning the
# destination race: when the destination matches MV_PLANT_DIR_AT, that path is
# replaced by a directory immediately before the real mv runs, so the staged file
# is moved into it and the install path stays a directory.
REAL_MV=$(command -v mv)
cat > "$MOCKBIN/mv" <<MOCK
#!/bin/bash
last="\${@: -1}"
if [ -n "\${MV_PLANT_DIR_AT:-}" ] && [ "\$last" = "\$MV_PLANT_DIR_AT" ]; then
    rm -f "\$last"
    mkdir "\$last"
fi
exec "$REAL_MV" "\$@"
MOCK
chmod +x "$MOCKBIN/mv"

# curl stand-in: serves release JSON and assets from the fixture directory and
# records its full argv so the download flags can be asserted.
cat > "$MOCKBIN/curl" <<'MOCK'
#!/bin/bash
set -uo pipefail
printf '%s\n' "$*" >> "$CURL_LOG"
out=""
url=""
prev=""
for arg in "$@"; do
    if [ -n "$prev" ]; then
        [ "$prev" = "-o" ] && out="$arg"
        prev=""
        continue
    fi
    case "$arg" in
        -o|--output) prev="-o" ;;
        --connect-timeout|--max-time) prev="$arg" ;;
        -q|--globoff|-s|--silent|-f|--fail|-L|--location|-sfL|-sL|-sf|-) ;;
        -*) ;;
        *) url="$arg" ;;
    esac
done
case "$url" in
    https://api.github.com/repos/lwying/sub2api/releases/latest) src="$FIXTURE_DIR/latest.json" ;;
    https://api.github.com/repos/lwying/sub2api/releases/tags/*) src="$FIXTURE_DIR/tags/${url##*/}.json" ;;
    https://api.github.com/repos/lwying/sub2api/releases|https://api.github.com/repos/lwying/sub2api/releases\?*) src="$FIXTURE_DIR/releases.json" ;;
    https://ipinfo.io/json)
        # Public-IP lookup: emulate "no network", like a host that cannot reach
        # ipinfo.io. curl exits non-zero with no output, which is not a harness
        # error but the documented fallback path.
        printf 'curl: (7) Failed to connect to ipinfo.io port 443\n' >&2
        exit 7
        ;;
    https://github.com/lwying/sub2api/releases/download/*/*)
        rest="${url#https://github.com/lwying/sub2api/releases/download/}"
        src="$FIXTURE_DIR/assets/${rest}"
        ;;
    *) echo "mock curl: unexpected URL: $url" >&2; exit 2 ;;
esac
if [ ! -f "$src" ]; then
    printf 'curl: (22) The requested URL returned error: 404\n' >&2
    exit 22
fi
if [ -n "$out" ]; then cp "$src" "$out"; else cat "$src"; fi
MOCK
chmod +x "$MOCKBIN/curl"

# ---------------------------------------------------------------- fixtures ----
make_fake_binary() {
    printf '\177ELF' > "$1"
    printf '%s\n' "$2" >> "$1"
    chmod +x "$1"
}

write_release_json() {
    local tag="$1" out="$2"
    shift 2
    {
        printf '{\n'
        printf '  "tag_name": "%s",\n' "$tag"
        printf '  "name": "Sub2API %s",\n' "$tag"
        printf '  "draft": false,\n'
        printf '  "prerelease": false,\n'
        printf '  "assets": [\n'
        local first=1 asset
        for asset in "$@"; do
            if [ "$first" -eq 1 ]; then first=0; else printf ',\n'; fi
            printf '    {\n      "name": "%s"\n    }' "$asset"
        done
        printf '\n  ]\n}\n'
    } > "$out"
}

checksums_line() {
    printf '%s  %s\n' "$(sha256sum "$1" | awk '{print $1}')" "$(basename "$1")"
}

# A complete release whose archive verifies, with an optional extra deploy member.
make_release() { # <tag> <binary-marker> [extra-deploy-member-name]
    local tag="$1" marker="$2" extra="${3:-}"
    local stage="$WORK_DIR/stage-$tag"
    mkdir -p "$stage/deploy" "$ASSETS/$tag"
    make_fake_binary "$stage/sub2api" "$marker"
    printf 'file: README.md\n' > "$stage/deploy/archive-deploy-marker.txt"
    if [ -n "$extra" ]; then
        make_fake_binary "$stage/deploy/$extra" "$marker-deploy-member"
    fi
    tar -czf "$ASSETS/$tag/sub2api_${tag#v}_linux_amd64.tar.gz" -C "$stage" .
    checksums_line "$ASSETS/$tag/sub2api_${tag#v}_linux_amd64.tar.gz" > "$ASSETS/$tag/checksums.txt"
    write_release_json "$tag" "$FIXTURES/tags/$tag.json" \
        "sub2api_${tag#v}_linux_amd64.tar.gz" checksums.txt
}

# v0.2.6: a complete, verifiable release.
make_release v0.2.6 new-binary
# v0.2.7: complete archive, but checksums.txt does not match it.
mkdir -p "$ASSETS/v0.2.7" "$WORK_DIR/stage-v0.2.7"
make_fake_binary "$WORK_DIR/stage-v0.2.7/sub2api" broken-release
tar -czf "$ASSETS/v0.2.7/sub2api_0.2.7_linux_amd64.tar.gz" -C "$WORK_DIR/stage-v0.2.7" .
printf '%s  %s\n' "$(printf 'ab%.0s' $(seq 1 32))" \
    "sub2api_0.2.7_linux_amd64.tar.gz" > "$ASSETS/v0.2.7/checksums.txt"
write_release_json v0.2.7 "$FIXTURES/tags/v0.2.7.json" \
    sub2api_0.2.7_linux_amd64.tar.gz checksums.txt
# v0.3.0: image-only release (no archives, no checksums).
write_release_json v0.3.0 "$FIXTURES/tags/v0.3.0.json"
mkdir -p "$ASSETS/v0.3.0"
# v0.1.0: complete release whose archive also carries deploy/sub2api.
make_release v0.1.0 root-binary sub2api
# The fork's current latest is the image-only release.
write_release_json v0.3.0 "$FIXTURES/latest.json"

cat > "$FIXTURES/releases.json" <<'JSON'
[
  {
    "tag_name": "v0.2.6",
    "draft": false,
    "prerelease": false
  }
]
JSON

# ------------------------------------------------------------------ harness ---
run_snippet() {
    local snippet="$1"
    (
        export PATH="$MOCKBIN:$PATH" FIXTURE_DIR="$FIXTURES"
        export SYSTEMCTL_LOG SYSTEMCTL_STATE SYSTEMCTL_START_COUNT CURL_LOG STUB_LOG SWAP_LOG
        if [ -n "${SYSTEMCTL_START_FAILS:-}" ]; then export SYSTEMCTL_START_FAILS; fi
        if [ -n "${SYSTEMCTL_START_FAIL_TIMES:-}" ]; then export SYSTEMCTL_START_FAIL_TIMES; fi
        if [ -n "${SYSTEMCTL_RESTART_FAILS:-}" ]; then export SYSTEMCTL_RESTART_FAILS; fi
        if [ -n "${CP_FAILS:-}" ]; then export CP_FAILS; fi
        if [ -n "${CP_SWAPS_SOURCE:-}" ]; then export CP_SWAPS_SOURCE; fi
        if [ -n "${ROOT_ONLY_SENTINEL:-}" ]; then export ROOT_ONLY_SENTINEL; fi
        if [ -n "${MV_PLANT_DIR_AT:-}" ]; then export MV_PLANT_DIR_AT; fi
        if [ -n "${CHOWN_FAILS:-}" ]; then export CHOWN_FAILS; fi
        bash -c '
            set -o pipefail
            source <(head -n -1 "$1") || exit 1
            INSTALL_DIR="$3"
            export INSTALL_DIR
            OS="linux"
            ARCH="amd64"
            export OS ARCH
            eval "$2"
        ' bash "$INSTALLER" "$snippet" "$INSTALL_DIR"
    )
}

reset_install() {
    rm -rf "$INSTALL_DIR"
    rm -f "$SYSTEMCTL_START_COUNT"
    # The version stamp describes the executable at $INSTALL_DIR/sub2api by
    # construction. This helper replaces that executable outside the installer, as an
    # operator or the service account would, so the recorded version no longer
    # describes anything that is installed: the stamp goes with the directory it
    # belongs to. Leaving it behind would let the installer's same-version check skip
    # the cases below, which are written for a binary of unrecorded (unknown) version.
    rm -f "$(dirname "$INSTALL_DIR")/.sub2api.version"
    mkdir -p "$INSTALL_DIR"
    make_fake_binary "$INSTALL_DIR/sub2api" old-binary
    printf 'active\n' > "$SYSTEMCTL_STATE"
    : > "$SYSTEMCTL_LOG"
}

systemctl_calls() { cat "$SYSTEMCTL_LOG"; }

assert_service_untouched() {
    local label="$1"
    if grep -q '^stop' "$SYSTEMCTL_LOG"; then
        fail "$label: the service was stopped although the release never passed verification"
    fi
}

assert_service_restored() {
    local label="$1"
    if ! grep -q '^stop' "$SYSTEMCTL_LOG"; then
        fail "$label: expected the service to have been stopped for the swap"
    fi
    if ! grep -q '^start' "$SYSTEMCTL_LOG"; then
        fail "$label: the service was stopped and never started again"
    fi
}

# --------------------------------------------- upgrade: fail-closed ordering --
# The latest release is image-only: the upgrade must fail without stopping the
# running service, and the installed binary must stay untouched.
reset_install
RC=0
OUT=$(run_snippet "LANG_CHOICE=en; upgrade" 2>&1) || RC=$?
if [ "$RC" -eq 0 ]; then
    fail "upgrade succeeded although the latest release is image-only: $OUT"
fi
assert_service_untouched "image-only-latest"
grep -q 'old-binary' "$INSTALL_DIR/sub2api" ||
    fail "image-only-latest: the installed binary was replaced"
if [ -e "$INSTALL_DIR/sub2api.backup" ]; then
    fail "image-only-latest: a backup was taken before the release was verified"
fi
printf '%s' "$OUT" | grep -Fq 'not installable' ||
    fail "image-only-latest: unexpected failure cause: $OUT"

# A release whose checksum does not verify: the swap fails after the stop, so the
# previous binary must be started again.
reset_install
write_release_json v0.2.7 "$FIXTURES/latest.json" sub2api_0.2.7_linux_amd64.tar.gz checksums.txt
RC=0
OUT=$(run_snippet "LANG_CHOICE=en; upgrade" 2>&1) || RC=$?
if [ "$RC" -eq 0 ]; then
    fail "upgrade succeeded although the checksum did not verify: $OUT"
fi
assert_service_restored "checksum-mismatch"
grep -q 'old-binary' "$INSTALL_DIR/sub2api" ||
    fail "checksum-mismatch: the installed binary was replaced"
printf '%s' "$OUT" | grep -Fq 'Previous service restored' ||
    fail "checksum-mismatch: the restore was not reported: $OUT"

# The rollback backup may only be written by an installation that really swaps the
# binary: a release that fails verification must leave the backup the operator
# already had in place, otherwise a failed upgrade destroys the last known-good
# binary.
reset_install
printf 'prior-rollback\n' > "$INSTALL_DIR/sub2api.backup"
write_release_json v0.2.7 "$FIXTURES/latest.json" sub2api_0.2.7_linux_amd64.tar.gz checksums.txt
RC=0
OUT=$(run_snippet "LANG_CHOICE=en; upgrade" 2>&1) || RC=$?
if [ "$RC" -eq 0 ]; then
    fail "upgrade succeeded although the checksum did not verify: $OUT"
fi
grep -q 'prior-rollback' "$INSTALL_DIR/sub2api.backup" ||
    fail "a failed upgrade overwrote the previous rollback backup"
grep -q 'old-binary' "$INSTALL_DIR/sub2api" ||
    fail "backup-preserved: the installed binary was replaced"

# Happy path: the service was stopped for the swap, so it must come back up (the
# newly started process is the new binary) and success may be reported.
reset_install
write_release_json v0.2.6 "$FIXTURES/latest.json" sub2api_0.2.6_linux_amd64.tar.gz checksums.txt
RC=0
OUT=$(run_snippet "LANG_CHOICE=en; upgrade" 2>&1) || RC=$?
if [ "$RC" -ne 0 ]; then
    fail "upgrade failed for a complete, verified release: $OUT"
fi
if ! grep -q '^start' "$SYSTEMCTL_LOG"; then
    fail "upgrade left the service down after the swap: $(systemctl_calls | tr '\n' ' ')"
fi
grep -q 'new-binary' "$INSTALL_DIR/sub2api" || fail "upgrade did not install the new binary"
printf '%s' "$OUT" | grep -Fq 'Upgrade completed' || fail "upgrade did not report success: $OUT"
# ... and the replaced binary was kept as the rollback backup, so deferring the
# backup to the swap did not drop the backup feature.
grep -q 'old-binary' "$INSTALL_DIR/sub2api.backup" ||
    fail "upgrade did not back up the binary it replaced"

# The no-version install dispatch ("install" without -v, and the default command)
# reinstalls the latest release over whatever is installed. Its swap replaces the
# installed binary by rename, so this path has to keep the replaced binary too:
# without a backup path the installed build is simply gone and the operator is left
# with no rollback point.
#
# The steps that reach outside INSTALL_DIR (unit file, user, directories) are stubbed;
# this case is about the dispatch keeping the replaced binary, and the suite must not
# write to the host. Everything from resolving the release to the swap and the backup
# runs for real.
INSTALL_STUBS='
    LANG_CHOICE=en
    select_language() { :; }
    configure_server() { :; }
    create_user() { :; }
    setup_directories() { :; }
    install_service() { :; }
    prepare_for_setup() { :; }
    get_public_ip() { :; }
'
for entry in 'main install' 'main'; do
    reset_install
    write_release_json v0.2.6 "$FIXTURES/latest.json" sub2api_0.2.6_linux_amd64.tar.gz checksums.txt
    : > "$SYSTEMCTL_LOG"
    RC=0
    OUT=$(run_snippet "$INSTALL_STUBS
        $entry
    " 2>&1) || RC=$?
    if [ "$RC" -ne 0 ]; then
        fail "install-no-version-backup ($entry): the install failed: $OUT"
    fi
    grep -q 'new-binary' "$INSTALL_DIR/sub2api" ||
        fail "install-no-version-backup ($entry): the release was not installed"
    grep -q 'old-binary' "$INSTALL_DIR/sub2api.backup" ||
        fail "install-no-version-backup ($entry): the replaced binary was not kept as a rollback backup"
    printf '%s' "$OUT" | grep -Fq 'Backup created' ||
        fail "install-no-version-backup ($entry): the backup was not reported: $OUT"
done

# Installing over an already-running service must restart it: a plain start would
# be a no-op and the OLD process would keep running while success was reported.
printf 'active\n' > "$SYSTEMCTL_STATE"
: > "$SYSTEMCTL_LOG"
RC=0
OUT=$(run_snippet "LANG_CHOICE=en; finish_fresh_install" 2>&1) || RC=$?
if [ "$RC" -ne 0 ]; then
    fail "finish_fresh_install failed with the service already running: $OUT"
fi
if ! grep -q '^restart' "$SYSTEMCTL_LOG"; then
    fail "install over a running service did not restart it: $(systemctl_calls | tr '\n' ' ')"
fi
printf '%s' "$OUT" | grep -Fq 'installation completed' ||
    fail "finish_fresh_install did not report completion: $OUT"

# A failing restart on a running service must be reported, not swallowed.
: > "$SYSTEMCTL_LOG"
if SYSTEMCTL_RESTART_FAILS=true run_snippet "finish_fresh_install" >/dev/null 2>&1; then
    fail "finish_fresh_install reported success although the restart failed"
fi
printf 'active\n' > "$SYSTEMCTL_STATE"

# --------------------------------------- public IP lookup is advisory ---------
# The lookup only feeds the completion banner, so it must not decide whether the
# install finishes: on a host that cannot reach the lookup endpoint the fallback
# is used, and the run must still reach (and report) the service start. The step
# before the lookup in a fresh install is the unit file; aborting here would leave
# a written unit and no running service.
printf 'inactive\n' > "$SYSTEMCTL_STATE"
: > "$SYSTEMCTL_LOG"
RC=0
OUT=$(run_snippet "LANG_CHOICE=en; get_public_ip; finish_fresh_install" 2>&1) || RC=$?
if [ "$RC" -ne 0 ]; then
    fail "a failed public-IP lookup aborted the install before the service was started: $OUT"
fi
if ! grep -q '^start' "$SYSTEMCTL_LOG"; then
    fail "a failed public-IP lookup skipped the service start: $(systemctl_calls | tr '\n' ' ')"
fi
printf '%s' "$OUT" | grep -Fq 'installation completed' ||
    fail "the install did not report completion after a failed public-IP lookup: $OUT"

# ------------------------------------------------ start_service honesty -------
reset_install
if ! run_snippet "start_service" >/dev/null 2>&1; then
    fail "start_service failed with the service already active"
fi
if ! grep -q '^restart' "$SYSTEMCTL_LOG"; then
    fail "start_service used a bare start on a running service: $(systemctl_calls | tr '\n' ' ')"
fi

printf 'inactive\n' > "$SYSTEMCTL_STATE"
: > "$SYSTEMCTL_LOG"
if ! run_snippet "start_service" >/dev/null 2>&1; then
    fail "start_service failed for an inactive service"
fi
grep -q '^start' "$SYSTEMCTL_LOG" || fail "start_service did not start an inactive service"
if grep -q '^restart' "$SYSTEMCTL_LOG"; then
    fail "start_service restarted an inactive service instead of starting it"
fi

printf 'active\n' > "$SYSTEMCTL_STATE"
: > "$SYSTEMCTL_LOG"
if SYSTEMCTL_RESTART_FAILS=true run_snippet "start_service" >/dev/null 2>&1; then
    fail "start_service reported success although the restart failed"
fi

# ----------------------------------- install_version: honest failure ----------
reset_install
: > "$SYSTEMCTL_LOG"
RC=0
OUT=$(SYSTEMCTL_START_FAILS=true run_snippet "LANG_CHOICE=en; install_version v0.2.6" 2>&1) || RC=$?
if [ "$RC" -eq 0 ]; then
    fail "install_version returned success although the service failed to start: $OUT"
fi
if printf '%s' "$OUT" | grep -Fq 'Specified version installed'; then
    fail "install_version claimed a completed installation after a failed start: $OUT"
fi
printf '%s' "$OUT" | grep -Fq 'The binary is in place but the service is not running' ||
    fail "install_version did not report the service as not restarted: $OUT"

# ----------------- a repair start after a completed swap is not the old service --
# The start that follows a completed swap is the installer's own start, and when it
# fails the abort goes through the EXIT trap. By then the executable at the install
# path is the NEW binary and the parked original is gone (it lives on as the rollback
# backup), so the service the trap starts runs that new binary - reporting it as
# "Previous service restored" would claim a rollback that never happened and hide
# which binary is actually running.
reset_install
write_release_json v0.2.6 "$FIXTURES/latest.json" sub2api_0.2.6_linux_amd64.tar.gz checksums.txt
: > "$SYSTEMCTL_LOG"
RC=0
OUT=$(SYSTEMCTL_START_FAIL_TIMES=1 run_snippet "LANG_CHOICE=en; upgrade" 2>&1) || RC=$?
if [ "$RC" -eq 0 ]; then
    fail "swap-then-start-fails: the upgrade reported success although the service did not come up: $OUT"
fi
grep -q 'new-binary' "$INSTALL_DIR/sub2api" ||
    fail "swap-then-start-fails: the verified binary was not installed"
if [ "$(cat "$SYSTEMCTL_STATE")" != "active" ]; then
    fail "swap-then-start-fails: the repair start did not bring the service back up"
fi
grep -q '^start' "$SYSTEMCTL_LOG" ||
    fail "swap-then-start-fails: the repair start never ran: $(systemctl_calls | tr '\n' ' ')"
if [ -z "$(cat "$SYSTEMCTL_START_COUNT" 2>/dev/null)" ] ||
    [ "$(cat "$SYSTEMCTL_START_COUNT")" -lt 2 ]; then
    fail "swap-then-start-fails: the failing first start and the repair start were not both exercised"
fi
printf '%s' "$OUT" | grep -Fq 'Previous service restored' &&
    fail "swap-then-start-fails: the previous service was claimed as restored although the new binary is installed: $OUT"
printf '%s' "$OUT" | grep -Fq 'Started the newly installed binary' ||
    fail "swap-then-start-fails: the started binary was not reported honestly: $OUT"

# ------------------------------------ deploy member must not shadow the binary --
reset_install
: > "$SYSTEMCTL_LOG"
RC=0
OUT=$(run_snippet "LANG_CHOICE=en; install_version v0.1.0" 2>&1) || RC=$?
if [ "$RC" -ne 0 ]; then
    fail "install_version failed for a verified release with a deploy member: $OUT"
fi
grep -q 'root-binary' "$INSTALL_DIR/sub2api" ||
    fail "a deploy/sub2api member overwrote the verified binary"
if grep -q 'deploy-member' "$INSTALL_DIR/sub2api"; then
    fail "the installed binary came from deploy/sub2api instead of the archive root"
fi
printf '%s' "$OUT" | grep -Fq 'skipped a deploy member' ||
    fail "the skipped deploy member was not reported: $OUT"

# ------------------------------------------------- curlrc hardening -----------
reset_install
: > "$CURL_LOG"
RC=0
OUT=$(run_snippet "LANG_CHOICE=en; install_version v0.2.6" 2>&1) || RC=$?
if [ "$RC" -ne 0 ]; then
    fail "install_version failed for a verified release: $OUT"
fi
grep -q 'releases/download/v0.2.6/sub2api_0.2.6_linux_amd64.tar.gz' "$CURL_LOG" ||
    fail "the archive was not downloaded from the fork release"
grep -q 'releases/download/v0.2.6/checksums.txt' "$CURL_LOG" ||
    fail "the checksum file was not downloaded from the fork release"
while IFS= read -r line; do
    case "$line" in
        *releases/download*)
            case "$line" in
                "-q --globoff -sfL"*)
                    ;;
                *)
                    fail "asset download does not start with '-q --globoff': $line"
                    ;;
            esac
            if [ "$(printf '%s' "$line" | grep -c 'https://')" -ne 1 ]; then
                fail "asset download argv carries more than one URL: $line"
            fi
            ;;
    esac
done < "$CURL_LOG"

# ------------------------------- rollback dispatch order (no version) ---------
# The candidate list is platform-filtered, so `rollback` without a version must
# detect the platform before listing. (Driven by inspection of the dispatch: the
# CLI language prompt needs a terminal, which a test harness cannot provide
# portably; the platform-filtered listing itself is exercised above and in
# install-fork-rollback-test.sh.)
ROLLBACK_BLOCK=$(sed -n '/^        rollback)/,/^            ;;/p' "$INSTALLER")
[ -n "$ROLLBACK_BLOCK" ] || fail "could not read the rollback dispatch from the installer"
if ! printf '%s\n' "$ROLLBACK_BLOCK" | awk '
    /detect_platform/ { seen = 1 }
    /list_versions/ { if (!seen) exit 1 }
' ; then
    fail "the rollback dispatch lists candidates before detecting the platform"
fi

# --------------------------------------------- EXIT trap ownership -----------
# A failure that aborts through set -e (no explicit error branch) must still
# restore a service that was stopped for the swap. That only works if exactly one
# EXIT trap exists and it is not replaced by a function that cleans temp files.
TRAP_COUNT=$(grep -c '^trap .* EXIT' "$INSTALLER")
if [ "$TRAP_COUNT" -ne 1 ]; then
    fail "expected exactly one EXIT trap in the installer, found $TRAP_COUNT"
fi
grep -q '^TEMP_DIR=""' "$INSTALLER" || fail "the installer does not declare a global temp directory"
# A trap command anywhere in the function (ignoring comments) would replace the
# single EXIT trap that owns the service restore.
if awk '/^download_and_extract\(\) \{/,/^\}/' "$INSTALLER" | grep -qE '^[[:space:]]*[^#]*trap[[:space:]]'; then
    fail "download_and_extract installs its own EXIT trap, which would drop the service restore"
fi

# cp failing during the swap: the install must abort with the previous binary
# intact and the service running again.
reset_install
: > "$SYSTEMCTL_LOG"
RC=0
OUT=$(CP_FAILS=true run_snippet "LANG_CHOICE=en; install_version v0.2.6" 2>&1) || RC=$?
if [ "$RC" -eq 0 ]; then
    fail "install_version succeeded although the binary swap failed: $OUT"
fi
assert_service_restored "cp-failure-mid-swap"
grep -q 'old-binary' "$INSTALL_DIR/sub2api" ||
    fail "cp-failure-mid-swap: the installed binary was replaced by a failed swap"
printf '%s' "$OUT" | grep -Fq 'Previous service restored' ||
    fail "cp-failure-mid-swap: the restore was not reported: $OUT"

# chown failing: this must abort before the swap, so the abort brings the service
# back up around the binary that is actually installed. Swapping first and failing
# here would leave the installer starting a binary it never prepared while
# reporting the previous service as restored. `upgrade` is the entry point used
# here because it backs up to the fixed sub2api.backup path this case plants a
# rollback sentinel at.
reset_install
write_release_json v0.2.6 "$FIXTURES/latest.json" sub2api_0.2.6_linux_amd64.tar.gz checksums.txt
printf 'prior-rollback\n' > "$INSTALL_DIR/sub2api.backup"
: > "$SYSTEMCTL_LOG"
RC=0
OUT=$(CHOWN_FAILS=true run_snippet "LANG_CHOICE=en; upgrade" 2>&1) || RC=$?
if [ "$RC" -eq 0 ]; then
    fail "install_version succeeded although chown failed: $OUT"
fi
assert_service_restored "chown-failure"
grep -q 'old-binary' "$INSTALL_DIR/sub2api" ||
    fail "chown-failure: the new binary was installed although the installer could not prepare it"
printf '%s' "$OUT" | grep -Fq 'Previous service restored' ||
    fail "chown-failure: the restore was not reported: $OUT"
for leftover in "$INSTALL_DIR"/.sub2api.new* \
    "$(dirname "$INSTALL_DIR")"/.sub2api.stage.*; do
    [ -e "$leftover" ] || continue
    fail "chown-failure: the aborted install left staging behind: $leftover"
done
# The replacement was not ready, so the backup of the binary that is still
# installed must not have been replaced either.
grep -q 'prior-rollback' "$INSTALL_DIR/sub2api.backup" ||
    fail "chown-failure: the rollback backup was replaced although the install never happened"

# ------------------------------- backup source must be read out of reach -------
# The executable the rollback backup copies sits in INSTALL_DIR, which the service
# account can write (the in-app updater replaces the binary from there). A link
# planted at that path makes a root copy read whatever it points at - a root-only
# file - and land it as the rollback backup *inside* INSTALL_DIR, which that
# account can read. Checking the entry and then copying it in place cannot close
# that window, because the account can win it: the cp stand-in below performs the
# swap on the copy whose destination is the rollback backup, i.e. immediately
# before the real cp opens its source.
#
# The installer must park the entry into its own staging directory by rename, which
# moves an entry without reading it, and take the backup from there - a path the
# account cannot write. Against an installer that stages the backup straight from
# INSTALL_DIR, the assertions below fail on the backup's content: that is the leak,
# and it is what INSTALLER_UNDER_TEST=<build without the park> reproduces.
ROOT_ONLY_SENTINEL="$WORK_DIR/root-only-sentinel.txt"
printf 'ROOT-ONLY-SENTINEL-CONTENT\n' > "$ROOT_ONLY_SENTINEL"
reset_install
write_release_json v0.2.6 "$FIXTURES/latest.json" sub2api_0.2.6_linux_amd64.tar.gz checksums.txt
: > "$SYSTEMCTL_LOG"
: > "$SWAP_LOG"
RC=0
OUT=$(CP_SWAPS_SOURCE=true run_snippet "LANG_CHOICE=en; upgrade" 2>&1) || RC=$?
if [ "$RC" -ne 0 ]; then
    fail "backup-source-swap: the upgrade failed although the release verifies: $OUT"
fi
if [ ! -s "$SWAP_LOG" ]; then
    fail "backup-source-swap: the injected swap never ran, so this case proved nothing"
fi
if [ -L "$INSTALL_DIR/sub2api.backup" ] || [ ! -f "$INSTALL_DIR/sub2api.backup" ]; then
    fail "backup-source-swap: the rollback backup is not a regular file"
fi
if grep -q 'ROOT-ONLY-SENTINEL-CONTENT' "$INSTALL_DIR/sub2api.backup"; then
    fail "backup-source-swap: the root-only sentinel was copied into the service-readable rollback backup"
fi
grep -q 'old-binary' "$INSTALL_DIR/sub2api.backup" ||
    fail "backup-source-swap: the backup is not the installed binary the installer validated"
if [ "$(cat "$ROOT_ONLY_SENTINEL")" != "ROOT-ONLY-SENTINEL-CONTENT" ]; then
    fail "backup-source-swap: the root copy wrote through the planted entry"
fi
if [ -L "$INSTALL_DIR/sub2api" ]; then
    fail "backup-source-swap: the installed executable is a symlink after the swap"
fi
grep -q 'new-binary' "$INSTALL_DIR/sub2api" ||
    fail "backup-source-swap: the verified binary was not installed"
for leftover in "$(dirname "$INSTALL_DIR")"/.sub2api.stage.*; do
    [ -e "$leftover" ] || continue
    fail "backup-source-swap: the completed install left staging behind: $leftover"
done

# Control for that case: the assertions above are only evidence if the injection can
# fail the invariant. Running the same injection against the pre-park behaviour - the
# check, then the copy that reads that same path - must put the sentinel content into
# the backup. That is the leak the park closes, and this control fails if the
# injection stops reaching the copy it is aimed at (a renamed staging file, say), so
# the case above cannot quietly become vacuous.
reset_install
write_release_json v0.2.6 "$FIXTURES/latest.json" sub2api_0.2.6_linux_amd64.tar.gz checksums.txt
: > "$SYSTEMCTL_LOG"
: > "$SWAP_LOG"
CONTROL_RC=0
CONTROL_OUT=$(CP_SWAPS_SOURCE=true run_snippet '
    LANG_CHOICE=en
    # The pre-park code path: the entry is checked and then the same path is the
    # source of the copy, and an abort had no parked executable to put back.
    park_installed_executable() {
        if [ ! -f "$INSTALL_DIR/sub2api" ] || [ -L "$INSTALL_DIR/sub2api" ]; then
            return 1
        fi
        PARKED_ORIGINAL="$INSTALL_DIR/sub2api"
        return 0
    }
    restore_parked_original() { return 0; }
    upgrade
' 2>&1) || CONTROL_RC=$?
if [ "$CONTROL_RC" -ne 0 ]; then
    fail "backup-source-swap control: the upgrade failed: $CONTROL_OUT"
fi
if [ ! -s "$SWAP_LOG" ]; then
    fail "backup-source-swap control: the injected swap never reached the backup copy"
fi
grep -q 'ROOT-ONLY-SENTINEL-CONTENT' "$INSTALL_DIR/sub2api.backup" ||
    fail "backup-source-swap control: the injection no longer leaks, so the case above is not evidence"

# ------------------------------- an abort after the park puts it back ----------
# Once the entry is parked in the staging directory it is the only copy of the
# installed binary outside the rollback backup: an abort that deleted the staging
# directory instead of putting it back would leave the install path empty, with
# nothing to start. The abort here happens after the park (the rollback path is a
# directory, which is refused before the swap).
reset_install
write_release_json v0.2.6 "$FIXTURES/latest.json" sub2api_0.2.6_linux_amd64.tar.gz checksums.txt
mkdir "$INSTALL_DIR/sub2api.backup"
: > "$SYSTEMCTL_LOG"
RC=0
OUT=$(run_snippet "LANG_CHOICE=en; upgrade" 2>&1) || RC=$?
if [ "$RC" -eq 0 ]; then
    fail "park-restore: the upgrade succeeded although the rollback path is a directory"
fi
grep -q 'old-binary' "$INSTALL_DIR/sub2api" ||
    fail "park-restore: the abort did not put the parked installed binary back"
grep -q 'new-binary' "$INSTALL_DIR/sub2api" &&
    fail "park-restore: the aborted install left the new binary installed"
if ! grep -q '^start' "$SYSTEMCTL_LOG"; then
    fail "park-restore: the executable was back but the service was not restored"
fi
printf '%s' "$OUT" | grep -Fq 'Previous service restored' ||
    fail "park-restore: the restore was not reported: $OUT"
for leftover in "$(dirname "$INSTALL_DIR")"/.sub2api.stage.*; do
    [ -e "$leftover" ] || continue
    fail "park-restore: the aborted install left staging behind: $leftover"
done

# ------------------------------- a refused restore preserves the binary --------
# The destination race: a directory planted at the install path right before the
# swap swallows the staged binary, and the park can no longer be undone (moving the
# parked executable there would move it *into* the directory). The run must fail,
# must not claim a restored service, and must keep the parked executable: removing
# the staging directory would delete the only copy of the installed binary.
reset_install
write_release_json v0.2.6 "$FIXTURES/latest.json" sub2api_0.2.6_linux_amd64.tar.gz checksums.txt
: > "$SYSTEMCTL_LOG"
RC=0
OUT=$(MV_PLANT_DIR_AT="$INSTALL_DIR/sub2api" run_snippet "LANG_CHOICE=en; upgrade" 2>&1) || RC=$?
if [ "$RC" -eq 0 ]; then
    fail "restore-refused: the upgrade succeeded although the install path became a directory mid-swap"
fi
[ -d "$INSTALL_DIR/sub2api" ] ||
    fail "restore-refused: the raced destination directory disappeared"
# `|| true`: with no match `ls` fails, and under this file's pipefail the assignment
# would abort the whole suite through set -e with no message at all instead of
# reaching the check below.
PARKED=$(ls "$(dirname "$INSTALL_DIR")"/.sub2api.stage.*/sub2api.parked 2>/dev/null | head -1) || true
if [ -z "$PARKED" ]; then
    fail "restore-refused: the parked installed binary was deleted instead of preserved"
fi
grep -q 'old-binary' "$PARKED" ||
    fail "restore-refused: the preserved file is not the installed binary"
printf '%s' "$OUT" | grep -Fq 'Could not put the parked installed executable back' ||
    fail "restore-refused: the preserved executable was not reported: $OUT"
printf '%s' "$OUT" | grep -Fq 'Staging directory kept' ||
    fail "restore-refused: the staging directory holding it was not reported: $OUT"
printf '%s' "$OUT" | grep -Fq 'Previous service restored' &&
    fail "restore-refused: a restored service was claimed although nothing was started: $OUT"
if grep -q '^start' "$SYSTEMCTL_LOG"; then
    fail "restore-refused: a service was started around an install path with no executable"
fi
# The recovery hint has to name the directory that blocked the restore: the command
# it prints moves the parked executable into that directory, so following it without
# clearing the blocker restores nothing.
printf '%s' "$OUT" | grep -Fq 'A directory occupies the install path' ||
    fail "restore-refused: the hint did not name the blocking directory: $OUT"

# The hint itself, isolated: with a directory at the install path the automatic
# restore is refused, and the manual command offered must not be one that nests the
# file (`mv parked INSTALL_DIR/sub2api` moves the executable *into* that directory and
# leaves the install path a directory).
reset_install
rm -f "$INSTALL_DIR/sub2api"
mkdir "$INSTALL_DIR/sub2api"
HINT_DIR="$(dirname "$INSTALL_DIR")/.sub2api.stage.hint"
mkdir -p "$HINT_DIR"
printf 'old-binary\n' > "$HINT_DIR/sub2api.parked"
HINT_RC=0
HINT_OUT=$(run_snippet "
    LANG_CHOICE=en
    PARKED_ORIGINAL='$HINT_DIR/sub2api.parked'
    restore_parked_original
" 2>&1) || HINT_RC=$?
if [ "$HINT_RC" -eq 0 ]; then
    fail "restore-hint: the refused restore reported success: $HINT_OUT"
fi
[ -f "$HINT_DIR/sub2api.parked" ] ||
    fail "restore-hint: the parked executable was not preserved"
grep -q 'old-binary' "$HINT_DIR/sub2api.parked" ||
    fail "restore-hint: the preserved file is not the parked executable"
if [ -e "$INSTALL_DIR/sub2api/sub2api" ]; then
    fail "restore-hint: the parked executable was moved into the blocking directory"
fi
printf '%s' "$HINT_OUT" | grep -Fq 'A directory occupies the install path' ||
    fail "restore-hint: the blocking directory was not named: $HINT_OUT"
printf '%s' "$HINT_OUT" | grep -Fq "$INSTALL_DIR/sub2api" ||
    fail "restore-hint: the hint does not name the blocking install path: $HINT_OUT"
rm -rf "$HINT_DIR"

# ------------------- a stale recorded version is not the installed version ------
# The stamp is written by this installer, but the executable it describes can be
# replaced without the stamp being rewritten: the in-app updater swaps the binary in
# INSTALL_DIR, and it has no idea this file exists. The recorded version then belongs
# to a binary that is no longer installed, and install_version compares exactly that
# version against the release it was asked for. Reporting it would make a rollback to
# the version the stamp still names exit with "Already at this version, no action
# needed" while installing nothing at all - the operator asks for a known-good release
# after an update went wrong and the run reports success for a version that is not
# there - and the backup it takes on the way would be named after a version the binary
# being replaced is not, so the file a later rollback would pick up as "the v0.2.6
# binary" is some other build.
#
# The installer therefore records the sha256 of the executable it installed alongside
# the version, and reports the version only while that hash still matches the
# executable at the install path. Once it does not, the version is unknown: the
# requested release is really installed, and the replaced binary is backed up under a
# name that claims no version.

# A stamp that still matches the executable it recorded stays a no-op. The check
# exists to skip an install that would change nothing, and the fix must not turn every
# repeated install of the same version into a re-download and a service restart.
reset_install
: > "$SYSTEMCTL_LOG"
RC=0
OUT=$(run_snippet "LANG_CHOICE=en; install_version v0.1.0" 2>&1) || RC=$?
if [ "$RC" -ne 0 ]; then
    fail "stale-stamp: the first install of the fixture release failed: $OUT"
fi
grep -q 'root-binary' "$INSTALL_DIR/sub2api" ||
    fail "stale-stamp: the fixture release was not installed: $OUT"
: > "$SYSTEMCTL_LOG"
RC=0
OUT=$(run_snippet "LANG_CHOICE=en; install_version v0.1.0" 2>&1) || RC=$?
if [ "$RC" -ne 0 ]; then
    fail "stale-stamp: re-installing the recorded version failed: $OUT"
fi
printf '%s' "$OUT" | grep -Fq 'Already at this version, no action needed' ||
    fail "stale-stamp: the verified recorded version did not short-circuit the install: $OUT"
if grep -q '^stop' "$SYSTEMCTL_LOG"; then
    fail "stale-stamp: the service was stopped for an install that would change nothing"
fi

# Now replace that executable out of band, as the in-app updater would, and ask for
# the version the stamp still records. The stamp no longer describes what is
# installed, so it must not decide that the request is already satisfied.
make_fake_binary "$INSTALL_DIR/sub2api" replaced-out-of-band
: > "$SYSTEMCTL_LOG"
RC=0
OUT=$(run_snippet "LANG_CHOICE=en; install_version v0.1.0" 2>&1) || RC=$?
if [ "$RC" -ne 0 ]; then
    fail "stale-stamp: the rollback after an out-of-band replacement failed: $OUT"
fi
printf '%s' "$OUT" | grep -Fq 'Already at this version, no action needed' &&
    fail "stale-stamp: the stale recorded version turned the rollback into a silent no-op: $OUT"
grep -q '^stop' "$SYSTEMCTL_LOG" ||
    fail "stale-stamp: the requested release was never installed: $OUT"
grep -q 'root-binary' "$INSTALL_DIR/sub2api" ||
    fail "stale-stamp: the requested release was not installed over the replacement: $OUT"
# The version recorded for the replaced binary is not the version of the replacement,
# so the backup that holds the replacement must not carry it.
if [ -e "$INSTALL_DIR/sub2api.backup.v0.1.0" ]; then
    fail "stale-stamp: the rollback backup was named after the stale recorded version"
fi
FOUND_BACKUP=0
for candidate in "$INSTALL_DIR"/sub2api.backup.*; do
    [ -e "$candidate" ] || continue
    if grep -q 'replaced-out-of-band' "$candidate"; then
        FOUND_BACKUP=1
    fi
done
[ "$FOUND_BACKUP" -eq 1 ] ||
    fail "stale-stamp: the replaced binary was not kept as a rollback backup"
# The replacement was installed by this run, so the stamp it leaves behind describes
# the release that is now installed and the next repeat is a no-op again - the
# stale version is not carried forward.
grep -qx 'v0.1.0' "$VERSION_STAMP" 2>/dev/null ||
    fail "stale-stamp: the installed release's version was not recorded in $VERSION_STAMP"

# ---------------------- the installed executable is never run as root ----------
# INSTALL_DIR is chowned to the service account, so the file at
# $INSTALL_DIR/sub2api can be replaced by that account (the in-app updater does
# exactly that). Every version probe used to execute it as root: get_current_version,
# the probe inline in upgrade, and install_version through get_current_version. A
# planted executable therefore ran as root. The version is now read from the
# root-owned stamp beside INSTALL_DIR instead of being asked of the binary, and the
# planted executable (which records being run) must stay unexecuted.
SENTINEL="$WORK_DIR/planted-executable-ran"
EXEC_PROBE_RAN="$WORK_DIR/exec-probe-ran"

# A planted installed executable: running it records that it ran. The sentinel path is
# written into the script text, so the case cannot pass by recording somewhere else.
make_planted_binary() { # <path> <sentinel>
    cat > "$1" <<PLANTED
#!/bin/sh
printf 'the installed executable was run\n' >> '$2'
exit 0
PLANTED
    chmod +x "$1"
}

# Whether this host can execute the planted fixture at all. Probed by running one:
# MSYS-style hosts decide executability from a file's contents rather than from a bit,
# so the empty-file `test -x` idiom used elsewhere under-reports there. Running the
# fixture is the very operation this case turns on, so its verdict is the one that
# matters - and if it does not run, the case would pass for the wrong reason.
make_planted_binary "$WORK_DIR/exec-probe" "$EXEC_PROBE_RAN"
rm -f "$EXEC_PROBE_RAN"
HOST_EXEC_BIT=0
if "$WORK_DIR/exec-probe" >/dev/null 2>&1 && [ -s "$EXEC_PROBE_RAN" ]; then
    HOST_EXEC_BIT=1
fi

# The contract itself, which does not depend on this host's execute bit: a host whose
# installed release has no recorded version reports "unknown" rather than executing
# the binary to find out.
reset_install
[ "$(run_snippet 'LANG_CHOICE=en; get_current_version' 2>/dev/null)" = "unknown" ] ||
    fail "version-probe: a host with no recorded version did not report 'unknown'"

if [ "$HOST_EXEC_BIT" -ne 1 ]; then
    echo "install service lifecycle: host cannot execute the planted fixture; skipping the planted-executable cases"
else
    # --- get_current_version: no execution of the installed executable ----------
    # The probe above already proved this fixture is live: the same helper writes it,
    # so a probe that executes it cannot pass unnoticed.
    reset_install
    rm -f "$SENTINEL"
    make_planted_binary "$INSTALL_DIR/sub2api" "$SENTINEL"
    [ "$(run_snippet 'LANG_CHOICE=en; get_current_version' 2>/dev/null)" = "unknown" ] ||
        fail "version-probe: get_current_version did not report 'unknown' for a binary it must not run"
    if [ -e "$SENTINEL" ]; then
        fail "version-probe: get_current_version executed the installed executable as root"
    fi

    # --- upgrade: the probe inline in it must not execute the binary -------------
    # The latest release must be installable for the upgrade to reach its version
    # probe; it is restored to the image-only one the earlier cases use at the end.
    reset_install
    write_release_json v0.2.6 "$FIXTURES/latest.json" sub2api_0.2.6_linux_amd64.tar.gz checksums.txt
    rm -f "$SENTINEL"
    make_planted_binary "$INSTALL_DIR/sub2api" "$SENTINEL"
    : > "$SYSTEMCTL_LOG"
    RC=0
    OUT=$(run_snippet "LANG_CHOICE=en; upgrade" 2>&1) || RC=$?
    if [ -e "$SENTINEL" ]; then
        fail "upgrade-probe: upgrade executed the installed executable as root"
    fi
    if [ "$RC" -ne 0 ]; then
        fail "upgrade-probe: the upgrade of a verified release failed: $OUT"
    fi
    # The upgrade must have reached its swap, or the probe above was never exercised.
    grep -q 'new-binary' "$INSTALL_DIR/sub2api" ||
        fail "upgrade-probe: the upgrade never swapped the verified release in: $OUT"
    grep -qx 'v0.2.6' "$VERSION_STAMP" 2>/dev/null ||
        fail "upgrade-probe: the installed release's version was not recorded in $VERSION_STAMP"

    # --- install_version: reports the recorded version, never runs the binary ----
    reset_install
    rm -f "$SENTINEL"
    RC=0
    OUT=$(run_snippet "LANG_CHOICE=en; install_version v0.1.0" 2>&1) || RC=$?
    if [ "$RC" -ne 0 ]; then
        fail "install_version-probe: the install failed: $OUT"
    fi
    grep -q 'root-binary' "$INSTALL_DIR/sub2api" ||
        fail "install_version-probe: the verified release was not installed: $OUT"
    grep -qx 'v0.1.0' "$VERSION_STAMP" 2>/dev/null ||
        fail "install_version-probe: the installed release's version was not recorded in $VERSION_STAMP"
    printf '%s' "$OUT" | grep -Fq 'Current version: v0.1.0' ||
        fail "install_version-probe: the reported version did not come from the recorded release: $OUT"

    # A binary that replaced the installed one out of band must not be executed
    # either, and the version recorded for the binary it replaced must not be
    # reported for it: that version is exactly what install_version compares the
    # requested release against, so reporting it would make an install of that
    # release exit successfully without installing anything (see the stale-stamp
    # cases below, which assert the same defect for a binary of unknown version).
    make_planted_binary "$INSTALL_DIR/sub2api" "$SENTINEL"
    rm -f "$SENTINEL"
    : > "$SYSTEMCTL_LOG"
    RC=0
    OUT=$(run_snippet "LANG_CHOICE=en; install_version v0.1.0" 2>&1) || RC=$?
    if [ -e "$SENTINEL" ]; then
        fail "install_version-probe: install_version executed the installed executable as root"
    fi
    if [ "$RC" -ne 0 ]; then
        fail "install_version-probe: re-installing after an out-of-band replacement failed: $OUT"
    fi
    printf '%s' "$OUT" | grep -Fq 'Already at this version, no action needed' &&
        fail "install_version-probe: the stale recorded version made the install a silent no-op: $OUT"
    grep -q '^stop' "$SYSTEMCTL_LOG" ||
        fail "install_version-probe: the requested release was never installed: $OUT"
    grep -q 'root-binary' "$INSTALL_DIR/sub2api" ||
        fail "install_version-probe: the requested release was not installed: $OUT"
    if [ -e "$INSTALL_DIR/sub2api.backup.v0.1.0" ]; then
        fail "install_version-probe: the rollback backup was named after the stale recorded version"
    fi

    # --- a stamp that cannot be recorded is reported, not fatal ------------------
    # The installation itself is complete at that point, so the run must not claim
    # failure; the version is simply not recorded, which the next run reports.
    reset_install
    rm -f "$SENTINEL"
    rm -f "$VERSION_STAMP"
    mkdir "$VERSION_STAMP"
    RC=0
    OUT=$(run_snippet "LANG_CHOICE=en; install_version v0.1.0" 2>&1) || RC=$?
    # The planted directory received the staged stamp instead of being replaced by it;
    # remove it so nothing below reads it as a stamp.
    rm -rf "$VERSION_STAMP"
    if [ "$RC" -ne 0 ]; then
        fail "stamp-unwritable: an install whose version could not be recorded failed: $OUT"
    fi
    grep -q 'root-binary' "$INSTALL_DIR/sub2api" ||
        fail "stamp-unwritable: the verified release was not installed: $OUT"
    printf '%s' "$OUT" | grep -Fq 'Could not record the installed version' ||
        fail "stamp-unwritable: the unrecorded version was not reported: $OUT"
fi

# Restore the fixture the earlier cases are written against: the latest release is
# image-only.
write_release_json v0.3.0 "$FIXTURES/latest.json"

echo "install service lifecycle checks passed"
