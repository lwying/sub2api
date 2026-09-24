#!/bin/bash
# 行为 5：注入抵抗、分支名校验、CI 状态只报事实、以及绝不执行破坏性操作。
#
# 覆盖票据要求：
#   - GitHub 事件/上游内容/分支名不得被当成 shell 代码执行（含文件名与提交信息里的元字符）；
#   - 非法分支名（大小写不同的默认分支、以 '-' 开头、含空格/分号/..）必须拒绝且不推送；
#   - CI 未批准 / 检查未成功 / 没有检查 -> 一律显示「未验证」，绝不声称是绿的；
#   - 不调用 gh pr merge/close/release，不打 tag，不推送默认分支。

set -euo pipefail

source "$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

new_fixture_root
init_upstream
init_fork
write_gh_stub

SYNC_BRANCH="chore/upstream-sync"
CANARY_NAME="fork-sync-canary-pwned.txt"

# ---------------------------------------------------------------------------
# A. CI 状态：只报事实
# ---------------------------------------------------------------------------
upstream_commit 'feat: inject $(touch '"$CANARY_NAME"') `touch '"$CANARY_NAME"'` ; rm -rf / 注入测试' \
    'app/upstream.txt' "upstream\n"
# 文件名本身带元字符（git 允许），同步工具必须把它当普通路径处理
upstream_commit 'feat: weird paths' 'app/x;touch '"$CANARY_NAME"';.txt' "weird\n"
upstream_commit 'feat: dollar path' 'app/dollar$(id).txt' "dollar\n"
upstream_commit 'feat: backtick path' 'app/back`id`.txt' "backtick\n"
upstream_commit 'feat: dash path' '-leading-dash.txt' "dash\n"

# day1：新建 PR，尚无检查
set_ci_checks '[]'
WORK_1="$(clone_fork "$FIXTURE_ROOT/work-1")"
REPORT_PATH="$FIXTURE_ROOT/report-1.md"
sync_run "$WORK_1"
assert_eq 0 "$SYNC_STATUS" "day1 must succeed: $SYNC_ERR"
assert_eq "created" "$(json_get "$SYNC_OUT" action)" "day1 action"
assert_eq "unverified" "$(json_get "$SYNC_OUT" ci.state)" "a brand new PR can never be reported as passed"
assert_contains "$(cat "$REPORT_PATH")" "未验证" "report must state it is unverified"
assert_not_contains "$(cat "$REPORT_PATH")" "已验证通过" "report must not claim verification"

# day2：检查仍在等待批准 -> 仍未验证
upstream_commit 'feat: second upstream change' 'app/second.txt' "second\n"
set_ci_checks '[{"name":"backend-ci","state":"PENDING"},{"name":"lint","state":"ACTION_REQUIRED"}]'
WORK_2="$(clone_fork "$FIXTURE_ROOT/work-2")"
REPORT_PATH="$FIXTURE_ROOT/report-2.md"
sync_run "$WORK_2"
assert_eq 0 "$SYNC_STATUS" "day2 must succeed: $SYNC_ERR"
assert_eq "updated" "$(json_get "$SYNC_OUT" action)" "day2 action"
assert_eq "unverified" "$(json_get "$SYNC_OUT" ci.state)" "pending checks must stay unverified"
assert_contains "$(cat "$REPORT_PATH")" "PENDING" "report must surface the real check states"
assert_contains "$(cat "$REPORT_PATH")" "未验证" "report must not call pending CI green"

# day3：检查全部成功 -> 仍要求维护者人工确认，且不发布
upstream_commit 'feat: third upstream change' 'app/third.txt' "third\n"
set_ci_checks '[{"name":"backend-ci","state":"SUCCESS"},{"name":"lint","state":"SUCCESS"}]'
WORK_3="$(clone_fork "$FIXTURE_ROOT/work-3")"
REPORT_PATH="$FIXTURE_ROOT/report-3.md"
sync_run "$WORK_3"
assert_eq 0 "$SYNC_STATUS" "day3 must succeed: $SYNC_ERR"
assert_eq "passed" "$(json_get "$SYNC_OUT" ci.state)" "all-success checks may be reported as passed"
REPORT="$(cat "$REPORT_PATH")"
assert_contains "$REPORT" "人工审查" "even with green checks the report must require maintainer review"
assert_contains "$REPORT" "不会**自动打 tag" "report must state that merging does not publish"
assert_contains "$REPORT" "未验证" "verification section must remain unverified until a human confirms"
assert_contains "$REPORT" "没有触及 fork 自有路径" \
    "a sync that leaves protected files alone must say so instead of listing fork divergence"

