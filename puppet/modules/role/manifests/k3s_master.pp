# role::k3s_master — K3s control-plane node (internal-master1, master2, master3).
#
# Manages: base dependencies and all UFW rules required by a K3s server.
# master1/master2 additionally have DRBD configured by the deploy phase; master3
# is control-plane only (etcd quorum).
# The K3s binary installation and cluster bootstrap are done by the
# orchestrator (the `setup-kubernetes` phase): they need an ordered token
# exchange between the servers, which is not declarative state.

class role::k3s_master {

  include role::base

  $internal_cidr = lookup('cms_ha::internal_cidr', String, undef, '192.168.10.0/24')
  $main_cidr = lookup('cms_ha::main_cidr', String, undef, '192.168.20.0/24')
  $monitor_ip = lookup('cms_ha::monitor_ip', String, undef, '192.168.10.20')

  # ---------------------------------------------------------------------------
  # PACKAGES — Prerequisites for K3s
  # ---------------------------------------------------------------------------
  package { ['curl', 'apt-transport-https', 'ca-certificates']:
    ensure => installed,
  }

  # ---------------------------------------------------------------------------
  # MYSQLD EXPORTER — Pre-configure config before installing package
  # ---------------------------------------------------------------------------
  file { '/etc/mysql':
    ensure => directory,
    owner  => 'root',
    group  => 'root',
    mode   => '0755',
  }

  file { '/usr/local/sbin/cms-ha-configure-mysqld-exporter.py':
    ensure => file,
    owner  => 'root',
    group  => 'root',
    mode   => '0755',
    source => 'puppet:///modules/role/cms-ha/configure-mysqld-exporter.py',
  }

  file { '/etc/default/prometheus-mysqld-exporter':
    ensure  => file,
    owner   => 'root',
    group   => 'root',
    mode    => '0644',
    content => "ARGS=\"--config.my-cnf=/etc/mysql/exporter.cnf\"\n",
  }

  package { 'prometheus-mysqld-exporter':
    ensure  => installed,
    require => [
      File['/etc/mysql'],
      File['/etc/default/prometheus-mysqld-exporter'],
    ],
  }

  exec { 'configure-mysqld-exporter-credentials':
    command => '/usr/bin/python3 /usr/local/sbin/cms-ha-configure-mysqld-exporter.py',
    unless  => '/usr/bin/python3 /usr/local/sbin/cms-ha-configure-mysqld-exporter.py check',
    require => [Package['prometheus-mysqld-exporter'], File['/usr/local/sbin/cms-ha-configure-mysqld-exporter.py']],
    notify  => Service['prometheus-mysqld-exporter'],
  }

  # dpkg starts the exporter as soon as the package lands, i.e. before the
  # credentials above exist. The unit then fails, restarts and burns through
  # systemd's start limit, after which every further start is refused with
  # "Start request repeated too quickly" — the state a fresh node was stuck in.
  # Clear it before the service resource touches the unit; the exec stays
  # idempotent because it skips a unit that is already active.
  exec { 'reset-failed-mysqld-exporter':
    command => '/usr/bin/systemctl reset-failed prometheus-mysqld-exporter || true',
    unless  => '/usr/bin/systemctl is-active --quiet prometheus-mysqld-exporter',
    require => Package['prometheus-mysqld-exporter'],
    before  => Service['prometheus-mysqld-exporter'],
  }

  service { 'prometheus-mysqld-exporter':
    ensure    => running,
    enable    => true,
    subscribe => [
      Exec['configure-mysqld-exporter-credentials'],
      File['/etc/default/prometheus-mysqld-exporter'],
    ],
    require   => Package['prometheus-mysqld-exporter'],
  }

  # ---------------------------------------------------------------------------
  # DRBD METRICS — replication health published to node_exporter (textfile)
  # ---------------------------------------------------------------------------
  # Only master1/master2 run the DRBD resource; on master3 (and before the DRBD
  # phase has run) the script finds no resource and publishes nothing, so the
  # DRBD alerts never fire on a node that is not supposed to have DRBD.
  file { '/usr/local/bin/cms-drbd-metrics.sh':
    ensure => file,
    owner  => 'root',
    group  => 'root',
    mode   => '0755',
    source => 'puppet:///modules/role/cms-ha/cms-drbd-metrics.sh',
  }

  # A systemd timer, not cron: the lab images do not reliably run a cron
  # daemon, and a missing publisher only shows up as blank dashboard panels and
  # silent alerts. systemd is the one scheduler guaranteed to be running.
  file { '/etc/systemd/system/cms-drbd-metrics.service':
    ensure => file,
    owner  => 'root',
    group  => 'root',
    mode   => '0644',
    source => 'puppet:///modules/role/cms-ha/cms-drbd-metrics.service',
    notify => Exec['cms-drbd-metrics-daemon-reload'],
  }

  file { '/etc/systemd/system/cms-drbd-metrics.timer':
    ensure => file,
    owner  => 'root',
    group  => 'root',
    mode   => '0644',
    source => 'puppet:///modules/role/cms-ha/cms-drbd-metrics.timer',
    notify => Exec['cms-drbd-metrics-daemon-reload'],
  }

