# role::base — Applied to every node in the infrastructure.
#
# Manages: node_exporter, chrony NTP, /etc/hosts, swap, UFW base policies, the
# deploy account's sudoers drop-in and the sshd root-login lockdown.
# Every specialised role must `include role::base` as its first statement.

class role::base {

  # Topology values come from Hiera. `cms-ha phase setup-puppet` regenerates
  # data/common.yaml from config.yaml, so a renamed node, a custom domain, a
  # different subnet or a non-default deploy account never leaves a manifest
  # pointing at the default lab. The defaults keep a manual Puppet run working
  # against the committed data file.
  $deploy_user = lookup('cms_ha::deploy_user', String, undef, 'ubuntu')
  $jumpstart_ip = lookup('cms_ha::jumpstart_ip', String, undef, '192.168.10.10')
  $monitor_ip = lookup('cms_ha::monitor_ip', String, undef, '192.168.10.20')
  $hosts_content = lookup('cms_ha::hosts_content', String, undef, '')

  if $hosts_content == '' {
    fail('cms_ha::hosts_content is empty: generate puppet/data/common.yaml with `cms-ha phase setup-puppet`')
  }

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
      server ${jumpstart_ip} iburst prefer
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
      Defaults:${deploy_user} !requiretty
      Defaults log_output
      ${deploy_user} ALL=(ALL) NOPASSWD:ALL
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

      # Topology entries generated from config.yaml by the Puppet phase.
      ${hosts_content}
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
    # ufw renders all three default policies on one line, so only the incoming
    # policy carries the "Default:" prefix. Matching the prefixed form made
    # this exec re-run on every Puppet agent run.
    unless  => '/usr/sbin/ufw status verbose | /usr/bin/grep -q "allow (outgoing)"',
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
    command => "/usr/sbin/ufw allow from ${monitor_ip} to any port 9100 proto tcp comment \"node_exporter\"",
    unless  => "/usr/sbin/ufw status | /usr/bin/grep -q \"9100/tcp.*${monitor_ip}\"",
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
