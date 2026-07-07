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

package amenity

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	"github.com/qiankunli/go-stdx/randx"
)

// Browser is the action surface the web layer adapts to HTTP. Deliberately
// small in v1 (docs/amenity.md §2): open-look-shoot; interactions come later.
// The RAW browser-level CDP websocket is never exposed north — it sees every
// bed's context. What IS exposed is a per-bed PROXIED endpoint (CDPToken +
// ServeCDP): the proxy pins the bed's own BrowserContext and filters
// target/context visibility, so a bed's playwright drives only its own slice
// of the shared browser.
type Browser interface {
	Goto(ctx context.Context, bedID, workspace, url string) (title, finalURL string, err error)
	// Screenshot captures the current page into the bed workspace and returns
	// the virtual /workspace path (fetchable via the file API, carried by
	// snapshots).
	Screenshot(ctx context.Context, bedID, workspace, relPath string) (string, error)
	Text(ctx context.Context, bedID, workspace string) (string, error)
	// Interaction verbs (docs/amenity.md §2), selector = CSS query.
	Click(ctx context.Context, bedID, workspace, selector string) error
	// Type sends text into the element; clear empties it first.
	Type(ctx context.Context, bedID, workspace, selector, text string, clear bool) error
	// Press dispatches a key (Enter, Tab, Escape, Backspace, Arrow*, or a
	// single literal char) to the focused element.
	Press(ctx context.Context, bedID, workspace, key string) error
	// Scroll scrolls the window by (dx, dy) pixels.
	Scroll(ctx context.Context, bedID, workspace string, dx, dy int) error
	// Wait blocks until selector becomes visible (bounded by the action timeout).
	Wait(ctx context.Context, bedID, workspace, selector string) error
	// CDPToken ensures the bed's browser slice and returns the secret that
	// authorizes its proxied CDP endpoint (see ServeCDP). The token is minted
	// per tenant and dies with it — possession proves the caller got it from
	// the bed-scoped /v1/browser/info, not by guessing a bed id.
	CDPToken(bedID, workspace string) (string, error)
	// ServeCDP bridges one already-upgraded websocket to the shared browser as
	// bedID's tenant view: Target.* visibility is filtered to the bed's own
	// browser contexts and Browser.close is refused. Blocks until either side
	// closes. conn is always closed on return.
	ServeCDP(conn net.Conn, bedID, token string) error
	ReleaseTenant(bedID string) error
}

// ChromiumConfig selects launch-or-attach (docs/amenity.md §5).
type ChromiumConfig struct {
	// ExecPath launches a hostel-owned Chromium ("" = probe common locations).
	ExecPath string
	// CDPURL attaches to an existing instance (sidecar/supervisor deployments
	// — the execd --jupyter-host shape); hostel then slices but does not own
	// the process.
	CDPURL string
	// IdleStop stops a LAUNCHED browser this long after its last tenant is
	// released (0 = never stop). Frees hundreds of MB between bursts.
	IdleStop time.Duration
	// ActionTimeout bounds one browser action (navigate, shot...).
	ActionTimeout time.Duration
	// DebugPort fixes the launched browser's --remote-debugging-port so the
	// per-bed CDP proxy has a stable upstream (chromedp alone would pick a
	// random port hostel can't dial back). 0 disables the proxy in launch mode;
	// attach mode always uses CDPURL as upstream.
	DebugPort int
}

// chromium is the first amenity: one shared browser, one isolated
// BrowserContext per bed.
type chromium struct {
	cfg    ChromiumConfig
	attach bool

	mu        sync.Mutex
	state     string
	allocCtx  context.Context
	allocStop context.CancelFunc
	master    context.Context // chromedp browser-level context
	masterCtl context.CancelFunc
	tenants   map[string]*chromiumTenant
	idleTimer *time.Timer

	// Crash supervision (the supervisor is the amenity itself, in-daemon —
	// docs/design.md 〈进程树〉): a watcher on the master context detects the
	// browser dying and flips back to idle with tenants dropped, so the NEXT
	// AcquireTenant lazily rebuilds its slice — no restart storm, and a bed
	// simply sees a fresh browser. notBefore gates ensureRunning with
	// exponential backoff so a crash-looping browser can't melt the pod.
	crashCount int
	lastCrash  time.Time
	notBefore  time.Time
}

