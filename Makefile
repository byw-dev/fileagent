# FileAgent — 统一构建入口
#
# 所有编译产物统一输出到项目根目录的 bin/ 目录。
# bin/ 已加入 .gitignore，不会提交到 git。
#
# 常用命令：
#   make build            — 构建全部二进制（controlplane + agent，纯 Go，不需要 Node）
#   make build-controlplane — 仅构建 controlplane（纯 API，不含前端）
#   make build-agent      — 仅构建 agent
#   make build-agent-windows — 交叉编译 Windows agent.exe（需 mingw-w64；权威产物见 CI）
#   make bundle           — 构建含 Web UI 的单文件 controlplane（+ agent）；需 Node 24 / pnpm 11
#   make build-webui      — 编译 webui 并拷贝 dist 至 controlplane 嵌入目录
#   make generate         — 重新生成所有代码生成产物（sqlc → controlplane/internal/db；proto → api/v1）
#   make generate-sqlc    — 仅重新生成 sqlc DB 代码
#   make generate-proto   — 仅重新生成 proto 契约代码（buf，不需要系统 protoc）
#   make test             — 运行全部单元测试
#   make tidy             — 整理所有模块的 go.mod / go.sum
#   make clean            — 删除 bin/ 目录
#
# 代码生成工具的版本由独立的 tools/ 子模块钉定（tools/go.mod 的 tool 指令），
# 统一经 `make generate` 调用；请勿手改 *.sql.go 等生成文件。

BIN_DIR := bin

WEBUI_EMBED_DIR := controlplane/internal/webui/dist

# buf 生成用的 scratch 目录（构建钉定版本的工具二进制 + 临时落点），见 generate-proto。
PROTO_TOOLS_DIR := .tmp-proto-tools
PROTO_OUT_DIR := .tmp-proto-gen

.PHONY: build build-controlplane build-agent build-agent-windows bundle build-webui build-controlplane-bundle generate generate-sqlc generate-proto test tidy clean

## build: 构建全部二进制（纯 Go，不含前端）
build: build-controlplane build-agent

## build-controlplane: 构建 Control Plane 服务（纯 API，不含前端）
build-controlplane:
	@mkdir -p $(BIN_DIR)
	cd controlplane && go build -o ../$(BIN_DIR)/controlplane ./cmd/server

## build-agent: 构建 Edge Agent
build-agent:
	@mkdir -p $(BIN_DIR)
	cd agent && go build -o ../$(BIN_DIR)/agent ./cmd/agent

## build-agent-windows: 交叉编译 Windows agent.exe（best-effort，见下方说明）
#  agent 依赖 mattn/go-sqlite3（CGO），从非 Windows 交叉编译需要 mingw-w64 工具链：
#    macOS:  brew install mingw-w64
#    Debian: apt-get install gcc-mingw-w64-x86-64
#  权威 Windows 产物由 CI 生成（.github/workflows/build-agent.yml → agent-windows-amd64）；
#  本目标仅供本地快速出包。CC 可用环境变量覆盖。
CC_WINDOWS ?= x86_64-w64-mingw32-gcc
build-agent-windows:
	@mkdir -p $(BIN_DIR)
	cd agent && GOOS=windows GOARCH=amd64 CGO_ENABLED=1 CC=$(CC_WINDOWS) \
		go build -o ../$(BIN_DIR)/agent.exe ./cmd/agent

## bundle: 构建含 Web UI 的单文件 controlplane（+ agent）
bundle: build-webui build-controlplane-bundle build-agent

## build-webui: 编译 webui 并拷贝 dist 至 controlplane 嵌入目录（需 Node 24 / pnpm 11）
build-webui:
	cd webui && pnpm install --frozen-lockfile && pnpm build
	rm -rf $(WEBUI_EMBED_DIR)
	cp -r webui/dist $(WEBUI_EMBED_DIR)

## build-controlplane-bundle: 以 webui build tag 编译，嵌入前端产物
#  显式依赖 build-webui：-tags webui 编译需要 $(WEBUI_EMBED_DIR) 已就绪。
#  写成前置依赖（而非仅靠 bundle 的依赖列表顺序）才能在 make -j 并行下保证
#  dist/ 先拷贝完成，避免嵌入编译读到不完整/缺失的目录。
build-controlplane-bundle: build-webui
	@mkdir -p $(BIN_DIR)
	cd controlplane && go build -tags webui -o ../$(BIN_DIR)/controlplane ./cmd/server

## generate: 重新生成所有代码生成产物（工具版本由 tools/ 子模块钉定）
generate: generate-sqlc generate-proto

## generate-sqlc: 用 tools/ 钉定的 sqlc 重新生成 controlplane DB 代码
#  GOWORK=off 让 tools/ 独立解析（不并入 go.work），保持依赖隔离。
generate-sqlc:
	GOWORK=off go -C tools tool sqlc generate -f ../controlplane/sqlc.yaml

## generate-proto: 用 tools/ 钉定的 buf + protoc-gen-go(-grpc) 重新生成 api/v1 契约代码
#  GOWORK=off 让 tools/ 独立解析（不并入 go.work），保持依赖隔离。
#  buf 自带编译器，不依赖系统 protoc；buf.gen.yaml 的插件是 local:（D-032 硬约束，严禁
#  remote:），protoc-gen-go / protoc-gen-go-grpc 也钉在 tools/go.mod。但 go tool 不会把
#  同模块的其他工具加进 PATH，所以先把三个工具构建到 scratch 目录再注入 PATH 后运行 buf
#  （buf 输出与插件版本只由 PATH 决定，因此 scratch 目录只是构建缓存，不影响产物内容）。
#  生成先落到 scratch 目录再 mv 回既有位置 api/v1/，保持生成物路径与 source 署名不变。
generate-proto:
	@rm -rf $(PROTO_TOOLS_DIR) $(PROTO_OUT_DIR)
	GOWORK=off go -C tools build -o ../$(PROTO_TOOLS_DIR)/ \
		github.com/bufbuild/buf/cmd/buf \
		google.golang.org/protobuf/cmd/protoc-gen-go \
		google.golang.org/grpc/cmd/protoc-gen-go-grpc
	PATH=$$PWD/$(PROTO_TOOLS_DIR):$$PATH $(PROTO_TOOLS_DIR)/buf generate
	@mv $(PROTO_OUT_DIR)/proto/v1/agent.pb.go $(PROTO_OUT_DIR)/proto/v1/agent_grpc.pb.go api/v1/
	@rm -rf $(PROTO_TOOLS_DIR) $(PROTO_OUT_DIR)

## test: 运行全部单元测试
test:
	cd controlplane && go test ./...
	cd agent && go test ./...

## tidy: 整理所有模块依赖
tidy:
	go mod tidy
	cd controlplane && go mod tidy
	cd agent && go mod tidy
	cd tools && GOWORK=off go mod tidy

## clean: 删除编译产物
clean:
	rm -rf $(BIN_DIR)
