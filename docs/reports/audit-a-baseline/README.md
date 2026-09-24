# A 基线审计 — 结论报告

> **执行**：2026-09-24，独立新会话 ｜ **章程**：[`docs/tasks/audit-a-baseline.md`](../../tasks/audit-a-baseline.md)
> **环境**：dev（compose volume 从零创建，走了一遍首次运行路径）
> **⚠️ 事后修正**：当时以为这是「全新环境」，其实**只有卷是全新的，镜像是本机缓存的**。
> 这个区别不是细节——见 §3 的 G-A2：MinIO 镜像早已从 Docker Hub 下架，
> 真正的全新机器**在第一步就起不来**。本审计自己被缓存骗过去了。
> **机械层脚本**：与本文同目录的 `m*.sh`，可复跑

---

## 0. 一句话结论

**目标 A 的十二环，在 dev 上全部实跑通过了。** 一台 agent + CP + Web UI，配一条采集规则，
文件采上去、索引里查得到、能下载——**每一环都有命令与输出为证**（§1）。

这与立项背景（2026-09-08「Agent 数据面从未端到端跑通过」）并不矛盾：那之后的 IC-1…IC-4a
把主路径修通了。

> **⚠️ 一条重要限定（事后补）**：上面这句「跑通了」成立的前提是**机器上已经缓存了
> MinIO 镜像**。该镜像此后已从 Docker Hub 下架，所以在一台真正干净的机器上，
> 十二环在第一步就起不来（G-A2）。**本审计自己没发现这一点**，是被本机缓存骗过去的。
> 两条挡 A 项均已随 PR #114 修复。

本次审计的价值因此不在"发现链路是断的"，而在两件事：

1. **首次运行路径是断的**（§3，挡 A 两条之一）——照着 `CLAUDE.md` 走，新环境起不来。
   三处文档错误，修复成本都是改几行字。（另一条 G-A2 是审计**漏掉**的，见 §3。）
2. **默认采集模式有 150 倍写放大**（AUD-9，§2）——A 的三个标准都达成，但这是上线前
   必须处理的一条，且**能力已存在**，只是默认值选错了。

另外确认一件对"以后怎么防"更重要的事：**CI 目前不提供任何服务容器，也从不跑
`-tags=integration`**，所以 **11 个** `//go:build integration` 文件在 CI 里一次都没跑过；
另有 **4 个**非 tag 但依赖 PostgreSQL 的测试文件，在 CI 里以 `ok`（SKIP）的形式**绿着**（AUD-10）。

---

## 1. 逐环判定（①–⑫）

全部 12 环判定为**能跑**，无「未验证」遗留。下表的"证据"列均为本次实跑所得。

| # | 环节 | 判定 | 证据 |
|---|------|------|------|
| ① | CP 启动 | ✅ 能跑（**两处限定见下**） | 迁移自动应用 `version:10`；`/healthz` 200 |
| ② | 管理员登录 | ✅ 能跑（**需 `make bundle`**） | REST 200 + 浏览器实登进 dashboard |
| ③ | 注册→审批→连接 | ✅ 能跑 | `pending` → approve 200 → `stream established` → 心跳 |
| ④ | STS 凭据 | ✅ 能跑 | `STS credentials issued`，1h 有效期 |
| ⑤ | 建 bucket | ✅ 能跑（**无事件订阅**） | API 201 → MinIO 真建出 → 规则可用 → 索引 `source=agent` |
| ⑥ | 建采集规则 | ✅ 能跑 | 201，6 个必填项，`mode` 仅收 watch/scheduled |
| ⑦ | 规则下发 | ✅ 能跑 | 建规则即推；CP 重启后 3 条规则全部重新同步 |
| ⑧ | 监听/发现 | ✅ 能跑 | fsnotify + 启动扫描：**规则下发前已存在的文件会被采** |
| ⑨ | 直传 MinIO | ✅ 能跑（**默认模式重度放大**） | 74B 单次；150MB 走 multipart 成功 |
| ⑩ | 上报→索引 | ✅ 能跑 | `upload result received` → `file indexed` |
| ⑪ | 列表/查询 | ✅ 能跑 | cursor 信封 + 分页 + 筛选；API 与 UI 两条路都验 |
| ⑫ | 下载 | ✅ 能跑 | 预签名 GET 200，**sha256 与源文件逐字节一致** |

### 关键证据摘录

**⑫ 端到端一致性**（最硬的一条）：

