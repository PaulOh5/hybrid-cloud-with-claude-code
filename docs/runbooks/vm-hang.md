# Runbook — VM Hang

## Detect
- User: "SSH가 안 들어가요" but `instance_total{state="running"}` is unchanged.
- `vm_internal_ip` is set in the DB but `ssh -J proxy.qlaud.net …` times out.
- Agent log: tunnel listener accepts then errors with timeout dialing the VM.

## Stabilise
1. Mark the instance failed so the user sees something concrete:
   ```bash
   # Optional — only if the user confirms it's not transient.
   curl -X DELETE -H "Authorization: Bearer $MAIN_API_ADMIN_TOKEN" \
     http://127.0.0.1:8080/admin/instances/<id>
   ```
2. Don't reboot the host — just kill the offending domain.

## Diagnose
1. SSH into the node hosting the VM.
2. Confirm libvirt thinks it's running:
   ```bash
   sudo virsh list --all | grep <instance_id>
   ```
3. Check guest console:
   ```bash
   sudo virsh console <instance_id>
   # Ctrl+] to exit
   ```
4. If the console scrolls kernel panics → kernel/initrd regression. Rebuild the base image (`scripts/refresh-base-image.sh` if added) or roll back to the previous snapshot.
5. If the console is dead → typically vfio device wedge. Check dmesg for "vfio_pci ... DMAR-IR".

## Recover
1. Force-destroy the libvirt domain:
   ```bash
   sudo virsh destroy <instance_id>
   sudo virsh undefine <instance_id>
   ```
2. Reset each passthrough GPU before re-use:
   ```bash
   sudo bash -c 'echo 1 > /sys/bus/pci/devices/0000:<bdf>/reset'
   ```
   The agent normally does this on `DestroyInstance`; running it manually
   handles cases where the agent itself was wedged.
3. Re-create through the API (admin) or have the user click 새 인스턴스.

## Post-mortem
- Save the guest console scroll-back if anything anomalous.
- Capture the host `dmesg -T | tail -200` and the agent log slice.
- If GPU reset failed: open a Phase 10 follow-up to gate destroy on a successful reset.
