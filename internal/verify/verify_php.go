package verify

import (
	"context"
	"fmt"
	"strings"
)

type phpPackageState struct {
	version string
	patched bool
}

// phpPatchState is what a frontend reports about its exposure to
// CVE-2026-109618 / CVE-2026-109619.
type phpPatchState struct {
	// packages is keyed by binary package name (php8.3-cli, php8.3-common,
	// libapache2-mod-php8.3), so a patched CLI cannot mask a vulnerable SAPI.
	packages map[string]phpPackageState
	// modPHP reports whether Apache serves PHP through mod_php.
	modPHP bool
	// mitigations holds the mitigations the frontend reported as in place.
	mitigations map[string]bool
}

// checkPHPSecurityPatch proves the frontends are not exposed to the two
// unauthenticated remote code executions published in the PHP core on
// 2026-10-21 (CVE-2026-109618 and CVE-2026-109619, "php2shell"): Apache with
// mod_php is the setup the PHP team rates as the highest risk, and the fix is
// a package upgrade that nothing in the stack would apply on its own.
//
// The version comparison happens on the node with dpkg, so the answer follows
// Debian's own version rules: Ubuntu backports the upstream patch and keeps
// the upstream version number, and only the Debian revision tells a patched
// node from a vulnerable one.
func (v *Verifier) checkPHPSecurityPatch(ctx context.Context) e2eOutcome {
	o := e2eOutcome{label: "PHP security patch"}
	var ips []string
	for _, cms := range v.cfg.Nodes.CMSFrontends {
		if cms.IP != "" {
			ips = append(ips, cms.IP)
		}
	}
	if len(ips) == 0 {
		o.detail = "no CMS frontend configured"
		return o
	}

	res := v.ssh.RunParallel(ctx, ips, phpPatchProbe)
	pass := true
	var details []string
	for _, r := range res {
		if r.Err != nil || r.ExitCode != 0 {
			pass = false
			details = append(details, fmt.Sprintf("%s: probe failed (exit %d) %s", r.Host, r.ExitCode, strings.TrimSpace(r.Output)))
			continue
		}
		state, err := parsePHPPatchProbe(r.Output)
		if err != nil {
			pass = false
			details = append(details, fmt.Sprintf("%s: %v", r.Host, err))
			continue
		}
		var problems []string
		for _, pkg := range phpPatchedPackages {
			st := state.packages[pkg]
			if !st.patched {
				problems = append(problems, fmt.Sprintf("%s %s is older than the patched %s (CVE-2026-109618/CVE-2026-109619)",
					pkg, st.version, phpPatchedPackage))
			}
		}
		for _, mitigation := range requiredPHPMitigations {
			if !state.mitigations[mitigation] {
				problems = append(problems, "missing mitigation "+mitigation)
			}
		}
		if len(problems) > 0 {
			pass = false
			details = append(details, fmt.Sprintf("%s: %s", r.Host, strings.Join(problems, ", ")))
			continue
		}
		sapi := "php-fpm"
		if state.modPHP {
			sapi = "mod_php"
		}
		details = append(details, fmt.Sprintf("%s: php %s patched (%d packages), %s hardened",
			r.Host, state.packages["php8.3-cli"].version, len(phpPatchedPackages), sapi))
	}
	o.pass = pass
	o.detail = strings.Join(details, "; ")
	return o
}

// parsePHPPatchProbe reads the labelled lines the PHP probe prints. Labelling
// keeps a PHP notice or a warning on the same stream from being misread as
// one of the answers.
func parsePHPPatchProbe(out string) (phpPatchState, error) {
	state := phpPatchState{packages: map[string]phpPackageState{}, mitigations: map[string]bool{}}
	reportedPatch := map[string]bool{}
	var modPHP string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "php_package="):
			name, version, ok := strings.Cut(strings.TrimPrefix(line, "php_package="), "|")
			if !ok || name == "" {
				return state, fmt.Errorf("malformed package line %q", line)
			}
			st := state.packages[name]
			st.version = version
			state.packages[name] = st
		case strings.HasPrefix(line, "php_patched="):
			name, patched, ok := strings.Cut(strings.TrimPrefix(line, "php_patched="), "|")
			if !ok || name == "" {
				return state, fmt.Errorf("malformed patch line %q", line)
			}
			switch patched {
			case "yes", "no":
			default:
				return state, fmt.Errorf("the probe reported an unknown patch state %q for %s", patched, name)
			}
			st := state.packages[name]
			st.patched = patched == "yes"
			state.packages[name] = st
			reportedPatch[name] = true
		case strings.HasPrefix(line, "php_mod_php="):
			modPHP = strings.TrimPrefix(line, "php_mod_php=")
		case strings.HasPrefix(line, "php_mitigation="):
			state.mitigations[strings.TrimPrefix(line, "php_mitigation=")] = true
		}
	}
	for _, pkg := range phpPatchedPackages {
		st, ok := state.packages[pkg]
		if !ok {
			return state, fmt.Errorf("the probe reported no state for %s", pkg)
		}
		if st.version == "" {
			return state, fmt.Errorf("the probe reported no installed version for %s", pkg)
		}
		if !reportedPatch[pkg] {
			return state, fmt.Errorf("the probe did not report whether %s carries the fix", pkg)
		}
	}
	switch modPHP {
	case "yes":
		state.modPHP = true
	case "no":
	default:
		return state, fmt.Errorf("the probe did not report whether Apache loads mod_php")
	}
	return state, nil
}

// checkLoadBalancers asks every load balancer for the site directly. The VIP
// only ever resolves to the active node, so without this a broken backup
// balancer would stay hidden until a failover happened in the middle of an
// incident. The VIP's own HTTPS answer is already covered by phase08.
