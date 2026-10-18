# ⚡ sshtui (Ghostty SSH Picker)

使用 `s --version` 查看本机版本，历史改动见 [CHANGELOG.md](CHANGELOG.md)。

专为 **Ghostty** 终端量身定制的轻量、极速 SSH 服务器连接选择器。交互与视觉设计深度契合 **Pi 现代终端美学**，支持密码与密钥双轨登录、实时模糊搜索与单键直连。

---

## ✨ 核心特性

- **现代 TUI 质感 & A-z 排序**：基于 Bubble Tea & Lip Gloss 开发，支持打字即搜（实时过滤名称、IP、分组、用户）、上下微调、高亮徽章与视口滚动。全量列表自动按 A-z 字母序排列。
- **智能秒级直连（Smart Direct-Connect）**：
  - 敲 `s 备注`（如 `s 爱尔兰` 或 `s dmit`），唯一匹配时**直接跳过 TUI 毫秒直连**！
  - 匹配多台时（如 `s oracle`），**自动带着筛选词唤起 TUI** 供精准上下选。
- **立即移交 SSH**：
  - 选定服务器或唯一匹配后立即启动 SSH；显示蓝色旋转动画与实际等待秒数，没有固定休眠或额外网络预检。SSH 首次输出或开始终端交互后停止动画，不显示虚构的百分比；重定向输出或极窄终端中不播放动画。
  - 默认连接及初始 SSH 握手超时为 10 秒，只尝试一次；服务器不可达或握手无响应时显示 SSH 错误并退回本地终端。此超时不限制正常登录后的会话时长。
- **密码 & 密钥双轨支持**：
  - 密钥登录：直接调用原生 SSH。
  - 密码登录：优先使用 `sshpass`，未安装时使用 OpenSSH 自带的 ASKPASS 机制（自动填密需要 OpenSSH 8.4+）。密码通过私有描述符或本地私有 Unix socket 传递，不放入命令行参数、环境变量或脚本。
  - 自动填密只响应 SSH 本身的认证请求；登录后的远端程序输出 `password:` 不会触发代填。
- **保留原生终端交互**：
  - SSH 直接读取用户终端，输出实时转发，支持窗口缩放、Ctrl+C 和退出码传递。显示连接动画或固定 Ghostty 标签时，程序等待 SSH 退出以完成清理；非交互情况下密钥和 sshpass 路径通过 `syscall.Exec` 移交进程。
  - 窄分屏自动使用紧凑布局；极小窗口提示放大，避免看不到目标时误连接。
- **服务器备注标签**：
  - 在 macOS Ghostty 1.3.1+ 中连接时，标签自动固定为服务器的 `name`（未填写时显示 `host`）；远端 shell 修改用户名或目录标题不会覆盖标签。
  - SSH 退出或连接失败后清除固定标题，恢复自动标题。通过 Ghostty 内置 AppleScript 接口完成，无需修改远端服务器或全局配置；首次使用时 macOS 可能提示自动化授权。接口不可用时提示错误并继续连接；tmux 中跳过此功能。
- **内置 Ghostty 乱码修复**：
  - Ghostty 默认传递 `TERM=xterm-ghostty`，在多数未装该 terminfo 的远程 Linux 上会导致 oh-my-zsh 提示符变 `?➜` 以及命令字符重叠。
  - 本工具在连接前自动将 TERM 重写为工业通用的 `xterm-256color`，从底层彻底解决远程乱码与光标错位。
- **自动心跳防卡死（KeepAlive & Auto-Timeout）**：
  - 默认开启客户端保活心跳（每 30 秒探测一次），防止中间 NAT 网关或云厂商路由器静默断开空闲 TCP 连接导致窗口死锁。
  - 连续 3 次心跳无应答（如笔记本休眠合盖）时，客户端 90 秒内自动优雅断开退回本地终端，彻底告别强关窗口。
- **极净退出机制（AltScreen）**：
  - 运行于终端备用缓冲屏，回车连接或退出时瞬间恢复干净的原生终端，不污染命令行历史缓冲区。
- **单一二进制**：
  - 不需要 Node.js、Python 或外部守护进程；连接和批量执行分别使用下文列出的系统命令。

---

## 🚀 快速使用

安装后系统已内置全局超短别名 `s`：

| 命令 | 说明 |
| :--- | :--- |
| **`s`** | **唤出 TUI 选择器**（支持实时打字过滤、上下方向键移动、回车连接） |
| `s --version` / `s -v` | 查看当前安装版本 |
| **`s 爱尔兰`** | **智能直连**：输入唯一备注或关键字（如 `s dmit`、`s 大阪`）**直接秒级连接，跳过 TUI**！ |
| `s oracle` | 关键字匹配到多台时，**自动带着该关键词唤起 TUI 筛选列表**供快速上下选 |
| **`s run "uptime"`** | **通过 Ansible 在所有已配置机器上执行命令** |
| `s run -g Prod "df -h"` | 只对特定分组（如 `Prod`）的所有机器并发执行命令 |
| **`s -e`** | **快速打开配置文件**（调用默认编辑器 `$EDITOR` 或 vim 编辑 `servers.yaml`） |
| `s -l` | 纯文本表格快速打印所有已配置的服务器列表（已按 A-z 字母序排列） |
| `s -p` | 打印当前使用的配置文件绝对路径 |
| **`s --ansible`** | 输出 Ansible Dynamic Inventory JSON 格式 |
| `s --ansible-ini` | 输出 Ansible 标准 INI 清单格式 |

---

## 🛠️ 配置文件说明

配置文件默认保存在：  
📁 **`~/.config/sshtui/servers.yaml`**（首次加载或编辑时自动生成模板；读取及编辑完成后检查权限并收紧为 `0600`）。

