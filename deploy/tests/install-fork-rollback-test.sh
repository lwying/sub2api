#!/bin/bash
#
# Focused offline tests for the fork installer's install/rollback safety gate.
#
# What is covered:
#   - only strict fork tags (vX.Y.Z, no leading zeros) are accepted;
#   - a release is installable only when THIS platform's archive AND
#     checksums.txt are published (image-only / missing-platform / missing
#     checksums all fail closed, before any download);
#   - a failed checksum, a missing checksums.txt, an unsafe archive path and an
#     archive without the binary all abort with the existing executable intact;
#   - a checksums.txt that is far larger than the manifest can be is refused
#     instead of being streamed into the temp directory;
#   - the reported version is never empty, the candidate list is not truncated
#     before filtering, and an asset only counts when it is in the assets array;
#   - nothing is staged inside INSTALL_DIR, which the service account owns: a
#     pre-created symlink there and a symlink-swap injected during the copy both
#     leave the linked file untouched and install a regular binary;
#   - a directory planted at the executable path (before or after the installer's
#     check) fails the install instead of silently receiving the binary;
#   - the download path never mutates the system (no systemctl/useradd/chown)
#     and never touches /opt/sub2api, the real install directory;
#   - every release request goes to the fork repository, never upstream.
#
# How it stays safe: `curl` is replaced by a mock that serves canned release JSON
# and archives from a fixture directory, every installer function runs in an
# isolated temporary INSTALL_DIR, and system-mutating commands are stubbed to
# record and fail. No test reaches GitHub, installs a binary or restarts a
# service.
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
INSTALLER="$ROOT_DIR/deploy/install.sh"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

fail() {
    echo "install-fork-rollback: $1" >&2
    exit 1
}

# ---------------------------------------------------------------- mock PATH ---
MOCK_BIN="$WORK_DIR/bin"
FORBIDDEN_LOG="$WORK_DIR/forbidden-calls"
CURL_LOG="$WORK_DIR/curl-calls"
mkdir -p "$MOCK_BIN"
: > "$FORBIDDEN_LOG"
: > "$CURL_LOG"

# Any attempt to touch the host (services, users, ownership) is a test failure.
for forbidden in systemctl useradd userdel usermod chown; do
    cat > "$MOCK_BIN/$forbidden" <<EOF
#!/bin/bash
printf '%s %s\n' "$forbidden" "\$*" >> "$FORBIDDEN_LOG"
exit 1
EOF
    chmod +x "$MOCK_BIN/$forbidden"
done

# The service account is answered as absent, so the ownership step the installer
# performs on a staged binary (before its swap) is skipped deterministically
# instead of depending on whether the host running the test happens to have it.
cat > "$MOCK_BIN/id" <<'EOF'
#!/bin/bash
exit 1
EOF
chmod +x "$MOCK_BIN/id"

# cp delegates to the real tool, but can play the local attacker that owns
# INSTALL_DIR: just before a copy opens its destination, a staging path inside
# INSTALL_DIR is replaced by a symlink to the sentinel outside it. The attacker is
# the service account, which can write INSTALL_DIR; a root-owned staging directory
# beside it is out of that account's reach, so nothing there is touched.
REAL_CP=$(command -v cp)
cat > "$MOCK_BIN/cp" <<MOCK
#!/bin/bash
last="\${@: -1}"
if [ "\${CP_SWAP_TO_SYMLINK:-}" = "true" ]; then
    case "\$last" in
        "\${INSTALL_DIR:-/nonexistent}"/.sub2api*)
            rm -f "\$last"
            ln -s "$WORK_DIR/staging-swap-sentinel.txt" "\$last"
            ;;
    esac
fi
exec "$REAL_CP" "\$@"
MOCK
chmod +x "$MOCK_BIN/cp"

# mv stand-in with two jobs. It speaks the BSD/macOS command line, which has no
# -T, so an option the real installer is not allowed to depend on fails loudly
# here. It can also play the local attacker winning the race: when the destination
# matches MV_PLANT_DIR_AT, that path is replaced by a directory immediately before
# the real mv runs, so the staged file is moved into it.
REAL_MV=$(command -v mv)
cat > "$MOCK_BIN/mv" <<MOCK
#!/bin/bash
last="\${@: -1}"
for arg in "\$@"; do
    case "\$arg" in
        -T|--no-target-directory|-[a-zA-Z]*T*)
            echo "mv: illegal option -- \$arg" >&2
            exit 64
            ;;
    esac
done
if [ -n "\${MV_PLANT_DIR_AT:-}" ] && [ "\$last" = "\$MV_PLANT_DIR_AT" ]; then
    rm -f "\$last"
    mkdir "\$last"
fi
exec "$REAL_MV" "\$@"
MOCK
chmod +x "$MOCK_BIN/mv"

# Offline curl stand-in: maps GitHub URLs onto fixture files and emulates curl's
# -f (HTTP error -> exit 22). It refuses upstream URLs outright.
cat > "$MOCK_BIN/curl" <<'MOCK'
#!/bin/bash
set -uo pipefail

out=""
url=""
prev=""
fail_on_error=0
for arg in "$@"; do
    if [ -n "$prev" ]; then
        case "$prev" in
            -o|--output) out="$arg" ;;
        esac
        prev=""
        continue
    fi
    case "$arg" in
        -o|--output|--connect-timeout|--max-time|--write-out|-w) prev="$arg" ;;
        -f|--fail|-sfL|-sf|-fL|--fail-with-body) fail_on_error=1 ;;
        -q|--globoff|-s|--silent|-L|--location|-sL|-) ;;
        -*) ;;
        "") ;;
        *) url="$arg" ;;
    esac
done

if [ -z "$url" ]; then
    echo "mock curl: no URL in: $*" >&2
    exit 2
fi
case "$url" in
    *Wei-Shaw/sub2api*)
        echo "mock curl: installer requested the upstream repository: $url" >&2
        exit 2
        ;;
esac
printf '%s\n' "$url" >> "$CURL_LOG"

case "$url" in
    https://api.github.com/repos/lwying/sub2api/releases/latest)
        src="$FIXTURE_DIR/latest.json"
        ;;
    https://api.github.com/repos/lwying/sub2api/releases/tags/*)
        src="$FIXTURE_DIR/tags/${url##*/}.json"
        ;;
    https://api.github.com/repos/lwying/sub2api/releases|https://api.github.com/repos/lwying/sub2api/releases\?*)
        src="$FIXTURE_DIR/releases.json"
        ;;
    https://github.com/lwying/sub2api/releases/download/*/*)
        rest="${url#https://github.com/lwying/sub2api/releases/download/}"
        src="$FIXTURE_DIR/assets/${rest}"
        ;;
    *)
        echo "mock curl: unexpected URL: $url" >&2
        exit 2
        ;;
esac

if [ ! -f "$src" ]; then
    # Real curl only fails on an HTTP error when -f is given; without it the 404
    # body is written and the exit status stays 0.
    if [ "$fail_on_error" -eq 1 ]; then
        printf 'curl: (22) The requested URL returned error: 404\n' >&2
        exit 22
    fi
    printf '{"message":"Not Found","documentation_url":"https://docs.github.com/rest"}\n'
    exit 0
fi
if [ -n "$out" ]; then
    cp "$src" "$out"
else
    cat "$src"
fi
MOCK
chmod +x "$MOCK_BIN/curl"

# ---------------------------------------------------------------- fixtures ----
FIXTURES="$WORK_DIR/fixtures"
mkdir -p "$FIXTURES/tags" "$FIXTURES/assets"

# GitHub-shaped release JSON: one field per line, exactly like the real API
# response, because the installer's parser is line based (it greps the
# "tag_name" line and matches asset "name" entries).
write_release_json() {
    local tag="$1"
    local out="$2"
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
            if [ "$first" -eq 1 ]; then
                first=0
            else
                printf ',\n'
            fi
            printf '    {\n'
            printf '      "name": "%s",\n' "$asset"
            printf '      "browser_download_url": "https://github.com/lwying/sub2api/releases/download/%s/%s"\n' \
                "$tag" "$asset"
            printf '    }'
        done
        printf '\n  ]\n}\n'
    } > "$out"
}

make_tag_fixture() {
    write_release_json "$1" "$FIXTURES/tags/$1.json" "${@:2}"
}

# Stand-in for a released Go binary. The installer checks the leading image
# magic, so the fixture carries ELF magic followed by a marker string.
make_fake_binary() {
    printf '\177ELF' > "$1"
    printf 'sub2api-test-binary\n' >> "$1"
    chmod +x "$1"
}

# A release archive as .goreleaser.yaml builds it: the sub2api executable plus
# the deploy/ directory, which also carries a nested tests/ tree - the archive
# ships deploy/* , and a recursive copy of that entry would put the test fixtures
# into the install directory.
stage_release_tree() {
    local stage="$1"
    mkdir -p "$stage/deploy/tests"
    make_fake_binary "$stage/sub2api"
    printf 'file: README.md\n' > "$stage/deploy/archive-deploy-marker.txt"
    printf '#!/bin/bash\necho fixture\n' > "$stage/deploy/tests/mock-fixture.sh"
}

checksums_line() {
    local file="$1"
    printf '%s  %s\n' "$(sha256sum "$file" | awk '{print $1}')" "$(basename "$file")"
}

ASSETS="$FIXTURES/assets"

# v0.2.6 - complete, installable release (the green path).
LINUX_ARCHIVE="sub2api_0.2.6_linux_amd64.tar.gz"
mkdir -p "$ASSETS/v0.2.6" "$WORK_DIR/stage-ok"
stage_release_tree "$WORK_DIR/stage-ok"
tar -czf "$ASSETS/v0.2.6/$LINUX_ARCHIVE" -C "$WORK_DIR/stage-ok" .
checksums_line "$ASSETS/v0.2.6/$LINUX_ARCHIVE" > "$ASSETS/v0.2.6/checksums.txt"
make_tag_fixture v0.2.6 \
    "$LINUX_ARCHIVE" checksums.txt \
    sub2api_0.2.6_darwin_arm64.tar.gz sub2api_0.2.6_windows_amd64.zip

