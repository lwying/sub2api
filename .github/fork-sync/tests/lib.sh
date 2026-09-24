#!/bin/bash
# fork-sync 离线测试夹具库。
#
# 硬约束：这些测试**不得访问网络**。
#   - 所有 git remote 都是本进程创建的本地 bare 仓库（local transport，无网络）；
#   - `gh` 由本文件生成的桩替代，只读写本地状态文件；破坏性 gh 子命令（merge/close/
#     delete/release/…）一律失败，从而把「工作流必须只维护一个待审 PR」钉住。
#   - 测试只操作临时目录，绝不触碰当前工作树（E:\Go\sub2api/.git 与本仓库 index）。

set -euo pipefail

FORK_SYNC_DIR="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
FORK_SYNC_SCRIPT="${FORK_SYNC_SCRIPT:-${FORK_SYNC_DIR}/upstream-sync.sh}"

# ---------------------------------------------------------------------------
# 断言
# ---------------------------------------------------------------------------

fail() {
    printf 'FAIL: %s\n' "$*" >&2
    exit 1
}

pass() {
    printf 'ok - %s\n' "$*"
}

assert_eq() {
    local expected="$1" actual="$2" what="$3"
    [[ "$expected" == "$actual" ]] || fail "${what}: expected '${expected}', got '${actual}'"
}

assert_file_exists() {
    [[ -f "$1" ]] || fail "expected file to exist: $1"
}

assert_contains() {
    local haystack="$1" needle="$2" what="$3"
    case "$haystack" in
        *"$needle"*) ;;
        *) fail "${what}: expected to contain '${needle}'; actual: ${haystack}" ;;
    esac
}

assert_not_contains() {
    local haystack="$1" needle="$2" what="$3"
    case "$haystack" in
        *"$needle"*) fail "${what}: expected NOT to contain '${needle}'; actual: ${haystack}" ;;
        *) ;;
    esac
}

assert_missing_path() {
    [[ ! -e "$1" ]] || fail "expected path to be absent: $1"
}

# 提取脚本 stdout 里的 JSON 字段（缺 python 时明确失败，不静默通过）。
json_get() {
    local json="$1" expr="$2" py
    if command -v python >/dev/null 2>&1; then
        py=python
    elif command -v python3 >/dev/null 2>&1; then
        py=python3
    else
        fail "no python interpreter available for JSON assertions"
    fi
    printf '%s' "$json" > "$FIXTURE_ROOT/json-in.json"
    "$py" -c 'import json, sys
doc = json.load(open(sys.argv[1], encoding="utf-8"))
cur = doc
for part in sys.argv[2].split("."):
    if part == "":
        continue
    if isinstance(cur, list):
        cur = cur[int(part)]
    else:
        cur = cur[part]
print(json.dumps(cur) if isinstance(cur, (dict, list, bool)) else cur)' \
        "$FIXTURE_ROOT/json-in.json" "$expr"
}

# ---------------------------------------------------------------------------
# 夹具
# ---------------------------------------------------------------------------

# 读取仓库里 "ref:path" 的对象。对象名经 stdin 传入 git cat-file --batch：
# 直接当参数传时，Windows/MSYS 会把 "refs/heads/x:dir/file" 改写成路径列表
# （反斜杠 + 分号），断言会读到空值、假失败。这里两种平台都走同一路径。
ref_blob_sha() {
    local repo="$1" ref="$2" path="$3"
    printf '%s:%s\n' "$ref" "$path" | git -C "$repo" cat-file --batch 2>/dev/null \
        | awk 'NR==1 { if ($2 == "missing") exit 0; print $1; exit }' || true
}

ref_first_line() {
    local repo="$1" ref="$2" path="$3"
    printf '%s:%s\n' "$ref" "$path" | git -C "$repo" cat-file --batch 2>/dev/null \
        | awk 'NR==2 { print; exit }' || true
}

ref_has_path() {
    local sha
    sha="$(ref_blob_sha "$1" "$2" "$3")"
    [ -n "$sha" ]
}

