# FileAgent — 统一构建入口
#
# 所有编译产物统一输出到项目根目录的 bin/ 目录。
# bin/ 已加入 .gitignore，不会提交到 git。
#
# 常用命令：
#   make build            — 构建全部二进制（controlplane + agent）
#   make build-controlplane — 仅构建 controlplane
#   make build-agent      — 仅构建 agent
#   make test             — 运行全部单元测试
#   make tidy             — 整理所有模块的 go.mod / go.sum
#   make clean            — 删除 bin/ 目录

BIN_DIR := bin

.PHONY: build build-controlplane build-agent test tidy clean

## build: 构建全部二进制
build: build-controlplane build-agent

## build-controlplane: 构建 Control Plane 服务
build-controlplane:
	@mkdir -p $(BIN_DIR)
	cd controlplane && go build -o ../$(BIN_DIR)/controlplane ./cmd/server

## build-agent: 构建 Edge Agent
build-agent:
	@mkdir -p $(BIN_DIR)
	cd agent && go build -o ../$(BIN_DIR)/agent ./cmd/agent

## test: 运行全部单元测试
test:
	cd controlplane && go test ./...
	cd agent && go test ./...

## tidy: 整理所有模块依赖
tidy:
	go mod tidy
	cd controlplane && go mod tidy
	cd agent && go mod tidy

## clean: 删除编译产物
clean:
	rm -rf $(BIN_DIR)