# v0.2.5 - release JSON promises checksums.txt but the asset is not served (404).
mkdir -p "$ASSETS/v0.2.5" "$WORK_DIR/stage-no-checksums"
stage_release_tree "$WORK_DIR/stage-no-checksums"
tar -czf "$ASSETS/v0.2.5/sub2api_0.2.5_linux_amd64.tar.gz" -C "$WORK_DIR/stage-no-checksums" .
make_tag_fixture v0.2.5 sub2api_0.2.5_linux_amd64.tar.gz checksums.txt

# v0.2.4 - checksums.txt published but does not match the archive.
mkdir -p "$ASSETS/v0.2.4" "$WORK_DIR/stage-mismatch"
stage_release_tree "$WORK_DIR/stage-mismatch"
tar -czf "$ASSETS/v0.2.4/sub2api_0.2.4_linux_amd64.tar.gz" -C "$WORK_DIR/stage-mismatch" .
printf 'ab%.0s' $(seq 1 32) > "$WORK_DIR/wrong-hash"
printf '%s  %s\n' "$(cat "$WORK_DIR/wrong-hash")" "sub2api_0.2.4_linux_amd64.tar.gz" \
    > "$ASSETS/v0.2.4/checksums.txt"
make_tag_fixture v0.2.4 sub2api_0.2.4_linux_amd64.tar.gz checksums.txt

# v0.2.3 - archive member escapes the extraction directory.
mkdir -p "$ASSETS/v0.2.3" "$WORK_DIR/stage-escape"
printf 'pwned\n' > "$WORK_DIR/stage-escape/sub2api"
tar -czf "$ASSETS/v0.2.3/sub2api_0.2.3_linux_amd64.tar.gz" -C "$WORK_DIR/stage-escape" sub2api \
    --transform='s,^sub2api,../escape.txt,' 2>/dev/null
checksums_line "$ASSETS/v0.2.3/sub2api_0.2.3_linux_amd64.tar.gz" > "$ASSETS/v0.2.3/checksums.txt"
make_tag_fixture v0.2.3 sub2api_0.2.3_linux_amd64.tar.gz checksums.txt

# v0.2.2 - archive carries an absolute member path.
mkdir -p "$ASSETS/v0.2.2" "$WORK_DIR/stage-abs"
printf 'pwned\n' > "$WORK_DIR/stage-abs/sub2api"
tar -czf "$ASSETS/v0.2.2/sub2api_0.2.2_linux_amd64.tar.gz" -C "$WORK_DIR/stage-abs" sub2api \
    --transform='s,^sub2api,/tmp/sub2api-abs-escape.txt,' 2>/dev/null
checksums_line "$ASSETS/v0.2.2/sub2api_0.2.2_linux_amd64.tar.gz" > "$ASSETS/v0.2.2/checksums.txt"
make_tag_fixture v0.2.2 sub2api_0.2.2_linux_amd64.tar.gz checksums.txt

# v0.2.1 - valid archive, but it holds no sub2api executable.
mkdir -p "$ASSETS/v0.2.1" "$WORK_DIR/stage-no-binary"
printf 'not the binary\n' > "$WORK_DIR/stage-no-binary/README.md"
tar -czf "$ASSETS/v0.2.1/sub2api_0.2.1_linux_amd64.tar.gz" -C "$WORK_DIR/stage-no-binary" .
checksums_line "$ASSETS/v0.2.1/sub2api_0.2.1_linux_amd64.tar.gz" > "$ASSETS/v0.2.1/checksums.txt"
make_tag_fixture v0.2.1 sub2api_0.2.1_linux_amd64.tar.gz checksums.txt

# v0.3.0 - image-only release: no archives, no checksums.
make_tag_fixture v0.3.0
mkdir -p "$ASSETS/v0.3.0"

# v0.2.7 - binary release without this platform's archive.
mkdir -p "$ASSETS/v0.2.7"
printf 'other platform\n' > "$ASSETS/v0.2.7/sub2api_0.2.7_darwin_arm64.tar.gz"
sha256sum "$ASSETS/v0.2.7/sub2api_0.2.7_darwin_arm64.tar.gz" | awk '{print $1"  sub2api_0.2.7_darwin_arm64.tar.gz"}' \
    > "$ASSETS/v0.2.7/checksums.txt"
make_tag_fixture v0.2.7 checksums.txt sub2api_0.2.7_darwin_arm64.tar.gz

# v0.2.0 - a vX.Y.Z tag published as a prerelease. It carries every asset, but a
# prerelease must not become an install target for a numeric-compare updater.
mkdir -p "$ASSETS/v0.2.0" "$WORK_DIR/stage-prerelease"
stage_release_tree "$WORK_DIR/stage-prerelease"
tar -czf "$ASSETS/v0.2.0/sub2api_0.2.0_linux_amd64.tar.gz" -C "$WORK_DIR/stage-prerelease" .
checksums_line "$ASSETS/v0.2.0/sub2api_0.2.0_linux_amd64.tar.gz" > "$ASSETS/v0.2.0/checksums.txt"
{
    printf '{\n'
    printf '  "tag_name": "v0.2.0",\n'
    printf '  "name": "Sub2API v0.2.0",\n'
    printf '  "draft": false,\n'
    printf '  "prerelease": true,\n'
    printf '  "assets": [\n'
    printf '    {\n      "name": "sub2api_0.2.0_linux_amd64.tar.gz"\n    },\n'
    printf '    {\n      "name": "checksums.txt"\n    }\n'
    printf '  ]\n}\n'
} > "$FIXTURES/tags/v0.2.0.json"

# A tag that is not a fork version at all.
make_tag_fixture v0.1.0-rc.1 sub2api_0.1.0-rc.1_linux_amd64.tar.gz checksums.txt

# Release list for `list-versions` and `latest` for the default upgrade path.
cat > "$FIXTURES/releases.json" <<'JSON'
[
  {
    "tag_name": "v0.3.0",
    "draft": false,
    "prerelease": false
  },
  {
    "tag_name": "v0.2.7",
    "draft": false,
    "prerelease": false
  },
  {
    "tag_name": "v0.2.6",
    "draft": false,
    "prerelease": false
  },
  {
    "tag_name": "v0.2.0",
    "draft": false,
    "prerelease": true
  },
  {
    "tag_name": "v0.1.0-rc.1",
    "draft": false,
    "prerelease": false
  }
]
JSON
# The fork's current "latest" is the image-only release; the upgrade path must
# refuse it. A later case repoints this at the complete v0.2.6 release.
write_release_json v0.3.0 "$FIXTURES/latest.json"

# ------------------------------------------------- hostile archive support ---
# Member NAMES are checked for absolute paths and "..", but that alone does not
# stop a symlink member such as `deploy -> /elsewhere` followed by a
# `deploy/pwned.txt` member: the later write follows the link out of the
# extraction root. A symlink member named `sub2api` is the same class of problem
# for the copy that installs the binary. These fixtures exercise that, plus
# hardlink members.
#
# The archives are written byte by byte because this host cannot create symlinks
# from the filesystem (`ln -s` produces a directory), and GNU tar dereferences a
# symlink named on its own command line. Each fixture therefore ships a manifest
# that the mock tar below turns into the listing and extraction behaviour real
# tar would exhibit on a host that can create links.
REAL_TAR=$(command -v tar)
MOCK_TAR_BIN="$WORK_DIR/mocktar-bin"
TAR_LOG="$WORK_DIR/tar-calls"
mkdir -p "$MOCK_TAR_BIN"
: > "$TAR_LOG"

ustar_zeros() {
    local n="$1"
    if [ "$n" -gt 0 ]; then printf '\0%.0s' $(seq 1 "$n"); fi
}

