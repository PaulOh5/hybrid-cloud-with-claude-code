#!/usr/bin/env bash
# One-time host preparation for a GPU node before compute-agent can schedule
# passthrough VMs. Idempotent — safe to re-run.
#
# Usage:  sudo ./scripts/node-bootstrap.sh <pci-addr> [<pci-addr> ...]
#         sudo ./scripts/node-bootstrap.sh 0000:16:00.0 0000:43:00.0
#
# What it does:
#   1. Checks VT-d / AMD-Vi is enabled (IOMMU groups populated).
#   2. Loads vfio-pci kernel module.
#   3. For each GPU PCI address: enumerates the IOMMU group, unbinds every
#      member from its current driver, and binds the group to vfio-pci by
#      writing vendor:device IDs to /sys/bus/pci/drivers/vfio-pci/new_id.
#   4. Pins the bindings across reboots via /etc/modprobe.d/vfio.conf so the
#      host boots with vfio-pci instead of nvidia.
#
# Run once per node, re-run whenever new GPUs are added.
set -euo pipefail

die() { echo "ERROR: $*" >&2; exit 1; }
info() { echo "[bootstrap] $*"; }

[[ $EUID -eq 0 ]] || die "must run as root"
[[ $# -ge 1 ]] || die "usage: $0 <pci-addr> [<pci-addr> ...]"

# --- 1. IOMMU check --------------------------------------------------------
if [[ ! -d /sys/kernel/iommu_groups ]] || [[ -z "$(ls /sys/kernel/iommu_groups 2>/dev/null)" ]]; then
    die "IOMMU not enabled — enable VT-d/AMD-Vi in BIOS and boot with intel_iommu=on (or amd_iommu=on)"
fi
info "IOMMU enabled ($(ls /sys/kernel/iommu_groups | wc -l) groups)"

# --- 2. Load vfio-pci ------------------------------------------------------
modprobe vfio-pci
info "vfio-pci module loaded"

# --- 3. Rebind requested GPUs + their IOMMU group siblings ----------------
declare -A IDS_TO_PIN

rebind_device() {
    local pci=$1
    local dev_path="/sys/bus/pci/devices/$pci"
    [[ -d $dev_path ]] || die "device not found: $pci"

    local vendor device
    vendor=$(cat "$dev_path/vendor") # e.g. 0x10de
    device=$(cat "$dev_path/device") # e.g. 0x2b85
    vendor=${vendor#0x}
    device=${device#0x}
    IDS_TO_PIN["${vendor} ${device}"]=1

    # Unbind from current driver if any.
    if [[ -e $dev_path/driver ]]; then
        local current
        current=$(basename "$(readlink "$dev_path/driver")")
        if [[ $current == "vfio-pci" ]]; then
            info "  $pci already on vfio-pci"
            return
        fi
        info "  $pci unbinding from $current"
        echo "$pci" > "$dev_path/driver/unbind"
    fi

    # Register the vendor:device pair with vfio-pci so it picks the device up.
    if ! grep -qE "^${vendor} ${device}( |$)" /sys/bus/pci/drivers/vfio-pci/ids_list 2>/dev/null; then
        echo "${vendor} ${device}" > /sys/bus/pci/drivers/vfio-pci/new_id 2>/dev/null || true
    fi

    # Force override so the next probe lands on vfio-pci, then bind.
    echo "vfio-pci" > "$dev_path/driver_override"
    echo "$pci" > /sys/bus/pci/drivers/vfio-pci/bind 2>/dev/null || true
    info "  $pci bound to vfio-pci (${vendor}:${device})"
}

for gpu_pci in "$@"; do
    group_dir=$(readlink -f "/sys/bus/pci/devices/$gpu_pci/iommu_group")
    [[ -d $group_dir ]] || die "no iommu group for $gpu_pci"
    group_id=$(basename "$group_dir")
    info "GPU $gpu_pci (iommu group $group_id):"

    for member in "$group_dir"/devices/*; do
        rebind_device "$(basename "$member")"
    done
done

# --- 4. Persist across reboots --------------------------------------------
CONF=/etc/modprobe.d/hybrid-vfio.conf
{
    echo "# managed by scripts/node-bootstrap.sh — do not edit by hand"
    echo "# hybrid-cloud GPU passthrough: bind these IDs to vfio-pci at boot."
    ids=""
    for pair in "${!IDS_TO_PIN[@]}"; do
        ids="${ids}${pair/ /:},"
    done
    ids=${ids%,}
    echo "options vfio-pci ids=${ids}"
    echo "softdep nvidia pre: vfio-pci"
    echo "softdep nouveau pre: vfio-pci"
} > "$CONF"
info "wrote $CONF (ids=${ids})"

# Rebuild initramfs so the pin takes effect from the very first kernel module
# load on the next boot. Without this, nvidia still wins the race.
if command -v update-initramfs >/dev/null 2>&1; then
    info "updating initramfs (this may take ~30s)"
    update-initramfs -u
fi

info "done. reboot recommended so the initramfs change is exercised."
