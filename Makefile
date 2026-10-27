# ──────────────────────────────────────────────────────────────────────────────
# Makefile — CMS High-Availability Infrastructure (Go CLI)
# ──────────────────────────────────────────────────────────────────────────────
#
# Standardised targets for building, deploying, validating, and maintaining
# the infrastructure. Run `make help` to see all available commands.
#
# Prerequisites:
#   - Go 1.26+ installed
#   - KVM/QEMU + libvirt installed on the host
#   - SSH key at ~/.ssh/id_ed25519_gar
#   - Ubuntu 24.04 Server ISO (for initial deployment only)

SHELL := /bin/bash
.DEFAULT_GOAL := help

# ── Build variables ──────────────────────────────────────────────────────────
BINARY      := cms-ha
GO          := go
GOFLAGS     := -trimpath
LDFLAGS     := -s -w -X main.version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOOS        := linux
GOARCH      := amd64
BUILD_DIR   := .
# The README promises a statically linked binary. Pure-Go builds avoid the
# libc/libresolv dependency the default CGO build introduces, at no size cost
# (measured: 12.19 MB dynamic vs 12.15 MB static). Override with
# `make build CGO_ENABLED=1` if a future dependency needs cgo.
CGO_ENABLED ?= 0

# Keep the compiler cache warm across build/vet/test/staticcheck: mixing flag
# sets invalidated it and turned a 0.35s cached build into an 8.7s rebuild.
export GOFLAGS

# ── Project paths ────────────────────────────────────────────────────────────
PROJECT_DIR   := $(shell pwd)
K8S_DIR       := $(PROJECT_DIR)/kubernetes
ANSIBLE_DIR   := $(PROJECT_DIR)/ansible

# ── Configurable variables ───────────────────────────────────────────────────
CONFIG      ?= config.yaml

# ══════════════════════════════════════════════════════════════════════════════
# BUILD
# ══════════════════════════════════════════════════════════════════════════════

.PHONY: build
build: ## Build the cms-ha binary (linux/amd64, static by default)
	@echo "▶ Building $(BINARY)..."
	GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=$(CGO_ENABLED) $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) ./cmd/cms-ha/
	@echo "✔ Built $(BUILD_DIR)/$(BINARY)"

.PHONY: build-check
build-check: ## Verify all packages compile
	@echo "▶ Checking compilation..."
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build ./...
	@echo "✔ All packages compile successfully"

.PHONY: test
test: ## Run Go unit tests
	@echo "▶ Running tests..."
	$(GO) test -race ./...

.PHONY: test-cover
test-cover: ## Run Go tests with cross-package coverage (coverage.out)
	@echo "▶ Running tests with coverage..."
	$(GO) test -coverpkg=./... -coverprofile=coverage.out ./...
	@$(GO) tool cover -func=coverage.out | tail -1

.PHONY: clean
clean: ## Remove build artifacts
	@echo "▶ Cleaning..."
	rm -f $(BUILD_DIR)/$(BINARY)
	$(GO) clean -cache -testcache
	@echo "✔ Clean"

# ══════════════════════════════════════════════════════════════════════════════
# DEPLOYMENT
# ══════════════════════════════════════════════════════════════════════════════

.PHONY: deploy
deploy: build ## Full deployment (PXE provisioning + all phases)
	@echo "▶ Starting full infrastructure deployment..."
	./$(BINARY) -c $(CONFIG) deploy

.PHONY: deploy-resume
deploy-resume: build ## Resume deployment with pre-installed VMs (phases 01-08)
	@echo "▶ Resuming deployment (skipping VM creation)..."
	./$(BINARY) -c $(CONFIG) deploy --skip-vm-create

.PHONY: deploy-fast
deploy-fast: build ## Full deploy from cloud images (opt-in, not the PXE gate)
	@echo "▶ Starting fast cloud-image deployment..."
	./$(BINARY) -c $(CONFIG) deploy --fast-provisioning

.PHONY: deploy-dry-run
deploy-dry-run: build ## Dry-run deployment (no commands executed)
	@echo "▶ Dry-run deployment..."
	./$(BINARY) -c $(CONFIG) deploy --skip-vm-create --dry-run

.PHONY: inventory
inventory: build ## Generate the Ansible inventory from config.yaml
	@echo "▶ Generating Ansible inventory..."
	./$(BINARY) -c $(CONFIG) inventory

