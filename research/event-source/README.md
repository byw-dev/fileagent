# B2 存储事件源实验

报告：[`docs/reports/storage-event-source.md`](../../docs/reports/storage-event-source.md)。不是产品代码。

```bash
cd research/event-source
docker compose up -d --wait                      # SeaweedFS 4.48 单节点，filer 元数据库 leveldb3
GOWORK=off go build -o ../../bin/eventsource .

# 订阅（本地流；加 -aggregated 换聚合流），心跳与事件都写进 JSONL
../../bin/eventsource watch -prefix /buckets/load -out watch.jsonl &
# 突发 + 空闲负载；或用 -delay 做持续轻负载
../../bin/eventsource load -c 128 -rounds 4 -burst 5s -idle 7s -out writes.jsonl
# 逐个执行带标签的 S3 操作，看每种操作翻译成什么事件
../../bin/eventsource ops -out ops.jsonl

python3 analyze.py load watch.jsonl writes.jsonl  # 心跳声明违例、乱序幅度、是否每个写入都有事件
docker compose down -v
```

`results/`：`summary.jsonl`（各组负载的分析结果）、`ops*.jsonl`（操作与事件对照样本）、`light-local-heartbeats.jsonl`（心跳与落盘水位序列）。
硬崩溃实验：订阅方离线时写入，`docker kill -s KILL` 后 `docker start`，再从写入前的时间戳订阅。