new_fixture_root() {
    FIXTURE_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/sub2api-fork-sync.XXXXXX")"
    FIXTURE_KEEP="${FIXTURE_KEEP:-0}"
    cleanup() {
        local status=$?
        if [[ "$status" -ne 0 || "$FIXTURE_KEEP" == "1" ]]; then
            printf 'fixture kept for inspection: %s\n' "$FIXTURE_ROOT" >&2
        else
            rm -rf "$FIXTURE_ROOT"
        fi
        return "$status"
    }
    trap cleanup EXIT
}

# 种子内容：同时存在于 fork 与 upstream，其中若干路径属于 fork 自有（受保护）文件。
seed_tree() {
    local dir="$1" flavor="$2"
    mkdir -p "$dir/app" "$dir/deploy" "$dir/.github/workflows" \
        "$dir/backend/cmd/server" "$dir/backend/internal/service"
    printf 'seed\n' > "$dir/README.md"
    printf 'package app\n\nfunc Normal() string { return "normal-v1" }\n' > "$dir/app/normal.go"
    printf '0.2.6\n' > "$dir/backend/cmd/server/VERSION"
    if [[ "$flavor" == "fork" ]]; then
        printf '#!/bin/sh\n# fork-owned installer (ticket 08)\necho fork-installer-v1\n' > "$dir/deploy/install.sh"
        printf 'name: Release\n# fork-owned release gate (ticket 06)\non:\n  push:\n    tags: [v*]\n' > "$dir/.github/workflows/release.yml"
        printf 'package service\n\nconst ForkUpdater = "fork-v1"\n' > "$dir/backend/internal/service/update_service.go"
        # 接线文件：fork 在这里注册自己的 provider（上游版本会把注册删掉）
        printf 'package main\n\n// fork wiring: registers the fork updater and the key billing snapshot\n' > "$dir/backend/cmd/server/wire_gen.go"
        # 受保护目录（fork 自有）：验证「上游往目录里新增文件」不会被叠加进来
        mkdir -p "$dir/backend/internal/releasecontract"
        printf 'package releasecontract\n\n// fork-owned contract implementation\n' > "$dir/backend/internal/releasecontract/marker.go"
    else
        printf '#!/bin/sh\necho upstream-installer-v1\n' > "$dir/deploy/install.sh"
        printf 'name: Release\non:\n  push:\n    tags: [v*]\n' > "$dir/.github/workflows/release.yml"
        printf 'package service\n\nconst ForkUpdater = "upstream-v1"\n' > "$dir/backend/internal/service/update_service.go"
        printf 'package main\n\n// upstream wiring\n' > "$dir/backend/cmd/server/wire_gen.go"
        mkdir -p "$dir/backend/internal/releasecontract"
        printf 'package releasecontract\n\n// upstream contract implementation\n' > "$dir/backend/internal/releasecontract/marker.go"
    fi
    chmod +x "$dir/deploy/install.sh" 2>/dev/null || true
}

# 克隆夹具仓库：显式关闭 autocrlf/eol 转换，并对齐工作树。
# 本机（Windows）全局配置可能开启 core.autocrlf=true；若在 clone 之后再改配置，
# 工作树会与 index 不一致，git 会误报「local changes」，夹具就失真了。
git_clone() {
    local src="$1" dst="$2"
    rm -rf "$dst"
    git clone -q -c core.autocrlf=false -c core.eol=lf "$src" "$dst"
    git -C "$dst" config core.autocrlf false
    git -C "$dst" config core.eol lf
    git -C "$dst" reset -q --hard HEAD
}

git_init_work() {
    local dir="$1" name="$2" email="$3"
    git -c init.defaultBranch=main init -q "$dir"
    git -C "$dir" config user.name "$name"
    git -C "$dir" config user.email "$email"
    git -C "$dir" config commit.gpgsign false
    git -C "$dir" config core.autocrlf false
}