pass "CI state is reported factually and never treated as verification"

# --- A2. `gh pr checks` 在检查未完成/失败时会以非零码退出，但**仍然打印状态**：
#        工具不能因为退出码就把这些状态丢掉、谎称「没有检查上报」。
upstream_commit 'feat: pending checks' 'app/pending.txt' "pending\n"
set_ci_checks '[{"name":"backend-ci","state":"PENDING"},{"name":"lint","state":"QUEUED"}]'
GH_STUB_CHECKS_EXIT=8
WORK_P="$(clone_fork "$FIXTURE_ROOT/work-checks-pending")"
REPORT_PATH="$FIXTURE_ROOT/report-checks-pending.md"
sync_run "$WORK_P"
GH_STUB_CHECKS_EXIT=""
assert_eq 0 "$SYNC_STATUS" "a pending-checks run must still succeed: $SYNC_ERR"
assert_eq "unverified" "$(json_get "$SYNC_OUT" ci.state)" "pending checks must stay unverified"
assert_contains "$(json_get "$SYNC_OUT" ci.detail)" "PENDING" \
    "the reported detail must keep the real check states, not claim none were reported"
assert_not_contains "$(json_get "$SYNC_OUT" ci.detail)" "no checks reported" \
    "states printed by a failing gh pr checks must not be discarded"
assert_contains "$(cat "$REPORT_PATH")" "PENDING" "the report must show the real check states"

upstream_commit 'feat: failing checks' 'app/failing.txt' "failing\n"
set_ci_checks '[{"name":"backend-ci","state":"FAILURE"}]'
GH_STUB_CHECKS_EXIT=1
WORK_F="$(clone_fork "$FIXTURE_ROOT/work-checks-failing")"
REPORT_PATH="$FIXTURE_ROOT/report-checks-failing.md"
sync_run "$WORK_F"
GH_STUB_CHECKS_EXIT=""
assert_eq 0 "$SYNC_STATUS" "a failing-checks run must still succeed: $SYNC_ERR"
assert_eq "unverified" "$(json_get "$SYNC_OUT" ci.state)" "failing checks must never be green"
assert_contains "$(json_get "$SYNC_OUT" ci.detail)" "FAILURE" \
    "the reported detail must name the failing check state"
assert_contains "$(cat "$REPORT_PATH")" "FAILURE" "the report must show the failing check"
set_ci_checks '[]'
pass "check states survive a non-zero gh pr checks exit code"

# ---------------------------------------------------------------------------
# B. 注入抵抗
# ---------------------------------------------------------------------------
# 没有任何被注入的命令被执行：fixture 目录下不得出现 canary 文件。
if find "$FIXTURE_ROOT" -name "$CANARY_NAME" -print -quit | grep -q .; then
    fail "injected shell command executed (canary file created)"
fi
# 元字符只是数据：必须原样出现在报告里，并被当成普通路径同步
assert_contains "$(cat "$FIXTURE_ROOT/report-1.md")" 'app/dollar$(id).txt' \
    "metacharacter path must appear literally in the report"
for odd in 'app/x;touch '"$CANARY_NAME"';.txt' 'app/dollar$(id).txt' 'app/back`id`.txt' '-leading-dash.txt'; do
    ref_has_path "$FORK_BARE" "$SYNC_BRANCH" "$odd" \
        || fail "oddly named upstream file must still be synced as a plain path: $odd"
done
if find "$FIXTURE_ROOT" -name "$CANARY_NAME" -print -quit | grep -q .; then
    fail "injected shell command executed (canary file created)"
fi
pass "shell metacharacters in event content, commit messages and paths are inert"

# ---------------------------------------------------------------------------
# C. 分支名与受保护清单校验
# ---------------------------------------------------------------------------
for bad in "main" "MAIN" "-dash" "a b" "x;touch pwned" "../escape" "refs/heads/x" "x..y" "HEAD"; do
    SYNC_BRANCH="$bad"
    sync_run "$FIXTURE_ROOT/work-1"
    assert_eq 1 "$SYNC_STATUS" "sync branch '${bad}' must be rejected as a usage error"
    assert_not_contains "$(fork_branch_list)" "pwned" "rejected branch names must not touch the remote"
done
assert_eq "$(printf 'chore/upstream-sync\nmain')" "$(fork_branch_list)" \
    "rejected branch names must not create any branch on the remote"
pass "branch names are validated against an allowlist before any git command runs"

