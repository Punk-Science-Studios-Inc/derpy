package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"derpy/internal/filter"

	core "github.com/punkscience/earmark/earmark-core"
)

func main() {
	configureCore()
	if err := rootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

// argsToExpr converts positional CLI args into a keyword expression string.
//
// Multi-word args (e.g. `derpy "Front Line Assembly"` — quotes stripped by
// the shell) are wrapped as a quoted phrase so the parser matches the path
// substring literally. Args that already contain expression syntax
// (AND/OR/parens/quotes) are passed through unchanged. Single-word args are
// passed through. Multiple args are joined with OR.
func argsToExpr(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		if isPhraseArg(a) {
			parts[i] = `"` + a + `"`
		} else {
			parts[i] = a
		}
	}
	return strings.Join(parts, " OR ")
}

// isPhraseArg reports whether a single CLI arg should be wrapped as a
// quoted phrase before parsing. True when the arg contains whitespace but
// no expression syntax — meaning the user's quoting intent was lost to the
// shell.
func isPhraseArg(s string) bool {
	if strings.ContainsAny(s, `"()`) {
		return false
	}
	fields := strings.Fields(s)
	if len(fields) <= 1 {
		return false
	}
	for _, f := range fields {
		switch strings.ToUpper(f) {
		case "AND", "OR":
			return false
		}
	}
	return true
}