# 建立 upstream bare + work clone；UPSTREAM_BARE / UPSTREAM_WORK 为全局。
init_upstream() {
    UPSTREAM_BARE="$FIXTURE_ROOT/upstream.git"
    UPSTREAM_WORK="$FIXTURE_ROOT/upstream-work"
    git -c init.defaultBranch=main init -q --bare "$UPSTREAM_BARE"
    git_init_work "$UPSTREAM_WORK" "Upstream Dev" "dev@upstream.example"
    seed_tree "$UPSTREAM_WORK" upstream
    git -C "$UPSTREAM_WORK" add -A
    git -C "$UPSTREAM_WORK" commit -q -m "chore: seed upstream"
    git -C "$UPSTREAM_WORK" remote add origin "$UPSTREAM_BARE"
    git -C "$UPSTREAM_WORK" push -q origin main
    git -C "$UPSTREAM_BARE" symbolic-ref HEAD refs/heads/main
}

# 建立 fork bare + work clone；FORK_BARE / FORK_WORK 为全局。
# 必须先调用 init_upstream：fork 是从上游 fork 出来的，历史与上游共享
# （否则 git merge 会因 unrelated histories 失败，与真实 fork 不符）。
init_fork() {
    FORK_BARE="$FIXTURE_ROOT/fork.git"
    FORK_WORK="$FIXTURE_ROOT/fork-work"
    git -c init.defaultBranch=main init -q --bare "$FORK_BARE"
    git_clone "$UPSTREAM_BARE" "$FORK_WORK"
    git -C "$FORK_WORK" config user.name "Fork Maintainer"
    git -C "$FORK_WORK" config user.email "maintainer@fork.example"
    git -C "$FORK_WORK" config commit.gpgsign false
    git -C "$FORK_WORK" config core.autocrlf false
    git -C "$FORK_WORK" remote remove origin
    git -C "$FORK_WORK" remote add origin "$FORK_BARE"
    seed_tree "$FORK_WORK" fork
    git -C "$FORK_WORK" add -A
    git -C "$FORK_WORK" commit -q -m "chore: fork customizations"
    git -C "$FORK_WORK" push -q origin main
    git -C "$FORK_BARE" symbolic-ref HEAD refs/heads/main
}

# 让 fork 的 main 与 upstream 完全一致（用于「无变更 → 不开 PR」场景）。
# 注：这里的 --force 只作用于本测试自建的 fixture bare，不代表被测工具允许强推。
align_fork_to_upstream() {
    git -C "$FORK_WORK" fetch -q "$UPSTREAM_BARE" main
    git -C "$FORK_WORK" reset -q --hard FETCH_HEAD
    git -C "$FORK_WORK" push -q --force origin main
}

# 模拟维护者在 GitHub 上合并同步 PR：把同步分支以 merge commit 的方式并入 fork 默认分支。
merge_sync_branch_into_fork_main() {
    local branch="$1"
    git -C "$FORK_WORK" fetch -q "$FORK_BARE" "$branch"
    git -C "$FORK_WORK" merge -q --no-ff FETCH_HEAD -m "Merge pull request: upstream sync ($branch)"
    git -C "$FORK_WORK" push -q origin main
}

# 模拟维护者用 squash 方式合并：在 fork 默认分支上提交一个与上游提交**等价的补丁**
# （同样的路径与内容，因此 patch-id 相同），分支尖端并不会成为 main 的祖先。
squash_equivalent_into_fork_main() {
    local path="$1" content="$2"
    mkdir -p "$(dirname "$FORK_WORK/$path")"
    printf '%b' "$content" > "$FORK_WORK/$path"
    git -C "$FORK_WORK" add -A
    git -C "$FORK_WORK" commit -q -m "squash merge: upstream sync ($path)"
    git -C "$FORK_WORK" push -q origin main
}

# 把桩状态里某个分支的 PR 标成已合并（模拟 PR 已合并、分支仍在）。
gh_stub_mark_merged() {
    local branch="$1"
    awk -F'\t' -v OFS='\t' -v head="$branch" '{ if ($3 == head) $2 = "MERGED"; print }' "$GH_STATE" > "$GH_STATE.next"
    mv "$GH_STATE.next" "$GH_STATE"
}