type chromiumTenant struct {
	bedID     string
	contextID cdp.BrowserContextID
	tabCtx    context.Context
	tabStop   context.CancelFunc
	// cdpToken authorizes the bed's proxied CDP endpoint. Minted with the
	// tenant, handed out only via CDPToken (the bed-scoped browser/info path),
	// dies with the tenant.
	cdpToken string
}

func (t *chromiumTenant) Close() error {
	t.tabStop()
	return nil
}

// chromiumCandidates are probed when --chromium-path is unset.
var chromiumCandidates = []string{
	"chromium", "chromium-browser", "google-chrome", "google-chrome-stable",
	"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	"/Applications/Chromium.app/Contents/MacOS/Chromium",
}

// NewChromium builds the amenity, probing availability at boot: launch mode
// needs a binary, attach mode needs the endpoint to answer /json/version.
// Returns ok=false when neither is usable — the caller then simply doesn't
// register it, and capabilities reports the facility as absent.
func NewChromium(cfg ChromiumConfig) (Browser, bool) {
	if cfg.ActionTimeout <= 0 {
		cfg.ActionTimeout = 30 * time.Second
	}
	c := &chromium{cfg: cfg, state: StateIdle, tenants: map[string]*chromiumTenant{}}

	if cfg.CDPURL != "" {
		c.attach = true
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get(strings.TrimSuffix(cfg.CDPURL, "/") + "/json/version")
		if err != nil {
			log.Printf("amenity: chromium attach probe %s failed: %v", cfg.CDPURL, err)
			return nil, false
		}
		resp.Body.Close()
		return c, true
	}

	path := cfg.ExecPath
	if path == "" {
		for _, cand := range chromiumCandidates {
			if p, err := exec.LookPath(cand); err == nil {
				path = p
				break
			}
		}
	} else if _, err := exec.LookPath(path); err != nil {
		path = ""
	}
	if path == "" {
		return nil, false
	}
	c.cfg.ExecPath = path
	return c, true
}

func (c *chromium) Name() string { return "chromium" }

func (c *chromium) State() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// ensureRunning starts (or attaches to) the shared browser on first demand.
// Caller holds c.mu.
func (c *chromium) ensureRunning() error {
	if c.state == StateRunning {
		return nil
	}
	// Crash-loop guard: after the watcher recorded a death, restarts are gated.
	// The error is the caller's signal to retry later — deliberately NOT a
	// blocking sleep, which would pin c.mu and freeze every bed's actions.
	if wait := time.Until(c.notBefore); wait > 0 {
		return fmt.Errorf("amenity: chromium restart gated for %s (crash #%d)", wait.Round(time.Millisecond), c.crashCount)
	}
	base := context.Background()
	if c.attach {
		c.allocCtx, c.allocStop = chromedp.NewRemoteAllocator(base, c.cfg.CDPURL)
	} else {
		opts := append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.ExecPath(c.cfg.ExecPath),
			chromedp.NoSandbox, // hostel often runs as root in a container; bed code can't reach this process anyway
		)
		if c.cfg.DebugPort > 0 {
			// Fixed port so the per-bed CDP proxy has a stable upstream to dial;
			// chromedp's own stderr-parsed ws URL is not exposed by its API.
			opts = append(opts, chromedp.Flag("remote-debugging-port", strconv.Itoa(c.cfg.DebugPort)))
		}
		c.allocCtx, c.allocStop = chromedp.NewExecAllocator(base, opts...)
	}
	c.master, c.masterCtl = chromedp.NewContext(c.allocCtx)
	// Force the browser up now so failures surface here, not mid-action.
	if err := chromedp.Run(c.master); err != nil {
		c.stopLocked()
		return fmt.Errorf("amenity: chromium start: %w", err)
	}
	c.state = StateRunning
	go c.watchMaster(c.master) // crash detector for THIS instance
	return nil
}

