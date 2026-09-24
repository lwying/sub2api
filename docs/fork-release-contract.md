# Fork 发布契约（06）

> 本文档记录 fork `lwying/sub2api` 的发布版本门禁与 Release 资产契约，供后续票据
> 07（内置更新）、08（回滚与安装）和 09（上游同步）复用。本文只描述**本地可复现**
> 的判定规则；真实发布权限、分支保护与远端资产必须由维护者现场核验（见文末）。

## 1. 适用范围

- 发布仓库固定为 `lwying/sub2api`。门禁输入中的 `fork_repo` 不等于该值时直接拒绝
  （`fork_repo_mismatch`），因此任何在别的仓库里跑的发布都不可能被当作 fork 发布。
- 上游 `Wei-Shaw/sub2api` 的 tag、`VERSION` 变动和已合并提交，本身都不构成 fork 发布。
- 本文档与门禁都**不发布 tag、不创建 Release、不写远端**。真实发布仍由维护者决定。

## 2. 版本契约

fork 使用独立、递增的三段数字版本 `vX.Y.Z`：

| 规则 | 拒绝码 |
| --- | --- |
| 必须是 `vX.Y.Z`：三段、全数字、无前导零、无预发布/构建后缀（`v0.2`、`v0.2.8-rc.1`、`V0.2.8`、`0.2.8` 均拒绝） | `bad_format` |
| 必须严格高于 fork 已发布基线 | `not_increasing` |
| 必须是 fork 自己决定发布的版本，不能只是上游同步误带的版本 | `upstream_only` |
| fork 已发布基线读不到时拒绝发布，而不是当作“从未发布” | `fork_baseline_unknown` |
| 触发入口只允许 tag push 与 `workflow_dispatch` | `unknown_event` |

手动发布以该次 `workflow_dispatch` 的 `simple_release` 输入决定是否仅发布镜像；tag push 则按仓库变量 `SIMPLE_RELEASE` 决定。两个入口的发布任务共用串行队列，不会同时写入可变的镜像 `latest` 标签或回写 VERSION。

**基线来源**：`fork_published_versions` 只允许来自 fork 自己的 Releases。本地 git tag
**不是**基线：本工作树同时带有从 upstream 拉取的 tag（`v0.1.172` … `v0.2.7`），用
tag 当基线会把上游版本误当成 fork 已发布版本。门禁的辅助输入
`upstream_versions` 用于识别这类版本：候选版本只存在于上游时就拒绝，维护者需要改选
一个更高的版本号。

**两个入口都要过门禁**：`release-gate` job 在 tag push 与手动 dispatch 下都会运行，
且 `update-version`、`build-frontend`、`release` 都 `needs` 它（版本归一化也只在门禁里做
一次）。手动发布时，前端与后端都检出门禁确定的 tag，避免混入默认分支的前端构建。
门禁失败时后续 job 全部不执行，不会产生任何发布产物。

### 2.1 基线采集的边界（本地不可核实，必须 fail-closed）

基线来自 `gh release list --limit 1000`，采集过程本身有三类不确定，处理如下：

| 情况 | 标记 | 门禁行为 |
| --- | --- | --- |
| fork Releases 查询失败 | `fork_baseline_unknown` | **拒绝**：读不到基线不等于没有基线 |
| 返回条数达到分页上限 | `fork_baseline_truncated` | **拒绝**：被截断的列表可能恰好漏掉最高的已发布版本 |
| upstream tags 分页查询或解析失败 | `upstream_versions_unknown` | 不拒绝，但判定里记录告警：该次「上游误带版本」检查未执行（fork 基线检查仍生效） |

工作流以分页方式读取 upstream tags；查询或解析失败仍按上表降级，不会把失败当成空列表。这三项都是**本地无法验证的远端事实**：`GITHUB_TOKEN` 能否读取 fork Releases 与
upstream tags、fork 真实 Release 数量是否远低于上限 1000、以及上游 API 是否可达。
门禁把「读不到」和「可能读不全」都当成拒绝，而不是当成空基线；只有 upstream 列表
失败属于可降级项，且降级本身会写进判定 JSON 与 job 日志（门禁会打印判定结果）。
维护者若确需在有大量历史 Release 的仓库发布，应显式提高 `PUBLISHED_LIMIT` 或人工
核实基线，而不是放宽该检查。

