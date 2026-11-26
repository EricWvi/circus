// Package ledgersnap implements the session ledger snapshot device calls
// prepare_ledger, collect_ledger, rebase_ledger and remove_ledger, which the
// control plane sends (never a worker) to keep a copy of a project's ledger
// on the device for one session (device.proto, "Session ledger snapshots").
//
// Layout, below the ledger directory <state>/circus/ledger (or
// <CIRCUS_HOME>/state/ledger), where <state> is resolved as for the
// daemon's other state (circushome.State):
//
//	<id>/                  the snapshot the session's agent edits
//	.<id>.circus-lock      the lock serializing calls on <id>
//	.manifests/<id>.json   its manifest (snapshot.Manifest), and <id>.json.tmp
//	.staging/<id>/         chunks of a preparation in progress
//	.staging/<id>.state    the last chunk accepted and its base commit
//	.staging/<id>.paths.<n> the paths chunk n asked for
//	.staging/<id>.old      a replaced snapshot being removed
//	.staging/<id>.tmp      a file of a rebase being written
//	.staging/<id>.aside    a file a rebase moved aside to check it, and
//	                       <id>.aside.path its path and expected hash
//	.staging/<id>.clock    the probe a manifest's time is taken from
//
// The directories above the ledger directory are created as needed and may
// be symlinks (a user's ~/.local may be one); the ledger directory and
// everything below it are opened relative to their parent without
// following symlinks, so a symlink there is PATH_CONFLICT, never followed.
// The id "cli" is refused: it is the circus ledger CLI's own directory.
//
// A snapshot is valid exactly when its manifest exists. A preparation's
// last chunk therefore removes the old manifest (whether or not a
// directory is there) and its chunk state, swaps the staged directory in
// and writes the new manifest last; a removal removes the manifest first.
// A crash in between leaves a directory without a manifest, which collect
// and rebase report as ABSENT and the next preparation replaces without
// reset; a last chunk sent again is told to start from chunk 0. A manifest
// whose directory was removed from outside is swept.
//
// On macOS (foldsNames), requested paths that a case-insensitive or
// normalizing file system would take for one name are refused, within a
// request, across a preparation's chunks, and for a rebase's written paths
// against the snapshot's. Everywhere, the last chunk checks that the file
// system holds exactly the requested paths. A rebase may rename a file to
// another spelling of its name (a case-only rename: delete A.md, write
// a.md); such a path's state is read only while its directory lists it
// under exactly its spelling, and the deletion is applied just before the
// write. As for every path, an agent's change between that last check and
// the edit itself (renaming A.md to a.md, or a parent directory to another
// case) is not checked: the edits may then be applied in the renamed tree,
// and the rebuilt base catches it only when a difference from the merged
// view remains to be seen. Volumes that rewrite the names
// they store (HFS+ decomposes them) are not supported: a rebase writing
// such a name changes the files and then fails its rebuild.
//
// A rebase never makes an agent's uncollected change part of the base.
// The request carries known, the changes against the current base that
// the control plane collected and merged; before changing anything the
// daemon compares the snapshot with its manifest and requires every path
// to hold its collected state (its known change, or its base content), or
// the target of the edit on that path, and every edit to find its
// expected content or its target. Any other state, or any problem a
// collection would report, is STALE: the agent changed something after
// that collection, and the control plane collects again. The edits are
// then applied one by one, each checked again right before and applied
// through snapshot.Applier, which moves a file aside and hashes it before
// discarding it and installs new files only where nothing exists: a file
// the agent rewrote, deleted or created on an edit's path stays as the
// agent left it, and the rebase stops there with STALE (the edits before
// it applied, the old base kept). The whole directory is then read into a
// new manifest, which must be exactly the merged view (the base, then
// known, then the edits) before it is written, last; a file the agent
// wrote anywhere else while the rebase ran fails that check: STALE again,
// with the edits applied and the old base kept. A write through a
// descriptor opened before its file was moved aside lands in the moved
// file: before that file is hashed, it goes back with it; after, it is
// lost (see snapshot.Applier). A file a killed rebase left aside is
// recovered before every collection and rebase (snapshot.Applier.Recover):
// put back, dropped when it holds the expected content and its path is
// taken, or else kept under a conflict name the next collection reports.
//
// A rebase whose base_commit the manifest already records was done before:
// REBASED when nothing changed since, else STALE, and its edits are never
// applied again.
//
// Accepting an edit's target per path is what lets the same request
// finish a rebase that died after some edits (those paths hold their
// targets, the others their collected state; the old manifest still
// stands, so the next collection reports the applied edits as changes and
// nothing is lost) and repeat a finished one (the new base holds every
// target and every known change).
//
// Staging of an id untouched for an hour is removed by the next ledger
// call of any id that can take that id's lock. At most MaxCalls ledger
// calls run at once (BUSY beyond); a call on an id whose lock is held is
// BUSY too.
//
// Resources: a collection holds at most 8 MiB of changed content plus the
// bounds listed in package snapshot; a chunk of a preparation holds its
// request (at most 8 MiB of content) and reads its files back once at the
// last chunk, one at a time. Disk: one snapshot of the ledger per session,
// its manifest, and during a preparation or a reset one staged copy.
package ledgersnap
