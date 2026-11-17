#!/usr/bin/env bash
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
  echo '请以 root 身份运行此脚本。' >&2
  exit 1
fi
if [[ ! -r /etc/os-release ]] || ! command -v apt-get >/dev/null; then
  echo '此脚本需要使用 apt 的 Linux 系统。' >&2
  exit 1
fi

root_home="$(getent passwd root 2>/dev/null | cut -d: -f6 || true)"
export HOME="${root_home:-/root}"

append_line_if_missing() {
  local line="$1" file="$2"
  mkdir -p "$(dirname "$file")"
  touch "$file"
  if [[ -s "$file" && -n "$(tail -c1 "$file")" ]]; then
    printf '\n' >> "$file"
  fi
  grep -qxF -- "$line" "$file" || printf '%s\n' "$line" >> "$file"
}

export DEBIAN_FRONTEND=noninteractive
dpkg_opts=(-o 'Dpkg::Options::=--force-confdef' -o 'Dpkg::Options::=--force-confold')
apt-get update
apt-get install -y "${dpkg_opts[@]}" vim ca-certificates curl wget git unzip xz-utils htop tmux jq zsh build-essential clang pkg-config libssl-dev python3 python3-pip python3-venv python3-dev iproute2 iputils-ping dnsutils
apt-get upgrade -y "${dpkg_opts[@]}"

# 安装到运行脚本的 root 用户。
if ! command -v docker >/dev/null; then
  curl -fsSL https://get.docker.com | sh
fi
if [[ ! -d "$HOME/.oh-my-zsh" ]]; then
  omz_installer="$(curl -fsSL https://raw.githubusercontent.com/ohmyzsh/ohmyzsh/master/tools/install.sh)"
  RUNZSH=no CHSH=no sh -c "$omz_installer" -- --unattended
fi
chsh -s "$(command -v zsh)" root
append_line_if_missing "alias dk='docker'" "$HOME/.zshrc"

mkdir -p /etc/security/limits.d
cat > /etc/security/limits.d/99-init.conf <<'EOF'
* soft nofile 327680
* hard nofile 655360
root soft nofile 327680
root hard nofile 655360
EOF

apply_sysctl() {
  local conf="$1"
  if ! sysctl -p "$conf"; then
    echo "警告: 应用 $conf 失败（可能处于容器或受限内核中），继续执行。" >&2
  fi
}

cat > /etc/sysctl.d/99-init.conf <<'EOF'
net.core.rmem_max=600000000
net.core.wmem_max=600000000
net.core.default_qdisc=fq
net.ipv4.tcp_congestion_control=bbr
net.core.somaxconn=65535
net.ipv4.tcp_max_syn_backlog=65535
net.core.netdev_max_backlog=65535
net.ipv4.tcp_tw_reuse=1
net.ipv4.tcp_fin_timeout=15
net.ipv4.ip_local_port_range=1024 65535
vm.overcommit_memory=1
fs.inotify.max_user_watches=524288
fs.file-max=2097152
EOF
modprobe tcp_bbr 2>/dev/null || true
apply_sysctl /etc/sysctl.d/99-init.conf

mem_kib=$(awk '/^MemTotal:/ {print $2}' /proc/meminfo)
if [[ ! $mem_kib =~ ^[0-9]+$ ]]; then
  echo '无法读取物理内存大小。' >&2
  exit 1
fi
if (( mem_kib < 2 * 1024 * 1024 )); then
  if [[ $(awk 'NR > 1 {print; exit}' /proc/swaps) ]]; then
    echo '已有启用的 swap，跳过创建。'
  elif [[ -e /swapfile || -L /swapfile ]]; then
    echo '/swapfile 已存在但未启用，为避免覆盖，请手动检查。' >&2
    exit 1
  else
    # 避免覆盖已有文件；失败时保留文件供检查。
    ( umask 077; set -o noclobber; : > /swapfile )
    dd if=/dev/zero of=/swapfile bs=1M count=2048 status=progress
    chmod 600 /swapfile
    mkswap /swapfile
    swapon /swapfile
    grep -qE '^[[:space:]]*/swapfile[[:space:]]' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab
    echo 'vm.swappiness=10' > /etc/sysctl.d/99-swap.conf
    apply_sysctl /etc/sysctl.d/99-swap.conf
  fi
else
  echo '物理内存不低于 2 GiB，跳过创建 swap。'
fi

export NVM_DIR="$HOME/.nvm"
if [[ ! -s "$NVM_DIR/nvm.sh" ]]; then
  curl -fsSL https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.7/install.sh | PROFILE=/dev/null bash
fi
set +euo pipefail
. "$NVM_DIR/nvm.sh" && nvm install --lts && nvm alias default 'lts/*'
nvm_status=$?
set -euo pipefail
if (( nvm_status != 0 )); then
  exit "$nvm_status"
fi
append_line_if_missing 'export NVM_DIR="$HOME/.nvm"' "$HOME/.zshrc"
append_line_if_missing '[ -s "$NVM_DIR/nvm.sh" ] && . "$NVM_DIR/nvm.sh"' "$HOME/.zshrc"
