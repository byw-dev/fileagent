# 质量闸门 track（QG）：让 CI 的绿灯可信

> **这是 `docs/tasks/active.md`「下一步」第 2 条的完整追踪文件**（该条原文只有一行摘要）。
> 规划于 2026-09-26，**跨会话执行**：苦力活派 worker，判断与验证归协调者。

## 🚚 开工摘要（新会话从这里读）

**当前状态**：规划完毕，**QG-1…QG-7 全部未开工**，零代码改动。
`master` = `d094418`（Cut A / PR #122 已合）。

**开工顺序**：读 `CLAUDE.md` → `docs/tasks/active.md` → **本文件** →
先做「落地顺序」步骤 0（起 PG 量基线，别跳过：门槛数字由它决定）。

**已在本会话核实的事（不要重做）**：见「关键事实」10 条 + 「缺口」6 条，全部为实测，
含 `gh api` 确认的 `master` 无分支保护、`ingest_integration_test.go:119` 的语义标记、
`deadletter`/`ingest` 自足跑迁移、16 个生成文件的头部注释原文。

**⚠️ 规划会话犯过的两个事实错误（已纠正，勿沿用旧说法）**：
① 曾以为 `deadletter_integration_test.go` 不跑迁移——**它跑**（临时 schema + rollback）；
② 曾以为 CP integration 测试没有语义标记——**有**，就在最要紧的那个测试里。

**未验证的推断（执行时必须先验）**：`go vet -tags=integration ./...` 能否编译通过。
今天没有任何东西编译过那 9 个文件，第一版可能先红一轮。

## Context

**来源**：`docs/tasks/active.md`「下一步」第 2 条（2026-09-24 产品拍板）。前置已完成：
第 1 条采集写放大（PR #118）、Cut A 测试信号可信（PR #122，`d094418`）。
**串行的理由**：测试还在随机红时加门槛，新门槛第一天就会被「重跑一下」消解。

**病根**（`docs/tasks/backlog.md`「CI 与门槛的执行力缺口」，来自 PR #110 四轮复审）：
> 仓库写明了一个门槛，但没有任何东西在执行它。

| # | 缺口 | 实测证据 | 定级 |
|---|------|---------|------|
| 1 | CI 无 PostgreSQL，PG 关键测试静默 skip | `webhook_r3_red*_test.go` 全部 `t.Skipf("no PostgreSQL")`；**IC-4a 的 PG 权威单调计数在 CI 里一次都没跑过**；`ci-cp.yml` 中 `postgres`/`services:` 零命中 | 🟠 P1 |
| 2 | 覆盖率门槛写明却无人执行 | CLAUDE.md 要求整体 ≥ 80%，实测 79.9%；`ci-cp.yml` 中 `coverage` 零命中 | 🟠 P1 |
| 3 | webui PR 门缺失 | 22 个 vitest 文件（176 用例）在 CI 里**从未执行** | AUD-11 |
| 4 | `pkg/trollsift` 无人跑 | 3 个测试文件、零 workflow；它是路径模板核心（D-034 三端对齐） | 本轮新发现 |
| 5 | Node 版本约束无人执行 | 「Node 24」写在 7 处、执行 0 处；且文档声称的 Corepack 机制是空的 | 本轮新发现 |
| 6 | **`master` 无分支保护** | `branches/master/protection` → **404**，`rulesets` → **`[]`**。没有任何东西**要求** CI 绿 | 🔴 **前提** |

缺口 6 是前提：闸门不是 required check，就只是建议——这刀会退化成「让红灯有内容」而非「让绿灯可信」。

**边界（不可越）**：生成代码不计入门槛，且**不能给生成代码补单测凑数**（与「禁止手改生成文件」结构冲突）；
`active.md`「明确不做」：**覆盖率补到 80% 不做**（「为凑数写测试正是 CLAUDE.md 禁止的」）。
本刀只让门槛**可执行**，不负责把数字堆上去。

## 关键事实（实测；推翻了几个想当然的做法）

1. **不能直接 `go test -tags=integration ./...`** —— `controlplane/internal/storage/sts_integration_test.go`
   **无 env 门控**、硬编码 `localhost:9000`，无 MinIO 时 `require.NoError` **硬失败而非 skip**。
   它是 9 个 integration 文件里唯一没门控的，也是 `-tags=integration` 在 CI 里从未能跑的那块石头。
2. **`*.sql.go` glob 是错的** —— 漏 `internal/db/models.go`(584) 与 `db.go`(31)。判据必须是文件头
   `^// Code generated .* DO NOT EDIT\.$`（package 子句之前），即 Go 官方那条正则。生成文件共 **16** 个
   （controlplane 14 + 根 module `api/v1/` 2）。
