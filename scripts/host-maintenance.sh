#!/usr/bin/env bash
# Monthly production host maintenance: install every pending package upgrade,
# including Docker's apt repository (unattended-upgrades covers Debian origins
# only), then reboot so a new kernel, dockerd, containerd and runc are the ones
# running. Installed as /usr/local/sbin/coves-host-maintenance; see
# docs/PRODUCTION_HOST_MAINTENANCE.md. A failed upgrade exits non-zero and
# skips the reboot.
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive
apt_options=(-q -y
  -o DPkg::Lock::Timeout=600
  -o Dpkg::Options::=--force-confdef
  -o Dpkg::Options::=--force-confold)

apt-get "${apt_options[@]}" update
# --with-new-pkgs installs new dependencies but never removes a package.
apt-get "${apt_options[@]}" --with-new-pkgs upgrade

echo "coves-host-maintenance: upgrade finished, rebooting"
systemctl reboot
