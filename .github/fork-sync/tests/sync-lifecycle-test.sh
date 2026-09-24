#!/bin/bash
# 行为 1：连续两天的上游提交最终只维持一个待审 PR；无上游变更时不重复开 PR。
#
# 场景与票据验收 1 对齐：
#   day1 upstream 有新提交、fork 无同步分支        -> 新建分支 + 新建唯一 PR
#   day2 upstream 又有新提交（同一 fork 分支仍在）  -> 更新同一分支 + 更新同一 PR
#   day3 upstream 无新提交                        -> noop，分支与 PR 不动
#   fork 已与 upstream 一致                        -> noop，不开 PR

set -euo pipefail

source "$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

new_fixture_root
init_upstream
init_fork
write_gh_stub
set_ci_checks '[]'

SYNC_BRANCH="chore/upstream-sync"

# --- 场景 A：fork 已与 upstream 一致时无事可做 ---------------------------------
align_fork_to_upstream
WORK_A="$(clone_fork "$FIXTURE_ROOT/work-a")"
sync_run "$WORK_A"
assert_eq 0 "$SYNC_STATUS" "noop run must succeed"
assert_eq "noop" "$(json_get "$SYNC_OUT" action)" "action for aligned fork"
assert_eq 0 "$(wc -l < "$GH_STATE" | tr -d ' ')" "no PR may be opened when nothing changed"
pass "noop when fork already matches upstream (no PR created)"

# --- day1：上游有新提交 --------------------------------------------------------
upstream_commit "feat: upstream change one" "app/one.txt" "one\n"
WORK_1="$(clone_fork "$FIXTURE_ROOT/work-1")"
sync_run "$WORK_1"
assert_eq 0 "$SYNC_STATUS" "day1 run must succeed: $SYNC_ERR"
assert_eq "created" "$(json_get "$SYNC_OUT" action)" "day1 action"
assert_eq 1 "$(json_get "$SYNC_OUT" pending_pr.number)" "day1 PR number"
DAY1_SHA="$(sync_branch_sha "$SYNC_BRANCH")"
[[ "$DAY1_SHA" != "absent" ]] || fail "day1 must create the sync branch"
assert_eq 1 "$(wc -l < "$GH_STATE" | tr -d ' ')" "exactly one PR after day1"
assert_file_exists "$GH_BODIES/pr-1.md"

# 分支内容必须带上上游那次提交的文件
ref_has_path "$FORK_BARE" "$SYNC_BRANCH" app/one.txt \
    || fail "sync branch must contain the upstream file"
# 且不得改动 fork 的受保护文件
assert_eq "0.2.6" "$(ref_first_line "$FORK_BARE" "$SYNC_BRANCH" backend/cmd/server/VERSION | tr -d '\r\n')" \
    "fork VERSION must be preserved"
pass "day1: single branch + single PR created"

# --- day2：上游又有新提交，必须更新同一个 PR ------------------------------------
upstream_commit "feat: upstream change two" "app/two.txt" "two\n"
WORK_2="$(clone_fork "$FIXTURE_ROOT/work-2")"
REPORT_PATH="$FIXTURE_ROOT/report-2.md"
sync_run "$WORK_2"
assert_eq 0 "$SYNC_STATUS" "day2 run must succeed: $SYNC_ERR"
assert_eq "updated" "$(json_get "$SYNC_OUT" action)" "day2 action"
assert_eq 1 "$(json_get "$SYNC_OUT" pending_pr.number)" "day2 must reuse PR #1"
assert_eq 1 "$(wc -l < "$GH_STATE" | tr -d ' ')" "still exactly one PR after day2"
assert_eq 1 "$(grep -c '^pr create' "$GH_LOG" || true)" "pr create must be called exactly once in total"
assert_eq 1 "$(grep -c '^pr edit' "$GH_LOG" || true)" "day2 must update the existing PR body"

ref_has_path "$FORK_BARE" "$SYNC_BRANCH" app/one.txt || fail "day2 must keep day1 changes"
ref_has_path "$FORK_BARE" "$SYNC_BRANCH" app/two.txt || fail "day2 must add day2 changes"

DAY2_SHA="$(sync_branch_sha "$SYNC_BRANCH")"
[[ "$DAY2_SHA" != "$DAY1_SHA" ]] || fail "day2 must advance the sync branch"
git -C "$FORK_BARE" merge-base --is-ancestor "$DAY1_SHA" "$DAY2_SHA" \
    || fail "day2 must move the branch forward only (no history rewrite)"
assert_contains "$(cat "$GH_BODIES/pr-1.md")" "app/two.txt" "day2 PR body must mention the new upstream change"
pass "day2: same branch advanced, same PR updated"

# --- day3：上游无新提交 --------------------------------------------------------
WORK_3="$(clone_fork "$FIXTURE_ROOT/work-3")"
sync_run "$WORK_3"
assert_eq 0 "$SYNC_STATUS" "day3 run must succeed: $SYNC_ERR"
assert_eq "noop" "$(json_get "$SYNC_OUT" action)" "day3 action"
assert_eq "$DAY2_SHA" "$(sync_branch_sha "$SYNC_BRANCH")" "day3 must not touch the branch"
assert_eq 1 "$(wc -l < "$GH_STATE" | tr -d ' ')" "day3 must not add a PR"
assert_eq 1 "$(grep -c '^pr create' "$GH_LOG" || true)" "day3 must not create another PR"
assert_eq 1 "$(grep -c '^pr edit' "$GH_LOG" || true)" "day3 must not rewrite the PR body"
pass "day3: noop leaves branch and PR untouched"

# --- 异常：同步分支上已经有两个待审 PR ---------------------------------------
# 机器人不得再新增第三个：一律阻塞，交维护者合并/关闭多余 PR。
upstream_commit "feat: upstream change four" "app/four.txt" "four\n"
gh_stub_add_pr 7 "$SYNC_BRANCH"
BRANCH_BEFORE="$(sync_branch_sha "$SYNC_BRANCH")"
WORK_4="$(clone_fork "$FIXTURE_ROOT/work-4")"
REPORT_PATH="$FIXTURE_ROOT/report-4.md"
sync_run "$WORK_4"
assert_eq 4 "$SYNC_STATUS" "two pending PRs must block with the ambiguity code: $SYNC_ERR"
assert_eq "blocked_ambiguous_prs" "$(json_get "$SYNC_OUT" action)" "ambiguous PR action"
assert_eq "$BRANCH_BEFORE" "$(sync_branch_sha "$SYNC_BRANCH")" \
    "an ambiguous PR state must not push anything"
assert_eq 2 "$(wc -l < "$GH_STATE" | tr -d ' ')" "no third PR may be created"
assert_eq 1 "$(grep -c '^pr create' "$GH_LOG" || true)" "pr create must still have run exactly once"
assert_contains "$(cat "$REPORT_PATH")" "待审 PR" "the report must explain the duplicate-PR blockage"
pass "more than one pending PR blocks instead of opening another"

printf 'PASS %s\n' "$(basename "$0")"
