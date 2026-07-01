// adtention client. One static binary, no runtime deps (replaces the bash + jq scripts).
//
// Subcommands (wired by the plugin):
//
//	status   statusLine command. Reads cache, prints the line. Never hits the network.
//	prompt   UserPromptSubmit hook. Silent. Spawns a detached `refresh`.
//	refresh  background worker. Classifies locally, calls the API, writes the cache.
//	setup    SessionStart hook. Installs the statusLine into the user's settings.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const (
	defaultAPI = "https://api.adtention.ai"
	minDwellS  = 15
	renderTTLs = 300 // statusLine re-renders ~every 10s in a live terminal; only bill if it rendered within this window
	dailyNote  = ""  // server enforces the daily cap
)

var categories = []string{"web3", "web", "devops", "data", "systems", "general"}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// ctrlRe matches every C0 control byte (incl. ESC, tab, newline) and DEL. Server-supplied ad
// copy is single-line plain text, so stripping these neutralizes terminal escape injection on
// render and keeps tabs/newlines out of the TSV impressions log.
var ctrlRe = regexp.MustCompile(`[\x00-\x1f\x7f]`)

func sanitizeAd(s string) string { return strings.TrimSpace(ctrlRe.ReplaceAllString(s, "")) }

// adTailRe matches a trailing " → domain" in ad copy. Stripped at render time: the visible
// domain is display-only (and not clickable in most terminals), and the real destination is
// the cached click URL behind /info. Leaving it out of the stored copy keeps old clients,
// which render the text verbatim, working unchanged.
var adTailRe = regexp.MustCompile(` → \S+$`)

// version is stamped at build time via -ldflags "-X main.version=...".
// Defaults to "dev" for local/unstamped builds.
var version = "dev"

// clientTag identifies the originating tool to the server (stamped on the publisher at
// register and on every impression at serve). The server sanitizes it to a slug.
const clientTag = "claude-code"

func main() {
	if len(os.Args) < 2 {
		return
	}
	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Println(version)
		return
	}
	dir := cacheDir()
	migrateOldCache(dir)
	os.MkdirAll(dir, 0o755)

	switch os.Args[1] {
	case "status":
		cmdStatus(dir)
	case "prompt":
		cmdPrompt(dir)
	case "refresh":
		cmdRefresh(dir)
	case "setup":
		cmdSetup(dir)
	case "open":
		cmdOpen(dir)
	case "key":
		cmdKey(dir)
	}
}

// ---------- shared helpers ----------

func home() string { h, _ := os.UserHomeDir(); return h }

func cacheDir() string {
	if c := os.Getenv("ADTENTION_CACHE"); c != "" {
		return c
	}
	return filepath.Join(home(), ".claude", "adtention")
}

func apiURL() string {
	if a := os.Getenv("ADTENTION_API"); a != "" {
		return a
	}
	return defaultAPI
}

// one-time migration from the pre-rename cache dir, preserving identity/balance
func migrateOldCache(dir string) {
	def := filepath.Join(home(), ".claude", "adtention")
	old := filepath.Join(home(), ".claude", "adline")
	if dir != def {
		return
	}
	if _, err := os.Stat(old); err == nil {
		if _, err2 := os.Stat(def); os.IsNotExist(err2) {
			os.Rename(old, def)
		}
	}
}

func readFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(b), "\n")
}

func writeFile(p, s string) { atomicWrite(p, []byte(s)) }

// atomicWrite writes b to p via a temp file + rename so a reader never sees a partial file and a
// crash can't truncate the target. The temp lives in the same dir (rename is atomic only within a
// filesystem); CreateTemp gives 0600 and the rename replaces a pre-existing symlink rather than
// writing through it. Falls back to a direct write if the temp can't be created.
func atomicWrite(p string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return os.WriteFile(p, b, 0o600)
	}
	tmp := f.Name()
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func visWidth(s string) int {
	return len([]rune(ansiRe.ReplaceAllString(s, "")))
}

