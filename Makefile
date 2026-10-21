.PHONY: all build install clean check release

BINARY_NAME=sshtui
INSTALL_DIR=$(HOME)/.local/bin
VERSION=$(shell cat VERSION)

all: build

build:
	go build -ldflags="-s -w" -o $(BINARY_NAME) main.go

install: build
	mkdir -p $(INSTALL_DIR)
	cp $(BINARY_NAME) $(INSTALL_DIR)/.$(BINARY_NAME).new
	mv -f $(INSTALL_DIR)/.$(BINARY_NAME).new $(INSTALL_DIR)/$(BINARY_NAME)
	ln -sf $(INSTALL_DIR)/$(BINARY_NAME) $(INSTALL_DIR)/s
	@echo "✓ 安装成功！已写入 $(INSTALL_DIR)/$(BINARY_NAME) 并软链接为 $(INSTALL_DIR)/s"

clean:
	rm -f $(BINARY_NAME)

check:
	go test ./...
	go vet ./...
	git diff --check

# Commit VERSION and CHANGELOG.md before publishing an immutable version tag.
release: check
	@test -z "$$(git status --porcelain)" || (echo "请先提交所有改动，再发布版本"; exit 1)
	@tag=v$(VERSION); \
	git tag -a "$$tag" -m "sshtui $(VERSION)" || exit; \
	if ! git push --atomic origin HEAD "refs/tags/$$tag"; then \
		git tag -d "$$tag" >/dev/null; \
		exit 1; \
	fi
