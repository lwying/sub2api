#!/bin/bash
# 行为 6：`.github/workflows/upstream-sync.yml` 的契约。
#
# 用结构化解析（不是文本搜索）固定这些不变量：
#   - 每天一次（03:17 UTC）+ 手动触发，只有这两种触发；
#   - 最小权限：contents: write + pull-requests: write，仅此而已；
#   - 单实例并发（不取消进行中的运行），避免同一时间出现两个同步 PR；
#   - 事件载荷不得拼进 shell 正文；所有 run 脚本必须能被 bash 解析；
#   - 不合并 PR、不打 tag、不发布 Release、不部署、不强推、不推送默认分支；
#   - 报告必须写入 job summary，且不把未批准的 CI 说成通过。

set -euo pipefail

source "$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

new_fixture_root

WORKFLOW="${FORK_SYNC_DIR}/../workflows/upstream-sync.yml"
assert_file_exists "$WORKFLOW"

if command -v python >/dev/null 2>&1; then
    PY=python
elif command -v python3 >/dev/null 2>&1; then
    PY=python3
else
    fail "python is required to parse the workflow YAML"
fi

# 把 workflow 里的 run 脚本逐个抽出来（制表符分隔的 base64），供 bash -n 与文本断言使用。
"$PY" - "$WORKFLOW" "$FIXTURE_ROOT/workflow-runs" <<'PY'
import base64, sys
import yaml

path, out_dir = sys.argv[1], sys.argv[2]
with open(path, encoding="utf-8") as fh:
    doc = yaml.safe_load(fh)

# YAML 1.1 会把未加引号的 `on:` 解析成布尔 True
triggers = doc.get("on") if "on" in doc else doc.get(True)

import os
os.makedirs(out_dir, exist_ok=True)
index = 0
for job_name, job in (doc.get("jobs") or {}).items():
    steps = job.get("steps") or []
    for step_no, step in enumerate(steps):
        script = step.get("run")
        if not script:
            continue
        index += 1
        name = f"{job_name}-{step_no}-{step.get('name', 'step')}".replace(" ", "_").replace("/", "_")
        with open(os.path.join(out_dir, f"{index}-{name}.sh"), "w", encoding="utf-8", newline="\n") as fh:
            fh.write(script)
        with open(os.path.join(out_dir, f"{index}-{name}.meta"), "w", encoding="utf-8") as fh:
            fh.write(f"{job_name}\t{step.get('name', '')}\n")

summary = {
    "jobs": list((doc.get("jobs") or {}).keys()),
    "permissions": doc.get("permissions"),
    "concurrency": doc.get("concurrency"),
    "runs": index,
    "triggers": triggers,
}
with open(os.path.join(out_dir, "summary.json"), "w", encoding="utf-8") as fh:
    import json
    json.dump(summary, fh, ensure_ascii=False, indent=2)
print(json.dumps(summary, ensure_ascii=False))
PY

RUNS_DIR="$FIXTURE_ROOT/workflow-runs"
RUN_FILES="$(find "$RUNS_DIR" -name '*.sh' | sort)"
[[ -n "$RUN_FILES" ]] || fail "workflow must contain at least one run script"

# --- 触发条件：每天 03:17 UTC + 手动，仅此两种 ---
assert_eq "1" "$(json_get "$(cat "$RUNS_DIR/summary.json")" runs)" "workflow must have exactly one run script"
"$PY" - "$WORKFLOW" <<'PY' || exit 1
import sys
import yaml

with open(sys.argv[1], encoding="utf-8") as fh:
    doc = yaml.safe_load(fh)
triggers = doc.get("on") if "on" in doc else doc.get(True)
assert isinstance(triggers, dict), f"expected a mapping of triggers, got {triggers!r}"
assert set(triggers) == {"schedule", "workflow_dispatch"}, f"only schedule + workflow_dispatch are allowed, got {sorted(triggers)}"
schedule = triggers["schedule"]
assert len(schedule) == 1, f"expected a single cron entry, got {schedule!r}"
assert schedule[0]["cron"] == "17 3 * * *", f"expected daily 03:17 UTC, got {schedule[0]['cron']!r}"
dispatch = triggers["workflow_dispatch"] or {}
inputs = dispatch.get("inputs") or {}
assert set(inputs) <= {"dry_run"}, f"unexpected dispatch inputs: {sorted(inputs)}"
if "dry_run" in inputs:
    assert inputs["dry_run"].get("type") == "boolean", "dry_run must be a boolean input"
PY
pass "triggers are daily 03:17 UTC + manual dispatch only"

# --- 最小权限 ---
assert_eq '{"contents": "write", "pull-requests": "write"}' \
    "$("$PY" -c 'import json,sys; print(json.dumps(json.load(open(sys.argv[1],encoding="utf-8"))["permissions"], sort_keys=True))' "$RUNS_DIR/summary.json")" \
    "workflow permissions must be exactly contents + pull-requests write"

# --- 并发：同组不取消，避免同时开两个 PR ---
"$PY" - "$WORKFLOW" <<'PY' || exit 1
import sys
import yaml

with open(sys.argv[1], encoding="utf-8") as fh:
    doc = yaml.safe_load(fh)
concurrency = doc.get("concurrency")
assert isinstance(concurrency, dict), f"concurrency must be a mapping, got {concurrency!r}"
assert concurrency.get("group"), "concurrency.group must be set"
assert concurrency.get("cancel-in-progress") is False, "cancel-in-progress must be false"
PY
pass "workflow is serialised by a concurrency group"

# --- run 脚本：可解析、无事件载荷插值、无破坏性命令 ---
while IFS= read -r file; do
    [ -n "$file" ] || continue
    # 把 ${{ ... }} 替换为占位符后做语法检查（表达式本身不是 shell）
    substituted="$FIXTURE_ROOT/$(basename "$file").sub"
    sed -e 's/\${{[^}]*}}/EXPR/g' "$file" > "$substituted"
    bash -n "$substituted" || fail "run script is not valid bash: $file"
    assert_not_contains "$(cat "$file")" '${{ github.event.' \
        "event payload must not be interpolated into run scripts"
    assert_not_contains "$(cat "$file")" '${{ inputs.' \
        "dispatch inputs must be passed via env, not interpolated"
    assert_not_contains "$(cat "$file")" 'git push' \
        "pushing is the sync script's job (with its own safe refspec)"
    assert_not_contains "$(cat "$file")" '--force' "run scripts must never force push"
done <<< "$RUN_FILES"
pass "every run script parses and avoids event-payload interpolation"

# --- 工作流整体：不合并、不发布、不部署、不打 tag ---
WORKFLOW_TEXT="$(cat "$WORKFLOW")"
for forbidden in 'gh pr merge' 'gh pr close' 'gh pr delete' 'gh release' 'git tag' \
    '--force-with-lease' 'goreleaser' 'release.yml' 'workflow_dispatch' ; do
    case "$forbidden" in
        workflow_dispatch) continue ;;
    esac
    assert_not_contains "$WORKFLOW_TEXT" "$forbidden" "workflow must not contain '$forbidden'"
done
assert_contains "$WORKFLOW_TEXT" '.github/fork-sync/upstream-sync.sh' "workflow must call the sync helper"
assert_contains "$WORKFLOW_TEXT" 'GITHUB_STEP_SUMMARY' "workflow must surface the report in the job summary"
assert_contains "$WORKFLOW_TEXT" 'persist-credentials: false' "checkout must not persist the default token"
pass "workflow never merges, publishes, deploys or tags"

printf 'PASS %s\n' "$(basename "$0")"
