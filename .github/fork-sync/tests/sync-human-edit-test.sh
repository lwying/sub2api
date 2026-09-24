#!/bin/bash
# 行为 3：同步分支上出现人工提交后，机器人停手、报告阻塞，绝不覆盖人工修复。
#
# 场景与票据验收 2 对齐：自动化分支被维护者接管（可能是解决冲突或补充修复），
# 之后的定时任务必须（a）识别出人工提交、（b）不推送、不重写历史、
# （c）不覆盖 PR 正文、（d）给出明确的阻塞信息。

set -euo pipefail

source "$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

new_fixture_root
init_upstream
init_fork
write_gh_stub
set_ci_checks '[]'

SYNC_BRANCH="chore/upstream-sync"

# day1：机器人建立同步分支与唯一 PR
upstream_commit "feat: upstream one" "app/one.txt" "one\n"
WORK_1="$(clone_fork "$FIXTURE_ROOT/work-1")"
REPORT_PATH="$FIXTURE_ROOT/report-1.md"
sync_run "$WORK_1"
assert_eq 0 "$SYNC_STATUS" "day1 must succeed: $SYNC_ERR"
assert_eq "created" "$(json_get "$SYNC_OUT" action)" "day1 action"
BOT_SHA="$(sync_branch_sha "$SYNC_BRANCH")"
BODY_BEFORE="$(cat "$GH_BODIES/pr-1.md")"

# 维护者在同步分支上追加人工修复提交
HUMAN_SHA="$(human_commit_on_sync_branch "$SYNC_BRANCH" "fix(fork-sync): maintainer hotfix on the sync branch")"
[[ "$HUMAN_SHA" != "$BOT_SHA" ]] || fail "human commit must advance the branch"

# 上游继续前进
upstream_commit "feat: upstream two" "app/two.txt" "two\n"

WORK_2="$(clone_fork "$FIXTURE_ROOT/work-2")"
REPORT_PATH="$FIXTURE_ROOT/report-2.md"
sync_run "$WORK_2"

assert_eq 3 "$SYNC_STATUS" "human-owned branch must block with the human exit code: $SYNC_ERR"
assert_eq "blocked_human_changes" "$(json_get "$SYNC_OUT" action)" "human commit detection"
assert_eq "[\"$HUMAN_SHA\"]" "$(json_get "$SYNC_OUT" blocked.paths)" "blocked paths must name the human commit"
assert_eq "$HUMAN_SHA" "$(sync_branch_sha "$SYNC_BRANCH")" "human commit must not be overwritten"
assert_eq 1 "$(wc -l < "$GH_STATE" | tr -d ' ')" "no new PR may be opened"
assert_eq 0 "$(grep -c '^pr edit' "$GH_LOG" || true)" "PR body must not be rewritten for a human-owned branch"
assert_eq "$BODY_BEFORE" "$(cat "$GH_BODIES/pr-1.md")" "PR body must be byte-identical"
assert_eq "fix(fork-sync): maintainer hotfix on the sync branch" \
    "$(git -C "$FORK_BARE" log -1 --format=%s "$SYNC_BRANCH")" "human commit message must survive"

REPORT="$(cat "$REPORT_PATH")"
assert_contains "$REPORT" "$HUMAN_SHA" "report must name the human commit"
assert_contains "$REPORT" "未验证" "report must still state that nothing is verified"
assert_not_contains "$REPORT" "已强推" "report must not claim a force push happened"

# 人工提交的内容不得被上游合并覆盖：分支 tip 仍是人工那次提交的树
assert_eq "$(git -C "$FORK_BARE" rev-parse "${SYNC_BRANCH}^{tree}")" \
    "$(git -C "$FORK_BARE" rev-parse "$HUMAN_SHA^{tree}")" "branch tree must still be the human one"

pass "human edits on the sync branch are detected, reported and never overwritten"

printf 'PASS %s\n' "$(basename "$0")"
