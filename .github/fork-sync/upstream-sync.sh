#!/bin/bash
# fork 上游同步：每天最多维护一个待审 PR。
#
# 方向固定：upstream `Wei-Shaw/sub2api`（只读）→ fork `lwying/sub2api`（一个受控同步分支）。
#
# 本脚本刻意不做的事：
#   - 不合并 PR、不推送默认分支、不打 tag、不发布 Release、不部署；
#   - 不强推（`git push` 永远不带 --force/--force-with-lease/+refspec）；
#   - 不删除/关闭已有 PR 或分支；
#   - 不覆盖同步分支上的人工提交（检测到即阻塞并报告）。
#
# 输入全部来自环境变量（见下），stdout 只输出一行 JSON 判定结果，
# 人类可读报告写到 REPORT_PATH。退出码：
#   0 已完成（创建/更新/无需动作）  1 用法或环境错误
#   2 上游合并冲突（非受保护文件）    3 同步分支含人工提交
#   4 同步分支存在多个待审 PR         5 git/gh 调用失败
#   6 远端同步分支历史被改写（无法快进）
#
# 环境变量：
#   FORK_REPO/UPSTREAM_REPO  owner/repo，默认 lwying/sub2api 与 Wei-Shaw/sub2api
#   FORK_URL/PUSH_URL/UPSTREAM_URL  git URL（默认按上面推导成 https://github.com/<slug>.git）
#   DEFAULT_BRANCH/SYNC_BRANCH      默认 main / chore/upstream-sync
#   REPO_DIR                        工作树（默认当前目录）
#   BOT_NAME/BOT_EMAIL              机器人提交身份
#   GH_BIN                          gh 可执行文件（测试注入桩）
#   DRY_RUN=1                       只判定与报告，不推送、不调用 gh 写操作
#   REPORT_PATH                     报告输出路径（创建/更新 PR 时作为 PR 正文）
#   JSON_OUT/GITHUB_OUTPUT          额外输出落盘位置
#   FORK_SYNC_DISPOSABLE_CHECKOUT=1 声明 REPO_DIR 是一次性、可丢弃检出（必需）
#   FORK_SYNC_TOKEN_KIND            dedicated | github_token | unknown，用于如实说明 CI 可见性
#   PROTECTED_PATHS_FILE            受保护路径清单（默认 protected-paths.txt）
#   PROTECTED_PATHS                 冒号分隔的受保护路径，覆盖上面的文件
#   PR_LIST_LIMIT                   待审 PR 分页上限（默认 20，达到即视为可能被截断而拒绝）
#   MAX_REPORT_DIFF_LINES           报告中 diff 的最大行数（默认 40）
#
# 依赖：bash、git、python3 或 python（用于结构化校验 gh 输出；缺失时 fail closed）。
#
# 离线可测：tests/ 用本地 bare 仓库做 remote、用桩替换 gh，全程不访问网络。

set -euo pipefail

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

# ---------------------------------------------------------------------------
# 配置与参数
# ---------------------------------------------------------------------------

FORK_REPO="${FORK_REPO:-lwying/sub2api}"
UPSTREAM_REPO="${UPSTREAM_REPO:-Wei-Shaw/sub2api}"
DEFAULT_BRANCH="${DEFAULT_BRANCH:-main}"
SYNC_BRANCH="${SYNC_BRANCH:-chore/upstream-sync}"
REPO_DIR="${REPO_DIR:-$PWD}"
BOT_NAME="${BOT_NAME:-fork-sync bot}"
BOT_EMAIL="${BOT_EMAIL:-fork-sync-bot@users.noreply.github.com}"
GH_BIN="${GH_BIN:-gh}"
DRY_RUN="${DRY_RUN:-0}"
REPORT_PATH="${REPORT_PATH:-}"
JSON_OUT="${JSON_OUT:-}"
MAX_REPORT_DIFF_LINES="${MAX_REPORT_DIFF_LINES:-40}"
PR_LIST_LIMIT="${PR_LIST_LIMIT:-20}"
PYTHON_BIN=""

PROTECTED_PATHS_FILE="${PROTECTED_PATHS_FILE:-$SCRIPT_DIR/protected-paths.txt}"

FORK_URL="${FORK_URL:-https://github.com/${FORK_REPO}.git}"
PUSH_URL="${PUSH_URL:-$FORK_URL}"
UPSTREAM_URL="${UPSTREAM_URL:-https://github.com/${UPSTREAM_REPO}.git}"

BASE_REF="refs/remotes/fork-sync/base"
SYNC_REF="refs/remotes/fork-sync/sync"
UPSTREAM_REF="refs/remotes/fork-sync/upstream"

EXIT_USAGE=1
EXIT_CONFLICT=2
EXIT_HUMAN=3
EXIT_AMBIGUOUS_PR=4
EXIT_TOOLING=5
EXIT_REWRITE=6

while [ "$#" -gt 0 ]; do
    case "$1" in
        --dry-run) DRY_RUN=1; shift ;;
        --report) REPORT_PATH="${2:-}"; shift 2 ;;
        --json-out) JSON_OUT="${2:-}"; shift 2 ;;
        -h|--help)
            sed -n '2,25p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *) printf 'unknown argument: %s\n' "$1" >&2; exit "$EXIT_USAGE" ;;
    esac
done

# 永不等待交互式凭据输入；凭据由调用方通过 GIT_ASKPASS/credential helper 提供。
export GIT_TERMINAL_PROMPT=0

log() { printf '%s\n' "$*" >&2; }

# ---------------------------------------------------------------------------
# 输入校验（分支名、slug、URL 一律白名单；事件载荷绝不进入命令行）
# ---------------------------------------------------------------------------

die_usage() {
    log "usage error: $*"
    exit "$EXIT_USAGE"
}

validate_branch_name() {
    local name="$1" what="$2" compare_default="${3:-1}"
    local lower_name lower_default
    [ -n "$name" ] || die_usage "$what must not be empty"
    [ "${#name}" -le 100 ] || die_usage "$what is longer than 100 characters"
    case "$name" in
        -*) die_usage "$what must not start with '-'" ;;
        *[!A-Za-z0-9._/-]*) die_usage "$what contains characters outside [A-Za-z0-9._/-]: $name" ;;
        *..*) die_usage "$what must not contain '..'" ;;
        *'@{'*) die_usage "$what must not contain '@{'" ;;
        *//*) die_usage "$what must not contain '//'" ;;
        refs/*) die_usage "$what must be a branch name without the 'refs/' prefix: $name" ;;
        .*|*/.*) die_usage "$what has an invalid leading dot segment: $name" ;;
        */|*.lock|*.) die_usage "$what has an invalid trailing segment: $name" ;;
    esac
    lower_name="$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')"
    [ "$lower_name" != "head" ] || die_usage "$what must not be HEAD"
    # 大小写不敏感地拒绝与默认分支同名（大小写不敏感的文件系统上也安全）。
    if [ "$compare_default" = "1" ]; then
        lower_default="$(printf '%s' "$DEFAULT_BRANCH" | tr '[:upper:]' '[:lower:]')"
        [ "$lower_name" != "$lower_default" ] || die_usage "$what must differ from the default branch: $name"
    fi
}

