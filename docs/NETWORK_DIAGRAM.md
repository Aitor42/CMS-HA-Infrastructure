# Network Architecture Diagrams — CMS Infrastructure

This document contains the technical architecture diagrams for the CMS high-availability infrastructure, rendered with Mermaid.

---

## 1. Complete Network Topology

Physical and logical network segmentation: the two subnets, the router between them, every node with its address, and the jumpstart's second interface. The service flows between nodes are kept out of this diagram (they crossed each other) and are shown separately in [section 2](#2-service-architecture).

```mermaid
graph TB
    WAN["☁ WAN — external clients"]

    subgraph ROUTER["🔒 ufw-router — Router / Firewall"]
        direction TB
        R_WAN["eth0 · DHCP (WAN)"]
        R_FW["UFW · NAT + IP forwarding<br/>DNAT :80/:443 → VIP 192.168.20.100"]
        R_INT["eth1 · 192.168.10.1"]
        R_MAIN["eth2 · 192.168.20.1"]
        R_WAN --- R_FW --- R_INT
        R_FW --- R_MAIN
    end

    subgraph INTERNAL["🟩 internal · 192.168.10.0/24 · virbr-int"]
        direction TB
        JS["🖥 jumpstart · .10<br/>Cobbler PXE/DHCP/DNS · Puppet · step-ca"]

        subgraph K3S["K3s HA cluster (embedded etcd)"]
            M1["🟢 internal-master1 · .11<br/>K3s server · DRBD primary"]
            M2["🟢 internal-master2 · .12<br/>K3s server · DRBD secondary"]
            M3["🟢 internal-master3 · .16<br/>K3s server · etcd quorum"]
            W1["🔵 internal-worker1 · .13"]
            W2["🔵 internal-worker2 · .14"]
            M1 ~~~ M2 ~~~ M3
            W1 ~~~ W2
        end

        ST["💾 internal-storage · .15<br/>NFS shared uploads"]
        MON["📊 internal-monitor · .20<br/>Prometheus · Grafana · Alertmanager"]
    end

    subgraph MAIN["🟦 main · 192.168.20.0/24 · virbr-main"]
        direction TB
        VIP(("★ VIP · 192.168.20.100<br/>keepalived"))
        subgraph LBS["⚖ Load balancers"]
            LB1["main-lb1 · .103<br/>Nginx"]
            LB2["main-lb2 · .104<br/>Nginx"]
            LB1 ~~~ LB2
        end
        subgraph FES["🌐 CMS frontends"]
            CMS1["main-cms1 · .101<br/>WordPress + Apache"]
            CMS2["main-cms2 · .102<br/>WordPress + Apache"]
            CMS1 ~~~ CMS2
        end
        HD["💻 main-hotdesk1..3 · .201-.203<br/>(up to 8)"]
    end

    WAN -->|"HTTP/S :80/:443"| R_WAN
    R_INT -->|"gateway"| INTERNAL
    R_MAIN -->|"gateway"| MAIN
    JS -.->|"2nd NIC · 192.168.20.10"| MAIN
```

### Legend

| Symbol | Meaning |
|:-------|:--------|
| Solid line (`→` / `—`) | Subnet attachment / physical link |
| Dotted line (`-.->`) | Secondary (dual-homed) interface |
| Subnet label | `internal` / `main` — the network names used by `config.yaml` (`virbr-int` / `virbr-main`) |
| `🔒` | Router / firewall |
| `🖥` | Provisioning server (Cobbler · Puppet · step-ca) |
| `⚖` | Load balancer |
| `🟢` | K3s master node (DRBD) |
| `🔵` | K3s worker node |
| `💾` | Storage node (NFS server) |
| `📊` | Monitoring node (Prometheus · Grafana · alert rules) |
| `🌐` | CMS frontend (WordPress + Apache) |
| `💻` | Hot-desk workstation |

