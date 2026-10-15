# derpy

A terminal music player with an opinion: shuffle everything, earmark what's hitting right, let the list expire.

## The idea

Most music software asks you to curate — to build playlists deliberately, to rate things, to organise. derpy takes the opposite approach. You point it at a directory, it shuffles everything, and you just listen. No queue management. No star ratings. No decisions.

When something stops you mid-listen — when a track is exactly right for the moment — you press `E` to earmark it. That's the only act of curation derpy asks of you.

Earmarks expire after 30 days. This is intentional. The list isn't a permanent archive; it's a snapshot of what's resonating *right now*. As your mood and context shift, the list empties and refills with something new. You're forced to keep discovering rather than retreating to the same familiar tracks.

## What it does

- Recursively scans a directory for audio files and shuffles them
- Minimal TUI: current track, progress bar, nothing else
- Filter playback by keyword expression: `derpy jazz AND piano`, `derpy "blue note" OR prestige`
- `[E]` earmarks the current track — encrypts it, uploads it to Blossom servers, and saves the manifest to a private Nostr list that follows you across devices. If you are in any channels, `[E]` opens a target picker: personal and each channel are toggles, so a track can go to your personal list, to channels, or both.
- `[C]` browses your channels — tracks friends have shared with you, playable inline
- `[P]` publishes a public Nostr note and a Bluesky note about what you're listening to, with a Bandcamp or YouTube link if one can be found
- `[D]` deletes the current file from disk
- `derpy sonic` shuffles and plays the library of a Subsonic-compatible server (Navidrome, Gonic, Airsonic) instead of a local directory
- `[F]` adds the current Sonic track to your server favourites, or removes it (shown with a ♥); `derpy sonic --favourites` plays only your favourites
- `derpy earmarks` plays your earmarked tracks as a playlist — using the local file if it exists, downloading and decrypting from Blossom if not
- Earmarks older than 30 days are automatically purged on startup, along with their uploaded audio chunks

## Installation

### One-liner (all platforms)

```bash
curl -fsSL https://punk-science-studios-inc.github.io/derpy/install.sh | bash
```

The script detects your OS and walks you through the right install.

### Per package manager

#### Linux (Debian/Ubuntu) — APT

```bash
# Add the GPG key and APT source, then install
curl -fsSL https://punk-science-studios-inc.github.io/derpy/apt/derpy-archive-keyring.gpg | sudo tee /usr/share/keyrings/derpy-archive-keyring.gpg
echo 'deb [signed-by=/usr/share/keyrings/derpy-archive-keyring.gpg] https://punk-science-studios-inc.github.io/derpy/apt/ stable main' | sudo tee /etc/apt/sources.list.d/derpy.list
sudo apt update && sudo apt install derpy
```

Supports **amd64** and **arm64**. The APT repo is GPG-signed and hosted on GitHub Pages.

#### macOS — Homebrew

```bash
brew install punkscience/homebrew-derpy/derpy
```

Supports **Intel** and **Apple Silicon**. Future releases auto-update the formula via goreleaser.

#### Windows

**Via Chocolatey** (submitted, pending moderation):

```powershell
choco install derpy
```

**Direct download** (available immediately):

```powershell
# amd64 (most systems)
Invoke-WebRequest -Uri https://github.com/Punk-Science-Studios-Inc/derpy/releases/latest/download/derpy_windows_amd64.zip -OutFile derpy.zip
Expand-Archive derpy.zip -DestinationPath .
.\derpy.exe --help

# arm64
Invoke-WebRequest -Uri https://github.com/Punk-Science-Studios-Inc/derpy/releases/latest/download/derpy_windows_arm64.zip -OutFile derpy.zip
```

### Build from source

Requires Go 1.21+.

```bash
git clone https://github.com/Punk-Science-Studios-Inc/derpy
cd derpy
go build -o derpy
```

## Usage

```bash
# Play everything in a directory, shuffled
derpy --source ~/Music

# Save a default source so you don't have to type it
derpy --set-default-source ~/Music
derpy

# Filter by keyword expression
derpy jazz AND piano
derpy "kind of blue" OR "a love supreme"
derpy (jazz OR blues) AND live

# Play from a Subsonic server. The first run asks for the URL, username and
# password, checks them, and saves them for later runs.
derpy sonic
derpy sonic jazz AND piano
derpy sonic --favourites                                   # only your favourites
derpy sonic --server https://music.example.com --user me   # switch server/account

# Play your earmarked tracks
derpy earmarks

# Print the earmark list
derpy list
```

## Controls

| Key | Action |
|-----|--------|
| `←` | Previous track |
| `→` | Next track |
| `SPACE` | Pause / Resume |
| `E` | Earmark current track |
| `P` | Post to Nostr |
| `D` | Delete current file from disk |
| `F` | Add/remove the current Sonic track from your favourites |
| `ESC` / `q` | Quit |

## Nostr setup

Earmarks and public posts require a Nostr private key. Everything earmarked is NIP-44 encrypted — relays store only ciphertext. The key never leaves your machine unencrypted.

```bash
# Save your key (accepts nsec1... bech32 or raw hex)
derpy nostr-key nsec1...

# The app will also prompt for it inline the first time you press [E] or [P]
```

## Blossom setup

When you earmark a track, derpy encrypts the audio file (AES-256-GCM) and uploads it to Blossom servers in 16 MB chunks. This means `derpy earmarks` works on any machine with the same Nostr key — even one that has never seen the original files.

The default servers are `blossom.band`, `cdn.satellite.earth`, and `nostr.build`. You can manage the list:

```bash
derpy blossom list
derpy blossom add https://your.server.com
derpy blossom remove https://blossom.band
derpy blossom reset
```

If you have a kind-10063 Nostr event advertising your preferred Blossom servers, derpy will discover and use those automatically.

## Relay setup

```bash
derpy relay list
derpy relay add wss://relay.example.com
derpy relay remove wss://relay.damus.io
derpy relay reset
```

Defaults: `relay.damus.io`, `nos.lol`, `relay.primal.net`, `nostr.wine`

## ListenBrainz

```bash
derpy token your_listenbrainz_token
# or export LISTENBRAINZ_TOKEN=...
```

Scrobbles at 25% completion for tracks longer than 30 seconds.

## Supported formats

MP3, FLAC, WAV, OGG, M4A, AAC

## Technical notes

- Audio via [gopxl/beep](https://github.com/gopxl/beep) + [jfreymuth/pulse](https://github.com/jfreymuth/pulse) (PulseAudio/PipeWire, pure Go)
- TUI via [charmbracelet/bubbletea](https://github.com/charmbracelet/bubbletea)
- MPRIS2 D-Bus service on Linux — works with `playerctl`, Waybar, and anything else that speaks MPRIS
- Nostr via [nbd-wtf/go-nostr](https://github.com/nbd-wtf/go-nostr), NIP-44 encryption, NIP-51 private lists, NIP-65 relay discovery
- Blossom: BUD-01/BUD-02 blob storage, BUD-11 Nostr keypair auth, kind-10063 server discovery
- Config at `~/.config/derpy/config.json` (0600 permissions)
- Offline queue at `~/.config/derpy/queue.json` — earmarks survive without internet and sync when connectivity returns