// rootCmd builds the top-level Cobra command for derpy.
func rootCmd() *cobra.Command {
	var noTUI bool
	var source string
	var setDefaultSource string

	cmd := &cobra.Command{
		Use:   "derpy [--source <dir>] [--set-default-source <dir>] [expression...]",
		Short: "Terminal music player — shuffles and plays a directory of audio files",
		Long: `derpy scans a directory recursively for audio files and plays them shuffled.

Keyword expression syntax (AND binds tighter than OR):
  jazz AND piano          path must contain both "jazz" and "piano"
  jazz OR blues           path must contain "jazz" or "blues"
  (jazz OR blues) AND piano
  "jazz piano"            exact phrase match

Multiple expression arguments are joined with OR:
  derpy jazz blues  →  jazz OR blues

A single argument containing spaces is treated as a phrase, since shells
strip the surrounding quotes before derpy sees them:
  derpy "Front Line Assembly"  →  "Front Line Assembly"

To use boolean operators inside a single argument, include AND/OR/parens
in the argument text:
  derpy "jazz AND piano"

Matching is case-insensitive against the full file path.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Handle --set-default-source before anything else.
			if setDefaultSource != "" {
				cfg, err := LoadConfig()
				if err != nil {
					cfg = &Config{}
				}
				cfg.DefaultSource = setDefaultSource
				if err := SaveConfig(cfg); err != nil {
					return fmt.Errorf("could not save config: %w", err)
				}
				path, _ := configFilePath()
				fmt.Printf("Default source set to %q in %s\n", setDefaultSource, path)
				return nil
			}

			// Resolve the music directory: flag > config default > error.
			musicDir := source
			if musicDir == "" {
				cfg, err := LoadConfig()
				if err == nil {
					musicDir = cfg.DefaultSource
				}
			}
			if musicDir == "" {
				return fmt.Errorf("no source directory specified — use --source <dir> or set a default with --set-default-source <dir>")
			}

			return runPlayer(musicDir, argsToExpr(args), noTUI)
		},
	}

	cmd.Flags().BoolVar(&noTUI, "no-tui", false, "Play without the terminal UI")
	cmd.Flags().StringVar(&source, "source", "", "Directory to scan for audio files")
	cmd.Flags().StringVar(&setDefaultSource, "set-default-source", "", "Save a default source directory to the config and exit")

	// Subcommands
	cmd.AddCommand(versionCmd())
	cmd.AddCommand(tokenCmd())
	cmd.AddCommand(nostrKeyCmd())
	cmd.AddCommand(blueskyKeyCmd())
	cmd.AddCommand(listCmd())
	cmd.AddCommand(relayCmd())
	cmd.AddCommand(blossomCmd())
	cmd.AddCommand(earmarksCmd())
	cmd.AddCommand(sonicCmd())

	return cmd
}

// versionCmd returns the 'version' subcommand that prints the derpy version
// string (injected at build time via ldflags, defaulting to "dev").
func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the derpy version",
		Long:  `Print the derpy version string. The default "dev" value means this binary was built without goreleaser ldflags injection.`,
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Println(Version)
		},
	}
}

// tokenCmd returns the 'token' subcommand that stores a ListenBrainz token.
func tokenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "token <listenbrainz_token>",
		Short: "Save a ListenBrainz user token to the config file",
		Long: `Save your ListenBrainz user token to ~/.config/derpy/config.json.

The token is read from the config file on startup (before checking the
LISTENBRAINZ_TOKEN environment variable). Pass an empty string to clear it:

  derpy token ""`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			token := args[0]

			cfg, err := LoadConfig()
			if err != nil {
				// Non-fatal: start with a blank config
				cfg = &Config{}
			}

			cfg.ListenBrainzToken = token
			if err := SaveConfig(cfg); err != nil {
				return fmt.Errorf("could not save config: %w", err)
			}

			if token == "" {
				fmt.Println("ListenBrainz token cleared.")
			} else {
				path, _ := configFilePath()
				fmt.Printf("ListenBrainz token saved to %s\n", path)
			}
			return nil
		},
	}
}

// nostrKeyCmd returns the 'nostr-key' subcommand that stores a Nostr private key.
func nostrKeyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "nostr-key <nsec_or_hex_key>",
		Short: "Save a Nostr private key to the config file",
		Long: `Save your Nostr private key to ~/.config/derpy/config.json.

The key is used to sign and publish track-earmark notes to public Nostr relays
when you press [E] while a track is playing.

You can pass either a bech32-encoded nsec key (nsec1...) or a raw 64-character
hex string.  The key is stored as hex internally.

IMPORTANT: Your private key is a secret — anyone who has it can post as you.
The config file is stored with 0600 permissions, but treat it like a password.

Pass an empty string to clear the saved key:

  derpy nostr-key ""`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rawInput := args[0]

			cfg, err := LoadConfig()
			if err != nil {
				cfg = &Config{}
			}

			if rawInput == "" {
				cfg.NostrPrivateKey = ""
				if err := SaveConfig(cfg); err != nil {
					return fmt.Errorf("could not save config: %w", err)
				}
				fmt.Println("Nostr private key cleared.")
				return nil
			}

			// Validate and normalise to hex before storing.
			hexKey, err := core.ResolvePrivateKey(rawInput)
			if err != nil {
				return fmt.Errorf("invalid key: %w", err)
			}

			cfg.NostrPrivateKey = hexKey
			if err := SaveConfig(cfg); err != nil {
				return fmt.Errorf("could not save config: %w", err)
			}
			path, _ := configFilePath()
			fmt.Printf("Nostr private key saved to %s\n", path)
			fmt.Println("Press [E] in derpy to earmark the current track on Nostr.")
			return nil
		},
	}
}

// blueskyKeyCmd returns the 'bluesky-key' subcommand that stores Bluesky
// credentials (handle + app-specific password) in the config file.
func blueskyKeyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "bluesky-key <handle> <app-password>",
		Short: "Save Bluesky credentials to the config file",
		Long: `Save your Bluesky handle and app-specific password to ~/.config/derpy/config.json.

Credentials are used to post about the current track when you press [P] in the
TUI. Create an app password in Bluesky Settings → Privacy and Security → App
Passwords.

Pass empty strings to clear the saved credentials:

  derpy bluesky-key "" ""`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			handle := args[0]
			appPassword := args[1]

			cfg, err := LoadConfig()
			if err != nil {
				cfg = &Config{}
			}

			if handle == "" && appPassword == "" {
				cfg.BlueskyHandle = ""
				cfg.BlueskyAppPassword = ""
				if err := SaveConfig(cfg); err != nil {
					return fmt.Errorf("could not save config: %w", err)
				}
				fmt.Println("Bluesky credentials cleared.")
				return nil
			}

			if handle == "" || appPassword == "" {
				return fmt.Errorf("both handle and app-password are required (or both empty to clear)")
			}

			cfg.BlueskyHandle = handle
			cfg.BlueskyAppPassword = appPassword
			if err := SaveConfig(cfg); err != nil {
				return fmt.Errorf("could not save config: %w", err)
			}
			path, _ := configFilePath()
			fmt.Printf("Bluesky credentials saved to %s\n", path)
			fmt.Println("Press [P] in derpy to post the current track to Bluesky.")
			return nil
		},
	}
}

// runPlayer scans the directory, optionally filters by a keyword expression,
// shuffles, and starts playback. exprStr may be empty to play everything.
func runPlayer(musicDir string, exprStr string, noTUI bool) error {
	if _, err := os.Stat(musicDir); os.IsNotExist(err) {
		return fmt.Errorf("directory does not exist: %s", musicDir)
	}

	fmt.Printf("Scanning source directory: %s\n", musicDir)
	fmt.Println("If startup appears stuck here, verify the source path is reachable (network/removable drives can block scans).")
	playlist, err := scanMusicDirectory(musicDir, exprStr)
	if err != nil {
		return fmt.Errorf("error scanning directory: %w", err)
	}
	fmt.Printf("Scan complete. Found %d audio file(s).\n", len(playlist))
	if len(playlist) == 0 {
		if exprStr != "" {
			return fmt.Errorf("no audio files matching %q found in directory: %s", exprStr, musicDir)
		}
		return fmt.Errorf("no audio files found in directory: %s", musicDir)
	}


	// Always shuffle by default
	shufflePlaylist(playlist)

	if noTUI {
		return runNoTUI(playlist)
	}
	return runTUI(playlist)
}

// runNoTUI plays the playlist sequentially without a terminal UI.
func runNoTUI(playlist []string) error {
	player := NewAudioPlayer()
	defer player.Shutdown()

	for i, track := range playlist {
		fmt.Printf("Playing: %s\n", track)

		if err := player.LoadTrack(track); err != nil {
			fmt.Fprintf(os.Stderr, "Error loading track %s: %v\n", track, err)
			_ = logTrackError(fmt.Errorf("failed to load track %q: %w", track, err))
			continue
		}
		if err := player.Play(); err != nil {
			fmt.Fprintf(os.Stderr, "Error playing track %s: %v\n", track, err)
			_ = logTrackError(fmt.Errorf("failed to play track %q: %w", track, err))
			continue
		}
		if i+1 < len(playlist) {
			prefetchEntry(playlist[i+1])
		}

		for !player.HasEnded() {
			// Poll position so that ListenBrainz scrobbling fires at 25%.
			_ = player.GetPosition()
			time.Sleep(100 * time.Millisecond)
		}
		player.Stop()
	}

	fmt.Println("Playlist finished.")
	return nil
}

// runTUI starts the Bubble Tea TUI player and, if the session D-Bus is
// available, registers an MPRIS2 service so external clients (playerctl,
// Waybar, etc.) can query and control playback.
func runTUI(playlist []string) error {
	model := NewPlayerModel(playlist)
	program := tea.NewProgram(model, tea.WithAltScreen())

	// Create the MPRIS service before starting the program so the D-Bus name
	// is registered before the first PropertiesChanged signal fires.
	mpris, err := NewMPRISService(program)
	if err != nil {
		// Non-fatal: log and continue without MPRIS.
		fmt.Fprintf(os.Stderr, "MPRIS: failed to start D-Bus service: %v\n", err)
	}
	// Give the model a reference so it can emit state-change signals.
	model.mpris = mpris
	defer mpris.Close()

	if _, err := program.Run(); err != nil {
		return fmt.Errorf("error running TUI: %w", err)
	}
	return nil
}

// scanMusicDirectory recursively scans root for audio files. If exprStr is
// non-empty it is parsed as a boolean keyword expression (AND/OR/parentheses)
// and only matching paths are returned. Matching is case-insensitive against
// the full absolute path.
func scanMusicDirectory(root string, exprStr string) ([]string, error) {
	audioExts := map[string]bool{
		".mp3":  true,
		".wav":  true,
		".flac": true,
		".ogg":  true,
		".m4a":  true,
		".aac":  true,
	}

	// Parse the expression once before the walk; fail fast on syntax errors.
	var expr filter.Expr
	if exprStr != "" {
		var err error
		expr, err = filter.ParseExpr(exprStr)
		if err != nil {
			return nil, fmt.Errorf("invalid keyword expression: %w", err)
		}
	}

	var all []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if audioExts[strings.ToLower(filepath.Ext(path))] {
			all = append(all, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if expr == nil {
		return all, nil
	}
	return filter.FilterExpr(all, expr), nil
}

// listCmd returns the 'list' subcommand that prints the private earmark list.
func listCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Print your private Nostr earmark list",
		Long: `Fetch and decrypt your private earmark list from Nostr relays.

Requires a Nostr private key saved via 'derpy nostr-key <key>'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			hexKey := resolveNostrKey()
			if hexKey == "" {
				return fmt.Errorf("no Nostr private key configured — run: derpy nostr-key <nsec_or_hex_key>")
			}

			fmt.Fprintln(cmd.OutOrStdout(), "Fetching earmarks from Nostr...")

			earmarks, err := core.FetchEarmarks(hexKey)
			if err != nil {
				return fmt.Errorf("could not fetch earmarks: %w", err)
			}

			if len(earmarks) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No earmarks found. Press [E] while a track is playing to add one.")
				return nil
			}


			fmt.Fprintf(cmd.OutOrStdout(), "\n%d earmark(s):\n\n", len(earmarks))
			for i, e := range earmarks {
				ts := time.Unix(e.Timestamp, 0).Format("2006-01-02 15:04")
				var desc string
				switch {
				case e.Artist != "" && e.Album != "" && e.Title != "":
					desc = fmt.Sprintf("%s — %s — %s", e.Artist, e.Album, e.Title)
				case e.Artist != "" && e.Title != "":
					desc = fmt.Sprintf("%s — %s", e.Artist, e.Title)
				case e.Title != "":
					desc = e.Title
				default:
					desc = "(unknown track)"
				}
				pathInfo := ""
				if e.Path != "" {
					pathInfo = "\n        " + e.Path
				}
				fmt.Fprintf(cmd.OutOrStdout(), " %3d.  %-60s  (%s)%s\n", i+1, desc, ts, pathInfo)
			}
			return nil
		},
	}
}

