#!/usr/bin/env python3
"""Apply local runtime credentials to WordPress without putting them in Puppet code."""

import json
import grp
import os
import re
import socket
import subprocess
import sys
import tempfile

FACTS = "/etc/cms-ha/runtime-secrets.json"
WP_CONFIG = "/var/www/html/wp-config.php"


def read_facts():
    with open(FACTS, encoding="utf-8") as stream:
        return json.load(stream)["cms_ha"]


def php_string(value):
    return "'" + value.replace("\\", "\\\\").replace("'", "\\'") + "'"


def write_config(content):
    fd, temp_path = tempfile.mkstemp(dir=os.path.dirname(WP_CONFIG), prefix=".wp-config-")
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            stream.write(content)
        os.chmod(temp_path, 0o640)
        os.chown(temp_path, 0, grp.getgrnam("www-data").gr_gid)
        os.replace(temp_path, WP_CONFIG)
    finally:
        if os.path.exists(temp_path):
            os.unlink(temp_path)


def configure_db_identity(key, value, check_only=False):
    if not value:
        raise RuntimeError(f"{key} is empty in runtime-secrets.json")
    with open(WP_CONFIG, encoding="utf-8") as stream:
        content = stream.read()
    pattern = re.compile(
        r"define\s*\(\s*(['\"])" + re.escape(key) + r"\1\s*,\s*.*?\s*\);",
        re.S,
    )
    replacement = "define( '" + key + "', " + php_string(value) + " );"
    updated, count = pattern.subn(lambda _match: replacement, content, count=1)
    if count != 1:
        raise RuntimeError(f"could not locate {key} in wp-config.php")
    if check_only:
        return 0 if updated == content else 1
    if updated != content:
        write_config(updated)
    return 0


def configure_db(check_only=False):
    password = read_facts()["database_password"]
    with open(WP_CONFIG, encoding="utf-8") as stream:
        content = stream.read()
    pattern = re.compile(r"define\s*\(\s*(['\"]DB_PASSWORD['\"])\s*,\s*.*?\s*\);", re.S)
    replacement = "define( 'DB_PASSWORD', " + php_string(password) + " );"
    # A callable replacement prevents re.sub from interpreting backslashes in
    # the escaped PHP password as replacement-group syntax.
    updated, count = pattern.subn(lambda _match: replacement, content, count=1)
    if count != 1:
        raise RuntimeError("could not locate DB_PASSWORD in wp-config.php")
    if check_only:
        return 0 if updated == content else 1
    if updated != content:
        write_config(updated)
    return 0


def configure_db_host(check_only=False):
    raw_hosts = read_facts().get("database_hosts", "")
    hosts = [host.strip() for host in raw_hosts.split(",") if host.strip()]
    if not hosts:
        raise RuntimeError("database_hosts is empty in runtime-secrets.json")
    with open(WP_CONFIG, encoding="utf-8") as stream:
        content = stream.read()

    php_hosts = ", ".join(php_string(host) for host in hosts)
    replacement = (
        "$db_hosts = [" + php_hosts + "];\n"
        "$active_db_host = $db_hosts[0];\n"
        "foreach ( $db_hosts as $db_host ) {\n"
        "    $db_host_parts = explode( ':', $db_host );\n"
        "    $db_host_port = isset( $db_host_parts[1] ) ? (int) $db_host_parts[1] : 3306;\n"
        "    $db_host_socket = @fsockopen( $db_host_parts[0], $db_host_port, $db_host_errno, $db_host_errstr, 1 );\n"
        "    if ( $db_host_socket ) {\n"
        "        fclose( $db_host_socket );\n"
        "        $active_db_host = $db_host;\n"
        "        break;\n"
        "    }\n"
        "}\n"
        "define( 'DB_HOST', $active_db_host );"
    )

    # Replace the full failover block when it already exists, otherwise replace
    # the stock localhost definition. Matching the block avoids leaving a
    # stale hardcoded host list behind after a configuration change.
    block_pattern = re.compile(
        r"\$db_hosts\s*=\s*\[.*?define\s*\(\s*['\"]DB_HOST['\"]\s*,\s*\$active_db_host\s*\)\s*;",
        re.S,
    )
    updated, count = block_pattern.subn(lambda _match: replacement, content, count=1)
    if count == 0:
        localhost_pattern = re.compile(
            r"define\s*\(\s*(['\"])DB_HOST\1\s*,\s*.*?\s*\);",
            re.S,
        )
        updated, count = localhost_pattern.subn(lambda _match: replacement, content, count=1)
    if count != 1:
        raise RuntimeError("could not locate DB_HOST in wp-config.php")
    if check_only:
        return 0 if updated == content else 1
    if updated != content:
        write_config(updated)
    return 0


def database_reachable():
    raw_hosts = read_facts().get("database_hosts", "")
    for host in [item.strip() for item in raw_hosts.split(",") if item.strip()]:
        address, _, port = host.rpartition(":")
        try:
            with socket.create_connection((address, int(port)), timeout=2):
                return 0
        except (OSError, ValueError):
            continue
    return 1


def install_wordpress():
    admin_password = read_facts()["wordpress_admin_password"]
    command = [
        "/usr/local/bin/wp", "core", "install",
        "--url=https://cms.fake-enterprise.com",
        "--title=Fake Enterprise CMS",
        "--admin_user=admin",
        "--admin_email=admin@fake-enterprise.com",
        "--skip-email", "--allow-root", "--path=/var/www/html",
        "--prompt=admin_password",
    ]
    result = subprocess.run(command, input=admin_password + "\n", text=True, check=False)
    return result.returncode


def main():
    action = sys.argv[1] if len(sys.argv) > 1 else ""
    if action == "configure-db":
        return configure_db()
    if action == "check-db":
        return configure_db(check_only=True)
    if action == "configure-db-name":
        return configure_db_identity("DB_NAME", read_facts()["database_name"])
    if action == "check-db-name":
        return configure_db_identity("DB_NAME", read_facts()["database_name"], check_only=True)
    if action == "configure-db-user":
        return configure_db_identity("DB_USER", read_facts()["database_user"])
    if action == "check-db-user":
        return configure_db_identity("DB_USER", read_facts()["database_user"], check_only=True)
    if action == "configure-db-host":
        return configure_db_host()
    if action == "check-db-host":
        return configure_db_host(check_only=True)
    if action == "db-reachable":
        return database_reachable()
    if action == "install":
        return install_wordpress()
    raise RuntimeError(
        "expected configure-db, check-db, configure-db-name, check-db-name, "
        "configure-db-user, check-db-user, configure-db-host, check-db-host, "
        "db-reachable, or install"
    )


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:  # pylint: disable=broad-exception-caught
        print(f"WordPress credential setup failed: {exc}", file=sys.stderr)
        sys.exit(1)
