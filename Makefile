# goronation の開発コマンド。CI も同じ `make check` を回す。
#
# go.work のルートでは `go build ./...` が使えない
# (directory prefix . does not contain modules listed in go.work)。
# そのため、go.work の全 module を列挙して、各 module の中で実行する。

SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c

# -race は cgo (gcc) を要する。ローカルに gcc が無い場合は、
#   make test GO_TEST_FLAGS=-count=1
# のように上書きして逃げる。CI (ubuntu runner) は既定のまま回す。
GO_TEST_FLAGS ?= -race -count=1

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

.PHONY: check fmt-check vet test build test-bwrap

check: fmt-check vet test build

# gofmt -l の出力が空であること。
fmt-check:
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then \
		echo "gofmt が必要なファイル:" >&2; \
		echo "$$out" >&2; \
		exit 1; \
	fi

vet:
	$(call each_module,go vet ./...)

test:
	$(call each_module,go test $(GO_TEST_FLAGS) ./...)

# 単一バイナリ・cgo 禁止 (CGO_ENABLED=0) の裏取り。
build:
	@mkdir -p bin
	cd cmd && CGO_ENABLED=0 go build -o "$(CURDIR)/bin/goro" ./goro

# bwrap を要するテストを、skip ではなく必須にして回す (CI の bwrap leg と同じ)。
test-bwrap:
	GORO_REQUIRE_BWRAP=1 $(MAKE) test