# 在 upstream 上追加一次提交，推送到 bare。
upstream_commit() {
    local message="$1" path="$2" content="$3"
    mkdir -p "$(dirname "$UPSTREAM_WORK/$path")"
    printf '%b' "$content" > "$UPSTREAM_WORK/$path"
    git -C "$UPSTREAM_WORK" add -A
    git -C "$UPSTREAM_WORK" commit -q -m "$message"
    git -C "$UPSTREAM_WORK" push -q origin main
}

upstream_head() {
    git -C "$UPSTREAM_BARE" rev-parse refs/heads/main
}

# 在 fork 自己的默认分支上追加提交（模拟 fork 定制/手工改动）。
fork_commit() {
    local message="$1" path="$2" content="$3"
    mkdir -p "$(dirname "$FORK_WORK/$path")"
    printf '%b' "$content" > "$FORK_WORK/$path"
    git -C "$FORK_WORK" add -A
    git -C "$FORK_WORK" commit -q -m "$message"
    git -C "$FORK_WORK" push -q origin main
}

fork_head() {
    git -C "$FORK_BARE" rev-parse refs/heads/main
}

# fresh clone（模拟 CI 每次全新检出）。
clone_fork() {
    local dir="$1"
    git_clone "$FORK_BARE" "$dir"
    git -C "$dir" config user.name "fork-sync bot"
    git -C "$dir" config user.email "fork-sync-bot@users.noreply.github.com"
    git -C "$dir" config commit.gpgsign false
    printf '%s\n' "$dir"
}

# 模拟维护者手工创建同步分支并推上去（例如在本地解决冲突后提交）。
human_create_sync_branch() {
    local branch="$1" message="$2" path="$3" content="$4"
    local dir="$FIXTURE_ROOT/human-branch-work"
    git_clone "$FORK_BARE" "$dir"
    git -C "$dir" config user.name "Human Maintainer"
    git -C "$dir" config user.email "human@fork.example"
    git -C "$dir" config commit.gpgsign false
    git -C "$dir" checkout -q -b "$branch"
    mkdir -p "$(dirname "$dir/$path")"
    printf '%b' "$content" > "$dir/$path"
    git -C "$dir" add -A
    git -C "$dir" commit -q -m "$message"
    git -C "$dir" push -q origin "$branch"
    git -C "$FORK_BARE" rev-parse "refs/heads/$branch"
}

# 更对抗的伪造：人工提交故意使用机器人的作者/提交者身份，甚至照抄 fork-sync trailer
# （含 MergeBase，否则会先因为缺这个 trailer 被拦下，测不到 parent2 绑定）。
# mode=plain  普通提交 + 抄来的 trailer
# mode=merge  伪造成 merge 形状，但第二个 parent 不是标记里写的上游提交
forge_bot_shaped_commit() {
    local branch="$1" mode="$2" upstream_sha="$3" base_sha="$4"
    local merge_base_sha="$4"
    local dir="$FIXTURE_ROOT/forge-work"
    git_clone "$FORK_BARE" "$dir"
    (
        cd "$dir"
        git config user.name "fork-sync bot"
        git config user.email "fork-sync-bot@users.noreply.github.com"
        git config commit.gpgsign false
        git checkout -q "$branch"
        printf 'forged\n' > "app/forged-$mode.txt"
        git add -A
        local message
        message="$(printf 'chore(fork-sync): merge Wei-Shaw/sub2api/main into %s\n\nFork-Sync-Upstream: %s\nFork-Sync-Base: %s\nFork-Sync-MergeBase: %s\nFork-Sync-Tool: .github/fork-sync/upstream-sync.sh\n' \
            "$branch" "$upstream_sha" "$base_sha" "$merge_base_sha")"
        if [ "$mode" = "merge" ]; then
            # 真 merge 形状（两个 parent），但第二个 parent 指向 fork 的 main，
            # 而不是标记里声称的上游提交。
            tree="$(git rev-parse 'HEAD^{tree}')"
            forged="$(git commit-tree "$tree" -p "$(git rev-parse HEAD)" -p "$(git rev-parse origin/main)" -m "$message")"
            git update-ref "refs/heads/$branch" "$forged"
        else
            git commit -q -m "$message"
        fi
        git push -q origin "$branch"
        git rev-parse "refs/heads/$branch"
    )
}