```
源文件 sha256:              1f16b00bc49e609383bd131260b375827dd0ae49c4f7a7dd958fdf2298f4ab6a
file_entries.sha256:        1f16b00bc49e609383bd131260b375827dd0ae49c4f7a7dd958fdf2298f4ab6a
预签名下载回来的 sha256:     1f16b00bc49e609383bd131260b375827dd0ae49c4f7a7dd958fdf2298f4ab6a
```

150MB 文件同样三者一致（`12ba5784…5415`）。

**① 四依赖缺失时的行为**（`m-deps.sh` 可复跑）：

| 依赖缺失 | 行为 |
|---|---|
| PostgreSQL | **fail-fast**，`fatal: database migration failed` |
| Redis | **fail-fast**，`fatal: redis connection failed` |
| NATS | **fail-fast**，`fatal: NATS connection failed` |
| **MinIO** | ⚠️ **不 fail-fast**：CP 正常启动，且 `/healthz` 返回 **200** |

MinIO 那一行与 `docs/ops/deployment.md` §0 的「任一依赖不可达即退出」直接冲突（AUD-4）。
原因在代码里很清楚：`miniogo.New` 只构造客户端、不拨号，失败推迟到首次 STS/presign/建桶。

> **⚠️ 第二处限定（事后补）**：上表的前提是「四个依赖能起来」。而在一台**没有镜像缓存**的
> 机器上，MinIO 容器**根本拉不起来**（G-A2）——所以 ① 在真正的全新环境上其实是 ❌。
> 本审计当时没发现，正是因为开发机缓存了镜像。

**⑩ `file_entries` 实际填充的字段**：

- 已填：`org_id` `agent_id` `rule_id` `bucket_id` `storage_path` `original_path`
  `file_name` `size_bytes` `sha256` `etag` `file_mtime` `status=completed`
  `uploaded_at` `observed_at` `source=agent`
- **未填**：`file_type_id`（NULL）、`content_type`（空）、`event_seq`（空）

---

## 2. 声称漂移清单

每条都带**三分类**。按章程纪律 7，拿不准一律归「文档撒谎」或「本是计划」，
**不默认归「代码欠债」**；本清单**没有任何一条**被判为代码欠债——
无一条能给出「曾被明确承诺」的证据。共 14 条。