validate_slug() {
    local slug="$1" what="$2"
    case "$slug" in
        *[!A-Za-z0-9._/-]*) die_usage "$what contains invalid characters: $slug" ;;
        */*/*) die_usage "$what must be 'owner/repo': $slug" ;;
        /*|*/) die_usage "$what must be 'owner/repo': $slug" ;;
        */*) ;;
        *) die_usage "$what must be 'owner/repo': $slug" ;;
    esac
}

validate_url() {
    local url="$1" what="$2"
    [ -n "$url" ] || die_usage "$what must not be empty"
    case "$url" in
        -*) die_usage "$what must not start with '-'" ;;
        *[!A-Za-z0-9._/@:+-]*) die_usage "$what contains unexpected characters: $url" ;;
    esac
}

validate_numeric() {
    local value="$1" what="$2"
    case "$value" in
        ""|*[!0-9]*) die_usage "$what must be a non-negative integer" ;;
    esac
}

validate_inputs() {
    validate_slug "$FORK_REPO" "FORK_REPO"
    validate_slug "$UPSTREAM_REPO" "UPSTREAM_REPO"
    validate_branch_name "$DEFAULT_BRANCH" "DEFAULT_BRANCH" 0
    validate_branch_name "$SYNC_BRANCH" "SYNC_BRANCH" 1
    validate_url "$FORK_URL" "FORK_URL"
    validate_url "$PUSH_URL" "PUSH_URL"
    validate_url "$UPSTREAM_URL" "UPSTREAM_URL"
    validate_numeric "$MAX_REPORT_DIFF_LINES" "MAX_REPORT_DIFF_LINES"
    [ -d "$REPO_DIR" ] || die_usage "REPO_DIR is not a directory: $REPO_DIR"
}

# ---------------------------------------------------------------------------
# 结果状态与 JSON 输出
# ---------------------------------------------------------------------------

ACTION="noop"
CI_STATE="unknown"
CI_DETAIL="workflow did not query check status"
TOKEN_KIND="${FORK_SYNC_TOKEN_KIND:-unknown}"
case "$TOKEN_KIND" in
    dedicated|github_token|unknown) ;;
    *) TOKEN_KIND="unknown" ;;
esac
BLOCKED_REASON=""
BLOCKED_PATHS=""
PROTECTED_CHANGED=""
PR_NUMBER=""
PR_URL=""
PR_ACTION="unchanged"
PR_NUMBERS=""
BASE_SHA=""
UPSTREAM_SHA=""
MERGE_BASE=""
OURS_REF=""
SYNC_PRESENT="false"
NEW_SYNC_SHA=""

json_escape() {
    printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' | tr -d '\000-\037'
}

# 读取以换行分隔的多值列表，输出 JSON 字符串数组。
json_string_list() {
    local raw="$1" result="" item
    while IFS= read -r item; do
        [ -n "$item" ] || continue
        result="${result}${result:+,}\"$(json_escape "$item")\""
    done <<< "$raw"
    printf '[%s]' "$result"
}

emit_json() {
    local doc
    doc=$(cat <<EOF
{
  "tool": "fork-sync/upstream-sync.sh",
  "action": "$(json_escape "$ACTION")",
  "dry_run": $( [ "$DRY_RUN" = "1" ] && printf 'true' || printf 'false' ),
  "fork_repo": "$(json_escape "$FORK_REPO")",
  "upstream_repo": "$(json_escape "$UPSTREAM_REPO")",
  "default_branch": "$(json_escape "$DEFAULT_BRANCH")",
  "sync_branch": "$(json_escape "$SYNC_BRANCH")",
  "base_sha": "$(json_escape "$BASE_SHA")",
  "upstream_sha": "$(json_escape "$UPSTREAM_SHA")",
  "merge_base": "$(json_escape "$MERGE_BASE")",
  "sync_branch_present": $SYNC_PRESENT,
  "sync_branch_sha": "$(json_escape "$NEW_SYNC_SHA")",
  "pending_pr": {
    "number": "${PR_NUMBER:-0}",
    "url": "$(json_escape "$PR_URL")",
    "action": "$(json_escape "$PR_ACTION")"
  },
  "ci": {
    "state": "$(json_escape "$CI_STATE")",
    "detail": "$(json_escape "$CI_DETAIL")"
  },
  "token_kind": "$(json_escape "$TOKEN_KIND")",
  "protected_paths_changed_upstream": $(json_string_list "$PROTECTED_CHANGED"),
  "blocked": {
    "reason": "$(json_escape "$BLOCKED_REASON")",
    "paths": $(json_string_list "$BLOCKED_PATHS")
  },
  "report_path": "$(json_escape "$REPORT_PATH")"
}
EOF
)
    printf '%s\n' "$doc"
    if [ -n "$JSON_OUT" ]; then
        printf '%s\n' "$doc" > "$JSON_OUT"
    fi
    if [ -n "${GITHUB_OUTPUT:-}" ]; then
        {
            printf 'action=%s\n' "$ACTION"
            printf 'sync_branch=%s\n' "$SYNC_BRANCH"
            printf 'pr_number=%s\n' "${PR_NUMBER:-0}"
            printf 'ci_state=%s\n' "$CI_STATE"
        } >> "$GITHUB_OUTPUT"
    fi
}

# ---------------------------------------------------------------------------
# 报告
# ---------------------------------------------------------------------------

REPORT_BODY=""

report_append() {
    REPORT_BODY="${REPORT_BODY}$1
"
}

write_report_if_requested() {
    [ -n "$REPORT_PATH" ] || return 0
    printf '%s' "$REPORT_BODY" > "$REPORT_PATH"
}

report_header() {
    report_append "# fork 上游同步（每日，最多一个待审 PR）"
    report_append ""
    report_append "- 上游（只读）：\`$UPSTREAM_REPO\`"
    report_append "- fork：\`$FORK_REPO\`"
    report_append "- 同步分支：\`$SYNC_BRANCH\`"
    report_append "- fork 默认分支：\`${BASE_SHA:-unknown}\`"
    report_append "- 上游默认分支：\`${UPSTREAM_SHA:-unknown}\`"
    report_append ""
}

report_upstream_changes() {
    local range="${MERGE_BASE:-$BASE_SHA}..$UPSTREAM_REF" stat
    stat="$(gitq diff --stat "$range" 2>/dev/null || true)"
    report_append "## 本次同步的上游变更"
    report_append ""
    if [ -n "$stat" ]; then
        report_append '```'
        report_append "$(printf '%s\n' "$stat" | awk -v n="$MAX_REPORT_DIFF_LINES" 'NR <= n')"
        report_append '```'
    else
        report_append "（无文件级差异）"
    fi
    report_append ""
}

report_protected_files() {
    local -a paths=()
    local p
    while IFS= read -r p; do
        [ -n "$p" ] || continue
        paths+=("$p")
    done <<< "$PROTECTED_CHANGED"

    report_append "## 受保护文件：上游改动没有被自动应用"
    report_append ""
    if [ "${#paths[@]}" -eq 0 ]; then
        report_append "本次上游改动没有触及 fork 自有路径。"
        report_append ""
        return 0
    fi
    report_append "以下路径属于 fork 自有（版本号、发布配置、更新器、安装器所有权）。"
    report_append "上游对它们的改动**没有**写入同步分支：同步分支保持 fork 的内容，"
    report_append "是否移植由维护者人工决定。"
    report_append ""
    local upstream_value fork_value
    for p in "${paths[@]}"; do
        report_append "- \`$p\`"
        case "$p" in
            *VERSION)
                upstream_value="$(first_line_at_ref "$UPSTREAM_REF" "$p" | tr -d '\r')"
                fork_value="$(first_line_at_ref "$OURS_REF" "$p" | tr -d '\r')"
                report_append "  - 上游版本号：\`$fork_value\` → \`$upstream_value\`；fork 保持 **\`$fork_value\`**，不采用上游版本号。"
                ;;
        esac
    done
    report_append ""
    report_append "上游对这些文件的原始改动（供维护者审查/移植）："
    report_append ""
    report_append '```diff'
    report_append "$(gitq diff "$MERGE_BASE" "$UPSTREAM_REF" -- "${paths[@]}" 2>/dev/null \
        | awk -v n="$MAX_REPORT_DIFF_LINES" 'NR <= n')"
    report_append '```'
    report_append ""
    report_append "未经维护者确认，这些上游改动不会成为 fork 的版本号或发布目标，也不会把 fork 的更新来源改回上游。"
    report_append ""
}

report_verification() {
    report_append "## 验证状态：未验证（等待维护者）"
    report_append ""
    if [ "$CI_STATE" = "passed" ]; then
        report_append "- 已上报的 CI 检查全部成功（$CI_DETAIL）。"
        report_append "- **这仍然不等于已验证**：必须由维护者人工审查后才能合并；合并本身不触发任何发布。"
    else
        report_append "- CI 状态：\`$CI_STATE\`（$CI_DETAIL）。"
        report_append "- 检查可能尚未运行，或等待维护者批准执行；在维护者批准并确认之前，本 PR **不得**被视为已通过验证，工作流也不会声称它是绿的。"
    fi
    case "$TOKEN_KIND" in
        github_token)
            report_append "- 本次推送/建 PR 用的是默认 \`GITHUB_TOKEN\`：GitHub **不会**因此触发该 PR 的 \`push\`/\`pull_request\` workflow，所以「没有检查」很可能意味着 CI 根本没有启动，而不是在等待批准。需要 CI 时必须由维护者手工触发/批准，或配置专用 token \`FORK_SYNC_TOKEN\` 后重跑。"
            ;;
        dedicated)
            report_append "- 本次推送/建 PR 用的是专用 token（\`FORK_SYNC_TOKEN\`）：PR 事件可以触发 CI，检查可能处于等待维护者批准的状态。"
            ;;
        *)
            report_append "- 未能确认本次推送使用的 token 类型（\`FORK_SYNC_TOKEN_KIND\` 未设置）：不要把「没有检查」当成已批准或已通过。"
            ;;
    esac
    report_append "- 合并本 PR **不会**自动打 tag、发布 Release、发布镜像或部署；发布是维护者的单独动作。"
    report_append ""
}

# ---------------------------------------------------------------------------
# git 操作
# ---------------------------------------------------------------------------

# 所有 git 调用都关闭换行转换：同步工具只关心与 git 对象逐字节一致的树，
# 否则在 core.autocrlf=true 的环境（例如 Windows runner）会凭空出现
# “local changes would be overwritten” 之类的伪冲突。
gitq() { git -C "$REPO_DIR" -c core.autocrlf=false -c core.eol=lf "$@"; }

require_git_repo() {
    gitq rev-parse --is-inside-work-tree >/dev/null 2>&1 \
        || { log "REPO_DIR is not a git work tree: $REPO_DIR"; exit "$EXIT_TOOLING"; }
}

# 工具会在 REPO_DIR 里执行 checkout -B / merge / reset --hard，所以必须显式声明
# 「这是一个可丢弃的一次性检出」（CI 的 runner checkout 就是这样）。
# 未声明时 fail closed，避免在维护者的真实工作副本里动历史。
require_disposable_checkout() {
    case "${FORK_SYNC_DISPOSABLE_CHECKOUT:-}" in
        1|true|yes) return 0 ;;
    esac
    block "blocked_no_disposable_checkout" \
        "REPO_DIR 必须是一次性（可丢弃）检出：本工具会在其中切换分支并 merge/reset --hard。请设置 FORK_SYNC_DISPOSABLE_CHECKOUT=1" ""
    exit "$EXIT_USAGE"
}

# 任何 fetch/checkout 之前先要求工作树干净：脏工作树（含未跟踪文件）一律拒绝运行，
# 绝不在上面执行 reset --hard / checkout --force。
require_clean_checkout() {
    local status
    if ! status="$(gitq status --porcelain 2>/dev/null)"; then
        block "blocked_tooling" "无法读取 REPO_DIR 的工作树状态" ""
        exit "$EXIT_TOOLING"
    fi
    if [ -n "$status" ]; then
        block "blocked_dirty_worktree" \
            "REPO_DIR 有未提交改动（含未跟踪文件），拒绝运行以免破坏本地内容；请改用干净的一次性检出" ""
        exit "$EXIT_USAGE"
    fi
}

remote_sha_for() {
    local heads="$1" ref="$2"
    printf '%s\n' "$heads" | awk -v want="$ref" '$2 == want { print $1; exit }'
}

fetch_ref() {
    local url="$1" src="$2" dst="$3" force="$4"
    local refspec="${src}:${dst}"
    [ "$force" = "1" ] && refspec="+${refspec}"
    gitq fetch --no-tags --quiet "$url" "$refspec" >/dev/null 2>&1
}

current_branch() {
    gitq symbolic-ref --quiet --short HEAD 2>/dev/null || printf ''
}

# ---------------------------------------------------------------------------
# 受保护路径（fork 自有：版本号、发布配置、更新器、安装器）
# ---------------------------------------------------------------------------

PROTECTED_LIST=""
RESTORED_PROTECTED=""

load_protected_paths() {
    local raw p
    if [ -n "${PROTECTED_PATHS:-}" ]; then
        raw="$(printf '%s' "$PROTECTED_PATHS" | tr ':' '\n')"
    elif [ -f "$PROTECTED_PATHS_FILE" ]; then
        raw="$(grep -v -E '^[[:space:]]*(#|$)' "$PROTECTED_PATHS_FILE" | tr -d '\r')"
    else
        die_usage "protected path list not found: $PROTECTED_PATHS_FILE"
    fi
    PROTECTED_LIST=""
    while IFS= read -r p; do
        p="$(printf '%s' "$p" | sed -e 's/[[:space:]]*$//')"
        [ -n "$p" ] || continue
        case "$p" in
            -*) die_usage "protected path must not start with '-': $p" ;;
            *..*) die_usage "protected path must not contain '..': $p" ;;
            *[!A-Za-z0-9._/-]*) die_usage "protected path has unexpected characters: $p" ;;
        esac
        PROTECTED_LIST="${PROTECTED_LIST}${p}"$'\n'
    done <<< "$raw"
    [ -n "$PROTECTED_LIST" ] || die_usage "protected path list is empty: $PROTECTED_PATHS_FILE"
}

is_protected_path() {
    local path="$1" entry
    while IFS= read -r entry; do
        [ -n "$entry" ] || continue
        case "$entry" in
            */) case "$path" in "${entry}"*) return 0 ;; esac ;;
            *) [ "$path" = "$entry" ] && return 0 ;;
        esac
    done <<< "$PROTECTED_LIST"
    return 1
}

