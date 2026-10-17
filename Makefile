.PHONY: all build install clean

BINARY_NAME=sshtui
INSTALL_DIR=$(HOME)/.local/bin

all: build

build:
	go build -ldflags="-s -w" -o $(BINARY_NAME) main.go

install: build
	mkdir -p $(INSTALL_DIR)
	cp $(BINARY_NAME) $(INSTALL_DIR)/$(BINARY_NAME)
	ln -sf $(INSTALL_DIR)/$(BINARY_NAME) $(INSTALL_DIR)/s
	@echo "✓ 安装成功！已写入 $(INSTALL_DIR)/$(BINARY_NAME) 并软链接为 $(INSTALL_DIR)/s"

clean:
	rm -f $(BINARY_NAME)