// relayCmd returns the 'relay' subcommand group for managing Nostr relays.
func relayCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "relay",
		Short: "Manage the Nostr relay list",
		Long: `Manage the list of Nostr relay WebSocket URLs that derpy publishes to
and fetches from.

When no relays are configured the built-in defaults are used. Adding even one
relay that you know accepts your pubkey is usually enough.`,
	}
	cmd.AddCommand(relayListCmd(), relayAddCmd(), relayRemoveCmd(), relayResetCmd())
	return cmd
}

func relayListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show the active relay list",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _ := LoadConfig()
			relays := LoadNostrRelays()
			if len(cfg.NostrRelays) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(using built-in defaults)")
			}
			for _, r := range relays {
				fmt.Fprintln(cmd.OutOrStdout(), r)
			}
			return nil
		},
	}
}

func relayAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add <wss://relay.example.com>",
		Short: "Add a relay to the list",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			url := args[0]
			if !strings.HasPrefix(url, "wss://") && !strings.HasPrefix(url, "ws://") {
				return fmt.Errorf("relay URL must start with wss:// or ws://")
			}
			cfg, err := LoadConfig()
			if err != nil {
				cfg = &Config{}
			}
			// Start from defaults if this is the first custom relay.
			if len(cfg.NostrRelays) == 0 {
				cfg.NostrRelays = append([]string{}, core.DefaultRelays()...)
			}
			for _, r := range cfg.NostrRelays {
				if r == url {
					fmt.Fprintf(cmd.OutOrStdout(), "%s is already in the relay list\n", url)
					return nil
				}
			}
			cfg.NostrRelays = append(cfg.NostrRelays, url)
			if err := SaveConfig(cfg); err != nil {
				return fmt.Errorf("could not save config: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Added %s\n", url)
			return nil
		},
	}
}

func relayRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <wss://relay.example.com>",
		Short: "Remove a relay from the list",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			url := args[0]
			cfg, err := LoadConfig()
			if err != nil {
				cfg = &Config{}
			}
			// Operate on defaults if nothing custom is saved yet.
			if len(cfg.NostrRelays) == 0 {
				cfg.NostrRelays = append([]string{}, core.DefaultRelays()...)
			}
			filtered := cfg.NostrRelays[:0]
			found := false
			for _, r := range cfg.NostrRelays {
				if r == url {
					found = true
				} else {
					filtered = append(filtered, r)
				}
			}
			if !found {
				return fmt.Errorf("%s is not in the relay list", url)
			}
			cfg.NostrRelays = filtered
			if err := SaveConfig(cfg); err != nil {
				return fmt.Errorf("could not save config: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed %s\n", url)
			return nil
		},
	}
}

func relayResetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reset",
		Short: "Reset the relay list to built-in defaults",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := LoadConfig()
			if err != nil {
				cfg = &Config{}
			}
			cfg.NostrRelays = nil // nil → LoadNostrRelays() returns defaults
			if err := SaveConfig(cfg); err != nil {
				return fmt.Errorf("could not save config: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Relay list reset to defaults:")
			for _, r := range core.DefaultRelays() {
				fmt.Fprintln(cmd.OutOrStdout(), " ", r)
			}
			return nil
		},
	}
}