.PHONY: provision
provision: inventory ## Create networks and VMs via Ansible (infra as code)
	@echo "▶ Provisioning infrastructure with Ansible..."
	cd $(ANSIBLE_DIR) && ansible-playbook playbooks/provision.yml

.PHONY: provision-check
provision-check: inventory ## Dry-run Ansible provisioning (no changes)
	@echo "▶ Dry-run Ansible provisioning..."
	cd $(ANSIBLE_DIR) && ansible-playbook playbooks/provision.yml --check --diff

# ══════════════════════════════════════════════════════════════════════════════
# PHASES (individual phase execution)
# ══════════════════════════════════════════════════════════════════════════════

.PHONY: phase-init
phase-init: build ## Phase 00: Initialize VMs
	./$(BINARY) -c $(CONFIG) phase init-vms

.PHONY: phase-cobbler
phase-cobbler: build ## Phase 01: Setup Cobbler
	./$(BINARY) -c $(CONFIG) phase setup-cobbler

.PHONY: phase-register
phase-register: build ## Phase 02: Register nodes in Cobbler
	./$(BINARY) -c $(CONFIG) phase register-nodes

.PHONY: phase-ssh
phase-ssh: build ## Phase 03: Repair SSH and Puppet
	./$(BINARY) -c $(CONFIG) phase repair-ssh

.PHONY: phase-puppet
phase-puppet: build ## Phase 04: Setup Puppet
	./$(BINARY) -c $(CONFIG) phase setup-puppet

.PHONY: phase-drbd
phase-drbd: build ## Phase 05: Setup DRBD
	./$(BINARY) -c $(CONFIG) phase setup-drbd

.PHONY: phase-k8s
phase-k8s: build ## Phase 06: Setup Kubernetes
	./$(BINARY) -c $(CONFIG) phase setup-kubernetes

.PHONY: phase-nginx
phase-nginx: build ## Phase 07: Setup Nginx + WordPress
	./$(BINARY) -c $(CONFIG) phase setup-nginx-wordpress

.PHONY: phase-monitoring
phase-monitoring: build ## Phase 08: Setup Monitoring
	./$(BINARY) -c $(CONFIG) phase setup-monitoring

.PHONY: phase-ufw
phase-ufw: build ## Phase 09: Setup UFW
	./$(BINARY) -c $(CONFIG) phase setup-ufw

.PHONY: phase-ca
phase-ca: build ## Phase 10: Setup Internal CA
	./$(BINARY) -c $(CONFIG) phase setup-ca

# ══════════════════════════════════════════════════════════════════════════════
# VERIFICATION & TESTING
# ══════════════════════════════════════════════════════════════════════════════

.PHONY: verify
verify: build ## Run full infrastructure health check
	@echo "▶ Running infrastructure verification..."
	./$(BINARY) -c $(CONFIG) verify

.PHONY: test-failover
test-failover: build ## Run automated chaos engineering / failover tests
	@echo "▶ Running failover tests..."
	./$(BINARY) -c $(CONFIG) test failover

.PHONY: test-failover-norestore
test-failover-norestore: build ## Run failover tests without restoring
	@echo "▶ Running failover tests (skip-restore mode)..."
	./$(BINARY) -c $(CONFIG) test failover --skip-restore

.PHONY: test-drills
test-drills: build ## Run the operational drills (backup/restore, TLS renewal, alert hygiene)
	@echo "▶ Running operational drills..."
	./$(BINARY) -c $(CONFIG) test drills

.PHONY: integration
integration: build ## End-to-end integration test on a real KVM host (deploy + verify + failover)
	@echo "▶ Running end-to-end integration test..."
	tests/integration/run.sh

.PHONY: integration-traffic
integration-traffic: build ## Integration test including the k6 load profile (TRAFFIC_DURATION=45 TRAFFIC_VUS=3)
	@echo "▶ Running end-to-end integration test with the k6 load profile..."
	tests/integration/run.sh --with-traffic

.PHONY: integration-drills
integration-drills: build ## Integration test including traffic and the operational drills
	@echo "▶ Running end-to-end integration test with traffic and drills..."
	tests/integration/run.sh --with-traffic --with-drills

.PHONY: integration-resume
integration-resume: build ## Resume the end-to-end integration test from the deploy checkpoint
	@echo "▶ Resuming end-to-end integration test..."
	tests/integration/run.sh --resume