3. **`internal/db/` 不能整包排除** —— 14 个生成文件与手写 `read_queries.go`(531)、`batchtag_queries.go`(151) 等
   **同属一个 Go 包**，`-coverpkg` 粒度是包。只能对 `coverage.out` 做**文件级过滤后重算加权总数**。
4. **`services:` 不吃 `docker/login-action` 凭据** —— 将来接私有 GHCR 的 MinIO 需 service 级 `credentials:`。
5. **`TEST_DATABASE_URL` 是总开关** —— CP 里 **11 处** DSN 读取点全部优先读它。**显式设了它，两套硬编码默认值
   （`fileagent_test` / `fileagent`）在 CI 里都是死路径**，库名分歧不会发作。不设则 `ingest`/`deadletter`
   继续静默 skip——而那正是 P1 的核心。
6. **真正需要预建表的只有 4 个文件**：`batchtag_integration_test.go` + 三个 `webhook_*_test.go`。
   `ingest` 与 `deadletter` **都自足**（各自 glob `../../migrations/*.up.sql` 进临时 schema + rollback）。
   ⚠️ 同包按文件名排序执行，`batchtag_*` 排在 `db_integration_test.go` **之前**，指望后者顺带建表不可靠。
7. **CP 侧有语义标记** —— `ingest_integration_test.go:119` 打印
   `t.Log("MUTATION | A1 | A2 | A3prime | P2 | A4b | LPAD")`，之后每个 mutant 一行。
   恰好就在最要紧的那个测试里，所以能照抄 `ci-agent.yml` 的 `grep -q "gate open"` 范式。
8. **AUD-8 标尺（均不带 tag）**：无 PG **79.4%** → PG 在线未设 `TEST_DATABASE_URL` **79.9%** →
   四个 PG 文件全开 **80.1%**。可用于事后判别「PG 是否静默消失」。
9. **webui 22 文件 176 用例在 Node 24 下全绿**；本机默认 Node 26 下 2 文件 15 用例**假红**
   （Node 26 自带实验性 `localStorage` 遮蔽 jsdom）。`pnpm test` 已是 `vitest run`；
   coverage provider 已配 `v8`，只缺 `thresholds`。
10. **Corepack 叙事是空的** —— `corepack` 在 `.github/`/`Makefile`/`deploy/` **零命中**；CI 实际用
    `pnpm/action-setup@v4` + **浮动 `version: '11'`**，绕过 `packageManager: pnpm@11.0.4`。
    且 Corepack 只管包管理器、**管不了 Node 自身**，而 `webui/README.md:25` 声称它锁「Node.js 和 pnpm 版本」。
    ~~AUD-12~~ 撤回时说过「只说明本地未开 `engine-strict`」——方向对，但没看出叙事本身是空的。

## 已拍板的决策

| 决策点 | 结论 |
|---|---|
| 覆盖率口径 | 排除生成代码后按 **80%**；不达标则设为**当前实测值（棘轮，只准上调）**，PR 里写清差多少 |
| PR 粒度 | **2 个 PR**：CP（缺口 1/2/4）、webui（缺口 3/5）；缺口 6 收尾由用户操作 |
| `sts_integration_test.go` | **补 env 门控**，与 `IC2A_LIVE`/`IC2B_LIVE`/`IC3_LIVE` 同模式 |
| 覆盖率是否带 tag | **带 `-tags=integration`**，与测试同一次跑完 |
| webui 覆盖率 | **只测量不判阈**（CLAUDE.md 的「核心 store/service 层」无定义，定阈值只会再造执行不了的门） |
| `pkg/trollsift` | 纳入 CP 那一刀 |
| Node 钉法 | `.nvmrc` 写 `24` + `engine-strict=true` |
| Corepack | **向 CI 现状对齐，拆除 Corepack 叙事** |
| PG 起法 | **`docker compose … up -d --wait postgres`**，不用 `services:`（偏离 `active.md` 字面，理由见下） |
| 分支保护 | 纳入本刀收尾，**由用户操作** |

## PR 1：CP 侧（缺口 1 + 2 + 4）

### 设计总纲
**机制在脚本，策略在 conf，workflow 只负责起 PG + 调一条 make。** 这是 `ci-cp.yml` 自己立的规矩
（"Uses `make generate` so local and CI run the identical command"）的延伸。
**正向断言优先**：「必须有某个东西 PASS」同时抓住 skip / 红 / 被改名删除三种退化，而
「没有 SKIP」抓不住第三种，且因为有 4 个合法 skip 注定要开白名单——那就不如一开始做成**账本**。