// watchMaster turns the master context's death into supervision: chromedp
// cancels it when the browser process exits (crash) — and orderly stops cancel
// it too, which onMasterGone tells apart by state.
func (c *chromium) watchMaster(master context.Context) {
	<-master.Done()
	c.onMasterGone(master)
}

// onMasterGone handles one master-context death. Only an UNEXPECTED death of
// the CURRENT instance counts as a crash: orderly stops (idle-stop timer,
// stopLocked) already flipped state off Running before releasing the lock, and
// a stale watcher's master no longer matches.
func (c *chromium) onMasterGone(master context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.master != master || c.state != StateRunning {
		return
	}
	now := time.Now()
	if now.Sub(c.lastCrash) > 5*time.Minute {
		c.crashCount = 0 // stable for a while: earlier crashes are history
	}
	c.crashCount++
	c.lastCrash = now
	backoff := time.Duration(1<<min(c.crashCount-1, 6)) * time.Second // 1s → 64s cap
	c.notBefore = now.Add(backoff)
	dropped := len(c.tenants)
	c.stopLocked()
	log.Printf("amenity: chromium died (crash #%d, %d tenant(s) dropped); restart gated for %s",
		c.crashCount, dropped, backoff)
}

// stopLocked tears the browser down. Caller holds c.mu.
func (c *chromium) stopLocked() {
	for id, t := range c.tenants {
		t.tabStop()
		delete(c.tenants, id)
	}
	if c.masterCtl != nil {
		c.masterCtl()
		c.masterCtl = nil
	}
	if c.allocStop != nil {
		c.allocStop()
		c.allocStop = nil
	}
	c.state = StateIdle
}

// tenant returns the bed's slice, creating context+tab lazily.
// Caller holds c.mu.
func (c *chromium) tenant(bedID, workspace string) (*chromiumTenant, error) {
	if t, ok := c.tenants[bedID]; ok {
		return t, nil
	}
	if err := c.ensureRunning(); err != nil {
		return nil, err
	}
	if c.idleTimer != nil {
		c.idleTimer.Stop()
		c.idleTimer = nil
	}

	var contextID cdp.BrowserContextID
	var targetID target.ID
	err := chromedp.Run(c.master, chromedp.ActionFunc(func(ctx context.Context) error {
		// Target.* context management is a BROWSER-session domain — route via
		// the browser executor, not the page session (else: Not allowed).
		bctx := cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser)
		id, err := target.CreateBrowserContext().Do(bctx)
		if err != nil {
			return err
		}
		contextID = id
		// Route downloads into the bed's own workspace.
		_ = browser.SetDownloadBehavior(browser.SetDownloadBehaviorBehaviorAllow).
			WithDownloadPath(filepath.Join(workspace, "downloads")).
			WithBrowserContextID(id).Do(bctx)
		// New headless refuses tab creation in a fresh context without an
		// explicit window ("no browser is open").
		targetID, err = target.CreateTarget("about:blank").WithBrowserContextID(id).WithNewWindow(true).Do(bctx)
		return err
	}))
	if err != nil {
		return nil, fmt.Errorf("amenity: chromium context for bed %s: %w", bedID, err)
	}
	tabCtx, tabStop := chromedp.NewContext(c.master, chromedp.WithTargetID(targetID))
	// Attach the target on the LONG-LIVED tab context now. Otherwise the first
	// per-action call would attach on its short-lived timeout context, and the
	// attach would be torn down when that context is cancelled — every action
	// after the first would hang (no session).
	if err := chromedp.Run(tabCtx); err != nil {
		tabStop()
		return nil, fmt.Errorf("amenity: chromium attach tab for bed %s: %w", bedID, err)
	}
	t := &chromiumTenant{bedID: bedID, contextID: contextID, tabCtx: tabCtx, tabStop: tabStop,
		cdpToken: randx.Hex(16)}
	c.tenants[bedID] = t
	return t, nil
}

