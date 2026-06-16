# role::base — Applied to every node in the infrastructure.
#
# Manages: node_exporter, chrony NTP, /etc/hosts, swap, and UFW base policies.
# Every specialised role must `include role::base` as its first statement.

class role::base {

  # ---------------------------------------------------------------------------
  # PACKAGES
  # ---------------------------------------------------------------------------
  package { ['prometheus-node-exporter', 'chrony', 'ufw']:
    ensure => installed,
  }

  # ---------------------------------------------------------------------------
  # CHRONY — NTP client pointing to jumpstart as internal time source
  # ---------------------------------------------------------------------------
  file { '/etc/chrony/chrony.conf':
    ensure  => file,
    owner   => 'root',
    group   => 'root',
    mode    => '0644',
    content => @("CHRONY"),
      # Internal NTP server (jumpstart)
      server 192.168.10.10 iburst prefer
      # Public fallback (only used when jumpstart is unreachable)
      pool ntp.ubuntu.com iburst
      # Force immediate step correction after resume from pause
      makestep 1.0 -1
      driftfile /var/lib/chrony/drift
      rtcsync
      logdir /var/log/chrony
      | CHRONY
    notify  => Service['chrony'],
    require => Package['chrony'],
  }

  service { 'chrony':
    ensure  => running,
    enable  => true,
    require => Package['chrony'],
  }

  # ---------------------------------------------------------------------------
  # NODE EXPORTER
  # ---------------------------------------------------------------------------
  service { 'prometheus-node-exporter':
    ensure  => running,
    enable  => true,
    require => Package['prometheus-node-exporter'],
  }

  # ---------------------------------------------------------------------------
  # /etc/hosts — Cluster-wide name resolution (no DNS dependency)
  # ---------------------------------------------------------------------------
  file { '/etc/hosts':
    ensure  => file,
    owner   => 'root',
    group   => 'root',
    mode    => '0644',
    content => @("HOSTS"),
      127.0.0.1       localhost
      127.0.1.1       ${facts['networking']['fqdn']} ${facts['networking']['hostname']}

      # Jumpstart / Puppet Server
      192.168.10.10   jumpstart.internal.local jumpstart puppet

      # Internal network (192.168.10.0/24)
      192.168.10.20   internal-monitor.internal.local internal-monitor
      192.168.10.11   internal-master1.internal.local internal-master1
      192.168.10.12   internal-master2.internal.local internal-master2
      192.168.10.13   internal-worker1.internal.local internal-worker1
      192.168.10.14   internal-worker2.internal.local internal-worker2
      192.168.10.15   internal-storage.internal.local internal-storage

      # Main network (192.168.20.0/24)
      192.168.20.100  main-lb.main.local main-lb
      192.168.20.101  main-cms1.main.local main-cms1
      192.168.20.102  main-cms2.main.local main-cms2

      # Hot-desk workstations
      192.168.20.201  main-hotdesk1.main.local main-hotdesk1
      192.168.20.202  main-hotdesk2.main.local main-hotdesk2
      192.168.20.203  main-hotdesk3.main.local main-hotdesk3
      192.168.20.204  main-hotdesk4.main.local main-hotdesk4
      192.168.20.205  main-hotdesk5.main.local main-hotdesk5
      192.168.20.206  main-hotdesk6.main.local main-hotdesk6
      192.168.20.207  main-hotdesk7.main.local main-hotdesk7
      192.168.20.208  main-hotdesk8.main.local main-hotdesk8
      | HOSTS
  }

  # ---------------------------------------------------------------------------
  # SWAP — 1 GB swapfile (safety net on memory-constrained hosts)
  # ---------------------------------------------------------------------------
  exec { 'create-swapfile':
    command => '/usr/bin/fallocate -l 1G /swapfile && /usr/bin/chmod 600 /swapfile && /usr/sbin/mkswap /swapfile',
    creates => '/swapfile',
  }

  mount { 'swap':
    ensure  => present,
    name    => 'none',
    device  => '/swapfile',
    fstype  => 'swap',
    options => 'sw',
    dump    => '0',
    pass    => '0',
    require => Exec['create-swapfile'],
  }

  exec { 'enable-swapfile':
    command => '/usr/sbin/swapon /swapfile',
    unless  => '/usr/sbin/swapon -s | /usr/bin/grep -q "/swapfile"',
    require => Mount['swap'],
  }

  # ---------------------------------------------------------------------------
  # UFW — Base firewall policies (applied on every node)
  # ---------------------------------------------------------------------------

  # Default policies: deny all incoming, allow outgoing
  exec { 'ufw-default-deny-incoming':
    command => '/usr/sbin/ufw default deny incoming',
    unless  => '/usr/sbin/ufw status verbose | /usr/bin/grep -q "Default: deny (incoming)"',
    require => Package['ufw'],
  }

  exec { 'ufw-default-allow-outgoing':
    command => '/usr/sbin/ufw default allow outgoing',
    unless  => '/usr/sbin/ufw status verbose | /usr/bin/grep -q "Default: allow (outgoing)"',
    require => Package['ufw'],
  }

  # Always allow SSH (prevents accidental lockout on any node)
  exec { 'ufw-allow-ssh':
    command => '/usr/sbin/ufw allow 22/tcp comment "SSH"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "22/tcp.*ALLOW"',
    require => Package['ufw'],
  }

  # Allow node_exporter scraping from internal-monitor only
  exec { 'ufw-allow-node-exporter':
    command => '/usr/sbin/ufw allow from 192.168.10.20 to any port 9100 proto tcp comment "node_exporter"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "9100/tcp.*192.168.10.20"',
    require => Package['ufw'],
  }

  # Enable UFW (idempotent)
  exec { 'ufw-enable':
    command => '/usr/sbin/ufw --force enable',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "Status: active"',
    require => [
      Exec['ufw-default-deny-incoming'],
      Exec['ufw-default-allow-outgoing'],
      Exec['ufw-allow-ssh'],
      Exec['ufw-allow-node-exporter'],
    ],
  }
}
