#!/bin/bash
# 行为 2：上游合并与 fork 冲突时，保留现状、报告阻塞，且下一次运行不覆盖。
#
# 场景与票据验收 2 对齐：
#   - 冲突（非受保护文件）      -> 阻塞、无分支、无 PR、fork 默认分支不变；
#   - 再跑一次                  -> 结果一致，仍不覆盖；
#   - 维护者手工建同步分支解决  -> 下一次运行识别为人工修复并阻塞，分支字节不变。

set -euo pipefail

source "$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

new_fixture_root
init_upstream
init_fork
write_gh_stub
set_ci_checks '[]'

SYNC_BRANCH="chore/upstream-sync"

# fork 与上游修改同一行 -> 真实冲突
fork_commit "feat(fork): fork keeps its own normal()" "app/normal.go" \
    'package app

func Normal() string { return "fork-v1" }
'
upstream_commit "feat(upstream): upstream changes normal()" "app/normal.go" \
    'package app

func Normal() string { return "upstream-v2" }
'

FORK_MAIN_BEFORE="$(fork_head)"

WORK_1="$(clone_fork "$FIXTURE_ROOT/work-1")"
REPORT_PATH="$FIXTURE_ROOT/report-1.md"
sync_run "$WORK_1"
assert_eq 2 "$SYNC_STATUS" "conflict must exit with the conflict code: $SYNC_ERR"
assert_eq "blocked_conflict" "$(json_get "$SYNC_OUT" action)" "conflict action"
assert_eq '["app/normal.go"]' "$(json_get "$SYNC_OUT" blocked.paths)" "conflict paths"
assert_not_contains "$(fork_branch_list)" "$SYNC_BRANCH" \
    "no sync branch may be created on conflict"
assert_eq 0 "$(wc -l < "$GH_STATE" | tr -d ' ')" "no PR may be opened on conflict"
assert_not_contains "$(cat "$GH_LOG")" "pr create" "conflict must not create a PR"
assert_eq "$FORK_MAIN_BEFORE" "$(fork_head)" "fork default branch must be untouched"
assert_contains "$(cat "$REPORT_PATH")" "app/normal.go" "report must name the conflicting path"
assert_not_contains "$(cat "$REPORT_PATH")" "动作：created" "blocked run must not claim a PR was created"
pass "conflicting upstream merge blocks without touching the fork"

# 再跑一次：同样阻塞，仍然不覆盖（幂等，不会因为重试就强推）
WORK_2="$(clone_fork "$FIXTURE_ROOT/work-2")"
REPORT_PATH="$FIXTURE_ROOT/report-2.md"
sync_run "$WORK_2"
assert_eq 2 "$SYNC_STATUS" "second run must also block: $SYNC_ERR"
assert_eq "blocked_conflict" "$(json_get "$SYNC_OUT" action)" "second run action"
assert_eq "absent" "$(sync_branch_sha "$SYNC_BRANCH")" "second run must not create a branch either"
pass "repeated runs keep blocking without overwriting anything"

# 维护者手工解决冲突并把结果推到同步分支上去
HUMAN_SHA="$(human_create_sync_branch "$SYNC_BRANCH" \
    "fix(fork-sync): resolve upstream conflict by hand" \
    "app/normal.go" 'package app

func Normal() string { return "fork-v1-resolved" }
')"
[[ "$HUMAN_SHA" != "absent" ]] || fail "human branch must exist"

# 上游又前进了一次
upstream_commit "feat(upstream): another upstream change" "app/third.txt" "third\n"

WORK_3="$(clone_fork "$FIXTURE_ROOT/work-3")"
REPORT_PATH="$FIXTURE_ROOT/report-3.md"
sync_run "$WORK_3"
assert_eq 3 "$SYNC_STATUS" "human resolution must block: $SYNC_ERR"
assert_eq "blocked_human_changes" "$(json_get "$SYNC_OUT" action)" "human resolution action"
assert_eq "$HUMAN_SHA" "$(sync_branch_sha "$SYNC_BRANCH")" \
    "human resolution must not be overwritten or rewritten"
assert_eq "$(ref_blob_sha "$FORK_BARE" "$HUMAN_SHA" app/normal.go)" \
    "$(ref_blob_sha "$FORK_BARE" "$SYNC_BRANCH" app/normal.go)" \
    "human resolution content must survive byte-for-byte"
assert_eq 0 "$(wc -l < "$GH_STATE" | tr -d ' ')" "no PR may be opened while a human resolution is pending"
pass "manual conflict resolution is preserved and reported as blocked"

printf 'PASS %s\n' "$(basename "$0")"
