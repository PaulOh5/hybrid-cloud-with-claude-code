# Runbooks

Operational checklists for the most common incidents on Hybrid Cloud.
Each runbook is structured the same way:

1. **Detect** — what's the signal that triggered this?
2. **Stabilise** — first action that limits blast radius.
3. **Diagnose** — narrow the cause down.
4. **Recover** — bring the system back to healthy.
5. **Post-mortem** — what to capture for follow-up.

| Runbook | When to use |
| --- | --- |
| [node-failure.md](./node-failure.md) | A compute node stops heart-beating or marks itself degraded. |
| [vm-hang.md](./vm-hang.md) | A guest VM is unresponsive but libvirt thinks it's running. |
| [ssh-proxy-crash.md](./ssh-proxy-crash.md) | Users can't open `ssh -J proxy.qlaud.net …`. |
| [db-failure.md](./db-failure.md) | Postgres is down, slow, or returning errors. |
| [full-rollback.md](./full-rollback.md) | Latest deploy is broken; revert services + DB to last known good. |
| [loki-setup.md](./loki-setup.md) | Wiring Loki + Promtail behind the existing JSON logs. |

Backup & restore scripts live at [`scripts/backup.sh`](../../scripts/backup.sh)
and [`scripts/restore.sh`](../../scripts/restore.sh). The `--dry-run` mode is
the supported way to verify connectivity without producing artifacts.
