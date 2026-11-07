package puppet

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/shell"
)

// catalogConcurrency resolves the first-catalog concurrency: the
// PUPPET_CONCURRENCY environment variable wins, then deploy.puppet_concurrency,
// and finally an automatic value derived from the host's CPU count (4 to 8).
// The measured run at concurrency 6 took 376s for 1,953s of per-node work
// (floor 325.5s), so ordering controls the tail but cannot beat the floor.
func (p *Phase) catalogConcurrency() (int, string) {
	if envC := strings.TrimSpace(os.Getenv("PUPPET_CONCURRENCY")); envC != "" {
		if c, err := strconv.Atoi(envC); err == nil && c > 0 {
			return c, "PUPPET_CONCURRENCY"
		}
		logging.Warn("Ignoring invalid PUPPET_CONCURRENCY=%q (expected a positive integer)", envC)
	}
	if c := p.cfg.Deploy.PuppetConcurrency; c > 0 {
		return c, "config"
	}
	c := runtime.NumCPU() / 2
	if c < 4 {
		c = 4
	}
	if c > 8 {
		c = 8
	}
	return c, "auto"
}

// firstCatalogCommand runs one agent catalog with the daemon stopped to avoid
// the catalog lock, then always leaves the service running. A failed catalog
// still restarts the daemon so the node keeps retrying instead of silently
// drifting with Puppet disabled.
func firstCatalogCommand(server string) string {
	return fmt.Sprintf(`
		set +e
		systemctl stop puppet 2>/dev/null || true
		/opt/puppetlabs/bin/puppet agent -t --server %s
		code=$?
		if [ $code -eq 0 ] || [ $code -eq 2 ]; then
			if ! systemctl start puppet; then
				echo "puppet service failed to start after a successful catalog" >&2
				exit 1
			fi
			if ! systemctl is-active --quiet puppet; then
				echo "puppet service is not active after a successful catalog" >&2
				exit 1
			fi
			exit 0
		fi
		systemctl start puppet 2>/dev/null || true
		exit $code
	`, shell.Quote(server))
}

// isPuppetSSLResetError identifies failures that can be repaired by deleting
// the agent's stale certificate and asking the server for a new one. A key
// mismatch is the historical case; signature/verify failures are the same
// class of stale or partially rotated SSL state.
func isPuppetSSLResetError(stdout, stderr string) bool {
	combined := strings.ToLower(stdout + "\n" + stderr)
	for _, marker := range []string{
		"does not match its private key",
		"certificate signature failure",
		"certificate verify failed",
	} {
		if strings.Contains(combined, marker) {
			return true
		}
	}
	return false
}

// catalogRank returns the role -> scheduling rank map for the first catalog.
// Frontends install WordPress, PHP and Apache; the monitor installs Prometheus,
// Grafana and Alertmanager. Starting the heaviest nodes last (the configured
// order puts frontends near the end) made their runtime the tail of the phase
// even though the concurrency window kept freeing slots earlier.
// Longest-processing-time-first is the standard makespan heuristic and does not
// change the concurrency limit itself.
func (p *Phase) catalogRank() map[string]int {
	ranks := map[string]int{}
	mark := func(specs []config.NodeDetail, rank int) {
		for _, n := range specs {
			if n.IP == "" {
				continue
			}
			if current, ok := ranks[n.IP]; !ok || rank < current {
				ranks[n.IP] = rank
			}
		}
	}
	mark(p.cfg.Nodes.CMSFrontends, 0)
	mark([]config.NodeDetail{p.cfg.Nodes.Monitor}, 1)
	mark(p.cfg.Nodes.Masters, 2)
	mark([]config.NodeDetail{p.cfg.Nodes.Storage}, 3)
	mark(p.cfg.Nodes.Workers, 4)
	mark(p.cfg.Nodes.LBs, 5)
	mark([]config.NodeDetail{p.cfg.Nodes.Router}, 6)
	return ranks
}