.PHONY: traffic
traffic: build ## Run traffic mix test
	@echo "▶ Running traffic generation..."
	./$(BINARY) -c $(CONFIG) traffic --internal

.PHONY: traffic-external
traffic-external: build ## Run external traffic test
	./$(BINARY) -c $(CONFIG) traffic --external

# ══════════════════════════════════════════════════════════════════════════════
# LINTING & VALIDATION
# ══════════════════════════════════════════════════════════════════════════════

.PHONY: lint
lint: lint-go lint-shell lint-yaml lint-actions lint-puppet lint-k8s lint-ansible ## Run all linters

.PHONY: lint-go
lint-go: ## Lint Go code (go vet + staticcheck)
	@echo "▶ Checking Go code..."
	$(GO) vet ./...
	@if command -v staticcheck >/dev/null 2>&1; then \
		staticcheck ./...; \
	else \
		echo "  ℹ staticcheck not installed — skipping"; \
	fi
	@echo "✔ Go code OK"

.PHONY: lint-shell
lint-shell: ## Lint shell scripts (ShellCheck + bash -n syntax)
	@echo "▶ Checking shell scripts..."
	@if command -v shellcheck >/dev/null 2>&1; then \
		find . -name "*.sh" -not -path "./.git/*" -exec shellcheck -S warning -e SC2029,SC2087,SC2034,SC2155,SC2054,SC2046 {} +; \
	else \
		echo "  ℹ ShellCheck not installed — skipping"; \
	fi

.PHONY: lint-actions
lint-actions: ## Lint GitHub Actions workflows (actionlint)
	@echo "▶ Checking GitHub Actions workflows..."
	@if command -v actionlint >/dev/null 2>&1; then \
		actionlint; \
	else \
		echo "  ℹ actionlint not installed — skipping"; \
	fi

.PHONY: lint-yaml
lint-yaml: ## Lint YAML manifests (yamllint)
	@echo "▶ Checking YAML files..."
	@if command -v yamllint >/dev/null 2>&1; then \
		yamllint -c .yamllint $(K8S_DIR)/ templates/monitoring/ .github/workflows/; \
	else \
		echo "  ℹ yamllint not installed — skipping"; \
	fi

.PHONY: lint-puppet
lint-puppet: ## Lint Puppet manifests (puppet-lint)
	@echo "▶ Checking Puppet manifests..."
	@if command -v puppet-lint >/dev/null 2>&1; then \
		find puppet/ -name "*.pp" -exec puppet-lint \
			--no-class_inherits_from_params_class-check \
			--no-documentation-check \
			--no-autoloader_layout-check \
			--no-140chars-check {} +; \
	else \
		echo "  ℹ puppet-lint not installed — skipping"; \
	fi

.PHONY: lint-k8s
lint-k8s: ## Validate Kubernetes manifests (kustomize render + kubeconform)
	@echo "▶ Validating Kubernetes manifests..."
	@if command -v kustomize >/dev/null 2>&1; then \
		kustomize build $(K8S_DIR)/overlays/lab >/dev/null && \
		kustomize build $(K8S_DIR)/backups >/dev/null && \
		kustomize build $(K8S_DIR)/jobs >/dev/null && \
		echo "  ✓ kustomize roots render"; \
	else \
		echo "  ℹ kustomize not installed — skipping render check"; \
	fi
	@if command -v kubeconform >/dev/null 2>&1; then \
		kubeconform -strict -summary -skip Kustomization -ignore-filename-pattern '\.patch\.yaml$$' -kubernetes-version 1.29.0 $(K8S_DIR); \
	else \
		echo "  ℹ kubeconform not installed — skipping"; \
	fi

.PHONY: security
security: ## Security scan: gosec (Go SAST) + govulncheck (deps) + Trivy (fs)
	@echo "▶ Security scan..."
	@if command -v gosec >/dev/null 2>&1; then \
		gosec -severity high -confidence medium -exclude-dir=tests ./... && \
		echo "  ✓ gosec: no HIGH findings"; \
	else \
		echo "  ℹ gosec not installed — skipping (go install github.com/securego/gosec/v2/cmd/gosec@latest)"; \
	fi
	@if command -v govulncheck >/dev/null 2>&1; then \
		govulncheck ./...; \
	else \
		echo "  ℹ govulncheck not installed — skipping (go install golang.org/x/vuln/cmd/govulncheck@latest)"; \
	fi
	@if command -v trivy >/dev/null 2>&1; then \
		echo "  · dependencies & secrets (blocking on HIGH/CRITICAL)"; \
		trivy fs --scanners vuln,secret --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 .; \
		echo "  · Kubernetes misconfig (report only)"; \
		trivy fs --scanners misconfig --severity HIGH,CRITICAL --exit-code 0 .; \
	else \
		echo "  ℹ trivy not installed — skipping"; \
	fi

