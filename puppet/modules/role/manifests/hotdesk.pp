# role::hotdesk — Shared hot-desk workstation (main-hotdesk1..N).
#
# Manages: base config (node_exporter, chrony, /etc/hosts, UFW) via role::base,
# plus a minimal desktop environment (XFCE + LightDM) for end-user access.

class role::hotdesk {

  include role::base

  # ---------------------------------------------------------------------------
  # PACKAGES — Minimal graphical desktop environment
  # ---------------------------------------------------------------------------
  package { [
    'xorg',
    'xfce4',
    'xfce4-terminal',
    'lightdm',
    'firefox',
    'libreoffice',
    'file-roller',
    'evince',
    'network-manager',
  ]:
    ensure => installed,
  }

  # ---------------------------------------------------------------------------
  # SERVICES
  # ---------------------------------------------------------------------------
  service { 'lightdm':
    ensure  => running,
    enable  => true,
    require => Package['lightdm'],
  }

  service { 'NetworkManager':
    ensure  => running,
    enable  => true,
    require => Package['network-manager'],
  }
}
