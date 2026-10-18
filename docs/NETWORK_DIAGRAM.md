# Network Architecture Diagrams — CMS Infrastructure

This document contains the technical architecture diagrams for the CMS high-availability infrastructure, rendered with Mermaid.

---

## 1. Complete Network Topology

Diagram showing the physical and logical network segmentation, including all nodes, their IP addresses, and inter-network connections.

```mermaid
graph TB
    subgraph INTERNET["☁ Internet / WAN"]
        CLOUD["External Clients"]
    end

    subgraph FW["🔒 Router / Firewall — ufw-router"]
        ETH0["eth0<br/>DHCP (WAN)"]
        UFW_CORE["UFW + NAT + IP Forwarding<br/>DNAT :80/:443 → 192.168.20.100"]
        ETH1["eth1<br/>192.168.10.1"]
        ETH2["eth2<br/>192.168.20.1"]
        ETH0 --- UFW_CORE
        UFW_CORE --- ETH1
        UFW_CORE --- ETH2
    end

    subgraph INTERNAL["🟩 Internal Network — 192.168.10.0/24"]
        direction TB
        JS["🖥 jumpstart<br/>192.168.10.10<br/>Cobbler · Puppet Server · step-ca"]

        subgraph K3S_CLUSTER["K3s HA Cluster"]
            direction LR
            M1["🟢 internal-master1<br/>192.168.10.11<br/>K3s Server · DRBD Primary"]
            M2["🟢 internal-master2<br/>192.168.10.12<br/>K3s Server · DRBD Secondary"]
            M3["🟢 internal-master3<br/>192.168.10.16<br/>K3s Server · etcd quorum"]
            W1["🔵 internal-worker1<br/>192.168.10.13<br/>K3s Agent"]
            W2["🔵 internal-worker2<br/>192.168.10.14<br/>K3s Agent"]
        end

        ST["💾 internal-storage<br/>192.168.10.15<br/>Centralised Storage"]
        MON["📊 internal-monitor<br/>192.168.10.20<br/>Prometheus · Grafana · alert rules"]
    end

    subgraph MAIN["🟦 Client Network — 192.168.20.0/24"]
        direction TB
        VIP(("★ VIP · 192.168.20.100<br/>keepalived"))

        subgraph LB_POOL["Load Balancers (VRRP)"]
            LB1["⚖ main-lb1<br/>192.168.20.103<br/>Nginx Reverse Proxy"]
            LB2["⚖ main-lb2<br/>192.168.20.104<br/>Nginx Reverse Proxy"]
        end

        subgraph CMS_POOL["CMS Frontends"]
            CMS1["🌐 main-cms1<br/>192.168.20.101<br/>WordPress + Apache"]
            CMS2["🌐 main-cms2<br/>192.168.20.102<br/>WordPress + Apache"]
        end

        subgraph HOTDESKS["Hot-desk Workstations"]
            HD1["💻 main-hotdesk1<br/>192.168.20.201"]
            HD2["💻 main-hotdesk2<br/>192.168.20.202"]
            HD38["💻 main-hotdesk3 (up to 8)<br/>192.168.20.203"]
        end
    end

    %% --- WAN traffic ---
    CLOUD -->|"HTTP/S :80/:443"| ETH0
    ETH1 --> INTERNAL
    ETH2 --> MAIN

    %% --- Dual-homed jumpstart ---
    JS -.->|"192.168.20.10<br/>secondary interface"| MAIN

    %% --- K3s cluster mesh ---
    M1 <-->|"DRBD sync<br/>tcp/7788"| M2
    M1 ---|"K3s API :6443"| W1
    M1 ---|"K3s API :6443"| W2
    M2 ---|"K3s API :6443"| W1
    M2 ---|"K3s API :6443"| W2

    %% --- Storage ---
    ST ---|"shared data"| M1
    ST ---|"shared data"| M2

    %% --- Load balancing ---
    VIP ---|"VRRP"| LB1
    VIP ---|"VRRP"| LB2
    LB1 -->|"proxy_pass :80"| CMS1
    LB1 -->|"proxy_pass :80"| CMS2
    LB2 -->|"proxy_pass :80"| CMS1
    LB2 -->|"proxy_pass :80"| CMS2

    %% --- CMS to DB ---
    CMS1 -->|"tcp/3306"| M1
    CMS2 -->|"tcp/3306"| M1

    %% --- Monitoring scrape targets (all nodes) ---
    MON -.->|"scrape :9100"| JS
    MON -.->|"scrape :9100"| M1
    MON -.->|"scrape :9100"| M2
    MON -.->|"scrape :9100"| W1
    MON -.->|"scrape :9100"| W2
    MON -.->|"scrape :9100"| ST
    MON -.->|"scrape :9100/:9113/:9117"| LB1
    MON -.->|"scrape :9100/:9113/:9117"| LB2
    MON -.->|"scrape :9100/:9117"| CMS1
    MON -.->|"scrape :9100/:9117"| CMS2
    MON -.->|"scrape :9100"| ETH1
```

### Legend

