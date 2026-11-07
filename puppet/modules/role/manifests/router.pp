# role::router — Perimeter router and firewall (ufw-router node).
#
# Manages: IP forwarding (sysctl), UFW default perimeter policies,
# inter-zone forwarding rules, and SSH access.
# NAT/DNAT rules (iptables) are applied by the orchestrator (the router
# bootstrap and the `ufw` phase), not by Puppet: they need the WAN interface
# name, which is discovered at runtime.

class role::router {

  include role::base

  $internal_cidr = lookup('cms_ha::internal_cidr', String, undef, '192.168.10.0/24')
  $main_cidr = lookup('cms_ha::main_cidr', String, undef, '192.168.20.0/24')
  $vip = lookup('cms_ha::main_vip', String, undef, '192.168.20.100')
  $monitor_ip = lookup('cms_ha::monitor_ip', String, undef, '192.168.10.20')

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
    # ufw >= 0.36 renders this policy as "deny (routed)" on the same line as
    # the other defaults; only the incoming policy has the "Default:" prefix.
    # Anchoring on the prefixed form made the exec re-run on every agent run.
    unless  => '/usr/sbin/ufw status verbose | /usr/bin/grep -q "deny (routed)"',
    require => Package['ufw'],
  }

  # ---------------------------------------------------------------------------
  # UFW — Inter-zone forwarding rules
  # ---------------------------------------------------------------------------

  # Allow Main network → Internet (WAN) — interface-agnostic rule via IP ranges
  exec { 'ufw-route-main-to-wan':
    command => "/usr/sbin/ufw route allow from ${main_cidr} to any comment \"Main -> Internet\"",
    # Route rules render as "To  Action  From" (the To/From columns are the
    # reverse of what a left-to-right regex expects), so anchor on the comment
    # the command itself sets: it is unique per rule and column-order agnostic.
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "Main -> Internet"',
    require => Exec['ufw-enable'],
  }

  # Allow Internal network → Internet (WAN)
  exec { 'ufw-route-internal-to-wan':
    command => "/usr/sbin/ufw route allow from ${internal_cidr} to any comment \"Internal -> Internet\"",
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "Internal -> Internet"',
    require => Exec['ufw-enable'],
  }

  # Allow Internal ↔ Main bidirectional routing
  exec { 'ufw-route-internal-to-main':
    command => "/usr/sbin/ufw route allow from ${internal_cidr} to ${main_cidr} comment \"Internal -> Main\"",
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "Internal -> Main"',
    require => Exec['ufw-enable'],
  }

  exec { 'ufw-route-main-to-internal':
    command => "/usr/sbin/ufw route allow from ${main_cidr} to ${internal_cidr} comment \"Main -> Internal\"",
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "Main -> Internal"',
    require => Exec['ufw-enable'],
  }

  # Allow DNAT forwarding: HTTP/HTTPS from WAN to load balancer
  exec { 'ufw-route-dnat-http':
    command => "/usr/sbin/ufw route allow proto tcp to ${vip} port 80 comment \"DNAT HTTP -> LB\"",
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "DNAT HTTP -> LB"',
    require => Exec['ufw-enable'],
  }

  exec { 'ufw-route-dnat-https':
    command => "/usr/sbin/ufw route allow proto tcp to ${vip} port 443 comment \"DNAT HTTPS -> LB\"",
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "DNAT HTTPS -> LB"',
    require => Exec['ufw-enable'],
  }

  # Allow monitor scrape traffic through the router
  exec { 'ufw-route-monitor-scrape-internal':
    command => "/usr/sbin/ufw route allow proto tcp from ${monitor_ip} to ${internal_cidr} port 9100 comment \"Monitor -> Internal nodes\"",
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "9100.*Monitor.*Internal"',
    require => Exec['ufw-enable'],
  }

  exec { 'ufw-route-monitor-scrape-main':
    command => "/usr/sbin/ufw route allow proto tcp from ${monitor_ip} to ${main_cidr} port 9100 comment \"Monitor -> Main nodes\"",
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "9100.*Monitor.*Main"',
    require => Exec['ufw-enable'],
  }

  exec { 'ufw-route-monitor-scrape-nginx':
    command => "/usr/sbin/ufw route allow proto tcp from ${monitor_ip} to ${main_cidr} port 9113 comment \"Monitor -> Nginx exporter\"",
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "9113.*Monitor.*Nginx"',
    require => Exec['ufw-enable'],
  }

  exec { 'ufw-route-monitor-scrape-apache':
    command => "/usr/sbin/ufw route allow proto tcp from ${monitor_ip} to ${main_cidr} port 9117 comment \"Monitor -> Apache exporter\"",
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "9117.*Monitor.*Apache"',
    require => Exec['ufw-enable'],
  }
}