// renderPath is the per-session render-heartbeat file. Keyed by session_id so a terminal
// session can't make a concurrent app session (same shared cache dir) look "rendered".
// Falls back to a shared key when session_id is absent (older hosts): degrades to
// per-machine, never over-gates.
func renderPath(dir, sessionID string) string {
	key := sanitizeKey(sessionID)
	if key == "" {
		key = "shared"
	}
	return filepath.Join(dir, "render_"+key)
}

// sanitizeKey keeps only filename-safe chars (session_id is a uuid, but be defensive).
func sanitizeKey(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return -1
	}, s)
}

// pruneRenders deletes per-session heartbeats from long-dead sessions so they don't pile up.
func pruneRenders(dir string) {
	matches, _ := filepath.Glob(filepath.Join(dir, "render_*"))
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, p := range matches {
		if fi, err := os.Stat(p); err == nil && fi.ModTime().Before(cutoff) {
			os.Remove(p)
		}
	}
}

// ---------- status: the render path ----------

type statusInput struct {
	SessionID string `json:"session_id"`
	Model     struct {
		DisplayName string `json:"display_name"`
	} `json:"model"`
	ContextWindow struct {
		UsedPercentage float64 `json:"used_percentage"`
	} `json:"context_window"`
	RateLimits struct {
		SevenDay struct {
			UsedPercentage float64 `json:"used_percentage"`
		} `json:"seven_day"`
	} `json:"rate_limits"`
}

func cmdStatus(dir string) {
	// self-cleanup: if the plugin has been uninstalled, the orphaned statusLine in the
	// user's settings would otherwise keep rendering. Remove (or restore) it and print nothing.
	if !pluginInstalled() {
		deregister(dir)
		return
	}

	raw, _ := io.ReadAll(os.Stdin)
	var in statusInput
	json.Unmarshal(raw, &in)

	// Render heartbeat, keyed by session: reaching here means the host actually renders our
	// statusLine for THIS session (terminal Claude Code). Surfaces that run our hooks but show
	// no statusLine (e.g. the Claude desktop app) never invoke status, so their session never
	// writes this, and refresh refuses to bill an impression for an ad that was never on screen.
	// Per-session (not per-machine) so a terminal session can't make a concurrent app session
	// in the same shared cache dir look "rendered".
	writeFile(renderPath(dir, in.SessionID), fmt.Sprintf("%d", time.Now().Unix()))

	model := in.Model.DisplayName
	if model == "" {
		model = "?"
	}
	if i := strings.Index(model, " ("); i >= 0 {
		model = model[:i]
	}

	// Sanitize on read too: the cache file is an untrusted boundary (it could be tampered with, or
	// written by an older client), so never let stored bytes emit escape sequences to the terminal.
	ad := sanitizeAd(readFile(filepath.Join(dir, "current_ad.txt")))
	balseg := sanitizeAd(readFile(filepath.Join(dir, "balance_display")))

	cols := 80
	if c := os.Getenv("COLUMNS"); c != "" {
		if n, err := fmt.Sscanf(c, "%d", &cols); err != nil || n == 0 {
			cols = 80
		}
	}

	// build our slot from whichever parts exist (green balance, cyan ad); both protected
	slot, slotW := "", 0
	if balseg != "" {
		slot = fmt.Sprintf("\x1b[1;32m%s\x1b[0m", balseg)
		slotW = len([]rune(balseg))
	}
	if ad != "" {
		// pitch only (drop the display domain), then a dim "→ /info" call to action.
		pitch := adTailRe.ReplaceAllString(ad, "")
		cta := " → /info"
		piece := fmt.Sprintf("\x1b[36m%s\x1b[0m\x1b[2m%s\x1b[0m", pitch, cta)
		w := len([]rune(pitch)) + len([]rune(cta))
		if slot != "" {
			slot += "  " + piece
			slotW += 2 + w
		} else {
			slot = piece
			slotW = w
		}
	}
	gap := 0
	if slot != "" {
		gap = 2
	}

	wrapped := readFile(filepath.Join(dir, "wrapped_cmd"))
	if wrapped != "" {
		their := runWrapped(wrapped, raw)
		if slot == "" {
			fmt.Print(their)
			return
		}
		if !strings.Contains(their, "\n") && visWidth(their)+slotW+2 <= cols {
			fmt.Printf("%s  %s", their, slot)
		} else {
			fmt.Printf("%s\n%s", their, slot)
		}
		return
	}

	// normal mode: our own segments, width-aware shed (drop model, then context; keep limit)
	vals := []string{model}
	if in.ContextWindow.UsedPercentage != 0 || strings.Contains(string(raw), "used_percentage") {
		vals = append(vals, fmt.Sprintf("context %d%%", round(in.ContextWindow.UsedPercentage)))
	}
	vals = append(vals, fmt.Sprintf("limit %d%%", round(in.RateLimits.SevenDay.UsedPercentage)))
	present := make([]bool, len(vals))
	for i := range present {
		present[i] = true
	}
	assemble := func() string {
		parts := []string{}
		for i, v := range vals {
			if present[i] && v != "" {
				parts = append(parts, v)
			}
		}
		return strings.Join(parts, " · ")
	}
	budget := cols - slotW - gap
	status := assemble()
	// drop from the front (model, then context), never the last (limit)
	for i := 0; i < len(vals)-1; i++ {
		if len([]rune(status)) <= budget {
			break
		}
		present[i] = false
		status = assemble()
	}
	if slot != "" {
		fmt.Printf("\x1b[2m%s\x1b[0m  %s", status, slot)
	} else {
		fmt.Printf("\x1b[2m%s\x1b[0m", status)
	}
}

