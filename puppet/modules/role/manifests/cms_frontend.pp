# role::cms_frontend — Apache + PHP + WordPress frontend (main-cms1, main-cms2).
#
# Manages: Apache2, all required PHP modules, WordPress download and DB config,
# WP-CLI installation and idempotent core install, UFW rules.

class role::cms_frontend {

  include role::base

  # ---------------------------------------------------------------------------
  # PACKAGES — Apache + full PHP stack for WordPress
  # ---------------------------------------------------------------------------
  package { [
    'apache2',
    'php',
    'php-mysql',
    'php-curl',
    'php-gd',
    'php-xml',
    'php-mbstring',
    'libapache2-mod-php',
    'wget',
    'tar',
    'curl',
  ]:
    ensure => installed,
  }

  # ---------------------------------------------------------------------------
  # APACHE — VirtualHost configuration
  # ---------------------------------------------------------------------------

  # Disable default site before enabling WordPress
  exec { 'apache-disable-default-site':
    command => '/usr/sbin/a2dissite 000-default.conf',
    onlyif  => '/usr/bin/test -L /etc/apache2/sites-enabled/000-default.conf',
    require => Package['apache2'],
    notify  => Service['apache2'],
  }

  # Enable mod_rewrite (required for WordPress permalinks)
  exec { 'apache-enable-rewrite':
    command => '/usr/sbin/a2enmod rewrite',
    unless  => '/usr/bin/test -f /etc/apache2/mods-enabled/rewrite.load',
    require => Package['apache2'],
    notify  => Service['apache2'],
  }

  # Deploy WordPress VirtualHost config from Puppet fileserver
  file { '/etc/apache2/sites-available/wordpress.conf':
    ensure  => file,
    owner   => 'root',
    group   => 'root',
    mode    => '0644',
    source  => 'puppet:///modules/role/apache/wordpress.conf',
    require => Package['apache2'],
    notify  => Service['apache2'],
  }

  exec { 'apache-enable-wordpress-site':
    command => '/usr/sbin/a2ensite wordpress.conf',
    unless  => '/usr/bin/test -L /etc/apache2/sites-enabled/wordpress.conf',
    require => File['/etc/apache2/sites-available/wordpress.conf'],
    notify  => Service['apache2'],
  }

  service { 'apache2':
    ensure  => running,
    enable  => true,
    require => Package['apache2'],
  }

  # ---------------------------------------------------------------------------
  # WORDPRESS — Download and extract (idempotent via creates guard)
  # ---------------------------------------------------------------------------
  exec { 'wordpress-download':
    command => '/usr/bin/wget -q --tries=3 -O /tmp/wordpress-latest.tar.gz https://wordpress.org/latest.tar.gz',
    creates => '/tmp/wordpress-latest.tar.gz',
    require => Package['wget'],
  }

  exec { 'wordpress-extract':
    command => '/bin/tar -xzf /tmp/wordpress-latest.tar.gz -C /tmp/',
    creates => '/tmp/wordpress/wp-config-sample.php',
    require => Exec['wordpress-download'],
  }

  exec { 'wordpress-deploy':
    command => '/bin/rm -rf /var/www/html/* && /bin/cp -a /tmp/wordpress/. /var/www/html/ && \
                /bin/chown -R www-data:www-data /var/www/html/',
    creates => '/var/www/html/wp-config-sample.php',
    require => Exec['wordpress-extract'],
  }

  # ---------------------------------------------------------------------------
  # WORDPRESS — wp-config.php with database credentials
  # ---------------------------------------------------------------------------
  exec { 'wordpress-config-create':
    command => '/bin/cp /var/www/html/wp-config-sample.php /var/www/html/wp-config.php',
    creates => '/var/www/html/wp-config.php',
    require => Exec['wordpress-deploy'],
  }

  exec { 'wordpress-config-db-name':
    command => "/bin/sed -i \"s/database_name_here/wordpress/\" /var/www/html/wp-config.php",
    unless  => "/usr/bin/grep -q \"define.*DB_NAME.*'wordpress'\" /var/www/html/wp-config.php",
    require => Exec['wordpress-config-create'],
  }