The `~~~` (invisible) links inside the cluster, load-balancer and frontend groups only keep each group laid out on one row; they carry no meaning.

---

## 2. Service Architecture

End-to-end traffic flow, from the external client to the database, plus the storage, observability and provisioning planes. Each balancer/frontend pair is collapsed into a single box (the individual nodes and addresses are in section 1).

```mermaid
graph LR
    subgraph ACCESS["👤 Client"]
        USER["Web browser"]
    end

    subgraph PERIMETER["🔒 Perimeter — ufw-router"]
        ROUTER["DNAT :80/:443 → VIP 192.168.20.100<br/>NAT masquerade outbound"]
    end

    subgraph LB_LAYER["⚖ Load balancing — main"]
        VIP(("★ VIP · 192.168.20.100<br/>keepalived · VRRP"))
        NGINX["Nginx reverse proxy<br/>main-lb1 · main-lb2"]
    end

    subgraph WEB_LAYER["🌐 Web layer — main"]
        APACHE["Apache + WordPress<br/>main-cms1 · main-cms2"]
    end

    subgraph DATA_LAYER["🗄 Data layer — internal"]
        MARIA["MariaDB 10.11 · K3s StatefulSet<br/>NodePort 30306 → .11 / .12"]
        DRBD["DRBD 9 · Protocol C<br/>tcp/7788 · master1 ⇄ master2"]
    end

    subgraph STORAGE_LAYER["💾 NFS uploads — internal"]
        STORE["internal-storage · .15<br/>/srv/nfs/wp-uploads"]
    end

    subgraph OBSERVE["📊 Observability — internal"]
        PROM["Prometheus · Grafana · Alertmanager<br/>internal-monitor · .20"]
    end

    subgraph PROVISION["🖥 Provisioning & PKI — internal"]
        JS["jumpstart · .10<br/>Cobbler PXE · Puppet · step-ca"]
    end

    USER -->|"HTTP/S"| ROUTER
    ROUTER -->|"DNAT"| VIP
    VIP -->|"VRRP"| NGINX
    NGINX -->|"upstream :80"| APACHE
    APACHE -->|"tcp/3306"| MARIA
    MARIA ---|"block device"| DRBD
    APACHE -.->|"mount"| STORE
    PROM -.->|"scrape"| APACHE
    JS -.->|"PXE + Puppet"| APACHE
```

### HTTP Traffic Flow

1. The **client** sends an HTTP/S request to the router's public IP.
2. The **router (UFW)** applies DNAT to redirect traffic on ports 80/443 to the keepalived **VIP** `192.168.20.100`, which is held by one of the two Nginx balancers (`main-lb1`/`main-lb2`).
3. **Nginx** distributes the request between `main-cms1` and `main-cms2` using round-robin.
4. The **Apache + WordPress** frontend processes the PHP request and queries **MariaDB** through the K3s **NodePort `30306`** on `internal-master1`/`internal-master2` (the frontend tries `.11` first and falls back to `.12`).
5. **DRBD** maintains synchronous replication of MariaDB data between `internal-master1` and `internal-master2`.
6. WordPress uploads live on the shared **NFS export** `internal-storage:/srv/nfs/wp-uploads`, mounted by both frontends, so either one can serve any request.

### Monitoring Flow

- **Prometheus** (192.168.10.20:9090) performs periodic scraping (every 15s) of all exporters and evaluates 19 alert rules (availability, service, backup/restore and HA — DRBD, etcd quorum, keepalived VIP). It scrapes `:9100` (node_exporter) on every node, `:9113` (nginx) on the balancers, `:9117` (Apache) on the frontends and `:9104` (mysqld) on the database.
- **Grafana** (192.168.10.20:3000) consumes Prometheus as a datasource and renders operational dashboards (node overview and HA overview).
- **Alertmanager** (192.168.10.20:9093) receives firing alerts from Prometheus and groups/deduplicates them for its UI and `amtool`; it ships with a no-op receiver, so add a webhook/email/Slack receiver to enable delivery.