### 为什么 compose 而非 `services:`
① PG 配置单一真相（就是开发者本地那份 `deploy/docker-compose.test.yml`），`services:` 会在 workflow 里
再抄一份必然漂移；② 本地完全同构复现；③ 将来接 MinIO 只需 `up -d --wait postgres minio` 一个词，
而 `services:` 需单独写 `credentials:`（见事实 4）；④ `ci-smoke.yml` 已有 compose 先例。
只点名 `postgres`，所以**不需要** `packages: read`、不碰私有 GHCR 镜像。

### 新增/修改文件

| 文件 | 内容 |
|---|---|
| `scripts/ci/cover-percent.sh`（新） | 按文件头正则识别生成文件 → 对 profile 做**文件级**过滤 → 重算加权覆盖率。输出 `<pct> <covered> <total> <genfiles>` |
| `scripts/ci/test-gate.sh`（新） | 驱动：`go vet`（含 tag）→ 先行迁移 → 全量 `go test -v -coverprofile` → 三层断言 → 覆盖率闸 |
| `ci/gate-controlplane.conf`（新） | **策略**：`COVERAGE_MIN`、`EXPECT_GENERATED_FILES`、`REQUIRED_PASS`、`REQUIRED_LOG(_COUNT)`、`ALLOWED_SKIP`、`TEST_DATABASE_URL_DEFAULT`、`SETUP_TEST` |
| `controlplane/internal/storage/sts_integration_test.go` | 加 `requireLiveMinIO(t)`（`STS_LIVE=1`），两个 Test 各调一次；顺手删只有 `t.Skip` 的空壳 `TestSTSManager_IssueCredentials_NoMinIO` |
| `.github/workflows/ci-cp.yml` | paths 补 `api/** pkg/** go.work* deploy/docker-compose.test.yml scripts/ci/** ci/**`；加 `concurrency`、`permissions: contents: read`、`timeout-minutes`；加起 PG 步骤；`vet + test` 换成 `make test-gate-controlplane`；加失败时 dump PG 日志 + 上传 `.ci-out/` artifact |
| `Makefile` | 加 `test-gate-controlplane`（+ 可选 `test-gate-agent`），同改 `.PHONY`（line 31）。`make test` 保留给本地快循环 |
| `.gitignore` | 加 `/.ci-out/` —— **否则 drift guard 的 `git status --porcelain` 会把 coverage.out 当漂移报红** |

### 覆盖率算法要点
- profile 行：`<import-path>/<file>.go:<start>.<col>,<end>.<col> <numStmts> <count>`；
  总覆盖率 = Σ(被覆盖 numStmts) / Σ(numStmts)。
- **以 `file:range` 整体为 key 去重**：go test 直接拼接各包 profile fragment，同一块可能出现多行，
  naive 累加会把分母算重。
- 模块路径读 `go.mod` 的 `module` 行，**不要用 `go list -m`**（go.work 模式下会打印所有 workspace 模块）。
- awk 里先 `sub(/\r$/, "")`（Windows checkout 的 CRLF 会让 `$` 匹配不上）。
- `END` 里**重读**生成文件清单取总数，而非「出现在 profile 里的那些」——这样零语句的 `models.go` 也算进去。

### 迁移谁来跑
`SETUP_TEST=TestMigrate_IdempotentOnCleanDB`（`./internal/db`）先单独跑一次建表。
它走的是**生产同一条路径**（`db.Migrate` + 嵌入 FS，D-023，与 `cmd/server/main.go:109` 同一函数）、
本身断言幂等、且**不需要为 CI 新增任何生产代码**。
> 否决 `controlplane/cmd/migrate`：新增的 0% 覆盖 main 包会进覆盖率分母，在一个「是否够 80%」的刀里自己挖坑。
> 否决 `psql -f migrations/*.up.sql`：不写 `schema_migrations` 记账，之后任何 `Migrate()` 都会从 0 重放 → 红。

### 三层防静默跳过（机制在脚本，名单在 conf）
1. **正向点名**：`grep -qE "^--- PASS: ${t} \("`，覆盖 14 个 PG 测试 + 1 个 tag-only 测试。
   用 `-v` 文本输出而非 `-json`（与 `ci-agent.yml` 同构、本地无 jq 依赖，且顶层 `--- PASS:` 在第 0 列、
   子测试缩进 4 空格，天然就是「只管顶层」的粒度）。
2. **语义标记**：`REQUIRED_LOG` 断言 `MUTATION | A1 | …` 那行存在；`REQUIRED_LOG_COUNT` 要求 8 行 mutant
   结果都在，证明 8×6 矩阵整个跑完而非刚进函数就返回。
3. **skip 账本**：未登记的 skip 一律红。`ALLOWED_SKIP` 里每条须写明为什么跳、**什么条件下恢复**
   （三个 `*_LIVE` + 两个 STS）。这层的价值是让「CI 今天还跑不到什么」变成必须被评审的文件。
   已知局限：子测试级 skip 不入账本，兜底是第 1 层的点名。

