# Production host maintenance

## What updates on its own

- **Nightly:** `unattended-upgrades` installs Debian main and security updates, kernels included. It does not cover Docker's apt repository (`docker-ce`, `containerd.io`, `docker-compose-plugin`, `docker-buildx-plugin`) and never reboots, so a new kernel is installed but not running.
- **Monthly:** `coves-host-maintenance.timer` runs on the first Sunday of each month at 03:00 Pacific (11:00 UTC in winter, 10:00 UTC in summer). It runs `apt-get --with-new-pkgs upgrade`, which picks up the Docker packages, then reboots. The backup timers run at 02:17–03:07 UTC, hours earlier.

The reboot takes about 3 minutes. All containers use `restart: unless-stopped` and come back on their own. The AppView restarts once on boot because Postgres is not ready yet. That is expected.

A failed upgrade exits non-zero and skips the reboot. It shows in `systemctl --failed` and `journalctl -u coves-host-maintenance.service`. Nothing alerts on failure. A run missed while the host was down is skipped, not run at the next boot.

To keep a package back (for example a Docker Compose major version that has not been tested against the deploy commands), `sudo apt-mark hold <package>`; the monthly upgrade skips held packages.

## Install or update

The script is installed outside `/opt/coves` for the same reason as the backup scripts (see `PRODUCTION_BACKUPS.md`). After changing it, re-run the `install` line.

```bash
cd /opt/coves
sudo install -m 755 scripts/host-maintenance.sh /usr/local/sbin/coves-host-maintenance
sudo cp scripts/systemd/coves-host-maintenance.{service,timer} /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now coves-host-maintenance.timer
systemctl list-timers coves-host-maintenance.timer
```

Do not `systemctl start coves-host-maintenance.service` to test it: that reboots the host.

## After a maintenance run

```bash
uname -r                                   # the newest installed linux-image
docker version --format '{{.Server.Version}}'
cat /proc/mdstat                           # md2 and md3 show [UU]
systemctl --failed
docker ps --format 'table {{.Names}}\t{{.Status}}'
```
