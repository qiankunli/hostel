// Copyright 2026 Li Qiankun
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bed

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"time"

	"github.com/qiankunli/go-stdx/shellx"

	"github.com/qiankunli/hostel/internal/isolation"
)

var bedIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func validBedID(id string) error {
	if !bedIDRe.MatchString(id) {
		return fmt.Errorf("bed: invalid id %q (allowed: alnum . _ -, ≤128)", id)
	}
	return nil
}

// Shell is a bed's long-running shell process. Commands are written to its
// stdin and framed by a per-run marker so stateful shell context (cwd, env,
// functions) persists across runs — matching "shell state survives across
// exec" semantics. A single reader goroutine drains combined stdout/stderr into
// lines; Run (serialized by runMu) is the sole consumer, so output can't leak
// between runs.
//
// LOCKING (the original single-mutex design deadlocked a live daemon — see
// the fix commit): runMu is held for a Run's whole duration and is touched by
// NOBODY else; mu guards only the dead flag and is held for nanoseconds. The
// reader goroutine needs mu (never runMu) to mark death before closing lines,
// so a dying shell can always unblock the Run that is waiting on the channel.
// Callers holding bed/manager locks may call Dead() safely for the same
// reason. Never add code that holds mu while blocking.
type Shell struct {
	proc  Proc
	stdin io.WriteCloser
	lines chan string // every output line; closed on EOF/exit

	runMu sync.Mutex // serializes Run; held while waiting for output
	mu    sync.Mutex // guards dead only; held briefly
	dead  bool
}

// startShell launches the shell confined by iso to ws via the spawner. cwdInBed,
// when set, becomes the starting directory via an initial `cd`. Stdio is
// explicit os.Pipe pairs (not StdinPipe/StdoutPipe) so the raw fds can cross a
// process boundary when the spawner is the bed's init.
func startShell(sp Spawner, bedID, shellPath string, iso isolation.Isolator, ws isolation.Workspace, cwdInBed string) (*Shell, error) {
	cmd := exec.Command(shellPath, "--noprofile", "--norc")
	// Same bed-id handle as one-shot commands (see buildCommand) — the session
	// shell inherits the daemon env otherwise, which lacks the bed id.
	cmd.Env = append(os.Environ(), "HOSTEL_BED_ID="+bedID)
	if err := iso.Wrap(cmd, ws); err != nil {
		return nil, err
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	cmd.Stdin = inR
	cmd.Stdout = outW
	cmd.Stderr = outW // interleave, like a terminal
	proc, err := sp.Start(bedID, cmd)
	// The child holds its own copies now (or never will, on error): drop ours
	// of the child-side ends — outW in particular, or the reader never EOFs.
	inR.Close()
	outW.Close()
	if err != nil {
		inW.Close()
		outR.Close()
		return nil, err
	}

	s := &Shell{proc: proc, stdin: inW, lines: make(chan string, 64)}
	if cwdInBed != "" {
		// Best-effort initial cwd; a failure surfaces in the first run's output.
		_, _ = io.WriteString(inW, "cd -- "+shellx.Quote(cwdInBed)+" || true\n")
	}
	// Single long-lived reader → lines channel.
	go func() {
		r := bufio.NewReader(outR)
		for {
			line, err := r.ReadString('\n')
			if line != "" {
				s.lines <- line
			}
			if err != nil {
				outR.Close()
				s.mu.Lock()
				s.dead = true
				s.mu.Unlock()
				close(s.lines)
				return
			}
		}
	}()
	go func() { _, _ = proc.Wait() }() // reap; EOF above drives dead state
	return s, nil
}

// Dead reports whether the shell process has exited.
func (s *Shell) Dead() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dead
}

// Close terminates the shell process group.
func (s *Shell) Close() {
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	if s.proc != nil {
		s.proc.Kill()
	}
}

// RunResult is the outcome of one command in the shell.
type RunResult struct {
	ExitCode int
}

// Run writes command to the shell and streams combined stdout/stderr to onLine
// until the command completes, detected by a unique end-marker echoing $?.
// ctx cancels the wait (the shell keeps running; caller may Close to abort).
func (s *Shell) Run(ctx context.Context, command string, onLine func(string)) (*RunResult, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.Dead() {
		return nil, fmt.Errorf("shell: session is dead")
	}

	marker := fmt.Sprintf("__hostel_end_%d__", time.Now().UnixNano())
	full := fmt.Sprintf("%s\nprintf '%%s %%d\\n' %s \"$?\"\n", command, marker)
	if _, err := io.WriteString(s.stdin, full); err != nil {
		return nil, fmt.Errorf("shell: write command: %w", err)
	}
	markerRe := regexp.MustCompile("^" + regexp.QuoteMeta(marker) + ` (\d+)\s*$`)

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case line, ok := <-s.lines:
			if !ok {
				return nil, fmt.Errorf("shell: session exited during run")
			}
			if m := markerRe.FindStringSubmatch(line); m != nil {
				code := 0
				fmt.Sscanf(m[1], "%d", &code)
				return &RunResult{ExitCode: code}, nil
			}
			if onLine != nil {
				onLine(line)
			}
		}
	}
}