// blossomCmd returns the 'blossom' subcommand group for managing Blossom servers.
func blossomCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "blossom",
		Short: "Manage the Blossom audio server list",
		Long: `Manage the list of Blossom server URLs used for uploading and downloading
encrypted audio chunks.

When no servers are configured the built-in defaults are used. At least two
servers are recommended so each chunk has redundancy.`,
	}
	cmd.AddCommand(blossomListCmd(), blossomAddCmd(), blossomRemoveCmd(), blossomResetCmd())
	return cmd
}

func blossomListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show the active Blossom server list",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _ := LoadConfig()
			servers := core.BlossomServers()
			if len(cfg.BlossomServers) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(using built-in defaults)")
			}
			for _, s := range servers {
				fmt.Fprintln(cmd.OutOrStdout(), s)
			}
			return nil
		},
	}
}

func blossomAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add <https://server.example.com>",
		Short: "Add a Blossom server to the list",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			url := args[0]
			if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
				return fmt.Errorf("server URL must start with https:// or http://")
			}
			cfg, err := LoadConfig()
			if err != nil {
				cfg = &Config{}
			}
			if len(cfg.BlossomServers) == 0 {
				cfg.BlossomServers = append([]string{}, core.DefaultBlossomServers()...)
			}
			for _, s := range cfg.BlossomServers {
				if s == url {
					fmt.Fprintf(cmd.OutOrStdout(), "%s is already in the server list\n", url)
					return nil
				}
			}
			cfg.BlossomServers = append(cfg.BlossomServers, url)
			if err := SaveConfig(cfg); err != nil {
				return fmt.Errorf("could not save config: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Added %s\n", url)
			return nil
		},
	}
}

func blossomRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <https://server.example.com>",
		Short: "Remove a Blossom server from the list",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			url := args[0]
			cfg, err := LoadConfig()
			if err != nil {
				cfg = &Config{}
			}
			if len(cfg.BlossomServers) == 0 {
				cfg.BlossomServers = append([]string{}, core.DefaultBlossomServers()...)
			}
			filtered := cfg.BlossomServers[:0]
			found := false
			for _, s := range cfg.BlossomServers {
				if s == url {
					found = true
				} else {
					filtered = append(filtered, s)
				}
			}
			if !found {
				return fmt.Errorf("%s is not in the server list", url)
			}
			cfg.BlossomServers = filtered
			if err := SaveConfig(cfg); err != nil {
				return fmt.Errorf("could not save config: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed %s\n", url)
			return nil
		},
	}
}

func blossomResetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reset",
		Short: "Reset the Blossom server list to built-in defaults",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := LoadConfig()
			if err != nil {
				cfg = &Config{}
			}
			cfg.BlossomServers = nil
			if err := SaveConfig(cfg); err != nil {
				return fmt.Errorf("could not save config: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Blossom server list reset to defaults:")
			for _, s := range core.DefaultBlossomServers() {
				fmt.Fprintln(cmd.OutOrStdout(), " ", s)
			}
			return nil
		},
	}
}

