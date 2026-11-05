# sshtui

`sshtui` 是一个面向 macOS 与 Linux 的轻量 SSH 终端选择器。它把主机清单保存在本地 YAML 文件中，提供 TUI 搜索、唯一匹配直连、密码或密钥登录，以及 Ansible 动态清单导出。

项目默认命令为 `sshtui`，安装后同时创建短命令 `s`。

## 特性

- 输入即过滤：按名称、分组、主机和用户搜索，支持键盘选择与滚动视口。
- 唯一匹配直连：`s dev-app` 可跳过 TUI；多个匹配项会进入已过滤的列表。
- 原生 SSH 会话：保留窗口缩放、Ctrl+C、退出码和交互式终端行为。
- 密钥与密码认证：密码通过绑定目标 SSH helper 身份的 OpenSSH ASKPASS 提供。
- 凭据保护：密码不进入命令行参数、环境变量或临时脚本。
- 连接保护：默认 10 秒连接超时、单次尝试和 SSH keepalive。
- 终端恢复：SSH 退出后恢复 TTY，并关闭远端程序可能遗留的备用屏、鼠标和括号粘贴模式。
- Ghostty 集成：在支持的 macOS Ghostty 版本中固定标签标题，退出后恢复自动标题。
- Ansible 集成：同一份 YAML 可导出为动态 JSON 或 INI inventory，也可直接批量执行命令。

## 运行要求

- macOS 或 Linux。
- `ssh`，通常由系统自带。
- 密码自动填入需要 OpenSSH 8.4 或更新版本。
- `ansible`：仅批量执行和 inventory 验证需要。
- Go 1.27 或更新版本：仅从源码构建需要。

## 安装

```bash
git clone https://github.com/onsraorg/sshtui.git
cd sshtui
make install
```

默认安装到 `~/.local/bin/sshtui`，并创建 `~/.local/bin/s` 软链接。请确保 `~/.local/bin` 已加入 `PATH`。

只构建当前目录中的二进制：

```bash
make build
```

## 快速开始

首次执行 `s` 或 `s -e` 时，程序会创建：

```text
~/.config/sshtui/servers.yaml
```

如果该文件不存在，程序也会查找 `~/.ssh/servers.yaml`。配置文件必须是普通文件，权限会收紧为 `0600`。

打开配置：

```bash
s -e
```

启动选择器：

```bash
s
```

直接连接唯一匹配项：

```bash
s dev-app
```

## 命令

| 命令 | 说明 |
| --- | --- |
| `s` | 打开交互式服务器选择器 |
| `s <筛选词>` | 唯一名称匹配时直连，否则打开过滤后的列表 |
| `s -e` | 使用 `$EDITOR` 或 `vim` 编辑配置 |
| `s -e <参数>` / `s -a` | 快速解析并追加服务器到配置（如 `s -e 1.2.3.4 22 mypass`） |
| `s -l` | 以纯文本列出服务器 |
| `s -p` | 输出当前配置文件路径 |
| `s --version` / `s -v` | 输出版本 |
| `s --ansible` | 输出 Ansible 动态 JSON inventory |
| `s --ansible-ini` | 输出 Ansible INI inventory |
| `s run "uptime"` | 在全部服务器执行命令 |
| `s run -g production "df -h"` | 在指定分组执行命令 |

筛选词包含空格时必须使用引号，例如 `s "prod db"`。多余的位置参数会被拒绝，避免静默连接错误主机。
### 快速添加服务器

通过 `s -e`（或 `s -a`、`s add`）直接带参数即可安全追加服务器到配置文件，自动识别私钥或密码：

```bash
# 1. 最常用形式（IP + 端口 + 密码）
s -e 192.168.1.100 22 MyPassword123

# 2. 密钥认证（自动识别路径或常见私钥后缀）
s -e 192.168.1.100 22 ~/.ssh/my_vps.pem

# 3. 省略端口（继承 default.port，未配置时使用 22）
s -e 192.168.1.100 MyPassword123

# 4. 自定义用户名或别名
s -e ubuntu@1.2.3.4 22 MyPassword123 "测试机"
s -e root@1.2.3.4:2222 MyPassword123
```
支持裸 IPv6 地址；指定 IPv6 端口时使用 `[2001:db8::1]:2222`。快速添加使用独立锁串行化读改写，支持空列表、流式列表及字段顺序变化，保留 YAML 注释，但可能统一缩进与排版。配置仅支持一个 YAML 文档。

## 配置

```yaml
default:
  user: deploy
  port: 22
  key: ~/.ssh/id_ed25519
  server_alive_interval: 15
  server_alive_count_max: 30

servers:
  - name: dev-app
    group: development
    host: 192.0.2.10

  - name: staging-api
    group: staging
    host: 198.51.100.20
    auth: password
    password: your_password_here

  - name: prod-db
    group: production
    host: 203.0.113.30
    port: 2222
    user: dbadmin
    auth: key
    key: ~/.ssh/prod_ed25519
    args: ["-J", "bastion.example.com"]
```

示例使用 RFC 5737 文档专用地址，不代表真实服务器。

