#!/bin/bash
# 行为 3b：人工提交**冒用机器人身份**（作者/提交者邮箱 = BOT_EMAIL，并照抄 fork-sync
# trailer）时也必须阻塞。
#
# 只按「作者邮箱 == BOT_EMAIL」或「消息里有 trailer」判断是可以通过伪造绕过的，
# 所以工具校验的是完整的「自动化提交形状」：
#   头部是恰好两个 parent 的 merge + 作者与提交者都是机器人身份 + 标题逐字节匹配
#   + 三个 trailer 齐全且为合法 sha + 第二个 parent 恰好等于标记中的上游提交。
# 任何不符 -> 阻塞，绝不在此基础上继续自动合并。
#
# 本测试用干净夹具（没有任何其它人工提交干扰），保证失败/通过都来自伪造检测本身。

set -euo pipefail

source "$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

new_fixture_root
init_upstream
init_fork
write_gh_stub
set_ci_checks '[]'

SYNC_BRANCH="chore/upstream-sync"

# day1：正常的机器人同步，建立分支与 PR
upstream_commit "feat: upstream one" "app/one.txt" "one\n"
WORK_1="$(clone_fork "$FIXTURE_ROOT/work-1")"
REPORT_PATH="$FIXTURE_ROOT/report-1.md"
sync_run "$WORK_1"
assert_eq 0 "$SYNC_STATUS" "day1 must succeed: $SYNC_ERR"
assert_eq "created" "$(json_get "$SYNC_OUT" action)" "day1 action"
BODY_BEFORE="$(cat "$GH_BODIES/pr-1.md")"
REAL_UPSTREAM_SHA="$(upstream_head)"
FAKE_BASE_SHA="$(fork_head)"

# 伪造：plain = 普通提交 + 抄来的 trailer + 机器人身份；
#       merge = 真 merge 形状，但第二个 parent 不是标记里写的上游提交。
for forge_mode in plain merge; do
    TIP_BEFORE="$(sync_branch_sha "$SYNC_BRANCH")"
    FORGED_SHA="$(forge_bot_shaped_commit "$SYNC_BRANCH" "$forge_mode" "$REAL_UPSTREAM_SHA" "$FAKE_BASE_SHA")"
    [[ "$FORGED_SHA" != "$TIP_BEFORE" ]] || fail "the forged commit must advance the branch"

    upstream_commit "feat: upstream after $forge_mode forgery" "app/after-$forge_mode.txt" "after\n"
    WORK_F="$(clone_fork "$FIXTURE_ROOT/work-forge-$forge_mode")"
    REPORT_PATH="$FIXTURE_ROOT/report-forge-$forge_mode.md"
    sync_run "$WORK_F"

    assert_eq 3 "$SYNC_STATUS" \
        "a forged bot-shaped ($forge_mode) head must block: $SYNC_ERR"
    assert_eq "blocked_human_changes" "$(json_get "$SYNC_OUT" action)" \
        "forged ($forge_mode) head must be reported as human changes"
    assert_eq "$FORGED_SHA" "$(sync_branch_sha "$SYNC_BRANCH")" \
        "a forged ($forge_mode) head must not be rewritten or advanced"
    assert_eq 0 "$(grep -c '^pr edit' "$GH_LOG" || true)" \
        "a forged ($forge_mode) head must not rewrite the PR body"
    assert_eq "$BODY_BEFORE" "$(cat "$GH_BODIES/pr-1.md")" \
        "a forged ($forge_mode) head must leave the PR body byte-identical"
    if ref_has_path "$FORK_BARE" "$SYNC_BRANCH" "app/after-$forge_mode.txt"; then
        fail "the upstream commit published after the forgery must not be merged in ($forge_mode)"
    fi
done
pass "bot-identity/trailer forgery is blocked by the automation-commit shape check"

printf 'PASS %s\n' "$(basename "$0")"
