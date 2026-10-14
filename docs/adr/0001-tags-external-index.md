# Tags are stored in an external index keyed by `tag.Sum`, not in the audio file

## Status

Superseded. Tag support was removed from derpy on 2026-10-14. It may return once Tags can be stored in, and searched from, the audio files themselves.

## Context

We want users to apply free-form string **Tags** to **Tracks** for later search (`derpy --tags rock,dnb`). The obvious-looking design — write tags into the audio file's embedded metadata (ID3v2 for MP3, Vorbis Comments for FLAC) — has real costs that don't pay back for derpy's narrow use case (intra-derpy search only, no interoperability requirement).

## Decision

Tags live in two derpy-owned JSON files under `~/.config/derpy/`:

- `tags.json` — `{ "<sum>": ["rock", "dnb"], ... }`. The source of truth.
- `sum-cache.json` — `{ "<abs-path>": { "mtime":..., "size":..., "sum":"<sha1>" }, ... }`. A mtime+size-validated path-to-fingerprint cache.

The key is the audio fingerprint produced by `github.com/dhowden/tag`'s `Sum(io.ReadSeeker)` function: a SHA-1 of the audio data alone, with embedded metadata (ID3v2, ID3v1, MP4 atoms, FLAC blocks) explicitly skipped per-format. This means the fingerprint survives renames, moves, and embedded-metadata edits (e.g. Picard retagging), but does not survive re-encoding.

A background goroutine populates `sum-cache.json` at startup by walking the source directory; the TUI shows a `indexing N/M tracks…` status line while it runs. Playback is never blocked on hashing. The cache fills incrementally across launches.

## Why not the alternatives

- **In-file metadata (ID3v2 + Vorbis Comments).** Forces format-specific writer dependencies (`bogem/id3v2`, `go-flac/flacvorbis`), restricts the feature to MP3+FLAC (M4A/AAC/WAV/OGG would each need their own code path), introduces a "wait until playback ends before writing" complication, and carries a small but real risk of corrupting users' source files. The interoperability we'd gain (Plex, Picard reading the tags) isn't something we asked for.
- **Absolute path as the index key.** Free, but breaks on every rename or folder reorganisation. Users routinely retag their libraries.
- **`(artist, title, album, duration)` metadata tuple.** Free, but breaks the moment the user edits embedded metadata with Picard — the exact tool people use to tidy up libraries.
- **Full-file SHA-256.** Survives renames but breaks on any embedded-metadata edit (cover art, ID3 frame change) because those bytes change too. Worse than `tag.Sum`.
- **Chromaprint / AcoustID.** Also survives re-encoding, but requires the external `fpcalc` binary and adds significant complexity. The re-encoding case isn't a real user need for V1.

## Consequences

- A tagged file that is later **re-encoded** (e.g. ripping the same CD again to a different bitrate) is a *new* fingerprint and loses its tags. Acceptable for V1 — addressable later via an opt-in Chromaprint relinker.
- A tagged file that is **moved or renamed** keeps its tags transparently — the next background pass re-hashes the new path and the same `sum` re-links to the existing tag entry.
- A tagged file that is **deleted from disk** leaves an orphan `sum → tags` entry in `tags.json`. Acceptable — orphans are cheap and can be GC'd by a later command if needed.
- Tags do **not** travel with the file. Copying music to another machine without also copying `~/.config/derpy/tags.json` loses the tags. This is a deliberate non-goal.
- Other software (Plex, Foobar2000, etc.) cannot see derpy tags. Deliberate.
