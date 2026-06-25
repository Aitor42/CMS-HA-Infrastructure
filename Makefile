# ──────────────────────────────────────────────────────────────────────────────
# Makefile — CMS High-Availability Infrastructure
# ──────────────────────────────────────────────────────────────────────────────
#
# Standardised targets for deploying, validating, and maintaining the
# infrastructure. Run `make help` to see all available commands.
#
# Prerequisites:
#   - KVM/QEMU + libvirt installed on the host
#   - SSH key at ~/.ssh/id_ed25519_gar
#   - Ubuntu 24.04 Server ISO (for initial deployment only)

SHELL := /bin/bash
.DEFAULT_GOAL := help

# ── Project paths ─────────────────────────────────────────────────────────────
PROJECT_DIR   := $(shell pwd)
SCRIPTS_DIR   := $(PROJECT_DIR)/scripts
UTILS_DIR     := $(SCRIPTS_DIR)/utils
TERRAFORM_DIR := $(PROJECT_DIR)/terraform
K8S_DIR       := $(PROJECT_DIR)/kubernetes

# ── Configurable variables ────────────────────────────────────────────────────
export VM_DIR              ?= $(HOME)/vm_storage
export LIBVIRT_DEFAULT_URI ?= qemu:///system

# ══════════════════════════════════════════════════════════════════════════════
# DEPLOYMENT
# ══════════════════════════════════════════════════════════════════════════════

.PHONY: deploy
deploy: ## Full deployment (PXE provisioning + all phases)
	@echo "▶ Starting full infrastructure deployment..."
	$(PROJECT_DIR)/deploy_all.sh

.PHONY: deploy-resume
deploy-resume: ## Resume deployment with pre-installed VMs (phases 01-08)
	@echo "▶ Resuming deployment (skipping VM creation)..."
	$(PROJECT_DIR)/deploy_all.sh --skip-vm-create

.PHONY: deploy-dry-run
deploy-dry-run: ## Dry-run deployment (no commands executed)
	@echo "▶ Dry-run deployment..."
	DRY_RUN=1 $(PROJECT_DIR)/deploy_all.sh --skip-vm-create

.PHONY: deploy-terraform
deploy-terraform: ## Deploy VMs via Terraform, then run service phases
	@echo "▶ Deploying infrastructure with Terraform..."
	cd $(TERRAFORM_DIR) && terraform init && terraform apply -auto-approve -var="vm_storage_path=$(VM_DIR)"
	@echo "▶ Running service deployment phases..."
	$(PROJECT_DIR)/deploy_all.sh --skip-vm-create

# ══════════════════════════════════════════════════════════════════════════════
# VERIFICATION & TESTING
# ══════════════════════════════════════════════════════════════════════════════

.PHONY: verify
verify: ## Run full infrastructure health check
	@echo "▶ Running infrastructure verification..."
	bash $(UTILS_DIR)/verify_all.sh

.PHONY: test-failover
test-failover: ## Run automated chaos engineering / failover tests
	@echo "▶ Running failover tests..."
	bash $(UTILS_DIR)/test_failover.sh

.PHONY: test-failover-norestore
test-failover-norestore: ## Run failover tests without restoring (for inspection)
	@echo "▶ Running failover tests (skip-restore mode)..."
	bash $(UTILS_DIR)/test_failover.sh --skip-restore

# ══════════════════════════════════════════════════════════════════════════════
# LINTING & VALIDATION (CI-equivalent targets)
# ══════════════════════════════════════════════════════════════════════════════

.PHONY: lint
lint: lint-shell lint-yaml lint-puppet lint-python lint-k8s ## Run all linters

.PHONY: lint-shell
lint-shell: ## Lint shell scripts (ShellCheck + bash -n syntax)
	@echo "▶ Checking shell scripts..."
	@ERRORS=0; \
	while IFS= read -r script; do \
		if ! bash -n "$$script" 2>&1; then \
			echo "  ✗ SYNTAX ERROR: $$script"; \
			ERRORS=$$((ERRORS + 1)); \
		fi; \
	done < <(find . -name "*.sh" -not -path "./.git/*"); \
	if [ "$$ERRORS" -gt 0 ]; then \
		echo "✗ $$ERRORS script(s) have syntax errors"; exit 1; \
	fi; \
	echo "✔ All shell scripts pass syntax check"
	@command -v shellcheck >/dev/null 2>&1 && \
		find . -name "*.sh" -not -path "./.git/*" -exec shellcheck -S warning {} + || \
		echo "  ℹ ShellCheck not installed — skipping static analysis"

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

.PHONY: lint-python
lint-python: ## Lint Python scripts (flake8)
	@echo "▶ Checking Python scripts..."
	@command -v flake8 >/dev/null 2>&1 && \
		flake8 . --count --select=E9,F63,F7,F82 --show-source --statistics || \
		echo "  ℹ flake8 not installed — skipping"

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
start: ## Start all VMs (resume paused / boot stopped)
	@echo "▶ Starting all VMs..."
	bash $(SCRIPTS_DIR)/start_all_vms.sh

.PHONY: stop
stop: ## Gracefully shut down all VMs
	@echo "▶ Shutting down all VMs..."
	@for vm in $$(virsh -c $(LIBVIRT_DEFAULT_URI) list --name 2>/dev/null); do \
		echo "  Shutting down $$vm..."; \
		virsh -c $(LIBVIRT_DEFAULT_URI) shutdown "$$vm" 2>/dev/null || true; \
	done
	@echo "✔ Shutdown signals sent to all running VMs"

.PHONY: repair
repair: ## Repair K3s cluster after pause/shutdown
	@echo "▶ Repairing K3s cluster..."
	bash $(UTILS_DIR)/repair_paused_kubernetes.sh

.PHONY: sync-clocks
sync-clocks: ## Synchronise clocks on all nodes
	@echo "▶ Synchronising clocks..."
	bash $(UTILS_DIR)/sync_vm_clocks.sh

.PHONY: backup-db
backup-db: ## Trigger a manual MariaDB backup
	@echo "▶ Triggering MariaDB backup..."
	@source $(SCRIPTS_DIR)/config.sh && \
	ssh $${SSH_OPTS} root@$${MASTER1_IP} \
		'kubectl create job --from=cronjob/mariadb-backup manual-backup-$(shell date +%s) -n cms' && \
	echo "✔ Backup job created. Check status: kubectl get jobs -n cms"

.PHONY: status
status: ## Show infrastructure status summary
	@echo ""
	@echo "═══════════════════════════════════════════════════════════"
	@echo " CMS HA Infrastructure — Status"
	@echo "═══════════════════════════════════════════════════════════"
	@echo ""
	@echo "VMs:"
	@virsh -c $(LIBVIRT_DEFAULT_URI) list --all 2>/dev/null | tail -n +3 || echo "  (libvirt not available)"
	@echo ""
	@source $(SCRIPTS_DIR)/config.sh 2>/dev/null && \
	echo "K3s Cluster:" && \
	ssh $${SSH_OPTS} root@$${MASTER1_IP} 'kubectl get nodes 2>/dev/null' 2>/dev/null || echo "  (cluster not reachable)"
	@echo ""

# ══════════════════════════════════════════════════════════════════════════════
# CLEANUP
# ══════════════════════════════════════════════════════════════════════════════

.PHONY: destroy-terraform
destroy-terraform: ## Destroy all Terraform-managed resources
	@echo "⚠ Destroying all Terraform infrastructure..."
	cd $(TERRAFORM_DIR) && terraform destroy -var="vm_storage_path=$(VM_DIR)"

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