### `sts` 门控为何不是「把失败降级成 skip」
那个硬失败从未保护任何东西——`-tags=integration` 在 CI 里一次都没跑过，它只是让整个 tag 无法运行的石头。
新的 skip **被账本管着**（必须登记 + 写明恢复条件），旧状态是「看不见的阻塞」，新状态是「看得见的欠债」。
相比包白名单（排掉且无记录、会腐烂），这是严格更强的。

## PR 2：webui 侧（缺口 3 + 5）

### 前置：Node 版本单一真相源（缺口 5 是 vitest 门的前置）
不先钉 Node，新门会因版本漂移而假红假绿（那 15 个假红就是证据）。
1. 新增 `webui/.nvmrc` = `24`（与 `engines` 的 `>=24.0.0 <25.0.0` 同语义）。放 `webui/` 而非仓库根，
   与 `cache-dependency-path: webui/pnpm-lock.yaml` 一致。
2. 新增 `webui/.npmrc` = `engine-strict=true` —— pnpm 在版本不符时**拒绝安装**而非仅警告。
   **这就是 ~~AUD-12~~ 撤回理由点出的缺环。**
3. CI 消硬编码（`ci-smoke.yml`、`build-webui.yml`、新 `ci-webui.yml`）：`setup-node` 改
   `node-version-file: webui/.nvmrc`；`pnpm/action-setup` 删掉 `version: '11'`、改从 `packageManager` 读
   （`package_json_file: webui/package.json`）。⚠️ **`pnpm/action-setup` 必须在 `setup-node` 之前**，
   否则 `cache: pnpm` 找不到 pnpm。
4. **拆除 Corepack 叙事**：重写 `webui/README.md`「版本管理」节与「启用 Corepack」步骤；
   `README.md:173` 去掉「通过 Corepack 管理」；`CLAUDE.md:119-120` 改成**指向**而不自带数字；
   `DECISIONS.md` 保留原决策与理由，但加一条「落地记录」说明实际机制是
   `.nvmrc` + `engine-strict` + `packageManager`、**Corepack 未启用**。
5. 收敛结果：7 处 → **2 个真相源**（`.nvmrc` 管 Node、`packageManager` 管 pnpm）。

### vitest PR 门
- **新建 `.github/workflows/ci-webui.yml`**，不塞进 `ci-smoke.yml` 的 `build-webui`：与
  `ci-cp`/`ci-agent`/`ci-proto` 一个模块一条的模式一致，且 paths 只过滤 `webui/**` + self
  （`ci-smoke.yml` 的 paths 宽到 `agent/** controlplane/** api/** …`，改 Go 代码会白跑前端测试）。
  `build-webui` 的职责是产出 dist artifact，不动它的职责（只改 Node 钉法）。
- 跑 `pnpm test`；另加 `pnpm test:coverage` **只打印不判阈**。
- **防静默跳过用正向下界**：从输出 grep `Test Files  <N> passed` / `Tests  <M> passed`，
  断言 N ≥ 22 且 M ≥ 176（当前实测），低于即 `::error::`。理由同 CP 侧。

## 收尾（缺口 6，用户操作）

本刀最后一步由用户开启 `master` 分支保护并把闸门登记为 required check
（至少 `codegen-drift + vet + test`、新 `ci-webui`）。
⚠️ **现在改 job `name:` 是免费的**（没有 required check 按名字登记），开保护之后就不免费——
所以 `ci-cp.yml` 的 job 名保持原样，尽管它已名不副实。
验证：`gh api repos/:owner/:repo/branches/master/protection` 不再 404，
且 `required_status_checks.contexts` 含该 job 名。

## 验证

### 正向对照（用 AUD-8 的尺子）
闸门每次**同时打印两个数**：排除生成代码的闸门数 + `go tool cover -func | tail -1` 的含生成代码对照数。
后者与 AUD-8 同量纲：若 CI 打出的对照数落回 79.x，说明 PG 或 `TEST_DATABASE_URL` 掉了。
带 tag + PG + 设变量应 **> 80.1%**。这不是「CI 绿了所以没问题」，而是**每次都把可判别的数字打出来**，
且与 14 条 `REQUIRED_PASS` 构成两个独立信号。