# NUL-padded fixed-width header field
ustar_field() {
    local value="$1" width="$2"
    printf '%s' "$value"
    ustar_zeros $(( width - ${#value} ))
}

ustar_header() {
    local type="$1" name="$2" size="$3" linkname="$4"
    ustar_field "$name" 100
    printf '0000644\0'
    printf '0000000\0'
    printf '0000000\0'
    printf '%011o\0' "$size"
    printf '%011o\0' "$(date +%s)"
    printf '        ' # checksum placeholder
    printf '%s' "$type"
    ustar_field "$linkname" 100
    printf 'ustar\0'
    printf '00'
    ustar_field root 32
    ustar_field root 32
    printf '0000000\0'
    printf '0000000\0'
    ustar_zeros 155
    ustar_zeros 12
}

# Append one member to $ARCHIVE_PATH (regular files carry $2's contents).
ustar_emit() {
    local type="$1" name="$2" linkname="$3" content="${4:-}"
    local size=0 bytes sum remainder
    if [ -n "$content" ]; then size=$(wc -c < "$content"); fi
    ustar_header "$type" "$name" "$size" "$linkname" > "$WORK_DIR/hdr.bin"
    bytes=$(wc -c < "$WORK_DIR/hdr.bin")
    if [ "$bytes" -ne 512 ]; then
        fail "ustar header is $bytes bytes, expected 512"
    fi
    sum=$(od -An -v -tu1 "$WORK_DIR/hdr.bin" | awk '{ for (i = 1; i <= NF; i++) s += $i } END { print s + 0 }')
    printf '%06o\0 ' "$sum" | dd of="$WORK_DIR/hdr.bin" bs=1 seek=148 conv=notrunc status=none
    cat "$WORK_DIR/hdr.bin" >> "$ARCHIVE_PATH"
    if [ -n "$content" ]; then
        cat "$content" >> "$ARCHIVE_PATH"
        remainder=$(( size % 512 ))
        if [ "$remainder" -ne 0 ]; then
            ustar_zeros $(( 512 - remainder )) >> "$ARCHIVE_PATH"
        fi
    fi
}

# make_hostile_archive <tag> <spec>...
# spec: "<type>|<name>|<linkname>|<content-file>" with type l (symlink),
# h (hardlink), 0 (file) or d (directory).
make_hostile_archive() {
    local tag="$1"
    shift
    local dir="$ASSETS/$tag"
    mkdir -p "$dir" "$WORK_DIR/hostile-$tag"
    local manifest="$WORK_DIR/hostile-$tag.manifest"
    : > "$manifest"
    ARCHIVE_PATH="$dir/sub2api_${tag#v}_linux_amd64.tar.gz"
    : > "$ARCHIVE_PATH"
    local spec type name linkname content
    for spec in "$@"; do
        IFS='|' read -r type name linkname content <<< "$spec"
        printf '%s|%s|%s|%s\n' "$type" "$name" "$linkname" "$content" >> "$manifest"
        case "$type" in
            l) ustar_emit 2 "$name" "$linkname" ;;
            h) ustar_emit 1 "$name" "$linkname" ;;
            d) ustar_emit 5 "$name" "" ;;
            0) ustar_emit 0 "$name" "" "$content" ;;
            *) fail "unknown hostile member type: $type" ;;
        esac
    done
    ustar_zeros 1024 >> "$ARCHIVE_PATH"
    gzip -c "$ARCHIVE_PATH" > "$ARCHIVE_PATH.gz"
    mv "$ARCHIVE_PATH.gz" "$ARCHIVE_PATH"
    checksums_line "$ARCHIVE_PATH" > "$dir/checksums.txt"
    cp "$manifest" "$dir/manifest"
    write_release_json "$tag" "$FIXTURES/tags/$tag.json" \
        "$(basename "$ARCHIVE_PATH")" checksums.txt
    HOSTILE_MANIFEST="$manifest"
}

# Offline tar stand-in. It answers from a manifest instead of the archive bytes
# and models the semantics the installer depends on:
#   - -tzf / -tvzf listing, with injectable failure (TAR_FAIL_LIST) and empty
#     output (TAR_EMPTY_LIST) so the installer's exit-status handling is tested;
#   - -xOzf <archive> -- <member>: single-member streaming to stdout, matching
#     GNU tar's prefix matching (operand "deploy" also selects "deploy/x"), and
#     never writing to the filesystem;
#   - full-tree -xzf extraction is still modelled so a regression back to it is
#     visible in $TAR_LOG (the tests assert it is never used).
cat > "$MOCK_TAR_BIN/tar" <<'MOCKTAR'
#!/bin/bash
set -uo pipefail
op=""
dest="."
archive=""
member=""
expect_dir=0
after_ddash=0
for arg in "$@"; do
    if [ "$expect_dir" -eq 1 ]; then
        dest="$arg"
        expect_dir=0
        continue
    fi
    if [ "$after_ddash" -eq 1 ]; then
        member="$arg"
        continue
    fi
    case "$arg" in
        -C) expect_dir=1 ;;
        --) after_ddash=1 ;;
        -tvzf|-tv|-tvf) op="listv" ;;
        -tzf|-tz|-t) op="list" ;;
        -xOzf|-xOz|-xO) op="stream" ;;
        -xzf|-xz|-x) op="extract" ;;
        -*) ;;
        *) archive="$arg" ;;
    esac
done
printf '%s\t%s\t%s\t%s\n' "$op" "$archive" "$dest" "$member" >> "$TAR_LOG"
if [ -z "$op" ]; then
    echo "mock tar: unsupported invocation: $*" >&2
    exit 2
fi
manifest=${TAR_MANIFEST:-}
[ -f "$manifest" ] || { echo "mock tar: no manifest for $archive" >&2; exit 2; }

member_content() { # prints the bytes a -xO read of this member would produce
    local type="$1" name="$2" linkname="$3" content="$4"
    case "$type" in
        d) return 0 ;;
        l) return 0 ;; # tar cannot read a symlink member for --to-stdout
        0) if [ -n "$content" ]; then cat "$content"; fi ;;
        h)
            if [ -n "$linkname" ] && [ -f "$linkname" ]; then cat "$linkname"; fi
            ;;
    esac
}

if [ "$op" = "list" ] || [ "$op" = "listv" ]; then
    if [ -n "${TAR_FAIL_LIST:-}" ]; then
        echo "tar: This does not look like a tar archive" >&2
        exit 2
    fi
    # Fail only the verbose (member type) listing, so the name listing and the
    # member streaming still work.
    if [ -n "${TAR_FAIL_TYPES:-}" ] && [ "$op" = "listv" ]; then
        echo "tar: Unexpected EOF in archive" >&2
        exit 2
    fi
    if [ -n "${TAR_EMPTY_LIST:-}" ]; then
        exit 0
    fi
    while IFS='|' read -r type name linkname content; do
        [ -n "$name" ] || continue
        if [ "$op" = "list" ]; then
            printf '%s\n' "$name"
            continue
        fi
        case "$type" in
            l) printf 'lrwxrwxrwx root/root        0 2026-09-01 00:00 %s -> %s\n' "$name" "$linkname" ;;
            h) printf 'hrwxr-xr-x root/root        0 2026-09-01 00:00 %s link to %s\n' "$name" "$linkname" ;;
            d) printf 'drwxr-xr-x root/root        0 2026-09-01 00:00 %s\n' "$name" ;;
            0) printf -- '-rw-r--r-- root/root %8s 2026-09-01 00:00 %s\n' \
                   "$([ -n "$content" ] && wc -c < "$content" || echo 0)" "$name" ;;
        esac
    done < "$manifest"
    exit 0
fi

if [ "$op" = "stream" ]; then
    [ -n "$member" ] || { echo "mock tar: stream without a member" >&2; exit 2; }
    found=0
    while IFS='|' read -r type name linkname content; do
        [ -n "$name" ] || continue
        case "$name" in
            "$member"|"$member"/*) ;;
            *) continue ;;
        esac
        found=1
        member_content "$type" "$name" "$linkname" "$content"
    done < "$manifest"
    if [ "$found" -eq 0 ]; then
        echo "tar: $member: Not found in archive" >&2
        exit 2
    fi
    exit 0
fi

# Full-tree extraction: follow symlinked path components exactly like open(2).
declare -A SYMLINK=()
resolve_write_path() {
    local name="$1" prefix="" rest="$name" head acc
    while [ -n "$rest" ]; do
        head="${rest%%/*}"
        if [ "$head" = "$rest" ]; then rest=""; else rest="${rest#*/}"; fi
        acc="$prefix$head"
        if [ -n "${SYMLINK[$acc]:-}" ]; then
            if [ -n "$rest" ]; then
                printf '%s/%s\n' "${SYMLINK[$acc]}" "$rest"
            else
                printf '%s\n' "${SYMLINK[$acc]}"
            fi
            return 0
        fi
        prefix="$acc/"
    done
    printf '%s/%s\n' "$dest" "$name"
}
while IFS='|' read -r type name linkname content; do
    [ -n "$name" ] || continue
    case "$type" in
        d)
            mkdir -p "$dest/$name"
            ;;
        0)
            target=$(resolve_write_path "$name")
            mkdir -p "$(dirname "$target")"
            if [ -n "$content" ]; then cp "$content" "$target"; else : > "$target"; fi
            ;;
        l)
            SYMLINK[$name]="$linkname"
            if ! ln -s "$linkname" "$dest/$name" 2>/dev/null; then
                # Host cannot create symlinks: model the consequence (a later
                # read/copy of this path resolves to the link target).
                if [ -f "$linkname" ]; then cp "$linkname" "$dest/$name";
                else mkdir -p "$dest/$name"; fi
            fi
            ;;
        h)
            mkdir -p "$(dirname "$dest/$name")"
            if ! ln "$linkname" "$dest/$name" 2>/dev/null; then
                if [ -f "$linkname" ]; then cp "$linkname" "$dest/$name";
                elif [ -f "$dest/$linkname" ]; then cp "$dest/$linkname" "$dest/$name";
                else : > "$dest/$name"; fi
            fi
            ;;
    esac
done < "$manifest"
exit 0
MOCKTAR
chmod +x "$MOCK_TAR_BIN/tar"

# ---------------------------------------------------------------- harness -----
EXPECTED_INSTALL_DIR="$WORK_DIR/install"

# Whether this host can represent a POSIX execute bit at all.
HOST_EXEC_BIT=0
if : > "$WORK_DIR/exec-probe" && chmod +x "$WORK_DIR/exec-probe" && [ -x "$WORK_DIR/exec-probe" ]; then
    HOST_EXEC_BIT=1
fi

# Whether this host can create a POSIX symlink at all. MSYS hosts copy instead of
# linking unless MSYS=winsymlinks:nativestrict is set, so the symlink cases below
# report themselves as skipped there instead of passing for the wrong reason.
HOST_SYMLINK=0
printf 'probe\n' > "$WORK_DIR/symlink-target"
if ln -s "$WORK_DIR/symlink-target" "$WORK_DIR/symlink-probe" 2>/dev/null &&
    [ -L "$WORK_DIR/symlink-probe" ]; then
    HOST_SYMLINK=1
fi

