package serveraclinit

import (
	"bytes"
	"strings"
	"text/template"
)

type rulesData struct {
	EnableNamespaces    bool
	ConsulSyncNamespace string
	EnableNSMirroring   bool
	MirroringPrefix     string
}

// Rules that don't currently have any
// modifications based on config changes
func (c *Command) snapshotAgentRules() string {
	snapshotAgentRules := `acl = "write"
key "consul-snapshot/lock" {
   policy = "write"
}
session_prefix "" {
   policy = "write"
}
service "consul-snapshot" {
   policy = "write"
}`

	return snapshotAgentRules
}

func (c *Command) entLicenseRules() string {
	entLicenseRules := `operator = "write"`

	return entLicenseRules
}

// Rules that depend on namespace configuration
func (c *Command) agentRules() (string, error) {
	templateData := rulesData{
		EnableNamespaces: c.flagEnableNamespaces,
	}

	agentRulesTpl := `
  node_prefix "" {
    policy = "write"
  }
{{- if .EnableNamespaces }}
namespace_prefix "" {
{{- end }}
  service_prefix "" {
    policy = "read"
  }
{{- if .EnableNamespaces }}
}
{{- end }}
`

	// Render the command
	var buf bytes.Buffer
	tpl := template.Must(template.New("root").Parse(strings.TrimSpace(
		agentRulesTpl)))
	err := tpl.Execute(&buf, &templateData)
	if err != nil {
		return "", err
	}

	return buf.String(), nil
}

func (c *Command) dnsRules() (string, error) {
	templateData := rulesData{
		EnableNamespaces: c.flagEnableNamespaces,
	}

	// DNS rules need to have access to all namespaces
	// to be able to resolve services in any namespace.
	dnsRulesTpl :=
		`
{{- if .EnableNamespaces }}
namespace_prefix "" {
{{- end }}
  node_prefix "" {
     policy = "read"
  }
  service_prefix "" {
     policy = "read"
  }
{{- if .EnableNamespaces }}
}
{{- end }}
`

	// Render the command
	var buf bytes.Buffer
	tpl := template.Must(template.New("root").Parse(strings.TrimSpace(
		dnsRulesTpl)))
	err := tpl.Execute(&buf, &templateData)
	if err != nil {
		return "", err
	}

	return buf.String(), nil
}

// This assumes users are using the default name for the service, i.e.
// "mesh-gateway".
func (c *Command) meshGatewayRules() (string, error) {
	templateData := rulesData{
		EnableNamespaces: c.flagEnableNamespaces,
	}

	meshGatewayRulesTpl := `
{{- if .EnableNamespaces }}
namespace_prefix "" {
{{- end }}
  service_prefix "" {
     policy = "read"
  }

  service "mesh-gateway" {
     policy = "write"
  }
{{- if .EnableNamespaces }}
}
{{- end }}
`

	// Render the command
	var buf bytes.Buffer
	tpl := template.Must(template.New("root").Parse(strings.TrimSpace(
		meshGatewayRulesTpl)))
	err := tpl.Execute(&buf, &templateData)
	if err != nil {
		return "", err
	}

	return buf.String(), nil
}

func (c *Command) syncRules() (string, error) {
	templateData := rulesData{
		EnableNamespaces:    c.flagEnableNamespaces,
		ConsulSyncNamespace: c.flagConsulSyncNamespace,
		EnableNSMirroring:   c.flagEnableNSMirroring,
		MirroringPrefix:     c.flagMirroringPrefix,
	}

	syncRulesTpl := `
  node "k8s-sync" {
	policy = "write"
  }
{{- if .EnableNamespaces }}
operator = "write"
{{- if .EnableNSMirroring }}
namespace_prefix "{{ .MirroringPrefix }}" {
{{- else }}
namespace "{{ .ConsulSyncNamespace }}" {
{{- end }}
{{- end }}
  node_prefix "" {
    policy = "read"
  }
  service_prefix "" {
    policy = "write"
  }
{{- if .EnableNamespaces }}
}
{{- end }}
`

	// Render the command
	var buf bytes.Buffer
	tpl := template.Must(template.New("root").Parse(strings.TrimSpace(
		syncRulesTpl)))
	err := tpl.Execute(&buf, &templateData)
	if err != nil {
		return "", err
	}

	return buf.String(), nil
}

// This should only be set when namespaces are enabled.
func (c *Command) injectorRules() (string, error) {
	templateData := rulesData{
		EnableNamespaces: c.flagEnableNamespaces,
	}

	// The Connect injector only needs permissions to create namespaces
	injectorRulesTpl := `
{{- if .EnableNamespaces }}
operator = "write"
{{- end }}
`

	// Render the command
	var buf bytes.Buffer
	tpl := template.Must(template.New("root").Parse(strings.TrimSpace(
		injectorRulesTpl)))
	err := tpl.Execute(&buf, &templateData)
	if err != nil {
		return "", err
	}

	return buf.String(), nil
}
