package puppet

import (
	"fmt"
	"net"
	"strconv"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"gopkg.in/yaml.v3"
)

// Prometheus scrape configuration is rendered from config.yaml for the same
// reason as site.pp and the Hiera data: the committed file describes the
// default lab, and a renamed node, a non-default network or a different number
// of workers used to leave Prometheus scraping hosts that do not exist.
type prometheusConfig struct {
	Global        map[string]string        `yaml:"global"`
	Alerting      prometheusAlerting       `yaml:"alerting"`
	RuleFiles     []string                 `yaml:"rule_files"`
	ScrapeConfigs []prometheusScrapeConfig `yaml:"scrape_configs"`
}

type prometheusAlerting struct {
	Alertmanagers []prometheusAlertmanager `yaml:"alertmanagers"`
}

type prometheusAlertmanager struct {
	StaticConfigs []prometheusTargetGroup `yaml:"static_configs"`
}

type prometheusScrapeConfig struct {
	JobName         string                    `yaml:"job_name"`
	Scheme          string                    `yaml:"scheme,omitempty"`
	BearerTokenFile string                    `yaml:"bearer_token_file,omitempty"`
	TLSConfig       *prometheusTLSConfig      `yaml:"tls_config,omitempty"`
	MetricsPath     string                    `yaml:"metrics_path,omitempty"`
	Params          map[string][]string       `yaml:"params,omitempty"`
	StaticConfigs   []prometheusTargetGroup   `yaml:"static_configs"`
	RelabelConfigs  []prometheusRelabelConfig `yaml:"relabel_configs,omitempty"`
}