| Symbol | Meaning |
|:-------|:--------|
| Solid line (`→`) | Data traffic / active connection |
| Dashed line (`-..->`) | Management / monitoring traffic |
| `🔒` | Router / firewall |
| `🖥` | Provisioning server (Cobbler · Puppet · step-ca) |
| `⚖` | Load balancer |
| `🟢` | K3s master node (DRBD) |
| `🔵` | K3s worker node |
| `💾` | Storage node |
| `📊` | Monitoring node (Prometheus · Grafana · alert rules) |
| `🌐` | CMS frontend (WordPress + Apache) |
| `💻` | Hot-desk workstation |
| `:9100` | node_exporter |
| `:9113` | nginx_exporter |
| `:9117` | apache_exporter |

---

## 2. Service Architecture

Diagram showing the traffic flow from external client to database, including the monitoring and provisioning layers.

```mermaid
graph LR
    subgraph CLIENT["👤 Client"]
        USER["Web Browser"]
    end

    subgraph PERIMETER["🔒 Perimeter"]
        ROUTER["ufw-router<br/>DNAT :80/:443 → LB<br/>NAT masquerade outbound"]
    end

    subgraph LB_LAYER["⚖ Load Balancing"]
        VIP2(("★ VIP<br/>192.168.20.100<br/>keepalived"))
        NGINX1["Nginx<br/>192.168.20.103<br/>main-lb1"]
        NGINX2["Nginx<br/>192.168.20.104<br/>main-lb2"]
    end

    subgraph WEB_LAYER["🌐 Web Layer"]
        APACHE1["Apache + WordPress<br/>192.168.20.101"]
        APACHE2["Apache + WordPress<br/>192.168.20.102"]
    end

    subgraph DB_LAYER["🗄 Data Layer — K3s Cluster"]
        MARIA["MariaDB 10.11<br/>K3s StatefulSet<br/>PV: /mnt/data/mariadb"]
        DRBD["DRBD 9<br/>Protocol C — Synchronous<br/>tcp/7788"]
    end

    subgraph STORAGE_LAYER["💾 Storage"]
        STORE["internal-storage<br/>192.168.10.15<br/>Centralised file storage"]
    end

    subgraph MONITOR["📊 Observability"]
        PROM["Prometheus<br/>192.168.10.20:9090"]
        ALERT["Alertmanager<br/>192.168.10.20:9093"]
        GRAF["Grafana<br/>192.168.10.20:3000"]
    end

    subgraph PROVISION["🖥 Provisioning & PKI"]
        COBBLER["Cobbler + Puppet<br/>192.168.10.10"]
        STEPCA["step-ca<br/>Internal PKI<br/>:8443"]
    end

    %% --- Request path ---
    USER -->|"HTTP/S"| ROUTER
    ROUTER -->|"DNAT"| VIP2
    VIP2 --> NGINX1
    VIP2 --> NGINX2
    NGINX1 -->|"upstream :80"| APACHE1
    NGINX1 -->|"upstream :80"| APACHE2
    NGINX2 -->|"upstream :80"| APACHE1
    NGINX2 -->|"upstream :80"| APACHE2
    APACHE1 -->|"tcp/3306"| MARIA
    APACHE2 -->|"tcp/3306"| MARIA
    MARIA ---|"block device"| DRBD

    %% --- Storage ---
    STORE ---|"shared data"| MARIA

    %% --- Monitoring ---
    PROM -.->|"scrape :9113"| NGINX1
    PROM -.->|"scrape :9113"| NGINX2
    PROM -.->|"scrape :9117"| APACHE1
    PROM -.->|"scrape :9117"| APACHE2
    PROM -.->|"scrape :9104"| MARIA
    PROM -->|"fires alerts"| ALERT
    GRAF -.->|"datasource"| PROM

    %% --- Provisioning ---
    COBBLER -.->|"PXE + Puppet"| NGINX1
    COBBLER -.->|"PXE + Puppet"| NGINX2
    COBBLER -.->|"PXE + Puppet"| APACHE1
    COBBLER -.->|"PXE + Puppet"| APACHE2
    STEPCA -.->|"issues/renews TLS cert"| NGINX1
    STEPCA -.->|"issues/renews TLS cert"| NGINX2
```

### HTTP Traffic Flow

1. The **client** sends an HTTP/S request to the router's public IP.
2. The **router (UFW)** applies DNAT to redirect traffic on ports 80/443 to the keepalived **VIP** `192.168.20.100`, which is held by one of the two Nginx balancers (`main-lb1`/`main-lb2`).
3. **Nginx** distributes the request between `main-cms1` and `main-cms2` using round-robin.
4. The **Apache + WordPress** frontend processes the PHP request and queries **MariaDB** in the K3s cluster.
5. **DRBD** maintains synchronous replication of MariaDB data between `internal-master1` and `internal-master2`.

### Monitoring Flow

- **Prometheus** (192.168.10.20:9090) performs periodic scraping (every 15s) of all exporters and evaluates 19 alert rules (availability, service, backup/restore and HA — DRBD, etcd quorum, keepalived VIP).
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
