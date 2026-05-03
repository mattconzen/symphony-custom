// Package fakeagent provides a re-exec fake-agent harness for testing Codex
// and Claude runtimes without requiring real binaries. Test binaries detect
// SYMPHONY_FAKE_AGENT_MODE env and call Run() from TestMain, which reads a
// script from SYMPHONY_FAKE_AGENT_SCRIPT_B64 and plays it out on stdout.
package fakeagent

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// Directive is one step in a fake-agent script.
type Directive struct {
	// WaitMs is an optional delay before writing.
	WaitMs int `json:"wait_ms,omitempty"`
	// WriteStdout is a raw string to write to stdout (newline appended if missing).
	WriteStdout string `json:"write_stdout,omitempty"`
	// WriteStdoutJSONL is an object to JSON-encode and write as a JSONL line.
	WriteStdoutJSONL any `json:"write_stdout_jsonl,omitempty"`
	// ExpectStdinLine, when non-empty, reads one line from stdin and discards it
	// (used to synchronize with the runtime sending a message).
	ExpectStdinLine string `json:"expect_stdin_line,omitempty"`
	// Exit, when non-zero or ExitSet is true, causes the process to exit.
	Exit    int  `json:"exit,omitempty"`
	ExitSet bool `json:"exit_set,omitempty"`
}

// ScriptFromEnv decodes the base64 script from the env var
// SYMPHONY_FAKE_AGENT_SCRIPT_B64 into a slice of Directives. Returns nil if
// the env var is not set.
func ScriptFromEnv() ([]Directive, error) {
	raw := os.Getenv("SYMPHONY_FAKE_AGENT_SCRIPT_B64")
	if raw == "" {
		return nil, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("fakeagent: base64 decode script: %w", err)
	}
	var directives []Directive
	if err := json.Unmarshal(decoded, &directives); err != nil {
		return nil, fmt.Errorf("fakeagent: unmarshal script: %w", err)
	}
	return directives, nil
}

// EncodeScript base64-encodes a slice of Directives for use as
// SYMPHONY_FAKE_AGENT_SCRIPT_B64.
func EncodeScript(directives []Directive) (string, error) {
	b, err := json.Marshal(directives)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// Run executes the fake-agent script from the environment. It reads the script
// from SYMPHONY_FAKE_AGENT_SCRIPT_B64, plays it out, then exits. If the env
// var is missing, Run returns immediately (caller can fall through to m.Run()).
func Run(mode string) {
	directives, err := ScriptFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeagent[%s]: load script: %v\n", mode, err)
		os.Exit(1)
	}
	if directives == nil {
		// No script; exit 0 (e.g. if the test binary was called without a script
		// just to satisfy the -test.run=^$ pattern).
		os.Exit(0)
	}

	stdin := bufio.NewReader(os.Stdin)
	stdout := bufio.NewWriter(os.Stdout)

	for _, d := range directives {
		if d.WaitMs > 0 {
			time.Sleep(time.Duration(d.WaitMs) * time.Millisecond)
		}

		if d.ExpectStdinLine != "" {
			// Read (and discard) one line from stdin.
			_, err := stdin.ReadString('\n')
			if err != nil && err != io.EOF {
				fmt.Fprintf(os.Stderr, "fakeagent[%s]: read stdin: %v\n", mode, err)
				os.Exit(1)
			}
		}

		if d.WriteStdout != "" {
			line := d.WriteStdout
			if len(line) == 0 || line[len(line)-1] != '\n' {
				line += "\n"
			}
			if _, err := fmt.Fprint(stdout, line); err != nil {
				fmt.Fprintf(os.Stderr, "fakeagent[%s]: write stdout: %v\n", mode, err)
				os.Exit(1)
			}
			stdout.Flush() //nolint:errcheck
		}

		if d.WriteStdoutJSONL != nil {
			b, err := json.Marshal(d.WriteStdoutJSONL)
			if err != nil {
				fmt.Fprintf(os.Stderr, "fakeagent[%s]: marshal jsonl: %v\n", mode, err)
				os.Exit(1)
			}
			b = append(b, '\n')
			if _, err := stdout.Write(b); err != nil {
				fmt.Fprintf(os.Stderr, "fakeagent[%s]: write jsonl: %v\n", mode, err)
				os.Exit(1)
			}
			stdout.Flush() //nolint:errcheck
		}

		if d.ExitSet || d.Exit != 0 {
			stdout.Flush() //nolint:errcheck
			os.Exit(d.Exit)
		}
	}

	stdout.Flush() //nolint:errcheck
	os.Exit(0)
}