| ID | 出处 | 实际情况 | 分类 | 严重度 |
|----|------|---------|------|--------|
| **AUD-1** | `CLAUDE.md:273`、`README.md:214` 直接 `bash init-minio.sh` | 脚本默认 `WEBHOOK_ENDPOINT=http://controlplane:8080/...`，dev compose 里**没有** `controlplane` 服务；MinIO 在 config-set 时**真的去拨号**，解析失败 → 脚本 `exit 1`，第 6/7 步（事件订阅 + 三项自检）全部未执行 | 文档撒谎 | 🔴 高 |
| **AUD-2** | `CLAUDE.md:268-270` `migrate -database … -path ./migrations up` | D-023 起迁移已内嵌，启动自动应用（实测 `version:10`）。该步骤已无必要，且 `migrate` CLI 未必在机器上 | 文档撒谎 | 🟠 中 |
| **AUD-3** | `CLAUDE.md:266` dev 用 `make build` | `make build` 产出**纯 API 二进制**，`GET /` = **404**，没有 Web UI。A 要求 Web UI，须 `make bundle` / `-tags webui`。dev 章节从未提及 | 文档撒谎 | 🔴 高 |
| **AUD-4** | `docs/ops/deployment.md:12` 「任一依赖不可达即退出（无重试循环）」，依赖表含 MinIO | 对 PG/Redis/NATS 成立；**对 MinIO 不成立**——CP 照常启动且 `/healthz` 200 | 文档撒谎 | 🟠 中 |
| **AUD-5** | `deploy/config/controlplane.env:2` 「由 `deploy/scripts/start-controlplane.sh` 加载」 | `deploy/scripts/` 下**只有** `init-minio.sh`，该脚本不存在 | 文档撒谎 | 🟢 低 |
| **AUD-6** | `docs/ops/operations.md` §1「CP 配置参考」 | 漏了代码实读的 `WEBHOOK_FAIL_LIMIT`、`WEBHOOK_MAX_PARSE_BYTES`（两者均有启动校验与安全下限） | 文档撒谎（不完整） | 🟢 低 |
| **AUD-7** | `controlplane/internal/api/handler/webhook_r2_red_test.go:43` 「covered in `redis_fail_store_r2_test.go`」 | 该文件**全仓库不存在**。真实覆盖在 `webhook_r3_red_test.go` 等处 | 文档撒谎 | 🟢 低 |
| **AUD-8** | `CLAUDE.md` 覆盖率 Go ≥80%、核心 ≥90%、webui ≥80% | **无任何执行者**（CI/Makefile 零处）。实测 CP **79.9%**、agent **75.4%**，均低于声称门槛 | 文档撒谎（门槛无执行力） | 🟠 中 |
| **AUD-9** | `agent/internal/watcher/watcher.go:85` 「overwrite（default mode）」 | 描述属实，但后果未被任何文档写明——见 §3 | 本是计划 | — |
| **AUD-10** | CI 绿 | `ci-cp.yml` / `ci-agent.yml` 跑 `go test ./...`，**不带 `-tags=integration`**，且**全仓库无一个 workflow 有 `services:` 块**。→ **11 个** `//go:build integration` 文件从未在 CI 跑过；另 **4 个**非 tag 但依赖 PG 的测试文件在 CI 里 **SKIP 成绿色**（实测：`TEST_DATABASE_URL` 指向不可达 DSN 时 `--- SKIP` 且包级结果仍是 `ok`）。**唯一的反例是好的**：`linux && overflow` 那个文件在 `ci-agent.yml` 里真跑，并用 grep 断言「没被静默跳过」 | 文档撒谎（"CI 绿"的含义被高估） | 🔴 高 |
| **AUD-11** | `webui` 22 个 vitest 文件 + `pnpm test` | **没有任何 CI workflow 运行它们**（`build-webui.yml` 是 `workflow_dispatch` 且只 build） | 文档撒谎 | 🟠 中 |
| **AUD-12** | `CLAUDE.md` Node **24**（`>=24.0.0 <25.0.0`） | 本机 Node **26.9.0**，`make build-webui` 照常成功（`BUILD_EXIT=0`）——约束未被执行 | 文档撒谎（约束无执行力） | 🟢 低 |
| **AUD-13** | `WEBHOOK_QUEUE_DIR=/data/minio-webhook-queue` | `/data` 是 MinIO 的存储根，该队列目录被 **`mc ls` 当作 bucket 列出**（与 data-sensor 并列）。CP 的 bucket API 读 PG，故未污染 UI | 文档撒谎（副作用未记） | 🟢 低 |
| **AUD-14** | `docs/tasks/bugs/open.md:237` IC-BUG-7 的「后果」行：「在当前架构下（**webhook 是唯一写入路径**），通过 Web UI 创建的任何 bucket，其文件都**永远不会进入索引**」 | **前提已被 IC-2a 推翻**。实测：在 API 新建的桶上采集一个文件，`file_entries` 出现该行且 `source=agent`——agent 的 `UploadResult` 才是主写入路径，与 bucket 是否订阅 webhook 无关。卡片**标题仍成立**（确实没注册通知），但**后果与 🟠 P1 定级已不成立** | 文档撒谎（缺陷卡的事实断言过期） | 🟠 中 |

### 校准集复核（章程 §6）

四条全部**独立重新发现或确认已修**，机械层脚本确实能捞出这一类：

| # | 校准项 | 复核结果 |
|---|--------|---------|
| 1 | `WEBHOOK_MAX_PARSE_BYTES` 声称可配 | ✅ **已修**：config → router → `h.parseCap` → `readBodyCapped` 全程贯通，启动日志实打 `parse_cap:8388608` |
| 2 | 某迁移 cleanup 声称 periodic | ✅ **已修**：`000009` 点名 `handler.RunStaleCounterCleanup`，`main.go:262` 确有 `go … time.Hour` 调用 |
| 3 | `readBodyCapped` 注释声称 hash 基于完整 body | ✅ **已修**：`io.Copy(io.Discard, tee)` 把超出 cap 的部分也喂给 hasher |
| 4 | IC-BUG-7 卡片前提 | ✅ **独立重新发现，且确认前提确已被推翻**：API 建的桶确实没有 webhook 订阅（标题成立），但卡片「文件永远不会进入索引」的后果**实测为假**——见 AUD-14 |

方法有效性的额外证据：同类问题**新捞到** AUD-7（注释点名了不存在的测试文件），
说明机械层不是只会复述已知结论。

---

## 3. 缺口清单（挡 A / 不挡 A）

> 按 A 的标尺重新二分，**未沿用** 2026-09-11 的旧口径。

### 🔴 挡 A（2 条，均已随 PR #114 修复）

**G-A1 — 首次运行路径跑不通**（= AUD-1 + AUD-2 + AUD-3）✅ 已修（#114）

A 是「一个**可运行**的版本」。照 `CLAUDE.md` 的 dev 章节逐条执行，新环境**起不来**：
`init-minio.sh` exit 1（webhook 端点不可达）、`migrate` 步骤已废、`make build` 没有 Web UI。
本次审计是靠三处绕行才把环境拉起来的。

