// Package fstools implements native-compatible text Read, Write and Edit tools.
//
// Tools keep no session state. The worker remembers the whole-file SHA-256 from
// each successful ReadResult, keyed by resolved path within a session, and sends
// it as expected_sha256 on Write and Edit. An absent hash means never read in
// that session. Successful writes and edits return the resulting hash so the
// worker can advance its view. The worker owns that policy and path identity;
// the daemon only compares the supplied hash with the current bytes. Hashes are
// always calculated over raw file bytes. Model-visible output contains one text
// body, without the CLI token suffix.
//
// Read decodes UTF-8 and BOM-marked UTF-16LE. Invalid UTF-8 is displayed with
// replacement runes and an encoding notice; Edit and Write refuse it. Edit and
// Write preserve BOM-marked UTF-16LE when writing. Read deliberately rejects
// FIFOs and sockets before opening them instead of reproducing native's
// indefinite block on an unwritten FIFO; character devices are read through a
// bounded, cancelable goroutine.
//
// Edit matches old_string literally. When no literal match exists, curly double
// or single quotes in the file match straight quotes in old_string, and straight
// quotes in new_string take the corresponding curly style when the matched text
// used them. Files are normalized to LF for matching, then written with their
// first line ending. An empty old_string writes new_string as the whole content
// of a missing or empty file; a non-empty file is an error.
package fstools
