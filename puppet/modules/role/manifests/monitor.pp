# role::monitor — Prometheus + Grafana observability stack (internal-monitor).
#
# Manages: Prometheus binary + config file, Grafana APT repo + package +
# provisioning files (datasource and dashboard provider), UFW rules.

class role::monitor {

  include role::base

  # ---------------------------------------------------------------------------
  # PROMETHEUS
  # ---------------------------------------------------------------------------
  package { 'prometheus':
    ensure => installed,
  }

  # Deploy prometheus.yml from Puppet module fileserver.
  # This file is the source of truth for all scrape targets.
  file { '/etc/prometheus/prometheus.yml':
    ensure  => file,
    owner   => 'prometheus',
    group   => 'prometheus',
    mode    => '0644',
    source  => 'puppet:///modules/role/prometheus/prometheus.yml',
    require => Package['prometheus'],
    notify  => Service['prometheus'],
  }

  service { 'prometheus':
    ensure  => running,
    enable  => true,
    require => Package['prometheus'],
  }

  # ---------------------------------------------------------------------------
  # GRAFANA — APT repository setup + package
  # ---------------------------------------------------------------------------
  package { 'curl':
    ensure => installed,
  }

  package { 'gnupg':
    ensure => installed,
  }

  exec { 'grafana-add-gpg-key':
    command => '/usr/bin/curl -fsSL https://apt.grafana.com/gpg.key | /usr/bin/gpg --dearmor -o /usr/share/keyrings/grafana.gpg',
    creates => '/usr/share/keyrings/grafana.gpg',
    require => [Package['curl'], Package['gnupg']],
  }

  file { '/etc/apt/sources.list.d/grafana.list':
    ensure  => file,
    owner   => 'root',
    group   => 'root',
    mode    => '0644',
    content => "deb [signed-by=/usr/share/keyrings/grafana.gpg] https://apt.grafana.com stable main\n",
    require => Exec['grafana-add-gpg-key'],
    notify  => Exec['grafana-apt-update'],
  }

  exec { 'grafana-apt-update':
    command     => '/usr/bin/apt-get update',
    refreshonly => true,
  }

  package { 'grafana':
    ensure  => installed,
    require => Exec['grafana-apt-update'],
  }

  service { 'grafana-server':
    ensure  => running,
    enable  => true,
    require => Package['grafana'],
  }

  # ---------------------------------------------------------------------------
  # GRAFANA PROVISIONING — Datasource (Prometheus) and Dashboard provider
  # ---------------------------------------------------------------------------
  file { '/etc/grafana/provisioning/datasources':
    ensure  => directory,
    owner   => 'root',
    group   => 'grafana',
    mode    => '0755',
    require => Package['grafana'],
  }

  file { '/etc/grafana/provisioning/datasources/prometheus.yaml':
    ensure  => file,
    owner   => 'root',
    group   => 'grafana',
    mode    => '0640',
    source  => 'puppet:///modules/role/grafana/datasource.yaml',
    require => File['/etc/grafana/provisioning/datasources'],
    notify  => Service['grafana-server'],
  }

  file { '/etc/grafana/provisioning/dashboards':
    ensure  => directory,
    owner   => 'root',
    group   => 'grafana',
    mode    => '0755',
    require => Package['grafana'],
  }

  file { '/etc/grafana/provisioning/dashboards/provider.yaml':
    ensure  => file,
    owner   => 'root',
    group   => 'grafana',
    mode    => '0640',
    source  => 'puppet:///modules/role/grafana/dashboard-provider.yaml',
    require => File['/etc/grafana/provisioning/dashboards'],
    notify  => Service['grafana-server'],
  }

  # ---------------------------------------------------------------------------
  # GRAFANA DASHBOARDS — JSON files loaded by the provider from disk
  # The provider is configured to read from /var/lib/grafana/dashboards.
  # Every JSON file placed here is auto-loaded at startup or within 30s.
  # ---------------------------------------------------------------------------
  file { '/var/lib/grafana/dashboards':
    ensure  => directory,
    owner   => 'grafana',
    group   => 'grafana',
    mode    => '0755',
    require => Package['grafana'],
  }

  file { '/var/lib/grafana/dashboards/node-exporter-overview.json':
    ensure  => file,
    owner   => 'grafana',
    group   => 'grafana',
    mode    => '0644',
    source  => 'puppet:///modules/role/grafana/node-exporter-overview.json',
    require => File['/var/lib/grafana/dashboards'],
    notify  => Service['grafana-server'],
  }

  # ---------------------------------------------------------------------------
  # UFW — Firewall rules for the monitor node
  # ---------------------------------------------------------------------------
  exec { 'ufw-prometheus':
    command => '/usr/sbin/ufw allow from 192.168.10.0/24 to any port 9090 proto tcp comment "Prometheus"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "9090"',
    require => Exec['ufw-enable'],
  }

  exec { 'ufw-grafana':
    command => '/usr/sbin/ufw allow from 192.168.10.0/24 to any port 3000 proto tcp comment "Grafana"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "3000"',
    require => Exec['ufw-enable'],
  }
}