type prometheusTLSConfig struct {
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`
}

type prometheusTargetGroup struct {
	Targets []string `yaml:"targets"`
}

type prometheusRelabelConfig struct {
	SourceLabels []string `yaml:"source_labels,omitempty"`
	TargetLabel  string   `yaml:"target_label,omitempty"`
	Replacement  string   `yaml:"replacement,omitempty"`
}

// generatedPrometheusConfig renders the scrape jobs from the configured
// topology. Empty roles (no LBs, no frontends, no workers) simply omit their
// job instead of leaving a target pointing at the default lab's address.
func (p *Phase) generatedPrometheusConfig() ([]byte, error) {
	join := func(ips []string, port int) []string {
		var out []string
		seen := make(map[string]bool)
		for _, ip := range ips {
			if ip == "" {
				continue
			}
			target := net.JoinHostPort(ip, strconv.Itoa(port))
			if !seen[target] {
				out = append(out, target)
				seen[target] = true
			}
		}
		return out
	}
	ipsFromSpecs := func(specs []config.NodeSpec) []string {
		ips := make([]string, 0, len(specs))
		for _, n := range specs {
			ips = append(ips, n.IP)
		}
		return ips
	}
	ipsFromDetails := func(nodes []config.NodeDetail) []string {
		ips := make([]string, 0, len(nodes))
		for _, n := range nodes {
			ips = append(ips, n.IP)
		}
		return ips
	}
	targets := func(specs []config.NodeSpec, port int) []string {
		return join(ipsFromSpecs(specs), port)
	}
	targetsFromDetails := func(nodes []config.NodeDetail, port int) []string {
		return join(ipsFromDetails(nodes), port)
	}
	mainServiceDetails := func() []config.NodeDetail {
		nodes := make([]config.NodeDetail, 0, len(p.cfg.Nodes.LBs)+len(p.cfg.Nodes.CMSFrontends))
		nodes = append(nodes, p.cfg.Nodes.LBs...)
		nodes = append(nodes, p.cfg.Nodes.CMSFrontends...)
		return nodes
	}

	var scrape []prometheusScrapeConfig
	add := func(job string, targets []string) {
		if len(targets) == 0 {
			return
		}
		scrape = append(scrape, prometheusScrapeConfig{
			JobName:       job,
			StaticConfigs: []prometheusTargetGroup{{Targets: targets}},
		})
	}

	nodeTargets := append(targets(p.cfg.InternalNodes(), 9100), targetsFromDetails(mainServiceDetails(), 9100)...)
	add("node_exporter", nodeTargets)
	add("nginx", targetsFromDetails(p.cfg.Nodes.LBs, 9113))
	add("apache", targetsFromDetails(p.cfg.Nodes.CMSFrontends, 9117))
	if master, ok := p.cfg.FirstMaster(); ok {
		add("mysql", []string{net.JoinHostPort(master.IP, "9104")})
	}

	clusterDetails := make([]config.NodeDetail, 0, len(p.cfg.Nodes.Masters)+len(p.cfg.Nodes.Workers))
	clusterDetails = append(clusterDetails, p.cfg.Nodes.Masters...)
	clusterDetails = append(clusterDetails, p.cfg.Nodes.Workers...)
	if kubeletTargets := targetsFromDetails(clusterDetails, 10250); len(kubeletTargets) > 0 {
		scrape = append(scrape, prometheusScrapeConfig{
			JobName:         "k3s_kubelet",
			Scheme:          "https",
			BearerTokenFile: "/etc/prometheus/k3s.token",
			TLSConfig:       &prometheusTLSConfig{InsecureSkipVerify: true},
			MetricsPath:     "/metrics",
			StaticConfigs:   []prometheusTargetGroup{{Targets: kubeletTargets}},
		})
	}
	// The aggregated API server exposes its request histogram on the same
	// endpoint the kubelet uses for authentication, so the existing read-only
	// Prometheus ServiceAccount token (nonResourceURLs: /metrics) can scrape
	// it. Without this job the dashboard cannot show apiserver latency and a
	// slow control plane stays invisible until a request times out.
	if apiserverTargets := targetsFromDetails(p.cfg.Nodes.Masters, 6443); len(apiserverTargets) > 0 {
		scrape = append(scrape, prometheusScrapeConfig{
			JobName:         "kube-apiserver",
			Scheme:          "https",
			BearerTokenFile: "/etc/prometheus/k3s.token",
			TLSConfig:       &prometheusTLSConfig{InsecureSkipVerify: true},
			MetricsPath:     "/metrics",
			StaticConfigs:   []prometheusTargetGroup{{Targets: apiserverTargets}},
		})
	}
	add("etcd", targetsFromDetails(p.cfg.Nodes.Masters, 2381))

	blackboxRelabel := []prometheusRelabelConfig{
		{SourceLabels: []string{"__address__"}, TargetLabel: "__param_target"},
		{SourceLabels: []string{"__param_target"}, TargetLabel: "instance"},
		{TargetLabel: "__address__", Replacement: "localhost:9115"},
	}
	if vip := p.cfg.LBVIP(); vip != "" {
		scrape = append(scrape, prometheusScrapeConfig{
			JobName:     "blackbox",
			MetricsPath: "/probe",
			Params:      map[string][]string{"module": {"http_2xx"}},
			StaticConfigs: []prometheusTargetGroup{{
				Targets: []string{"https://" + vip + "/"},
			}},
			RelabelConfigs: blackboxRelabel,
		})
	}
	if frontends := targetsFromDetails(p.cfg.Nodes.CMSFrontends, 80); len(frontends) > 0 {
		scrape = append(scrape, prometheusScrapeConfig{
			JobName:        "blackbox-frontends",
			MetricsPath:    "/probe",
			Params:         map[string][]string{"module": {"tcp_connect"}},
			StaticConfigs:  []prometheusTargetGroup{{Targets: frontends}},
			RelabelConfigs: blackboxRelabel,
		})
	}

	config := prometheusConfig{
		Global: map[string]string{
			"scrape_interval":     "15s",
			"evaluation_interval": "15s",
			"scrape_timeout":      "10s",
		},
		Alerting: prometheusAlerting{Alertmanagers: []prometheusAlertmanager{
			{StaticConfigs: []prometheusTargetGroup{{Targets: []string{"localhost:9093"}}}},
		}},
		RuleFiles:     []string{"/etc/prometheus/alert-rules.yml"},
		ScrapeConfigs: scrape,
	}
	body, err := yaml.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("render prometheus.yml: %w", err)
	}
	header := "# Generated by `cms-ha phase setup-puppet` from config.yaml.\n# Do not edit on jumpstart: the next run overwrites this file.\n"
	return append([]byte(header), body...), nil
}