## 3. Release 资产契约

| 发布形态 | 配置 | 可安装平台 |
| --- | --- | --- |
| 完整二进制发布 | `.goreleaser.yaml`：`archives` + `checksums.txt` | 有对应归档的平台 |
| 仅镜像发布 | `.goreleaser.simple.yaml`：`archives: []`、`checksum.disable: true`、`release.skip_upload: true` | 无（任何平台都不可安装） |

完整二进制发布必须同时具备：

- 五个平台归档：`sub2api_<version>_{linux,darwin}_{amd64,arm64}.tar.gz` 与
  `sub2api_<version>_windows_amd64.zip`（与 `.goreleaser.yaml` 的 goos/goarch 矩阵一致，
  `windows/arm64` 被 ignore）。
- `checksums.txt`。**缺少校验文件时该发布视为不可安装**（`checksums_missing`），
  而不是“大概没问题”。这是本票采用的上线默认值：现有下载域名白名单只校验主机，
  不足以证明资产确实来自 fork，因此不可校验的归档不得变成一键更新来源。
  更新器/安装脚本侧的执行落地属于 07/08，本票只固定契约与判定。

仅镜像发布必须被判定为 `image_only`：它没有任何平台二进制资产，因此不得出现在
“可一键更新”提示或可执行的历史回滚候选中。若一个声明为仅镜像的 Release 意外带了
归档或校验文件，判定为 `kind_mismatch`（仍然不可安装）并在发布 job 里报错。

判定结果以平台列表形式给出（`installable_platforms` / `missing_platforms`），
07/08 应按平台取用，而不是只看一个整体布尔值。

### 3.1 校验时机与不完整 Release（重要，勿当成回滚/阻止保证）

资产校验发生在 **GoReleaser 已经创建并发布 Release 之后**。必须精确理解它的能力边界：

- **它不能阻止一个不完整的 Release 被公开。** 校验运行前 Release 与已上传的资产已经在
  远端存在（可能缺平台归档或缺 `checksums.txt`）。本校验**不会删除、不会修复、不会撤回**
  该 Release。
- 它**只能**做两件事：让 release job 失败，以及因此**不执行 VERSION 写回**（即这次失败
  不会"伪造"出一个新版本）。清理或重发属于维护者人工动作，且按回滚约定**不覆盖已发布
  tag、不强推默认分支**。唯一的事前控制是发布前的版本/类型门禁与正确的 `--config`；
  资产完整性本身只能在发布后才能观测到。
- **读不到资产列表 ≠ 没有二进制资产。** 若 `gh release view` 查询失败，工作流不会把它
  折叠成空列表：会带上 `assets_unverified` 交给门禁，判定为 `assets_unverified`，
  并在 **binary 与 image_only 两种情况下都让 job 失败**，且打印的结论是"验证未完成"，
  **不宣称任何可安装性**。只有查询成功且确实没有资产时，才允许输出 image-only 分类。
- 因此 07/08 **不得**把"存在一个 Release"当成"可安装"：必须对同一资产契约逐平台判定
  （有对应归档 **且** 有 `checksums.txt`）。不完整、未验证或仅镜像的 Release 不得出现在
  一键更新提示或历史回滚候选中。
- 仅镜像与完整二进制的区分来自同一个 `SIMPLE_RELEASE` 开关：它既决定 GoReleaser 用哪个
  `--config`，也决定门禁把这次运行归类为 `image_only` 还是 `binary`；发布后的资产校验
  直接消费门禁的输出，因此两者不会各算一套。这条一致性由单元测试断言。

## 4. VERSION 写回与分支保护

`backend/cmd/server/VERSION` 是 fork 自有文件（编译期 embed 进二进制）：