### 变异矩阵（照 PR #122 的做法，逐条跑并把 RED/GREEN 表贴进 PR）
| 变异 | 期望 | 证明什么 |
|---|---|---|
| M0 基线 | GREEN | 下面各条的对照 |
| M1 `unset TEST_DATABASE_URL` | RED（点名 `TestObservationMutationMatrix`+`TestDeadLetterUpsertMutation`） | 那个变量是承重的——P1 的核心 |
| M2 删掉起 PG 步骤 | RED（setup 步就报「PG 没起来」） | PG 承重，且失败信息一步到位 |
| M3 DSN 库名改成不存在的 | RED | 库名/迁移一致性有人守 |
| M4 删掉 `SETUP_TEST` | RED（点名 `TestBatchTagQueries_Integration`） | 文件名排序那个坑真实存在 |
| M5 把某个 required 测试改名（不改 conf） | RED | **正向断言能抓「改名/删除」，负向断言抓不到** |
| M6 新加一个只有 `t.Skip` 的测试 | RED（`undeclared skip`） | 账本真的在管新 skip |
| M7 改掉 `t.Log` 的标记文案 | RED | 语义标记断言非哑（对文案漂移敏感是**有意的**） |
| M8 `-run` 只跑一个 mutant | RED（`REQUIRED_LOG_COUNT`） | 8×6 矩阵真跑完了 |
| M9 注释掉一个有覆盖的手写测试 | RED（coverage < 门槛） | 覆盖率闸真在比，不是打印 |
| M10 新建假生成文件（头部写 `// Code generated`）塞 200 行未覆盖语句 | RED（`EXPECT_GENERATED_FILES` 15≠14）**且百分比不动** | 排除按头部注释生效；没人能靠「声明自己是生成代码」偷偷放水 |
| M11 删掉 `deadletter.sql.go` 的头部注释 | RED（14→13）**且百分比下降** | 判据不是文件名 glob |
| M12 `COVERAGE_MIN` +0.5 | RED | 门槛值承重 |

M1/M2/M3/M12 只改 workflow/conf 可直接在 CI 验；M4–M11 本地 `make test-gate-controlplane` 即可。

### 落地顺序（每步可独立验证）
0. **先量基线**（不改文件）：四种配置各跑一次，前三个应复现 AUD-8 的 79.4/79.9/80.1；对不上先查环境。
1. `cover-percent.sh` → 拿步骤 0 的 profile 验：应打印 4 个数、清单正好 14 行且含 `models.go`/`db.go`。
2. `sts` 加门控 → 无 MinIO 时 `go test -tags=integration ./internal/storage` 从**红**变**skip**。
3. `test-gate.sh` + conf（`COVERAGE_MIN` 先填 0）→ PG 起着时全绿；停掉 PG 应在 setup 步就明确报错。
4. `.gitignore` + Makefile → 跑完闸门后 `git status --porcelain` 为空（drift guard 不误报的前提）。
5. **定门槛**：读步骤 3 的数 → ≥80 填 `80.0`；<80 填实测值并在 conf 注释+PR 写清差多少。
6. 改 `ci-cp.yml` → draft PR 看 CI 日志。
7. 跑变异矩阵，表贴进 PR。
8. 记账：backlog 勾掉第 1、2 条；新立三条（见下）。
9. **用户操作**：开分支保护 + 设 required check。

## 必须在 PR 描述里写明的三件事（否则等于用「已落地」掩盖「没落地」）
1. **覆盖率口径变了**：门槛比的是「带 `-tags=integration` + 排除生成代码」，与 backlog 记的
   79.9%/81.0% **不是同一把尺子**。不写清，三个月后会有人拿两个不可比的数字吵一轮——
   而这正是 backlog 第 2 条记的原病。
2. **CLAUDE.md 的「核心业务逻辑 ≥ 90%」仍未执行**：本刀只落「整体 80%」。CLAUDE.md 没定义
   「核心业务逻辑」是哪些包，硬猜清单是替产品做决定。闸门会顺手打一张 per-package 表（只报告不闸）
   给将来定清单的人用。
3. **绿灯何时开始被要求**：步骤 9 完成前，以上全部只是「跑了」不是「挡住了」。

## 新立 backlog 三条
- 需要一份「核心业务逻辑」包清单，才能执行 CLAUDE.md 的 ≥ 90%
- CI 接 MinIO（解封 `STS_LIVE` 两个测试 + agent 的 `uploader_integration_test.go`）
- 棘轮的机械保障：CI 比对 `git show master:ci/gate-*.conf`，只允许 `>=`（对 agent/webui 同样适用）

## 风险（按翻车概率排序）
1. **`git status --porcelain` 误报漂移** —— 产物必须全进 `/.ci-out/`，且 drift guard 排在测试之前。
   `*.out` 已被忽略但 `test.log` 与目录本身不是。本地跑完闸门再 `make generate` 最容易撞上。
2. **改 job `name:` 会打掉 required check** —— 步骤 9 之后不再免费。保持原名是故意的。
3. **共享 PG 上的跨包 flake** —— golang-migrate 的 advisory lock、`CleanupStaleWebhookFailCounters`
   的全表删除、残留行。`GO_TEST_P=1` 是先手防御；若仍 flake，下一步把三个 webhook 文件也改成
   「临时 schema + rollback」（`deadletter`/`ingest` 是现成样板）。
   **闸门 flake 比闸门慢危险得多**——会随机红的闸门三天内就会被人加 `continue-on-error`。
