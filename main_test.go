package main

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"derpy/internal/filter"
)

func TestArgsToExpr(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"empty", nil, ""},
		{"single word", []string{"jazz"}, "jazz"},
		{"two single-word args ORed", []string{"jazz", "blues"}, "jazz OR blues"},
		{"phrase (shell stripped quotes)", []string{"Front Line Assembly"}, `"Front Line Assembly"`},
		{"expression with AND passed through", []string{"jazz AND piano"}, "jazz AND piano"},
		{"expression with OR passed through", []string{"jazz OR blues"}, "jazz OR blues"},
		{"expression with parens passed through", []string{"(jazz OR blues) AND piano"}, "(jazz OR blues) AND piano"},
		{"pre-quoted phrase passed through", []string{`"Front Line Assembly"`}, `"Front Line Assembly"`},
		{"phrase + single word", []string{"Front Line Assembly", "blues"}, `"Front Line Assembly" OR blues`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := argsToExpr(tt.args)
			if got != tt.want {
				t.Errorf("argsToExpr(%q) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

// TestArgsToExprParses verifies the output of argsToExpr is always a valid
// expression that ParseExpr accepts (regression test for the
// "Front Line Assembly" bug — unexpected token "line").
func TestArgsToExprParses(t *testing.T) {
	inputs := [][]string{
		{"Front Line Assembly"},
		{"front line assembly"},
		{"AC/DC"},
		{"jazz", "blues"},
		{"jazz AND piano"},
		{"(jazz OR blues) AND piano"},
		{"Front Line Assembly", "Skinny Puppy"},
	}
	for _, args := range inputs {
		expr := argsToExpr(args)
		if expr == "" {
			continue
		}
		if _, err := filter.ParseExpr(expr); err != nil {
			t.Errorf("argsToExpr(%q) = %q failed to parse: %v", args, expr, err)
		}
	}
}

func TestScanMusicDirectory(t *testing.T) {
	// Create a temporary directory structure
	tmpDir, err := os.MkdirTemp("", "derpy-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}

	defer os.RemoveAll(tmpDir)

	// Create structure:
	// /Gospel/Jesus.mp3
	// /Gospel/Oldies.wav
	// /Pop/Song.flac
	// /Other.mp3

	structure := []string{
		"Gospel/Jesus.mp3",
		"Gospel/Oldies.wav",
		"Pop/Song.flac",
		"Other.mp3",
	}

	for _, p := range structure {
		fullPath := filepath.Join(tmpDir, p)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			t.Fatalf("Failed to create dir: %v", err)
		}
		if err := os.WriteFile(fullPath, []byte("fake content"), 0644); err != nil {
			t.Fatalf("Failed to write file: %v", err)
		}
	}

	tests := []struct {
		name     string
		expr     string
		expected []string
	}{
		{
			name: "No expression",
			expr: "",
			expected: []string{
				filepath.Join(tmpDir, "Gospel/Jesus.mp3"),
				filepath.Join(tmpDir, "Gospel/Oldies.wav"),
				filepath.Join(tmpDir, "Other.mp3"),
				filepath.Join(tmpDir, "Pop/Song.flac"),
			},
		},
		{
			name: "Single term (jesus)",
			expr: "jesus",
			expected: []string{
				filepath.Join(tmpDir, "Gospel/Jesus.mp3"),
			},
		},
		{
			name: "Term matches directory (gospel)",
			expr: "gospel",
			expected: []string{
				filepath.Join(tmpDir, "Gospel/Jesus.mp3"),
				filepath.Join(tmpDir, "Gospel/Oldies.wav"),
			},
		},
		{
			name: "OR expression",
			expr: "jesus OR pop",
			expected: []string{
				filepath.Join(tmpDir, "Gospel/Jesus.mp3"),
				filepath.Join(tmpDir, "Pop/Song.flac"),
			},
		},
		{
			name: "AND expression",
			expr: "gospel AND jesus",
			expected: []string{
				filepath.Join(tmpDir, "Gospel/Jesus.mp3"),
			},
		},
		{
			name: "AND expression — no match",
			expr: "gospel AND pop",
			expected: nil,
		},
		{
			name: "Grouped expression: (gospel OR pop) AND mp3",
			expr: "(gospel OR pop) AND mp3",
			expected: []string{
				filepath.Join(tmpDir, "Gospel/Jesus.mp3"),
			},
		},
		{
			name: "Case-insensitive matching",
			expr: "JESUS",
			expected: []string{
				filepath.Join(tmpDir, "Gospel/Jesus.mp3"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := scanMusicDirectory(tmpDir, tt.expr)
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.expected) {
				t.Errorf("got %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestRunPlayerPrintsStartupScanFeedback(t *testing.T) {
	tmpDir := t.TempDir()

	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create stdout pipe: %v", err)
	}
	os.Stdout = w

	runErr := runPlayer(tmpDir, "", true)

	_ = w.Close()
	os.Stdout = origStdout

	out, _ := io.ReadAll(r)
	output := string(out)
	_ = r.Close()

	if runErr == nil || !strings.Contains(runErr.Error(), "no audio files found in directory") {
		t.Fatalf("expected no-audio-files error, got: %v", runErr)
	}
	if !strings.Contains(output, "Scanning source directory:") {
		t.Fatalf("expected startup scan feedback in stdout, got: %q", output)
	}
	if !strings.Contains(output, "Scan complete. Found 0 audio file(s).") {
		t.Fatalf("expected scan completion feedback in stdout, got: %q", output)
	}
}