filter_protected() {
    local p
    while IFS= read -r p; do
        [ -n "$p" ] || continue
        if is_protected_path "$p"; then printf '%s\n' "$p"; fi
    done
}

filter_unprotected() {
    local p
    while IFS= read -r p; do
        [ -n "$p" ] || continue
        if ! is_protected_path "$p"; then printf '%s\n' "$p"; fi
    done
}

# 把受保护路径还原成「我们这一侧」的内容：冲突（stage>0）与干净合并都覆盖。
# 注意 `git checkout <ours> -- <path>` 是**叠加**式的：它不会删除上游新增进来的文件，
# 对受保护目录（如 backend/internal/releasecontract/）会留下残留。所以先删掉
# 「ours 里没有、但合并带进来」的路径，再恢复 ours 的内容。
# 上游对受保护内容的改动因此不会静默进入同步分支，也不会因为版本号冲突而卡死同步。
restore_protected_paths() {
    local ours="$1" p restored="" status file
    while IFS= read -r p; do
        [ -n "$p" ] || continue
        if ! gitq ls-files -u -- "$p" 2>/dev/null | grep -q .; then
            if gitq diff --quiet "$ours" -- "$p" >/dev/null 2>&1; then
                continue
            fi
        fi
        while IFS=$'\t' read -r status file; do
            [ -n "$file" ] || continue
            case "$status" in
                A*) gitq rm -q -f --ignore-unmatch -- "$file" >/dev/null 2>&1 || true ;;
            esac
        done < <(gitq diff --name-status --no-renames "$ours" -- "$p" 2>/dev/null || true)
        if ref_path_exists "$ours" "$p"; then
            gitq checkout "$ours" -- "$p"
        else
            gitq rm -q -f --ignore-unmatch -- "$p" >/dev/null 2>&1 || true
        fi
        restored="${restored}${p}"$'\n'
    done <<< "$PROTECTED_LIST"
    RESTORED_PROTECTED="$restored"
}

