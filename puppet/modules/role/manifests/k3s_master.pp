# role::k3s_master — K3s control-plane node (internal-master1, master2, master3).
#
# Manages: base dependencies and all UFW rules required by a K3s server.
# master1/master2 additionally have DRBD configured by the deploy phase; master3
# is control-plane only (etcd quorum).
# The K3s binary installation and cluster bootstrap remain in 06_setup_kubernetes.sh
# because they require ordered token exchange between masters (not declarative state).

class role::k3s_master {

  include role::base

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

  cron { 'cms-drbd-metrics':
    ensure  => present,
    command => '/usr/local/bin/cms-drbd-metrics.sh >/dev/null 2>&1',
    user    => 'root',
    minute  => '*',
    require => [
      File['/usr/local/bin/cms-drbd-metrics.sh'],
      File['/var/lib/prometheus/node-exporter'],
    ],
  }

  # ---------------------------------------------------------------------------
  # UFW — K3s control-plane firewall rules
  # ---------------------------------------------------------------------------

  # Kubernetes API server
  exec { 'ufw-k3s-api':
    command => '/usr/sbin/ufw allow 6443/tcp comment "K3s API server"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "6443/tcp"',
    require => Exec['ufw-enable'],
  }

  # etcd peer communication (HA cluster)
  exec { 'ufw-etcd-peers':
    command => '/usr/sbin/ufw allow from 192.168.10.0/24 to any port 2379:2380 proto tcp comment "etcd"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "2379:2380/tcp"',
    require => Exec['ufw-enable'],
  }

  # etcd metrics (k3s supervisor, exposed with --etcd-expose-metrics).
  # Prometheus on internal-monitor scrapes them for the EtcdNoQuorum alert.
  exec { 'ufw-etcd-metrics':
    command => '/usr/sbin/ufw allow from 192.168.10.20 to any port 2381 proto tcp comment "etcd metrics"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "2381"',
    require => Exec['ufw-enable'],
  }

  # Kubelet API (required for kubectl exec, logs, metrics)
  exec { 'ufw-kubelet':
    command => '/usr/sbin/ufw allow from 192.168.10.0/24 to any port 10250 proto tcp comment "Kubelet API"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "10250/tcp"',
    require => Exec['ufw-enable'],
  }

  # Flannel VXLAN overlay network
  exec { 'ufw-flannel-vxlan':
    command => '/usr/sbin/ufw allow from 192.168.10.0/24 to any port 8472 proto udp comment "Flannel VXLAN"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "8472/udp"',
    require => Exec['ufw-enable'],
  }

  # MariaDB NodePort (exposed by K3s service)
  exec { 'ufw-mariadb-nodeport':
    command => '/usr/sbin/ufw allow 30306/tcp comment "MariaDB NodePort"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "30306/tcp"',
    require => Exec['ufw-enable'],
  }

  # mysqld_exporter metrics
  exec { 'ufw-mysqld-exporter':
    command => '/usr/sbin/ufw allow from 192.168.10.20 to any port 9104 proto tcp comment "mysqld_exporter"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "9104"',
    require => Exec['ufw-enable'],
  }

  # DRBD replication port (master1 ↔ master2)
  exec { 'ufw-drbd':
    command => '/usr/sbin/ufw allow from 192.168.10.0/24 to any port 7788 proto tcp comment "DRBD replication"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "7788/tcp"',
    require => Exec['ufw-enable'],
  }
}
