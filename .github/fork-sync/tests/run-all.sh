#!/bin/bash
# 运行 fork-sync 的全部离线测试。
#
# 用法：bash .github/fork-sync/tests/run-all.sh [测试文件名子串...]
#
# 这些测试**不访问网络**，并且这一点是被强制验证的：整个套件在
# 把所有 HTTP(S) 代理指向一个死端口的环境下运行；夹具的 remote 全是本地 bare 仓库，
# `gh` 由桩替代。任何真的去连 GitHub 的尝试都会立刻失败而不是悄悄通过。
#
# 依赖：bash、git、python（含 PyYAML，用于解析 workflow）。缺依赖时明确失败，不跳过。

set -euo pipefail

TEST_DIR="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

filter=("$@")

if ! command -v git >/dev/null 2>&1; then
    printf 'FAIL: git is required to run these tests\n' >&2
    exit 1
fi
if command -v python >/dev/null 2>&1; then
    PY=python
elif command -v python3 >/dev/null 2>&1; then
    PY=python3
else
    printf 'FAIL: python is required (workflow YAML contract test)\n' >&2
    exit 1
fi
"$PY" -c 'import yaml' 2>/dev/null || {
    printf 'FAIL: PyYAML is required to parse .github/workflows/upstream-sync.yml\n' >&2
    exit 1
}

# 不给测试任何出口网络：代理指向死端口。
export http_proxy="http://127.0.0.1:9"
export https_proxy="http://127.0.0.1:9"
export HTTP_PROXY="$http_proxy"
export HTTPS_PROXY="$https_proxy"
export ALL_PROXY="$http_proxy"
export no_proxy=""
export NO_PROXY=""
export GIT_TERMINAL_PROMPT=0

tests=()
while IFS= read -r test_file; do
    [ -n "$test_file" ] || continue
    case "$(basename "$test_file")" in
        lib.sh|run-all.sh) continue ;;
    esac
    tests+=("$test_file")
done < <(find "$TEST_DIR" -maxdepth 1 -name '*-test.sh' | sort)

[[ "${#tests[@]}" -gt 0 ]] || { printf 'FAIL: no tests found in %s\n' "$TEST_DIR" >&2; exit 1; }

selected=()
for test_file in "${tests[@]}"; do
    if [ "${#filter[@]}" -eq 0 ]; then
        selected+=("$test_file")
        continue
    fi
    for needle in "${filter[@]}"; do
        case "$(basename "$test_file")" in
            *"$needle"*) selected+=("$test_file"); break ;;
        esac
    done
done

[[ "${#selected[@]}" -gt 0 ]] || { printf 'FAIL: no test matched the given filters\n' >&2; exit 1; }

failures=0
passed=0
for test_file in "${selected[@]}"; do
    name="$(basename "$test_file")"
    printf '\n=== %s ===\n' "$name"
    if bash "$test_file"; then
        passed=$((passed + 1))
    else
        failures=$((failures + 1))
        printf 'FAILED: %s\n' "$name" >&2
    fi
done

printf '\n%s: %d passed, %d failed\n' "$(basename "$0")" "$passed" "$failures"
[ "$failures" -eq 0 ]