  exec { 'wordpress-config-db-user':
    command => "/bin/sed -i \"s/username_here/wp_user/\" /var/www/html/wp-config.php",
    unless  => "/usr/bin/grep -q \"define.*DB_USER.*'wp_user'\" /var/www/html/wp-config.php",
    require => Exec['wordpress-config-create'],
  }

  exec { 'wordpress-config-db-pass':
    command => "/bin/sed -i \"s/password_here/WpS3cur3P4ss!/\" /var/www/html/wp-config.php",
    unless  => "/usr/bin/grep -q \"define.*DB_PASSWORD.*'WpS3cur3P4ss!'\" /var/www/html/wp-config.php",
    require => Exec['wordpress-config-create'],
  }

  exec { 'wordpress-config-db-host':
    command => "/bin/sed -i \"s/localhost/192.168.10.11:30306/\" /var/www/html/wp-config.php",
    unless  => "/usr/bin/grep -q 'DB_HOST.*192.168.10.11:30306' /var/www/html/wp-config.php",
    require => Exec['wordpress-config-create'],
  }

  # Deploy .htaccess for clean URL rewriting
  file { '/var/www/html/.htaccess':
    ensure  => file,
    owner   => 'www-data',
    group   => 'www-data',
    mode    => '0644',
    source  => 'puppet:///modules/role/apache/wordpress.htaccess',
    require => Exec['wordpress-deploy'],
  }

  # ---------------------------------------------------------------------------
  # WP-CLI — WordPress command-line interface (idempotent install)
  # ---------------------------------------------------------------------------
  exec { 'wpcli-install':
    command => '/usr/bin/wget -q -O /usr/local/bin/wp \
      https://raw.githubusercontent.com/wp-cli/builds/gh-pages/phar/wp-cli.phar \
      && /bin/chmod +x /usr/local/bin/wp',
    creates => '/usr/local/bin/wp',
    require => Package['wget'],
  }

  # ---------------------------------------------------------------------------
  # WORDPRESS CORE INSTALL — Runs only if WordPress is not yet installed in DB
  # ---------------------------------------------------------------------------
  exec { 'wordpress-core-install':
    command => '/usr/local/bin/wp core install \
      --url="https://cms.fake-enterprise.com" \
      --title="Fake Enterprise CMS" \
      --admin_user="admin" \
      --admin_password="WpS3cur3P4ss!" \
      --admin_email="admin@fake-enterprise.com" \
      --skip-email \
      --allow-root \
      --path=/var/www/html',
    unless  => '/usr/local/bin/wp core is-installed --allow-root --path=/var/www/html',
    require => [
      Exec['wpcli-install'],
      Exec['wordpress-config-db-host'],
      Exec['wordpress-config-db-user'],
      Exec['wordpress-config-db-pass'],
      Exec['wordpress-config-db-name'],
    ],
    user    => 'root',
    timeout => 120,
  }

  exec { 'wordpress-rewrite-structure':
    command => '/usr/local/bin/wp rewrite structure \'/%postname%/\' --hard --allow-root --path=/var/www/html',
    unless  => '/usr/local/bin/wp rewrite list --allow-root --path=/var/www/html 2>/dev/null | grep -q "postname"',
    require => Exec['wordpress-core-install'],
    user    => 'root',
  }

  # ---------------------------------------------------------------------------
  # UFW — Firewall rules for CMS frontend nodes
  # ---------------------------------------------------------------------------
  exec { 'ufw-cms-http':
    command => '/usr/sbin/ufw allow 80/tcp comment "HTTP WordPress"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "80/tcp.*ALLOW IN"',
    require => Exec['ufw-enable'],
  }

  exec { 'ufw-cms-apache-exporter':
    command => '/usr/sbin/ufw allow from 192.168.10.20 to any port 9117 proto tcp comment "apache_exporter"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "9117"',
    require => Exec['ufw-enable'],
  }
}