---

## 3. Deployment Sequence

Diagram showing the execution order of deployment phases when running the solution.

```mermaid
graph TD
    START(["▶ Start Deployment<br/>cms-ha deploy"])

    subgraph VM_PROVISION["🖥 VM Provisioning"]
        STEP00A["Step 00a · Create networks + Jumpstart VM<br/>cms-ha phase init-vms --jumpstart-only"]
        WAIT_JS["⏳ Wait for Jumpstart SSH"]
        STEP01["Step 01 · Install Cobbler on Jumpstart<br/>cms-ha phase setup-cobbler"]
        STEP015["Step 01.5 · Register all nodes in Cobbler<br/>cms-ha phase register-nodes"]
        STEP00B["Step 00b · Batch PXE install all client VMs<br/>cms-ha vm install-batches"]
        WAIT_NODES["⏳ Wait for all VMs SSH"]
        STEP018["Step 01.8 · Repair SSH keys + Puppet CA<br/>cms-ha phase repair-ssh"]
    end

    subgraph CONFIG_MGMT["⚙ Configuration Management"]
        STEP02["Step 02 · Puppet Server + Agent convergence<br/>cms-ha phase setup-puppet<br/>(includes hot-desk provisioning)"]
    end

    subgraph HA_INFRA["🔧 HA Infrastructure"]
        STEP03["Step 03 · DRBD HA block replication<br/>cms-ha phase setup-drbd"]
        STEP035["Step 03.5 · Off-DRBD backups (Garage S3)<br/>cms-ha phase setup-backup"]
        STEP04["Step 04 · K3s HA cluster + MariaDB StatefulSet<br/>cms-ha phase setup-kubernetes"]
        STEP05["Step 05 · Nginx LB + Apache WordPress frontends<br/>cms-ha phase setup-nginx-wordpress"]
    end

    subgraph OBSERVE["📊 Observability"]
        STEP06["Step 06 · Prometheus + Grafana + alert rules<br/>cms-ha phase setup-monitoring"]
    end

    subgraph SECURITY["🔒 Security"]
        STEP07["Step 07 · UFW perimeter + per-node firewall<br/>cms-ha phase setup-ufw"]
        STEP08["Step 08 · Internal CA (step-ca) + TLS certs<br/>cms-ha phase setup-ca"]
    end

    TRAFFIC["Step 09 · Traffic mix + load testing<br/>cms-ha traffic"]
    END(["✅ Deployment Complete"])

    START --> STEP00A
    STEP00A --> WAIT_JS
    WAIT_JS --> STEP01
    STEP01 --> STEP015
    STEP015 --> STEP00B
    STEP00B --> WAIT_NODES
    WAIT_NODES --> STEP018
    STEP018 --> STEP02
    STEP02 --> STEP03
    STEP03 --> STEP035
    STEP035 --> STEP04
    STEP04 --> STEP05
    STEP05 --> STEP06
    STEP06 --> STEP07
    STEP07 --> STEP08
    STEP08 --> END
    END -.->|"optional"| TRAFFIC
```

### Deployment Sequence Notes

- `cms-ha deploy` runs this whole sequence in one shot; the individual `cms-ha phase …` commands shown above are the equivalent for re-running a single phase.
- The initial deployment requires launching the **Jumpstart node (Phase 00a)** first, as it serves as the provisioning server for all other clients.
- **Phase 00b** is performed **sequentially in batches** (`cms-ha vm install-batches`) to prevent the host's physical memory (27 GB) from being exhausted by the OS installers' initial requirements.
- The `cms-ha deploy --skip-vm-create` command orchestrates all phases from **Phase 01.8** onwards, once all client VMs are installed and running with optimised RAM.
- Configuration phases (Puppet, DRBD, K3s, Nginx, UFW, Monitoring, and Internal CA) are **strictly sequential** due to mutual service dependencies.
