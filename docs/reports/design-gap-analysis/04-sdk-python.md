# 04 — Python SDK 与设计文档对照

> 对照基准：`docs/design/system-design.md` 第八章
> 注：T3-3（SDK+CP 联调）尚未执行，本模块是唯一"从未与真实服务端对过"的模块，
> 静态一致 ≠ 能用，动态验证见 `06-e2e-verification.md`。

---

## 1. 结构与功能范围（§8.1 / §8.2）

| 项 | 状态 | 说明 |
|----|------|------|
| 项目结构 | ✅ | client/auth/http/resources/models/exceptions 与设计一致，另有 upload_logs 模型（设计结构图漏列但 §8.1 功能表有） |
| 认证 + Token 自动刷新 | ✅（静态） | `auth.py`：先 refresh 后重登录，与 §8.4 伪码一致；路径 `/api/auth/*` 正确 |
| files.list / iter / get / download / stream | ✅（静态） | 与 §8.2 示例 API 对齐 |
| batch-download-urls | 待查 | §8.1 功能表列出，resources/files.py 是否实现待确认 |
| 异常类型 6 种 | ✅ | exceptions.py |
| cursor 分页 | ✅（静态） | `models/pagination.py`：items/total/next_cursor/has_more，与 §8.5 契约一致 |

## 2. 风险点

1. **零联调历史**：CP 的响应信封经历过 T3-2-FIX 的 12 项修正（`{items,total,next_cursor}`
   信封统一），SDK 的模型是照设计写的——设计与 CP 实际响应之间的漂移会在 T3-3 时爆出，
   属于可预测的下一轮"联调时功能反复坏"。
2. FileQuery 的过滤参数名（file_type_name/agent_ids/path_prefix/start_time...）
   与 CP `files.go List` 实际支持的 query 参数是否一致，未经验证。
3. Token 过期自动重试链路（401 → refresh → 重放原请求）只有 respx mock 测试，
   真实 CP 的 401 响应格式若与 mock 不同即失效。

## 3. Java SDK（§8.3）

未开工，backlog T4-4 排期中，与计划一致，无差异。

## 4. 小结

结构层面与设计高度一致，但这正是历史上每个模块联调前的状态。
建议 T3-3 联调前先跑本报告 06 章的 e2e 校验清单，把契约错位在联调前静态比对出来。