**为什么仍判挡 A**：A 要求系统可被人跑起来；跑不起来的版本不算可运行。
**但修复成本极低**——三处都是改文档/改一个默认值，不需要写功能代码。

建议的最小修复：
1. `init-minio.sh` 的 `WEBHOOK_ENDPOINT` 默认值改为对 dev 可达的值，或在 dev 文档里写明必须覆盖，
   **并说明脚本必须在 CP 起来之后跑**（MinIO 会真的拨号校验）；
2. 删掉 `CLAUDE.md` 的 `migrate` 步骤；
3. dev 章节改用 `make bundle`，或写明 `make build` 不含 Web UI。

**G-A2 — MinIO 镜像已从 Docker Hub 下架，任何全新环境都起不来** ✅ 已修（#114）

**本审计漏掉了这一条**，是在 PR #114 的首次 CI 实跑上撞出来的：

```
docker: Error response from daemon: pull access denied for minio/minio,
repository does not exist or may require 'docker login'
```

`docker manifest inspect` 逐个确认（绕过本地缓存）：`minio/minio` 的所有 tag、
`minio/mc` 全部不可拉，而 `postgres:15-alpine` / `redis:7-alpine` / `nats:2-alpine` /
`alpine:3.20` 均正常——**不是限流、不是网络，是镜像确实没了**（MinIO 转向商用，
同时移除了 `dl.min.io` 的 mc CLI 下载）。

影响面：`docker-compose.dev.yml` / `prod.yml` / `test.yml` **三个都起不来**。
修复：registry 换 `quay.io/minio/minio`（仍可拉，含钉的那个 tag，镜像内容一致）。

> **⚠️ 这条对审计方法的教训比对代码的更重要。** 本报告 §0 曾自称在「全新 dev 环境」
> 上实跑——**卷确实是全新的，镜像不是**。审计因此把一台「镜像早已缓存」的机器
> 误当成了干净环境，于是整份报告的 ① 判定其实建立在一个不成立的前提上。
> 这正是报告自己在 §4 里写的那句话：**「在作者机器上能跑」证明不了首次运行路径成立**——
> 只不过这次被绊倒的是审计者本人。
>
> **可复用的判据**：验证「首次运行」时，`docker volume rm` 不够，还要
> `docker image rm`（或用一台从未拉过这些镜像的机器 / CI runner）。

### 🟡 不挡 A（记录归档，本轮不处理）

| # | 缺口 | 为什么不挡 A |
|---|------|-------------|
| 1 | **API 建的 bucket 没有 webhook 事件订阅**（IC-BUG-7 标题部分成立） | 实测：新桶上采集→上传→索引**全通**，`source=agent`。缺的只是**对账兜底**，不是主路径。对账本就不在 A 范围。⚠️ 该卡当前 🟠 P1 的定级建立在已被推翻的前提上（AUD-14），**建议重新定级** |
| 2 | **默认 `append_mode=overwrite` 无防抖**（见下） | A 的三条标准都达成了 |
| 3 | MinIO 缺失时 CP 不 fail-fast 且 `/healthz` 200（AUD-4） | 单机 dev/PoC 下 MinIO 总是在的；属健壮性 |
| 4 | `file_type_id` / `content_type` 从不填充 | A 不要求分类与 MIME |
| 5 | CI 从不跑 integration/服务依赖测试（AUD-10/11） | 不影响能否运行；但影响**将来别再退化**，见 §4 |
| 6 | 覆盖率门槛无执行力且未达标（AUD-8） | 同上 |
| 7 | `append_mode=tail` 已 fail-closed 挡掉（IC-BUG-46） | 已挡掉，不会静默丢数据；A 不需要 tail |

#### ⚠️ 不挡 A 但强烈建议随 A 一起修：默认采集模式的写放大

这是本次实跑撞见的最重的一条，**不在任何现有缺陷卡里**。

把一个 **150MB** 文件 `cp` 进被监听目录（默认 `append_mode=overwrite`）：

```
agent 日志 "uploading file" 命中 big.csv:   151 次
upload_logs 行数:                          150 行
  其中 bytes_transferred = 157286400 (完整):  147 行
  其中 bytes_transferred = 5MB / 6MB / 2MB:     3 行  ← 读到了正在写入的半个文件
MinIO CompleteMultipartUpload 事件:         150 次
实际写入流量:                               ≈ 22 GB（为了一个 150MB 的文件）
```