# 非法受保护清单（前导 '-' / '..'）同样必须失败
SYNC_BRANCH="chore/upstream-sync"
set +e
( cd "$FIXTURE_ROOT/work-1" && REPO_DIR="$FIXTURE_ROOT/work-1" \
    FORK_URL="$FORK_BARE" UPSTREAM_URL="$UPSTREAM_BARE" PUSH_URL="$FORK_BARE" \
    PROTECTED_PATHS='-evil' GH_BIN="$GH_BIN" GH_STUB_STATE="$GH_STATE" \
    GH_STUB_BODIES="$GH_BODIES" GH_STUB_LOG="$GH_LOG" \
    bash "$FORK_SYNC_SCRIPT" >/dev/null 2>&1 )
bad_protected_status=$?
set -e
assert_eq 1 "$bad_protected_status" "a malformed protected-path list must be rejected"
pass "protected path list is validated"

# ---------------------------------------------------------------------------
# D. 绝不执行破坏性操作
# ---------------------------------------------------------------------------
assert_not_contains "$(cat "$GH_LOG")" "pr merge" "the sync tool must never merge a PR"
assert_not_contains "$(cat "$GH_LOG")" "pr close" "the sync tool must never close a PR"
assert_not_contains "$(cat "$GH_LOG")" "pr delete" "the sync tool must never delete anything"
assert_not_contains "$(cat "$GH_LOG")" "release" "the sync tool must never touch releases"
assert_eq "" "$(git -C "$FORK_BARE" tag)" "no tags may be created"
assert_not_contains "$(grep -c '^pr list' "$GH_LOG")" "0" "the tool should list PRs, not blindly create them"
pass "no destructive gh or git operations were performed"

# ---------------------------------------------------------------------------
# E. gh pr list 失败必须 fail-closed：不能因为「看起来没有待审 PR」而重复开票
# ---------------------------------------------------------------------------
upstream_commit 'feat: upstream while listing is broken' 'app/list-fail.txt' "listfail\n"
BRANCH_BEFORE="$(sync_branch_sha "$SYNC_BRANCH")"
PR_COUNT_BEFORE="$(wc -l < "$GH_STATE" | tr -d ' ')"
CREATE_BEFORE="$(grep -c '^pr create' "$GH_LOG" || true)"

WORK_E="$(clone_fork "$FIXTURE_ROOT/work-list-fail")"
GH_STUB_LIST_FAIL=1
sync_run "$WORK_E"
GH_STUB_LIST_FAIL=""
assert_eq 5 "$SYNC_STATUS" "a failed gh pr list must stop the run: $SYNC_ERR"
assert_eq "blocked_tooling" "$(json_get "$SYNC_OUT" action)" "gh pr list failure must be reported as blocked_tooling"
assert_eq "$BRANCH_BEFORE" "$(sync_branch_sha "$SYNC_BRANCH")" \
    "a failed gh pr list must not push anything"
assert_eq "$PR_COUNT_BEFORE" "$(wc -l < "$GH_STATE" | tr -d ' ')" "a failed gh pr list must not create a second PR"
assert_eq "$CREATE_BEFORE" "$(grep -c '^pr create' "$GH_LOG" || true)" \
    "a failed gh pr list must not call pr create"
pass "a failed PR listing fails closed instead of opening a duplicate PR"

# 恢复后仍能正常更新同一个 PR
sync_run "$(clone_fork "$FIXTURE_ROOT/work-list-recover")"
assert_eq 0 "$SYNC_STATUS" "after the listing recovers the sync must work again: $SYNC_ERR"
assert_eq 1 "$(wc -l < "$GH_STATE" | tr -d ' ')" "still exactly one PR after recovery"
pass "sync resumes and keeps a single PR once listing works again"

# ---------------------------------------------------------------------------
# F. 本地破坏性 git 操作的门禁：必须是一次性检出，且工作树干净
# ---------------------------------------------------------------------------
# F1：没有显式声明「一次性检出」-> 拒绝运行，且不碰远端
WORK_F1="$(clone_fork "$FIXTURE_ROOT/work-no-optin")"
GH_CALLS_BEFORE="$(wc -l < "$GH_LOG" | tr -d ' ')"
BRANCH_BEFORE="$(sync_branch_sha "$SYNC_BRANCH")"
FORK_SYNC_DISPOSABLE_CHECKOUT=""
sync_run "$WORK_F1"
assert_eq 1 "$SYNC_STATUS" "a missing disposable-checkout opt-in must be a usage error"
assert_eq "blocked_no_disposable_checkout" "$(json_get "$SYNC_OUT" action)" "opt-in guard must be reported"
assert_eq "$BRANCH_BEFORE" "$(sync_branch_sha "$SYNC_BRANCH")" "the opt-in guard must not touch the remote"
assert_eq "$GH_CALLS_BEFORE" "$(wc -l < "$GH_LOG" | tr -d ' ')" "the opt-in guard must not call gh at all"
[[ -z "$(git -C "$WORK_F1" rev-parse --verify --quiet refs/remotes/fork-sync/base || true)" ]] \
    || fail "the opt-in guard must run before any fetch"
