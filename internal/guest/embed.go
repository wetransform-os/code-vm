// Package guest holds the assets installed into the sandbox VM: the Lima
// template and every script, systemd unit and config file delivered to the
// guest. They are embedded in the binary and shipped as Lima `mode: data`
// provision entries, so the CLI and the guest side can never fall out of sync.
package guest

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

//go:embed all:files
var assets embed.FS

// DataFile is one file to materialise in the guest.
type DataFile struct {
	Path        string
	Permissions string
	Content     string
}

// LimaTemplate returns the unrendered Lima instance template.
func LimaTemplate() (string, error) {
	b, err := assets.ReadFile("files/lima/code-sandbox.yaml.tpl")
	if err != nil {
		return "", fmt.Errorf("read embedded Lima template: %w", err)
	}
	return string(b), nil
}

// ConfigRoot is where the canonical config tree lives in the guest. It mirrors
// the agent's home; lock-settings.sh copies it there and locks every file on
// each boot, and `code-vm config apply` re-delivers it to a running VM.
const ConfigRoot = "/usr/local/share/sandbox-config"

// guestPath maps an embedded path to its install location and permissions.
// An empty path means the file is not delivered to the guest.
func guestPath(p string) (string, string) {
	rel, ok := trimPrefix(p, "files/scripts/")
	if ok {
		if rel == "sandbox-exec" {
			return "/usr/local/bin/sandbox-exec", "0755"
		}
		return "/usr/local/lib/sandbox/" + rel, "0755"
	}
	if rel, ok := trimPrefix(p, "files/systemd/"); ok {
		return "/etc/systemd/system/" + rel, "0644"
	}
	if rel, ok := trimPrefix(p, "files/config/"); ok {
		return ConfigRoot + "/" + rel, "0444"
	}
	return "", ""
}

func trimPrefix(s, prefix string) (string, bool) {
	if !strings.HasPrefix(s, prefix) {
		return "", false
	}
	return strings.TrimPrefix(s, prefix), true
}

// DataFiles returns every embedded asset that is delivered to the guest,
// sorted by guest path so rendering is deterministic.
func DataFiles() ([]DataFile, error) {
	var out []DataFile
	err := fs.WalkDir(assets, "files", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		dst, perms := guestPath(p)
		if dst == "" {
			return nil // the Lima template is rendered, not delivered
		}
		b, err := assets.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", p, err)
		}
		out = append(out, DataFile{Path: path.Clean(dst), Permissions: perms, Content: string(b)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// ConfigFiles returns the subset of DataFiles that lands under ConfigRoot:
// what lock-settings.sh consumes. Scripts and units are left out on purpose;
// they are only ever installed by Lima before a boot.
func ConfigFiles() ([]DataFile, error) {
	all, err := DataFiles()
	if err != nil {
		return nil, err
	}
	var out []DataFile
	for _, f := range all {
		if strings.HasPrefix(f.Path, ConfigRoot+"/") {
			out = append(out, f)
		}
	}
	return out, nil
}