// orderCatalogNodes returns a copy sorted heaviest-first, stable for equal
// ranks so the configured order is still the tie-breaker. Nodes without a known
// role (hotdesks and future additions) run last; a wrong guess only changes
// order, never correctness.
func (p *Phase) orderCatalogNodes(nodes []config.NodeSpec) []config.NodeSpec {
	const unknownRank = 7
	ranks := p.catalogRank()
	rankOf := func(spec config.NodeSpec) int {
		if rank, ok := ranks[spec.IP]; ok {
			return rank
		}
		return unknownRank
	}

	ordered := make([]config.NodeSpec, len(nodes))
	copy(ordered, nodes)
	sort.SliceStable(ordered, func(i, j int) bool {
		return rankOf(ordered[i]) < rankOf(ordered[j])
	})
	return ordered
}

func (p *Phase) runFirstCatalog(ctx context.Context, nodes []config.NodeSpec) error {
	if len(nodes) == 0 {
		return nil
	}
	var errs []string
	var mu sync.Mutex
	jumpstartIP := p.cfg.Nodes.Jumpstart.IP
	server := p.jumpstartServerName()
	cmd := firstCatalogCommand(server)

	concurrency, source := p.catalogConcurrency()
	if concurrency > len(nodes) {
		concurrency = len(nodes)
	}

	ordered := p.orderCatalogNodes(nodes)
	logging.Info("Running first catalog in parallel across %d nodes (concurrency limit: %d, source: %s)...", len(nodes), concurrency, source)

	// A fixed worker pool consumes the sorted slice, so the first `concurrency`
	// nodes to start are deterministically the heaviest ones. The previous
	// semaphore-per-goroutine version left that choice to the scheduler.
	jobs := make(chan config.NodeSpec)
	var wg sync.WaitGroup

	runNode := func(spec config.NodeSpec) {
		logging.Info("Applying Puppet catalog on %s (%s)...", spec.Name, spec.IP)

		nodeCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		stdout, stderr, code, err := p.pool.RunCommand(nodeCtx, spec.IP, cmd)

		// Self-healing: remove the stale client certificate and its server
		// record, then retry once. This covers both a private-key mismatch
		// and the signature/verify failures left by a partial CA rotation.
		if code != 0 && isPuppetSSLResetError(stdout, stderr) {
			logging.Warn("Stale Puppet SSL state detected on %s (%s). Performing self-healing reset...", spec.Name, spec.FQDN)
			if spec.FQDN != "" {
				cleanCmd := fmt.Sprintf("puppetserver ca clean --certname %s", shell.Quote(spec.FQDN))
				if _, cleanStderr, cleanCode, cleanErr := p.pool.RunCommand(nodeCtx, jumpstartIP, cleanCmd); cleanErr != nil || cleanCode != 0 {
					logging.Warn("Could not clean Puppet certificate for %s on jumpstart (exit %d): %v %s", spec.FQDN, cleanCode, cleanErr, logging.Tail(cleanStderr, 5))
				}
			}
			if _, resetStderr, resetCode, resetErr := p.pool.RunCommand(nodeCtx, spec.IP, "rm -rf /etc/puppetlabs/puppet/ssl /etc/puppet/ssl"); resetErr != nil || resetCode != 0 {
				logging.Warn("Could not remove stale Puppet SSL directory on %s (exit %d): %v %s", spec.IP, resetCode, resetErr, logging.Tail(resetStderr, 5))
			}
			stdout, stderr, code, err = p.pool.RunCommand(nodeCtx, spec.IP, cmd)
		}

		if err != nil || code != 0 {
			mu.Lock()
			errs = append(errs, fmt.Sprintf("%s (%s): exit %d: %v\nSTDOUT: %s\nSTDERR: %s", spec.Name, spec.IP, code, err, strings.TrimSpace(stdout), strings.TrimSpace(stderr)))
			mu.Unlock()
		} else {
			logging.Success("Node %s converged successfully", spec.Name)
		}
	}

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for spec := range jobs {
				runNode(spec)
			}
		}()
	}
	for _, spec := range ordered {
		jobs <- spec
	}
	close(jobs)
	wg.Wait()

	if len(errs) > 0 {
		return fmt.Errorf("failed catalog run on some nodes:\n%s", strings.Join(errs, "\n"))
	}
	return nil
}