### 字段

| 字段 | 说明 |
| --- | --- |
| `name` | 显示名称，也是 Ansible 主机别名；建议保持唯一 |
| `group` | TUI 和 Ansible 分组，默认 `Default` |
| `host` | SSH 主机名或地址，必填 |
| `port` | SSH 端口，默认继承 `default.port`，最终回退为 `22` |
| `user` | SSH 用户，默认继承 `default.user`，最终回退为 `root` |
| `auth` | `key` 或 `password`；省略时根据是否存在 `password` 推断 |
| `key` | 私钥路径，支持 `~/`，可继承 `default.key` |
| `password` | 密码认证使用的本地明文凭据 |
| `args` | 额外的原生 SSH 参数，放在内置默认参数之前 |

未知字段、无效端口、无效认证类型和显示字段中的控制字符都会被拒绝。密码最多 1023 个 UTF-8 字节，不能包含 NUL、回车或换行。

`args` 是受信任配置，可以改变 SSH 行为，也可以通过 `ProxyCommand` 启动本地程序。不要使用来源不明的配置文件。

## SSH 行为

程序会追加以下默认选项：

```text
StrictHostKeyChecking=accept-new
ConnectTimeout=10
ConnectionAttempts=1
ServerAliveInterval=15
ServerAliveCountMax=30
TCPKeepAlive=no
IPQoS=none

服务器自己的 `args` 位于这些默认值之前。OpenSSH 对相关选项采用第一个值，因此可按服务器覆盖默认值：

```yaml
args: ["-o", "ConnectTimeout=30"]
```

连接时 `TERM` 会设置为 `xterm-256color`，避免远端缺少 `xterm-ghostty` terminfo 时出现提示符或光标错位。

## 密码认证

密码连接统一通过 ASKPASS：在权限为 `0700` 的临时目录内建立 Unix socket，仅向目标 SSH 直接启动且通过进程身份校验的 helper 提供密码。代理 SSH 的 helper 和无关进程不会获得目标密码；跳板机应单独配置密钥或其他认证方式。

ASKPASS 只回答 SSH 的密码提示，不回答主机确认或私钥 passphrase，也不会在登录后的远端程序再次显示 `password:` 时自动输入。不再使用无法区分目标和跳板机提示的 `sshpass`。

配置文件和导出的 inventory 可能包含明文密码。不要提交 `servers.yaml`，导出 INI 时应限制权限：

```bash
(umask 077; s --ansible-ini > hosts.ini)
```

## Ansible

直接把程序作为动态 inventory：

```bash
ansible all -i "$(command -v s)" -m ping
ansible production -i "$(command -v s)" -m shell -a "uptime"
```

批量执行的快捷形式：

```bash
s run "uptime"
s run -g production "df -h"
```

主机名称必须唯一。`all` 和 `_meta` 是保留分组名。INI 主机名不能包含 `[]`、冒号或换行；遇到这类名称请使用动态 JSON inventory。

## Ghostty

在 macOS Ghostty 1.3.1 或更新版本中，连接期间标签会固定为服务器 `name`，空名称则使用 `host`。SSH 退出或连接失败后恢复自动标题。

此功能使用 Ghostty 的 AppleScript 接口。macOS 首次调用时可能请求自动化权限；接口不可用时会显示错误并继续连接。tmux 中不会修改 Ghostty 标签。

若希望所有手动 SSH 会话也使用兼容终端定义，可在 Ghostty 配置中加入：

```ini
term = xterm-256color
```

## 开发

```bash
go test ./...
go test -race ./...
go vet ./...
git diff --check
```

测试使用本地替身，不连接真实远端主机。安装 `expect` 和 `ansible-inventory` 后，会覆盖伪终端交互、ASKPASS 认证隔离、Ghostty 标题以及 inventory 往返。

面向用户的行为变更必须同步更新 `VERSION` 和 `CHANGELOG.md`。维护者发布版本时，在提交后执行：

```bash
make release
```

该目标会重新运行测试与静态检查，要求工作区干净、当前分支为 `origin` 的默认分支，并校验 `VERSION` 为无前导零的三段数字 `X.Y.Z`（不支持预发布或构建后缀）。随后创建对应的 `vX.Y.Z` 标签，原子推送明确的分支引用和标签。推送失败时先核实远端：仅清理本次创建且确认未发布的本地标签；远端已有标签或状态无法确认时保留本地标签，需人工核查后重试。禁止覆盖已发布标签。

GitHub 发布工作流校验标签与 `VERSION` 一致，并要求 `CHANGELOG.md` 有对应的非空版本说明；同标签发布串行执行，任务超时为 20 分钟，Action 固定完整提交 SHA。

## 贡献

欢迎提交问题和 Pull Request。提交前请：

1. 说明可复现的输入、实际结果与预期结果。
2. 为行为修改添加一个能防止回归的测试。
3. 运行开发章节中的检查。
4. 不要提交真实主机、用户名、密码、私钥或 inventory 输出。

报告安全问题时，请勿在公开 issue 中附带真实凭据或服务器地址。