4. **`-tags=integration` 在 CI 里第一次被编译** —— 今天没有任何东西 vet 过那 9 个文件，
   第一版 PR 可能先红一轮。`go vet -tags=integration` 放最前面就是让它几秒内红而不是等 20 分钟。
5. **runner 的 5432 端口** —— `ubuntu-latest` 预装 PostgreSQL（默认未启动）。真撞上是 compose bind 失败（响的）。
6. **bash 3.2**（macOS 自带）—— 无 `readarray`、`set -u` 下展开空数组报错、
   `[ … ] && cmd` 作独立语句在 `set -e` 下会误退出。全部空数组先判 `${#arr[@]}`、条件一律 `if/then`。
7. **`REQUIRED_PASS`/`ALLOWED_SKIP` 会腐烂** —— 测试改名须同改 conf。这是**有意的摩擦**，
   但要在 conf 头部写清，否则第一个撞上的人会以为脚本坏了。
8. **子测试级 skip 不入账本** —— 把测试体搬进会 skip 的 `t.Run` 可绕过第 3 层；兜底是第 1、2 层。

---

# 派工与编排

## 任务单元前缀：`QG-x`（Quality Gate）

**定前缀前已 grep 过全仓库**（惯例：发明任务/缺陷前缀前必须先查冲突。
教训来源：曾拍脑袋起 `CI-1…CI-14`，与 Continuous Integration 直接撞车）。

现有前缀占用（真实，非记忆）：`IC`(688) `BUG`(529) `T3`(274) `CC`(140) `MT`(90) `WR`(80)
`SEC`(55) `AUD`(44) `T4`(36) `T0`(7)。

选 `QG-`，否决其它候选的理由：
- ❌ `CI-` —— 已稳定表示 Continuous Integration（本仓 `.github/workflows/ci-*.yml` 与
  CLAUDE.md 多处），不可作任务前缀
- ❌ `GATE-` —— ID 形式虽 0 占用，但 `GATE` 作为**普通词**在仓库出现 **88 处**
  （`gate open`、`双闸`、`fail-closed gate`…），`grep GATE` 会淹在噪声里 → **失去定位性**，
  与 `CI-` 同型的坑
- ❌ 裸 `W1/W2/…` —— 无前缀、项目内无唯一性无法定位，且与已占用的 `WR-x`（Web UI 重做）视觉混淆
- ✅ `QG-` —— ID 形式 0 占用，作为独立词 **0 处**，2 字母与 `IC`/`CC`/`MT`/`WR` 风格一致，
  语义覆盖全部单元（测试真跑、覆盖率、前端门、版本钉、分支保护都是质量闸门）

编号一律 `QG-<数字>`，缺陷沿用既有构词 `QG-BUG-<编号>`。

## 执行流程

```
QG-0  docs-only PR（落地本文件 + active.md 指针）     ← ✅ 已完成
        │
        ├─ PR 1（CP）：QG-1 ─→ QG-3 ─→ QG-4        QG-2 可与 QG-1 并行
        │                │
        │        协调者：步骤 0 量基线 → 步骤 5 定门槛 → 变异矩阵 M1–M12
        │
        └─ PR 2（webui）：QG-5 ─→ QG-6           与 PR 1 完全并行
                │
        协调者：cherry-pick / 推送 / PR 描述 / 每轮 codex 复审
                │
QG-7  用户：开 master 分支保护 + 设 required check      ← 最后一步
```

| ID | 内容 | 谁做 |
|---|---|---|
| ~~**QG-0**~~ | 落地本文件 + `active.md` 指针（docs-only PR） | ✅ 已完成 |
| **QG-1** | `scripts/ci/cover-percent.sh` | opencode |
| **QG-2** | `sts` 门控 + 删空壳测试 | opencode |
| **QG-3** | `scripts/ci/test-gate.sh` + `ci/gate-controlplane.conf` | opencode |
| **QG-4** | `ci-cp.yml` + `Makefile` + `.gitignore` | opencode |
| **QG-5** | webui Node 钉法 + `ci-webui.yml` | opencode |
| **QG-6** | 拆除 Corepack 叙事（4 处文档） | opencode |
| **QG-7** | `master` 分支保护 + required check | **用户** |

## 不可外包（协调者必须自己做）