# 模拟维护者/其他人直接在同步分支上追加人工修复提交。
human_commit_on_sync_branch() {
    local branch="$1" message="$2"
    local dir="$FIXTURE_ROOT/human-work"
    git_clone "$FORK_BARE" "$dir"
    git -C "$dir" config user.name "Human Maintainer"
    git -C "$dir" config user.email "human@fork.example"
    git -C "$dir" config commit.gpgsign false
    git -C "$dir" checkout -q "$branch"
    printf 'manual fix\n' > "$dir/app/manual-fix.txt"
    git -C "$dir" add -A
    git -C "$dir" commit -q -m "$message"
    git -C "$dir" push -q origin "$branch"
    git -C "$FORK_BARE" rev-parse "refs/heads/$branch"
}

sync_branch_sha() {
    local branch="$1"
    git -C "$FORK_BARE" rev-parse --verify --quiet "refs/heads/$branch" || printf 'absent\n'
}

fork_branch_list() {
    git -C "$FORK_BARE" for-each-ref --format='%(refname:short)' refs/heads/ | sort
}

# ---------------------------------------------------------------------------
# gh 桩
# ---------------------------------------------------------------------------

write_gh_stub() {
    GH_BIN_DIR="$FIXTURE_ROOT/bin"
    GH_BIN="$GH_BIN_DIR/gh"
    GH_STATE="$FIXTURE_ROOT/gh-state.tsv"
    GH_BODIES="$FIXTURE_ROOT/gh-bodies"
    GH_LOG="$FIXTURE_ROOT/gh-calls.log"
    mkdir -p "$GH_BIN_DIR" "$GH_BODIES"
    : > "$GH_STATE"
    : > "$GH_LOG"

    cat > "$GH_BIN" <<'STUB'
#!/bin/bash
# 离线 gh 桩：只读写 GH_STUB_STATE / GH_STUB_BODIES。破坏性操作一律失败。
set -euo pipefail

STATE="${GH_STUB_STATE:?}"
BODIES="${GH_STUB_BODIES:?}"
LOG="${GH_STUB_LOG:?}"
CHECKS="${GH_STUB_CHECKS:-[]}"
LOGIN="${GH_STUB_LOGIN:-fork-sync-bot}"

printf '%s\n' "$*" >> "$LOG"

[ "${1:-}" = "pr" ] || { printf 'gh stub: only `pr` is supported, got: %s\n' "${1:-}" >&2; exit 2; }
shift
action="${1:-}"
shift || true

repo=""; head=""; want_state=""; body_file=""; jq_expr=""; json_fields=""; limit="20"
positional=()
while [ "$#" -gt 0 ]; do
    case "$1" in
        --repo) repo="$2"; shift 2 ;;
        --head) head="$2"; shift 2 ;;
        --state) want_state="$2"; shift 2 ;;
        --body-file) body_file="$2"; shift 2 ;;
        --json) json_fields="$2"; shift 2 ;;
        --jq) jq_expr="$2"; shift 2 ;;
        --limit) limit="$2"; shift 2 ;;
        --title|--base|--body) shift 2 ;;
        --*) shift ;;
        *) positional+=("$1"); shift ;;
    esac
done

max_number() {
    local max=0 n
    while IFS=$'\t' read -r n _rest; do
        [ -n "$n" ] || continue
        [[ "$n" =~ ^[0-9]+$ ]] || continue
        [ "$n" -gt "$max" ] && max="$n"
    done < "$STATE"
    printf '%s' "$max"
}

body_path() { printf '%s/pr-%s.md' "$BODIES" "$1"; }

