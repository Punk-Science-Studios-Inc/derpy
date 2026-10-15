# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

**derpy** is a terminal-based music player in Go. It recursively scans a directory for audio files, shuffles them, and plays them with a Bubble Tea TUI. ListenBrainz scrobbling is optional via `LISTENBRAINZ_TOKEN` env var.

## Build & Run

```bash
# No system dev headers required — PulseAudio/PipeWire is used at runtime
go mod tidy
go build -o derpy

# Run
./derpy <music_directory>
./derpy --no-tui <music_directory>
```

## Architecture

### The earmark protocol is not in this repo

Everything to do with earmarks — AES-256-GCM chunking, Blossom upload/download/mirror, the NIP-44 self-encrypted Nostr list, and channels — lives in **`github.com/punkscience/earmark/earmark-core`**, shared with the [earmark CLI](https://github.com/punkscience/earmark). derpy imports it as `core`.

`blossom.go`, `nostr_list.go` and most of `nostr.go` used to live here and were deleted when the core was extracted. **Do not reintroduce protocol code in this repo** — a second implementation is exactly the drift the extraction was done to prevent. Read `earmark-core/AGENTS.md` before changing anything protocol-shaped, and `docs/PROTOCOL.md` in that repo for the wire format.

The core reads no config files. `configureCore()` in `config.go` pushes derpy's relay and Blossom server lists into it; it runs at startup in `main()` and again from `SaveConfig`. A new core setting that is not added to `configureCore` silently does nothing.

> **Versioned dependency.** derpy consumes tagged releases of the core (`earmark-core/vX.Y.Z` tags on the earmark repo), currently v0.1.0. It builds standalone — no sibling checkout needed. To develop against unreleased core changes, add a temporary `replace => ../earmark/earmark-core` and drop it before committing.

| File | Role |
|------|------|
| `main.go` | CLI args, recursive directory scan, playlist init, TUI/non-TUI dispatch |
| `model.go` | Bubble Tea model — keyboard input, progress bar rendering, note-saving to `~/track-notes.md` |
| `audio.go` | `AudioPlayer` — Beep-based decoding/playback, pause/resume, position tracking via `CompletionStreamer`. Uses `speaker.go` functions instead of `gopxl/beep/speaker`. |
| `speaker.go` | Custom PulseAudio speaker backed by `jfreymuth/pulse` (pure Go, no ALSA headers). Exposes `speakerInit/Play/Clear/Lock/Unlock` used by `audio.go`. |
| `mpris.go` / `mpris_windows.go` / `mpris_darwin.go` | OS media-control integration behind one `MPRISService` API (build-tagged per OS). Linux = MPRIS2 D-Bus (`mpris.go`); Windows = System Media Transport Controls via a WinRT MediaPlayer-owned SMTC (`mpris_windows.go`); macOS = no-op stub. `mpris_types.go` holds the platform-neutral `mprisXxxMsg` command messages consumed by `model.go`. |
| `listenbrainz.go` | Optional scrobbling — submits "playing now" immediately, scrobbles at 25% completion (min 30s tracks) |
| `shuffle.go` | Fisher-Yates shuffle seeded with current time |
| `subsonic.go` | Subsonic API client (token auth). Lists the library via paged `search3`, downloads each track to a session temp dir on demand, and prefetches the next server track in the background once the current one starts (one prefetch slot; a new one discards the old). Playlist entries for server tracks are `subsonic:<id>`; `AudioPlayer.LoadTrack` resolves them through the package-level `subsonicSession`. |
| `sonic.go` | `derpy sonic` command — prompts for and saves server URL/user/password on first run, then plays the server library like a local directory. `[D]` is a no-op for server tracks. `--favourites` plays only favourites (`getStarred2`); `[F]` toggles the current server track via `star`/`unstar`. derpy says "favourite" and shows ♥ where Subsonic says "starred". |


**Audio formats:** mp3, wav, flac, ogg, m4a, aac
**Key libraries:** `charmbracelet/bubbletea`, `gopxl/beep`, `dhowden/tag` (metadata), `hirigaray/go-listenbrainz`

**TUI:** 100ms tick interval for progress updates. Position is calculated from elapsed wall-clock time with pause offset.

**Keyboard controls:** `←`/`→` prev/next track, `SPACE` pause/resume, `[E]` earmark, `[C]` channels, `[P]` post, `[D]` delete, `[F]` favourite (Sonic tracks), `ESC`/`q` quit.

### Channels

A channel is a room of Nostr identities sharing earmarks. The full contract is the Channels section of `docs/PROTOCOL.md` in the earmark repo. What matters here:

- **`[E]` opens a target picker** when the user is in any channels, with personal pre-selected — so the pre-channels flow is still `[E]` then enter. Personal is one toggle among the targets: untick it to share to channels only. At least one target must stay selected. Channel state is loaded once in the background at startup so `[E]` never blocks on a relay.
- **`[C]` browses the channel feed** and plays a selected post inline: downloaded, decrypted, and slotted in after the current track. Listening is not keeping — adopting a post into your own stash is `earmark channel keep` in the CLI.
- **Posting shares nothing new.** The post hands members the existing chunk hashes and file key, so it costs one small event per member and zero bytes.
- **There is no backfill**, and the empty-feed message says so. A newly joined channel being empty is correct, not broken.

**Tags:** Removed. `docs/adr/0001-tags-external-index.md` records the old external-index design (superseded). Tags may return only if they can be stored in, and searched from, the audio files themselves.

## Development Directives

- Use SOLID principles
- Comment code well
- Always write tests (`*_test.go` files) — the project currently has none and needs them
- Refer to `.copilot/tech-spec.md` for feature tracking; mark features complete when done
- Use Cobra for any new CLI commands (currently uses `flag` package — prefer Cobra per project rules)

## Agent skills

### Issue tracker

Issues live in GitHub Issues on `Punk-Science-Studios-Inc/derpy` via the `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

Five canonical labels: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context layout — one `CONTEXT.md` + `docs/adr/` at the repo root (created lazily by `/grill-with-docs`). See `docs/agents/domain.md`.
