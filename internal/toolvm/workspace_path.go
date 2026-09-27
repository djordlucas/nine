package toolvm

import (
	"encoding/json"
	"path"
	"strings"
)

// rewriteWorkspacePath maps a host workspace path in a shipped tool's `path`
// argument onto the guest mount at /work.
//
// The two sides of the workspace have different names. `shell` runs in the host
// directory and prints host paths (/data/workspace/notes.txt); a shipped tool
// sees only the guest mount (/work/notes.txt), because grantDescription
// deliberately withholds the host side — a tool is told where it is mounted, not
// where that lives on the operator's disk. A model that reads a path out of
// shell output and hands it to read_file is therefore naming a file the guest
// has no name for, and gets "cannot read" for a file that plainly exists.
//
// The translation happens here, in the daemon, for exactly that reason: the host
// knows both names and the guest is told neither. It is argument normalization,
// not containment — a path that does not start with the workspace root is passed
// through untouched, and wazero's pre-open remains the only thing that decides
// what the tool can reach (adr/rich-js-tools.md §6.4).
//
// Applied only to shipped tools holding the shipped workspace mount: their
// schemas are first-party and their `path` property is known to be a workspace
// path. A generated tool's arguments are its own business.
func rewriteWorkspacePath(args json.RawMessage, hostRoot string) json.RawMessage {
	if hostRoot == "" || len(args) == 0 {
		return args
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(args, &obj); err != nil {
		return args // not an object; nothing named `path` to rewrite
	}
	raw, ok := obj["path"]
	if !ok {
		return args
	}
	var p string
	if err := json.Unmarshal(raw, &p); err != nil || p == "" {
		return args
	}
	mapped, changed := mapHostPath(p, hostRoot)
	if !changed {
		return args
	}
	enc, err := json.Marshal(mapped)
	if err != nil {
		return args
	}
	obj["path"] = enc
	out, err := json.Marshal(obj)
	if err != nil {
		return args
	}
	return out
}

// mapHostPath rewrites one path, reporting whether it changed. The root itself
// maps to the mount root, and a path merely sharing a textual prefix with the
// root (/data/workspace-backup next to /data/workspace) is left alone, because
// it is a different directory.
func mapHostPath(p, hostRoot string) (string, bool) {
	root := path.Clean(hostRoot)
	clean := path.Clean(p)
	if clean == root {
		return ShippedWorkspaceGuest, true
	}
	if !strings.HasPrefix(clean, root+"/") {
		return p, false
	}
	return path.Join(ShippedWorkspaceGuest, strings.TrimPrefix(clean, root+"/")), true
}