| 事 | 为什么不能派 |
|---|---|
| 步骤 0 量基线、步骤 5 定门槛 | 要读实测数字做判断；`active.md` 明令不许为凑数写测试，worker 会倾向于「把数字弄绿」 |
| 变异矩阵 M1–M12 | **PR #122 的教训**：worker 只变异了它自己想到的那一个分支，漏掉另一分支与抗漂移主张本身。协调者必须自跑全矩阵 |
| cherry-pick / 推送 / PR 描述 | 分支与 PR 归协调者；worker 只提交到自己的一次性分支 |
| 每轮 codex 复审的派发与裁决 | 评审意见要逐条判「对→改 / 错→反驳 / 半对→修正」，不能盲从 |
| master 分支保护 | repo 设置，用户操作 |

## 派工 spec（可直接投喂 worker；每份都要自带前置与验收）

**所有 spec 的公共约束**（写进每一份）：
- 先 `export TMPDIR="$PWD/bin"`（`bin/` 已 gitignore）；**禁止覆盖 `GOCACHE`**；不在 worktree 外写东西
- 只改 spec 列出的文件；`git add` 只加实际改的文件，**禁止 `git add -A`**
- 提交到自己当前分支，`<type>(<scope>): <subject>` 格式，**不要 push、不要碰 PR**，报 commit SHA
- 规范见仓库根 `CLAUDE.md`
- 若认为 spec 的改法有错，**不要照抄**——说明理由并给出你的方案

### QG-1 `scripts/ci/cover-percent.sh`（无依赖）
**Change**：新建脚本，算「排除生成代码后」的加权语句覆盖率。
用法 `cover-percent.sh <module-dir> <profile> [<genlist-out>]`，stdout 一行四个数
`<pct> <covered> <total> <genfiles>`。
**要点**（都是坑，逐条核实）：判据是文件头 `^// Code generated .* DO NOT EDIT\.$`
且只看 **package 子句之前**（Go 官方那条正则），**不是** `*.sql.go` glob；
profile 行 `<import-path>/<file>.go:<s>.<c>,<e>.<c> <numStmts> <count>`，
**以 `file:range` 整体为 key 去重**（go test 拼接各包 fragment，同一块可能多行，naive 累加会算重分母）；
模块路径读 `go.mod` 的 `module` 行（**不要 `go list -m`**，go.work 下会打印所有 workspace 模块）；
awk 先 `sub(/\r$/,"")`（CRLF）；`END` 里**重读** genlist 取总数（零语句的 `models.go` 也要算）。
**Acceptance**：① 拿一份现成 profile 跑，`genfiles` = **14**，清单全在 `controlplane/internal/db/`
且**含 `models.go` 与 `db.go`**；② 临时删掉 `deadletter.sql.go` 的头部注释 → `genfiles` 变 **13**
且百分比**下降**（证明判据认注释不认文件名），改回后恢复；③ bash 3.2 兼容（macOS 自带）。

### QG-2 `sts` 门控（无依赖，可与 QG-1 并行）
**Change**：`controlplane/internal/storage/sts_integration_test.go` 加
`requireLiveMinIO(t)`（`os.Getenv("STS_LIVE") != "1"` 则 `t.Skip`，文案写明恢复条件与 D-036），
在 `TestIssueCredentials_Integration` 与 `TestIssueCredentials_ScopingDeniesOtherBuckets`
开头各调一次；**顺手删掉** `sts_test.go:105` 只有一句 `t.Skip` 的空壳
`TestSTSManager_IssueCredentials_NoMinIO`。
**不要**把门控藏进 `newRootClient` helper。
**Acceptance**：无 MinIO 时 `go test -tags=integration ./internal/storage` 从**红**变**skip**；
`GOOS` 不变、`go vet -tags=integration ./internal/storage` 通过。

### QG-3 `scripts/ci/test-gate.sh` + `ci/gate-controlplane.conf`（依赖 QG-1）
**Change**：驱动脚本 + 策略 conf。顺序：`go vet`（含 tag 再 vet 一遍）→ `SETUP_TEST` 先行迁移
→ 全量 `go test -v -covermode=atomic -coverprofile` → 三层断言 → 覆盖率闸。
**机制在脚本、策略在 conf，不要互串。**
三层断言与 conf 字段见上文「三层防静默跳过」。`COVERAGE_MIN` **先填 0**（数字由协调者定）。
产物落 `.ci-out/<module>/`。尽量把所有问题一次报完（「红了看不出为什么」等于没红）。
**Acceptance**：PG 起着时 `make test-gate-controlplane` 全绿且打印 14 条 `ok` + 5 条
`ok(declared)`；**停掉 PG 应在 setup 步就明确报「PG 没起来，先跑 compose」**，而不是刷 14 条红；
bash 3.2 兼容（空数组先判 `${#arr[@]}`、条件一律 `if/then`）。

