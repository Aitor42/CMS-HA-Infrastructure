package cmshainfra

import "embed"

//go:embed all:templates
var TemplatesFS embed.FS

//go:embed all:kubernetes
var KubernetesFS embed.FS

// GrafanaFS holds the dashboards the monitoring role ships. Puppet serves the
// very same files from puppet/modules/role/files/grafana, so the verification
// checks the JSON that is actually deployed instead of a copy.
//
//go:embed all:puppet/modules/role/files/grafana
var GrafanaFS embed.FS
