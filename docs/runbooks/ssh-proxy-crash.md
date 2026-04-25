# Runbook — ssh-proxy crash

## Detect
- All SSH attempts to `*.qlaud.net` return `Connection refused`.
- Process gone: `ssh h20a 'pgrep -fa bin/ssh-proxy' → empty`.
- main-api keeps issuing tickets but nobody is consuming them.

## Stabilise
1. Existing user sessions over the proxy are already dropped — there's
   nothing to preserve. Bring it back as fast as possible.

## Diagnose
1. Check the process state:
   ```bash
   ssh h20a 'pgrep -fa bin/ssh-proxy; tail -100 ~/hybrid-cloud/logs/ssh-proxy.log'
   ```
2. Common causes:
   - **Port 22 already taken**: ensure systemd `sshd` isn't competing for
     `:22` on the same interface (we run sshd on a non-default port).
   - **Host key file missing/perms**: see `ssh-proxy-hostkey` 0o600 owner.
   - **main-api `/internal/ssh-ticket` returns 5xx**: the proxy logs a
     ticket failure and may exit on repeated 5xx. Bring main-api back
     first, then the proxy.

## Recover
1. Restart:
   ```bash
   ssh h20a 'cd ~/hybrid-cloud && pkill -f bin/ssh-proxy; sleep 1; nohup ./run-proxy.sh > logs/ssh-proxy.log 2>&1 & disown'
   ```
2. Verify:
   ```bash
   ssh -o BatchMode=yes -o ConnectTimeout=5 -p 22 invalid@proxy.qlaud.net true 2>&1 | grep -i "permission\|administratively"
   ```
   A `direct-tcpip` rejection means the proxy is up. (We expect failure here — we're checking the proxy responded, not that auth worked.)

## Post-mortem
- If the proxy crashed from a panic: capture the full log and pin the goroutine count if possible.
- If it kept dying on restart: bind a different port temporarily and add an alert on `up{job="ssh-proxy"}`.