# ---------------------------------------------------------------------------
# 合并、冲突分类与人工提交检测
# ---------------------------------------------------------------------------

collect_conflict_paths() {
    gitq diff --name-only --diff-filter=U | sort -u
}

abort_merge_if_any() {
    if gitq rev-parse --verify --quiet MERGE_HEAD >/dev/null; then
        gitq merge --abort >/dev/null 2>&1 || true
    fi
}

write_merge_message() {
    local path="$1"
    {
        printf 'chore(fork-sync): merge %s/%s into %s\n\n' \
            "$UPSTREAM_REPO" "$DEFAULT_BRANCH" "$SYNC_BRANCH"
        printf 'Automated upstream sync. Maintainer review is required before merge;\n'
        printf 'this commit does not tag, release, publish or deploy anything.\n\n'
        # Base 记录合并当时的 fork 默认分支（仅供诊断：它**不**保证是下一次 parent1 的祖先，
        # 因为维护者可能已经合并过这个 PR、把默认分支推到了分支尖端之后）。
        # MergeBase 是「本次合并实际用到的共同祖先」，它是本次提交的祖先，因此可以用来
        # 把标记与真实内容绑定，而不会在默认分支前进后误判。
        printf 'Fork-Sync-Upstream: %s\n' "$UPSTREAM_SHA"
        printf 'Fork-Sync-Base: %s\n' "$BASE_SHA"
        printf 'Fork-Sync-MergeBase: %s\n' "${MERGE_BASE:-$BASE_SHA}"
        printf 'Fork-Sync-Tool: %s\n' "$TOOL_PATH"
    } > "$path"
}

commit_merge() {
    local message_file="$1"
    GIT_AUTHOR_NAME="$BOT_NAME" GIT_AUTHOR_EMAIL="$BOT_EMAIL" \
    GIT_COMMITTER_NAME="$BOT_NAME" GIT_COMMITTER_EMAIL="$BOT_EMAIL" \
        gitq commit -q -F "$message_file"
}

TOOL_PATH=".github/fork-sync/upstream-sync.sh"

# 自动化提交的「形状」校验：这是本工具唯一能离线验证、且与真实内容绑定的凭据。
# 只按作者邮箱或消息里的 trailer 判断会被简单伪造绕过，所以这里要求：
#   1) 头部是 merge 提交：恰好两个 parent；
#   2) 作者与提交者都等于本工具声明的机器人身份；
#   3) 标题逐字节等于本工具生成的格式；
#   4) 三个 fork-sync trailer 齐全，Upstream/Base 是 40 位十六进制 sha；
#   5) 第二个 parent 恰好就是 Upstream 标记指向的提交（把标记与真实内容绑起来）。
# 任何一条不符 -> 视为人工/外来提交，一律阻塞。
#
# 诚实的边界：git 的元数据（作者、消息、parent 列表）本身都可以被刻意伪造。
# 这里做的是「形状 + 与内容绑定」的校验，不是密码学证明；无法防住完全模仿以上形状
# 的冒充者，也正因如此，任何不确定的情况都选择阻塞并交给维护者。
automation_commit_shape_ok() {
    local ref="$1" parents author committer subject upstream_sha base_sha tool_line parent1 parent2
    [ -n "$ref" ] || return 1

    parents="$(gitq log -1 --format=%P "$ref" 2>/dev/null || true)"
    [ -n "$parents" ] || return 1
    # 只有两个 parent 才是本工具产生的 merge 形状
    case "$parents" in
        *" "*) ;;
        *) return 1 ;;
    esac
    case "$parents" in
        *" "*" "*) return 1 ;;   # 三个及以上 parent
    esac
    parent1="${parents%% *}"
    parent2="${parents##* }"

    [ "$(gitq log -1 --format=%an "$ref" 2>/dev/null || true)" = "$BOT_NAME" ] || return 1
    [ "$(gitq log -1 --format=%ae "$ref" 2>/dev/null || true)" = "$BOT_EMAIL" ] || return 1
    [ "$(gitq log -1 --format=%cn "$ref" 2>/dev/null || true)" = "$BOT_NAME" ] || return 1
    [ "$(gitq log -1 --format=%ce "$ref" 2>/dev/null || true)" = "$BOT_EMAIL" ] || return 1

    subject="$(gitq log -1 --format=%s "$ref" 2>/dev/null || true)"
    [ "$subject" = "chore(fork-sync): merge ${UPSTREAM_REPO}/${DEFAULT_BRANCH} into ${SYNC_BRANCH}" ] || return 1

    upstream_sha="$(gitq log -1 --format=%B "$ref" 2>/dev/null | awk '/^Fork-Sync-Upstream: /{ print $2; exit }')"
    base_sha="$(gitq log -1 --format=%B "$ref" 2>/dev/null | awk '/^Fork-Sync-Base: /{ print $2; exit }')"
    merge_base_sha="$(gitq log -1 --format=%B "$ref" 2>/dev/null | awk '/^Fork-Sync-MergeBase: /{ print $2; exit }')"
    tool_line="$(gitq log -1 --format=%B "$ref" 2>/dev/null | awk '/^Fork-Sync-Tool: /{ print $2; exit }')"
    for sha in "$upstream_sha" "$base_sha" "$merge_base_sha"; do
        [ "${#sha}" -eq 40 ] || return 1
        case "$sha" in *[!0-9a-f]*) return 1 ;; esac
    done
    [ "$tool_line" = "$TOOL_PATH" ] || return 1

    # 标记必须与真实内容绑定：第二个 parent 就是被合并进来的那个上游提交
    [ "$parent2" = "$upstream_sha" ] || return 1
    # 第一个 parent 必须真实存在于本地
    gitq cat-file -e "${parent1}^{commit}" >/dev/null 2>&1 || return 1
    # Base 只是诊断信息：既要求它是 parent1 的祖先（默认分支前进后被合并 PR 打破），
    # 也不要求对象仍然可达（默认分支若被强行改写，旧的 base 可能已不可达）。
    # 真正把标记与内容绑定的是上面「parent2 == 上游提交」与下面「MergeBase 是本提交祖先」。
    # MergeBase 才是可验证的绑定：它是本次合并实际用到的共同祖先，必为本次提交的祖先。
    gitq merge-base --is-ancestor "$merge_base_sha" "$ref" >/dev/null 2>&1 || return 1
    return 0
}

