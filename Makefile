.PHONY: all build install clean check release

BINARY_NAME=sshtui
INSTALL_DIR=$(HOME)/.local/bin

all: build

build:
	go build -ldflags="-s -w" -o "$(BINARY_NAME)" .

install: build
	@mkdir -p "$(INSTALL_DIR)"
	@set -eu; abs_dir="$$(CDPATH= cd -- "$(INSTALL_DIR)" && pwd)"; \
	tmp_file="$$(mktemp "$$abs_dir/.$(BINARY_NAME).XXXXXX")"; \
	trap 'rm -f "$$tmp_file"' EXIT; \
	cp "$(BINARY_NAME)" "$$tmp_file" && \
	chmod 755 "$$tmp_file" && \
	mv -f "$$tmp_file" "$$abs_dir/$(BINARY_NAME)" && \
	ln -sf "$$abs_dir/$(BINARY_NAME)" "$$abs_dir/s" && \
	trap - EXIT; \
	echo "✓ 安装成功！已写入 $$abs_dir/$(BINARY_NAME) 并软链接为 $$abs_dir/s"

clean:
	rm -f "$(BINARY_NAME)"
	rm -rf dist

check:
	go test ./...
	go vet ./...
	git diff --check

# Commit VERSION and CHANGELOG.md before publishing an immutable version tag.
release: check
	@test -z "$$(git status --porcelain)" || (echo "请先提交所有改动，再发布版本"; exit 1)
	@./scripts/release-check.sh
	@set -eu; branch="$$(git symbolic-ref --short HEAD)"; \
	version="$$(./scripts/read-version.sh VERSION)"; \
	tag="v$$version"; \
	./scripts/push-release.sh "$$tag" "$$branch"