func round(f float64) int { return int(f + 0.5) }

func runWrapped(cmdStr string, stdin []byte) string {
	c := exec.Command("/bin/sh", "-c", cmdStr)
	c.Stdin = bytes.NewReader(stdin)
	out, _ := c.Output()
	return strings.TrimRight(string(out), "\n")
}

// pluginInstalled reports whether the adtention plugin is still installed. On any read or
// parse failure it returns true (fail safe: never self-remove when we cannot be sure).
func pluginInstalled() bool {
	p := filepath.Join(home(), ".claude", "plugins", "installed_plugins.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return true
	}
	var d struct {
		Plugins map[string]any `json:"plugins"`
	}
	if json.Unmarshal(b, &d) != nil {
		return true
	}
	for k := range d.Plugins {
		if strings.Contains(k, "adtention") {
			return true
		}
	}
	return false
}

// deregister removes our statusLine from the user's settings (restoring a wrapped one if we
// saved it), but only if the current statusLine is actually ours.
func deregister(dir string) {
	settingsPath := filepath.Join(home(), ".claude", "settings.json")
	b, err := os.ReadFile(settingsPath)
	if err != nil {
		return
	}
	var settings map[string]any
	if json.Unmarshal(b, &settings) != nil {
		return
	}
	sl, ok := settings["statusLine"].(map[string]any)
	if !ok {
		return
	}
	cmd, _ := sl["command"].(string)
	if !strings.Contains(cmd, "adtention") && !strings.Contains(cmd, "adline") {
		return // someone else's statusLine now; leave it
	}
	if pb, err := os.ReadFile(filepath.Join(dir, "prev_statusline.json")); err == nil {
		var prev any
		if json.Unmarshal(pb, &prev) == nil && prev != nil {
			settings["statusLine"] = prev // restore the user's original
		} else {
			delete(settings, "statusLine")
		}
	} else {
		delete(settings, "statusLine")
	}
	if out, err := json.MarshalIndent(settings, "", "  "); err == nil {
		atomicWrite(settingsPath, out)
	}
}

// ---------- prompt: the UserPromptSubmit hook (silent, detached) ----------