# 读取 "ref:path" 的内容。对象名经 stdin 传给 git cat-file --batch：
# 若把它当命令行参数，Windows/MSYS 会把 "refs/heads/x:dir/file" 这类参数
# 误当成路径列表改写（反斜杠 + 分号），导致本地 Windows 运行读到空内容。
blob_at_ref() {
    local ref="$1" path="$2"
    printf '%s:%s\n' "$ref" "$path" | gitq cat-file --batch 2>/dev/null || true
}

ref_path_exists() {
    local header
    header="$(blob_at_ref "$1" "$2" | awk 'NR==1')"
    case "$header" in
        ""|*missing*) return 1 ;;
        *) return 0 ;;
    esac
}

first_line_at_ref() {
    blob_at_ref "$1" "$2" | awk 'NR==2 { print; exit }'
}

sync_marker_upstream() {
    gitq log -1 --format=%B "$1" 2>/dev/null \
        | awk '/^Fork-Sync-Upstream: /{ print $2; exit }'
}

# 同步分支上是否存在「不是本工具生成的」提交。
# 只看同步分支相对 fork 默认分支与上游多出来的提交（--not UPSTREAM_REF），
# 否则上游自己的提交会被误判；然后逐个套用「自动化提交形状」校验——本工具只产生
# 这种形状的 merge 提交，所以任何形状不符的提交（包括冒用机器人身份的）都算外来提交，
# 一律阻塞，绝不在其基础上继续自动合并。
foreign_commits_on_sync_branch() {
    local ref="$1" merge_base sha result=""
    merge_base="$(gitq merge-base "$BASE_REF" "$ref" 2>/dev/null || true)"
    [ -n "$merge_base" ] || return 0
    while IFS= read -r sha; do
        [ -n "$sha" ] || continue
        if automation_commit_shape_ok "$sha"; then
            continue
        fi
        result="${result}${sha}"$'\n'
    done < <(gitq rev-list "${merge_base}..${ref}" --not "$UPSTREAM_REF" 2>/dev/null || true)
    printf '%s' "$result"
}

# ---------------------------------------------------------------------------
# gh：只读列表 + 创建/更新 PR
# ---------------------------------------------------------------------------

gh_pr_list_open() {
    # 故意不吞掉失败：读不到待审 PR 列表时，绝不能假设「没有待审 PR」而重复开票。
    "$GH_BIN" pr list --repo "$FORK_REPO" --head "$SYNC_BRANCH" --state open \
        --limit "$PR_LIST_LIMIT" --json number,state,headRefName,url 2>/dev/null
}

# 结构化校验 gh 的 PR 列表输出：必须是完整、字段齐全的 JSON 数组，并且没有达到
# 分页上限（达到上限说明可能还有更多 PR，不能当成「就这些」）。
# 逐行输出 PR 编号；任何不确定的情况都返回非零，由调用方 fail closed——
# 绝不能用 grep/tr 之类的文本解析，否则畸形或截断的响应会被读成「没有待审 PR」，
# 进而重复开票。
parse_pr_list() {
    local json_file="$1"
    [ -n "$PYTHON_BIN" ] || return 2
    "$PYTHON_BIN" - "$json_file" "$PR_LIST_LIMIT" "$SYNC_BRANCH" "$FORK_REPO" <<'PY'
import json
import sys

path, limit, expected_head, fork_repo = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4]

try:
    with open(path, encoding="utf-8") as handle:
        payload = json.load(handle)
except Exception as exc:
    print(f"pr list is not valid JSON: {exc}", file=sys.stderr)
    sys.exit(1)

if not isinstance(payload, list):
    print("pr list is not a JSON array", file=sys.stderr)
    sys.exit(1)
if len(payload) >= limit:
    print(
        f"pr list returned {len(payload)} entries, which is the page limit ({limit}); "
        "it may be truncated",
        file=sys.stderr,
    )
    sys.exit(1)

for entry in payload:
    if not isinstance(entry, dict):
        print("pr list entry is not an object", file=sys.stderr)
        sys.exit(1)
    number = entry.get("number")
    if not isinstance(number, int) or isinstance(number, bool):
        print("pr list entry has no integer 'number'", file=sys.stderr)
        sys.exit(1)
    for key in ("headRefName", "state", "url"):
        value = entry.get(key)
        if not isinstance(value, str) or not value:
            print(f"pr list entry has no '{key}'", file=sys.stderr)
            sys.exit(1)
    head = entry["headRefName"]
    if head != expected_head:
        print(f"pr list entry head is {head!r}, expected {expected_head!r}", file=sys.stderr)
        sys.exit(1)
    # gh 的 --json state 取值为大写的 OPEN/CLOSED/MERGED；这里大小写不敏感地要求 OPEN，
    # 因为调用方本来就带了 --state open。
    state = entry["state"]
    if state.upper() != "OPEN":
        print(f"pr list entry state is {state!r}, expected OPEN", file=sys.stderr)
        sys.exit(1)
    # URL 必须是本 fork 上、编号一致的那个 PR 的地址；不能是任意字符串，
    # 否则「拿上游 PR 或别的仓库的 PR 冒充」就看不出来了。
    url = entry["url"]
    expected_url = f"https://github.com/{fork_repo}/pull/{number}"
    if url.rstrip("/").lower() != expected_url.lower():
        print(f"pr list entry url is {url!r}, expected {expected_url!r}", file=sys.stderr)
        sys.exit(1)
    print(number)
PY
}

detect_json_tool() {
    if command -v python3 >/dev/null 2>&1; then
        PYTHON_BIN="python3"
    elif command -v python >/dev/null 2>&1; then
        PYTHON_BIN="python"
    else
        PYTHON_BIN=""
    fi
}

# `gh pr create` 的退出码为 0 并不等于拿到的是本 fork 的 PR。
# 必须校验输出确实是 https://github.com/<FORK_REPO>/pull/<number>，否则返回非零：
# 调用方绝不能凭一个解析不出来的输出宣称「已创建」。
gh_pr_number_from_url() {
    local url="$1"
    [ -n "$PYTHON_BIN" ] || return 2
    "$PYTHON_BIN" - "$url" "$FORK_REPO" <<'PY'
import re
import sys

url, fork_repo = sys.argv[1].strip(), sys.argv[2]
pattern = rf"https://github\.com/{re.escape(fork_repo)}/pull/([0-9]+)/?"
match = re.fullmatch(pattern, url, re.IGNORECASE)
if not match:
    print(f"gh pr create output is not a {fork_repo} pull request URL: {url!r}", file=sys.stderr)
    sys.exit(1)
print(match.group(1))
PY
}

# 读取并结构化校验待审 PR 列表，结果放在 PR_NUMBERS（每行一个编号）。
# 失败时返回非零，由调用方 fail closed——绝不能把「读不到」当成「没有待审 PR」。
fetch_pending_pr_numbers() {
    local pr_list_file
    PR_NUMBERS=""
    pr_list_file="$(mktemp "${TMPDIR:-/tmp}/fork-sync-pr-list.XXXXXX")"
    if ! gh_pr_list_open > "$pr_list_file"; then
        rm -f "$pr_list_file"
        return 1
    fi
    if ! PR_NUMBERS="$(parse_pr_list "$pr_list_file")"; then
        rm -f "$pr_list_file"
        return 1
    fi
    rm -f "$pr_list_file"
    return 0
}

