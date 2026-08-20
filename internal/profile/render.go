package profile

import (
	"fmt"
	"path"
	"strings"

	"github.com/wetransform/code-vm/internal/guest"
)

// GuestRoot is where profile content lands in the guest. Delivered root-owned
// outside every mount: the agent may read it, never write it.
const GuestRoot = "/usr/local/share/sandbox-profiles"

// ManifestPath is the env file provision-system.sh and apply-profiles.sh
// source. It is always delivered — even with zero active profiles — so a
// deactivated profile's stale tree on the guest disk is never applied.
const ManifestPath = GuestRoot + "/manifest.env"

// GuestFiles renders the active profiles into the guest files both delivery
// paths install: mode:data entries at start, staged pushes on `profile apply`.
func GuestFiles(profiles []Profile) []guest.DataFile {
	out := []guest.DataFile{manifestEnv(profiles)}
	for _, p := range profiles {
		var list strings.Builder
		for _, f := range p.Files {
			perm := "0444"
			if f.Executable {
				// The applier keys the installed mode off this bit.
				perm = "0555"
			}
			out = append(out, guest.DataFile{
				Path:        path.Join(GuestRoot, p.Name, "files", f.Rel),
				Permissions: perm,
				Content:     string(f.Content),
			})
			list.WriteString(f.Rel + "\n")
		}
		// files.list names what THIS version ships; the applier installs
		// exactly these, so files dropped from the profile stop being applied
		// even though mode:data cannot delete their leftovers. Rendered for
		// EVERY active profile, empty when it ships no files: like
		// manifest.env, it must always be delivered, because a profile
		// version that drops its last file still needs to overwrite a
		// previous version's non-empty files.list with an empty one — a
		// boot-path mode:data entry can never delete a stale file, only
		// overwrite it.
		out = append(out, guest.DataFile{
			Path:        path.Join(GuestRoot, p.Name, "files.list"),
			Permissions: "0444",
			Content:     list.String(),
		})
		if p.Hook != nil {
			out = append(out, guest.DataFile{
				Path:        path.Join(GuestRoot, p.Name, "hook"),
				Permissions: "0555",
				Content:     string(p.Hook),
			})
		}
	}
	return out
}

// manifestEnv renders the applier's inputs. Every value was validated at load
// time against charsets free of whitespace and quotes, so %q quoting is safe
// for shell sourcing.
func manifestEnv(profiles []Profile) guest.DataFile {
	var names, packages, hooks []string
	seenPkg := map[string]bool{}
	shell := ""
	for _, p := range profiles {
		names = append(names, p.Name)
		for _, pkg := range p.Manifest.Packages {
			if !seenPkg[pkg] {
				seenPkg[pkg] = true
				packages = append(packages, pkg)
			}
		}
		if p.Manifest.Shell != "" {
			shell = p.Manifest.Shell // last profile wins, like file collisions
		}
		if p.Manifest.Hook != "" {
			hooks = append(hooks, p.Name)
		}
	}
	var b strings.Builder
	b.WriteString("# Written by code-vm. Sourced by provision-system.sh and apply-profiles.sh.\n")
	fmt.Fprintf(&b, "PROFILES=%q\n", strings.Join(names, " "))
	fmt.Fprintf(&b, "PROFILE_PACKAGES=%q\n", strings.Join(packages, " "))
	fmt.Fprintf(&b, "PROFILE_SHELL=%q\n", shell)
	fmt.Fprintf(&b, "PROFILE_HOOKS=%q\n", strings.Join(hooks, " "))
	return guest.DataFile{Path: ManifestPath, Permissions: "0444", Content: b.String()}
}

// AllowDomains merges the config's extraDomains with every active profile's
// domains, preserving order and dropping duplicates. This one list feeds both
// provision.env (boot) and the Squid fragment (running VM), so the two paths
// cannot drift.
func AllowDomains(extra []string, profiles []Profile) []string {
	seen := map[string]bool{}
	var out []string
	add := func(ds []string) {
		for _, d := range ds {
			if d == "" || seen[d] {
				continue
			}
			seen[d] = true
			out = append(out, d)
		}
	}
	add(extra)
	for _, p := range profiles {
		add(p.Manifest.Domains)
	}
	return out
}
