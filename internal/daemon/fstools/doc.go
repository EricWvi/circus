// Package fstools implements native-compatible text Read, Write and Edit tools.
//
// Tools keep no session state. The worker remembers the whole-file SHA-256 from
// each successful ReadResult, keyed by resolved path within a session, and
// sends it as expected_sha256 on Write and Edit. An absent hash means never
// read in that session. Edit adds native's context-current parenthetical
// whenever a hash is sent, while native adds it only when the session holds the
// whole file (tests/parity/NATIVE-BEHAVIOR.md); the hash alone cannot express a
// partially read file whose changes are still checked. Successful writes and
// edits return the resulting hash so the worker can advance its view. The
// worker owns that policy and path identity; the daemon only compares the
// supplied hash with the current bytes. Hashes are always calculated over raw
// file bytes. Model-visible output contains one text body, without the CLI
// token suffix.
//
// Read decodes UTF-8 and BOM-marked UTF-16LE. Invalid UTF-8 is displayed with
// replacement runes and an encoding notice; malformed UTF-16LE is displayed
// with replacement runes. Files with a measured binary extension are refused
// whatever their content; other files with NUL bytes in their first 8000 raw
// bytes are read as text with a NUL notice, which Edit and Write refuse. One CR
// before a line ending, or at the end of the file, is not displayed, and the
// body loses trailing whitespace. A leading UTF-8 byte-order mark is an
// encoding marker like the UTF-16LE one: it is not displayed, Edit cannot match
// it, and Write and Edit keep it. Edit and Write refuse both malformed
// encodings and preserve valid BOM-marked UTF-16LE when writing. Outside .md
// and .mdx files, Write content and Edit replacement text for an existing file
// lose trailing whitespace on every line. Read deliberately rejects FIFOs and
// sockets before opening them instead of reproducing native's indefinite block
// on an unwritten FIFO; character devices are read through a bounded,
// cancelable goroutine. Ranged Read retains only selected lines and caps their
// decoded line-content payload at 3,200,000 bytes, excluding line numbers,
// tabs, separating newlines, and line endings.
//
// Edit and Write reject existing files larger than 256 MiB. Before allocating a
// replacement, Edit also computes the result size from the match count and
// rejects a result over 256 MiB; with replace_all=false, only one replacement
// counts toward that check.
//
// Edit matches old_string literally. When no literal match exists, curly double
// or single quotes in the file match straight quotes in old_string, and
// straight quotes in new_string take the corresponding curly style when the
// matched text used them. Files are normalized to LF for matching, while
// old_string and new_string are used as given, then written with CRLF only when
// CRLF endings outnumber lone LF endings in the first 4096 bytes. An empty
// replacement also deletes the LF that follows the matched text. An empty
// old_string writes new_string as the whole content of a missing or empty file;
// a non-empty file is an error, reported before any encoding check.
package fstools
