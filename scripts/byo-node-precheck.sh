#!/usr/bin/env bash
# byo-node-precheck.sh — operator's pre-flight check for a Phase 2 BYO
# node. Run as the partner's user on the partner's host (sudo for
# kernel + IOMMU probes). Exits 0 (GO) when every check passes; 1
# (NO-GO) on any blocker. Output is structured so the operator can
# copy/paste the failures into the partner email.
#
# Plan §3.3 acceptance:
#   - IOMMU enabled
#   - vfio-pci module loaded
#   - kernel >= 5.15
#   - NVIDIA driver present
#   - outbound 443 -> main-api reachable
#   - outbound 8443 -> ssh-proxy mux endpoint reachable (P11)
#   - free RAM, free disk
#
# No external dependencies — the script uses curl, ip, lsmod, free,
# df, uname, dmesg, getconf and reads /proc, /sys.

set -u
set -o pipefail

# --- defaults the operator can override via env -----------------------
MAIN_API_HOST="${MAIN_API_HOST:-qlaud.net}"
MUX_HOST="${MUX_HOST:-mux.qlaud.net}"
MUX_PORT="${MUX_PORT:-8443}"
MIN_KERNEL_MAJOR="${MIN_KERNEL_MAJOR:-5}"
MIN_KERNEL_MINOR="${MIN_KERNEL_MINOR:-15}"
MIN_FREE_RAM_GB="${MIN_FREE_RAM_GB:-8}"
MIN_FREE_DISK_GB="${MIN_FREE_DISK_GB:-100}"
DISK_PATH="${DISK_PATH:-/var}"

PASS_COUNT=0
FAIL_COUNT=0
WARN_COUNT=0
declare -a FAILURES=()
declare -a WARNINGS=()

# Colorised output if stdout is a TTY.
if [ -t 1 ]; then
    GREEN='\033[0;32m'
    YELLOW='\033[0;33m'
    RED='\033[0;31m'
    BOLD='\033[1m'
    NC='\033[0m'
else
    GREEN=''
    YELLOW=''
    RED=''
    BOLD=''
    NC=''
fi

pass() {
    PASS_COUNT=$((PASS_COUNT + 1))
    printf "  ${GREEN}✓${NC} %s\n" "$1"
}
fail() {
    FAIL_COUNT=$((FAIL_COUNT + 1))
    FAILURES+=("$1")
    printf "  ${RED}✗${NC} %s\n" "$1"
    if [ -n "${2:-}" ]; then
        printf "      %s\n" "$2"
    fi
}
warn() {
    WARN_COUNT=$((WARN_COUNT + 1))
    WARNINGS+=("$1")
    printf "  ${YELLOW}!${NC} %s\n" "$1"
    if [ -n "${2:-}" ]; then
        printf "      %s\n" "$2"
    fi
}

section() {
    printf "\n${BOLD}%s${NC}\n" "$1"
}

# --- checks -----------------------------------------------------------

check_kernel() {
    section "Kernel"
    local version major minor
    version="$(uname -r)"
    major="${version%%.*}"
    minor="${version#*.}"
    minor="${minor%%.*}"
    if [ -z "$major" ] || [ -z "$minor" ]; then
        fail "kernel: cannot parse version" "uname -r returned: $version"
        return
    fi
    if [ "$major" -gt "$MIN_KERNEL_MAJOR" ] || \
       { [ "$major" -eq "$MIN_KERNEL_MAJOR" ] && [ "$minor" -ge "$MIN_KERNEL_MINOR" ]; }; then
        pass "kernel ${version} (>= ${MIN_KERNEL_MAJOR}.${MIN_KERNEL_MINOR})"
    else
        fail "kernel ${version} below minimum ${MIN_KERNEL_MAJOR}.${MIN_KERNEL_MINOR}" \
             "Upgrade the kernel before continuing."
    fi
}

check_iommu() {
    section "IOMMU (passthrough requirement)"
    # /sys/class/iommu/ has at least one entry when an IOMMU is active.
    if [ -d /sys/class/iommu ] && [ -n "$(ls -A /sys/class/iommu 2>/dev/null)" ]; then
        pass "IOMMU active (/sys/class/iommu has entries)"
        return
    fi
    # Older kernels: dmesg mentions "DMAR: IOMMU enabled" or "AMD-Vi: AMD IOMMUv2 functionality not available".
    if dmesg 2>/dev/null | grep -qE "(DMAR: IOMMU enabled|AMD-Vi: AMD IOMMU)"; then
        pass "IOMMU active (dmesg)"
        return
    fi
    fail "IOMMU not active" \
         "Enable VT-d/AMD-Vi in BIOS and add intel_iommu=on (or amd_iommu=on) to GRUB_CMDLINE_LINUX."
}

check_vfio() {
    section "vfio-pci module"
    if lsmod 2>/dev/null | grep -q '^vfio_pci\b\|^vfio-pci\b'; then
        pass "vfio-pci module loaded"
    elif modinfo vfio-pci >/dev/null 2>&1; then
        warn "vfio-pci available but not loaded" \
             "Add 'vfio-pci' to /etc/modules-load.d/ and reboot, or run 'sudo modprobe vfio-pci'."
    else
        fail "vfio-pci module not available" \
             "Install kernel-modules-extra or rebuild with CONFIG_VFIO_PCI."
    fi
}

