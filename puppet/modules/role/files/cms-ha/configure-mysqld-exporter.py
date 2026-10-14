#!/usr/bin/env python3
"""Write the mysqld exporter client config from the protected runtime fact."""

import json
import grp
import os
import sys
import tempfile

FACTS = "/etc/cms-ha/runtime-secrets.json"
CONFIG = "/etc/mysql/exporter.cnf"


def expected_content():
    with open(FACTS, encoding="utf-8") as stream:
        password = json.load(stream)["cms_ha"]["database_exporter_password"]
    return f"[client]\nuser=exporter\npassword={password}\nhost=127.0.0.1\nport=30306\n"


def main():
    expected = expected_content()
    if len(sys.argv) > 1 and sys.argv[1] == "check":
        try:
            with open(CONFIG, encoding="utf-8") as stream:
                return 0 if stream.read() == expected else 1
        except FileNotFoundError:
            return 1

    fd, temp_path = tempfile.mkstemp(dir="/etc/mysql", prefix=".exporter-")
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            stream.write(expected)
        os.chmod(temp_path, 0o640)
        os.chown(temp_path, 0, grp.getgrnam("prometheus").gr_gid)
        os.replace(temp_path, CONFIG)
    finally:
        if os.path.exists(temp_path):
            os.unlink(temp_path)
    return 0


if __name__ == "__main__":
    sys.exit(main())
