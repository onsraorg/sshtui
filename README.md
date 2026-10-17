# ⚡ sshtui (Ghostty SSH Picker)

专为 **Ghostty** 终端量身定制的轻量、极速 SSH 服务器连接选择器。交互与视觉设计深度契合 **Pi 现代终端美学**，支持密码与密钥双轨登录、实时模糊搜索与单键直连。

---

## ✨ 核心特性

- **现代 TUI 质感**：基于 Bubble Tea & Lip Gloss 开发，支持打字即搜（实时过滤名称、IP、分组、用户）、上下微调、高亮徽章与视口滚动。
- **密码 & 密钥双轨支持**：
  - 密钥登录：直接调用原生 SSH。
  - 密码登录：优先使用 `sshpass` 自动填充（若未安装则自动回退至系统内置 `expect`），告别手动输密。
- **纯粹的原生终端体验（零性能损耗）**：
  - 选定服务器后，通过 `syscall.Exec` 直接将当前进程替换为 SSH，**100% 保持原生终端所有能力**（支持 Tmux、Vim、完整真彩、窗口重绘、Ctrl+C、SSH-Agent 转发）。
- **内置 Ghostty 乱码修复**：
  - Ghostty 默认传递 `TERM=xterm-ghostty`，在多数未装该 terminfo 的远程 Linux 上会导致 oh-my-zsh 提示符变 `?➜` 以及命令字符重叠。
  - 本工具在连接前自动将 TERM 重写为工业通用的 `xterm-256color`，从底层彻底解决远程乱码与光标错位。
- **自动心跳防卡死（KeepAlive & Auto-Timeout）**：
  - 默认开启客户端保活心跳（每 30 秒探测一次），防止中间 NAT 网关或云厂商路由器静默断开空闲 TCP 连接导致窗口死锁。
  - 连续 3 次心跳无应答（如笔记本休眠合盖）时，客户端 90 秒内自动优雅断开退回本地终端，彻底告别强关窗口。
- **极净退出机制（AltScreen）**：
  - 运行于终端备用缓冲屏，回车连接或退出时瞬间恢复干净的原生终端，不污染命令行历史缓冲区。
- **极速免依赖**：
  - 单一二进制文件（~3MB），零毫秒启动，不需要 Node.js、Python 或外部守护进程。

---

## 🚀 快速使用

安装后系统已内置全局超短别名 `s`：

| 命令 | 说明 |
| :--- | :--- |
| **`s`** | **唤出 TUI 选择器**（支持实时打字过滤、上下方向键移动、回车连接，带网络预检与进度条动效） |
| `s prod` | 唤出选择器并直接带入初始搜索词（例如只看 `prod` 机器） |
| **`s run "uptime"`** | **一键通过 Ansible 并发在所有 12 台机器上执行命令**（无需背复杂的 Ansible 参数） |
| `s run -g Prod "df -h"` | 只对特定分组（如 `Prod`）的所有机器并发执行命令 |
| **`s -e`** | **快速打开配置文件**（调用默认编辑器 `$EDITOR` 或 vim 编辑 `servers.yaml`） |
| `s -l` | 纯文本表格快速打印所有已配置的服务器列表（已按 A-z 字母序排列） |
| `s -p` | 打印当前使用的配置文件绝对路径 |
| **`s --ansible`** | 输出 Ansible Dynamic Inventory JSON 格式 |
| `s --ansible-ini` | 输出 Ansible 标准 INI 清单格式 |

---

## 🛠️ 配置文件说明

配置文件默认保存在：  
📁 **`~/.config/sshtui/servers.yaml`**（程序首次运行会自动生成模板，并强制设定权限为 `0600` 保护密码安全）。

也可以保存在 `~/.ssh/servers.yaml`（程序会自动探测）。

### 配置格式示例

```yaml
# ==============================================================================
# sshtui 服务器配置
# 路径: ~/.config/sshtui/servers.yaml (权限请保持 600)
# ==============================================================================

# 全局默认设置 (服务器条目若未填，会自动继承此处默认值)
default:
  user: root
  port: 22
  key: ~/.ssh/id_ed25519
  server_alive_interval: 30 # 保活心跳间隔(秒)，默认 30，防超时卡死
  server_alive_count_max: 3  # 最大重试次数，默认 3 次无响应自动断开

servers:
  # 1. 密码登录服务器
  - name: "开发测试机-01"
    group: "Dev"
    host: "192.168.1.102"
    port: 22
    user: "root"
    auth: "password"
    password: "your_password_here"

  # 2. 密钥登录服务器
  - name: "生产主库 (PostgreSQL)"
    group: "Prod"
    host: "10.0.0.5"
    port: 2222
    user: "admin"
    auth: "key"
    key: "~/.ssh/id_ed25519"
    args: ["-o", "ServerAliveInterval=60"] # 可选：支持传入任意原生 ssh 参数

  # 3. 极简写法（自动继承 default 的 user、port、key）
  - name: "香港日常开发机"
    group: "Dev"
    host: "47.242.100.20"
```

---

## 🤖 与 Ansible 自动化运维无缝联动

`sshtui` 原生支持将 `servers.yaml` 导出为符合 **Ansible Dynamic Inventory 官方规范** 的资产清单，**一份配置，两处复用**：

### 1. 动态清单直接运行（无需生成任何中间文件）
```bash
# 查看所有 12 台机器的磁盘剩余空间 (使用进程替换)
ansible all -i <(s --ansible) -m shell -a "df -h"

# 或者直接让 Ansible 将 s 命令识别为动态清单脚本
ansible all -i $(which s) -m ping

# 只对特定分组 (例如 Prod) 执行命令
ansible Prod -i <(s --ansible) -m shell -a "uptime"
```

### 2. 导出为静态 INI 主机清单
```bash
# 导出一份干净的 hosts.ini
s --ansible-ini > hosts.ini

# 运行 playbook
ansible-playbook -i hosts.ini my_playbook.yml
```

> **参数自动映射说明**：
> - 密钥登录自动映射为 `ansible_ssh_private_key_file`
> - 密码登录自动映射为 `ansible_password` & `ansible_ssh_pass`
> - 自动注入 `-o StrictHostKeyChecking=accept-new` 与心跳保活参数，批量执行不会因主机确认或超时挂死。

---

## 👻 Ghostty 进阶联动技巧

### 1. 配置专属快捷键（右侧分屏一键唤出）
编辑你的 Ghostty 配置文件 `~/.config/ghostty/config`：

```ini
# 按 Cmd+Shift+S 在当前窗口右侧切出分屏，并直接运行 s 服务器选择器
keybind = super+shift+s=new_split:right
```

### 2. 终端标题与全局 TERM（推荐）
如果你日常也有手动敲原生 `ssh` 的习惯，建议在 `~/.config/ghostty/config` 中加入：
```ini
term = xterm-256color
```
确保所有终端连接均使用兼容性最好的终端定义。

---

## 📦 编译与重新安装

本项目源码存放于：`~/sshtui`。如果后续想自行修改界面颜色、字段或增加功能，可以随时编译：

```bash
cd ~/sshtui

# 编译并自动安装至 ~/.local/bin/sshtui 与 ~/.local/bin/s
make install
```

---

## 📋 依赖要求

- **操作系统**：macOS (Apple Silicon / Intel) 或 Linux
- **依赖命令**：
  - `ssh`（系统自带）
  - `sshpass`（用于密码登录自动填充，macOS 可通过 `brew install hudochenkov/sshpass/sshpass` 安装，若无则自动回退至系统内置 `expect`）
