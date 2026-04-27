# byo-node-precheck.sh

Phase 2 BYO node readiness check. Run on the partner's host to verify
the node can host hybrid-cloud workloads before issuing a node token.

## Usage

```bash
# Default — checks against qlaud.net + mux.qlaud.net:8443
sudo ./byo-node-precheck.sh

# Override targets (staging / pre-prod)
MAIN_API_HOST=staging.qlaud.net \
MUX_HOST=mux-staging.qlaud.net \
MUX_PORT=8443 \
sudo ./byo-node-precheck.sh
```

Exit code:
- `0` — GO (all checks pass; warnings allowed)
- `1` — NO-GO (one or more blocker)

## Checks

| Check | Source |
|---|---|
| Kernel ≥ 5.15 | `uname -r` |
| IOMMU active | `/sys/class/iommu/` + `dmesg` |
| vfio-pci module | `lsmod` + `modinfo` |
| NVIDIA driver | `/proc/driver/nvidia/version` |
| Outbound TCP `:443` to main-api | bash `/dev/tcp/...` |
| Outbound TCP `:8443` to mux endpoint | bash `/dev/tcp/...` |
| Free RAM ≥ 8 GiB | `/proc/meminfo` MemAvailable |
| Free disk ≥ 100 GiB on `/var` | `df -P` |

## Tunable thresholds (env vars)

| Var | Default |
|---|---|
| `MAIN_API_HOST` | `qlaud.net` |
| `MUX_HOST` | `mux.qlaud.net` |
| `MUX_PORT` | `8443` |
| `MIN_KERNEL_MAJOR` / `MIN_KERNEL_MINOR` | `5` / `15` |
| `MIN_FREE_RAM_GB` | `8` |
| `MIN_FREE_DISK_GB` | `100` |
| `DISK_PATH` | `/var` |

## Dependencies

Only standard CLI: `bash` (≥ 4), `curl`, `awk`, `grep`, `df`, `lsmod`,
`modinfo`, `uname`, `dmesg`. No package install required on a typical
Ubuntu 22.04 / 24.04 host.

## Operator workflow

1. Send the script to the partner: `scp byo-node-precheck.sh partner:~/`
2. Partner runs it as themselves (script uses `dmesg` which may need
   sudo on locked-down hosts; warn the partner if blockers reference
   IOMMU/vfio).
3. Partner pastes output back to the operator.
4. Operator addresses NO-GO items; proceeds to `byo-node-onboarding.md`
   when GO.