  exec { 'cms-drbd-metrics-daemon-reload':
    command     => '/bin/systemctl daemon-reload',
    refreshonly => true,
    before      => Service['cms-drbd-metrics.timer'],
  }

  service { 'cms-drbd-metrics.timer':
    ensure  => running,
    enable  => true,
    require => [
      File['/usr/local/bin/cms-drbd-metrics.sh'],
      File['/var/lib/prometheus/node-exporter'],
      File['/etc/systemd/system/cms-drbd-metrics.service'],
      File['/etc/systemd/system/cms-drbd-metrics.timer'],
      Exec['cms-drbd-metrics-daemon-reload'],
    ],
  }

  # Earlier revisions published from cron; remove the entry so an upgraded node
  # does not also run the script every minute.
  cron { 'cms-drbd-metrics':
    ensure => absent,
  }

  # ---------------------------------------------------------------------------
  # UFW — K3s control-plane firewall rules
  # ---------------------------------------------------------------------------

  # Hiera currently exposes subnet CIDRs, not master/worker/frontend IP lists
  # or the hypervisor's administration address. Keep the internal subnet for
  # joins and monitoring; orchestration runs kubectl locally over SSH. Exact
  # node/admin allowlists require generated role data, not hostname heuristics.
  exec { 'ufw-k3s-api':
    command => "/usr/sbin/ufw allow from ${internal_cidr} to any port 6443 proto tcp comment \"K3s API from Internal subnet\"",
    unless  => "/usr/sbin/ufw status | /usr/bin/grep -E '^6443/tcp[[:space:]]+ALLOW[[:space:]]' | /usr/bin/grep -Fq ' ${internal_cidr} '",
    require => Exec['ufw-enable'],
  }

  # Remove the previous IPv4/IPv6 public grants only after the replacements
  # exist. A port-only guard would mistake those old grants for scoped rules.
  [6443, 30306].each |$port| {
    exec { "ufw-k3s-remove-public-${port}":
      command => "/usr/sbin/ufw --force delete allow ${port}/tcp",
      onlyif  => "/usr/sbin/ufw status | /usr/bin/grep -Eq '^${port}/tcp[[:space:]].*ALLOW.*Anywhere'",
      require => [Exec['ufw-k3s-api'], Exec['ufw-mariadb-nodeport-main'], Exec['ufw-mariadb-nodeport-internal']],
    }
  }

  # etcd peer communication (HA cluster)
  exec { 'ufw-etcd-peers':
    command => "/usr/sbin/ufw allow from ${internal_cidr} to any port 2379:2380 proto tcp comment \"etcd\"",
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "2379:2380/tcp"',
    require => Exec['ufw-enable'],
  }

  # etcd metrics (k3s supervisor, exposed with --etcd-expose-metrics).
  # Prometheus on internal-monitor scrapes them for the EtcdNoQuorum alert.
  exec { 'ufw-etcd-metrics':
    command => "/usr/sbin/ufw allow from ${monitor_ip} to any port 2381 proto tcp comment \"etcd metrics\"",
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "2381"',
    require => Exec['ufw-enable'],
  }

  # Kubelet API (required for kubectl exec, logs, metrics)
  exec { 'ufw-kubelet':
    command => "/usr/sbin/ufw allow from ${internal_cidr} to any port 10250 proto tcp comment \"Kubelet API\"",
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "10250/tcp"',
    require => Exec['ufw-enable'],
  }

  # Flannel VXLAN overlay network
  exec { 'ufw-flannel-vxlan':
    command => "/usr/sbin/ufw allow from ${internal_cidr} to any port 8472 proto udp comment \"Flannel VXLAN\"",
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "8472/udp"',
    require => Exec['ufw-enable'],
  }

  # Main contains the CMS clients; Internal contains the hypervisor running
  # verify/traffic TCP healthchecks and the monitor. mysqld_exporter uses
  # 127.0.0.1:30306 (UFW's existing loopback allowance is left intact).
  # These are subnet bounds only: kube-proxy DNAT/FORWARD can bypass UFW INPUT.
  # Do not claim NodePort isolation without a pre-DNAT/CNI enforcement layer.
  $database_sources = { 'main' => $main_cidr, 'internal' => $internal_cidr }
  $database_sources.each |$zone, $source| {
    exec { "ufw-mariadb-nodeport-${zone}":
      command => "/usr/sbin/ufw allow from ${source} to any port 30306 proto tcp comment \"MariaDB NodePort from ${zone}\"",
      unless  => "/usr/sbin/ufw status | /usr/bin/grep -E '^30306/tcp[[:space:]]+ALLOW[[:space:]]' | /usr/bin/grep -Fq ' ${source} '",
      require => Exec['ufw-enable'],
    }
  }

  # mysqld_exporter metrics
  exec { 'ufw-mysqld-exporter':
    command => "/usr/sbin/ufw allow from ${monitor_ip} to any port 9104 proto tcp comment \"mysqld_exporter\"",
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "9104"',
    require => Exec['ufw-enable'],
  }

  # DRBD replication port (master1 ↔ master2)
  exec { 'ufw-drbd':
    command => "/usr/sbin/ufw allow from ${internal_cidr} to any port 7788 proto tcp comment \"DRBD replication\"",
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "7788/tcp"',
    require => Exec['ufw-enable'],
  }
}
