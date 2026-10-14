#!/usr/bin/env python3
"""Apply local runtime credentials to WordPress without putting them in Puppet code."""

import json
import grp
import os
import re
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
    if updated == content:
        return 0
    fd, temp_path = tempfile.mkstemp(dir=os.path.dirname(WP_CONFIG), prefix=".wp-config-")
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            stream.write(updated)
        os.chmod(temp_path, 0o640)
        os.chown(temp_path, 0, grp.getgrnam("www-data").gr_gid)
        os.replace(temp_path, WP_CONFIG)
    finally:
        if os.path.exists(temp_path):
            os.unlink(temp_path)
    return 0


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
    if action == "install":
        return install_wordpress()
    raise RuntimeError("expected configure-db, check-db, or install")


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as exc:  # pylint: disable=broad-exception-caught
        print(f"WordPress credential setup failed: {exc}", file=sys.stderr)
        sys.exit(1)
