package cloudinit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kdomanski/iso9660"
)

func TestRenderUserData_SSHKeysAndHostname(t *testing.T) {
	t.Parallel()

	got := RenderUserData(Request{
		InstanceID: "11111111-1111-1111-1111-111111111111",
		Hostname:   "demo-vm",
		SSHPubkeys: []string{"ssh-ed25519 AAAA user@host", "ssh-rsa BBBB other@host"},
	})
	for _, want := range []string{
		"#cloud-config",
		"hostname: demo-vm",
		"preserve_hostname: false",
		`- ssh-ed25519 AAAA user@host`,
		`- ssh-rsa BBBB other@host`,
		"- default",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestRenderUserData_NoKeys(t *testing.T) {
	t.Parallel()

	got := RenderUserData(Request{InstanceID: "x", Hostname: "h"})
	if strings.Contains(got, "ssh_authorized_keys:") {
		t.Fatalf("did not expect ssh_authorized_keys:\n%s", got)
	}
}

func TestRenderMetaData(t *testing.T) {
	t.Parallel()

	got := RenderMetaData(Request{InstanceID: "abc", Hostname: "demo-vm"})
	if !strings.Contains(got, "instance-id: abc") {
		t.Fatalf("no instance-id: %s", got)
	}
	if !strings.Contains(got, "local-hostname: demo-vm") {
		t.Fatalf("no local-hostname: %s", got)
	}
}

func TestBuildSeed_ISOContainsFiles(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "seed.iso")
	f, err := os.Create(path) //nolint:gosec // tempdir path
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	err = BuildSeed(f, Request{
		InstanceID: "inst-1",
		Hostname:   "demo",
		SSHPubkeys: []string{"ssh-ed25519 AAAA"},
	})
	_ = f.Close()
	if err != nil {
		t.Fatalf("BuildSeed: %v", err)
	}

	f2, err := os.Open(path) //nolint:gosec // tempdir path
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = f2.Close() }()
	img, err := iso9660.OpenImage(f2)
	if err != nil {
		t.Fatalf("open iso: %v", err)
	}
	root, err := img.RootDir()
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	children, err := root.GetChildren()
	if err != nil {
		t.Fatalf("children: %v", err)
	}

	want := map[string]bool{"user-data": false, "meta-data": false}
	for _, c := range children {
		// Some iso implementations uppercase filenames — normalize before
		// comparing.
		name := strings.ToLower(c.Name())
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	for k, ok := range want {
		if !ok {
			t.Errorf("missing %s in iso (got %d files)", k, len(children))
		}
	}

	// Sanity check the user-data body by scanning one file.
	for _, c := range children {
		if strings.ToLower(c.Name()) != "user-data" {
			continue
		}
		reader := c.Reader()
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(reader); err != nil {
			t.Fatalf("read user-data: %v", err)
		}
		if !strings.Contains(buf.String(), "#cloud-config") {
			t.Fatalf("user-data missing cloud-config header:\n%s", buf.String())
		}
	}
}

func TestBuildSeed_Validation(t *testing.T) {
	t.Parallel()

	if err := BuildSeed(&bytes.Buffer{}, Request{Hostname: "x"}); err == nil {
		t.Fatal("expected error when InstanceID missing")
	}
	if err := BuildSeed(&bytes.Buffer{}, Request{InstanceID: "x"}); err == nil {
		t.Fatal("expected error when Hostname missing")
	}
}