# 同步分支相对 fork 默认分支是否已经「没有可提议的内容」：
#   - 分支尖端已在默认分支里（PR 以 merge 方式合并后保留分支）；或
#   - 分支上的提交在默认分支上都能找到等价补丁（squash 合并）。
# 用来避免为一个已合并的分支开一个没有任何提交差异的空 PR。
nothing_to_propose() {
    local ref="$1" cherry_out plus_shas nonmerge_plus=0 sha
    if gitq merge-base --is-ancestor "$ref" "$BASE_REF" >/dev/null 2>&1; then
        return 0
    fi
    if ! cherry_out="$(gitq cherry "$BASE_REF" "$ref" 2>/dev/null)"; then
        # 判断不了就不要静默 noop：当作还有内容，走正常的补建/阻塞路径。
        return 1
    fi
    plus_shas="$(printf '%s\n' "$cherry_out" | awk '$1 == "+" { print $2 }')"
    [ -n "$plus_shas" ] || return 0
    # 机器人的 merge 提交自身没有独立补丁（在 cherry 里也会显示为 +），
    # 只有「非 merge 且找不到等价补丁」的提交才算真的有内容可提议。
    while IFS= read -r sha; do
        [ -n "$sha" ] || continue
        if ! gitq rev-list --parents -n 1 "$sha" 2>/dev/null | grep -q ' .* '; then
            nonmerge_plus=$((nonmerge_plus + 1))
        fi
    done <<< "$plus_shas"
    [ "$nonmerge_plus" -eq 0 ]
}

# 为当前同步分支新建 PR（调用前必须先写好 REPORT_PATH 正文）。
# 失败时走 pr_step_failed：分支可能已经推送成功，这一点必须如实报告。
create_pending_pr() {
    local create_out
    if ! create_out="$("$GH_BIN" pr create --repo "$FORK_REPO" --base "$DEFAULT_BRANCH" \
        --head "$SYNC_BRANCH" \
        --title "chore(fork-sync): sync upstream ${UPSTREAM_REPO}" \
        --body-file "$REPORT_PATH")"; then
        pr_step_failed "创建 PR 失败"
    fi
    if ! PR_NUMBER="$(gh_pr_number_from_url "$create_out")"; then
        log "gh pr create returned an unusable output: $create_out"
        pr_step_failed "PR 创建结果无法校验（gh 返回成功，但输出不是本 fork 的 PR URL）"
    fi
    # 只输出经过校验的规范化 URL，绝不把未校验的外部字符串当成事实。
    PR_URL="https://github.com/${FORK_REPO}/pull/${PR_NUMBER}"
    PR_ACTION="created"
}

query_ci_state() {
    local pr="$1" states trimmed bad
    CI_STATE="unknown"
    CI_DETAIL="no pull request to query"
    [ -n "$pr" ] || return 0
    # gh pr checks 在「有检查但未完成/未通过」时会以非零码退出（例如 8=待定、1=失败），
    # 但**仍会把状态打印到 stdout**。丢掉这些输出会把「失败/待定」误报成「没有检查上报」，
    # 所以这里保留 stdout，只用「输出是否为空」区分「真的没有检查」。
    states="$("$GH_BIN" pr checks "$pr" --repo "$FORK_REPO" --json name,state \
        --jq '.[] | .state' 2>/dev/null || true)"
    trimmed="$(printf '%s' "$states" | tr -d '[:space:]')"
    if [ -z "$trimmed" ]; then
        CI_STATE="unverified"
        CI_DETAIL="no checks reported yet; CI may await maintainer approval"
        return 0
    fi
    bad="$(printf '%s\n' "$states" | grep -v -E '^(SUCCESS|NEUTRAL|SKIPPED)$' | tr '\n' ' ' || true)"
    if [ -n "$bad" ]; then
        CI_STATE="unverified"
        CI_DETAIL="checks not successful: $bad"
    else
        CI_STATE="passed"
        CI_DETAIL="all reported checks completed successfully"
    fi
}

# ---------------------------------------------------------------------------
# 阻塞出口
# ---------------------------------------------------------------------------

block() {
    local action="$1" reason="$2" paths="$3"
    ACTION="$action"
    BLOCKED_REASON="$reason"
    BLOCKED_PATHS="$paths"
    report_append "## 阻塞：$reason"
    report_append ""
    local item
    while IFS= read -r item; do
        [ -n "$item" ] || continue
        report_append "- \`$item\`"
    done <<< "$paths"
    [ -n "$paths" ] && report_append ""
    report_append "同步分支 \`$SYNC_BRANCH\` 与已有 PR 保持原状。本次运行**没有**推送、没有强推、没有关闭或删除任何分支或 PR；维护者处理完毕后，下一次定时任务会继续。"
    report_append ""
    report_verification
    write_report_if_requested
    emit_json
}

# PR 步骤失败：分支已经推上去了，所以不能说「什么都没做」。
# 不删分支、不另建 PR、不强推；报告事实并让维护者接手或等下一次重试。
pr_step_failed() {
    local reason="$1"
    ACTION="blocked_pr_step_failed"
    BLOCKED_REASON="$reason"
    report_append "## 阻塞：$reason"
    report_append ""
    report_append "同步分支 \`$SYNC_BRANCH\` 已推送成功（分支内容本身是正确的），但 PR 步骤失败。"
    report_append "本工具不会强推、不会删除分支、也不会另建 PR：请维护者手工创建/更新该 PR，或等下一次定时任务重试。"
    report_append ""
    report_append "**无法确认 PR 是否真的创建/更新成功**：请到仓库确认后再手工补建（下一次定时任务也会在发现「有分支、无待审 PR」时重试）。"
    report_append ""
    report_verification
    write_report_if_requested
    emit_json
    exit "$EXIT_TOOLING"
}

restore_start_state() {
    local branch="$1" sha="$2"
    abort_merge_if_any
    if [ -n "$branch" ]; then
        gitq checkout -q --force "$branch"
        gitq reset -q --hard "$sha"
    else
        gitq checkout -q --force --detach "$sha"
    fi
}

# ---------------------------------------------------------------------------
# 主流程
# ---------------------------------------------------------------------------

