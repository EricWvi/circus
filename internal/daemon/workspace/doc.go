// Package workspace implements the prepare_workspace and remove_workspace
// device calls, which manage a project's default directory on the device
// (docs/wire.md "Workspace preparation").
//
// Prepare resolves the path, creates its parent, takes a lock beside the
// target, removes hidden clones that earlier attempts left, and then
// creates an empty directory, accepts what is there, or clones the
// repository into a hidden sibling and renames it onto the target once
// complete. git runs without a shell, with the Bash tool's login-shell
// environment minus repository-locating variables, in its own process
// group that is killed and reaped on cancellation. A clone outliving the
// call becomes a job of the call's session with a two-hour lifetime; its
// outcome stays available to resume_job_id for thirty minutes. At most two
// preparations run at once.
//
// Remove deletes only a real directory directly inside
// <home>/circus/projects, through directory descriptors that each refuse
// symlinks and mount crossings at open time (openat2 with
// RESOLVE_NO_SYMLINKS|RESOLVE_NO_XDEV on Linux, O_NOFOLLOW and st_dev on
// macOS), so no symlink is followed and no other mount entered, bind
// mounts included.
//
// Credentials in repository URLs never appear in results, job commands or
// logs: everything leaving the tool passes gitenv.SanitizeURL or
// gitenv.RedactText.
package workspace
