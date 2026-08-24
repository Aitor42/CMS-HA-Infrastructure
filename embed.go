package cmshainfra

import "embed"

//go:embed all:templates
var TemplatesFS embed.FS

//go:embed all:kubernetes
var KubernetesFS embed.FS
