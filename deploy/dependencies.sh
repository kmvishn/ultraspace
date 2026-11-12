# Packages and host setup that ultraspace features need.
# Sourced by install.sh and update.sh. The caller defines apt_install.
#
# Add a package here when a feature needs it. Both a fresh install and
# update.sh will install it. Config and data are not touched.

# host_kind: "home" for a physical box in the house, "vps" for a cloud VM.
# ULTRASPACE_HOST=home|vps overrides the guess (a home VM looks like a VPS,
# and rented bare metal looks like a home machine).
host_kind() {
  case "${ULTRASPACE_HOST:-}" in
    vps|cloud) echo vps; return ;;
    home|physical|lan) echo home; return ;;
  esac
  if command -v systemd-detect-virt >/dev/null 2>&1; then
    # It prints "none" on bare metal but still exits non-zero there, so do
    # not treat a non-zero exit as a VM.
    virt=$(systemd-detect-virt 2>/dev/null || true)
    case "$virt" in ""|none) echo home ;; *) echo vps ;; esac
    return
  fi
  echo home
}

# Groups the systemd unit names. They must exist or the service will not
# start, so a VPS gets the empty groups without the daemons behind them.
ensure_group() {
  getent group "$1" >/dev/null 2>&1 || groupadd --system "$1" 2>/dev/null || true
}

# Supplementary groups for the ultraspace user on this kind of host.
host_groups() {
  if [ "$(host_kind)" = vps ]; then
    echo "systemd-journal,adm"
  else
    echo "bluetooth,audio,video,netdev,pulse-access,systemd-journal,adm"
  fi
}

install_dependencies() {
  kind=$(host_kind)
  for g in systemd-journal pulse-access audio bluetooth netdev; do ensure_group "$g"; done
  if [ "$kind" = vps ]; then
    echo "VPS host: installing only what a cloud server needs (no audio, Bluetooth, Wi-Fi or hotspot)..."
    deps="ffmpeg curl ca-certificates"
  else
    echo "Home server: installing system dependencies (audio, bluetooth, media, hardware, network)..."
    deps="ffmpeg mpv pulseaudio pulseaudio-utils pulseaudio-module-bluetooth bluez network-manager pciutils usbutils curl ca-certificates espeak-ng"
  fi
  apt_install $deps || warn "some packages could not be installed; you may need to install them manually"

  # Polkit rule. A VPS gets reboot and service restart only: no power-off
  # (that would lock the owner out) and no NetworkManager (there is none).
  if [ -d /etc/polkit-1/rules.d ]; then
    if [ "$kind" = vps ]; then
      cat << 'EOF' > /etc/polkit-1/rules.d/50-ultraspace-network.rules
// Allow ultraspace to reboot and restart its own service without prompting for root.
polkit.addRule(function(action, subject) {
    if ((action.id.indexOf("org.freedesktop.login1.reboot") == 0 ||
         action.id == "org.freedesktop.systemd1.manage-units") &&
        (subject.user == "ultraspace" || subject.isInGroup("sudo") || subject.isInGroup("wheel"))) {
        return polkit.Result.YES;
    }
});
EOF
    else
      cat << 'EOF' > /etc/polkit-1/rules.d/50-ultraspace-network.rules
// Allow ultraspace service to manage network, power, reboot, and service restart without prompting for root
polkit.addRule(function(action, subject) {
    if ((action.id.indexOf("org.freedesktop.NetworkManager.") == 0 ||
         action.id.indexOf("org.freedesktop.login1.power-off") == 0 ||
         action.id.indexOf("org.freedesktop.login1.reboot") == 0 ||
         action.id == "org.freedesktop.systemd1.manage-units") &&
        (subject.user == "ultraspace" || subject.isInGroup("sudo") || subject.isInGroup("wheel"))) {
        return polkit.Result.YES;
    }
});
EOF
    fi
  fi

  # Everything below is home hardware: server speakers and the setup hotspot.
  if [ "$kind" = vps ]; then
    return 0
  fi

  # D-Bus permissions for system PulseAudio + BlueZ Bluetooth audio endpoints
  if [ -d /etc/dbus-1/system.d ]; then
    cat << 'EOF' > /etc/dbus-1/system.d/pulseaudio-system.conf
<!DOCTYPE busconfig PUBLIC
 "-//freedesktop//DTD D-BUS Bus Configuration 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <policy user="pulse">
    <allow own="org.pulseaudio.Server"/>
    <allow send_destination="org.bluez"/>
    <allow send_interface="org.bluez.Manager"/>
    <allow send_interface="org.bluez.MediaEndpoint1"/>
    <allow send_interface="org.bluez.Profile1"/>
  </policy>
  <policy context="default">
    <allow send_destination="org.pulseaudio.Server"/>
    <allow receive_sender="org.pulseaudio.Server"/>
  </policy>
</busconfig>
EOF
  fi

  # Enable Bluetooth audio modules and configure stream routing in /etc/pulse/system.pa
  if [ -f /etc/pulse/system.pa ]; then
    if ! grep -q "module-bluetooth-discover" /etc/pulse/system.pa; then
      printf '\n### Ultraspace Bluetooth & Audio Modules\nload-module module-bluetooth-policy\nload-module module-bluetooth-discover\n' >> /etc/pulse/system.pa
    fi
    # Ensure module-stream-restore does not forcefully pin streams to old sinks
    sed -i 's/load-module module-stream-restore$/load-module module-stream-restore restore_device=no/' /etc/pulse/system.pa
  fi

  # System-wide PulseAudio service for headless server operation
  if [ ! -f /etc/systemd/system/pulseaudio.service ]; then
    cat << 'EOF' > /etc/systemd/system/pulseaudio.service
[Unit]
Description=PulseAudio Sound Server (System-wide)
After=sound.target bluetooth.service
Wants=bluetooth.service

[Service]
Type=notify
ExecStart=/usr/bin/pulseaudio --daemonize=no --system --disallow-exit --disallow-module-loading=no --log-target=journal
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable --now pulseaudio.service 2>/dev/null || true
  fi

  # Add pulse user to bluetooth and audio groups
  if id pulse >/dev/null 2>&1; then
    usermod -aG bluetooth,audio pulse 2>/dev/null || true
  fi

  # DNS for the setup hotspot only. NetworkManager reads this when it shares
  # a connection, so phones that join Ultraspace-Setup get the setup page.
  mkdir -p /etc/NetworkManager/dnsmasq-shared.d
  cat << 'EOF' > /etc/NetworkManager/dnsmasq-shared.d/ultraspace-setup.conf
# Captive portal for the Ultraspace-Setup hotspot. Not used by the host's own DNS.
address=/#/10.42.0.1
dhcp-option=114,http://10.42.0.1/
EOF
}
