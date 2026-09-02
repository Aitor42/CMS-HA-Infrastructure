# ──────────────────────────────────────────────────────────────────────────────
# Makefile — CMS High-Availability Infrastructure (Go CLI)
# ──────────────────────────────────────────────────────────────────────────────
#
# Standardised targets for building, deploying, validating, and maintaining
# the infrastructure. Run `make help` to see all available commands.
#
# Prerequisites:
#   - Go 1.22+ installed
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

# ── Project paths ────────────────────────────────────────────────────────────
PROJECT_DIR   := $(shell pwd)
TERRAFORM_DIR := $(PROJECT_DIR)/terraform
K8S_DIR       := $(PROJECT_DIR)/kubernetes

# ── Configurable variables ───────────────────────────────────────────────────
CONFIG      ?= config.yaml

# ══════════════════════════════════════════════════════════════════════════════
# BUILD
# ══════════════════════════════════════════════════════════════════════════════

.PHONY: build
build: ## Build the cms-ha binary (linux/amd64)
	@echo "▶ Building $(BINARY)..."
	GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) ./cmd/cms-ha/
	@echo "✔ Built $(BUILD_DIR)/$(BINARY)"

.PHONY: build-check
build-check: ## Verify all packages compile
	@echo "▶ Checking compilation..."
	$(GO) build ./...
	@echo "✔ All packages compile successfully"

.PHONY: test
test: ## Run Go unit tests
	@echo "▶ Running tests..."
	$(GO) test -v -race ./...

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

.PHONY: deploy-dry-run
deploy-dry-run: build ## Dry-run deployment (no commands executed)
	@echo "▶ Dry-run deployment..."
	./$(BINARY) -c $(CONFIG) deploy --skip-vm-create --dry-run

.PHONY: deploy-terraform
deploy-terraform: build ## Deploy VMs via Terraform, then run service phases
	@echo "▶ Deploying infrastructure with Terraform..."
	cd $(TERRAFORM_DIR) && terraform init && terraform apply -auto-approve
	@echo "▶ Running service deployment phases..."
	./$(BINARY) -c $(CONFIG) deploy --skip-vm-create

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
lint: lint-go lint-shell lint-yaml lint-puppet lint-k8s ## Run all linters

.PHONY: lint-go
lint-go: ## Lint Go code (go vet + staticcheck)
	@echo "▶ Checking Go code..."
	$(GO) vet ./...
	@command -v staticcheck >/dev/null 2>&1 && staticcheck ./... || \
		echo "  ℹ staticcheck not installed — skipping"
	@echo "✔ Go code OK"

.PHONY: lint-shell
lint-shell: ## Lint shell scripts (ShellCheck + bash -n syntax)
	@echo "▶ Checking shell scripts..."
	@command -v shellcheck >/dev/null 2>&1 && \
		find . -name "*.sh" -not -path "./.git/*" -exec shellcheck -S warning {} + || \
		echo "  ℹ ShellCheck not installed — skipping"

.PHONY: lint-yaml
lint-yaml: ## Lint YAML manifests (yamllint)
	@echo "▶ Checking YAML files..."
	@command -v yamllint >/dev/null 2>&1 && \
		yamllint -c .yamllint $(K8S_DIR)/ templates/monitoring/ .github/workflows/ || \
		echo "  ℹ yamllint not installed — skipping"

.PHONY: lint-puppet
lint-puppet: ## Lint Puppet manifests (puppet-lint)
	@echo "▶ Checking Puppet manifests..."
	@command -v puppet-lint >/dev/null 2>&1 && \
		find puppet/ -name "*.pp" -exec puppet-lint \
			--no-class_inherits_from_params_class-check \
			--no-documentation-check \
			--no-autoloader_layout-check {} + || \
		echo "  ℹ puppet-lint not installed — skipping"

.PHONY: lint-k8s
lint-k8s: ## Validate Kubernetes manifests (kubeconform)
	@echo "▶ Validating Kubernetes manifests..."
	@command -v kubeconform >/dev/null 2>&1 && \
		kubeconform -strict -summary -kubernetes-version 1.29.0 $(K8S_DIR)/*.yaml || \
		echo "  ℹ kubeconform not installed — skipping"

.PHONY: lint-terraform
lint-terraform: ## Validate Terraform configuration
	@echo "▶ Validating Terraform..."
	cd $(TERRAFORM_DIR) && terraform fmt -check -recursive
	cd $(TERRAFORM_DIR) && terraform init -backend=false && terraform validate

# ══════════════════════════════════════════════════════════════════════════════
# MAINTENANCE & OPERATIONS
# ══════════════════════════════════════════════════════════════════════════════

.PHONY: start
start: build ## Start all VMs (resume paused / boot stopped)
	@echo "▶ Starting all VMs..."
	./$(BINARY) -c $(CONFIG) vm start

.PHONY: stop
stop: ## Gracefully shut down all VMs
	@echo "▶ Shutting down all VMs..."
	@for vm in $$(virsh list --name 2>/dev/null); do \
		echo "  Shutting down $$vm..."; \
		virsh shutdown "$$vm" 2>/dev/null || true; \
	done
	@echo "✔ Shutdown signals sent to all running VMs"

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

.PHONY: destroy
destroy: build ## Destroy all VMs and networks (cleanup)
	@echo "⚠ Destroying all infrastructure..."
	./$(BINARY) -c $(CONFIG) phase init-vms --cleanup

.PHONY: destroy-terraform
destroy-terraform: ## Destroy all Terraform-managed resources
	@echo "⚠ Destroying all Terraform infrastructure..."
	cd $(TERRAFORM_DIR) && terraform destroy

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
