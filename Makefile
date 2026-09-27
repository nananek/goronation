# goronation の開発コマンド。CI も同じ `make check` を回す。
#
# go.work のルートでは `go build ./...` が使えない
# (directory prefix . does not contain modules listed in go.work)。
# そのため、go.work の全 module を列挙して、各 module の中で実行する。

SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c

# 追加の go test フラグ。CI は -v を渡して、skip の理由と bwrap の版をログに出す
# (go test は -v が無いと、skip の理由も t.Logf も出さない)。
GO_TEST_EXTRA ?=

# -race は cgo (gcc) を要する。ローカルに gcc が無い場合は、
#   make test GO_TEST_FLAGS=-count=1
# のように上書きして逃げる。CI (ubuntu runner) は既定のまま回す。
GO_TEST_FLAGS ?= -race -count=1 $(GO_TEST_EXTRA)

# 全 module で $(1) を実行する。module が 1 つも見つからなければ失敗する
# (空振りで緑にしない)。
define each_module
	@mods="$$(go list -m -f '{{.Dir}}')"; \
	test -n "$$mods" || { echo "go.work に module が見つからない" >&2; exit 1; }; \
	while IFS= read -r dir; do \
		echo "==> $$dir: $(1)"; \
		(cd "$$dir" && $(1)); \
	done <<< "$$mods"
endef

.PHONY: check fmt-check vet vet-darwin test build test-bwrap docs docs-check help

# `## ` の後ろは、make help が出す説明 (target の行に書く)。
check: fmt-check vet vet-darwin test build ## 下の fmt-check・vet・vet-darwin・test・build をすべて実行する (CI が回すのはこれ)

# gofmt -l の出力が空であること。
fmt-check: ## gofmt -l の出力が空であること
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then \
		echo "gofmt が必要なファイル:" >&2; \
		echo "$$out" >&2; \
		exit 1; \
	fi

vet: ## 全 module に go vet ./...
	$(call each_module,go vet ./...)

# seatbelt (macOS) を書く人が触る module (core・sandbox) が、darwin でビルドできること (Linux 専用のコードが混ざらないこと)。cgo は要らない。
vet-darwin: ## core・sandbox に GOOS=darwin go vet (Linux 専用のコードの混入を防ぐ)
	@for dir in core sandbox; do \
		echo "==> $$dir: GOOS=darwin go vet ./..."; \
		(cd "$$dir" && GOOS=darwin go vet ./...) || exit 1; \
	done

test: ## 全 module に go test (フラグは GO_TEST_FLAGS・GO_TEST_EXTRA で変える)
	$(call each_module,go test $(GO_TEST_FLAGS) ./...)

# 単一バイナリ・cgo 禁止 (CGO_ENABLED=0) の裏取り。
build: ## CGO_ENABLED=0 で bin/goro を作る (単一バイナリと cgo 禁止の裏取り)
	@mkdir -p bin
	cd cmd && CGO_ENABLED=0 go build -o "$(CURDIR)/bin/goro" ./goro

# bwrap を要するテストを、skip ではなく必須にして回す (CI の bwrap leg と同じ)。
test-bwrap: ## GORO_REQUIRE_BWRAP=1 で make test を実行する (bwrap を skip でなく必須にする)
	GORO_REQUIRE_BWRAP=1 $(MAKE) test

# 文書 (docs/reference/ と、ADR の索引の生成区間) を、Go の doc comment から生成する。
# docgen は、cwd の 2 つ上を repo の root に固定する (tools/docgen で実行する)。
# docs-check は、まだ check に入れない: 生成物をコミットするまで、check が赤になるため。
docs: ## doc comment から docs/reference/ と ADR の索引を生成する (tools/docgen。問題があれば失敗)
	cd tools/docgen && go run .

docs-check: ## 生成物が最新で、文書の形式と上限を満たすかを、書かずに検査する (問題があれば失敗)
	cd tools/docgen && go run . -check

help: ## この一覧を出す
	@awk -F':.*## ' '/^[a-zA-Z0-9_-]+:.*## /{printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)
