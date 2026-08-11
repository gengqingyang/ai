package tools

const installationEvidenceCommand = `# evidence:installation
printf 'kv\tcollected_at\t'; date -Ins
printf 'kv\tos\t'; . /etc/os-release 2>/dev/null; printf '%s\n' "${PRETTY_NAME:-unknown}"
printf 'kv\tsystem_state\t%s\n' "$(systemctl is-system-running 2>/dev/null || true)"
printf 'kv\tfailed_services\t'; systemctl --failed --no-legend --plain 2>/dev/null | awk 'NF {n++} END {print n+0}'
printf 'kv\tuptime_seconds\t'; awk '{print int($1)}' /proc/uptime
printf 'kv\tmemory_available_mb\t'; awk '/MemAvailable:/ {print int($2/1024)}' /proc/meminfo
for mount in / /boot /xyapp; do
  df -Pk "$mount" 2>/dev/null | awk -v m="$mount" 'NR==2 {gsub(/%/, "", $5); printf "mount\t%s\t%s\n", m, $5}'
done
for name in activation agent installIPK; do
  if pgrep -x "$name" >/dev/null 2>&1; then state=running; else state=stopped; fi
  printf 'process\t%s\t%s\n' "$name" "$state"
done`

const trafficEvidenceCommand = `# evidence:traffic
seconds={{SAMPLE_SECONDS}}
iface=$(ip route show default 2>/dev/null | awk 'NR==1 {print $5}')
printf 'kv\tcollected_at\t'; date -Ins
printf 'kv\tinterface\t%s\n' "$iface"
printf 'kv\tsample_seconds\t%s\n' "$seconds"
for stat in rx_bytes rx_packets rx_errors rx_dropped tx_bytes tx_packets tx_errors tx_dropped; do
  value=$(cat "/sys/class/net/$iface/statistics/$stat" 2>/dev/null || printf 'missing')
  printf 'counter0\t%s\t%s\n' "$stat" "$value"
done
sleep "$seconds"
for stat in rx_bytes rx_packets rx_errors rx_dropped tx_bytes tx_packets tx_errors tx_dropped; do
  value=$(cat "/sys/class/net/$iface/statistics/$stat" 2>/dev/null || printf 'missing')
  printf 'counter1\t%s\t%s\n' "$stat" "$value"
done`

const pluginEvidenceCommand = `# evidence:plugin
printf 'kv\tcollected_at\t'; date -Ins
for spec in 'nextipk.service|nextipk' 'virtualrouter.service|virtualrouter' 'sysinfo.service|sysinfo' 'iaas-service-keepalive.service|iaas-service-ke'; do
  unit=${spec%%|*}
  process=${spec##*|}
  active=$(systemctl is-active "$unit" 2>/dev/null || true)
  sub=$(systemctl show "$unit" --property=SubState 2>/dev/null | sed 's/^SubState=//')
  fragment=$(systemctl show "$unit" --property=FragmentPath 2>/dev/null | sed 's/^FragmentPath=//')
  exit_code=$(systemctl show "$unit" --property=ExecMainStatus 2>/dev/null | sed 's/^ExecMainStatus=//')
  pid=$(pgrep -xo "$process" 2>/dev/null)
  executable=not-running
  if test -n "$pid"; then executable=$(readlink -f "/proc/$pid/exe" 2>/dev/null); fi
  printf 'component\t%s\t%s\t%s\t%s\t%s\t%s\n' "$unit" "$active" "$sub" "$exit_code" "$fragment" "$executable"
done`

const kernelEvidenceCommand = `# evidence:kernel
printf 'kv\tcollected_at\t'; date -Ins
printf 'kv\tcurrent\t'; uname -r
printf 'kv\tdefault\t'; grubby --default-kernel 2>/dev/null | sed 's#^/boot/vmlinuz-##'
if rpm -qf "/boot/vmlinuz-$(uname -r)" >/dev/null 2>&1; then current_installed=yes; else current_installed=no; fi
printf 'kv\tcurrent_installed\t%s\n' "$current_installed"
rpm -q kernel --qf 'installed\t%{VERSION}-%{RELEASE}.%{ARCH}\n' 2>/dev/null`

const networkEvidenceCommand = `# evidence:network
printf 'kv\tcollected_at\t'; date -Ins
ip -o link show 2>/dev/null | awk -F': ' '{name=$2; sub(/@.*/, "", name); state=($0 ~ /state UP/) ? "up" : "down"; printf "interface\t%s\t%s\n", name, state}'
ip -o -4 addr show scope global 2>/dev/null | awk '{name=$2; sub(/@.*/, "", name); printf "address\t%s\t%s\n", name, $4}'
ip route show default 2>/dev/null | awk 'NR==1 {printf "default_route\t%s\t%s\n", $5, $3}'
awk '/^nameserver[[:space:]]+/ {printf "dns\t%s\n", $2}' /etc/resolv.conf 2>/dev/null
iface=$(ip route show default 2>/dev/null | awk 'NR==1 {print $5}')
for stat in rx_errors rx_dropped tx_errors tx_dropped; do
  value=$(cat "/sys/class/net/$iface/statistics/$stat" 2>/dev/null || printf 'missing')
  printf 'counter\t%s\t%s\n' "$stat" "$value"
done`
