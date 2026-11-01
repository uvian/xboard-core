SHELL := /bin/bash
XRAY_VERSION ?= v26.10.31
XRAY_DIR := third_party/xray
XRAY_REPO ?= https://github.com/XTLS/Xray-core
EXE := $(if $(filter Windows_NT,$(OS)),.exe,)

.PHONY: help vendor patches build test lint smoke verify clean push

help:
	@echo "make vendor     拉取 Xray-core $(XRAY_VERSION) 到 $(XRAY_DIR) 并打补丁"
	@echo "make patches    只重放 $(XRAY_DIR)/patches/*.patch（幂等）"
	@echo "make build      构建二进制到 bin/xboard-core"
	@echo "make test       单元测试"
	@echo "make smoke      合成配置冒烟（不打真实网络）"
	@echo "make verify     汇总检查（fmt+vet+test+build+smoke+补丁幂等）"
	@echo "make push       提交并推送到 origin（MSG=\"...\" make push）"

vendor:
	@test -d $(XRAY_DIR)/core || ( \
	  tmp=$$(mktemp -d) && \
	  git clone --depth 1 --branch $(XRAY_VERSION) $(XRAY_REPO) $$tmp && \
	  mkdir -p $(XRAY_DIR) && \
	  (cd $$tmp && tar --exclude=.git -cf - .) | (cd $(XRAY_DIR) && tar -xf -) && \
	  rm -rf $$tmp && echo "vendored $(XRAY_VERSION)" )
	@$(MAKE) patches

patches:
	@bash scripts/apply-patches.sh

build:
	@mkdir -p bin
	go build -trimpath -ldflags "-s -w" -o bin/xboard-core$(EXE) ./cmd/xboard-core

test:
	go test ./...

lint:
	@out=$$(gofmt -l cmd internal); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	go vet ./cmd/... ./internal/...

smoke:
	go run ./cmd/xboard-core -smoke

verify: lint test build smoke
	@$(MAKE) patches | tail -1
	@echo "verify ok"

clean:
	rm -rf bin

push:
	git add -A && git commit -m "$${MSG:-chore: update}" && git push -u origin HEAD
