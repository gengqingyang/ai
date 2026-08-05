.PHONY: help build run tidy fmt vet test clean

BIN := bin/chat

help:
	@echo "make build  编译到 $(BIN)"
	@echo "make run    直接运行交互式聊天"
	@echo "make tidy   整理依赖"
	@echo "make fmt    格式化"
	@echo "make vet    静态检查"
	@echo "make test   跑测试"

build:
	go build -o $(BIN) ./cmd/chat

run:
	go run ./cmd/chat

tidy:
	go mod tidy

fmt:
	go fmt ./...

vet:
	go vet ./...

test:
	go test ./...

clean:
	rm -rf bin
