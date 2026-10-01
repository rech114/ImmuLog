// SPDX-License-Identifier: Apache-2.0

package gitx

import "context"

// transport.go -- network transport. It reuses git's own transports
// (ssh / https / file / git), which makes this node a git **client** and means
// we never implement a git protocol server ourselves.
//
// That is what "reuse the wheel" buys: ssh, https, pack negotiation and delta
// transfer all come for free.

// Quarantine is the root namespace for the quarantine area.
//
// **Network input never writes trusted state directly.** Fetched refs land
// here first; only after verification does a CAS promote them into
// refs/feeds/*.
const Quarantine = "refs/quarantine/"

// FetchInto pulls a remote namespace into the local quarantine area.
//
// remotePattern is a wildcard (e.g. refs/feeds/*) and localPrefix is normally
// Quarantine + <slot name>. The "+" is deliberate: the quarantine is meant to
// be overwritten, while trusted state never updates through this path.
func (r *Repo) FetchInto(ctx context.Context, url, remotePattern, localPrefix string) error {
	refspec := "+" + remotePattern + ":" + localPrefix + "/*"
	_, err := r.run(ctx, nil, "fetch", "--no-tags", "--quiet", url, refspec)
	return err
}

// PushRef pushes one local ref to the same name on the remote (best-effort;
// failure is not fatal).
func (r *Repo) PushRef(ctx context.Context, url, ref string) error {
	_, err := r.run(ctx, nil, "push", "--quiet", url, ref+":"+ref)
	return err
}
