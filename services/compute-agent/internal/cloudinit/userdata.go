// Package cloudinit builds NoCloud seed ISOs that libvirt attaches to a VM's
// cdrom so Ubuntu cloud images pick up our SSH keys and hostname on first boot.
package cloudinit

import (
	"fmt"
	"io"
	"strings"

	"github.com/kdomanski/iso9660"
	"gopkg.in/yaml.v3"
)

// Request is the input to BuildSeed.
type Request struct {
	InstanceID string
	Hostname   string
	SSHPubkeys []string
	// ExtraRunCmds runs after cloud-init's default setup. Phase 3 only needs
	// empty; Phase 4+ will add GPU driver probes here.
	ExtraRunCmds []string
}

// BuildSeed writes a NoCloud-format ISO to w containing user-data + meta-data
// derived from req. libvirt must attach the ISO to the VM as a readonly
// cdrom with label CIDATA.
func BuildSeed(w io.Writer, req Request) error {
	if err := req.validate(); err != nil {
		return err
	}
	userData := renderUserData(req)
	metaData := renderMetaData(req)

	writer, err := iso9660.NewWriter()
	if err != nil {
		return fmt.Errorf("iso writer: %w", err)
	}
	defer func() { _ = writer.Cleanup() }()

	if err := writer.AddFile(strings.NewReader(userData), "user-data"); err != nil {
		return fmt.Errorf("add user-data: %w", err)
	}
	if err := writer.AddFile(strings.NewReader(metaData), "meta-data"); err != nil {
		return fmt.Errorf("add meta-data: %w", err)
	}

	// CIDATA is the required volume identifier for NoCloud. Without it,
	// cloud-init silently skips the cdrom seed.
	if err := writer.WriteTo(w, "CIDATA"); err != nil {
		return fmt.Errorf("write iso: %w", err)
	}
	return nil
}

// RenderUserData is exposed so tests and debugging tools can diff the YAML
// without producing an ISO.
func RenderUserData(req Request) string { return renderUserData(req) }

// RenderMetaData mirrors RenderUserData for the meta-data half.
func RenderMetaData(req Request) string { return renderMetaData(req) }

func (r Request) validate() error {
	if r.InstanceID == "" {
		return fmt.Errorf("cloudinit: InstanceID required")
	}
	if r.Hostname == "" {
		return fmt.Errorf("cloudinit: Hostname required")
	}
	return nil
}

func renderUserData(r Request) string {
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	fmt.Fprintf(&b, "hostname: %s\n", yamlString(r.Hostname))
	fmt.Fprintf(&b, "preserve_hostname: false\n")
	fmt.Fprintf(&b, "manage_etc_hosts: true\n")
	if len(r.SSHPubkeys) > 0 {
		b.WriteString("ssh_authorized_keys:\n")
		for _, k := range r.SSHPubkeys {
			fmt.Fprintf(&b, "  - %s\n", yamlString(k))
		}
	}
	b.WriteString("users:\n")
	b.WriteString("  - default\n")
	if len(r.ExtraRunCmds) > 0 {
		b.WriteString("runcmd:\n")
		for _, c := range r.ExtraRunCmds {
			fmt.Fprintf(&b, "  - %s\n", yamlString(c))
		}
	}
	return b.String()
}

func renderMetaData(r Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "instance-id: %s\n", yamlString(r.InstanceID))
	fmt.Fprintf(&b, "local-hostname: %s\n", yamlString(r.Hostname))
	return b.String()
}

// yamlString returns a YAML-safe scalar encoding of s. Hand-rolled escape
// (just `:`, `#`, `\n`, `\t`, leading/trailing space) misses backslashes,
// non-ASCII characters, and control bytes — all of which can appear in SSH
// pubkey comments or non-Latin hostnames and would corrupt the YAML
// stream. Delegating to yaml.Marshal is one line and inherits the spec's
// full escape table.
func yamlString(s string) string {
	out, err := yaml.Marshal(s)
	if err != nil {
		// yaml.Marshal of a string never fails in practice; fall back to
		// a defensively-quoted form so we never inject raw input.
		return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
	}
	// yaml.Marshal appends a trailing newline ("foo\n"); strip it so the
	// caller can put yamlString output inline with its own formatting.
	return strings.TrimRight(string(out), "\n")
}
