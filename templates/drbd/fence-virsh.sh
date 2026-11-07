#!/bin/sh
# Invoked only by the fencing key's authorized_keys forced command.
# Match the whole SSH request, never evaluate or split untrusted shell input.
set -eu
PATH=/usr/bin:/bin
export PATH
case "${SSH_ORIGINAL_COMMAND-}" in
# {{range .}}
  'virsh destroy {{.}}') set -- destroy '{{.}}' ;;
  'virsh domstate {{.}}') set -- domstate '{{.}}' ;;
# {{end}}
  *) echo 'Denied: fencing permits only destroy/domstate of configured masters' >&2; exit 126 ;;
esac
# Do not inherit SSH-supplied libvirt, loader, shell or sudo environment settings.
exec /usr/bin/env -i PATH=/usr/bin:/bin LC_ALL=C /usr/bin/sudo -n /usr/bin/virsh "$1" "$2"