# Run a snippet inside a fresh shell that has sourced the installer (minus its
# main call). INSTALL_DIR is redirected to the scratch directory AFTER sourcing,
# because the installer assigns it at the top of the file.
run_in_installer() {
    local snippet="$1"
    local extra_path="${2:-}"
    local harness_path="${extra_path:+$extra_path:}$MOCK_BIN:$PATH"
    # Hard guard: the run must resolve curl (and, when a hostile fixture is
    # exercised, tar) to the offline stand-ins. Without this a path-building
    # mistake would silently let a real network call or a real extraction
    # happen, and the test would pass for the wrong reason.
    local resolved_curl resolved_tar
    resolved_curl=$(PATH="$harness_path" command -v curl)
    if [ "$resolved_curl" != "$MOCK_BIN/curl" ]; then
        fail "the harness resolved curl to '$resolved_curl' instead of the mock"
    fi
    if [ -n "$extra_path" ]; then
        resolved_tar=$(PATH="$harness_path" command -v tar)
        if [ "$resolved_tar" != "$MOCK_TAR_BIN/tar" ]; then
            fail "the harness resolved tar to '$resolved_tar' instead of the mock"
        fi
    fi
    (
        export FIXTURE_DIR="$FIXTURES" PATH="$harness_path"
        export CURL_LOG FORBIDDEN_LOG TAR_LOG
        if [ -n "${TAR_MANIFEST:-}" ]; then
            export TAR_MANIFEST
        fi
        if [ -n "${TAR_FAIL_LIST:-}" ]; then
            export TAR_FAIL_LIST
        fi
        if [ -n "${TAR_EMPTY_LIST:-}" ]; then
            export TAR_EMPTY_LIST
        fi
        if [ -n "${TAR_FAIL_TYPES:-}" ]; then
            export TAR_FAIL_TYPES
        fi
        if [ -n "${CP_SWAP_TO_SYMLINK:-}" ]; then
            export CP_SWAP_TO_SYMLINK
        fi
        if [ -n "${MV_PLANT_DIR_AT:-}" ]; then
            export MV_PLANT_DIR_AT
        fi
        bash -c '
            set -o pipefail
            source <(head -n -1 "$1") || exit 1
            INSTALL_DIR="$3"
            export INSTALL_DIR
            OS="linux"
            ARCH="amd64"
            export OS ARCH
            eval "$2"
        ' bash "$INSTALLER" "$snippet" "$EXPECTED_INSTALL_DIR"
    )
}

reset_install_dir() {
    rm -rf "$EXPECTED_INSTALL_DIR"
    mkdir -p "$EXPECTED_INSTALL_DIR"
    printf 'old-binary\n' > "$EXPECTED_INSTALL_DIR/sub2api"
    rm -f "$EXPECTED_INSTALL_DIR/.sub2api.new"
}

assert_existing_binary_intact() {
    local label="$1"
    [ -f "$EXPECTED_INSTALL_DIR/sub2api" ] || fail "$label: existing executable disappeared"
    if [ "$(cat "$EXPECTED_INSTALL_DIR/sub2api")" != "old-binary" ]; then
        fail "$label: failure overwrote the existing executable"
    fi
    if [ -e "$EXPECTED_INSTALL_DIR/.sub2api.new" ]; then
        fail "$label: failure left a staged binary behind"
    fi
}

# ------------------------------------------------------------ static checks ---
if grep -q 'Wei-Shaw/sub2api' "$INSTALLER"; then
    fail "installer still references the upstream repository"
fi
grep -Fxq 'GITHUB_REPO="lwying/sub2api"' "$INSTALLER" ||
    fail "installer release source is not lwying/sub2api"

# Archive naming must keep matching the release config, which is the single
# source of truth the Go release contract also reads.
grep -Fq '{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}' "$ROOT_DIR/.goreleaser.yaml" ||
    fail "goreleaser archive name template changed; installer asset names may have drifted"
grep -Fq "name_template: 'checksums.txt'" "$ROOT_DIR/.goreleaser.yaml" ||
    fail "goreleaser checksum name changed; installer checksums asset may have drifted"

if grep -Fq 'noRollbackVersions' "$INSTALLER"; then
    fail "installer references a frontend-only message key"
fi

# The candidate list is filtered by running platform, so the entry point must
# detect the platform before listing or it would report an empty platform.
sed -n '/list-versions|versions)/,/^            ;;/p' "$INSTALLER" | grep -Fq 'detect_platform' ||
    fail "the list-versions entry point does not detect the platform"

# Reading tar's member list through a process substitution would swallow its
# exit status: a corrupt archive would then skip the member checks and be
# extracted anyway. Every tar call that gates a decision must have its status
# checked explicitly.
if grep -qE 'done < <\([[:space:]]*tar ' "$INSTALLER"; then
    fail "installer reads the tar listing through a process substitution, hiding its exit status"
fi
for required in 'if ! tar -tzf' 'if ! tar -tvzf' 'if ! tar -xOzf'; do
    grep -Fq "$required" "$INSTALLER" ||
        fail "installer does not check the exit status of: $required"
done
if grep -qE 'tar -xzf .*-C ' "$INSTALLER"; then
    fail "installer still unpacks the whole archive instead of streaming verified members"
fi

# The installer also runs on darwin (detect_platform), where mv has no -T: the
# destination-entry handling must be portable. Comments are stripped first, since
# the installer explains that choice in one.
if grep -v '^[[:space:]]*#' "$INSTALLER" | grep -qE '(^|[^-])mv[[:space:]]+-[a-zA-Z]*T'; then
    fail "installer uses mv -T, which BSD/macOS mv does not support"
fi

# The download/install step must not be able to touch services or users, and the
# only ownership change it may make is on the staged binary inside INSTALL_DIR,
# before the swap: chowning the installed path instead would abort with the new
# binary already in place, and the abort would start it while reporting the
# previous service as restored.
DOWNLOAD_FN=$(awk '/^download_and_extract\(\) \{/,/^\}/' "$INSTALLER")
[ -n "$DOWNLOAD_FN" ] || fail "could not read download_and_extract from the installer"
DOWNLOAD_CODE=$(printf '%s\n' "$DOWNLOAD_FN" | grep -v '^[[:space:]]*#')
if printf '%s' "$DOWNLOAD_CODE" | grep -Eq 'systemctl|useradd|userdel|usermod'; then
    fail "download_and_extract can mutate services or users"
fi
if printf '%s' "$DOWNLOAD_CODE" | grep -q 'chown' &&
    printf '%s' "$DOWNLOAD_CODE" | grep 'chown' | grep -qv '"$staged"'; then
    fail "download_and_extract chowns something other than the staged binary"
fi

# ------------------------------------------------------- version validation ---
for bad in '' '1.2' '1.2.3' 'v1.2' 'v1.2.3.4' 'v1.2.3-rc.1' 'v01.2.3' 'v1.02.3' 'v1.2.03' 'vX.Y.Z'; do
    if run_in_installer "is_strict_release_tag '$bad'"; then
        fail "is_strict_release_tag accepted invalid tag '$bad'"
    fi
done
for good in v0.2.6 v1.2.3 v10.20.30; do
    run_in_installer "is_strict_release_tag '$good'" ||
        fail "is_strict_release_tag rejected valid tag '$good'"
done

[ "$(run_in_installer 'platform_archive_name v0.2.6')" = 'sub2api_0.2.6_linux_amd64.tar.gz' ] ||
    fail "linux/amd64 archive name is wrong"

# ----------------------------------------------------- installability gate ---
assert_not_installable() {
    local tag="$1"
    local want="$2"
    local out code
    if out=$(run_in_installer "release_installability '$tag'"); then
        fail "release $tag was treated as installable"
    fi
    code=$(printf '%s\n' "$out" | head -n1 | cut -d'|' -f1)
    [ "$code" = "$want" ] ||
        fail "release $tag: expected reason '$want', got '$code'"
}

assert_installable() {
    local tag="$1"
    local out
    if ! out=$(run_in_installer "release_installability '$tag'"); then
        fail "release $tag was rejected as not installable ($out)"
    fi
}

assert_installable v0.2.6
assert_not_installable v0.3.0 checksums_asset_missing
assert_not_installable v0.2.7 platform_asset_missing
assert_not_installable v0.9.9 release_not_found
assert_not_installable v0.2.0 release_not_published

# validate_version: rejects non-fork tags and non-installable releases, and
# normalizes a bare X.Y.Z input to the fork tag.
for bad_version in v1.2 v0.2.6-rc.1 v0.1.0-rc.1 v01.2.3 v0.3.0 v0.2.7; do
    if run_in_installer "validate_version $bad_version" >/dev/null 2>&1; then
        fail "validate_version accepted '$bad_version'"
    fi
done
[ "$(run_in_installer 'validate_version 0.2.6' 2>/dev/null)" = 'v0.2.6' ] ||
    fail "validate_version did not normalize 0.2.6 to v0.2.6"

# The asset gate must read the release's assets array, not any "name" in the
# payload: a release whose own title (or an uploader login) reads like the
# checksum asset publishes no manifest, so it must not be treated as installable.
mkdir -p "$ASSETS/v0.2.8" "$WORK_DIR/stage-decoy"
stage_release_tree "$WORK_DIR/stage-decoy"
tar -czf "$ASSETS/v0.2.8/sub2api_0.2.8_linux_amd64.tar.gz" -C "$WORK_DIR/stage-decoy" .
cat > "$FIXTURES/tags/v0.2.8.json" <<'JSON'
{
  "tag_name": "v0.2.8",
  "name": "checksums.txt",
  "draft": false,
  "prerelease": false,
  "assets": [
    {
      "name": "sub2api_0.2.8_linux_amd64.tar.gz",
      "browser_download_url": "https://github.com/lwying/sub2api/releases/download/v0.2.8/sub2api_0.2.8_linux_amd64.tar.gz"
    }
  ]
}
JSON
if run_in_installer 'validate_version v0.2.8' >/dev/null 2>&1; then
    fail "a release that only names itself like the checksum asset was accepted as installable"
fi

