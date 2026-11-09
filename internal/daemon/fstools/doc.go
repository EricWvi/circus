// Package fstools implements native-compatible text Read, Write and Edit tools.
//
// Tools keep no session state. The worker remembers the whole-file SHA-256 from
// each successful ReadResult, keyed by resolved path within a session, and sends
// it as expected_sha256 on Write and Edit. An absent hash means never read in
// that session. Successful writes and edits return the resulting hash so the
// worker can advance its view. The worker owns that policy and path identity;
// the daemon only compares the supplied hash with the current bytes.
// Model-visible output contains one text body, without the CLI token suffix.
//
// Edit matches old_string literally. When no literal match exists, curly double
// quotes in the file match straight quotes in old_string, and straight quotes
// in new_string become alternating curly quotes when the matched text used
// them. In CRLF files, multiline old_string and new_string are normalized to
// CRLF before matching. An empty old_string writes new_string as the whole
// content of a missing or empty file; a non-empty file is an error.
package fstools
