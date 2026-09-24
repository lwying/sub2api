#!/bin/bash
# 行为 9：默认分支前进 / PR 已合并之后，机器人必须继续正常工作，而不是误判。
#
# 覆盖两个已核实的缺陷：
#   (1) fork 默认分支在两次运行之间前进（维护者合并了同步 PR）后，机器人下一次合并
#       不能被「自动化提交形状」误判成人工修改而永久阻塞；
#   (2) PR 已合并但同步分支仍在、上游又没有新提交时，不能去开一个「没有任何提交差异」
#       的 PR：必须识别出无可提议内容并 noop（merge 与 squash 两种合并方式都要成立）。

set -euo pipefail

source "$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

new_fixture_root
init_upstream
init_fork
write_gh_stub
set_ci_checks '[]'

# ---------------------------------------------------------------------------
# (1) 默认分支前进后仍能继续同步
# ---------------------------------------------------------------------------
SYNC_BRANCH="chore/sync-advance"
upstream_commit "feat: upstream one" "app/one.txt" "one\n"
WORK_1="$(clone_fork "$FIXTURE_ROOT/work-advance-1")"
REPORT_PATH="$FIXTURE_ROOT/report-advance-1.md"
sync_run "$WORK_1"
assert_eq 0 "$SYNC_STATUS" "day1 must succeed: $SYNC_ERR"
assert_eq "created" "$(json_get "$SYNC_OUT" action)" "day1 action"
SHA_DAY1="$(sync_branch_sha "$SYNC_BRANCH")"

# 维护者合并 PR：fork 默认分支前进，同步分支尖端成为 main 的祖先
merge_sync_branch_into_fork_main "$SYNC_BRANCH"
MAIN_AFTER_MERGE="$(fork_head)"

# day2：上游又有新提交 -> 必须能继续（此前会被形状校验误判为人工提交而阻塞）
upstream_commit "feat: upstream two" "app/two.txt" "two\n"
WORK_2="$(clone_fork "$FIXTURE_ROOT/work-advance-2")"
REPORT_PATH="$FIXTURE_ROOT/report-advance-2.md"
sync_run "$WORK_2"
assert_eq 0 "$SYNC_STATUS" "day2 after the fork advanced must not be blocked: $SYNC_ERR"
assert_eq "updated" "$(json_get "$SYNC_OUT" action)" "day2 action after the fork advanced"
SHA_DAY2="$(sync_branch_sha "$SYNC_BRANCH")"
[[ "$SHA_DAY2" != "$SHA_DAY1" ]] || fail "day2 must advance the sync branch"
git -C "$FORK_BARE" merge-base --is-ancestor "$SHA_DAY1" "$SHA_DAY2" \
    || fail "day2 must move the branch forward only"
ref_has_path "$FORK_BARE" "$SYNC_BRANCH" app/two.txt || fail "day2 must contain the new upstream file"
pass "a fork default branch that advanced does not block the next sync"

# day3：上游第三天又有提交 -> 仍然要继续（形状校验要在「base 已前进」的链上继续成立）
upstream_commit "feat: upstream three" "app/three.txt" "three\n"
WORK_3="$(clone_fork "$FIXTURE_ROOT/work-advance-3")"
REPORT_PATH="$FIXTURE_ROOT/report-advance-3.md"
sync_run "$WORK_3"
assert_eq 0 "$SYNC_STATUS" "day3 must not be blocked either: $SYNC_ERR"
assert_eq "updated" "$(json_get "$SYNC_OUT" action)" "day3 action"
ref_has_path "$FORK_BARE" "$SYNC_BRANCH" app/three.txt || fail "day3 must contain the third upstream file"
pass "repeated merges with an advancing base keep working"

# ---------------------------------------------------------------------------
# (2) PR 已合并、分支仍在、上游无变化 -> noop（不得开「无差异」PR）
# ---------------------------------------------------------------------------
SHA_BEFORE="$(sync_branch_sha "$SYNC_BRANCH")"
# 真实的 PR 合并：分支以 merge 方式进入默认分支（分支本身保留），桩里也不再有待审 PR
merge_sync_branch_into_fork_main "$SYNC_BRANCH"
gh_stub_mark_merged "$SYNC_BRANCH"
WORK_4="$(clone_fork "$FIXTURE_ROOT/work-merged-noop")"
REPORT_PATH="$FIXTURE_ROOT/report-merged-noop.md"
CREATE_BEFORE="$(grep -c '^pr create' "$GH_LOG" || true)"
sync_run "$WORK_4"
assert_eq 0 "$SYNC_STATUS" "a merged PR with nothing to propose must not fail: $SYNC_ERR"
assert_eq "noop" "$(json_get "$SYNC_OUT" action)" \
    "a branch already contained in the default branch has nothing to propose"
assert_eq "$CREATE_BEFORE" "$(grep -c '^pr create' "$GH_LOG" || true)" \
    "no empty pull request may be created"
assert_eq "$SHA_BEFORE" "$(sync_branch_sha "$SYNC_BRANCH")" "noop must not touch the branch"
pass "a merged PR whose branch remains is a noop, not an empty PR"

# ---------------------------------------------------------------------------
# (2b) squash 合并：分支尖端不是 main 的祖先，但补丁已等价存在 -> 也要 noop
# ---------------------------------------------------------------------------
SYNC_BRANCH="chore/sync-squash"
upstream_commit "feat: upstream four" "app/four.txt" "four\n"
WORK_5="$(clone_fork "$FIXTURE_ROOT/work-squash-1")"
REPORT_PATH="$FIXTURE_ROOT/report-squash-1.md"
sync_run "$WORK_5"
assert_eq 0 "$SYNC_STATUS" "squash scenario day1 must succeed: $SYNC_ERR"
assert_eq "created" "$(json_get "$SYNC_OUT" action)" "squash scenario day1 action"

# 维护者用 squash 合并：main 上出现等价补丁，分支尖端不是 main 的祖先
squash_equivalent_into_fork_main "app/four.txt" "four\n"
gh_stub_mark_merged "$SYNC_BRANCH"
CREATE_BEFORE="$(grep -c '^pr create' "$GH_LOG" || true)"
WORK_6="$(clone_fork "$FIXTURE_ROOT/work-squash-2")"
REPORT_PATH="$FIXTURE_ROOT/report-squash-2.md"
sync_run "$WORK_6"
assert_eq 0 "$SYNC_STATUS" "squash scenario must not fail: $SYNC_ERR"
assert_eq "noop" "$(json_get "$SYNC_OUT" action)" \
    "an equivalent patch already on the default branch means nothing to propose"
assert_eq "$CREATE_BEFORE" "$(grep -c '^pr create' "$GH_LOG" || true)" \
    "a squash-merged PR must not produce an empty follow-up PR"
pass "a squash-merged sync is recognised as nothing to propose"

printf 'PASS %s\n' "$(basename "$0")"