func cmdPrompt(dir string) {
	raw, _ := io.ReadAll(os.Stdin)
	var in struct {
		Cwd            string `json:"cwd"`
		TranscriptPath string `json:"transcript_path"`
		SessionID      string `json:"session_id"`
	}
	json.Unmarshal(raw, &in)
	cwd := in.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	// pass session_id so refresh can check this session's render heartbeat (see cmdStatus)
	c := exec.Command(self, "refresh", cwd, in.TranscriptPath, in.SessionID)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach
	c.Start()
	// do not Wait: fire and forget. Print nothing (stdout is injected into the prompt).
}

// cmdOpen opens the current sponsor's click URL in the browser. It is invoked by the
// /adtention:sponsor command (a !`...` shell call in the command file), so it runs as its
// own short-lived process: launch the browser, print one line. The click URL was cached by
// the last refresh and 302-redirects through the server, so the click is attributable.
func cmdOpen(dir string) {
	click := readFile(filepath.Join(dir, "current_click.txt"))
	if click == "" {
		fmt.Println("adtention: no sponsor to open yet. Send a prompt first, then try again.")
		return
	}
	url := click
	if strings.HasPrefix(url, "/") {
		url = apiURL() + url // server-relative path → our API host (https)
	}
	// Only hand http(s) URLs to the OS opener. A server (or MITM, or ADTENTION_API override) must
	// not be able to launch file://, smb://, or a custom protocol handler.
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		fmt.Println("adtention: refusing to open a non-web sponsor link.")
		return
	}
	openURL(url)
	fmt.Println("adtention: opened the current sponsor in your browser.")
}

// cmdKey prints this install's publisher_id + secret so the user can link it to their account and
// claim its earnings on the portal. Read-only: it surfaces the existing local identity, it does not
// register or change anything. Printing the secret is safe by design: it's the one-time claim proof,
// and linking is write-once server-side (a claimed install can't be re-linked to another account),
// so a secret later seen in the transcript is inert. Prints ONLY on this explicit invocation, never
// in the status line or a hook.
func cmdKey(dir string) {
	var id struct {
		PublisherID string `json:"publisher_id"`
		Secret      string `json:"secret"`
	}
	if b, err := os.ReadFile(filepath.Join(dir, "identity.json")); err == nil {
		json.Unmarshal(b, &id)
	}
	if id.PublisherID == "" || id.Secret == "" {
		fmt.Println("adtention: no install identity yet. Open Claude Code and send one prompt to register your install, then run this again.")
		return
	}
	// Show the cached balance so the user can confirm THIS is the install they mean before linking
	// (e.g. when they run more than one install). Cache-fresh as of the last prompt; the server
	// stays authoritative.
	var balUSD float64
	if bal := readFile(filepath.Join(dir, "balance")); bal != "" {
		fmt.Sscanf(bal, "%f", &balUSD)
	}
	fmt.Println("Your ADtention publisher key. Link it to claim and cash out your earnings.")
	fmt.Println()
	fmt.Println("  publisher_id:  " + id.PublisherID)
	fmt.Println("  secret:        " + id.Secret)
	fmt.Printf("  balance:       $%.2f\n", balUSD)
	fmt.Println()
	fmt.Println("Link at:  https://app.adtention.ai/earn/link")
}

// openURL launches the default browser for u (best effort; errors are ignored).
func openURL(u string) {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", u)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		c = exec.Command("xdg-open", u)
	}
	c.Run() // wait: the opener hands off quickly, and we os.Exit right after
}

// ---------- refresh: classify locally, call the API, write the cache ----------