// CDPToken implements Browser: ensure the bed's tenant and hand out the secret
// that authorizes its proxied CDP endpoint.
func (c *chromium) CDPToken(bedID, workspace string) (string, error) {
	if !c.proxyable() {
		return "", fmt.Errorf("amenity: chromium CDP proxy unavailable (launch mode needs --chromium-debug-port)")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t, err := c.tenant(bedID, workspace)
	if err != nil {
		return "", err
	}
	return t.cdpToken, nil
}

// proxyable reports whether a stable upstream CDP endpoint exists: attach mode
// always has one (CDPURL), launch mode only with a fixed debug port.
func (c *chromium) proxyable() bool {
	return c.attach || c.cfg.DebugPort > 0
}

// upstreamHTTPBase is the browser's own devtools HTTP endpoint ("/json/version"
// lives there). Valid only when proxyable().
func (c *chromium) upstreamHTTPBase() string {
	if c.attach {
		return strings.TrimSuffix(c.cfg.CDPURL, "/")
	}
	return "http://127.0.0.1:" + strconv.Itoa(c.cfg.DebugPort)
}

// upstreamWSURL resolves the browser-level websocket URL via /json/version.
// Resolved per proxy session, not cached: it changes whenever the shared
// browser restarts (crash supervision, idle-stop).
func (c *chromium) upstreamWSURL(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.upstreamHTTPBase()+"/json/version", nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("amenity: chromium /json/version: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &v); err != nil || v.WebSocketDebuggerURL == "" {
		return "", fmt.Errorf("amenity: chromium /json/version: no webSocketDebuggerUrl (err=%v)", err)
	}
	return v.WebSocketDebuggerURL, nil
}

// ServeCDP implements Browser: authenticate the token, then bridge the client
// websocket to the shared browser filtered to this bed's contexts.
func (c *chromium) ServeCDP(conn net.Conn, bedID, token string) error {
	defer conn.Close()
	c.mu.Lock()
	t, ok := c.tenants[bedID]
	authorized := ok && token != "" && subtle.ConstantTimeCompare([]byte(t.cdpToken), []byte(token)) == 1
	var contextID cdp.BrowserContextID
	if authorized {
		contextID = t.contextID
	}
	c.mu.Unlock()
	if !authorized {
		// The token is minted with the tenant and only handed out via
		// browser/info; a mismatch means a guessed bed id or a stale token
		// (tenant re-created). Refuse — never fall back to unfiltered CDP.
		return fmt.Errorf("amenity: chromium CDP: unauthorized for bed %s", bedID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	upstream, err := c.upstreamWSURL(ctx)
	cancel()
	if err != nil {
		return err
	}
	return proxyCDP(conn, upstream, bedID, string(contextID))
}

// AcquireTenant implements Amenity.
func (c *chromium) AcquireTenant(bedID, workspace string) (Tenant, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tenant(bedID, workspace)
}

// ReleaseTenant implements Amenity (and Browser): dispose the bed's browser
// context; the last tenant arms the idle-stop timer for launched browsers.
func (c *chromium) ReleaseTenant(bedID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tenants[bedID]
	if !ok {
		return nil
	}
	delete(c.tenants, bedID)
	if c.state == StateRunning {
		_ = chromedp.Run(c.master, chromedp.ActionFunc(func(ctx context.Context) error {
			bctx := cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser)
			return target.DisposeBrowserContext(t.contextID).Do(bctx)
		}))
	}
	t.tabStop()
	if len(c.tenants) == 0 && !c.attach && c.cfg.IdleStop > 0 && c.state == StateRunning {
		c.idleTimer = time.AfterFunc(c.cfg.IdleStop, func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			// Re-check under lock: a tenant may have arrived meanwhile.
			if len(c.tenants) == 0 && c.state == StateRunning {
				log.Printf("amenity: chromium idle for %s, stopping", c.cfg.IdleStop)
				c.stopLocked()
			}
		})
	}
	return nil
}

// run executes actions in the bed's tab with the action timeout applied.
func (c *chromium) run(ctx context.Context, bedID, workspace string, actions ...chromedp.Action) error {
	c.mu.Lock()
	t, err := c.tenant(bedID, workspace)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	actx, cancel := context.WithTimeout(t.tabCtx, c.cfg.ActionTimeout)
	defer cancel()
	// Honor the caller's cancellation too (HTTP request context).
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-actx.Done():
		}
	}()
	return chromedp.Run(actx, actions...)
}

