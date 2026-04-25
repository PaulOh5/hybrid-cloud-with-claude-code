# Runbook — Node Failure

## Detect
- Grafana **Hybrid Cloud Overview** shows `gpu_slot_used` for one node-name flat-lining at 0 unexpectedly.
- `up{job="compute-agent"} == 0` for ≥ 5 min.
- `instance_total{state="failed"}` rising.
- User reports: "내 인스턴스가 갑자기 멈췄어요".

## Stabilise
1. Verify it's the node, not the API:
   ```bash
   curl -sf http://127.0.0.1:8080/admin/nodes -H "Authorization: Bearer $MAIN_API_ADMIN_TOKEN" | jq '.nodes[] | select(.status != "online") | {node_name, status, last_heartbeat_at}'
   ```
2. If the node has been offline > 1 minute, the agent stale-sweeper has
   already flipped the row. Slot reservations are paused for that node —
   no new instances will land on it until heartbeat resumes.
3. Notify users with running instances on the affected node:
   ```sql
   select i.id, i.owner_id, u.email
   from instances i
   join users u on u.id = i.owner_id
   join nodes n on n.id = i.node_id
   where n.node_name = '<node>' and i.state in ('running', 'provisioning');
   ```

## Diagnose
1. SSH into the host that runs the agent.
2. `systemctl status compute-agent` (or `pgrep -af compute-agent` if running via nohup).
3. Tail the agent log: `tail -200 ~/hybrid-cloud/logs/compute-agent.log`.
4. Common causes:
   - **libvirtd down**: `systemctl status libvirtd` — restart if needed.
   - **Network partition to main-api**: `nc -vz <main-api-host> 8081`.
   - **Out of disk**: `df -h /var/lib/hybrid` — purge old qcow2 + ISOs.
   - **GPU vfio bind regression**: see Phase 4 bootstrap script in `scripts/node-bootstrap.sh`.

## Recover
1. Fix the root cause (start libvirtd, free disk, reboot, etc.).
2. Restart the agent:
   ```bash
   ssh <node> 'cd ~/hybrid-cloud && pkill -f bin/compute-agent; sleep 1; nohup ./run-agent.sh > logs/compute-agent.log 2>&1 & disown'
   ```
3. Watch heartbeat resume:
   ```bash
   curl -s http://127.0.0.1:8080/admin/nodes -H "Authorization: Bearer $MAIN_API_ADMIN_TOKEN" | jq '.nodes[] | select(.node_name == "<node>") | .status'
   ```
   Expect `"online"` within ~15 s.
4. If running VMs were lost (libvirtd restarted, host rebooted): force-stop the affected instance rows from the admin dashboard so users see the failure quickly:
   ```bash
   curl -X DELETE -H "Authorization: Bearer $MAIN_API_ADMIN_TOKEN" \
     http://127.0.0.1:8080/admin/instances/<id>
   ```

## Post-mortem
- Capture the agent log around the incident window.
- Note the duration and how many users were affected.
- File a follow-up if:
  - libvirtd died spontaneously,
  - heartbeat TTL needs tuning (currently 60 s),
  - we should auto-restart the agent via systemd (see Phase 10 follow-up).