func cmdRefresh(dir string) {
	cwd, transcript, sessionID := "", "", ""
	if len(os.Args) > 2 {
		cwd = os.Args[2]
	}
	if len(os.Args) > 3 {
		transcript = os.Args[3]
	}
	if len(os.Args) > 4 {
		sessionID = os.Args[4]
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	// single-flight lock with stale recovery
	lock := filepath.Join(dir, "refresh.lock")
	if fi, err := os.Stat(lock); err == nil {
		if time.Since(fi.ModTime()) > 60*time.Second {
			os.Remove(lock)
		}
	}
	if err := os.Mkdir(lock, 0o755); err != nil {
		return
	}
	defer os.Remove(lock)

	pruneRenders(dir)
	api := apiURL()

	// Identity registration is one-time and NON-BILLABLE, so it runs BEFORE the render gate.
	// The gate below exists only to suppress BILLING for impressions nobody saw (e.g. the Claude
	// desktop app runs our hooks but renders no statusLine). Registration records no impression
	// and bills nothing, so gating it only causes harm: a user whose statusLine hasn't rendered
	// at the instant refresh runs (first-prompt race, a >TTL idle gap, a brief desktop visit)
	// would otherwise never get an identity and never appear as a publisher. Register first; gate
	// only the serve.
	idFile := filepath.Join(dir, "identity.json")
	pub := readPublisher(idFile)
	if pub == "" {
		ref := readRefCode(dir)
		pub = registerAndSave(api, idFile, ref)
		if pub != "" {
			os.Remove(filepath.Join(dir, "ref")) // one-shot: consume the invite, never re-attribute
		}
	}
	if pub == "" {
		return // server unreachable and no identity
	}

	// Bill only for ads actually on screen. status writes a render heartbeat for its session
	// whenever our statusLine is drawn; if THIS session's heartbeat is missing or stale, the host
	// runs our hooks but shows no statusLine (e.g. the Claude desktop app), so we must not serve
	// or record an impression nobody saw. (Registration above is exempt: it bills nothing.)
	if r := readFile(renderPath(dir, sessionID)); r == "" {
		return
	} else {
		var ts int64
		fmt.Sscanf(r, "%d", &ts)
		if time.Now().Unix()-ts >= renderTTLs {
			return
		}
	}

	category, source := classify(cwd, transcript)

	// dwell / frequency cap
	last := readFile(filepath.Join(dir, "last_serve"))
	now := time.Now().Unix()
	if last != "" {
		var lv int64
		fmt.Sscanf(last, "%d", &lv)
		if now-lv < minDwellS {
			return
		}
	}
	writeFile(filepath.Join(dir, "last_serve"), fmt.Sprintf("%d", now))

	nonce := fmt.Sprintf("%d-%s", now, randHex(4))
	resp := serve(api, pub, category, nonce)
	if strings.Contains(resp, "unknown_publisher") {
		pub = registerAndSave(api, idFile, "") // self-heal: re-register, no re-attribution
		if pub != "" {
			resp = serve(api, pub, category, nonce+"-r")
		}
	}
	if resp == "" {
		return // unreachable: keep last cached ad
	}

	var r struct {
		Text       string  `json:"text"`
		BalanceUSD float64 `json:"balance_usd"`
		ClickURL   string  `json:"click_url"`
		ImpID      string  `json:"impression_id"`
	}
	json.Unmarshal([]byte(resp), &r)

	// Server content is untrusted at the terminal boundary: strip control bytes from the ad copy
	// before it is ever cached, rendered, or written to the TSV log (no escape injection, no
	// tab/newline log injection).
	r.Text = sanitizeAd(r.Text)

	// click target for /sponsor (and OSC 8 links). Server returns click_url on a fresh
	// serve; on a dedup it only returns impression_id, so derive it then.
	click := sanitizeAd(r.ClickURL)
	if click == "" && r.ImpID != "" {
		click = "/v1/click/" + r.ImpID
	}

	if strings.Contains(resp, "balance_usd") {
		writeFile(filepath.Join(dir, "balance"), fmt.Sprintf("%.5f", r.BalanceUSD))
		writeFile(filepath.Join(dir, "balance_display"), fmt.Sprintf("⊕ $%.2f", r.BalanceUSD))
	}
	if r.Text == "" {
		writeFile(filepath.Join(dir, "current_ad.txt"), "")    // no inventory: clear the slot
		writeFile(filepath.Join(dir, "current_click.txt"), "") // and its click target
		return
	}
	writeFile(filepath.Join(dir, "current_ad.txt"), r.Text)
	writeFile(filepath.Join(dir, "current_click.txt"), click)
	writeFile(filepath.Join(dir, "category.txt"), category)
	writeFile(filepath.Join(dir, "source.txt"), source)
	appendFile(filepath.Join(dir, "impressions.log"),
		fmt.Sprintf("%d\t%s\t%s\t%s\n", now, source, category, r.Text))
}

func appendFile(p, s string) {
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(s)
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func readPublisher(idFile string) string {
	var id struct {
		PublisherID string `json:"publisher_id"`
	}
	b, err := os.ReadFile(idFile)
	if err != nil {
		return ""
	}
	json.Unmarshal(b, &id)
	return id.PublisherID
}

func registerAndSave(api, idFile, ref string) string {
	body := fmt.Sprintf(`{"client":%q}`, clientTag) // owning tool for this publisher
	if ref != "" {
		body = fmt.Sprintf(`{"client":%q,"ref":%q}`, clientTag, ref) // attribute this install to the referrer
	}
	resp := post(api+"/v1/register", body)
	if resp == "" {
		return ""
	}
	// identity.json holds the whole register response (publisher_id, secret, referral_code)
	os.WriteFile(idFile, []byte(resp), 0o600)
	var id struct {
		PublisherID string `json:"publisher_id"`
	}
	json.Unmarshal([]byte(resp), &id)
	return id.PublisherID
}

// referral attribution: a code from $ADTENTION_REF, else the one-shot <cache>/ref file (written
// by the deep-link landing's prep step), rides the FIRST register only. Sanitized to the code
// alphabet so nothing untrusted reaches the request body.
func readRefCode(dir string) string {
	if v := os.Getenv("ADTENTION_REF"); v != "" {
		return sanitizeRef(v)
	}
	return sanitizeRef(readFile(filepath.Join(dir, "ref")))
}

func sanitizeRef(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteByte(byte(r))
			if b.Len() >= 32 {
				break
			}
		}
	}
	return b.String()
}

