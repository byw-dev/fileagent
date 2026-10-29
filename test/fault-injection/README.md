# B3 故障注入测试（filer 订阅链路）

> 迁自 PR #135（B3-PR3b）的故障注入脚本，原存于 gitignored 目录 `bin/b3-evidence/pr3b-fi/`，
> 现入库为 `test/fault-injection/`。脚本内 `F3/F6/F7/N4/N5/R3-x/R4-x` 等标签来自
> PR #135 的评审轮次，保留作历史锚点。

## 它验证什么

CP 的「订阅 SeaweedFS filer 元数据事件 → S3 HEAD 回读 → 写入 PG 账本」链路（D-037）
在各类故障下的**最终一致性**：故障注入结束后，S3 实际存活对象集合必须与 PG
`file_entries` 的 `completed` 集合**等集合相等**（不是抽样、不是只数个数），被删对象
在账本中必须是 `status='deleted'` 或合法缺席，断流必须留下 `storage_event_marks`
的 `gap` 窗口记录。

单测与集成测试覆盖不到这条链路的故障面：它们用的是**假 filer 流**（内存里造的
事件序列），断不了「真 SeaweedFS 容器被杀/重启后，订阅、游标、回读、入账各环节
各自的真实行为」。本脚本自起一套完全隔离的环境（独立 compose 项目 `fileagent-fi`
+ 高位端口），从当前工作树构建被测二进制，对真实容器注入故障。

## 四个场景

| # | 场景 | 操作 | 断言 |
|---|------|------|------|
| 1 | 断流重连 | CP 经一个可杀 TCP 代理（`faultproxy/`，纯字节转发）订阅 filer；写入 200 个新对象 + 删除 20 个新建 + 删除/覆盖各 15 个**已入账**对象后，`kill -9` 代理制造断流，再重启代理 | ① S3 存活集合 == 账本 `completed` 集合 == 预先固定的期望存活集合（180 个）；② 新建即删的对象在账本为 `deleted` 或合法缺席；③ 已入账后删除组全部 `status='deleted'`；④ 已入账后覆盖组行必须**存在**且 `completed` + `source='storage_readback'` + size/etag 匹配新内容（缺行即失败）；⑤ `storage_event_marks` 新增 `gap` 标记 |
| 2 | filer 优雅重启 | 持续写入 120 个对象期间 `docker compose restart seaweed-filer`（`stop_grace_period` 保障优雅停机），重启后删除 10 个 | 与场景 1 同口径的等集合断言：存活 110、计划 120、删除 10、增量写失败 0 |
| 3 | CP 停机期间写入 | 停 CP（TERM→KILL 确认退出）→ 写 120 个新 + 删 12 个新建 + 删/覆盖各 15 个已入账 → 重启 CP（同一 DB，游标来自 `storage_event_cursors` 持久化行） | 同口径等集合断言 + 已入账后删除/覆盖两组断言（同场景 1 ③④） |
| 4 | filer 硬杀 | 连写 80 个拉满落盘窗口后 `docker kill -s KILL seaweed-filer`，显式 `docker start` + 等 healthy，再写 20 个 | **只记录、不要求通过**：统计硬杀窗口内「S3 有、账本无」的丢失对象数与 `gap` 窗口，结果写入 `SCENARIO4_LOST=<n> (recorded only)`，不计数进 FAILURES |

## 通过判据

- **`FAILURES=0` 且退出码 0**。任何一项断言失败都计入 `FAILURES`，脚本以
  `exit $FAILURES` 结束（cleanup 失败也强制非零退出）。
- 场景 4 只记录，不要求通过。
- `FI_HANG_DEMO=1` 是**预期的红**：注入一次挂起查询（`pg_sleep 300`），证明所有
  外部调用都有界（脚本必须在 15s 上限内按失败退出，退出码非 0）。

## 前置条件

- **docker**（compose v2，容器需可拉取 postgres/redis/nats/seaweedfs 镜像）
- **mc**（MinIO client，用于 S3 写入/删除/列举）
- **Go**（构建 `bin/controlplane` 与 `bin/faultproxy-bin`，与仓库其余部分同版本要求）
- **python3（仅标准库，兜底，非必需）**：脚本先验证 `date +%s%N` 输出为纯数字
  （新版 macOS 与 Linux 的 `date` 均满足，如 Darwin 24 实测支持 `%N`），可用则
  直接用 `date`；仅当 `date` 不支持 `%N`（部分旧版 macOS/BSD `date` 会输出
  字面 `N`）时才退回 `python3 -c 'time.time_ns()'` 生成纳秒时间戳；两者都
  不可用时用秒级 `date +%s` 兜底。即：python3 只在宿主 `date` 缺 `%N` 支持
  时才是前置条件
- 端口空闲（被占用时脚本 fail-closed 拒绝运行）：`25132 26179 27122 28122
  29334 28334 28898 28899 28081 29091 28990`

## 运行方法

```bash
bash test/fault-injection/run.sh            # 全部 4 个场景
FI_ONLY=2 bash test/fault-injection/run.sh  # 只跑场景 2（可组合：FI_ONLY=1,3）
FI_HANG_DEMO=1 bash test/fault-injection/run.sh   # 有界调用自证（预期失败退出）
```

脚本自己构建被测二进制（从当前工作树），不需要预先 `make build`。
输出与中间产物全部落在 `bin/tmp/fi/`（结果汇总 `result.txt`、CP 日志 `cp.log`、
代理日志 `proxy.log`、各场景计划/清单文件等），不污染仓库其它目录。

退出时 cleanup 对**自己创建的资源**做 `down -v --remove-orphans`（两个 compose
项目）、按 pid 杀 CP/代理/写入器进程组并确认退出、核验自有端口全部释放；端口
预检失败（拒绝路径）则不创建任何东西、不做任何清理动作。

## 与 `deploy/scripts/smoke.sh` 的关系

`smoke.sh` 验证**正常路径**（happy path：起环境 → 走完整链路 → 全绿）；本脚本
验证**故障恢复**（断流、重启、停机、硬杀之后账本与存储最终一致）。两者互补，
都过才算 SeaweedFS 事件源链路有回归防护。

## 何时必须跑

改动以下任一路径时，必须在合入前完整跑一遍（`FAILURES=0`）：

- `controlplane/internal/storage/seaweedfs/**`（订阅适配器 / 回读）
- `controlplane/internal/storageevent/**`（事件源契约与 Sink）
- `controlplane/cmd/server/storage_event_source.go`（事件源装配）
- `controlplane/migrations/**`（`storage_event_cursors` / `storage_event_marks` 表）
- `deploy/seaweedfs/**`、`deploy/docker-compose.seaweedfs.yml`（拓扑与配置）
- `test/fault-injection/**`（本脚本自身）

## CI 状态

**尚未接入 CI。** 计划作为路径触发的 CI 门禁（下一个 PR 添加 workflow：先非必过、
稳定后设为必过），当前只能本地运行。
