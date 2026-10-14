# derpy

Terminal music player whose opinion is: shuffle everything, mark what's resonating, let the marks expire. This document is the glossary — implementation details live in code and ADRs.

## Language

**Track**:
A single audio file (mp3, flac, wav, ogg, m4a, aac) on disk that derpy can play.
_Avoid_: Song, file (when referring to playable content)

**Earmark**:
A user-flagged Track that "hit right" in the moment. Expires after 30 days. Encrypted, uploaded to Blossom, and recorded in a private Nostr list that follows the user's identity across devices.
_Avoid_: Favourite, bookmark, like

**Channel** — A named room of Nostr identities who share Earmarks with each
other. Membership is set by the channel's creator. Every message is NIP-59 gift
wrapped, so no relay can tell a channel exists. A channel post hands members the
file key for a copy already on Blossom; nothing is re-uploaded, and posts stay
playable for 30 days. There is no backfill: joining shows you only what is
posted afterwards. Defined in `docs/PROTOCOL.md` in the earmark repo.
