# research/storage-layer-eval — 存储层替代调研的测试工具与原始结果

配套报告：[`docs/reports/storage-layer-eval.md`](../../docs/reports/storage-layer-eval.md)（**结论以报告为准**）。
报告里标为「仓内可复算」的数字都能用本目录复现；「模型」与「现场记录」两类的边界见报告文首。

> ⚠️ **研究工具，不是产品代码。** 不进 CI，不计覆盖率，也不受 CLAUDE.md 里产品代码的规范约束
> （单测、zap 日志等）。改动它不影响任何模块。
> 所有凭据（`minioadmin`、`cpadmin-secret`、`etladmin-secret`、`iam.json` 的 `signingKey`）**只用于本地测试**。

## 目录

| 路径 | 是什么 | 对应报告 |
|---|---|---|
| `probe/` | Go 能力探针（minio-go v7.0.91，与项目同版本）：STS、分片续传、预签名、键语义、校验和、事件；以及 `mp-prepare`/`mp-verify`（重启）、`expiry`（G1.12）、`sts-save`/`sts-use`（G1.13） | §1 Tier 0–1 |
| `sub/` | Go 工具：SeaweedFS filer 元数据订阅器（`sub`）、写入器（`write`）、因果竞态（`causal`）、重放核对（`check`）、压测（`load`/`measure`）、webhook 接收端（`sink`） | §1 Tier 4、§3 |
| `eco/` | Python 生态兼容测试（xarray / pandas / pyarrow / DuckDB / `credential_process`），`requirements.txt` 为实测版本 | §1 Tier 5 |
| `minio/` `seaweedfs/` `seaweedfs-nowh/` `seaweedfs-ent/` `juicefs/` `juicefs-redis/` | 各候选的 compose（高位端口、独立项目名，不碰 dev/smoke 环境）与初始化配置 | §0 |
| `run-bench.sh` `bench-repeat.sh` `aggregate.py` | 第四档压测（按站点/日期分层的叶目录形态，10 万 / 50 万 / 100 万）与三轮中位数汇总（含冷缓存读取量、内存与磁盘占用） | §1 Tier 4 |
| `hdd_model.py` | 机械盘外推模型（以 minio-inventory 生产实测为标定，**模型而非实测**） | §1 Tier 4 |
| `subscribe_summary.py` | 从 `results/subscribe/` 离线复算元数据订阅实验（完整性、重复、乱序、延迟、清理后续订） | §3 |
| `io-cold.sh` | 冷缓存读盘次数（重启 + 清页缓存 + 读 `/proc/diskstats`） | §1 Tier 4 |
| `crash-events.sh` | 存储被 `kill -9` 后事件补投（MinIO / JuiceFS） | §1 G4.7 |
| `purge-test.sh` | SeaweedFS `fs.log.purge` 后从旧游标续订 | §3 |
| `ab-mem.sh` | SeaweedFS filer webhook 内存无界增长的 A/B | §1 Tier 3 |
| `redact.py` | 发布证据前的脱敏（私有 IP、OS/架构、MinIO 部署 ID 与 `x-amz-id-2` 节点名指纹、临时密钥前缀），幂等 | — |
| `lifecycle/` | 生命周期规则测试用的三份规则（仅过期 / 仅清理未完成分片 / 两者同时） | §1 G5 |
| `results/` | **已发布的证据**：`bench/`（压测与冷缓存）、`probe/` `probe-neg/`（探针与负向对照）、`crash/`、`eco-*.json`、`subscribe/`（订阅实验原始日志 + `summary.md`）、`ab-mem.log`、`hdd-model.md` | 全文 |

端口：MinIO `19100`，SeaweedFS `19200`（filer gRPC `19288`），SeaweedFS 企业版 `19300`，
JuiceFS `19400`，JuiceFS+Redis `19500`，webhook 接收端 `18990`。

## 复现

> **所有工具默认写到 `tmp/`（已 gitignore），不会覆盖 `results/`。** `results/` 是已发布的证据；
> 只有在有意替换证据时，才把 `tmp/` 下的新结果复制过去，并同步更新报告。
> **复制之前先脱敏**（仓库公开）：`python3 redact.py tmp/<目录>`，它会替换私有 IP、客户端操作系统与架构、
> MinIO 部署 ID、`x-amz-id-2` 节点名指纹和临时密钥前缀；可重复执行，再跑一次应无输出。

前置：Docker（本轮用 OrbStack）、Go（`sub` 需 1.27+，`GOTOOLCHAIN=auto` 会自动拉取）、`mc`、`uv`（只有 `eco/` 需要）。
MinIO 镜像是 D-036 的私有 GHCR 镜像，首次需 `docker login ghcr.io`。