也可以保存在 `~/.ssh/servers.yaml`（程序会自动探测）。

`s -e` 和 `s -p` 不要求 YAML 能解析，因此配置写坏后仍可编辑或查看路径。`EDITOR` 支持参数和带引号的路径，例如 `code --wait`；不执行 shell 表达式。

### 配置格式示例

```yaml
# ==============================================================================
# sshtui server configuration
# Path: ~/.config/sshtui/servers.yaml (keep mode 0600)
# ==============================================================================

# Server entries inherit omitted values from these defaults.
default:
  user: root
  port: 22
  key: ~/.ssh/id_ed25519
  server_alive_interval: 30 # Keepalive interval in seconds; defaults to 30.
  server_alive_count_max: 3  # Disconnect after three unanswered keepalives.

servers:
  # Password authentication
  - name: "开发测试机-01"
    group: "Dev"
    host: "192.168.1.102"
    port: 22
    user: "root"
    auth: "password"
    password: "your_password_here"

  # Key authentication
  - name: "生产主库 (PostgreSQL)"
    group: "Prod"
    host: "10.0.0.5"
    port: 2222
    user: "admin"
    auth: "key"
    key: "~/.ssh/id_ed25519"
    args: ["-o", "ServerAliveInterval=60"] # Optional native SSH arguments.

  # Minimal entry: inherit user, port and key.
  - name: "香港日常开发机"
    group: "Dev"
    host: "47.242.100.20"
```

密码可包含引号、美元符号、反斜线和中文；最多 1023 个 UTF-8 字节（OpenSSH 限制），不接受换行及 NUL 字符，避免凭据工具静默截断。`args` 在密钥、sshpass、ASKPASS 和 Ansible 路径中都会保留，显式 `-o` 设置优先于内置默认值。

慢线路可在对应服务器的 `args` 中设置 `['-o', 'ConnectTimeout=30']`，将连接超时调为 30 秒。该超时针对连接及初始握手，不包含 DNS 解析耗时或等待用户输入密码的时间。

---

## 🤖 与 Ansible 自动化运维无缝联动

`sshtui` 原生支持将 `servers.yaml` 导出为符合 **Ansible Dynamic Inventory 官方规范** 的资产清单，**一份配置，两处复用**：

### 1. 动态清单直接运行（无需生成任何中间文件）
```bash
# Ansible calls s --list to obtain the dynamic inventory.
ansible all -i "$(command -v s)" -m shell -a "df -h"

ansible all -i "$(command -v s)" -m ping

# Run only on a specific group.
ansible Prod -i "$(command -v s)" -m shell -a "uptime"
```

### 2. 导出为静态 INI 主机清单
```bash
# Inventories may contain passwords; restrict output permissions.
(umask 077; s --ansible-ini > hosts.ini)

# Run a playbook.
ansible-playbook -i hosts.ini my_playbook.yml
```

> **参数自动映射说明**：
> - 密钥登录自动映射为 `ansible_ssh_private_key_file`
> - 密码登录自动映射为 `ansible_password` & `ansible_ssh_pass`
> - 自动注入 `-o StrictHostKeyChecking=accept-new`、10 秒连接超时、单次连接尝试与心跳保活默认值，允许通过 `args` 覆盖。
> - 主机名称（空名称使用 host）必须唯一；冲突时拒绝导出，避免把命令发往错误机器。`all` 和 `_meta` 为保留分组名。
> - INI 支持带空格的名称和含引号的密码；主机别名含 `[]`、冒号或换行时请改用动态 JSON 清单。

---

## 👻 Ghostty 进阶联动技巧

### 1. 配置专属快捷键（右侧分屏一键唤出）
编辑你的 Ghostty 配置文件 `~/.config/ghostty/config`：

```ini
# Open a split to the right with Cmd+Shift+S.
keybind = super+shift+s=new_split:right
```

### 2. 服务器备注标签与全局 TERM

在 macOS Ghostty 1.3.1+ 内运行 `s`，选择服务器后标签会自动显示 `servers.yaml` 中的 `name`，例如「Oracle-大阪」。正常退出、连接超时或认证失败后恢复自动标题。分屏共享同一个标签，标签显示最近通过 `s` 连接的服务器备注；各连接退出时会清除该标签的固定标题。

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

# Build and install ~/.local/bin/sshtui and ~/.local/bin/s.
make install
```

验证修复和回归行为：

```bash
go test ./...
go vet ./...
```

测试通过本地替身验证 SSH 调用，并用系统 SSH 连接本机无响应端口验证握手超时，不连接外部服务器。安装 `expect` 和 `ansible-inventory` 后，会额外检查终端交互和 INI 清单往返；`expect` 仅用作测试终端驱动。缺少对应工具时相关集成测试会跳过。

### 版本与发布

`VERSION` 是版本号的唯一来源，编译时嵌入二进制。每次功能增加更新次版本号，兼容性修复更新补丁版本号，不兼容变更更新主版本号；同时在 `CHANGELOG.md` 记录变更。

更新代码、`VERSION` 和更新记录后，提交到 Git，再运行 `make release`。该命令执行测试、静态检查和差异检查，要求工作区干净，随后创建 `vX.Y.Z` 标签，并将当前提交和该标签原子推送到 `origin`。已有版本标签不能重复创建。

---

## 📋 依赖要求

- **操作系统**：macOS (Apple Silicon / Intel) 或 Linux
- **依赖命令**：
  - `ssh`（系统自带；未安装 sshpass 时，自动填密需要 OpenSSH 8.4+）
  - `sshpass`（可选的密码代填工具，macOS 可通过 `brew install hudochenkov/sshpass/sshpass` 安装）
  - `ansible`（仅 `s run` 等批量运维命令需要）