# ------------------------------------------------------- current version -----
# The reported version is interpolated into the rollback backup file name, so it
# must never be empty. `grep | head` succeeds with empty output, which is exactly
# what a binary that cannot report a version produces, so the placeholder cannot
# hang off the pipeline's exit status. pipefail is turned off for this call because
# the installer itself does not enable it: with pipefail the pipeline would fail
# and mask the very bug this checks.
reset_install_dir
CURRENT=$(run_in_installer 'set +o pipefail; get_current_version' 2>/dev/null)
[ "$CURRENT" = "unknown" ] ||
    fail "get_current_version printed '$CURRENT' for a binary that cannot report a version"

# -------------------------------------------------------- latest / listing ---
# latest.json currently points at the image-only v0.3.0.
: > "$CURL_LOG"
if run_in_installer 'get_latest_version' >/dev/null 2>&1; then
    fail "get_latest_version accepted an image-only latest release"
fi
if grep -q 'releases/download' "$CURL_LOG"; then
    fail "an image-only latest release triggered a download"
fi

write_release_json v0.2.6 "$FIXTURES/latest.json" \
    "$LINUX_ARCHIVE" checksums.txt
[ "$(run_in_installer 'get_latest_version >/dev/null; printf "%s\n" "$LATEST_VERSION"')" = 'v0.2.6' ] ||
    fail "get_latest_version did not select the installable latest release"

LISTING=$(run_in_installer 'list_versions' 2>&1)
printf '%s\n' "$LISTING" | grep -qE '^  v0\.2\.6$' ||
    fail "list_versions did not list the installable release v0.2.6; output: $LISTING"
[ "$(printf '%s\n' "$LISTING" | grep -cE '^  v')" -eq 1 ] ||
    fail "list_versions listed more than the one installable candidate"
if printf '%s\n' "$LISTING" | grep -qE '^  v0\.3\.0$'; then
    fail "list_versions offered the image-only release as a rollback candidate"
fi
if printf '%s\n' "$LISTING" | grep -qE '^  v0\.2\.7$'; then
    fail "list_versions offered a release without this platform's archive"
fi
if printf '%s\n' "$LISTING" | grep -qE '^  v0\.1\.0-rc\.1$'; then
    fail "list_versions offered a non-fork version tag"
fi
if printf '%s\n' "$LISTING" | grep -qE '^  v0\.2\.0$'; then
    fail "list_versions offered a prerelease as a rollback candidate"
fi

# The candidate cap applies to what is listed, not to the feed: image-only builds
# share the feed with installable releases, and truncating it first would hide an
# older release that is still a valid rollback target behind newer ones that are
# not (the fork publishes image-only releases for most tags).
cp "$FIXTURES/releases.json" "$WORK_DIR/releases.json.saved"
{
    printf '[\n'
    for i in $(seq 1 25); do
        printf '  { "tag_name": "v9.%s.0", "draft": false, "prerelease": false },\n' "$i"
    done
    printf '  { "tag_name": "v0.2.6", "draft": false, "prerelease": false }\n'
    printf ']\n'
} > "$FIXTURES/releases.json"
LISTING=$(run_in_installer 'list_versions' 2>&1)
cp "$WORK_DIR/releases.json.saved" "$FIXTURES/releases.json"
printf '%s\n' "$LISTING" | grep -qE '^  v0\.2\.6$' ||
    fail "list_versions hid an installable release behind newer non-installable ones; output: $LISTING"
[ "$(printf '%s\n' "$LISTING" | grep -cE '^  v')" -eq 1 ] ||
    fail "the long feed listing did not offer exactly the one installable candidate"

# ------------------------------------------------------- download: green -----
: > "$CURL_LOG"
if ! GREEN_OUT=$(run_in_installer "LATEST_VERSION='v0.2.6'; download_and_extract" 2>&1); then
    fail "download_and_extract failed for a complete, verified release: $GREEN_OUT"
fi
[ -f "$EXPECTED_INSTALL_DIR/sub2api" ] || fail "installed binary is missing"
grep -q 'sub2api-test-binary' "$EXPECTED_INSTALL_DIR/sub2api" ||
    fail "installed binary does not match the verified archive"
[ "$(wc -c < "$EXPECTED_INSTALL_DIR/sub2api")" -eq "$(wc -c < "$WORK_DIR/stage-ok/sub2api")" ] ||
    fail "installed binary has the wrong size"
# This host may be unable to express a POSIX execute bit (MSYS derives it from
# file contents, so an ELF fixture reports non-executable even after chmod), so
# the execute bit is only asserted where the host can represent it.
if [ "$HOST_EXEC_BIT" -eq 1 ] && [ ! -x "$EXPECTED_INSTALL_DIR/sub2api" ]; then
    fail "installed binary is not executable"
fi
# The archive's deploy/ contents are flattened into INSTALL_DIR, matching the
# existing install layout (/opt/sub2api/install.sh, ...).
[ -f "$EXPECTED_INSTALL_DIR/archive-deploy-marker.txt" ] ||
    fail "archive deploy/ files were not installed"
# A directory member is not installed: the tests/ tree the archive carries is not
# a runtime asset, and copying it recursively is what a `cp -r` of the entry did.
if [ -e "$EXPECTED_INSTALL_DIR/tests" ]; then
    fail "the archive's nested deploy/tests tree was copied into the install directory"
fi
# Nothing may be staged inside INSTALL_DIR, and the staging directory itself must
# be removed again.
for leftover in "$EXPECTED_INSTALL_DIR"/.sub2api.new* \
    "$(dirname "$EXPECTED_INSTALL_DIR")"/.sub2api.stage.*; do
    [ -e "$leftover" ] || continue
    fail "staged binary was left behind after a successful install: $leftover"
done
grep -q 'lwying/sub2api/releases/download/v0.2.6/checksums.txt' "$CURL_LOG" ||
    fail "installer did not fetch checksums.txt"

# A directory planted at the destination must fail the swap instead of receiving
# the binary: the rename treats that path as the file itself, so an install can
# never report success while the executable is a directory the service account
# created.
reset_install_dir
rm -f "$EXPECTED_INSTALL_DIR/sub2api"
mkdir "$EXPECTED_INSTALL_DIR/sub2api"
if DIR_OUT=$(run_in_installer "LATEST_VERSION='v0.2.6'; download_and_extract" 2>&1); then
    fail "download_and_extract reported success although the destination is a directory"
fi
[ -d "$EXPECTED_INSTALL_DIR/sub2api" ] ||
    fail "the planted destination directory disappeared"
if [ -n "$(ls -A "$EXPECTED_INSTALL_DIR/sub2api")" ]; then
    fail "the staged binary was moved into a planted directory"
fi

# The same thing, but planted between the installer's check and its rename - the
# race the pre-check cannot close. The install must still fail closed instead of
# reporting a completed swap into that directory.
reset_install_dir
if ! RACE_OUT=$(MV_PLANT_DIR_AT="$EXPECTED_INSTALL_DIR/sub2api" run_in_installer "LATEST_VERSION='v0.2.6'; download_and_extract" 2>&1); then
    : # expected: the swap is reported as failed
else
    fail "download_and_extract reported success although the destination became a directory mid-swap"
fi
[ -d "$EXPECTED_INSTALL_DIR/sub2api" ] ||
    fail "the race-injected destination directory disappeared"