case "$action" in
    list)
        if [ -n "${GH_STUB_LIST_FAIL:-}" ]; then
            printf 'gh stub: pr list failed on purpose (GH_STUB_LIST_FAIL)\n' >&2
            exit 1
        fi
        if [ -n "${GH_STUB_LIST_RAW:-}" ]; then
            printf '%s\n' "$GH_STUB_LIST_RAW"
            exit 0
        fi
        if [ -n "${GH_STUB_LIST_COUNT:-}" ]; then
            i=1
            printf '['
            while [ "$i" -le "$GH_STUB_LIST_COUNT" ]; do
                [ "$i" -eq 1 ] || printf ','
                printf '{"number":%s,"state":"OPEN","headRefName":"%s","url":"https://github.com/%s/pull/%s"}' \
                    "$i" "$head" "$repo" "$i"
                i=$((i + 1))
            done
            printf ']\n'
            exit 0
        fi
        rows=""
        while IFS=$'\t' read -r n st h _url; do
            [ -n "$n" ] || continue
            [ "$h" = "$head" ] || continue
            if [ -n "$want_state" ]; then
                case "$st" in
                    OPEN) cur="open" ;;
                    MERGED) cur="merged" ;;
                    CLOSED) cur="closed" ;;
                    *) cur="$st" ;;
                esac
                [ "$cur" = "$want_state" ] || continue
            fi
            rows="${rows}${rows:+,}{\"number\":${n},\"state\":\"${st}\",\"headRefName\":\"${h}\",\"url\":\"https://github.com/${repo}/pull/${n}\"}"
        done < "$STATE"
        printf '[%s]\n' "$rows"
        ;;
    create)
        if [ -n "${GH_STUB_CREATE_FAIL:-}" ]; then
            printf 'gh stub: pr create failed on purpose (GH_STUB_CREATE_FAIL)\n' >&2
            exit 1
        fi
        if [ -n "${GH_STUB_CREATE_MALFORMED:-}" ]; then
            # 模拟「退出码 0 但输出不可用」：工具不能据此宣称创建成功
            printf '%s\n' "$GH_STUB_CREATE_MALFORMED"
            exit 0
        fi
        n=$(( $(max_number) + 1 ))
        printf '%s\tOPEN\t%s\thttps://github.com/%s/pull/%s\n' "$n" "$head" "$repo" "$n" >> "$STATE"
        [ -n "$body_file" ] && cp "$body_file" "$(body_path "$n")"
        printf 'https://github.com/%s/pull/%s\n' "$repo" "$n"
        ;;
    edit|comment)
        n="${positional[0]:-}"
        [[ "$n" =~ ^[0-9]+$ ]] || { printf 'gh stub: %s needs a number\n' "$action" >&2; exit 2; }
        if [ "$action" = "edit" ] && [ -n "$body_file" ]; then
            cp "$body_file" "$(body_path "$n")"
        fi
        printf 'https://github.com/%s/pull/%s\n' "$repo" "$n"
        ;;
    view|checks)
        n="${positional[0]:-}"
        if [ -z "$n" ]; then
            n="$(awk -F'\t' -v h="$head" '$3==h {print $1}' "$STATE" | tail -1)"
        fi
        [[ "$n" =~ ^[0-9]+$ ]] || { printf 'gh stub: no PR found\n' >&2; exit 1; }
        if [ "$action" = "checks" ]; then
            case "$jq_expr" in
                '.[] | .state') ;;
                '') ;;
                *) printf 'gh stub: unsupported --jq for checks: %s\n' "$jq_expr" >&2; exit 2 ;;
            esac
            if command -v python >/dev/null 2>&1; then PY=python; else PY=python3; fi
            "$PY" - "$CHECKS" <<'PY'
import json, sys
for item in json.loads(sys.argv[1]):
    print(item.get("state", ""))
PY
            # 真实 gh 在检查未通过/未完成时会以非零码退出，但**仍然打印状态**；
            # 桩必须复现这一点，才能验证工具没有把输出丢掉。
            if [ -n "${GH_STUB_CHECKS_EXIT:-}" ]; then
                exit "$GH_STUB_CHECKS_EXIT"
            fi
        else
            case "$jq_expr" in
                '.[0] | "\(.number) \(.url)"')
                    awk -F'\t' -v n="$n" '$1==n {printf "%s https://github.com/%s/pull/%s\n", $1, $4, $1; exit}' "$STATE" | sed 's#https://github.com/##' ;;
                '.[0].number')
                    printf '%s\n' "$n" ;;
                *)
                    printf 'gh stub: unsupported --jq for view: %s\n' "$jq_expr" >&2; exit 2 ;;
            esac
        fi
        ;;
    merge|close|reopen|ready|delete|lock|unlock)
        printf 'gh stub: destructive gh pr %s must never be called by the sync workflow\n' "$action" >&2
        exit 3
        ;;
    api)
        printf 'gh stub: gh api must never be called; use gh pr subcommands\n' >&2
        exit 3
        ;;
    *)
        printf 'gh stub: unsupported gh pr subcommand: %s\n' "$action" >&2
        exit 2
        ;;
