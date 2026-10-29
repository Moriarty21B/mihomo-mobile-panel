set -eu
DIR=/data/other/mihomo
ports() { netstat -ln 2>/dev/null; }
has_port() { ports | awk -v p="$1" -v proto="$2" '$1 ~ proto && $4 ~ (":" p "$") {found=1} END {exit !found}'; }
ready() { has_port 7892 '^tcp' && has_port 1053 '^tcp' && has_port 1053 '^udp'; }
pids() {
  for f in /proc/[0-9]*/exe; do
    e=$(readlink "$f" 2>/dev/null || true)
    case "$e" in "$DIR/mihomo"|"$DIR/mihomo (deleted)") p=${f#/proc/}; echo "${p%/exe}";; esac
  done
}
# Delete by current line number, descending: handles repeated entries and old per-device rules without eval.
cleanup() {
  nums=$(iptables -t nat -L PREROUTING -n --line-numbers | awk '
    $2=="MIHOMO_TCP" {print $1; next}
    $2=="REDIRECT" && ($3=="tcp" || $3=="udp") && /dpt:53( |$)/ && /redir ports 1053( |$)/ {print $1}' | sort -rn)
  for n in $nums; do iptables -t nat -D PREROUTING "$n" || return 1; done
}
enabled() { iptables -t nat -S PREROUTING | grep -q -- '-j MIHOMO_TCP'; }
dns_enabled() { iptables -t nat -S PREROUTING | grep -E -- '--dport 53 .*--to-ports 1053' >/dev/null; }
lock() {
  test ! -d /tmp/mihomo-manager.lock || { echo '旧管理工具正在运行，请完成后重试。'; exit 1; }
  exec 9>/tmp/mihomo-manager.flock
  flock -n 9 || { echo '另一管理操作正在运行，请稍后重试。'; exit 1; }
  trap 'exit 130' INT TERM HUP
}
turn_on() {
  ready || { echo '7892 或 TCP/UDP 1053 未监听，未修改规则。请先启动 Mihomo。'; return 1; }
  cleanup || return 1
  iptables -t nat -N MIHOMO_TCP 2>/dev/null || iptables -t nat -L MIHOMO_TCP -n >/dev/null || return 1
  iptables -t nat -F MIHOMO_TCP || return 1
  # Local/private destinations must stay reachable; fake-IP 198.18.0.0/15 is intentionally NOT bypassed.
  for cidr in 0.0.0.0/8 10.0.0.0/8 100.64.0.0/10 127.0.0.0/8 169.254.0.0/16 172.16.0.0/12 192.168.0.0/16 224.0.0.0/4 240.0.0.0/4; do
    iptables -t nat -A MIHOMO_TCP -d "$cidr" -j RETURN || return 1
  done
  iptables -t nat -A MIHOMO_TCP -p tcp -j REDIRECT --to-ports 7892 || return 1
  iptables -t nat -A PREROUTING -i br-lan -p tcp -j MIHOMO_TCP || return 1
  iptables -t nat -I PREROUTING 1 -i br-lan -p udp --dport 53 -j REDIRECT --to-ports 1053 || return 1
  iptables -t nat -I PREROUTING 1 -i br-lan -p tcp --dport 53 -j REDIRECT --to-ports 1053 || return 1
}
safe_on() {
  ready || { echo '7892 或 TCP/UDP 1053 未监听，保留原规则；请先启动 Mihomo，或用菜单 2 恢复直连。'; return 1; }
  if ! turn_on; then cleanup; echo '开启失败，已撤销本工具的入口，避免留下半套规则。'; return 1; fi
  echo '已开启：全 LAN TCP + TCP/UDP DNS；UDP 业务保持原状。'
}
log_path() {
  for f in "$DIR/manager.log" "$DIR/mihomo.log" "$DIR/run.log" "$DIR/log.txt" /tmp/mihomo.log; do
    if [ -f "$f" ]; then echo "$f"; return; fi
  done
  return 1
}