# INSTALL_DIR belongs to the service account, so a fixed staging path there can be
# pre-created as a symlink to any file on the host: a root copy onto that path
# would rewrite the link target, and renaming the link into place would install the
# link itself as the executable. The installer must stage outside INSTALL_DIR, in
# a directory that account cannot write, and must not follow a planted link.
STAGING_SENTINEL="$WORK_DIR/staging-sentinel.txt"
SWAP_SENTINEL="$WORK_DIR/staging-swap-sentinel.txt"
if [ "$HOST_SYMLINK" -eq 1 ]; then
    # Case 1: a staging path inside INSTALL_DIR is pre-created as a symlink.
    printf 'SENTINEL-OUTSIDE-CONTENT\n' > "$STAGING_SENTINEL"
    reset_install_dir
    ln -s "$STAGING_SENTINEL" "$EXPECTED_INSTALL_DIR/.sub2api.new"
    if ! SYMLINK_OUT=$(run_in_installer "LATEST_VERSION='v0.2.6'; download_and_extract" 2>&1); then
        fail "download_and_extract failed with a pre-created staging symlink present: $SYMLINK_OUT"
    fi
    if [ "$(cat "$STAGING_SENTINEL")" != "SENTINEL-OUTSIDE-CONTENT" ]; then
        fail "the installer followed a pre-created staging symlink and rewrote the file it points at"
    fi
    if [ -L "$EXPECTED_INSTALL_DIR/sub2api" ]; then
        fail "the installed executable is a symlink; the planted link was renamed into place"
    fi
    grep -q 'sub2api-test-binary' "$EXPECTED_INSTALL_DIR/sub2api" ||
        fail "the verified binary was not installed while a staging symlink was present"

    # Case 2: the rollback backup is written into the same directory, so it must
    # not be redirected through a planted link either.
    printf 'BACKUP-SENTINEL-OUTSIDE-CONTENT\n' > "$STAGING_SENTINEL"
    reset_install_dir
    ln -s "$STAGING_SENTINEL" "$EXPECTED_INSTALL_DIR/sub2api.backup"
    if ! SYMLINK_OUT=$(run_in_installer "LATEST_VERSION='v0.2.6'; BACKUP_PATH='$EXPECTED_INSTALL_DIR/sub2api.backup'; download_and_extract" 2>&1); then
        fail "download_and_extract failed with a pre-created backup symlink present: $SYMLINK_OUT"
    fi
    if [ "$(cat "$STAGING_SENTINEL")" != "BACKUP-SENTINEL-OUTSIDE-CONTENT" ]; then
        fail "the installer followed a pre-created backup symlink and rewrote the file it points at"
    fi
    if [ -L "$EXPECTED_INSTALL_DIR/sub2api.backup" ]; then
        fail "the rollback backup path is still a symlink; the planted link was not replaced"
    fi
    grep -q 'old-binary' "$EXPECTED_INSTALL_DIR/sub2api.backup" ||
        fail "the replaced binary was not backed up while a backup symlink was present"

    # Case 2c: the archive's deploy/ files are flattened into INSTALL_DIR as well,
    # so a pre-created symlink at one of their names is the same attack on that
    # copy: `cp` onto the name writes through the link instead of replacing it, and
    # the archive's auxiliary file lands in a file the service account could not
    # write itself. These files are not pointless - `install-datamanagementd.sh`
    # resolves the unit next to the binary and `apple-container.sh` reads the
    # adjacent .env.example - so they must still be installed for an honest
    # directory, only never through a planted link or into a planted directory.
    printf 'DEPLOY-SENTINEL-OUTSIDE-CONTENT\n' > "$WORK_DIR/deploy-sentinel.txt"
    reset_install_dir
    ln -s "$WORK_DIR/deploy-sentinel.txt" "$EXPECTED_INSTALL_DIR/archive-deploy-marker.txt"
    if ! DEPLOY_OUT=$(run_in_installer "LATEST_VERSION='v0.2.6'; download_and_extract" 2>&1); then
        fail "download_and_extract failed with a pre-created deploy symlink present: $DEPLOY_OUT"
    fi
    if [ "$(cat "$WORK_DIR/deploy-sentinel.txt")" != "DEPLOY-SENTINEL-OUTSIDE-CONTENT" ]; then
        fail "the installer followed a pre-created deploy symlink and rewrote the file it points at"
    fi
    if [ -L "$EXPECTED_INSTALL_DIR/archive-deploy-marker.txt" ]; then
        fail "the installed deploy file is still a symlink; the planted link was not replaced"
    fi
    grep -q 'file: README.md' "$EXPECTED_INSTALL_DIR/archive-deploy-marker.txt" ||
        fail "the archive's deploy file was not installed while a deploy symlink was present"

    # Case 2d: the same name, but a link to a *directory*. `mv` follows such a link
    # and moves the operand *inside* the directory it points at, reporting success
    # while the entry at the name stays a link (measured: mv exits 0, the target
    # directory gains the file, the link stays in place). That name therefore has
    # to be refused before the rename instead of handed to it, or the archive's
    # file would land inside a directory the service account chose.
    mkdir -p "$WORK_DIR/deploy-link-target"
    reset_install_dir
    ln -s "$WORK_DIR/deploy-link-target" "$EXPECTED_INSTALL_DIR/archive-deploy-marker.txt"
    if ! DEPLOY_LINK_OUT=$(run_in_installer "LATEST_VERSION='v0.2.6'; download_and_extract" 2>&1); then
        fail "download_and_extract failed with a deploy link to a directory present: $DEPLOY_LINK_OUT"
    fi
    if [ -n "$(ls -A "$WORK_DIR/deploy-link-target")" ]; then
        fail "the deploy file was moved into the directory a planted link points at"
    fi
    grep -q 'sub2api-test-binary' "$EXPECTED_INSTALL_DIR/sub2api" ||
        fail "the verified binary was not installed while a deploy link to a directory was present"

    # Case 3b: the installed executable itself is a symlink out of INSTALL_DIR. The
    # backup copies it as root and lands in INSTALL_DIR, which the service account
    # can read, so the link target must never be copied there.
    printf 'ROOT-ONLY-CONTENT\n' > "$WORK_DIR/root-only-sentinel.txt"
    reset_install_dir
    rm -f "$EXPECTED_INSTALL_DIR/sub2api"
    ln -s "$WORK_DIR/root-only-sentinel.txt" "$EXPECTED_INSTALL_DIR/sub2api"
    if run_in_installer "LATEST_VERSION='v0.2.6'; BACKUP_PATH='$EXPECTED_INSTALL_DIR/sub2api.backup'; download_and_extract" >/dev/null 2>&1; then
        fail "download_and_extract accepted a symlinked installed executable"
    fi
    if [ -e "$EXPECTED_INSTALL_DIR/sub2api.backup" ]; then
        fail "the link target was copied into a backup the service account can read"
    fi

    # Case 3: the active race. An attacker who owns INSTALL_DIR can watch for the
    # staging file and swap it for a symlink just before the root copy opens it,
    # which no unpredictable name prevents. The cp stand-in performs exactly that
    # swap for any staging path inside INSTALL_DIR, so this case fails unless the
    # installer stages in a directory that account cannot write.
    printf 'SWAP-SENTINEL-OUTSIDE-CONTENT\n' > "$SWAP_SENTINEL"
    reset_install_dir
    if ! SWAP_OUT=$(CP_SWAP_TO_SYMLINK=true run_in_installer "LATEST_VERSION='v0.2.6'; download_and_extract" 2>&1); then
        fail "download_and_extract failed with an injected staging swap: $SWAP_OUT"
    fi
    if [ "$(cat "$SWAP_SENTINEL")" != "SWAP-SENTINEL-OUTSIDE-CONTENT" ]; then
        fail "the root copy wrote through a staging path inside the service-writable INSTALL_DIR"
    fi
    if [ -L "$EXPECTED_INSTALL_DIR/sub2api" ]; then
        fail "the installed executable is a symlink after an injected staging swap"
    fi
    grep -q 'sub2api-test-binary' "$EXPECTED_INSTALL_DIR/sub2api" ||
        fail "the verified binary was not installed after an injected staging swap"
else
    echo "install-fork-rollback: host cannot create symlinks; skipping the staging-symlink cases" >&2
fi

# A directory planted at a deploy member's name must not receive the file either:
# `mv` moves an operand *into* an existing directory instead of replacing it, so
# the name would silently get a nested copy. The install has already replaced the
# executable by then and these files are auxiliary, so the member is reported as
# skipped rather than failing the install - and the planted directory is left
# alone, never written into. This case needs no symlinks, so it runs everywhere.
reset_install_dir
rm -f "$EXPECTED_INSTALL_DIR/archive-deploy-marker.txt"
mkdir "$EXPECTED_INSTALL_DIR/archive-deploy-marker.txt"
if ! DEPLOY_DIR_OUT=$(run_in_installer "LATEST_VERSION='v0.2.6'; download_and_extract" 2>&1); then
    fail "download_and_extract failed because a deploy destination is a directory: $DEPLOY_DIR_OUT"
fi
[ -d "$EXPECTED_INSTALL_DIR/archive-deploy-marker.txt" ] ||
    fail "the planted deploy destination directory disappeared"
if [ -n "$(ls -A "$EXPECTED_INSTALL_DIR/archive-deploy-marker.txt")" ]; then
    fail "the deploy file was moved into a planted directory"
fi
grep -q 'sub2api-test-binary' "$EXPECTED_INSTALL_DIR/sub2api" ||
    fail "the verified binary was not installed while a deploy destination was a directory"
# Whatever staging the deploy files go through must be removed again as well.
for leftover in "$(dirname "$EXPECTED_INSTALL_DIR")"/.sub2api.stage.*; do
    [ -e "$leftover" ] || continue
    fail "a staging directory was left behind by the deploy copy: $leftover"
done

# ------------------------------------------------------- download: red -------
expect_download_failure() {
    local tag="$1"
    local label="$2"
    local expect_text="$3"
    local out
    reset_install_dir
    if out=$(run_in_installer "LATEST_VERSION='$tag'; download_and_extract" 2>&1); then
        fail "download_and_extract installed $tag but should have failed ($label)"
    fi
    assert_existing_binary_intact "$label"
    if [ -n "$expect_text" ]; then
        printf '%s' "$out" | grep -Fq "$expect_text" ||
            fail "$label: expected '$expect_text' in the failure output, got: $out"
    fi
}

# checksums.txt listed by the API but not actually served (HTTP 404)
expect_download_failure v0.2.5 checksum-file-missing 'checksums.txt'
# published checksum does not match the downloaded archive
expect_download_failure v0.2.4 checksum-mismatch 'abab'
# archive member escapes the extraction directory
expect_download_failure v0.2.3 unsafe-relative-path 'escape.txt'
# archive member uses an absolute path
expect_download_failure v0.2.2 unsafe-absolute-path 'sub2api-abs-escape.txt'
# archive is verified but holds no binary
expect_download_failure v0.2.1 binary-missing 'sub2api'

for escaped in "$WORK_DIR/escape.txt" /tmp/sub2api-abs-escape.txt; do
    if [ -e "$escaped" ]; then
        fail "an unsafe archive path escaped extraction: $escaped"
    fi
done

# v0.1.6 - checksums.txt is published but carries no line for this archive, so
# the archive cannot be tied to a published checksum.
mkdir -p "$ASSETS/v0.1.6" "$WORK_DIR/stage-no-entry"
stage_release_tree "$WORK_DIR/stage-no-entry"
tar -czf "$ASSETS/v0.1.6/sub2api_0.1.6_linux_amd64.tar.gz" -C "$WORK_DIR/stage-no-entry" .
printf '%s  %s\n' "abababababababababababababababababababababababababababababababab" \
    "some_other_asset.tar.gz" > "$ASSETS/v0.1.6/checksums.txt"
make_tag_fixture v0.1.6 sub2api_0.1.6_linux_amd64.tar.gz checksums.txt
expect_download_failure v0.1.6 checksum-entry-missing 'sub2api_0.1.6_linux_amd64.tar.gz'

