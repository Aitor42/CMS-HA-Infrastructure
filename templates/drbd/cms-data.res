{{- $m1 := index .Nodes.Masters 0 -}}
{{- $m2 := index .Nodes.Masters 1 -}}
resource cms_data {
  protocol C;

  handlers {
    split-brain "/usr/lib/drbd/notify-split-brain.sh root";
  }

  net {
    # Validate integrity during synchronisation
    verify-alg sha256;
    connect-int 10;
    ping-int 10;
    ping-timeout 5;
    after-sb-0pri discard-zero-changes;
    after-sb-1pri discard-secondary;
    after-sb-2pri disconnect;
  }

  disk {
    # Detach local disk on physical I/O errors
    on-io-error detach;
    resync-rate 100M;
  }

  on {{ $m1.FQDN }} {{ $m1.Name }} {
    device /dev/drbd0;
    disk /dev/vdb;
    address {{ $m1.IP }}:7788;
    meta-disk internal;
  }

  on {{ $m2.FQDN }} {{ $m2.Name }} {
    device /dev/drbd0;
    disk /dev/vdb;
    address {{ $m2.IP }}:7788;
    meta-disk internal;
  }
}
