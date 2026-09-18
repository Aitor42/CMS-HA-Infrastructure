# role::k3s_master — K3s control-plane node (internal-master1, internal-master2).
#
# Manages: base dependencies and all UFW rules required by a K3s master.
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

  file { '/etc/mysql/exporter.cnf':
    ensure  => file,
    owner   => 'root',
    group   => 'root',
    mode    => '0644',
    content => "[client]\nuser=exporter\npassword=exporter_password\nhost=127.0.0.1\nport=30306\n",
    require => File['/etc/mysql'],
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
      File['/etc/mysql/exporter.cnf'],
      File['/etc/default/prometheus-mysqld-exporter'],
    ],
  }

  service { 'prometheus-mysqld-exporter':
    ensure    => running,
    enable    => true,
    subscribe => [
      File['/etc/mysql/exporter.cnf'],
      File['/etc/default/prometheus-mysqld-exporter'],
    ],
    require   => Package['prometheus-mysqld-exporter'],
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