# v0.1.7 - checksums.txt carries the correct line but is far larger than a checksum
# file can plausibly be (a handful of lines, one per released asset). The download
# must be bounded instead of streaming the whole body into the temp directory.
mkdir -p "$ASSETS/v0.1.7" "$WORK_DIR/stage-oversized"
stage_release_tree "$WORK_DIR/stage-oversized"
tar -czf "$ASSETS/v0.1.7/sub2api_0.1.7_linux_amd64.tar.gz" -C "$WORK_DIR/stage-oversized" .
{
    checksums_line "$ASSETS/v0.1.7/sub2api_0.1.7_linux_amd64.tar.gz"
    head -c 1100000 /dev/zero | tr '\0' '#'
    printf '\n'
} > "$ASSETS/v0.1.7/checksums.txt"
make_tag_fixture v0.1.7 sub2api_0.1.7_linux_amd64.tar.gz checksums.txt
reset_install_dir
if OVERSIZED_OUT=$(run_in_installer "LANG_CHOICE=en; LATEST_VERSION='v0.1.7'; download_and_extract" 2>&1); then
    fail "download_and_extract installed a release whose checksum file is oversized"
fi
assert_existing_binary_intact "checksums-too-large"
printf '%s' "$OVERSIZED_OUT" | grep -Fq 'implausibly large' ||
    fail "checksums-too-large: the oversized checksum file was not refused: $OVERSIZED_OUT"

# image-only release: refused by the gate, and nothing is downloaded
: > "$CURL_LOG"
reset_install_dir
if run_in_installer "LATEST_VERSION='v0.3.0'; download_and_extract" >/dev/null 2>&1; then
    fail "an image-only release was installed"
fi
assert_existing_binary_intact "image-only-release"
if grep -q 'releases/download' "$CURL_LOG"; then
    fail "an image-only release triggered a download"
fi

# release without this platform's archive: refused the same way
reset_install_dir
if run_in_installer "LATEST_VERSION='v0.2.7'; download_and_extract" >/dev/null 2>&1; then
    fail "a release without this platform's archive was installed"
fi
assert_existing_binary_intact "missing-platform-asset"

# ------------------------------------------------- link members: rejected ----
# A release archive is only ever expected to hold regular files and directories.
# Any symlink or hard link member is refused before extraction, because a symlink
# member can redirect a later member's write outside the extraction root and a
# symlinked sub2api would make the install copy read an unrelated file.
ESCAPE_DIR="$WORK_DIR/outside"
OUTSIDE_SECRET="$WORK_DIR/outside-secret.txt"
PAYLOAD="$WORK_DIR/hostile-payload.txt"
HOSTILE_BIN="$WORK_DIR/hostile-bin.txt"
mkdir -p "$ESCAPE_DIR"
printf 'SECRET-OUTSIDE-CONTENT\n' > "$OUTSIDE_SECRET"
printf 'pwned-via-symlink\n' > "$PAYLOAD"
make_fake_binary "$HOSTILE_BIN"

# Fake platform images: the installer must accept only the image format of the
# platform it is installing on, so a Mach-O must not be installable on Linux.
make_fake_binary "$WORK_DIR/fake-elf"
printf '\317\372\355\376' > "$WORK_DIR/fake-macho"
printf 'macho-body\n' >> "$WORK_DIR/fake-macho"
chmod +x "$WORK_DIR/fake-macho"

run_in_installer "OS=linux; verify_binary_image '$WORK_DIR/fake-elf'" ||
    fail "an ELF image was not accepted on linux"
if run_in_installer "OS=darwin; verify_binary_image '$WORK_DIR/fake-elf'"; then
    fail "an ELF image was accepted on darwin"
fi
run_in_installer "OS=darwin; verify_binary_image '$WORK_DIR/fake-macho'" ||
    fail "a Mach-O image was not accepted on darwin"
if run_in_installer "OS=linux; verify_binary_image '$WORK_DIR/fake-macho'"; then
    fail "a Mach-O image was accepted on linux, so a non-installable asset would be installed"
fi
if run_in_installer "OS=linux; verify_binary_image '$PAYLOAD'"; then
    fail "a text file was accepted as an executable image"
fi
# With no od on PATH the image cannot be inspected, which must fail closed.
if run_in_installer "PATH=/nonexistent; OS=linux; verify_binary_image '$WORK_DIR/fake-elf'" >/dev/null 2>&1; then
    fail "image verification passed without od, so it is not fail closed"
fi

# v0.1.1 - a directory member that is a symlink, plus a member written through it.
make_hostile_archive v0.1.1 \
    "l|deploy|$ESCAPE_DIR|" \
    "0|deploy/pwned.txt||$PAYLOAD" \
    "0|sub2api||$HOSTILE_BIN"
SYMLINK_FIXTURE="$ASSETS/v0.1.1/sub2api_0.1.1_linux_amd64.tar.gz"

# v0.1.2 - the sub2api member itself is a symlink to a file outside the archive.
make_hostile_archive v0.1.2 \
    "0|deploy/marker.txt||$PAYLOAD" \
    "l|sub2api|$OUTSIDE_SECRET|"

# v0.1.3 - hardlink members, one of them naming a path outside the archive.
make_hostile_archive v0.1.3 \
    "0|sub2api||$HOSTILE_BIN" \
    "h|deploy/inner-link.txt|sub2api|" \
    "h|deploy/outside-link.txt|$OUTSIDE_SECRET|"
HARDLINK_FIXTURE="$ASSETS/v0.1.3/sub2api_0.1.3_linux_amd64.tar.gz"

# The fixtures must really contain link members, as reported by this host's own
# tar rather than by the mock, and the symlinked-directory case must really carry
# the follow-up member.
REAL_SYMLINK_TYPES=$("$REAL_TAR" -tvzf "$SYMLINK_FIXTURE" | awk '{ print substr($1, 1, 1) }')
if ! printf '%s\n' "$REAL_SYMLINK_TYPES" | grep -qx 'l'; then
    fail "hostile fixture carries no symlink member according to real tar: $REAL_SYMLINK_TYPES"
fi
if ! printf '%s\n' "$("$REAL_TAR" -tzf "$SYMLINK_FIXTURE")" | grep -qx 'deploy/pwned.txt'; then
    fail "hostile fixture is missing the follow-up member written through the symlink"
fi
REAL_HARDLINK_TYPES=$("$REAL_TAR" -tvzf "$HARDLINK_FIXTURE" | awk '{ print substr($1, 1, 1) }')
if ! printf '%s\n' "$REAL_HARDLINK_TYPES" | grep -qx 'h'; then
    fail "hostile fixture carries no hardlink member according to real tar: $REAL_HARDLINK_TYPES"
fi

# Case 1: symlinked directory plus a member written through it.
TAR_MANIFEST="$ASSETS/v0.1.1/manifest"
reset_install_dir
: > "$TAR_LOG"
HOSTILE_RC=0
HOSTILE_OUT=$(run_in_installer "LATEST_VERSION='v0.1.1'; download_and_extract" "$MOCK_TAR_BIN" 2>&1) || HOSTILE_RC=$?
if [ -e "$ESCAPE_DIR/pwned.txt" ]; then
    fail "a member was written outside the extraction root through a symlink"
fi
if [ "$HOSTILE_RC" -eq 0 ]; then
    fail "installer accepted an archive whose directory member is a symlink: $HOSTILE_OUT"
fi
assert_existing_binary_intact "symlinked-directory-member"
if grep -q '^stream\|^extract' "$TAR_LOG"; then
    fail "the installer opened an archive containing a symlink member"
fi
if ! grep -q 'releases/tags/v0.1.1' "$CURL_LOG"; then
    fail "the hostile symlink case never reached the mocked release lookup"
fi

# Case 2: the sub2api member itself is a symlink to a file outside the archive.
TAR_MANIFEST="$ASSETS/v0.1.2/manifest"
reset_install_dir
: > "$TAR_LOG"
HOSTILE_RC=0
HOSTILE_OUT=$(run_in_installer "LATEST_VERSION='v0.1.2'; download_and_extract" "$MOCK_TAR_BIN" 2>&1) || HOSTILE_RC=$?
if grep -rq 'SECRET-OUTSIDE-CONTENT' "$EXPECTED_INSTALL_DIR" 2>/dev/null; then
    fail "the install copy followed a symlink and read a file outside the archive"
fi
if [ "$HOSTILE_RC" -eq 0 ]; then
    fail "installer accepted an archive whose sub2api member is a symlink: $HOSTILE_OUT"
fi
assert_existing_binary_intact "symlinked-binary-member"
if grep -q '^stream\|^extract' "$TAR_LOG"; then
    fail "the installer opened an archive containing a symlinked sub2api"
fi

# Case 3: hardlink members, including one naming a path outside the archive.
TAR_MANIFEST="$ASSETS/v0.1.3/manifest"
reset_install_dir
: > "$TAR_LOG"
HOSTILE_RC=0
HOSTILE_OUT=$(run_in_installer "LATEST_VERSION='v0.1.3'; download_and_extract" "$MOCK_TAR_BIN" 2>&1) || HOSTILE_RC=$?
if [ "$(cat "$OUTSIDE_SECRET")" != "SECRET-OUTSIDE-CONTENT" ]; then
    fail "a hardlink member modified a file outside the archive"
fi
if [ "$HOSTILE_RC" -eq 0 ]; then
    fail "installer accepted an archive containing hardlink members: $HOSTILE_OUT"
fi
assert_existing_binary_intact "hardlink-member"
if grep -q '^stream\|^extract' "$TAR_LOG"; then
    fail "the installer opened an archive containing hardlink members"
fi

# Case 3b: a member whose path runs through another member that is a file
# ("deploy/conflict" and "deploy/conflict/child.txt"). Building the tree would
# have to turn a file into a directory; the member operand would also pull in
# more than one member, so the archive is refused before anything is written.
make_hostile_archive v0.1.5 \
    "0|sub2api||$HOSTILE_BIN" \
    "0|deploy/conflict||$PAYLOAD" \
    "0|deploy/conflict/child.txt||$PAYLOAD"
TAR_MANIFEST="$ASSETS/v0.1.5/manifest"
reset_install_dir
: > "$TAR_LOG"
HOSTILE_RC=0
HOSTILE_OUT=$(run_in_installer "LATEST_VERSION='v0.1.5'; download_and_extract" "$MOCK_TAR_BIN" 2>&1) || HOSTILE_RC=$?
if [ "$HOSTILE_RC" -eq 0 ]; then
    fail "installer accepted an archive whose member paths conflict: $HOSTILE_OUT"