### QG-4 `ci-cp.yml` + `Makefile` + `.gitignore`（依赖 QG-3）
**Change**：见上文「新增/修改文件」表对应三行。用 compose 起 PG（**不用 `services:`**），
只点名 `postgres`；`permissions: contents: read`（不需要 `packages: read`）。
⚠️ **job `name:` 保持原样不改**（分支保护开启后按名字登记 required check）。
⚠️ **`.gitignore` 加 `/.ci-out/`，且 drift guard 步骤必须排在测试之前**——否则
`git status --porcelain` 把 coverage.out 当漂移报红。
**Acceptance**：跑完闸门后 `git status --porcelain` 为空；draft PR 的 CI 日志含
14 条 `ok`、`MUTATION | A1 …` 那张表、两个覆盖率数字。

### QG-5 webui Node 钉法 + `ci-webui.yml`（与 CP 完全独立，可并行）
**Change**：新增 `webui/.nvmrc`(=`24`)、`webui/.npmrc`(=`engine-strict=true`)、
`.github/workflows/ci-webui.yml`（paths 只 `webui/**` + self）；改 `ci-smoke.yml` 与
`build-webui.yml` 的 Node/pnpm 钉法（`node-version-file` + `packageManager`，删掉硬编码与浮动版本）。
⚠️ **`pnpm/action-setup` 必须在 `setup-node` 之前**。
防静默跳过用正向下界：grep `Test Files  <N> passed` / `Tests  <M> passed`，断言 N ≥ 22 且 M ≥ 176。
`pnpm test:coverage` **只打印不判阈**。
**Acceptance**：本机 `cd webui && pnpm test` 在 Node 24 下 22 文件/176 用例全绿；
故意切到 Node 26 时 `pnpm install` 因 `engine-strict` **拒绝**（这是 `engine-strict` 生效的证据）。

### QG-6 拆除 Corepack 叙事（依赖 QG-5）
**Change**：`webui/README.md`「版本管理」节 + 「启用 Corepack」步骤重写；`README.md:173` 去掉
「通过 Corepack 管理」；`CLAUDE.md:119-120` 改成指向 `webui/.nvmrc` 与 `packageManager`、
不再自带数字；`DECISIONS.md` 对应决策**保留原决策与理由**，加一条「落地记录」说明实际机制是
`.nvmrc` + `engine-strict` + `packageManager`、**Corepack 未启用**。
**Acceptance**：全仓库 `grep -rn corepack --include='*.md'` 的每一处都与实际机制一致；
「Node 24」的数字只剩 `webui/.nvmrc` 一处真相源（其余全是指向）。

## 编排踩坑（2026-09-26 实测，供派工时参考）

- **opencode 的 `worker-start` preamble 必然投递失败**：receipt 报
  `turnStart: unsupported` / `provider: unsupported`，终端输入框是空的。
  恢复路径（协议内）：`worker-abandon <旧 dispatch>`（它「不动进程、不动文件系统」，**保留终端与 worktree**）
  → `task-create` 新 Task → `dispatch --task <新> --to <terminal> --return-preamble`
  （**不带** `--inject`、**不带** `--dry-run`）→ 写进 worker worktree 的 `bin/DISPATCH.md`
  （**需先 `mkdir -p`**，`bin/` 不在仓库树里）→ `orca terminal send` 一条**单行**消息让它 `cat`。
- **该 preamble 用 `--from <terminal> --task-id --dispatch-id` 形式、不带 capability token，
  但鉴权有效**（实测心跳到达协调者收件箱）。注意 `--dry-run` 返回的是**占位** preamble
  （`ctx_dryrun`、无 capability），worker 照它做会导致心跳与 `worker_done` 全被拒。
- **终端已有 active dispatch 时 `dispatch` 会被拒**（`already has an active dispatch`）→ 先 abandon。
- **验证「提交成功」必须用增量判定**：`accepted: true` 与 `bytesWritten` 都不证明提交。
  看 agent 产生的标记数是否增加（`Thought:` / `Read <file>` / 工具调用行）。
- **心跳会占 FIFO 队首**，导致 `check --wait --types worker_done,...` 空转 → 必须 ack 才继续。
- **只能有一个 waiter**（第二个返回 `waitInterrupted: waiter_exists`）。
- **每轮返工都要 `task-create` 新 Task**（已 dispatch 的 Task 再 start 报 `task_not_startable`）。
- **`worker-release` 对 retained worker 返回 `state: retained` 且不删资源** →
  物理清理用 `orca worktree rm --worktree path:<abs>`（它会连带关终端），
  再 `git branch -D` 删一次性分支（squash 合并后 git 不认它们已合并）。
- 本轮实测：codex = GPT-5.6-Sol，opencode = GLM-5.3-Flash（Zhipu）。
- **评审节奏**：每轮返工后必须再评审（PR #122 第 3 轮就在第 2 轮的返工里找出新引入的盲区）；
  spec 里要明确问「是否已收敛、有没有必须合并前修的」，否则评审会一直挤出主观 nit。
