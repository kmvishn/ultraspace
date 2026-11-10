#!/usr/bin/env bash
# headless-laptop-setup.sh
# Optimizes a laptop with a broken/missing screen to operate as a 24/7 headless home server.
#
# Usage:
#   sudo ./headless-laptop-setup.sh

set -euo pipefail

echo "=========================================================="
echo "  ultraspace: Headless Laptop Home Server Setup Helper    "
echo "=========================================================="

if [ "$EUID" -ne 0 ]; then
    echo "Error: Please run as root (sudo ./headless-laptop-setup.sh)"
    exit 1
fi

echo ""
echo "[1/4] Disabling sleep when laptop lid is closed..."
LOGIND_CONF="/etc/systemd/logind.conf.d/ultraspace-headless.conf"
mkdir -p /etc/systemd/logind.conf.d
cat << 'EOF' > "$LOGIND_CONF"
[Login]
HandleLidSwitch=ignore
HandleLidSwitchExternalPower=ignore
HandleLidSwitchDocked=ignore
LidSwitchIgnoreInhibited=no
EOF
systemctl restart systemd-logind || true
echo "  -> Lid switch sleep disabled."

echo ""
echo "[2/4] Disabling broken display panel backlight to save power & heat..."
BACKLIGHT_SERVICE="/etc/systemd/system/ultraspace-screen-off.service"
cat << 'EOF' > "$BACKLIGHT_SERVICE"
[Unit]
Description=Turn off display backlight on headless laptop
After=multi-user.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh -c 'for b in /sys/class/backlight/*; do [ -e "$b/brightness" ] && echo 0 > "$b/brightness" || true; done'

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now ultraspace-screen-off.service || true
echo "  -> Backlight disabled."

echo ""
echo "[3/4] Configuring Polkit for unprivileged NetworkManager control..."
POLKIT_RULE="/etc/polkit-1/rules.d/50-ultraspace-network.rules"
mkdir -p /etc/polkit-1/rules.d
cat << 'EOF' > "$POLKIT_RULE"
// Allow ultraspace service to manage network and wifi without prompting for root
polkit.addRule(function(action, subject) {
    if (action.id.indexOf("org.freedesktop.NetworkManager.") == 0 &&
        (subject.user == "ultraspace" || subject.isInGroup("sudo") || subject.isInGroup("wheel"))) {
        return polkit.Result.YES;
    }
});
EOF
echo "  -> Polkit rule configured."

echo ""
echo "[4/6] Configuring Bluetooth & Audio groups for Ultraspace..."
if id ultraspace >/dev/null 2>&1; then
    usermod -aG bluetooth,audio,netdev,pulse-access ultraspace || true
    echo "  -> Added ultraspace to bluetooth, audio, netdev, pulse-access groups."
fi

echo ""
echo "[5/6] Checking battery charge threshold..."
FOUND_THRESHOLD=0
for f in /sys/class/power_supply/BAT*/charge_control_end_threshold; do
    if [ -f "$f" ]; then
        echo "  -> Found battery charge threshold control: $f"
        echo 80 > "$f" || true
        echo "  -> Battery charge limit set to 80% to protect against swelling."
        FOUND_THRESHOLD=1
        break
    fi
done
if [ "$FOUND_THRESHOLD" -eq 0 ]; then
    echo "  -> Note: Battery threshold not natively exposed by this vendor's ACPI driver (safe to ignore)."
fi

echo ""
echo "[5/5] Ensuring Ultraspace config has full filesystem access..."
mkdir -p /etc/ultraspace
if [ -f /etc/ultraspace/config.toml ]; then
    if ! grep -q "files_root" /etc/ultraspace/config.toml; then
        echo 'files_root = "/"' >> /etc/ultraspace/config.toml
    fi
else
    cat << 'EOF' > /etc/ultraspace/config.toml
# Ultraspace Headless Server Configuration
files_root = "/"
EOF
fi
echo "  -> Full filesystem access enabled (files_root = '/')."

echo ""
echo "=========================================================="
echo "  Headless setup complete! Your laptop is ready to run.   "
echo "=========================================================="

