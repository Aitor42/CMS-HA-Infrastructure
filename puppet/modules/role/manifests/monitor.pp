# role::monitor — Prometheus + Grafana observability stack (internal-monitor).
#
# Manages: Prometheus binary + config file, Prometheus Alertmanager
# (prometheus-alertmanager package + config), the blackbox exporter, the Grafana
# APT repo + package + provisioning files (datasource, dashboard provider and
# home-dashboard drop-in), UFW rules.

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

  # k6 streams its metrics to Prometheus' remote-write endpoint
  # (traffic.prometheus_rw_url) so the k6 panels of the HA overview dashboard
  # have data. The package does not enable the receiver, so without this flag
  # every `cms-ha traffic` run logs "Failed to send the time series data" and
  # the panels stay empty. The flag is appended to the packaged ARGS - Debian
  # compiles the config and storage paths into the binary, so an empty ARGS is
  # complete - and Prometheus is restarted to pick it up.
  exec { 'prometheus-enable-remote-write':
    command => '/bin/sed -i "s|^ARGS=\"|ARGS=\"--web.enable-remote-write-receiver |" /etc/default/prometheus && /usr/bin/systemctl restart prometheus',
    unless  => '/usr/bin/grep -q -- "--web.enable-remote-write-receiver" /etc/default/prometheus',
    require => Service['prometheus'],
  }

  # ---------------------------------------------------------------------------
  # PROMETHEUS ALERTMANAGER — alert routing/deduplication (same node)
  # Prometheus forwards firing alerts here (see the `alerting:` block in
  # prometheus.yml). Listens on tcp/9093; config at /etc/prometheus/alertmanager.yml.
  # ---------------------------------------------------------------------------
  package { 'prometheus-alertmanager':
    ensure => installed,
  }

  file { '/etc/prometheus/alertmanager.yml':
    ensure  => file,
    owner   => 'prometheus',
    group   => 'prometheus',
    mode    => '0644',
    source  => 'puppet:///modules/role/prometheus/alertmanager.yml',
    require => Package['prometheus-alertmanager'],
    notify  => Service['prometheus-alertmanager'],
  }

  service { 'prometheus-alertmanager':
    ensure  => running,
    enable  => true,
    require => Package['prometheus-alertmanager'],
  }

  # ---------------------------------------------------------------------------
  # BLACKBOX EXPORTER — HTTP/TCP reachability probes for Prometheus
  # ---------------------------------------------------------------------------
  package { 'prometheus-blackbox-exporter':
    ensure => installed,
  }

  # The Ubuntu package ships its configuration at /etc/prometheus/blackbox.yml
  # and its unit reads $ARGS from this defaults file. It does *not* create
  # /etc/blackbox_exporter, so writing the probe modules there (the previous
  # behaviour) failed the run on the missing parent directory and left the
  # exporter unable to start. Point the unit at the packaged path instead; the
  # module's own probe definitions matter because they skip TLS verification
  # for the lab's internally-issued certificate.
  file { '/etc/default/prometheus-blackbox-exporter':
    ensure  => file,
    owner   => 'root',
    group   => 'root',
    mode    => '0644',
    content => "ARGS=\"--config.file=/etc/prometheus/blackbox.yml\"\n",
    require => Package['prometheus-blackbox-exporter'],
    notify  => Service['prometheus-blackbox-exporter'],
  }

  file { '/etc/prometheus/blackbox.yml':
    ensure  => file,
    owner   => 'root',
    group   => 'root',
    mode    => '0644',
    source  => 'puppet:///modules/role/prometheus/blackbox.yml',
    require => Package['prometheus-blackbox-exporter'],
    notify  => Service['prometheus-blackbox-exporter'],
  }

  # The package install starts the exporter before those files exist, so a
  # fresh node burns through systemd's start limit and every later restart is
  # refused ("Start request repeated too quickly"). Clear that state first; the
  # exec stays idempotent by skipping a unit that is already active.
  exec { 'reset-failed-blackbox-exporter':
    command => '/usr/bin/systemctl reset-failed prometheus-blackbox-exporter || true',
    unless  => '/usr/bin/systemctl is-active --quiet prometheus-blackbox-exporter',
    require => Package['prometheus-blackbox-exporter'],
    before  => Service['prometheus-blackbox-exporter'],
  }

  service { 'prometheus-blackbox-exporter':
    ensure  => running,
    enable  => true,
    require => [
      Package['prometheus-blackbox-exporter'],
      File['/etc/default/prometheus-blackbox-exporter'],
      File['/etc/prometheus/blackbox.yml'],
    ],
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
    command => '/usr/bin/curl -fsSL https://apt.grafana.com/gpg.key | /usr/bin/gpg --dearmor --yes -o /usr/share/keyrings/grafana.gpg',
    unless  => '/usr/bin/test -s /usr/share/keyrings/grafana.gpg',
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
    command => '/usr/bin/apt-get update',
    unless  => '/usr/bin/dpkg -s grafana >/dev/null 2>&1',
    require => File['/etc/apt/sources.list.d/grafana.list'],
  }

  # Keep this pinned: the Grafana APT repository serves a moving `stable`
  # channel, and an unpinned package makes deploys non-reproducible. Update
  # this version together with docs/SOFTWARE_BASELINE.md.
  package { 'grafana':
    ensure  => '13.2.4',
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

  # HA overview: DRBD replication/role, etcd quorum, keepalived VIP ownership,
  # blackbox probes and k6 load-test metrics (see alert-rules.yml).
  file { '/var/lib/grafana/dashboards/ha-overview.json':
    ensure  => file,
    owner   => 'grafana',
    group   => 'grafana',
    mode    => '0644',
    source  => 'puppet:///modules/role/grafana/ha-overview.json',
    require => File['/var/lib/grafana/dashboards'],
    notify  => Service['grafana-server'],
  }

  # First open in the browser must land on the HA graphs, not on Grafana's
  # welcome page. [dashboards] default_home_dashboard_path reads the file
  # straight from disk, so it works before the dashboard is registered in the
  # database. The value is passed as a systemd drop-in environment variable
  # (GF_<section>_<key>) to leave the packaged unit and grafana.ini untouched;
  # `cms-ha verify` asserts the home dashboard through Grafana's API.
  file { '/etc/systemd/system/grafana-server.service.d':
    ensure  => directory,
    owner   => 'root',
    group   => 'root',
    mode    => '0755',
    require => Package['grafana'],
  }

  file { '/etc/systemd/system/grafana-server.service.d/home-dashboard.conf':
    ensure  => file,
    owner   => 'root',
    group   => 'root',
    mode    => '0644',
    # The monitoring phase writes the admin password to this file (0640
    # root:grafana) after Puppet converges. The leading '-' keeps the unit
    # bootable on a fresh node, before that phase has run; the CLI reset makes
    # the password effective even on an existing database.
    content => "[Service]\nEnvironmentFile=-/etc/grafana/grafana-admin.env\nEnvironment=GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH=/var/lib/grafana/dashboards/ha-overview.json\n",
    require => [
      Package['grafana'],
      File['/etc/systemd/system/grafana-server.service.d'],
      File['/var/lib/grafana/dashboards/ha-overview.json'],
    ],
    notify  => [Exec['grafana-systemd-reload'], Service['grafana-server']],
  }

  # Environment= in a drop-in only reaches the new process after systemd has
  # re-read its units, and that has to happen before the restart above.
  exec { 'grafana-systemd-reload':
    command     => '/usr/bin/systemctl daemon-reload',
    refreshonly => true,
    before      => Service['grafana-server'],
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

  exec { 'ufw-blackbox':
    command => '/usr/sbin/ufw allow from 192.168.10.0/24 to any port 9115 proto tcp comment "Blackbox exporter"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "9115"',
    require => Exec['ufw-enable'],
  }

  exec { 'ufw-alertmanager':
    command => '/usr/sbin/ufw allow from 192.168.10.0/24 to any port 9093 proto tcp comment "Alertmanager"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "9093"',
    require => Exec['ufw-enable'],
  }
}