pass "missing disposable-checkout opt-in is refused before any git mutation"

# F2：工作树脏（含未跟踪文件）-> 拒绝运行，内容字节不变
FORK_SYNC_DISPOSABLE_CHECKOUT=1
WORK_F2="$(clone_fork "$FIXTURE_ROOT/work-dirty")"
printf 'local work in progress\n' >> "$WORK_F2/app/normal.go"
printf 'untracked scratch\n' > "$WORK_F2/app/untracked-scratch.txt"
DIRTY_SHA="$(ref_blob_sha "$WORK_F2" HEAD app/normal.go)"
DIRTY_BYTES="$(cksum "$WORK_F2/app/normal.go" | awk '{ print $1 "-" $2 }')"
GH_CALLS_BEFORE="$(wc -l < "$GH_LOG" | tr -d ' ')"
BRANCH_BEFORE="$(sync_branch_sha "$SYNC_BRANCH")"
sync_run "$WORK_F2"
assert_eq 1 "$SYNC_STATUS" "a dirty work tree must be refused"
assert_eq "blocked_dirty_worktree" "$(json_get "$SYNC_OUT" action)" "dirty-tree guard must be reported"
assert_eq "$DIRTY_BYTES" "$(cksum "$WORK_F2/app/normal.go" | awk '{ print $1 "-" $2 }')" \
    "the dirty file must be byte-identical after the refused run"
assert_file_exists "$WORK_F2/app/untracked-scratch.txt"
assert_eq "$DIRTY_SHA" "$(ref_blob_sha "$WORK_F2" HEAD app/normal.go)" "local WIP must stay local"
assert_eq "$BRANCH_BEFORE" "$(sync_branch_sha "$SYNC_BRANCH")" "a refused run must not move any branch"
assert_eq "$GH_CALLS_BEFORE" "$(wc -l < "$GH_LOG" | tr -d ' ')" "a refused run must not call gh"
[[ -z "$(git -C "$WORK_F2" rev-parse --verify --quiet refs/remotes/fork-sync/base || true)" ]] \
    || fail "the dirty-tree guard must run before any fetch"
pass "a dirty work tree is refused without touching files, branches or the remote"

# ---------------------------------------------------------------------------
# G. CI 可见性：默认 GITHUB_TOKEN 建/改的 PR 不会触发 workflow，
#    「没有检查」不能被说成「等待批准」，更不能说成通过。
# ---------------------------------------------------------------------------
upstream_commit 'feat: token visibility' 'app/token-visibility.txt' "visibility\n"

FORK_SYNC_TOKEN_KIND="github_token"
WORK_G1="$(clone_fork "$FIXTURE_ROOT/work-token-default")"
REPORT_PATH="$FIXTURE_ROOT/report-token-default.md"
sync_run "$WORK_G1"
assert_eq 0 "$SYNC_STATUS" "default-token run must succeed: $SYNC_ERR"
assert_eq "github_token" "$(json_get "$SYNC_OUT" token_kind)" "token kind must be reported"
REPORT_DEFAULT="$(cat "$REPORT_PATH")"
assert_contains "$REPORT_DEFAULT" "GITHUB_TOKEN" "the report must name the token that was used"
assert_contains "$REPORT_DEFAULT" "不会" "the report must state that GITHUB_TOKEN does not trigger the PR workflows"
pass "default GITHUB_TOKEN runs state that CI may not have started at all"

FORK_SYNC_TOKEN_KIND="dedicated"
upstream_commit 'feat: dedicated token visibility' 'app/token-visibility-2.txt' "visibility2\n"
WORK_G2="$(clone_fork "$FIXTURE_ROOT/work-token-dedicated")"
REPORT_PATH="$FIXTURE_ROOT/report-token-dedicated.md"
sync_run "$WORK_G2"
assert_eq 0 "$SYNC_STATUS" "dedicated-token run must succeed: $SYNC_ERR"
assert_eq "dedicated" "$(json_get "$SYNC_OUT" token_kind)" "dedicated token kind must be reported"
REPORT_DEDICATED="$(cat "$REPORT_PATH")"
assert_contains "$REPORT_DEDICATED" "专用 token" "the report must explain dedicated-token CI visibility"
assert_contains "$REPORT_DEDICATED" "未验证" "the report must still say the PR is unverified"
assert_not_contains "$REPORT_DEDICATED" "已验证通过" "no run may claim the PR is verified"
pass "token type is surfaced so 'no checks' is never mistaken for approval"

