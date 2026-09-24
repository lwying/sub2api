#!/bin/bash
# 行为 8：推送成功但 PR 创建失败后，必须能恢复，且绝不能「假 noop」。
#
# 场景：day1 同步分支推送成功、`gh pr create` 失败；day2 上游**没有**新提交。
# 期望：
#   - day1 明确报告 PR 步骤失败（非 noop），分支已推送这一事实要如实写出；
#   - day2 **不得**因为「没有新提交」就 noop 掉：同步分支已是最新但没有待审 PR，
#     必须为同一分支补建 PR（只调 gh，不重新推送、不强推）；
#   - day3 PR 已存在且上游仍无变化 -> 正常 noop，不会开出第二个 PR。

set -euo pipefail

source "$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

new_fixture_root
init_upstream
init_fork
write_gh_stub
set_ci_checks '[]'

SYNC_BRANCH="chore/upstream-sync"

# --- day1：推送成功，但 PR 创建失败 ------------------------------------------
upstream_commit "feat: upstream one" "app/one.txt" "one\n"
WORK_1="$(clone_fork "$FIXTURE_ROOT/work-1")"
REPORT_PATH="$FIXTURE_ROOT/report-1.md"
GH_STUB_CREATE_FAIL=1
sync_run "$WORK_1"
GH_STUB_CREATE_FAIL=""

assert_eq 5 "$SYNC_STATUS" "a failed PR creation must fail the run: $SYNC_ERR"
assert_eq "blocked_pr_step_failed" "$(json_get "$SYNC_OUT" action)" \
    "a failed PR creation must be reported, never as noop"
assert_eq 0 "$(wc -l < "$GH_STATE" | tr -d ' ')" "no PR exists after the failed creation"
BRANCH_AFTER_DAY1="$(sync_branch_sha "$SYNC_BRANCH")"
[[ "$BRANCH_AFTER_DAY1" != "absent" ]] || fail "day1 must have pushed the sync branch"
assert_contains "$(cat "$REPORT_PATH")" "已推送成功" \
    "the report must honestly state that the branch was pushed"
pass "a failed PR creation is reported, not silently swallowed"

# --- day1b：dry run 必须完全不写远端（这里正是「需要补建 PR」的状态） ------------
# 该状态下若 DRY_RUN 没在调用 gh 写操作之前生效，就会真的创建 PR。
DRY_SHA_BEFORE="$(sync_branch_sha "$SYNC_BRANCH")"
CREATE_BEFORE_DRY="$(grep -c '^pr create' "$GH_LOG" || true)"
WORK_DRY="$(clone_fork "$FIXTURE_ROOT/work-dry-run")"
REPORT_PATH="$FIXTURE_ROOT/report-dry-run.md"
DRY_RUN=1
sync_run "$WORK_DRY"
DRY_RUN=0
assert_eq 0 "$SYNC_STATUS" "a dry run must succeed: $SYNC_ERR"
assert_eq "would_recover_pr" "$(json_get "$SYNC_OUT" action)" \
    "a dry run in the recovery state must report what it would do"
assert_eq "$CREATE_BEFORE_DRY" "$(grep -c '^pr create' "$GH_LOG" || true)" \
    "a dry run must never call gh pr create"
assert_eq 0 "$(wc -l < "$GH_STATE" | tr -d ' ')" "a dry run must not create a PR"
assert_eq "$DRY_SHA_BEFORE" "$(sync_branch_sha "$SYNC_BRANCH")" \
    "a dry run must not push or move the branch"
assert_contains "$(cat "$REPORT_PATH")" "dry run" "the dry-run report must say so"
pass "a dry run performs no remote writes even in the recovery path"

# --- day2：上游没有新提交，但缺 PR -> 必须补建（不重新推送） -------------------
WORK_2="$(clone_fork "$FIXTURE_ROOT/work-2")"
REPORT_PATH="$FIXTURE_ROOT/report-2.md"
sync_run "$WORK_2"

assert_eq 0 "$SYNC_STATUS" "recovering the missing PR must succeed: $SYNC_ERR"
assert_not_contains "$(json_get "$SYNC_OUT" action)" "noop" \
    "a branch without a pending PR must never be treated as noop"
assert_eq 1 "$(wc -l < "$GH_STATE" | tr -d ' ')" "the recovery run must create exactly one PR"
# day1 的失败尝试 + day2 的重试 = 2 次调用，但真正建成的 PR 只有 1 个
assert_eq 2 "$(grep -c '^pr create' "$GH_LOG" || true)" "pr create must have been retried"
assert_eq "$BRANCH_AFTER_DAY1" "$(sync_branch_sha "$SYNC_BRANCH")" \
    "the recovery must not re-push or move the branch"