# ══════════════════════════════════════════════════════════════════════════════
.PHONY: lint-ansible
lint-ansible: ## Lint Ansible playbooks and roles (ansible-lint + syntax-check)
	@echo "▶ Checking Ansible code..."
	@if command -v ansible-lint >/dev/null 2>&1; then \
		ANSIBLE_CONFIG=$(ANSIBLE_DIR)/ansible.cfg ansible-lint $(ANSIBLE_DIR); \
	else \
		echo "  ℹ ansible-lint not installed — skipping"; \
	fi
	@if command -v ansible-playbook >/dev/null 2>&1; then \
		ANSIBLE_CONFIG=$(ANSIBLE_DIR)/ansible.cfg ansible-playbook --syntax-check \
			-i $(ANSIBLE_DIR)/inventory/hosts.yml \
			$(ANSIBLE_DIR)/playbooks/provision.yml; \
	else \
		echo "  ℹ ansible-playbook not installed — skipping"; \
	fi

# ══════════════════════════════════════════════════════════════════════════════
# MAINTENANCE & OPERATIONS
# ══════════════════════════════════════════════════════════════════════════════

.PHONY: start
start: build ## Start all VMs (resume paused / boot stopped)
	@echo "▶ Starting all VMs..."
	./$(BINARY) -c $(CONFIG) vm start

.PHONY: stop
stop: build ## Gracefully shut down all VMs
	@echo "▶ Shutting down all VMs..."
	./$(BINARY) -c $(CONFIG) vm stop

.PHONY: shrink
shrink: build ## Resize all VMs to production RAM sizing
	@echo "▶ Shrinking VM RAM..."
	./$(BINARY) -c $(CONFIG) vm shrink

.PHONY: repair
repair: build ## Repair K3s cluster after pause/shutdown
	@echo "▶ Repairing K3s cluster..."
	./$(BINARY) -c $(CONFIG) repair k8s

.PHONY: sync-clocks
sync-clocks: build ## Synchronise clocks on all nodes
	@echo "▶ Synchronising clocks..."
	./$(BINARY) -c $(CONFIG) repair clocks

.PHONY: backup-db
backup-db: build ## Trigger a manual MariaDB backup
	@echo "▶ Triggering MariaDB backup..."
	./$(BINARY) -c $(CONFIG) backup db

.PHONY: status-ssh
status-ssh: build ## Check SSH connectivity to all VMs
	./$(BINARY) -c $(CONFIG) status ssh

# ══════════════════════════════════════════════════════════════════════════════
# SECRETS MANAGEMENT
# ══════════════════════════════════════════════════════════════════════════════

.PHONY: secrets-generate
secrets-generate: build ## Generate a new age encryption key pair
	./$(BINARY) -c $(CONFIG) secrets generate-key

.PHONY: secrets-encrypt
secrets-encrypt: build ## Encrypt sensitive fields in config.yaml
	./$(BINARY) -c $(CONFIG) secrets encrypt

.PHONY: secrets-decrypt
secrets-decrypt: build ## Decrypt and display config
	./$(BINARY) -c $(CONFIG) secrets decrypt

# ══════════════════════════════════════════════════════════════════════════════
# CLEANUP
# ══════════════════════════════════════════════════════════════════════════════

.PHONY: cleanup
cleanup: build ## Destroy all VMs, disks, and virtual networks
	@echo "⚠ Destroying all infrastructure..."
	./$(BINARY) -c $(CONFIG) cleanup

.PHONY: destroy
destroy: cleanup ## Alias for cleanup

# ══════════════════════════════════════════════════════════════════════════════
# HELP
# ══════════════════════════════════════════════════════════════════════════════

.PHONY: help
help: ## Show this help message
	@echo ""
	@echo "CMS High-Availability Infrastructure — Make Targets"
	@echo "════════════════════════════════════════════════════"
	@echo ""
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-24s\033[0m %s\n", $$1, $$2}'
	@echo ""
