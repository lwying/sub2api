#!/bin/bash
# 行为 7：工作流那一步「真的」会怎样。
#
# 契约测试只检查文本与结构；这里把 run 脚本原文抽出来、在临时目录里用**普通 bash
# （不带 -e）**执行，验证：
#   - 同步脚本成功 -> 步骤成功；
#   - 同步脚本阻塞（冲突/人工提交，非零退出）-> 步骤失败（维护者收到通知），
#     且报告仍然写进 job summary（失败原因可见）；
#   - 不依赖运行器隐式的 `bash -e`：脚本自己声明 set -euo pipefail。

set -euo pipefail

source "$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

new_fixture_root

WORKFLOW="${FORK_SYNC_DIR}/../workflows/upstream-sync.yml"
assert_file_exists "$WORKFLOW"

if command -v python >/dev/null 2>&1; then
    PY=python
else
    PY=python3
fi

RUN_SCRIPT="$FIXTURE_ROOT/workflow-step.sh"
"$PY" - "$WORKFLOW" "$RUN_SCRIPT" <<'PY'
import sys
import yaml

with open(sys.argv[1], encoding="utf-8") as fh:
    doc = yaml.safe_load(fh)

runs = []
for job in (doc.get("jobs") or {}).values():
    for step in job.get("steps") or []:
        if step.get("run"):
            runs.append(step["run"])
assert len(runs) == 1, f"expected exactly one run step, got {len(runs)}"

# 表达式不是 shell；run 正文里不应出现表达式，但保险起见做一次占位替换。
script = runs[0].replace("${{", "EXPR{{")
with open(sys.argv[2], "w", encoding="utf-8", newline="\n") as fh:
    fh.write(script)
print("extracted run step", file=sys.stderr)
PY

# 每个场景都跑在独立沙箱里：假的 .github/fork-sync/upstream-sync.sh 负责成功或阻塞，
# 并把工作流传进来的凭据环境记录下来，供断言检查（token 只能经 GIT_ASKPASS 提供，
# 绝不进 argv、也不进 git URL）。
# 结果放在 STEP_* 全局变量里（函数在子 shell 里跑的话这些变量会丢失，所以不用 $( ) 调用）。
run_step_with_stub() {
    local stub_status="$1" stub_report="$2" sandbox
    sandbox="$(mktemp -d "${TMPDIR:-/tmp}/sub2api-fork-sync-step.XXXXXX")"
    mkdir -p "$sandbox/.github/fork-sync" "$sandbox/tmp"
    cat > "$sandbox/.github/fork-sync/upstream-sync.sh" <<STUB
#!/bin/bash
printf '%s\n' '{"action":"stub"}'
printf '%s\n' "$stub_report" > "\${REPORT_PATH}"
{
  printf 'GIT_ASKPASS=%s\n' "\${GIT_ASKPASS:-}"
  printf 'GIT_TERMINAL_PROMPT=%s\n' "\${GIT_TERMINAL_PROMPT:-}"
  printf 'ARGV=%s\n' "\$*"
} > "$sandbox/env-dump.txt"
exit $stub_status
STUB

    set +e
    (
        cd "$sandbox"
        RUNNER_TEMP="$sandbox/tmp" \
        GH_TOKEN="dummy-token-value" \
        REPORT_PATH="$sandbox/tmp/report.md" \
        GITHUB_STEP_SUMMARY="$sandbox/tmp/summary.md" \
        bash "$RUN_SCRIPT" > "$sandbox/stdout.txt" 2> "$sandbox/stderr.txt"
    )
    STEP_STATUS=$?
    set -e
    STEP_SANDBOX="$sandbox"
    STEP_SUMMARY="$sandbox/tmp/summary.md"
    STEP_STDOUT="$sandbox/stdout.txt"
    STEP_STDERR="$sandbox/stderr.txt"
    STEP_ENV_DUMP="$sandbox/env-dump.txt"
}

# --- 成功：步骤成功，报告进 summary ---
run_step_with_stub 0 "report-body-ok"
assert_eq 0 "$STEP_STATUS" "a successful sync must pass the step"
assert_contains "$(cat "$STEP_SUMMARY")" "report-body-ok" "the report must reach the job summary"
assert_not_contains "$(cat "$STEP_STDOUT")" "::error::" "a successful run must not raise an error annotation"
pass "successful sync: step succeeds and the report is summarised"

# --- 阻塞：步骤失败，但报告仍然进 summary ---
run_step_with_stub 2 "report-body-blocked"
assert_eq 2 "$STEP_STATUS" "a blocked sync must fail the step so maintainers are notified"
assert_contains "$(cat "$STEP_SUMMARY")" "report-body-blocked" "the blocking report must still be summarised"
assert_contains "$(cat "$STEP_STDOUT")" "::error::" "a blocked sync must annotate the failure"
pass "blocked sync: step fails loudly while keeping the report visible"

# --- 凭据接线：一次性检出的 token 只经 GIT_ASKPASS 提供 ---
ASKPASS_PATH="$(sed -n 's/^GIT_ASKPASS=//p' "$STEP_ENV_DUMP")"
[[ -n "$ASKPASS_PATH" ]] || fail "the step must export GIT_ASKPASS for credential-less checkout/push"
assert_file_exists "$ASKPASS_PATH"
[[ -x "$ASKPASS_PATH" ]] || fail "the askpass helper must be executable"
assert_eq "0" "$(sed -n 's/^GIT_TERMINAL_PROMPT=//p' "$STEP_ENV_DUMP")" \
    "GIT_TERMINAL_PROMPT must be 0 so git never blocks on a prompt"

# 用户名提示 -> 固定的 x-access-token；密码提示 -> 环境里的 token（不落盘、不进 argv）
assert_eq "x-access-token" "$("$ASKPASS_PATH" "Username for 'https://github.com':" 2>/dev/null)" \
    "askpass must answer the username prompt"
assert_eq "dummy-token-value" \
    "$(GH_TOKEN="dummy-token-value" "$ASKPASS_PATH" "Password for 'https://x-access-token@github.com':" 2>/dev/null)" \
    "askpass must read the token from the environment at call time"

# token 不得出现在传给同步脚本的 argv 里，也不得写进 askpass 文件
assert_not_contains "$(sed -n 's/^ARGV=//p' "$STEP_ENV_DUMP")" "dummy-token-value" \
    "the token must never be passed as a command-line argument"
assert_not_contains "$(cat "$ASKPASS_PATH")" "dummy-token-value" \
    "the token must never be written into the askpass helper"
pass "the disposable checkout receives credentials only through GIT_ASKPASS"

# --- 脚本自己不依赖隐式 -e：抽出来的正文必须显式声明 ---
assert_contains "$(cat "$RUN_SCRIPT")" "set -euo pipefail" \
    "the run step must set -euo pipefail explicitly instead of relying on the runner"

printf 'PASS %s\n' "$(basename "$0")"
