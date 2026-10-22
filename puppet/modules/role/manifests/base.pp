# role::base — Applied to every node in the infrastructure.
#
# Manages: node_exporter, chrony NTP, /etc/hosts, swap, UFW base policies, the
# deploy account's sudoers drop-in and the sshd root-login lockdown.
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
  # Enable the textfile collector so in-cluster CronJobs can publish their own
  # metrics (e.g. cms_backup_last_success_timestamp_seconds) without adding an
  # extra exporter.
  file { '/var/lib/prometheus/node-exporter':
    ensure  => directory,
    owner   => 'prometheus',
    group   => 'prometheus',
    mode    => '0755',
    require => Package['prometheus-node-exporter'],
  }

  file { '/etc/default/prometheus-node-exporter':
    ensure  => file,
    owner   => 'root',
    group   => 'root',
    mode    => '0644',
    content => "ARGS=\"--collector.textfile.directory=/var/lib/prometheus/node-exporter\"\n",
    notify  => Service['prometheus-node-exporter'],
    require => Package['prometheus-node-exporter'],
  }

  service { 'prometheus-node-exporter':
    ensure  => running,
    enable  => true,
    require => Package['prometheus-node-exporter'],
  }

  # ---------------------------------------------------------------------------
  # DEPLOY ACCOUNT — non-root orchestration user with audited sudo
  # ---------------------------------------------------------------------------
  # The orchestrator logs in as this non-root account (`ssh.user`) and elevates
  # privileged commands with `sudo -n`. Command scope is intentionally broad
  # (the orchestrator runs arbitrary privileged operations), so scope is
  # enforced by the dedicated, key-only account plus disabled root logins below,
  # and every sudo invocation is logged.
  file { '/etc/sudoers.d/90-cms-ha':
    ensure       => file,
    owner        => 'root',
    group        => 'root',
    mode         => '0440',
    validate_cmd => '/usr/sbin/visudo -cf %',
    content      => @("SUDOERS"),
      # Managed by CMS-HA — non-root deploy account for the orchestrator.
      Defaults:ubuntu !requiretty
      Defaults log_output
      ubuntu ALL=(ALL) NOPASSWD:ALL
      | SUDOERS
  }

  # ---------------------------------------------------------------------------
  # SSHD — no direct root logins (use the deploy account + sudo instead)
  # ---------------------------------------------------------------------------
  package { 'openssh-server':
    ensure => installed,
  }

  file { '/etc/ssh/sshd_config.d/10-cms-ha-root-login.conf':
    ensure  => file,
    owner   => 'root',
    group   => 'root',
    mode    => '0644',
    content => "PermitRootLogin no\n",
    notify  => Service['sshd'],
    require => Package['openssh-server'],
  }

  service { 'sshd':
    ensure  => running,
    enable  => true,
    require => Package['openssh-server'],
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

      # Perimeter router and firewall
      192.168.10.1    ufw-router.internal.local ufw-router

      # Jumpstart / Puppet Server / Internal CA (step-ca)
      192.168.10.10   jumpstart.internal.local jumpstart puppet ca.internal.local ca

      # Internal network (192.168.10.0/24)
      192.168.10.20   internal-monitor.internal.local internal-monitor
      192.168.10.11   internal-master1.internal.local internal-master1
      192.168.10.12   internal-master2.internal.local internal-master2
      192.168.10.16   internal-master3.internal.local internal-master3
      192.168.10.13   internal-worker1.internal.local internal-worker1
      192.168.10.14   internal-worker2.internal.local internal-worker2
      192.168.10.15   internal-storage.internal.local internal-storage

      # Main network (192.168.20.0/24)
      192.168.20.100  main-lb.main.local main-lb   # keepalived VIP
      192.168.20.103  main-lb1.main.local main-lb1
      192.168.20.104  main-lb2.main.local main-lb2
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
  # SWAP — 512 MB swapfile (safety net on memory-constrained hosts)
  # ---------------------------------------------------------------------------
  exec { 'create-swapfile':
    command => '/bin/rm -f /swapfile && /usr/bin/fallocate -l 512M /swapfile && /usr/bin/chmod 600 /swapfile && /usr/sbin/mkswap /swapfile',
    unless  => '/usr/sbin/swaplabel /swapfile 2>/dev/null',
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
