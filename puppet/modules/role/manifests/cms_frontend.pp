# role::cms_frontend — Apache + PHP + WordPress frontend (main-cms1, main-cms2).
#
# Manages: Apache2, all required PHP modules, WordPress download and DB config,
# WP-CLI installation and idempotent core install, UFW rules.

class role::cms_frontend {

  include role::base

  $storage_ip = lookup('cms_ha::storage_ip', String, undef, '192.168.10.15')
  $monitor_ip = lookup('cms_ha::monitor_ip', String, undef, '192.168.10.20')

  # The Ubuntu Noble revision of the PHP 8.3 branch that carries the fix for
  # CVE-2026-109618 and CVE-2026-109619 ("php2shell", published 2026-10-21).
  # Ubuntu backports the upstream patch instead of bumping the upstream
  # version, so a patched node still answers "PHP 8.3.6": the Debian revision
  # is what tells a patched node from a vulnerable one. This is the single
  # place to bump when the next PHP security update lands.
  $php_patched_version = '8.3.6-0ubuntu0.24.04.13'

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
    'prometheus-apache-exporter',
    'nfs-common',
  ]:
    ensure => installed,
  }

  # ---------------------------------------------------------------------------
  # PHP SECURITY PATCH — CVE-2026-109618 / CVE-2026-109619 ("php2shell")
  #
  # The metapackages above are only installed, never upgraded, so a node
  # provisioned before the patch would keep the vulnerable build forever. The
  # versioned packages below carry the floor instead: every Puppet run moves
  # them to the patched release (or to a newer one).
  # ---------------------------------------------------------------------------

  # Refresh the APT cache so the patched release is visible. The guard skips
  # the refresh once every pinned package satisfies the floor, so a converged
  # node does not reach for the archive on every run. Checking all three
  # matters: a patched CLI with an outdated libapache2-mod-php8.3 would still
  # leave Apache serving the vulnerable code. flock keeps concurrent runs of
  # this exec from overlapping.
  exec { 'apt-get-update-cms-frontend':
    command => '/usr/bin/flock -w 600 /var/lib/dpkg/lock-frontend /usr/bin/apt-get update',
    unless  => "/bin/bash -c 'ok=1; for p in php8.3-cli php8.3-common libapache2-mod-php8.3; do v=\$(/usr/bin/dpkg-query -W \"\$p\" 2>/dev/null | /usr/bin/cut -f2); if [ -z \"\$v\" ] || ! /usr/bin/dpkg --compare-versions \"\$v\" ge \"${php_patched_version}\"; then ok=0; fi; done; [ \"\$ok\" = 1 ]'",
  }

  # No space after the operator: Puppet parses the value as a Debian version
  # range and only accepts forms such as ">=1.2.3". With the space the parse
  # fails and the whole string reaches apt-get as a literal version.
  package { ['php8.3-cli', 'php8.3-common', 'libapache2-mod-php8.3']:
    ensure  => ">=${php_patched_version}",
    require => Exec['apt-get-update-cms-frontend'],
    # mod_php lives inside the Apache process: the new library is only used
    # after Apache is restarted.
    notify  => Service['apache2'],
  }

  # ---------------------------------------------------------------------------
  # PHP HARDENING — the mitigations both CVEs need, on top of the patch
  #
  # The patched package is the fix. These settings remove the request
  # patterns the two bugs need, so a frontend that missed the package
  # upgrade is not exploitable through them either.
  # ---------------------------------------------------------------------------

  # The Apache SAPI reads its ini files at startup, so the setting is in
  # place before the request is parsed: the PHP team asks for exactly this,
  # and not for ini_set() (too late) nor for a .user.ini (per request).
  file { '/etc/php/8.3/apache2/conf.d':
    ensure => directory,
    owner  => 'root',
    group  => 'root',
    mode   => '0755',
  }

  file { '/etc/php/8.3/apache2/conf.d/99-cms-ha-php-hardening.ini':
    ensure  => file,
    owner   => 'root',
    group   => 'root',
    mode    => '0644',
    require => [
      Package['libapache2-mod-php8.3'],
      File['/etc/php/8.3/apache2/conf.d'],
    ],
    notify  => Service['apache2'],
    content => @("PHPINIHARDENING")
      # CMS-HA - PHP hardening for CVE-2026-109618 / CVE-2026-109619
      #
      # The patched PHP package is the fix for both bugs. This file closes the
      # session path that CVE-2026-109619 was confirmed with: the upload
      # progress tracking is enabled by default and can leave a request's
      # internal state pointing at freed memory that a later request reads.
      #
      # It is read when Apache starts, so it applies before the request is
      # parsed, which is where the bug lives.
      session.upload_progress.enabled = Off
      | PHPINIHARDENING
  }

  # The same mitigations at the web server, where they cannot be undone by
  # the application: php_admin_value is not overridable with ini_set() and
  # not overridable from .htaccess, and the request size is bounded before
  # PHP ever sees the body.
  file { '/etc/apache2/conf-available/php-hardening.conf':
    ensure  => file,
    owner   => 'root',
    group   => 'root',
    mode    => '0644',
    require => Package['libapache2-mod-php8.3'],
    notify  => Service['apache2'],
    content => @("APACHEPHPHARDENING")
      # CMS-HA - PHP hardening for CVE-2026-109618 / CVE-2026-109619
      #
      # The version floor above supplies the CVE fixes; unlimited memory is
      # not safe on the 1 GiB lab nodes. Prefork concurrency is bounded in
      # mods-available/mpm_prefork.conf, leaving headroom for Apache and the OS.
      <Directory /var/www/html>
          php_admin_value memory_limit 128M
          php_admin_flag session.upload_progress.enabled Off
          php_admin_value post_max_size 32M
          php_admin_value upload_max_filesize 16M
          LimitRequestBody 33554432
      </Directory>
      | APACHEPHPHARDENING
  }

  # Replace the packaged prefork settings at their standard location rather
  # than redefining them later in conf-enabled/php-hardening.conf. This keeps
  # a single authoritative limit and consistent initial/spare process counts.
  file { '/etc/apache2/mods-available/mpm_prefork.conf':
    ensure  => file,
    owner   => 'root',
    group   => 'root',
    mode    => '0644',
    require => Package['apache2'],
    notify  => Service['apache2'],
    content => @("APACHEPREFORK")
      # CMS-HA - bounded mod_php concurrency for 1 GiB lab frontends
      <IfModule mpm_prefork_module>
          StartServers 2
          MinSpareServers 1
          MaxSpareServers 2
          ServerLimit 4
          MaxRequestWorkers 4
          MaxConnectionsPerChild 500
      </IfModule>
      | APACHEPREFORK
  }

  exec { 'apache-enable-php-hardening':
    command => '/usr/sbin/a2enconf php-hardening',
    unless  => '/usr/bin/test -L /etc/apache2/conf-enabled/php-hardening.conf',
    require => File['/etc/apache2/conf-available/php-hardening.conf'],
    notify  => Service['apache2'],
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
    require => [
      Package['apache2'],
      Exec['mount-wp-uploads'],
      Exec['apache-storage-daemon-reload'],
      File['/etc/systemd/system/apache2.service.d/uploads.conf'],
    ],
  }

  file { '/etc/apache2/conf-available/server-status.conf':
    ensure  => file,
    owner   => 'root',
    group   => 'root',
    mode    => '0644',
    content => "<Location /server-status>\n    SetHandler server-status\n    Require local\n    RewriteEngine Off\n</Location>\n",
    require => Package['apache2'],
    notify  => Service['apache2'],
  }

  exec { 'apache-enable-server-status':
    command => '/usr/sbin/a2enconf server-status',
    unless  => '/usr/bin/test -L /etc/apache2/conf-enabled/server-status.conf',
    require => File['/etc/apache2/conf-available/server-status.conf'],
    notify  => Service['apache2'],
  }

  service { 'prometheus-apache-exporter':
    ensure  => running,
    enable  => true,
    require => [
      Package['prometheus-apache-exporter'],
      Service['apache2'],
      Exec['apache-enable-server-status'],
    ],
  }

  # ---------------------------------------------------------------------------
  # WORDPRESS — Download and extract (idempotent via creates guard)
  # ---------------------------------------------------------------------------
  exec { 'wordpress-download':
    command => '/bin/rm -f /tmp/wordpress-latest.tar.gz && /usr/bin/wget -q --timeout=30 --tries=3 -O /tmp/wordpress-latest.tar.gz https://wordpress.org/latest.tar.gz',
    unless  => '/bin/tar -tzf /tmp/wordpress-latest.tar.gz >/dev/null 2>&1',
    require => Package['wget'],
  }

  exec { 'wordpress-extract':
    command => '/bin/tar -xzf /tmp/wordpress-latest.tar.gz -C /tmp/',
    creates => '/tmp/wordpress/wp-config-sample.php',
    require => Exec['wordpress-download'],
  }

  # Copy over the tree instead of deleting /var/www/html first. The uploads
  # directory is an NFS mount and wp-config.php contains generated secrets;
  # a re-run of this resource must not wipe either of them.
  exec { 'wordpress-deploy':
    command => '/bin/cp -a /tmp/wordpress/. /var/www/html/ && \
                /bin/chown -R www-data:www-data /var/www/html/',
    creates => '/var/www/html/wp-config-sample.php',
    require => Exec['wordpress-extract'],
  }

  # Apache ships /var/www/html/index.html and DirectoryIndex prefers it over
  # index.php, so it shadows the WordPress home page. Keep this as its own
  # resource: wordpress-deploy has a creates guard and is skipped on re-runs,
  # so an inline removal would leave the file behind on an upgraded node.
  file { '/var/www/html/index.html':
    ensure  => absent,
    require => [Package['apache2'], Exec['wordpress-deploy']],
  }

  # ---------------------------------------------------------------------------
  # WORDPRESS — wp-config.php with database credentials
  # ---------------------------------------------------------------------------
  file { '/usr/local/sbin/cms-ha-configure-wordpress.py':
    ensure => file,
    owner  => 'root',
    group  => 'root',
    mode   => '0755',
    source => 'puppet:///modules/role/cms-ha/configure-wordpress.py',
  }

  exec { 'wordpress-config-create':
    command => '/bin/cp /var/www/html/wp-config-sample.php /var/www/html/wp-config.php',
    creates => '/var/www/html/wp-config.php',
    require => Exec['wordpress-deploy'],
  }

  exec { 'wordpress-config-db-name':
    command => '/usr/bin/python3 /usr/local/sbin/cms-ha-configure-wordpress.py configure-db-name',
    unless  => '/usr/bin/python3 /usr/local/sbin/cms-ha-configure-wordpress.py check-db-name',
    require => [Exec['wordpress-config-create'], File['/usr/local/sbin/cms-ha-configure-wordpress.py']],
  }

  exec { 'wordpress-config-db-user':
    command => '/usr/bin/python3 /usr/local/sbin/cms-ha-configure-wordpress.py configure-db-user',
    unless  => '/usr/bin/python3 /usr/local/sbin/cms-ha-configure-wordpress.py check-db-user',
    require => [Exec['wordpress-config-create'], File['/usr/local/sbin/cms-ha-configure-wordpress.py']],
  }

  exec { 'wordpress-config-db-pass':
    command => '/usr/bin/python3 /usr/local/sbin/cms-ha-configure-wordpress.py configure-db',
    unless  => '/usr/bin/python3 /usr/local/sbin/cms-ha-configure-wordpress.py check-db',
    require => [Exec['wordpress-config-create'], File['/usr/local/sbin/cms-ha-configure-wordpress.py']],
  }

  exec { 'wordpress-config-db-host':
    command => '/usr/bin/python3 /usr/local/sbin/cms-ha-configure-wordpress.py configure-db-host',
    unless  => '/usr/bin/python3 /usr/local/sbin/cms-ha-configure-wordpress.py check-db-host',
    require => [Exec['wordpress-config-create'], File['/usr/local/sbin/cms-ha-configure-wordpress.py']],
  }

  exec { 'wordpress-config-ssl-reverse-proxy':
    command => '/bin/sed -i "/<?php/a if (isset(\$_SERVER[\'HTTP_X_FORWARDED_PROTO\']) && \$_SERVER[\'HTTP_X_FORWARDED_PROTO\'] === \'https\') { \$_SERVER[\'HTTPS\'] = \'on\'; }" /var/www/html/wp-config.php',
    unless  => '/usr/bin/grep -q "HTTP_X_FORWARDED_PROTO" /var/www/html/wp-config.php',
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
  # SHARED STORAGE — NFS mount for WordPress uploads (centralised persistence)
  # ---------------------------------------------------------------------------
  # Do not chmod/chown the mounted export on subsequent Puppet runs.
  file { '/var/www/html/wp-content/uploads':
    ensure  => directory,
    require => Exec['wordpress-deploy'],
  }

  # If the mount disappears, www-data must not write into the underlying
  # local directory while systemd is stopping Apache. Run only when unmounted.
  exec { 'protect-local-uploads':
    command => '/bin/chown root:root /var/www/html/wp-content/uploads && /bin/chmod 0555 /var/www/html/wp-content/uploads',
    onlyif  => '/bin/bash -c "! /bin/mountpoint -q /var/www/html/wp-content/uploads"',
    require => File['/var/www/html/wp-content/uploads'],
    before  => Mount['/var/www/html/wp-content/uploads'],
  }

  file { '/usr/local/sbin/cms-ha-check-uploads':
    ensure  => file,
    owner   => 'root',
    group   => 'root',
    mode    => '0755',
    content => @(UPLOADSCHECK)
      #!/bin/sh
      set -eu
      # -M requires the exact mountpoint, not an ancestor filesystem. Check
      # the export, filesystem, write mode and root (reject subdirectory binds).
      # legacy-soft is only a migration gate; service startup defaults to hard.
      record=$(/usr/bin/findmnt -rn -M /var/www/html/wp-content/uploads -o SOURCE,FSTYPE,OPTIONS,FSROOT) || exit 1
      printf '%s\n' "$record" | /usr/bin/awk -v expected="$1" -v mode="${2:-hard}" '
          NF == 4 && $1 == expected && ($2 == "nfs" || $2 == "nfs4") &&
          $3 ~ /(^|,)rw(,|$)/ && $3 !~ /(^|,)ro(,|$)/ && $4 == "/" {
            if (mode == "hard")
              ok = ($3 ~ /(^|,)hard(,|$)/ && $3 !~ /(^|,)(soft|softerr|softreval)(,|$)/)
            else if (mode == "legacy-soft")
              ok = ($3 ~ /(^|,)soft(,|$)/ && $3 !~ /(^|,)(hard|softerr|softreval)(,|$)/)
          }
          END { exit !(ok && NR == 1) }'
      | UPLOADSCHECK
  }

  file { '/usr/local/sbin/cms-ha-mount-uploads':
    ensure  => file,
    owner   => 'root',
    group   => 'root',
    mode    => '0755',
    require => File['/usr/local/sbin/cms-ha-check-uploads'],
    content => @(UPLOADSMOUNT)
      #!/bin/sh
      set -eu
      target=/var/www/html/wp-content/uploads
      expected="${1:?expected NFS export required}"
      check() { /usr/local/sbin/cms-ha-check-uploads "$expected" "${1:-hard}"; }
      fail() { echo "ERROR: $*" >&2; exit 1; }

      check && exit 0
      /bin/systemctl stop apache2
      state=$(/bin/systemctl show --property=ActiveState --value apache2) || fail "Cannot query Apache state"
      [ "$state" = inactive ] || fail "Apache is not fully stopped"

      if /bin/mountpoint -q "$target"; then
        # Only the known writable legacy export may be changed. In particular,
        # do not repair an arbitrary mount, a read-only export or a bind root.
        check legacy-soft || fail "Refusing to migrate an unexpected uploads mount"
        if ! /bin/mount -o remount,rw,hard,timeo=600,retrans=2,nfsvers=4 "$expected" "$target"; then
          echo "NFS remount failed; checking whether a normal unmount/mount is safe" >&2
        fi
        check && exit 0
        # Remount can fail or silently retain soft. Recheck before unmounting:
        # a changed identity or mode must never trigger destructive recovery.
        check legacy-soft || fail "Uploads mount changed during migration"
        /bin/umount "$target"
        if /bin/mountpoint -q "$target"; then
          fail "Another mount remains at the uploads path"
        fi
      fi

      # A legacy mount hid this local directory from protect-local-uploads.
      # Protect it now while exposed, without chmod/chown on the NFS export.
      /bin/chown root:root "$target"
      /bin/chmod 0555 "$target"
      /bin/mount -t nfs -o rw,hard,timeo=600,retrans=2,nfsvers=4 "$expected" "$target"
      check || fail "Uploads mount failed final verification"
      # Puppet, not this helper, starts Apache only after successful return.
      | UPLOADSMOUNT
  }

  $uploads_mount_unit = 'var-www-html-wp\x2dcontent-uploads.mount'

  file { '/etc/systemd/system/apache2.service.d':
    ensure  => directory,
    owner   => 'root',
    group   => 'root',
    mode    => '0755',
    require => Package['apache2'],
  }

  file { '/etc/systemd/system/apache2.service.d/uploads.conf':
    ensure  => file,
    owner   => 'root',
    group   => 'root',
    mode    => '0644',
    content => @("UPLOADSUNIT"),
      [Unit]
      RequiresMountsFor=/var/www/html/wp-content/uploads
      BindsTo=${uploads_mount_unit}
      After=${uploads_mount_unit}

      [Service]
      ExecStartPre=/usr/local/sbin/cms-ha-check-uploads ${storage_ip}:/srv/nfs/wp-uploads
      | UPLOADSUNIT
    require => [
      File['/etc/systemd/system/apache2.service.d'],
      File['/usr/local/sbin/cms-ha-check-uploads'],
    ],
    notify  => [Exec['apache-storage-daemon-reload'], Service['apache2']],
  }

  exec { 'apache-storage-daemon-reload':
    command     => '/bin/systemctl daemon-reload',
    refreshonly => true,
    subscribe   => Mount['/var/www/html/wp-content/uploads'],
    before      => Service['apache2'],
  }

  mount { '/var/www/html/wp-content/uploads':
    ensure  => present,
    device  => "${storage_ip}:/srv/nfs/wp-uploads",
    fstype  => 'nfs',
    options => '_netdev,auto,rw,hard,timeo=600,retrans=2,nfsvers=4',
    require => [
      Package['nfs-common'],
      File['/var/www/html/wp-content/uploads'],
      # Verify/migrate the live mount before the provider updates fstab. An
      # unexpected live export must block this resource too.
      Exec['mount-wp-uploads'],
    ],
  }

  # Dependencies protect future starts; also stop an already-running Apache
  # during an upgrade if the current mount is absent or unsafe. A failed
  # recovery must leave the existing service stopped, not merely skip restart.
  exec { 'apache-stop-unsafe-uploads':
    command => '/bin/systemctl stop apache2',
    unless  => "/usr/local/sbin/cms-ha-check-uploads ${storage_ip}:/srv/nfs/wp-uploads",
    require => [
      Package['apache2'],
      File['/usr/local/sbin/cms-ha-check-uploads'],
    ],
    before  => Exec['mount-wp-uploads'],
  }

  exec { 'mount-wp-uploads':
    # Migrate only the verified legacy export while Apache is stopped. Any
    # failed recovery blocks Apache and the shared bootstrap lock.
    command => "/usr/local/sbin/cms-ha-mount-uploads ${storage_ip}:/srv/nfs/wp-uploads",
    unless  => "/usr/local/sbin/cms-ha-check-uploads ${storage_ip}:/srv/nfs/wp-uploads",
    require => [
      Package['nfs-common'],
      File['/var/www/html/wp-content/uploads'],
      File['/usr/local/sbin/cms-ha-check-uploads'],
      File['/usr/local/sbin/cms-ha-mount-uploads'],
      Exec['apache-stop-unsafe-uploads'],
    ],
  }

  # ---------------------------------------------------------------------------
  # WP-CLI — WordPress command-line interface (idempotent install)
  # ---------------------------------------------------------------------------
  exec { 'wpcli-install':
    command => '/usr/bin/wget -q --timeout=30 --tries=3 -O /usr/local/bin/wp \
      https://raw.githubusercontent.com/wp-cli/builds/gh-pages/phar/wp-cli.phar \
      && /bin/chmod +x /usr/local/bin/wp',
    unless  => '/usr/bin/test -s /usr/local/bin/wp && /usr/bin/test -x /usr/local/bin/wp',
    require => Package['wget'],
  }

  # ---------------------------------------------------------------------------
  # WORDPRESS CORE INSTALL — Runs only if WordPress is not yet installed in DB
  # ---------------------------------------------------------------------------
  # The uploads share is the only lock both frontends can see. Re-check inside
  # the lock: a resumed Puppet run can start this resource on both nodes even
  # though phase 07 normally serialises them.
  exec { 'wordpress-core-install':
    command => '/usr/bin/flock -w 300 /var/www/html/wp-content/uploads/.cms-ha-install.lock /bin/bash -c \'/usr/local/bin/wp core is-installed --allow-root --path=/var/www/html || /usr/bin/python3 /usr/local/sbin/cms-ha-configure-wordpress.py install\'',
    onlyif  => '/usr/bin/python3 /usr/local/sbin/cms-ha-configure-wordpress.py db-reachable',
    unless  => '/usr/bin/test -x /usr/local/bin/wp && /usr/local/bin/wp core is-installed --allow-root --path=/var/www/html',
    require => [
      Exec['wpcli-install'],
      Exec['wordpress-config-db-host'],
      Exec['wordpress-config-db-user'],
      Exec['wordpress-config-db-pass'],
      File['/usr/local/sbin/cms-ha-configure-wordpress.py'],
      Exec['wordpress-config-db-name'],
      Exec['wordpress-config-ssl-reverse-proxy'],
      Mount['/var/www/html/wp-content/uploads'],
      Exec['mount-wp-uploads'],
    ],
    user    => 'root',
    timeout => 420,
  }

  exec { 'wordpress-rewrite-structure':
    command => '/usr/local/bin/wp rewrite structure \'/%postname%/\' --hard --allow-root --path=/var/www/html',
    onlyif  => '/usr/bin/python3 /usr/local/sbin/cms-ha-configure-wordpress.py db-reachable',
    # `wp rewrite list` prints the generated rewrite *rules*, not the permalink
    # structure, so it never contains "postname" and the exec re-ran every time.
    # The option is what the command actually sets, so read that and match it
    # exactly (a different structure that merely contains "postname" must still
    # be rewritten).
    unless  => '/usr/bin/test -x /usr/local/bin/wp && /usr/local/bin/wp option get permalink_structure --allow-root --path=/var/www/html 2>/dev/null | grep -qx "/%postname%/"',
    require => Exec['wordpress-core-install'],
    user    => 'root',
  }

  # ---------------------------------------------------------------------------
  # UFW — Firewall rules for CMS frontend nodes
  # ---------------------------------------------------------------------------
  exec { 'ufw-cms-http':
    command => '/usr/sbin/ufw allow 80/tcp comment "HTTP WordPress"',
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "80/tcp.*ALLOW"',
    require => Exec['ufw-enable'],
  }

  exec { 'ufw-cms-apache-exporter':
    command => "/usr/sbin/ufw allow from ${monitor_ip} to any port 9117 proto tcp comment \"apache_exporter\"",
    unless  => '/usr/sbin/ufw status | /usr/bin/grep -q "9117"',
    require => Exec['ufw-enable'],
  }
}