- 发布 job 内的 `update-version` 只在本 job 工作区写入该文件作为构建输入，**不推送**。
- `sync-version-file` 只在 release job 成功后才运行（`needs.release.result == 'success'`），
  因此 CI/Release 失败不会伪造出新版本。
- 写回前必须先过 `releasegate sync-version`：候选版本必须**严格高于**当前 VERSION 文件，
  否则以 `would_move_backwards` 失败（`not_increasing` 的同类保护，防止把版本号写低）。
- 默认分支可能受保护，所以推送必须由维护者显式开启仓库变量
  `FORK_ALLOW_DEFAULT_BRANCH_VERSION_PUSH`。未开启时判定为 `push_not_permitted`：
  这是一个**成功**的“跳过”，不是静默 no-op，也不会绕过分支保护强推；此时版本文件
  应通过维护者控制的 PR 途径更新。
- 工作流不再把 `github.event.inputs.tag` 直接拼进脚本正文，dispatch 输入一律通过
  `env:` 传入并由 `jq --arg` 组装成 JSON，避免发布入口的命令注入。

## 5. 本地验证（可复现，无需 tag / Release / 网络）

```bash
cd backend
go test -tags=unit ./internal/releasecontract/...   # 版本门禁、资产契约、CLI 退出码
go build ./cmd/releasegate                          # 门禁可执行文件
```

用本地 fixture 直接跑门禁。可执行程序退出码 0=接受、1=拒绝、2=输入不可用；
`go run` 会把程序的非零退出码折叠为自身的退出码 1，需区分 1/2 时请先构建再运行程序：

```bash
cd backend
go build -o releasegate ./cmd/releasegate
./releasegate gate         -input internal/releasecontract/testdata/gate_upstream_only.json
./releasegate gate         -input internal/releasecontract/testdata/gate_baseline_truncated.json
./releasegate assets       -input internal/releasecontract/testdata/assets_image_only.json
./releasegate assets       -input internal/releasecontract/testdata/assets_binary_no_checksums.json
./releasegate sync-version -input internal/releasecontract/testdata/sync_push_not_permitted.json
```

`releasegate` **不读取网络、git 或 GitHub**：所有事实由调用方（工作流或 fixture）提供，
所以同一判定在本地与 CI 完全一致。门槛检查同时覆盖工作流接线本身：单元测试会读取
`.github/workflows/release.yml` 与 `.goreleaser*.yaml`，断言两个入口都过门禁、
默认分支推送带显式开关、以及仅镜像配置不含平台二进制。

## 6. 需要维护者现场核验的事项（手动，本票未执行）

以下均未在本票验证，也不由本票授权；执行前需维护者自行确认：

1. fork 仓库的实际 Release 列表、真实构建产物与 `checksums.txt` 内容；fork 实际
   Release 数量是否远低于 `PUBLISHED_LIMIT`（1000），以及 `gh release list` 分页是否
   真的覆盖全部历史（本地无法核实，见 2.1）。
2. `lwying/sub2api` 的 Actions 权限、工作流审批要求，以及 `GITHUB_TOKEN` 能否读取
   fork 自身 Releases / upstream tags。
3. 默认分支保护规则的实际配置；若不允许任何直推，则不要开启
   `FORK_ALLOW_DEFAULT_BRANCH_VERSION_PUSH`，改用 PR 途径更新 VERSION。
4. “缺少 `checksums.txt` 是否禁止安装”的最终确定值。本票按已批准计划的默认值
   实现为“禁止”，但仍需 07/08 在更新器/安装脚本侧落地一致的执行。
5. `DEV_GUIDE.md` 的 fork 地址已改为 `lwying/sub2api`；发布前仍须核对实际 remote、Release 来源与维护者权限，不能仅凭文档地址判断发布已验证。
6. 发布后资产校验失败时留下的**不完整 Release 需要人工处理**（清理或重发）；本票
   不自动删除 Release、不覆盖 tag、不强推分支，理由见 3.1。
