package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"derpy/internal/filter"
)

// sonicCmd returns the 'sonic' subcommand, which plays a Subsonic server's
// library shuffled, the same way the root command plays a local directory.
func sonicCmd() *cobra.Command {
	var noTUI bool
	var server string
	var user string

	cmd := &cobra.Command{
		Use:   "sonic [--server <url>] [--user <name>] [expression...]",
		Short: "Shuffle and play music from a Subsonic server",
		Long: `Play the library of a Subsonic-compatible server (Navidrome, Gonic,
Airsonic, ...) shuffled, exactly like derpy plays a local directory.

The first run asks for the server URL, username and password, checks them
against the server, and saves them to ~/.config/derpy/config.json. Later runs
connect without asking. Pass --server or --user to switch to a different
server or account; you are asked for the password again and the new details
are saved.

Keyword expressions work as they do for local playback, matched against the
server's file path for each song (or "artist/album/title" when the server
does not report paths):
  derpy sonic jazz AND piano

Tracks are downloaded one at a time as they play and deleted afterwards.
Formats derpy cannot decode are transcoded to mp3 by the server. [D] does
nothing for server tracks.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := LoadConfig()
			if err != nil {
				cfg = &Config{}
			}

			in := bufio.NewReader(os.Stdin)
			p := sonicPrompter{
				in:           in,
				out:          cmd.OutOrStdout(),
				readPassword: func() (string, error) { return readTerminalPassword(in) },
			}
			details, err := resolveSonicDetails(cfg, server, user, p)
			if err != nil {
				return err
			}

			client := NewSubsonicClient(details.server, details.user, details.password)
			fmt.Fprintf(cmd.OutOrStdout(), "Connecting to %s...\n", client.baseURL)
			if err := client.Ping(); err != nil {
				return fmt.Errorf("could not connect to %s: %w", client.baseURL, err)
			}

			// Save only after the server accepted the details, so a typo is
			// never remembered.
			if details.changed {
				cfg.SubsonicServer = client.baseURL
				cfg.SubsonicUser = details.user
				cfg.SubsonicPassword = details.password
				if err := SaveConfig(cfg); err != nil {
					return fmt.Errorf("could not save config: %w", err)
				}
				path, _ := configFilePath()
				fmt.Fprintf(cmd.OutOrStdout(), "Subsonic server saved to %s\n", path)
			}

			subsonicSession = client
			defer func() {
				client.Close()
				subsonicSession = nil
			}()

			fmt.Fprintln(cmd.OutOrStdout(), "Fetching library...")
			songs, err := client.Songs(func(n int) {
				fmt.Fprintf(cmd.OutOrStdout(), "\r  %d song(s)", n)
			})
			fmt.Fprintln(cmd.OutOrStdout())
			if err != nil {
				return fmt.Errorf("could not fetch library: %w", err)
			}

			exprStr := argsToExpr(args)
			playlist, err := subsonicPlaylist(songs, exprStr)
			if err != nil {
				return err
			}
			if len(playlist) == 0 {
				if exprStr != "" {
					return fmt.Errorf("no songs matching %q found on %s", exprStr, client.baseURL)
				}
				return fmt.Errorf("no songs found on %s", client.baseURL)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Found %d song(s).\n", len(playlist))

			shufflePlaylist(playlist)

			if noTUI {
				return runNoTUI(playlist)
			}
			return runTUI(playlist)
		},
	}

	cmd.Flags().BoolVar(&noTUI, "no-tui", false, "Play without the terminal UI")
	cmd.Flags().StringVar(&server, "server", "", "Subsonic server URL (saved for future runs)")
	cmd.Flags().StringVar(&user, "user", "", "Subsonic username (saved for future runs)")
	return cmd
}

// subsonicPlaylist turns songs into "subsonic:<id>" playlist entries, keeping
// only those whose match text satisfies exprStr. An empty exprStr keeps all.
func subsonicPlaylist(songs []subsonicSong, exprStr string) ([]string, error) {
	var expr filter.Expr
	if exprStr != "" {
		var err error
		expr, err = filter.ParseExpr(exprStr)
		if err != nil {
			return nil, fmt.Errorf("invalid keyword expression: %w", err)
		}
	}

	var playlist []string
	for _, s := range songs {
		if expr != nil && len(filter.FilterExpr([]string{s.matchText()}, expr)) == 0 {
			continue
		}
		playlist = append(playlist, subsonicPrefix+s.ID)
	}
	return playlist, nil
}

// sonicDetails are the connection details for one run.
type sonicDetails struct {
	server   string
	user     string
	password string
	changed  bool // differs from the saved config and should be saved once verified
}

// sonicPrompter asks the user for missing connection details.
type sonicPrompter struct {
	in           *bufio.Reader
	out          io.Writer
	readPassword func() (string, error)
}

// ask prints label and returns the trimmed line the user typed. An empty
// answer is asked again.
func (p sonicPrompter) ask(label string) (string, error) {
	for {
		fmt.Fprint(p.out, label)
		line, err := p.in.ReadString('\n')
		line = strings.TrimSpace(line)
		if line != "" {
			return line, nil
		}
		if err != nil {
			return "", fmt.Errorf("no answer for %q: %w", strings.TrimSpace(label), err)
		}
	}
}

// resolveSonicDetails merges flags with the saved config and prompts for
// anything missing. Flags win over the config. Changing the server or user
// discards the saved password, since it belongs to the old account.
func resolveSonicDetails(cfg *Config, serverFlag, userFlag string, p sonicPrompter) (sonicDetails, error) {
	d := sonicDetails{
		server:   normalizeServerURL(serverFlag),
		user:     strings.TrimSpace(userFlag),
		password: cfg.SubsonicPassword,
	}

	if d.server == "" {
		d.server = cfg.SubsonicServer
	}
	if d.server == "" {
		fmt.Fprintln(p.out, "No Subsonic server configured.")
		answer, err := p.ask("Server URL: ")
		if err != nil {
			return d, err
		}
		d.server = normalizeServerURL(answer)
	}
	sameServer := d.server == cfg.SubsonicServer

	if d.user == "" && sameServer {
		d.user = cfg.SubsonicUser
	}
	if d.user == "" {
		answer, err := p.ask("Username: ")
		if err != nil {
			return d, err
		}
		d.user = answer
	}

	if !sameServer || d.user != cfg.SubsonicUser {
		d.password = ""
	}
	if d.password == "" {
		fmt.Fprint(p.out, "Password: ")
		pw, err := p.readPassword()
		fmt.Fprintln(p.out)
		if err != nil {
			return d, fmt.Errorf("could not read password: %w", err)
		}
		if pw == "" {
			return d, fmt.Errorf("a password is required")
		}
		d.password = pw
	}

	d.changed = d.server != cfg.SubsonicServer || d.user != cfg.SubsonicUser || d.password != cfg.SubsonicPassword
	return d, nil
}

// readTerminalPassword reads a password from stdin without echo when stdin is
// a terminal, and as a plain line from in when it is piped. in must be the
// reader the other prompts use, or input it already buffered is lost.
func readTerminalPassword(in *bufio.Reader) (string, error) {
	if term.IsTerminal(os.Stdin.Fd()) {
		b, err := term.ReadPassword(os.Stdin.Fd())
		return string(b), err
	}
	line, err := in.ReadString('\n')
	if line = strings.TrimSpace(line); line != "" {
		return line, nil
	}
	return "", err
}
