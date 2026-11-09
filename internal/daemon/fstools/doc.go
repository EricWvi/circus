// Package fstools implements native-compatible text Read, Write and Edit tools.
//
// Tools keep no session state. The worker remembers the whole-file SHA-256 from
// each successful ReadResult, keyed by resolved path within a session, and sends
// it as expected_sha256 on Write and Edit. An absent hash means never read in
// that session. Successful writes and edits return the resulting hash so the
// worker can advance its view. The worker owns that policy and path identity;
// the daemon only compares the supplied hash with the current bytes.
// Model-visible output contains one text body, without the CLI token suffix.
package fstools