func serve(api, pub, category, nonce string) string {
	payload := fmt.Sprintf(`{"publisher_id":%q,"category":%q,"nonce":%q,"client":%q}`, pub, category, nonce, clientTag)
	return post(api+"/v1/serve", payload)
}

func post(url, body string) string {
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		return ""
	}
	req.Header.Set("content-type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// ---------- classification (local; only the resulting tag is ever sent) ----------

func classify(cwd, transcript string) (category, source string) {
	if transcript != "" {
		if c := classifyTopic(transcript); c != "" {
			return c, "topic"
		}
	}
	return classifyFolder(cwd), "folder"
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }
func glob(pat string) bool { m, _ := filepath.Glob(pat); return len(m) > 0 }

func classifyFolder(d string) string {
	switch {
	case exists(filepath.Join(d, "foundry.toml")) || glob(filepath.Join(d, "*.sol")) || glob(filepath.Join(d, "hardhat.config.*")):
		return "web3"
	case exists(filepath.Join(d, "Dockerfile")) || glob(filepath.Join(d, "*.tf")):
		return "devops"
	case exists(filepath.Join(d, "package.json")):
		return "web"
	case exists(filepath.Join(d, "requirements.txt")) || glob(filepath.Join(d, "*.py")):
		return "data"
	case exists(filepath.Join(d, "Cargo.toml")) || exists(filepath.Join(d, "go.mod")):
		return "systems"
	}
	return "general"
}

var topicPatterns = map[string]*regexp.Regexp{
	"web3":    regexp.MustCompile(`solidity|ethereum|web3|smart contract|defi|onchain|blockchain|wallet|stablecoin|crypto|erc-?20`),
	"web":     regexp.MustCompile(`react|tailwind|next\.js|frontend|vite|jsx|tsx|css|component`),
	"devops":  regexp.MustCompile(`docker|kubernetes|terraform|kubectl|nginx|ci/cd|pipeline|deployment`),
	"data":    regexp.MustCompile(`dataset|training data|pandas|embedding|inference|fine-tune|gpu|machine learning`),
	"systems": regexp.MustCompile(`goroutine|borrow checker|mutex|concurrency|memory safety|rustc`),
}

func classifyTopic(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) > 400 {
		lines = lines[len(lines)-400:]
	}
	text := strings.ToLower(strings.Join(lines, "\n"))

	// Iterate categories in fixed order and keep the first strict max, so equal scores tie-break
	// deterministically (map iteration order is random; the old sort over a map was non-stable).
	best, bestN := "", 0
	for _, cat := range categories {
		re := topicPatterns[cat]
		if re == nil {
			continue
		}
		if n := len(re.FindAllString(text, -1)); n > bestN {
			best, bestN = cat, n
		}
	}
	if bestN >= 3 {
		return best
	}
	return ""
}