**根因**（`agent/internal/watcher/watcher.go:176-179`）：只有 `AppendModeCloseWait`
走 `runCloseWait`（500ms 空闲防抖）；**默认的 `overwrite` 走 `runFsnotify`，每个
Create/Write 事件直接触发一次整文件上传**。

**能力已经存在**——同一个 150MB 文件，改用 `append_mode=close_wait` 后：

```
upload_logs 行数: 1    （完整 157286400 字节，一次成功）
```

所以这**不是要新建功能**，是默认值的选择。两点风险：
- **写放大 150×**：边缘设备的上行带宽与 MinIO 的写入压力都会被放大两个数量级；
- **索引/对象不一致窗口**：有 3 次上传读到了部分写入的内容。本次最终结果是一致的
  （三个 sha256 全等），但"最后写对象的那次"与"最后上报索引的那次"并不保证是同一次上传，
  这个窗口是真实存在的。

`file_entries` 的 `(bucket_id, storage_path)` 唯一约束保证了最终只有 1 行索引——
这是它没有酿成脏数据的原因，**不是因为上传路径本身收敛**。

---

## 4. 建议：把机械层做成一道 CI 闸

章程 §12 说「本审计最有价值的产出可能不是清单，而是一道 CI 闸」。本轮的经验支持这个判断，
但**优先级排序和章程的设想不同**：

本轮 13 条漂移里，靠 grep 类脚本捞出来的多是低严重度的（AUD-5/6/7/12/13）；
真正重的三条——AUD-1（首次运行断）、AUD-3（默认构建没 UI）、AUD-9 的写放大——
**都是跑一遍才发现的，静态检查捞不到**。

因此建议按这个顺序，而不是先做声称扫描：

1. **（最高价值）一条真正的 e2e 冒烟 CI** ✅ **已落地（PR #114）**：起 compose →
   跑 `init-minio.sh` → 起 CP → 注册/审批一个 agent → 建规则 → 落一个文件 →
   断言 `file_entries` 有行 → 断言预签名下载的 sha256 与源文件一致。
   落地形态见 `deploy/scripts/smoke.sh` 与 `.github/workflows/ci-smoke.yml`。

   **它在首次 CI 实跑就兑现了价值**，揪出两个本地完全不可见的问题：
   `dl.min.io` 不再提供 mc 二进制（下载回来是公告文本）、以及 G-A2 的镜像下架。
   两者都因为开发机有缓存而在本地永远发现不了——**这条冒烟抓到的第一批问题，
   恰恰是本次人工审计漏掉的那一类**。
2. **给 CI 加服务容器 + 跑 `-tags=integration`**：否则现有那 15 个测试文件（11 tagged + 4 PG 依赖）永远是装饰。
   并照抄 `ci-agent.yml` 里 inotify overflow 那段的做法——**grep 日志确认测试真的跑了**，
   那是本仓库已有的、正确的防 silent-skip 写法。
3. **webui 的 PR 门**：22 个 vitest 文件目前完全没有执行者。
4. **（最后）声称扫描**：本目录的 `m*.sh` 可直接作为起点。建议只对
   **代码注释 + 迁移注释**强制（章程 §7 的高精度对象），**不要**对设计/规划文档强制——
   否则会把「本是计划」批量误判成缺陷，正是章程 §8 警告的那种假 backlog。

---

## 5. 机械层脚本（可复跑）

与本文同目录：

| 脚本 | 作用 |
|------|------|
| `m1-envvars.sh` | 被声称的环境变量 vs 代码实读 |
| `m1b-config-tables.sh` | `operations.md` 配置表 ↔ 代码（双向） |
| `m2-named-things.sh` | 文档里点名的文件路径是否存在 |
| `m2b-comment-refs.sh` | **代码/迁移注释**里点名的文件是否存在（高精度对象） |
| `m3-periodic.sh` | 「periodic/启动时/后台」声称是否有真实调度者 |
| `m6-test-gates.sh` | build tag + `t.Skip` 闸门 vs CI 实际提供的依赖 |
| `m-deps.sh` | 四依赖逐个停掉，观察 CP 启动行为 |

---

## 6. 纪律自查

- ✅ 12 环全部有带证据的判定，无「未验证」
- ✅ 机械层每类检查都有可复跑脚本
- ✅ 校准集四条全部独立重发现/确认已修，并新捞到同类的 AUD-7
- ✅ 每条缺口都打了挡 A / 不挡 A
- ✅ **审计只读**：未修改任何产品代码、配置或文档；dev 环境里创建的 bucket/规则/文件是实跑产物
- ⚠️ **本报告自身适用章程纪律 2**：下一位执行者不应把上表当基线，该复核的请复核
