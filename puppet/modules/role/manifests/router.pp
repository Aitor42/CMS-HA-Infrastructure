# role::router — Perimeter router and firewall (ufw-router node).
#
# Manages: IP forwarding (sysctl), UFW default perimeter policies,
# inter-zone forwarding rules, and SSH access.
# NAT/DNAT rules (iptables) remain in 05_setup_ufw.sh because they require
# knowledge of the actual WAN interface name, which is detected at runtime.

class role::router {

  include role::base

  package { 'netplan.io':
    ensure => installed,
  }

  # ---------------------------------------------------------------------------
  # IP FORWARDING — Enable kernel packet forwarding (L3 routing)
  # ---------------------------------------------------------------------------
  file { '/etc/sysctl.d/99-ip-forward.conf':
    ensure  => file,
    owner   => 'root',
    group   => 'root',
    mode    => '0644',
    content => "net.ipv4.ip_forward=1\n",
    notify  => Exec['apply-sysctl'],
  }

  exec { 'apply-sysctl':
    command     => '/usr/sbin/sysctl --system',
    refreshonly => true,
  }

  # ---------------------------------------------------------------------------
  # UFW — Perimeter firewall default policies
  # ---------------------------------------------------------------------------

  # Override the base 'deny incoming' with 'deny forward' as well
  exec { 'ufw-default-deny-forward':
    command => '/usr/sbin/ufw default deny forward',
    unless  => '/usr/sbin/ufw status verbose | /usr/bin/grep -q "Default: deny (forward)"',
    require => Package['ufw'],
  }

  # ---------------------------------------------------------------------------
  # UFW — Inter-zone forwarding rules
  # ---------------------------------------------------------------------------

  # Allow Main network → Internet (WAN) — interface-agnostic rule via IP ranges
  exec { 'ufw-route-main-to-wan':
    command => '/usr/sbin/ufw route allow from 192.168.20.0/24 to any comment "Main -> Internet"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "192.168.20.0/24.*FWD"',
    require => Exec['ufw-enable'],
  }

  # Allow Internal ↔ Main bidirectional routing
  exec { 'ufw-route-internal-to-main':
    command => '/usr/sbin/ufw route allow from 192.168.10.0/24 to 192.168.20.0/24 comment "Internal -> Main"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "192.168.10.0/24.*192.168.20.0/24.*FWD"',
    require => Exec['ufw-enable'],
  }

  exec { 'ufw-route-main-to-internal':
    command => '/usr/sbin/ufw route allow from 192.168.20.0/24 to 192.168.10.0/24 comment "Main -> Internal"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "192.168.20.0/24.*192.168.10.0/24.*FWD"',
    require => Exec['ufw-enable'],
  }

  # Allow DNAT forwarding: HTTP/HTTPS from WAN to load balancer
  exec { 'ufw-route-dnat-http':
    command => '/usr/sbin/ufw route allow proto tcp to 192.168.20.100 port 80 comment "DNAT HTTP -> LB"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "192.168.20.100.*80/tcp.*FWD"',
    require => Exec['ufw-enable'],
  }

  exec { 'ufw-route-dnat-https':
    command => '/usr/sbin/ufw route allow proto tcp to 192.168.20.100 port 443 comment "DNAT HTTPS -> LB"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "192.168.20.100.*443/tcp.*FWD"',
    require => Exec['ufw-enable'],
  }

  # Allow monitor scrape traffic through the router
  exec { 'ufw-route-monitor-scrape-internal':
    command => '/usr/sbin/ufw route allow proto tcp from 192.168.10.20 to 192.168.10.0/24 port 9100 comment "Monitor -> Internal nodes"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "9100.*Monitor.*Internal"',
    require => Exec['ufw-enable'],
  }

  exec { 'ufw-route-monitor-scrape-main':
    command => '/usr/sbin/ufw route allow proto tcp from 192.168.10.20 to 192.168.20.0/24 port 9100 comment "Monitor -> Main nodes"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "9100.*Monitor.*Main"',
    require => Exec['ufw-enable'],
  }

  exec { 'ufw-route-monitor-scrape-nginx':
    command => '/usr/sbin/ufw route allow proto tcp from 192.168.10.20 to 192.168.20.100 port 9113 comment "Monitor -> Nginx exporter"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "9113.*Monitor.*Nginx"',
    require => Exec['ufw-enable'],
  }

  exec { 'ufw-route-monitor-scrape-apache':
    command => '/usr/sbin/ufw route allow proto tcp from 192.168.10.20 to 192.168.20.0/24 port 9117 comment "Monitor -> Apache exporter"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "9117.*Monitor.*Apache"',
    require => Exec['ufw-enable'],
  }
}
