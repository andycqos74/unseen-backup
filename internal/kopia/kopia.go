// Package kopia drives the kopia command-line tool, the backup engine for
// jobs with engine "kopia". The server uses it to own the repository and run
// `kopia server`; agents use it to snapshot and restore through that server.
//
// Shelling out (rather than linking Kopia as a library) keeps the agent a
// small binary that still builds with Go 1.20 for Windows 7, and lets Kopia
// be upgraded independently. Keep this package stdlib-only and Go 1.20
// compatible: the agent imports it.
package kopia

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// CLI runs kopia with one config file. Secrets go through the environment,
// never argv.
type CLI struct {
	Bin        string // path to the kopia binary
	ConfigFile string
	CacheDir   string
	LogDir     string
	Password   string            // KOPIA_PASSWORD: repository or server-user password
	ExtraEnv   map[string]string // e.g. KOPIA_SERVER_PASSWORD
}

func (c *CLI) cmd(ctx context.Context, args ...string) *exec.Cmd {
	full := []string{"--config-file=" + c.ConfigFile}
	if c.LogDir != "" {
		full = append(full, "--log-dir="+c.LogDir)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, c.Bin, full...)
	env := os.Environ()
	env = append(env, "KOPIA_CHECK_FOR_UPDATES=false", "KOPIA_PASSWORD="+c.Password)
	if c.CacheDir != "" {
		env = append(env, "KOPIA_CACHE_DIRECTORY="+c.CacheDir)
	}
	for k, v := range c.ExtraEnv {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	return cmd
}

// Error is a failed kopia invocation with its stderr.
type Error struct {
	Args   []string
	Err    error
	Stderr string
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if i := strings.LastIndexByte(msg, '\n'); i >= 0 && len(msg) > 400 {
		msg = msg[i+1:]
	}
	if len(msg) > 400 {
		msg = msg[:400] + "…"
	}
	name := "kopia"
	if len(e.Args) > 0 {
		name += " " + e.Args[0]
		if len(e.Args) > 1 && !strings.HasPrefix(e.Args[1], "-") {
			name += " " + e.Args[1]
		}
	}
	if msg == "" {
		return fmt.Sprintf("%s: %v", name, e.Err)
	}
	return fmt.Sprintf("%s: %v: %s", name, e.Err, msg)
}

// Run executes kopia and returns stdout. A non-zero exit is an *Error that
// still carries stdout (some commands report partial success that way).
func (c *CLI) Run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := c.cmd(ctx, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), &Error{Args: args, Err: err, Stderr: errb.String()}
	}
	return out.Bytes(), nil
}

// Stream runs kopia and streams its stdout to w (e.g. `show <file object>`).
func (c *CLI) Stream(ctx context.Context, w io.Writer, args ...string) error {
	cmd := c.cmd(ctx, args...)
	var errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = w, &errb
	if err := cmd.Run(); err != nil {
		return &Error{Args: args, Err: err, Stderr: errb.String()}
	}
	return nil
}

// Snapshot is the part of a snapshot manifest this system uses.
type Snapshot struct {
	ID     string `json:"id"`
	Source struct {
		Host     string `json:"host"`
		UserName string `json:"userName"`
		Path     string `json:"path"`
	} `json:"source"`
	StartTime  time.Time         `json:"startTime"`
	Tags       map[string]string `json:"tags"`
	Incomplete string            `json:"incomplete"`
	RootEntry  struct {
		Obj  string  `json:"obj"`
		Summ Summary `json:"summ"`
	} `json:"rootEntry"`
}

// Summary is a directory summary (totals for the whole snapshot at the root).
type Summary struct {
	Size      int64 `json:"size"`
	Files     int64 `json:"files"`
	Dirs      int64 `json:"dirs"`
	NumFailed int64 `json:"numFailed"`
	Errors    []struct {
		Path  string `json:"path"`
		Error string `json:"error"`
	} `json:"errors"`
}

// DecodeSnapshots parses `snapshot create --json` / `snapshot list --json`
// output: either a JSON array or a sequence of JSON objects.
func DecodeSnapshots(data []byte) ([]Snapshot, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil
	}
	if data[0] == '[' {
		var list []Snapshot
		err := json.Unmarshal(data, &list)
		return list, err
	}
	var list []Snapshot
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var s Snapshot
		err := dec.Decode(&s)
		if errors.Is(err, io.EOF) {
			return list, nil
		}
		if err != nil {
			return list, err
		}
		if s.ID != "" {
			list = append(list, s)
		}
	}
}

// DirEntry is one entry of a directory object (`kopia show <dir object>`).
type DirEntry struct {
	Name  string    `json:"name"`
	Type  string    `json:"type"` // f | d | s (symlink) | ...
	Mode  string    `json:"mode"`
	Size  int64     `json:"size"`
	MTime time.Time `json:"mtime"`
	Obj   string    `json:"obj"`
}

// ReadDir lists a directory object.
func (c *CLI) ReadDir(ctx context.Context, obj string) ([]DirEntry, error) {
	out, err := c.Run(ctx, "show", obj)
	if err != nil {
		return nil, err
	}
	var d struct {
		Stream  string     `json:"stream"`
		Entries []DirEntry `json:"entries"`
	}
	if err := json.Unmarshal(out, &d); err != nil {
		return nil, fmt.Errorf("kopia show %s: not a directory listing: %w", obj, err)
	}
	return d.Entries, nil
}