# ---------------------------------------------------------------------------
# H. 待审 PR 列表必须被结构化校验：畸形/截断/达到分页上限一律 fail closed，
#    绝不能在解析失败时把「读不到」当成「没有待审 PR」而重复开票。
# ---------------------------------------------------------------------------
BRANCH_BEFORE="$(sync_branch_sha "$SYNC_BRANCH")"
PR_COUNT_BEFORE="$(wc -l < "$GH_STATE" | tr -d ' ')"
CREATE_BEFORE="$(grep -c '^pr create' "$GH_LOG" || true)"

upstream_commit 'feat: upstream for malformed list' 'app/list-malformed.txt' "malformed\n"
WORK_H="$(clone_fork "$FIXTURE_ROOT/work-malformed")"

for malformed in '[{}]' '[' '[]trailing' '{"number":1}' \
    '[{"number":"one","state":"OPEN","headRefName":"x","url":"u"}]' \
    '[{"number":1,"state":"open","headRefName":"chore/upstream-sync","url":"https://github.com/Wei-Shaw/sub2api/pull/1"}]' \
    '[{"number":1,"state":"OPEN","headRefName":"chore/upstream-sync","url":"https://github.com/lwying/sub2api/pull/2"}]' \
    '[{"number":1,"state":"MERGED","headRefName":"chore/upstream-sync","url":"https://github.com/lwying/sub2api/pull/1"}]'; do
    GH_STUB_LIST_RAW="$malformed"
    sync_run "$WORK_H"
    GH_STUB_LIST_RAW=""
    assert_eq 5 "$SYNC_STATUS" "malformed pr list '$malformed' must fail closed: $SYNC_ERR"
    assert_eq "blocked_tooling" "$(json_get "$SYNC_OUT" action)" \
        "malformed pr list '$malformed' must be reported as blocked_tooling"
done
assert_eq "$BRANCH_BEFORE" "$(sync_branch_sha "$SYNC_BRANCH")" "malformed lists must not push"
assert_eq "$PR_COUNT_BEFORE" "$(wc -l < "$GH_STATE" | tr -d ' ')" "malformed lists must not create a PR"
assert_eq "$CREATE_BEFORE" "$(grep -c '^pr create' "$GH_LOG" || true)" "malformed lists must not call pr create"
pass "malformed or truncated PR listings fail closed"

# 达到分页上限 -> 可能还有更多 PR，同样必须拒绝
GH_STUB_LIST_COUNT=20
sync_run "$WORK_H"
GH_STUB_LIST_COUNT=""
assert_eq 5 "$SYNC_STATUS" "a full page of PRs must fail closed: $SYNC_ERR"
assert_eq "blocked_tooling" "$(json_get "$SYNC_OUT" action)" "a full page must be reported as blocked_tooling"
assert_eq "$BRANCH_BEFORE" "$(sync_branch_sha "$SYNC_BRANCH")" "a full page must not push"
assert_eq "$PR_COUNT_BEFORE" "$(wc -l < "$GH_STATE" | tr -d ' ')" "a full page must not create a PR"
pass "a saturated PR listing is refused instead of assumed to be complete"

# 正例：形状合法的单条列表（state 小写也接受）必须继续被采信，
# 证明上面的 fail-closed 不是「一律拒绝」。
GH_STUB_LIST_RAW='[{"number":4,"state":"open","headRefName":"chore/upstream-sync","url":"https://github.com/lwying/sub2api/pull/4"}]'
sync_run "$WORK_H"
GH_STUB_LIST_RAW=""
assert_eq 0 "$SYNC_STATUS" "a well-formed PR listing must still be accepted: $SYNC_ERR"
assert_eq "4" "$(json_get "$SYNC_OUT" pending_pr.number)" "the parsed PR number must be used"
assert_eq "updated" "$(json_get "$SYNC_OUT" action)" "an existing pending PR must be updated, not duplicated"
pass "well-formed listings (including lowercase state) are still accepted"

printf 'PASS %s\n' "$(basename "$0")"
