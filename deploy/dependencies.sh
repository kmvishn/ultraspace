# Packages and host setup that ultraspace features need.
# Sourced by install.sh and update.sh. The caller defines apt_install.
#
# Add a package here when a feature needs it. Both a fresh install and
# update.sh will install it. Config and data are not touched.

install_dependencies() {
  echo "Installing system dependencies (audio, bluetooth, media, hardware, network)..."
  deps="ffmpeg mpv pulseaudio pulseaudio-utils pulseaudio-module-bluetooth bluez network-manager pciutils usbutils curl ca-certificates espeak-ng"
  apt_install $deps || warn "some packages could not be installed; you may need to install them manually"

  # Polkit rule for unprivileged NetworkManager and System Power/Reboot control
  if [ -d /etc/polkit-1/rules.d ]; then
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
