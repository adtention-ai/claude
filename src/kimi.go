//go:build darwin || linux

// The Kimi Code surface: a PTY wrapper, ported from adtention-ai/kimi's adkimi.py
// (the working v1 and reference spec - its README lists the hard-won terminal
// lessons; do not relearn them here).
//
// Kimi Code has no statusline/plugin-UI API and runs no third-party code
// in-process, so this surface owns the terminal instead: kimi runs inside a PTY
// reporting one row FEWER than the real terminal, scrolling is confined to those
// rows with a DECSTBM region, and the sponsor line is painted on the reserved
// bottom row. Kimi never knows the row exists; the line never enters the model
// context (zero token cost).
//
// Economics are identical to the other surfaces: a serve is billable, so it only
// happens on a REAL prompt (Enter in the chatbox), dwell-gated; every Nth slot is
// a locally-rendered house unit that must NOT call /v1/serve; display is
// cache-first and offline-safe.
//
// Terminal contracts (violating either breaks Kimi):
//   - 0x07 (BEL) terminates OSC sequences. Terminals answer Kimi's startup color
//     queries on stdin with BEL-terminated replies: only a LONE 0x07 chunk is
//     ctrl+g, and forwarded bytes are never mutated (a stripped terminator hangs
//     Kimi's input parser mid-OSC).
//   - Repaint only after ~50ms of output quiet so our escapes never interleave
//     with a partially-written kimi frame (kimi batches with ?2026 sync marks).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

const kimiClientTag = "kimi"

var kimiHouseLines = []string{
	"Connect your account",
	"Refer a friend, earn together",
	"Cash out from your dashboard",
}

// ---------- paths & env ----------