main() {
    validate_inputs
    load_protected_paths
    # gh 输出必须用真正的 JSON 解析器校验；没有解析器就停手（不能退化成文本猜测）。
    detect_json_tool
    if [ -z "$PYTHON_BIN" ]; then
        block "blocked_tooling" \
            "找不到 python 解析器（需要 python3 或 python）来结构化校验 gh 输出；已停止，未做任何改动" ""
        exit "$EXIT_TOOLING"
    fi
    require_git_repo
    # 这两道门禁必须在任何 fetch/checkout/merge 之前。
    require_disposable_checkout
    require_clean_checkout

    # 1. 远端引用状态（先 ls-remote，避免依赖 fetch 的错误文案）
    local heads
    heads="$(gitq ls-remote --heads "$FORK_URL")" \
        || { log "cannot list refs on FORK_URL"; exit "$EXIT_TOOLING"; }

    local base_remote sync_remote
    base_remote="$(remote_sha_for "$heads" "refs/heads/$DEFAULT_BRANCH")"
    sync_remote="$(remote_sha_for "$heads" "refs/heads/$SYNC_BRANCH")"
    [ -n "$base_remote" ] || { log "fork default branch not found: $DEFAULT_BRANCH"; exit "$EXIT_TOOLING"; }

    fetch_ref "$FORK_URL" "refs/heads/$DEFAULT_BRANCH" "$BASE_REF" 1 \
        || { log "cannot fetch fork default branch"; exit "$EXIT_TOOLING"; }
    BASE_SHA="$(gitq rev-parse "$BASE_REF")"

    fetch_ref "$UPSTREAM_URL" "refs/heads/$DEFAULT_BRANCH" "$UPSTREAM_REF" 1 \
        || { log "cannot fetch upstream default branch"; exit "$EXIT_TOOLING"; }
    UPSTREAM_SHA="$(gitq rev-parse "$UPSTREAM_REF")"

    report_header
    log "fork base:     $BASE_SHA"
    log "upstream head: $UPSTREAM_SHA"

    # 2. 同步分支：存在则读取远端状态；历史被改写（无法快进）时阻塞
    if [ -n "$sync_remote" ]; then
        SYNC_PRESENT="true"
        if ! fetch_ref "$FORK_URL" "refs/heads/$SYNC_BRANCH" "$SYNC_REF" 0; then
            block "blocked_remote_rewrite" \
                "远端 $SYNC_BRANCH 无法快进到本地记录（历史可能被强制改写）" ""
            exit "$EXIT_REWRITE"
        fi
    fi

    # 3. 人工提交检测（在动任何东西之前）
    if [ "$SYNC_PRESENT" = "true" ]; then
        local marker human
        # 头部必须是本工具生成的自动化提交形状：只按作者邮箱/trailer 放行会被伪造绕过。
        if ! automation_commit_shape_ok "$SYNC_REF"; then
            human="$(foreign_commits_on_sync_branch "$SYNC_REF")"
            [ -n "$human" ] || human="$(gitq rev-parse "$SYNC_REF")"
            block "blocked_human_changes" \
                "同步分支头部不是本工具生成的自动化合并提交（形状/身份/标记与内容不符），已停止；不覆盖、不强推。若这是旧版本工具或人工改写过的分支，请维护者合并/关闭对应 PR 后删除该分支，下一次运行会从默认分支重建" "$human"
            exit "$EXIT_HUMAN"
        fi
        human="$(foreign_commits_on_sync_branch "$SYNC_REF")"
        if [ -n "$human" ]; then
            block "blocked_human_changes" \
                "同步分支包含非自动化提交，已停止以免覆盖人工修复" "$human"
            exit "$EXIT_HUMAN"
        fi
        marker="$(sync_marker_upstream "$SYNC_REF")"
        log "sync branch records upstream: $marker"
    fi

    # 4. 是否真的需要合并
    local needs_sync="false"
    if [ "$SYNC_PRESENT" = "true" ]; then
        [ "$(sync_marker_upstream "$SYNC_REF")" != "$UPSTREAM_SHA" ] && needs_sync="true"
    else
        [ "$UPSTREAM_SHA" != "$BASE_SHA" ] && needs_sync="true"
    fi

    if [ "$needs_sync" != "true" ]; then
        # 没有新提交。但如果同步分支已存在，还必须确认它确实有一个待审 PR：
        # 上一次可能「推送成功、PR 创建失败」，直接 noop 会永久留下一个没有 PR 的分支。
        if [ "$SYNC_PRESENT" != "true" ]; then
            ACTION="noop"
            report_append "## 动作：noop（没有需要同步的提交）"
            report_append ""
            report_append "上游没有新的提交，也没有同步分支，因此不新建分支、不重复开 PR。"
            report_append ""
            report_verification
            write_report_if_requested
            emit_json
            return 0
        fi

        local pending_count
        if ! fetch_pending_pr_numbers; then
            block "blocked_tooling" \
                "无法读取或结构化校验待审 PR 列表；不能判断同步分支是否已有待审 PR，已停止且未做任何改动" ""
            exit "$EXIT_TOOLING"
        fi
        pending_count="$(printf '%s\n' "$PR_NUMBERS" | grep -c . || true)"

        case "$pending_count" in
            0)
                # 先确认这个分支还有「可提议的内容」：PR 已合并（merge/squash）而分支仍在时，
                # 不该去开一个没有任何差异的空 PR，也不该把它当成需要恢复的状态。
                if nothing_to_propose "$SYNC_REF"; then
                    ACTION="noop"
                    report_header
                    report_append "## 动作：noop（同步分支已并入默认分支，无可提议内容）"
                    report_append ""
                    report_append "同步分支 \`$SYNC_BRANCH\` 仍在，但它的内容已经在 fork 默认分支里"
                    report_append "（PR 已合并，或等价补丁已在默认分支上），因此本次不创建 PR、也不改动分支。"
                    report_append ""
                    report_verification
                    write_report_if_requested
                    emit_json
                    return 0
                fi

                # dry run 必须在**调用任何 gh 写操作之前**生效：这条路径同样会创建 PR。
                if [ "$DRY_RUN" = "1" ]; then
                    ACTION="would_recover_pr"
                    PR_ACTION="skipped_dry_run"
                    report_header
                    report_append "## 动作：would_recover_pr（dry run）"
                    report_append ""
                    report_append "同步分支 \`$SYNC_BRANCH\` 已存在且已包含当前上游提交 \`$UPSTREAM_SHA\`，但没有对应的待审 PR。"
                    report_append "若去掉 dry run，本次会为同一分支**新建** PR（不重新推送、不强推、不改动分支内容）。"
                    report_append "dry run 不执行任何 gh 写操作、不推送、不改动分支。"
                    report_append ""
                    report_verification
                    write_report_if_requested
                    emit_json
                    return 0
                fi

                # 分支已是最新却没有待审 PR：补建 PR（不重新推送、不动分支内容）。
                [ -n "$REPORT_PATH" ] || die_usage "REPORT_PATH is required to create a pull request"
                report_header
                report_append "## 动作：recovered_pr（为已存在的同步分支补建待审 PR）"
                report_append ""
                report_append "同步分支 \`$SYNC_BRANCH\` 已存在且已包含当前上游提交 \`$UPSTREAM_SHA\`，但没有对应的待审 PR："
                report_append "上一次运行推送成功但 PR 创建失败，或者该 PR 已被关闭/合并而分支仍保留。"
                report_append ""
                report_append "本次**没有**重新推送、没有强推、没有改动分支内容，只为同一分支新建 PR。"
                report_append ""
                report_verification
                write_report_if_requested
                create_pending_pr
                query_ci_state "$PR_NUMBER"
                ACTION="recovered_pr"
                emit_json
                return 0
                ;;
            1)
                PR_NUMBER="$(printf '%s\n' "$PR_NUMBERS" | awk 'NR==1 { print $1 }')"
                PR_URL="https://github.com/${FORK_REPO}/pull/${PR_NUMBER}"
                ACTION="noop"
                report_append "## 动作：noop（上游无新提交，待审 PR #${PR_NUMBER} 仍在）"
                report_append ""
                report_append "上游没有新的提交，已有同步分支和待审 PR 保持不变，不会重复开 PR。"
                report_append ""
                report_verification
                write_report_if_requested
                emit_json
                return 0
                ;;
            *)
                block "blocked_ambiguous_prs" \
                    "同步分支上有 $pending_count 个待审 PR，请先合并或关闭多余 PR 再继续" ""
                exit "$EXIT_AMBIGUOUS_PR"
                ;;
        esac
    fi

    # 5. 在本地合并上游，按冲突类型决定动作
    local start_branch start_sha
    start_branch="$(current_branch)"
    start_sha="$(gitq rev-parse HEAD)"

    if [ "$SYNC_PRESENT" = "true" ]; then
        OURS_REF="$SYNC_REF"
    else
        OURS_REF="$BASE_REF"
    fi
    gitq checkout -q -B "$SYNC_BRANCH" "$OURS_REF"

    local merge_status=0 merge_output=""
    merge_output="$(gitq merge --no-ff --no-commit "$UPSTREAM_REF" 2>&1)" || merge_status=$?
    log "merge status: $merge_status"
    printf '%s\n' "$merge_output" >&2

    MERGE_BASE="$(gitq merge-base "$(gitq rev-parse HEAD)" "$UPSTREAM_REF" 2>/dev/null || true)"

    # 本次同步带来的上游改动中，落在受保护路径上的部分：只报告，不采用。
    # 用 merge-base..upstream（而不是 ours..upstream）：后者会把 fork 自己长期
    # 领先上游的定制也列进来，让报告误称「上游改了这些文件」。
    PROTECTED_CHANGED="$(gitq diff --name-only "${MERGE_BASE:-$OURS_REF}" "$UPSTREAM_REF" 2>/dev/null \
        | sort | filter_protected)"

    if [ "$merge_status" -ne 0 ]; then
        local conflicts unprotected_conflicts
        conflicts="$(collect_conflict_paths)"
        unprotected_conflicts="$(printf '%s\n' "$conflicts" | filter_unprotected)"
        if [ -n "$unprotected_conflicts" ]; then
            restore_start_state "$start_branch" "$start_sha"
            report_upstream_changes
            report_protected_files
            block "blocked_conflict" \
                "上游合并与 fork 现状冲突，需要维护者人工解决" "$unprotected_conflicts"
            exit "$EXIT_CONFLICT"
        fi
        if [ -z "$conflicts" ]; then
            restore_start_state "$start_branch" "$start_sha"
            report_append "## git merge 输出（供维护者诊断）"
            report_append ""
            report_append '```'
            report_append "$(printf '%s\n' "$merge_output" | awk -v n="$MAX_REPORT_DIFF_LINES" 'NR <= n')"
            report_append '```'
            report_append ""
            block "blocked_merge_error" \
                "git merge 失败且没有冲突标记（非冲突类错误），未改动远端" ""
            exit "$EXIT_TOOLING"
        fi
        log "conflicts limited to fork-owned paths; keeping the fork versions: $(printf '%s' "$conflicts" | tr '\n' ' ')"
    fi

    # 6. 受保护路径一律以 fork 为准（覆盖「干净合并」与「冲突」两种情形）
    restore_protected_paths "$OURS_REF"
    if [ -n "$RESTORED_PROTECTED" ]; then
        log "kept fork content for: $(printf '%s' "$RESTORED_PROTECTED" | tr '\n' ' ')"
    fi

    # 6. 已经是最新（上游提交已在分支里）
    if gitq diff --cached --quiet && gitq diff --quiet \
        && ! gitq rev-parse --verify --quiet MERGE_HEAD >/dev/null; then
        restore_start_state "$start_branch" "$start_sha"
        ACTION="noop"
        report_append "## 动作：noop（分支已包含上游提交）"
        report_append ""
        report_verification
        write_report_if_requested
        emit_json
        return 0
    fi

    # 7. 提交合并（机器人身份 + 可追溯标记）
    local msg_file="${REPO_DIR}/.git/fork-sync-commit-message"
    write_merge_message "$msg_file"
    commit_merge "$msg_file"
    rm -f "$msg_file"
    NEW_SYNC_SHA="$(gitq rev-parse HEAD)"

    report_upstream_changes
    report_protected_files

    if [ "$DRY_RUN" = "1" ]; then
        restore_start_state "$start_branch" "$start_sha"
        if [ "$SYNC_PRESENT" = "true" ]; then ACTION="would_update"; else ACTION="would_create"; fi
        PR_ACTION="skipped_dry_run"
        report_append "## 动作：$ACTION（dry run）"
        report_append ""
        report_append "Dry run：没有推送分支，也没有创建或更新 PR。"
        report_append ""
        report_verification
        write_report_if_requested
        emit_json
        return 0
    fi

    # 8. 待审 PR：>1 个时不再新增，交人工处理
    [ -n "$REPORT_PATH" ] || {
        restore_start_state "$start_branch" "$start_sha"
        die_usage "REPORT_PATH is required to create or update a pull request"
    }
    local numbers count
    # 读不到或读不懂列表 -> 不能假设「没有待审 PR」：那会直接变成重复开票。fail closed。
    if ! fetch_pending_pr_numbers; then
        restore_start_state "$start_branch" "$start_sha"
        block "blocked_tooling" \
            "无法读取或结构化校验待审 PR 列表；为避免重复开票已停止，未推送、未创建或更新任何 PR" ""
        exit "$EXIT_TOOLING"
    fi
    numbers="$PR_NUMBERS"
    count="$(printf '%s\n' "$numbers" | grep -c . || true)"
    if [ "$count" -gt 1 ]; then
        restore_start_state "$start_branch" "$start_sha"
        block "blocked_ambiguous_prs" \
            "同步分支上有 $count 个待审 PR，请先合并或关闭多余 PR 再继续" ""
        exit "$EXIT_AMBIGUOUS_PR"
    fi

    # 9. CI 状态：只报告事实，绝不声称通过。
    #    更新场景读到的是「上一次推送」的检查结果；新建场景此时还没有检查。
    if [ "$count" -eq 1 ]; then
        PR_NUMBER="$(printf '%s' "$numbers" | awk '{ print $1 }')"
        PR_URL="https://github.com/${FORK_REPO}/pull/${PR_NUMBER}"
        query_ci_state "$PR_NUMBER"
        CI_DETAIL="previous head: ${CI_DETAIL}; 本次推送后检查会重新排队，可能等待维护者批准"
    else
        CI_STATE="unverified"
        CI_DETAIL="newly created pull request; no checks reported yet, CI may await maintainer approval"
    fi

    # 10. 只向前推送该分支：显式 refspec、无 --force、无 --tags
    if ! gitq push --no-tags --quiet "$PUSH_URL" \
        "refs/heads/${SYNC_BRANCH}:refs/heads/${SYNC_BRANCH}"; then
        restore_start_state "$start_branch" "$start_sha"
        block "blocked_push_rejected" \
            "推送被拒绝（非快进或权限不足）；未做任何强推" ""
        exit "$EXIT_TOOLING"
    fi

    # 11. 正文（含 CI 事实）落盘后再维护 PR：有且仅有一次正文写入
    if [ "$count" -eq 0 ]; then
        ACTION="created"
        PR_ACTION="created"
    else
        ACTION="updated"
        PR_ACTION="updated"
    fi
    report_append "## 动作：$ACTION"
    report_append ""
    report_verification
    write_report_if_requested

    # 12. 维护同一个 PR：没有待审 PR 才新建，否则只更新正文
    if [ "$count" -eq 0 ]; then
        create_pending_pr
    else
        if ! "$GH_BIN" pr edit "$PR_NUMBER" --repo "$FORK_REPO" --body-file "$REPORT_PATH" >/dev/null; then
            pr_step_failed "更新 PR 正文失败"
        fi
    fi

    emit_json
    return 0
}

main "$@"
