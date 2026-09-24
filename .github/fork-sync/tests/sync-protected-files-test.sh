#!/bin/bash
# 行为 4：fork 自有文件（版本号、发布配置、更新器、安装器）不被上游改动自动覆盖，
# 但上游对这些文件的改动必须出现在 PR 正文里供维护者审查。
#
# 覆盖两种危险情形：
#   a. 上游改动与 fork 不冲突（会静默合并进去）-> 必须还原为 fork 的内容；
#   b. 上游改动与 fork 冲突（同一行被双方修改）-> 不得因此阻塞整个同步，按 fork 处理并报告。
# 普通文件的上游改动仍必须正常合并。

set -euo pipefail

source "$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

new_fixture_root
init_upstream
init_fork
write_gh_stub
set_ci_checks '[]'

SYNC_BRANCH="chore/upstream-sync"

FORK_INSTALLER_SHA="$(ref_blob_sha "$FORK_WORK" HEAD deploy/install.sh)"
FORK_RELEASE_WF_SHA="$(ref_blob_sha "$FORK_WORK" HEAD .github/workflows/release.yml)"
FORK_UPDATER_SHA="$(ref_blob_sha "$FORK_WORK" HEAD backend/internal/service/update_service.go)"
FORK_WIRING_SHA="$(ref_blob_sha "$FORK_WORK" HEAD backend/cmd/server/wire_gen.go)"
[[ -n "$FORK_INSTALLER_SHA" && -n "$FORK_RELEASE_WF_SHA" && -n "$FORK_UPDATER_SHA" && -n "$FORK_WIRING_SHA" ]] \
    || fail "fixture must expose the fork-owned content for comparison"

# a. 上游改版本号 + 安装器 + 发布工作流（不冲突）
upstream_commit "chore: bump upstream VERSION to 0.2.9" "backend/cmd/server/VERSION" "0.2.9\n"
upstream_commit "fix(upstream): installer tweak" "deploy/install.sh" \
    '#!/bin/sh
echo upstream-installer-v1
echo "# upstream-only tweak"
'
upstream_commit "ci(upstream): release workflow tweak" ".github/workflows/release.yml" \
    'name: Release
on:
  push:
    tags: [v*]
  workflow_dispatch:
'
# b. 上游改更新器，与 fork 的同一行不同改法 -> 真冲突，且属于受保护文件
upstream_commit "fix(upstream): updater uses upstream-v2" "backend/internal/service/update_service.go" \
    'package service

const ForkUpdater = "upstream-v2"
'
# fork 的接线文件：上游会重写它，从而删掉 fork 的 provider 注册 -> 不得自动采用
upstream_commit "refactor(upstream): regenerate wiring" "backend/cmd/server/wire_gen.go" \
    'package main

// upstream wiring (regenerated)
func upstreamWiring() {}

'
# 受保护目录：上游既改了已有文件、又往里新增了文件 -> 两者都不得进入同步分支
upstream_commit "fix(upstream): contract marker" "backend/internal/releasecontract/marker.go" \
    'package releasecontract

// upstream contract implementation (v2)
'
upstream_commit "feat(upstream): add contract file" "backend/internal/releasecontract/upstream-extra.go" \
    'package releasecontract

// upstream-only addition
'
# 普通文件的上游改动必须照常合并
upstream_commit "feat(upstream): normal file change" "app/upstream-only.txt" "upstream\n"

WORK="$(clone_fork "$FIXTURE_ROOT/work")"
REPORT_PATH="$FIXTURE_ROOT/report.md"
sync_run "$WORK"

assert_eq 0 "$SYNC_STATUS" "protected-file conflicts must not block the sync: $SYNC_ERR"
assert_eq "created" "$(json_get "$SYNC_OUT" action)" "action"
assert_eq '[".github/workflows/release.yml", "backend/cmd/server/VERSION", "backend/cmd/server/wire_gen.go", "backend/internal/releasecontract/marker.go", "backend/internal/releasecontract/upstream-extra.go", "backend/internal/service/update_service.go", "deploy/install.sh"]' \
    "$(json_get "$SYNC_OUT" protected_paths_changed_upstream)" "protected paths must be reported (sorted)"

# fork 自有文件必须保持 fork 的内容
assert_eq "0.2.6" "$(ref_first_line "$FORK_BARE" "$SYNC_BRANCH" backend/cmd/server/VERSION | tr -d '\r\n')" \
    "upstream VERSION bump must not be applied"
assert_eq "$FORK_INSTALLER_SHA" "$(ref_blob_sha "$FORK_BARE" "$SYNC_BRANCH" deploy/install.sh)" \
    "upstream installer changes must not be applied"
assert_eq "$FORK_RELEASE_WF_SHA" "$(ref_blob_sha "$FORK_BARE" "$SYNC_BRANCH" .github/workflows/release.yml)" \
    "upstream release workflow changes must not be applied"
assert_eq "$FORK_UPDATER_SHA" "$(ref_blob_sha "$FORK_BARE" "$SYNC_BRANCH" backend/internal/service/update_service.go)" \
    "conflicting upstream updater change must be resolved in favour of the fork"
# 接线文件：上游重写会删掉 fork 的 provider 注册，必须保持 fork 自己的版本
assert_eq "$FORK_WIRING_SHA" "$(ref_blob_sha "$FORK_BARE" "$SYNC_BRANCH" backend/cmd/server/wire_gen.go)" \
    "upstream wiring rewrites must not silently drop the fork's provider registrations"
# 受保护目录：目录内已有的 fork 文件必须保持 fork 内容……
assert_eq "$(ref_blob_sha "$FORK_WORK" HEAD backend/internal/releasecontract/marker.go)" \
    "$(ref_blob_sha "$FORK_BARE" "$SYNC_BRANCH" backend/internal/releasecontract/marker.go)" \
    "upstream edits inside a protected directory must not be applied"
# ……而且上游**新增**到受保护目录里的文件也不能被叠加进来（checkout 覆盖式恢复会漏掉它）
ref_has_path "$FORK_BARE" "$SYNC_BRANCH" backend/internal/releasecontract/upstream-extra.go \
    && fail "an upstream-added file inside a protected directory must not be merged in"

# 普通文件的上游改动必须被合并
assert_eq "upstream" "$(ref_first_line "$FORK_BARE" "$SYNC_BRANCH" app/upstream-only.txt | tr -d '\r\n')" \
    "non-protected upstream changes must still be synced"

REPORT="$(cat "$REPORT_PATH")"
assert_contains "$REPORT" "受保护文件" "report must have a protected-files section"
assert_contains "$REPORT" "backend/cmd/server/VERSION" "report must list the protected version file"
assert_contains "$REPORT" "0.2.9" "report must show the upstream VERSION value for review"
assert_contains "$REPORT" "0.2.6" "report must show the fork VERSION that was kept"
assert_contains "$REPORT" "没有" "report must state the upstream changes were not applied"
assert_contains "$REPORT" "backend/cmd/server/wire_gen.go" "report must list the fork-owned wiring file"
assert_contains "$REPORT" "移植" "report must tell maintainers to port upstream changes by hand"
assert_contains "$(cat "$GH_BODIES/pr-1.md")" "受保护文件" "PR body must carry the protected-files section"

# 未受保护文件不得被误列入受保护清单
assert_not_contains "$(json_get "$SYNC_OUT" protected_paths_changed_upstream)" "app/upstream-only.txt" \
    "non-protected paths must not be listed as protected"

pass "fork-owned files survive upstream changes and are reported for review"

printf 'PASS %s\n' "$(basename "$0")"