```bash
cd research/storage-layer-eval
./build.sh                                   # → bin/probe、bin/sub、pyenv/

# 能力探针（以 MinIO 为例；SeaweedFS 换端口 19200，见各 compose）
docker compose -f minio/compose.yml up -d --wait && ./minio/setup.sh
TARGET=minio S3_ENDPOINT=localhost:19100 EVENTS=webhook ./bin/probe run      # → tmp/probe/
MUTATE=loose-policy TARGET=minio-loose ./bin/probe run                        # 负向对照：G1.4/G1.5 应翻红

# 第四档压测（每次全新环境；JuiceFS 一轮约 55 分钟）
./run-bench.sh seaweedfs-nowh "100000 500000 1000000"    # → tmp/bench/results.jsonl
./io-cold.sh seaweedfs-nowh localhost:19200 list          # → tmp/bench/io-cold.jsonl
python3 aggregate.py tmp/bench                            # 不带参数则汇总已发布的 results/bench

# 只读复算（不需要任何存储服务）
python3 aggregate.py && python3 subscribe_summary.py && python3 hdd_model.py

# Python 生态（需先按 eco_test.py 顶部说明建好 etladmin 身份，见下）
pyenv/bin/python eco/eco_test.py minio localhost:19100 arn:aws:iam:::role/etl   # → tmp/eco-minio.json
```

生命周期规则（报告 Tier 0 G5）：对每个已初始化的环境执行
`for f in exp abort both; do mc ilm import <alias>/probe-other < lifecycle/lc-$f.json; mc ilm export <alias>/probe-other; done`，
比较导入前后规则是否被拒绝或剥离。

注：已发布的 `results/bench/` 是用另一个 key 前缀字面值跑出来的；前缀只是名字，不影响任何测量（列举、读盘次数都只取决于层级形态与对象数）。

**仓内不可复算的项**（报告中标为「现场记录」；工具仍可重跑，但本轮原始输出未留存）：
JuiceFS+Redis 变体的吞吐与每文件内存、SeaweedFS 企业版与开源版的二进制符号对比、e3b 因果实验与存储真值的逐对象比对、
首轮压测中 SeaweedFS 带 webhook 时的内存峰值 15.5GB、生命周期规则导入导出的结果、重启耗时（SeaweedFS 优雅关闭约 25 秒、JuiceFS 0.3 秒）、JuiceFS 网关在元数据库不可用时退出。

`eco_test.py` 需要一个 ETL 身份：MinIO 上用 `mc admin user add … etladmin` + `minio/etl-policy.json`；
SeaweedFS 的 `seaweedfs/s3.json`、`iam.json` 已内置 `etladmin` 与 `etl-role`。

## 踩过的坑（复现前先看）

- **循环脚本一律用 bash 跑。** zsh 不会按空格拆分 `set -- $spec` 和 `$dc` 这类变量，命令会静默地什么都不做。
- **Go 构建必须 `GOWORK=off`**（`build.sh` 已处理），否则仓库根的 `go.work` 会报 `main module does not contain package`。
- **MinIO 配 webhook 时会真的去拨目标**：配置期间要有接收端在监听（`setup.sh` 起了临时监听）。
- **新版 `mc` 与 JuiceFS 网关的 2021 版管理接口不全兼容**：绑定 policy 要走旧接口
  `PUT /minio/admin/v3/set-user-or-group-policy`（curl `--aws-sigv4`），改配置后重启容器而非 `mc admin service restart`。
- **minio-go 会把 `DurationSeconds < 3600` 钳成 3600**：`expiry` 子命令用手工签名绕过。
- **SeaweedFS 关闭需约 25 秒**：Docker 默认 10 秒宽限会强杀，丢最近一分钟的元数据日志；要么 `docker stop -t 120`，要么接受这一点。
- **SeaweedFS 不要开 filer webhook**（目标不可达时内存无界增长）；`seaweedfs-nowh/` 即无 webhook 的版本，压测都用它。
- **JuiceFS 网关在元数据库不可用时直接退出**：`io-cold.sh` 对 JuiceFS 先重启 PG、等就绪，再启动网关。
- `io-cold.sh` 用按 digest 钉定的 Alpine 特权容器（测试时的 `alpine:latest`）读 `/proc/diskstats`、清页缓存；块设备名（`vdb1`）取决于你的 Docker 虚拟机。

## 清理

```bash
for p in minio seaweedfs seaweedfs-nowh seaweedfs-ent juicefs juicefs-redis; do docker compose -f $p/compose.yml down -v; done
rm -rf bin tmp pyenv subexp eco/work-*      # 均在 .gitignore 内
```