// earmarksCmd returns the 'earmarks' subcommand that plays earmarked files
// as a playlist.
//
// Local files are added to the playlist immediately. Files that only exist as
// Blossom-uploaded chunks are downloaded in parallel before playback begins;
// a progress summary is printed to stdout while waiting.
func earmarksCmd() *cobra.Command {
	var noTUI bool
	var nuke bool

	cmd := &cobra.Command{
		Use:   "earmarks",
		Short: "Play your earmarked tracks as a playlist",
		Long: `Fetch your private Nostr earmark list and play the files as a playlist.

Tracks are played in the order they were earmarked. Files that exist on disk
are used directly. Files that were uploaded to Blossom servers are downloaded,
decrypted, and reassembled in parallel before playback begins.

Use --nuke to delete all Blossom chunks and wipe the earmark list entirely.

Requires a Nostr private key saved via 'derpy nostr-key <key>'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			hexKey := resolveNostrKey()
			if hexKey == "" {
				return fmt.Errorf("no Nostr private key configured — run: derpy nostr-key <nsec_or_hex_key>")
			}

			if nuke {
				fmt.Fprintln(cmd.OutOrStdout(), "Fetching earmarks...")
				n, err := core.NukeEarmarks(hexKey)
				if err != nil {
					return fmt.Errorf("nuke failed: %w", err)
				}
				if n == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "Nothing to nuke — earmark list was already empty.")
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "Nuked %d earmark(s) and their Blossom chunks.\n", n)
				}
				return nil
			}

			fmt.Fprintln(cmd.OutOrStdout(), "Fetching earmarks from Nostr...")

			earmarks, err := core.FetchEarmarks(hexKey)
			if err != nil {
				return fmt.Errorf("could not fetch earmarks: %w", err)
			}
			if len(earmarks) == 0 {
				return fmt.Errorf("no earmarks found — press [E] while a track is playing to add one")
			}

			// Separate earmarks into local files and those requiring download.
			// playlist is pre-sized so each goroutine can write to its own index.
			playlist := make([]string, len(earmarks))
			isTemp := make([]bool, len(earmarks))

			type dlWork struct {
				idx     int
				earmark core.Earmark
			}
			var work []dlWork

			for i, e := range earmarks {
				if e.Path != "" {
					if _, err := os.Stat(e.Path); err == nil {
						playlist[i] = e.Path
						continue
					}
				}
				if e.Blossom != nil {
					work = append(work, dlWork{i, e})
					continue
				}
				// No local file and no Blossom manifest — will be filtered out below.
			}

			// Download all Blossom tracks in parallel, collecting results.
			if len(work) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "Downloading %d Blossom track(s)...\n", len(work))

				type dlResult struct {
					idx  int
					path string
					err  error
				}
				results := make(chan dlResult, len(work))

				for _, w := range work {
					w := w
					go func() {
						dlCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
						defer cancel()
						path, err := core.DownloadAndReassemble(dlCtx, w.earmark.Blossom, hexKey, nil)
						results <- dlResult{w.idx, path, err}
					}()
				}

				done := 0
				for range work {
					r := <-results
					done++
					if r.err != nil {
						e := earmarks[r.idx]
						desc := e.Title
						if desc == "" {
							desc = filepath.Base(e.Path)
						}
						fmt.Fprintf(cmd.ErrOrStderr(), "  [%d/%d] failed: %s — %v\n",
							done, len(work), desc, r.err)
					} else {
						playlist[r.idx] = r.path
						isTemp[r.idx] = true
						fmt.Fprintf(cmd.OutOrStdout(), "  [%d/%d] ready\n", done, len(work))
					}
				}
			}

			// Compact: filter out empty slots (missing files) preserving order.
			var finalPlaylist []string
			var tempFiles []string
			skipped := 0
			for i, p := range playlist {
				if p == "" {
					skipped++
					continue
				}
				finalPlaylist = append(finalPlaylist, p)
				if isTemp[i] {
					tempFiles = append(tempFiles, p)
				}
			}

			// Temp files are cleaned up when the player exits.
			defer func() {
				for _, f := range tempFiles {
					os.Remove(f)
				}
			}()

			if len(finalPlaylist) == 0 {
				msg := "no earmarked files available for playback"
				if skipped > 0 {
					msg += fmt.Sprintf(" (%d skipped — file not found and no Blossom upload)", skipped)
				}
				return fmt.Errorf("%s", msg)
			}

			if skipped > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "Playing %d track(s), %d skipped.\n\n", len(finalPlaylist), skipped)
			}


			if noTUI {
				return runNoTUI(finalPlaylist)
			}
			return runTUI(finalPlaylist)
		},
	}

	cmd.Flags().BoolVar(&noTUI, "no-tui", false, "Play without the terminal UI")
	cmd.Flags().BoolVar(&nuke, "nuke", false, "Delete all Blossom chunks and wipe the earmark list")
	return cmd
}
