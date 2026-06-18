resource cms_data {
  protocol C;

  net {
    # Validate integrity during synchronisation
    verify-alg sha256;
    connect-int 10;
    ping-int 10;
    ping-timeout 5;
  }

  disk {
    # Detach local disk on physical I/O errors
    on-io-error detach;
    resync-rate 100M;
  }

  on internal-master1 {
    device /dev/drbd0;
    disk /dev/vdb;
    address 192.168.10.11:7788;
    meta-disk internal;
  }

  on internal-master2 {
    device /dev/drbd0;
    disk /dev/vdb;
    address 192.168.10.12:7788;
    meta-disk internal;
  }
}
