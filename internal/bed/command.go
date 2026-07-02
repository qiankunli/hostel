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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

func randomID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// Command is one one-shot execution (spec /command), foreground or background.
// Output lines are buffered for the status/logs endpoints; a foreground caller
// additionally streams them live via the onLine callback.
type Command struct {
	ID    string
	BedID string

	mu         sync.Mutex
	lines      []string
	running    bool
	exitCode   *int
	err        string
	startedAt  time.Time
	finishedAt *time.Time

	cmd  *exec.Cmd
	done chan struct{} // closed when the process has been reaped
}

// commandBufferLimit caps buffered output lines per command so a chatty
// background process can't grow the daemon unbounded; older lines are dropped
// (the cursor semantics of /logs still hold — indices keep increasing).
const commandBufferLimit = 100_000

// dropped counts lines evicted from the front; line i lives at lines[i-dropped].
type cursorState struct{ dropped int64 }

// Snapshot of command state for the status endpoint.
type CommandStatus struct {
	ID         string
	Running    bool
	ExitCode   *int
	Err        string
	StartedAt  time.Time
	FinishedAt *time.Time
	Content    string
}

// CommandRegistry tracks one-shot commands. Ids are daemon-global because the
// spec's status/logs endpoints don't carry a bed dimension.
type CommandRegistry struct {
	mu      sync.Mutex
	cmds    map[string]*Command
	cursors map[string]*cursorState
}

func newCommandRegistry() *CommandRegistry {
	return &CommandRegistry{cmds: make(map[string]*Command), cursors: make(map[string]*cursorState)}
}

// start launches cmd and streams combined stdout/stderr into the buffer (and
// onLine when given). timeout > 0 kills the process group at the deadline.
func (r *CommandRegistry) start(bedID string, cmd *exec.Cmd, timeout time.Duration, onLine func(string)) (*Command, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true // kill the whole tree on interrupt/timeout

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = cmd.Stdout

	c := &Command{
		ID:        "cmd-" + randomID(),
		BedID:     bedID,
		running:   true,
		startedAt: time.Now(),
		cmd:       cmd,
		done:      make(chan struct{}),
	}
	if err := cmd.Start(); err != nil {
		stdout.Close()
		return nil, err
	}

	r.mu.Lock()
	r.cmds[c.ID] = c
	r.cursors[c.ID] = &cursorState{}
	r.mu.Unlock()

	var timer *time.Timer
	if timeout > 0 {
		timer = time.AfterFunc(timeout, func() { c.Interrupt() })
	}

	go func() {
		reader := bufio.NewReader(stdout)
		for {
			line, err := reader.ReadString('\n')
			if line != "" {
				c.appendLine(r, line)
				if onLine != nil {
					onLine(line)
				}
			}
			if err != nil {
				break
			}
		}
		werr := cmd.Wait()
		if timer != nil {
			timer.Stop()
		}
		now := time.Now()
		c.mu.Lock()
		c.running = false
		c.finishedAt = &now
		code := 0
		if werr != nil {
			if ee, ok := werr.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				code = -1
				c.err = werr.Error()
			}
		}
		c.exitCode = &code
		c.mu.Unlock()
		close(c.done)
	}()
	return c, nil
}

func (c *Command) appendLine(r *CommandRegistry, line string) {
	c.mu.Lock()
	c.lines = append(c.lines, line)
	if len(c.lines) > commandBufferLimit {
		evict := len(c.lines) - commandBufferLimit
		c.lines = c.lines[evict:]
		r.mu.Lock()
		if cs, ok := r.cursors[c.ID]; ok {
			cs.dropped += int64(evict)
		}
		r.mu.Unlock()
	}
	c.mu.Unlock()
}

// Wait blocks until the process is reaped (foreground streaming).
func (c *Command) Wait() { <-c.done }

// Interrupt kills the process group.
func (c *Command) Interrupt() {
	c.mu.Lock()
	proc := c.cmd.Process
	running := c.running
	c.mu.Unlock()
	if running && proc != nil {
		_ = syscall.Kill(-proc.Pid, syscall.SIGKILL)
	}
}

// Status snapshots the command for /command/status/{id}.
func (c *Command) Status() CommandStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return CommandStatus{
		ID:         c.ID,
		Running:    c.running,
		ExitCode:   c.exitCode,
		Err:        c.err,
		StartedAt:  c.startedAt,
		FinishedAt: c.finishedAt,
		Content:    strings.Join(c.lines, ""),
	}
}

// Get looks a command up by id.
func (r *CommandRegistry) Get(id string) (*Command, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.cmds[id]
	return c, ok
}

// Logs returns output lines after the 0-based line cursor (-1 = from start),
// plus the next cursor (last line index seen) and whether it is still running.
func (r *CommandRegistry) Logs(id string, cursor int64) (content string, next int64, running bool, err error) {
	r.mu.Lock()
	c, ok := r.cmds[id]
	cs := r.cursors[id]
	r.mu.Unlock()
	if !ok {
		return "", 0, false, fmt.Errorf("command %s not found", id)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	dropped := int64(0)
	if cs != nil {
		dropped = cs.dropped
	}
	total := dropped + int64(len(c.lines))
	start := cursor + 1 // lines after `cursor`
	if cursor < 0 {
		start = 0
	}
	if start < dropped {
		start = dropped // evicted lines are gone; resume at oldest retained
	}
	if start < total {
		content = strings.Join(c.lines[start-dropped:], "")
	}
	next = total - 1
	if next < 0 {
		next = 0
	}
	return content, next, c.running, nil
}

// killBed interrupts every command belonging to a bed (bed teardown).
func (r *CommandRegistry) killBed(bedID string) {
	r.mu.Lock()
	var victims []*Command
	for _, c := range r.cmds {
		if c.BedID == bedID {
			victims = append(victims, c)
		}
	}
	r.mu.Unlock()
	for _, c := range victims {
		c.Interrupt()
	}
}