func kimiEnv(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func kimiBin() string {
	return kimiEnv("ADTENTION_KIMI_BIN", filepath.Join(home(), ".kimi-code", "bin", "kimi"))
}

func adtentionHome() string {
	return kimiEnv("ADTENTION_HOME", filepath.Join(home(), ".adtention"))
}

func kimiPortal() string {
	return strings.TrimRight(kimiEnv("ADTENTION_PORTAL", "https://app.adtention.ai"), "/")
}

func kimiIdentityFile() string { return filepath.Join(adtentionHome(), "kimi-identity.json") }
func kimiCacheFile() string    { return filepath.Join(adtentionHome(), "kimi-sponsor.json") }

// ---------- state ----------

type kimiSponsor struct {
	Text string `json:"text"`
	URL  string `json:"url,omitempty"`
}

type kimiCache struct {
	Sponsor    *kimiSponsor `json:"sponsor"`
	Balance    float64      `json:"balance"`
	ServeCount int          `json:"serve_count"`
	Linked     bool         `json:"linked"`
}

type kimiState struct {
	mu        sync.Mutex
	sponsor   *kimiSponsor
	balance   float64
	count     int
	house     bool
	houseIdx  int
	publisher string
	linked    bool
	lastServe time.Time
}

func (s *kimiState) writeCacheLocked() {
	// The house unit's URL is never cached: it embeds the secret and the cache
	// file is not 0600 (the identity file is).
	b, err := json.Marshal(kimiCache{Sponsor: s.sponsor, Balance: s.balance, ServeCount: s.count, Linked: s.linked})
	if err != nil {
		return
	}
	os.MkdirAll(adtentionHome(), 0o755)
	atomicWrite(kimiCacheFile(), b)
}

// ---------- protocol (branded UA: Cloudflare 1010-blocks default library agents) ----------

func kimiPost(url, body string) string {
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		return ""
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("user-agent", "adtention-kimi/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b)
}

func kimiEnsureRegistered(st *kimiState) string {
	st.mu.Lock()
	if st.publisher != "" {
		p := st.publisher
		st.mu.Unlock()
		return p
	}
	st.mu.Unlock()
	if p := readPublisher(kimiIdentityFile()); p != "" {
		st.mu.Lock()
		st.publisher = p
		st.mu.Unlock()
		return p
	}
	body := fmt.Sprintf(`{"client":%q}`, kimiClientTag)
	if ref := sanitizeRef(os.Getenv("ADTENTION_REF")); ref != "" {
		body = fmt.Sprintf(`{"client":%q,"ref":%q}`, kimiClientTag, ref)
	}
	resp := kimiPost(apiURL()+"/v1/register", body)
	if resp == "" {
		return ""
	}
	var id struct {
		PublisherID string `json:"publisher_id"`
	}
	json.Unmarshal([]byte(resp), &id)
	if id.PublisherID == "" {
		return ""
	}
	os.MkdirAll(adtentionHome(), 0o755)
	os.WriteFile(kimiIdentityFile(), []byte(resp), 0o600) // whole response: publisher_id, secret, referral_code
	st.mu.Lock()
	st.publisher = id.PublisherID
	st.mu.Unlock()
	return id.PublisherID
}

func kimiApplyServe(st *kimiState, resp string) bool {
	var data struct {
		Text         string  `json:"text"`
		BalanceUSD   float64 `json:"balance_usd"`
		ClickURL     string  `json:"click_url"`
		ImpressionID string  `json:"impression_id"`
		Linked       *bool   `json:"linked"`
	}
	if json.Unmarshal([]byte(resp), &data) != nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.house = false // a fresh sponsor replaces the connect unit
	if data.Linked != nil {
		st.linked = *data.Linked
	}
	if data.BalanceUSD > st.balance {
		st.balance = data.BalanceUSD // floor: never visually wipe a shown balance
	}
	text := sanitizeAd(data.Text)
	if text == "" {
		st.writeCacheLocked()
		return true
	}
	click := sanitizeAd(data.ClickURL)
	if click == "" && data.ImpressionID != "" {
		click = "/v1/click/" + data.ImpressionID
	}
	if strings.HasPrefix(click, "/") {
		click = apiURL() + click
	}
	if !strings.HasPrefix(click, "http://") && !strings.HasPrefix(click, "https://") {
		click = ""
	}
	st.sponsor = &kimiSponsor{Text: text, URL: click}
	st.writeCacheLocked()
	return true
}

func kimiServeImpression(st *kimiState, cwd string, wake chan<- struct{}) {
	defer func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}()
	pub := kimiEnsureRegistered(st)
	if pub == "" {
		st.mu.Lock()
		st.lastServe = time.Time{} // registration failed; let the next prompt retry
		st.mu.Unlock()
		return
	}
	serveOnce := func(p string) string {
		body := fmt.Sprintf(`{"publisher_id":%q,"category":%q,"nonce":%q,"client":%q}`,
			p, classifyFolder(cwd), fmt.Sprintf("%d-%s", time.Now().UnixMilli(), randHex(4)), kimiClientTag)
		return kimiPost(apiURL()+"/v1/serve", body)
	}
	resp := serveOnce(pub)
	if strings.Contains(resp, "unknown_publisher") {
		// self-heal: identity dropped server-side; re-register and retry once
		st.mu.Lock()
		st.publisher = ""
		st.mu.Unlock()
		os.Remove(kimiIdentityFile())
		if pub2 := kimiEnsureRegistered(st); pub2 != "" {
			resp = serveOnce(pub2)
		}
	}
	if resp != "" {
		kimiApplyServe(st, resp)
	}
}

// onPrompt fills the slot if the dwell window passed: every HOUSE_EVERY-th slot is
// the local connect unit (no serve, no billing); the rest are billable serves.
func kimiOnPrompt(st *kimiState, cwd string, dwell time.Duration, houseEvery int, wake chan<- struct{}) {
	st.mu.Lock()
	if time.Since(st.lastServe) < dwell {
		st.mu.Unlock()
		return
	}
	st.lastServe = time.Now()
	st.count++
	isHouse := houseEvery > 0 && st.count%houseEvery == 0
	if isHouse {
		st.house = true
		st.houseIdx = (st.count/houseEvery - 1) % len(kimiHouseLines)
		st.writeCacheLocked()
	}
	st.mu.Unlock()
	if isHouse {
		select {
		case wake <- struct{}{}:
		default:
		}
		return
	}
	go kimiServeImpression(st, cwd, wake)
}

// ---------- links ----------

// earnURL: composed at press/draw time from the local identity (the secret exists
// ONLY on this machine). Fragment, not query: the hash never reaches server logs
// or Referer headers. Falls back to the bare earn page pre-register.
func kimiEarnURL() string {
	var id struct {
		PublisherID string `json:"publisher_id"`
		Secret      string `json:"secret"`
	}
	if b, err := os.ReadFile(kimiIdentityFile()); err == nil {
		json.Unmarshal(b, &id)
	}
	if id.PublisherID != "" && id.Secret != "" {
		return kimiPortal() + "/earn#pub=" + id.PublisherID + "&secret=" + id.Secret
	}
	return kimiPortal() + "/earn"
}

func kimiOpenURL(u string) {
	if u == "" || (!strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://")) {
		u = "https://adtention.ai"
	}
	if cmd := os.Getenv("ADTENTION_OPEN_CMD"); cmd != "" { // test override
		exec.Command(cmd, u).Start()
		return
	}
	go openURL(u)
}

// ---------- rendering ----------

func kimiOSC8(url, text string) string {
	return "\x1b]8;;" + url + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}

// drawAd paints the sponsor line on the real bottom row without moving Kimi's
// cursor. Layout (single left-aligned unit, copy verbatim - brand leads it):
//
//	⊕ $0.03  Tailscale: VPN with 0 config → ctrl+g
//	⊕ $0.03  Connect your account → ctrl+e        (house slot)
func kimiDrawAd(st *kimiState, out *os.File, rows, cols int) {
	st.mu.Lock()
	sponsor, balance, house, houseIdx, linked := st.sponsor, st.balance, st.house, st.houseIdx, st.linked
	st.mu.Unlock()

	var adText, url, key string
	switch {
	case house:
		// Once the server says this install is claimed, the "Connect" nudge is
		// retired and the rotation continues over the remaining variants.
		lines := kimiHouseLines
		if linked {
			lines = kimiHouseLines[1:]
		}
		adText, url, key = lines[houseIdx%len(lines)], "", "ctrl+e"
	case sponsor != nil:
		adText, url, key = sponsor.Text, sponsor.URL, "ctrl+g"
	default:
		adText, url, key = "adtention", "https://adtention.ai", "ctrl+g"
	}
	prefix := fmt.Sprintf("⊕ $%.2f  ", balance)
	suffix := " → " + key
	// Width math on VISIBLE runes only; escapes are added after truncation.
	budget := cols - 1 - len([]rune(prefix)) - len([]rune(suffix))
	if r := []rune(adText); len(r) > budget {
		if budget > 1 {
			adText = string(r[:budget-1]) + "…"
		} else {
			adText = ""
		}
	}
	ad := adText
	if url != "" {
		ad = kimiOSC8(url, adText)
	}
	line := "\x1b7" + // save cursor
		fmt.Sprintf("\x1b[%d;1H", rows) + // jump to real last row
		"\x1b[2K" + // clear it
		"\x1b[2m" + prefix + ad + suffix + "\x1b[0m" +
		"\x1b8" // restore cursor
	out.WriteString(line)
}

// ---------- the wrapper ----------

func cmdKimi(args []string) {
	real := kimiBin()
	if _, err := os.Stat(real); err != nil {
		fmt.Fprintln(os.Stderr, "adtention: kimi not found at "+real)
		os.Exit(1)
	}
	// Non-interactive (pipes, scripts) or explicit opt-out: passthrough untouched.
	if os.Getenv("ADTENTION_DISABLE") != "" ||
		!term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		syscall.Exec(real, append([]string{real}, args...), os.Environ())
		return
	}

	dwell := 15 * time.Second
	if v, err := strconv.Atoi(kimiEnv("ADTENTION_DWELL_MS", "15000")); err == nil {
		dwell = time.Duration(v) * time.Millisecond
	}
	houseEvery := 10
	if v, err := strconv.Atoi(kimiEnv("ADTENTION_HOUSE_EVERY", "10")); err == nil {
		houseEvery = v
	}
	cwd, _ := os.Getwd()

	st := &kimiState{}
	// 1. Hydrate display from cache - instant, works offline.
	if b, err := os.ReadFile(kimiCacheFile()); err == nil {
		var c kimiCache
		if json.Unmarshal(b, &c) == nil {
			st.sponsor, st.balance, st.count = c.Sponsor, c.Balance, c.ServeCount
			st.linked = c.Linked
		}
	}
	// 2. Register ahead of the first prompt (non-billable).
	go kimiEnsureRegistered(st)

	rows, cols := 24, 80
	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		cols, rows = w, h
	}

	c := exec.Command(real, args...)
	master, err := pty.StartWithSize(c, &pty.Winsize{
		Rows: uint16(max(rows-1, 3)), Cols: uint16(cols), // one row fewer: the last row is ours
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "adtention: "+err.Error())
		os.Exit(1)
	}
	defer master.Close()

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		c.Process.Kill()
		os.Exit(1)
	}
	var outMu sync.Mutex // stdout writes: copy loop vs draw
	restore := func() {
		term.Restore(int(os.Stdin.Fd()), oldState)
		outMu.Lock()
		os.Stdout.WriteString("\x1b[r")                       // clear scroll region
		fmt.Fprintf(os.Stdout, "\x1b[%d;1H\x1b[2K", rows)     // wipe the ad row
		outMu.Unlock()
	}
	setRegion := func() {
		outMu.Lock()
		fmt.Fprintf(os.Stdout, "\x1b[1;%dr", rows-1)
		outMu.Unlock()
	}
	draw := func() {
		outMu.Lock()
		kimiDrawAd(st, os.Stdout, rows, cols)
		outMu.Unlock()
	}
	setRegion()
	draw()

	wake := make(chan struct{}, 1)
	var lastOut atomicTime
	lastOut.set(time.Now())
	dirty := make(chan struct{}, 1)
	markDirty := func() {
		select {
		case dirty <- struct{}{}:
		default:
		}
	}

	// Resize: recompute, re-shrink the child, re-confine scrolling, repaint.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for range winch {
			if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
				cols, rows = w, h
			}
			pty.Setsize(master, &pty.Winsize{Rows: uint16(max(rows-1, 3)), Cols: uint16(cols)})
			setRegion()
			draw()
		}
	}()

	// stdin -> kimi, with hotkey interception. A lone keypress arrives as a
	// single-byte read; anything longer (paste, OSC replies) is forwarded verbatim.
	go func() {
		buf := make([]byte, 65536)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil || n == 0 {
				return
			}
			d := buf[:n]
			if n == 1 && d[0] == 0x07 { // ctrl+g: open sponsor (or earn on a house slot)
				st.mu.Lock()
				sp, house := st.sponsor, st.house
				st.mu.Unlock()
				if house {
					kimiOpenURL(kimiEarnURL())
				} else if sp != nil {
					kimiOpenURL(sp.URL)
				} else {
					kimiOpenURL("")
				}
				continue // swallowed: kimi never sees it
			}
			if n == 1 && d[0] == 0x05 { // ctrl+e: connect earnings
				kimiOpenURL(kimiEarnURL())
				continue
			}
			master.Write(d)
			for _, b := range d {
				if b == '\r' || b == '\n' {
					kimiOnPrompt(st, cwd, dwell, houseEvery, wake)
					break
				}
			}
		}
	}()

	// Repaint scheduler: after output quiet (50ms), or on a serve/house wake.
	go func() {
		tick := time.NewTicker(25 * time.Millisecond)
		defer tick.Stop()
		pending := false
		for {
			select {
			case <-dirty:
				pending = true
			case <-wake:
				pending = true
			case <-tick.C:
				if pending && time.Since(lastOut.get()) > 50*time.Millisecond {
					draw()
					pending = false
				}
			}
		}
	}()

	// kimi -> stdout. EOF means kimi exited.
	buf := make([]byte, 65536)
	for {
		n, err := master.Read(buf)
		if n > 0 {
			outMu.Lock()
			os.Stdout.Write(buf[:n])
			outMu.Unlock()
			lastOut.set(time.Now())
			markDirty()
		}
		if err != nil {
			break
		}
	}
	restore()
	err = c.Wait()
	if ee, ok := err.(*exec.ExitError); ok {
		os.Exit(ee.ExitCode())
	}
}

// ---------- small helpers ----------

type atomicTime struct {
	mu sync.Mutex
	t  time.Time
}

func (a *atomicTime) set(t time.Time) { a.mu.Lock(); a.t = t; a.mu.Unlock() }
func (a *atomicTime) get() time.Time  { a.mu.Lock(); defer a.mu.Unlock(); return a.t }

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