assert_contains "$(cat "$REPORT_PATH")" "待审 PR" \
    "the recovery report must explain why a PR was created without new upstream commits"
pass "a missing pending PR is recovered without touching the branch"

# --- day3：PR 已存在、上游仍无变化 -> 正常 noop，且不重复开票 ------------------
WORK_3="$(clone_fork "$FIXTURE_ROOT/work-3")"
REPORT_PATH="$FIXTURE_ROOT/report-3.md"
sync_run "$WORK_3"
assert_eq 0 "$SYNC_STATUS" "day3 must succeed: $SYNC_ERR"
assert_eq "noop" "$(json_get "$SYNC_OUT" action)" "with a pending PR and no upstream change it is a noop"
assert_eq 1 "$(wc -l < "$GH_STATE" | tr -d ' ')" "no second PR may appear"
assert_eq 2 "$(grep -c '^pr create' "$GH_LOG" || true)" "pr create must not be called again once a PR exists"
assert_eq "$BRANCH_AFTER_DAY1" "$(sync_branch_sha "$SYNC_BRANCH")" "noop must not move the branch"
pass "once a pending PR exists the noop path is restored"

# --- day4：PR 已被合并、上游又有新提交，但 gh pr create 返回不可用的 URL --------
# 退出码 0 不代表我们拿到了本 fork 的 PR：不能据此宣称创建成功。
sed -e 's/\tOPEN\t/\tMERGED\t/' "$GH_STATE" > "$GH_STATE.merged" && mv "$GH_STATE.merged" "$GH_STATE"
upstream_commit "feat: upstream after the PR was merged" "app/after-merge.txt" "after\n"
WORK_4="$(clone_fork "$FIXTURE_ROOT/work-4")"
REPORT_PATH="$FIXTURE_ROOT/report-4.md"
BRANCH_BEFORE_DAY4="$(sync_branch_sha "$SYNC_BRANCH")"

for bad_url in 'not-a-url' 'https://github.com/Wei-Shaw/sub2api/pull/9'; do
    GH_STUB_CREATE_MALFORMED="$bad_url"
    sync_run "$WORK_4"
    GH_STUB_CREATE_MALFORMED=""
    assert_eq 5 "$SYNC_STATUS" "an unusable create output ('$bad_url') must fail the run: $SYNC_ERR"
    assert_eq "blocked_pr_step_failed" "$(json_get "$SYNC_OUT" action)" \
        "an unusable create output ('$bad_url') must not be reported as success"
    assert_eq "0" "$(json_get "$SYNC_OUT" pending_pr.number)" \
        "no PR number may be claimed when the output cannot be validated ('$bad_url')"
    assert_contains "$(cat "$REPORT_PATH")" "无法确认" \
        "the report must say the PR creation could not be confirmed ('$bad_url')"
done

assert_eq 0 "$(awk -F'\t' '$2 == "OPEN"' "$GH_STATE" | wc -l | tr -d ' ')" \
    "an unusable create output must not produce or claim a pending PR"
[[ "$(sync_branch_sha "$SYNC_BRANCH")" != "absent" ]] || fail "the pushed branch must be kept"
pass "an unusable create output fails closed instead of claiming success"

# --- day5：上游无新提交，缺待审 PR -> 仍然补建（分支内容已在 day4 推上去） -------
BRANCH_AFTER_DAY4="$(sync_branch_sha "$SYNC_BRANCH")"
WORK_5="$(clone_fork "$FIXTURE_ROOT/work-5")"
REPORT_PATH="$FIXTURE_ROOT/report-5.md"
sync_run "$WORK_5"
assert_eq 0 "$SYNC_STATUS" "recovery after an unusable create output must succeed: $SYNC_ERR"
assert_eq "recovered_pr" "$(json_get "$SYNC_OUT" action)" "the missing PR must be recovered"
assert_eq "$BRANCH_AFTER_DAY4" "$(sync_branch_sha "$SYNC_BRANCH")" \
    "recovery must not re-push the already-pushed branch"
assert_eq 1 "$(awk -F'\t' '$2 == "OPEN"' "$GH_STATE" | wc -l | tr -d ' ')" \
    "exactly one pending PR must exist after recovery"
pass "the branch pushed by the failed run is reused, not pushed again"

printf 'PASS %s\n' "$(basename "$0")"
