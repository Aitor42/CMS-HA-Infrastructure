#!/usr/bin/env python3
# settings-patch.py
#
# Applies required Cobbler settings.yaml overrides for the CMS infrastructure.
# Run on the Jumpstart node with: python3 settings-patch.py

import yaml

path = "/etc/cobbler/settings.yaml"
with open(path) as f:
    cfg = yaml.safe_load(f) or {}

cfg["server"]                    = "192.168.10.10"
cfg.pop("next_server", None)
cfg["next_server_v4"]            = "192.168.10.10"
cfg["next_server_v6"]            = "::1"
cfg["manage_dhcp"]               = True
cfg["manage_dhcp_v4"]            = True
cfg["manage_dns"]                = True
cfg["manage_tftpd"]              = True
cfg["pxe_just_once"]             = True
cfg["always_write_dhcp_entries"] = True
cfg["bind_zonefile_path"]        = "/var/cache/bind"
cfg["default_password_crypted"]  = "$6$LOCKED$zQ8.e3KlMUuKr2VVoNm4c5UwkHb9XsGdpT1qFaEhWy6LjnrCDivPt0OIZmBx7QgAsk"

with open(path, "w") as f:
    yaml.dump(cfg, f, default_flow_style=False)

print("  [OK] settings.yaml updated successfully")