check_nvidia() {
    section "NVIDIA driver"
    if [ -d /proc/driver/nvidia ]; then
        local v=""
        if [ -r /proc/driver/nvidia/version ]; then
            # Format: "NVRM version: NVIDIA UNIX x86_64 Kernel Module  580.126.09  ..."
            v="$(grep -oE 'Kernel Module[[:space:]]+[0-9]+\.[0-9]+(\.[0-9]+)?' /proc/driver/nvidia/version 2>/dev/null \
                | grep -oE '[0-9]+\.[0-9]+(\.[0-9]+)?' || true)"
        fi
        if [ -n "$v" ]; then
            pass "NVIDIA driver loaded (version ${v})"
        else
            pass "NVIDIA driver loaded"
        fi
        return
    fi
    if lsmod 2>/dev/null | grep -q '^nvidia\b'; then
        pass "nvidia kernel module loaded"
        return
    fi
    fail "NVIDIA driver not loaded" \
         "Install the NVIDIA driver per the partner guide and reboot. (Older Pascal/Volta GPUs need driver 470+; Ada/Hopper 535+.)"
}

# tcp_check tests outbound TCP using bash's /dev/tcp redirect — works
# without nc on minimal hosts. Returns 0 on success, 1 on failure.
tcp_check() {
    local host="$1" port="$2" timeout="${3:-5}"
    timeout "$timeout" bash -c "exec 3<>/dev/tcp/$host/$port" 2>/dev/null
    local rc=$?
    exec 3<&- 2>/dev/null || true
    exec 3>&- 2>/dev/null || true
    return $rc
}

check_outbound() {
    section "Outbound network"
    if tcp_check "$MAIN_API_HOST" 443 5; then
        pass "main-api ${MAIN_API_HOST}:443 reachable"
    else
        fail "main-api ${MAIN_API_HOST}:443 unreachable" \
             "Allow outbound TCP/443 to ${MAIN_API_HOST} (most home networks already do)."
    fi
    if tcp_check "$MUX_HOST" "$MUX_PORT" 5; then
        pass "ssh-proxy mux ${MUX_HOST}:${MUX_PORT} reachable"
    else
        fail "ssh-proxy mux ${MUX_HOST}:${MUX_PORT} unreachable" \
             "Allow outbound TCP/${MUX_PORT} to ${MUX_HOST}. Some corporate firewalls block non-:443 outbound."
    fi
}

check_resources() {
    section "Resources"
    # free RAM in GiB
    local free_kb free_gb
    free_kb="$(awk '/^MemAvailable:/ {print $2}' /proc/meminfo 2>/dev/null || echo 0)"
    free_gb=$((free_kb / 1024 / 1024))
    if [ "$free_gb" -ge "$MIN_FREE_RAM_GB" ]; then
        pass "free RAM ${free_gb} GiB (>= ${MIN_FREE_RAM_GB})"
    else
        fail "free RAM ${free_gb} GiB below ${MIN_FREE_RAM_GB} GiB" \
             "VM allocation needs headroom; close memory-hungry processes or add RAM."
    fi

    # free disk on $DISK_PATH (typically /var/lib/hybrid lives under /var)
    local free_disk_kb free_disk_gb
    free_disk_kb="$(df -P "$DISK_PATH" 2>/dev/null | awk 'NR==2 {print $4}')"
    free_disk_gb=$((free_disk_kb / 1024 / 1024))
    if [ "$free_disk_gb" -ge "$MIN_FREE_DISK_GB" ]; then
        pass "free disk on ${DISK_PATH}: ${free_disk_gb} GiB (>= ${MIN_FREE_DISK_GB})"
    else
        fail "free disk on ${DISK_PATH}: ${free_disk_gb} GiB below ${MIN_FREE_DISK_GB} GiB" \
             "VM disks back onto ${DISK_PATH}; free space or move ${DISK_PATH} to a larger volume."
    fi
}

# --- main -------------------------------------------------------------

printf "${BOLD}byo-node-precheck${NC} — Phase 2 partner node readiness\n"
printf "Host:     %s\n" "$(hostname 2>/dev/null || echo unknown)"
printf "Date:     %s\n" "$(date -u +"%Y-%m-%dT%H:%M:%SZ" 2>/dev/null || date)"
printf "Targets:  main-api=%s mux=%s:%s\n" "$MAIN_API_HOST" "$MUX_HOST" "$MUX_PORT"

check_kernel
check_iommu
check_vfio
check_nvidia
check_outbound
check_resources

section "Result"
printf "  pass=%d  warn=%d  fail=%d\n" "$PASS_COUNT" "$WARN_COUNT" "$FAIL_COUNT"

if [ "$FAIL_COUNT" -gt 0 ]; then
    printf "\n${RED}${BOLD}NO-GO${NC} — %d blocker(s):\n" "$FAIL_COUNT"
    for f in "${FAILURES[@]}"; do
        printf "  - %s\n" "$f"
    done
    exit 1
fi

if [ "$WARN_COUNT" -gt 0 ]; then
    printf "\n${YELLOW}${BOLD}GO with warnings${NC} — %d item(s) need attention before production traffic:\n" "$WARN_COUNT"
    for w in "${WARNINGS[@]}"; do
        printf "  - %s\n" "$w"
    done
fi

printf "\n${GREEN}${BOLD}GO${NC} — node ready for Phase 2 onboarding.\n"
exit 0