esac
STUB
    chmod +x "$GH_BIN"
}

# 记录 CI 检查状态（GH_STUB_CHECKS 的 JSON）。
set_ci_checks() {
    GH_STUB_CHECKS="${1:-[]}"
    export GH_STUB_CHECKS
}

# 直接在桩状态里塞一个待审 PR（用于构造「同步分支上有多个待审 PR」的异常场景）。
gh_stub_add_pr() {
    local number="$1" head="$2"
    printf '%s\tOPEN\t%s\thttps://github.com/lwying/sub2api/pull/%s\n' \
        "$number" "$head" "$number" >> "$GH_STATE"
}

# ---------------------------------------------------------------------------
# 运行被测脚本
# ---------------------------------------------------------------------------

# sync_run <clone_dir> [args...]
# 结果写到 SYNC_STATUS / SYNC_OUT / SYNC_ERR（stdout 只应包含 JSON）。
sync_run() {
    local clone="$1"; shift
    set +e
    (
        cd "$clone"
        REPO_DIR="$clone" \
        FORK_REPO="lwying/sub2api" \
        UPSTREAM_REPO="Wei-Shaw/sub2api" \
        FORK_URL="$FORK_BARE" \
        PUSH_URL="$FORK_BARE" \
        UPSTREAM_URL="$UPSTREAM_BARE" \
        DEFAULT_BRANCH="${DEFAULT_BRANCH:-main}" \
        SYNC_BRANCH="${SYNC_BRANCH:-chore/upstream-sync}" \
        GH_BIN="$GH_BIN" \
        GH_STUB_STATE="$GH_STATE" \
        GH_STUB_BODIES="$GH_BODIES" \
        GH_STUB_LOG="$GH_LOG" \
        GH_STUB_CHECKS="${GH_STUB_CHECKS:-[]}" \
        GH_STUB_CHECKS_EXIT="${GH_STUB_CHECKS_EXIT:-}" \
        GH_STUB_LIST_FAIL="${GH_STUB_LIST_FAIL:-}" \
        GH_STUB_LIST_RAW="${GH_STUB_LIST_RAW:-}" \
        GH_STUB_LIST_COUNT="${GH_STUB_LIST_COUNT:-}" \
        GH_STUB_CREATE_FAIL="${GH_STUB_CREATE_FAIL:-}" \
        GH_STUB_CREATE_MALFORMED="${GH_STUB_CREATE_MALFORMED:-}" \
        REPORT_PATH="${REPORT_PATH:-$FIXTURE_ROOT/report.md}" \
        BOT_NAME="${BOT_NAME:-fork-sync bot}" \
        BOT_EMAIL="${BOT_EMAIL:-fork-sync-bot@users.noreply.github.com}" \
        FORK_SYNC_DISPOSABLE_CHECKOUT="${FORK_SYNC_DISPOSABLE_CHECKOUT-1}" \
        FORK_SYNC_TOKEN_KIND="${FORK_SYNC_TOKEN_KIND:-}" \
        DRY_RUN="${DRY_RUN:-0}" \
        bash "$FORK_SYNC_SCRIPT" "$@"
    ) >"$FIXTURE_ROOT/out.txt" 2>"$FIXTURE_ROOT/err.txt"
    SYNC_STATUS=$?
    set -e
    SYNC_OUT="$(cat "$FIXTURE_ROOT/out.txt")"
    SYNC_ERR="$(cat "$FIXTURE_ROOT/err.txt")"
    REPORT_PATH="${REPORT_PATH:-$FIXTURE_ROOT/report.md}"
}