fi
assert_existing_binary_intact "conflicting-member-paths"
# Members are streamed one at a time into the temp directory, so a harmless
# member may be read before a later member is rejected; the conflicting members
# themselves must never be streamed, and nothing may reach the install dir.
if awk -F'\t' '$1 == "stream" && ($4 == "deploy/conflict" || $4 == "deploy/conflict/child.txt") { found = 1 } END { exit !found }' "$TAR_LOG"; then
    fail "the installer streamed a conflicting member"
fi
if [ -e "$EXPECTED_INSTALL_DIR/conflict" ] || [ -e "$EXPECTED_INSTALL_DIR/conflict/child.txt" ]; then
    fail "a conflicting member reached the install directory"
fi

# ------------------------------------------ archive listing must be checked ---
# A failing or empty member listing must abort the install instead of skipping
# the checks above and extracting the archive. The manifest describes the
# ordinary v0.2.6 archive so the failure can only come from the injected
# listing behaviour, not from a missing mock fixture.
LEGIT_MANIFEST="$WORK_DIR/legit.manifest"
{
    printf 'd|deploy||\n'
    printf '0|deploy/archive-deploy-marker.txt||%s\n' "$WORK_DIR/stage-ok/deploy/archive-deploy-marker.txt"
    printf '0|sub2api||%s\n' "$WORK_DIR/stage-ok/sub2api"
} > "$LEGIT_MANIFEST"

# The failure injected below is limited to the listing: the same member can
# still be streamed out of the archive, which is what makes this case meaningful.
# If the installer ignored the listing's exit status it would go on to stream the
# member and install it (the failure mode under test) rather than abort.
if [ -z "$(TAR_LOG=/dev/null TAR_MANIFEST="$LEGIT_MANIFEST" TAR_FAIL_LIST=1 \
    "$MOCK_TAR_BIN/tar" -xOzf "$ASSETS/v0.2.6/sub2api_0.2.6_linux_amd64.tar.gz" -- sub2api 2>/dev/null)" ]; then
    fail "the mock tar cannot stream while the listing failure is injected, so these cases prove nothing"
fi

# Case 4: `tar -t` exits non-zero (corrupt or tampered archive).
reset_install_dir
: > "$TAR_LOG"
HOSTILE_RC=0
HOSTILE_OUT=$(TAR_FAIL_LIST=1 TAR_MANIFEST="$LEGIT_MANIFEST" \
    run_in_installer "LANG_CHOICE=en; LATEST_VERSION='v0.2.6'; download_and_extract" "$MOCK_TAR_BIN" 2>&1) || HOSTILE_RC=$?
if [ "$HOSTILE_RC" -eq 0 ]; then
    fail "installer succeeded although the archive listing failed: $HOSTILE_OUT"
fi
printf '%s' "$HOSTILE_OUT" | grep -Fq 'Could not read the archive member list' ||
    fail "the failed listing was not the reported cause: $HOSTILE_OUT"
assert_existing_binary_intact "archive-list-failed"
if grep -q '^stream\|^extract' "$TAR_LOG"; then
    fail "the installer opened an archive whose listing failed"
fi
if [ -e "$ESCAPE_DIR/pwned.txt" ]; then
    fail "an archive whose listing failed wrote outside the extraction root"
fi

# Case 4b: only the member type listing (`tar -tv`) fails, while both the name
# listing and member streaming would succeed.
reset_install_dir
: > "$TAR_LOG"
HOSTILE_RC=0
HOSTILE_OUT=$(TAR_FAIL_TYPES=1 TAR_MANIFEST="$LEGIT_MANIFEST" \
    run_in_installer "LANG_CHOICE=en; LATEST_VERSION='v0.2.6'; download_and_extract" "$MOCK_TAR_BIN" 2>&1) || HOSTILE_RC=$?
if [ "$HOSTILE_RC" -eq 0 ]; then
    fail "installer succeeded although the member type listing failed: $HOSTILE_OUT"
fi
printf '%s' "$HOSTILE_OUT" | grep -Fq 'Could not read the archive member list' ||
    fail "the failed member type listing was not the reported cause: $HOSTILE_OUT"
assert_existing_binary_intact "archive-type-list-failed"
if grep -q '^stream\|^extract' "$TAR_LOG"; then
    fail "the installer opened an archive whose member type listing failed"
fi

# Case 5: `tar -t` succeeds but prints nothing.
reset_install_dir
: > "$TAR_LOG"
HOSTILE_RC=0
HOSTILE_OUT=$(TAR_EMPTY_LIST=1 TAR_MANIFEST="$LEGIT_MANIFEST" \
    run_in_installer "LANG_CHOICE=en; LATEST_VERSION='v0.2.6'; download_and_extract" "$MOCK_TAR_BIN" 2>&1) || HOSTILE_RC=$?
if [ "$HOSTILE_RC" -eq 0 ]; then
    fail "installer succeeded although the archive listing was empty: $HOSTILE_OUT"
fi
printf '%s' "$HOSTILE_OUT" | grep -Fq 'Could not read the archive member list' ||
    fail "the empty listing was not the reported cause: $HOSTILE_OUT"
assert_existing_binary_intact "archive-list-empty"
if grep -q '^stream\|^extract' "$TAR_LOG"; then
    fail "the installer opened an archive whose listing was empty"
fi

# Case 6: a file that is not a tar archive at all, listed by the real tar.
mkdir -p "$ASSETS/v0.1.4"
printf 'this is not a tar archive\n' > "$ASSETS/v0.1.4/sub2api_0.1.4_linux_amd64.tar.gz"
checksums_line "$ASSETS/v0.1.4/sub2api_0.1.4_linux_amd64.tar.gz" > "$ASSETS/v0.1.4/checksums.txt"
write_release_json v0.1.4 "$FIXTURES/tags/v0.1.4.json" \
    sub2api_0.1.4_linux_amd64.tar.gz checksums.txt
reset_install_dir
HOSTILE_RC=0
HOSTILE_OUT=$(run_in_installer "LATEST_VERSION='v0.1.4'; download_and_extract" 2>&1) || HOSTILE_RC=$?
if [ "$HOSTILE_RC" -eq 0 ]; then
    fail "installer succeeded on a non-archive download: $HOSTILE_OUT"
fi
assert_existing_binary_intact "corrupt-archive"
if [ -e "$ESCAPE_DIR/pwned.txt" ]; then
    fail "a non-archive download wrote outside the extraction root"
fi

# ------------------------------------------- real tar + real Go binary --------
# End-to-end compatibility check against the host's own tar, with the exact
# artifact kind the release pipeline publishes: a Go binary for this platform.
# This proves the `-xOzf -- <member>` operand works with the real GNU/BSD tar
# (not only with the mock) and that the image check accepts a genuine artifact.
if command -v go >/dev/null 2>&1; then
    mkdir -p "$WORK_DIR/gobuild"
    printf 'module installertest\n\ngo 1.21\n' > "$WORK_DIR/gobuild/go.mod"
    printf 'package main\n\nfunc main() {}\n' > "$WORK_DIR/gobuild/main.go"
    if (cd "$WORK_DIR/gobuild" && GOCACHE="$WORK_DIR/gocache" GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
            go build -o "$WORK_DIR/gobuild/sub2api" .) 2>/dev/null; then
        mkdir -p "$WORK_DIR/stage-real/deploy" "$ASSETS/v0.0.9"
        cp "$WORK_DIR/gobuild/sub2api" "$WORK_DIR/stage-real/sub2api"
        printf 'file: README.md\n' > "$WORK_DIR/stage-real/deploy/archive-deploy-marker.txt"
        tar -czf "$ASSETS/v0.0.9/sub2api_0.0.9_linux_amd64.tar.gz" -C "$WORK_DIR/stage-real" .
        checksums_line "$ASSETS/v0.0.9/sub2api_0.0.9_linux_amd64.tar.gz" > "$ASSETS/v0.0.9/checksums.txt"
        make_tag_fixture v0.0.9 sub2api_0.0.9_linux_amd64.tar.gz checksums.txt
        reset_install_dir
        if ! REAL_OUT=$(run_in_installer "LATEST_VERSION='v0.0.9'; download_and_extract" 2>&1); then
            fail "a release carrying a genuine Go binary was not installed: $REAL_OUT"
        fi
        if [ "$(wc -c < "$WORK_DIR/gobuild/sub2api")" -ne "$(wc -c < "$EXPECTED_INSTALL_DIR/sub2api")" ]; then
            fail "the installed binary is not the verified Go binary"
        fi
        if [ "$(od -An -v -tx1 -N4 "$EXPECTED_INSTALL_DIR/sub2api" | tr -d ' \n')" != "7f454c46" ]; then
            fail "the installed Go binary lost its ELF image magic"
        fi
        if [ -e "$EXPECTED_INSTALL_DIR/../.sub2api.new" ]; then
            fail "the staged binary was left behind"
        fi
    else
        echo "install-fork-rollback: could not build the Go fixture; skipping one case" >&2
    fi
else
    echo "install-fork-rollback: go is not available; skipping the real-toolchain case" >&2
fi

# ----------------------------------------------------------- final guards ----
# Every request must have gone to the fork, and the guard stubs must be unused.
if grep -q 'Wei-Shaw' "$CURL_LOG"; then
    fail "installer requested an upstream release URL"
fi
if [ -s "$FORBIDDEN_LOG" ]; then
    fail "installer touched the host during a dry run: $(cat "$FORBIDDEN_LOG")"
fi
if [ ! -s "$CURL_LOG" ]; then
    fail "the mock curl was never used; the test did not exercise the installer"
fi

echo "install fork rollback checks passed"
