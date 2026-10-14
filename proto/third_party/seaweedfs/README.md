# Vendored SeaweedFS proto

本目录存放从 SeaweedFS 上游 **逐字节复制**（不改动任何内容，包括 `go_package`）的
proto 定义，供 CP 的存储事件源适配器生成 Go 代码（D-038）。生成物落在
`controlplane/internal/storage/seaweedfs/filerpb/`，由 `make generate-proto` 统一产出。

| 项 | 值 |
|---|---|
| 上游仓库 | https://github.com/seaweedfs/seaweedfs |
| 版本（tag） | `4.48`（与将要部署的镜像同版本） |
| commit | `530be3e37337488ecc34d58441e0bc476e121c93`（tag `4.48` 为 lightweight tag，直指该 commit） |
| 上游文件 | `weed/pb/filer.proto` |
| 本地文件 | `proto/third_party/seaweedfs/filer.proto` |
| sha256 | `bec48f9b5f84b2dafcb882382933dbe5584c2bccb011fe6f8dd18f0b8b1fd2a9` |

## 硬规则

- **文件内容必须与上游逐字节一致**（用上面的 sha256 校验）。任何「顺手修一下」都会
  破坏「上游 tag ↔ 仓库文件」的可信映射；要改就去上游改，再升级本目录。
- `go_package` 指向 SeaweedFS 自己的模块路径，**保持原样**；生成时由 buf 的 managed
  mode override 改写到 `github.com/byw-dev/fileagent/controlplane/internal/storage/seaweedfs/filerpb`
  （见根目录 `buf.gen.yaml`）。因此 `controlplane` 的任何 go.mod 都**不得**引入
  `github.com/seaweedfs/seaweedfs` 模块（D-038）。

## 如何升级

1. 确定目标 tag（应与 deploy 使用的镜像版本一致），解析出 commit：
   `git ls-remote --tags https://github.com/seaweedfs/seaweedfs.git '<tag>*'`
2. 覆盖本文件：
   `curl -fsSL -o proto/third_party/seaweedfs/filer.proto https://raw.githubusercontent.com/seaweedfs/seaweedfs/<tag>/weed/pb/filer.proto`
3. `shasum -a 256 proto/third_party/seaweedfs/filer.proto`，把新值写进上面的表格。
4. `make generate-proto`（或 `make generate`），提交生成物。
5. `buf lint` + `buf breaking --against '.git#branch=master'` 必须通过。
   注意：**上游协议自身的删字段 / 改编号同样会触发 `buf breaking`**——这是有意保留的
   信号（适配器 `Translate` 依赖这些字段），不要为了绿灯把本目录加进 breaking 豁免；
   应当先核对 `internal/storage/seaweedfs` 的翻译逻辑与新协议仍然一致。
6. 重点 diff 上游 `weed/pb/filer.proto` 以及事件构造/心跳/日志命名相关源码
   （`weed/filer/filer_notify.go`、`weed/server/filer_grpc_server_sub_meta.go`、
   `weed/filer/filer_notify_read.go`），确认 `internal/storage/seaweedfs` 里的注释与
   实现所依据的行为没有漂移。
7. `cd controlplane && go vet ./... && go test ./... -count=1`。