func (c *chromium) Goto(ctx context.Context, bedID, workspace, url string) (string, string, error) {
	var title, loc string
	err := c.run(ctx, bedID, workspace,
		chromedp.Navigate(url),
		chromedp.Title(&title),
		chromedp.Location(&loc),
	)
	return title, loc, err
}

func (c *chromium) Text(ctx context.Context, bedID, workspace string) (string, error) {
	var text string
	err := c.run(ctx, bedID, workspace,
		chromedp.Text("body", &text, chromedp.ByQuery))
	return text, err
}

func (c *chromium) Click(ctx context.Context, bedID, workspace, selector string) error {
	return c.run(ctx, bedID, workspace,
		chromedp.WaitVisible(selector, chromedp.ByQuery),
		chromedp.Click(selector, chromedp.ByQuery))
}

func (c *chromium) Type(ctx context.Context, bedID, workspace, selector, text string, clear bool) error {
	actions := []chromedp.Action{chromedp.WaitVisible(selector, chromedp.ByQuery)}
	if clear {
		// Focus the node, then empty the FOCUSED element — no selector goes
		// into JS (no injection), and it's deterministic where chromedp.Clear
		// / SetValue are flaky in headless. SendKeys after still fires real
		// keyboard events, which SPAs rely on.
		actions = append(actions,
			chromedp.Focus(selector, chromedp.ByQuery),
			chromedp.Evaluate(`document.activeElement && (document.activeElement.value = "")`, nil))
	}
	actions = append(actions, chromedp.SendKeys(selector, text, chromedp.ByQuery))
	return c.run(ctx, bedID, workspace, actions...)
}

// namedKeys maps friendly key names to their key-event runes; anything not
// listed is sent as a literal (a single char like "a" works as-is).
var namedKeys = map[string]string{
	"Enter": kb.Enter, "Tab": kb.Tab, "Escape": kb.Escape, "Backspace": kb.Backspace,
	"Delete": kb.Delete, "ArrowDown": kb.ArrowDown, "ArrowUp": kb.ArrowUp,
	"ArrowLeft": kb.ArrowLeft, "ArrowRight": kb.ArrowRight, "PageDown": kb.PageDown,
	"PageUp": kb.PageUp, "Home": kb.Home, "End": kb.End,
}

func (c *chromium) Press(ctx context.Context, bedID, workspace, key string) error {
	send := key
	if mapped, ok := namedKeys[key]; ok {
		send = mapped
	}
	return c.run(ctx, bedID, workspace, chromedp.KeyEvent(send))
}

func (c *chromium) Scroll(ctx context.Context, bedID, workspace string, dx, dy int) error {
	// Numeric-only interpolation — no injection surface.
	js := fmt.Sprintf("window.scrollBy(%d, %d)", dx, dy)
	return c.run(ctx, bedID, workspace, chromedp.Evaluate(js, nil))
}

func (c *chromium) Wait(ctx context.Context, bedID, workspace, selector string) error {
	return c.run(ctx, bedID, workspace, chromedp.WaitVisible(selector, chromedp.ByQuery))
}

func (c *chromium) Screenshot(ctx context.Context, bedID, workspace, relPath string) (string, error) {
	if relPath == "" {
		relPath = fmt.Sprintf("screenshots/shot-%d.png", time.Now().UnixMilli())
	}
	rel := filepath.ToSlash(filepath.Clean(relPath))
	if strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("amenity: screenshot path escapes the workspace: %q", relPath)
	}
	var buf []byte
	if err := c.run(ctx, bedID, workspace, chromedp.CaptureScreenshot(&buf)); err != nil {
		return "", err
	}
	dst := filepath.Join(workspace, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(dst, buf, 0o644); err != nil {
		return "", err
	}
	return "/workspace/" + rel, nil
}

var (
	_ Amenity = (*chromium)(nil)
	_ Browser = (*chromium)(nil)
)