// ---------- setup: install the statusLine into the user's settings ----------

func cmdSetup(dir string) {
	// show $0.00 from the first render
	bd := filepath.Join(dir, "balance_display")
	if !exists(bd) {
		writeFile(bd, "⊕ $0.00")
	}

	root := os.Getenv("CLAUDE_PLUGIN_ROOT")
	if root == "" {
		if self, err := os.Executable(); err == nil {
			root = filepath.Dir(filepath.Dir(self))
		}
	}
	self, _ := os.Executable()
	target := self
	if root != "" {
		target = filepath.Join(root, "bin", "adtention")
	}
	// Single-quote the path: the host runs this statusLine command through a shell, and Go's %q
	// emits a double-quoted string in which $(...), backticks, and $VAR still expand. A single-
	// quoted path can't, so a path containing shell metacharacters stays inert.
	cmdLine := shQuote(target) + " status"

	settingsPath := filepath.Join(home(), ".claude", "settings.json")
	var settings map[string]any
	if b, err := os.ReadFile(settingsPath); err == nil {
		// If the file exists but is malformed JSON, do NOT proceed: writing back would replace the
		// user's entire settings (permissions, env, model, hooks, MCP servers) with just our
		// statusLine. Bail and leave the file untouched. (deregister already guards the same way.)
		if json.Unmarshal(b, &settings) != nil {
			return
		}
	}
	if settings == nil {
		settings = map[string]any{}
	}

	current := ""
	if sl, ok := settings["statusLine"].(map[string]any); ok {
		if c, ok := sl["command"].(string); ok {
			current = c
		}
	}
	if current == cmdLine {
		return // already installed
	}

	// wrap a pre-existing statusLine, but never one of our own (any command that
	// references the plugin, which always lives under an "adtention" path)
	isOurs := strings.Contains(current, "adtention") || strings.Contains(current, "adline")
	if current != "" && !isOurs {
		writeFile(filepath.Join(dir, "wrapped_cmd"), current)
		prev := filepath.Join(dir, "prev_statusline.json")
		if !exists(prev) {
			if b, err := json.Marshal(settings["statusLine"]); err == nil {
				writeFile(prev, string(b))
			}
		}
	} else {
		os.Remove(filepath.Join(dir, "wrapped_cmd"))
	}

	settings["statusLine"] = map[string]any{
		"type":            "command",
		"command":         cmdLine,
		"refreshInterval": 10,
	}
	if b, err := json.MarshalIndent(settings, "", "  "); err == nil {
		atomicWrite(settingsPath, b)
	}
}

// shQuote wraps s in single quotes for safe use in a POSIX shell command, escaping any embedded
// single quote as '\”. Nothing inside single quotes is expanded by the shell.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
