// adtention client. One static binary, no runtime deps (replaces the bash + jq scripts).
//
// Subcommands (wired by the plugin):
//   status   statusLine command. Reads cache, prints the line. Never hits the network.
//   prompt   UserPromptSubmit hook. Silent. Spawns a detached `refresh`.
//   refresh  background worker. Classifies locally, calls the API, writes the cache.
//   setup    SessionStart hook. Installs the statusLine into the user's settings.
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
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	defaultAPI = "https://api.adtention.ai"
	minDwellS  = 15
	renderTTLs = 300 // statusLine re-renders ~every 10s in a live terminal; only bill if it rendered within this window
	dailyNote  = "" // server enforces the daily cap
)

var categories = []string{"web3", "web", "devops", "data", "systems", "general"}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// adTailRe matches a trailing " → domain" in ad copy. Stripped at render time: the visible
// domain is display-only (and not clickable in most terminals), and the real destination is
// the cached click URL behind /info. Leaving it out of the stored copy keeps old clients,
// which render the text verbatim, working unchanged.
var adTailRe = regexp.MustCompile(` → \S+$`)

// version is stamped at build time via -ldflags "-X main.version=...".
// Defaults to "dev" for local/unstamped builds.
var version = "dev"

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

func writeFile(p, s string) { os.WriteFile(p, []byte(s), 0o644) }

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

	ad := readFile(filepath.Join(dir, "current_ad.txt"))
	balseg := readFile(filepath.Join(dir, "balance_display"))

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
		os.WriteFile(settingsPath, out, 0o644)
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
		url = apiURL() + url
	}
	openURL(url)
	fmt.Println("adtention: opened the current sponsor in your browser.")
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

	// Bill only for ads actually on screen. status writes a render heartbeat for its session
	// whenever our statusLine is drawn; if THIS session's heartbeat is missing or stale, the
	// host runs our hooks but shows no statusLine (e.g. the Claude desktop app), so we must
	// not register or record an impression nobody saw.
	pruneRenders(dir)
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
	api := apiURL()

	// identity: register once
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

	// click target for /sponsor (and OSC 8 links). Server returns click_url on a fresh
	// serve; on a dedup it only returns impression_id, so derive it then.
	click := r.ClickURL
	if click == "" && r.ImpID != "" {
		click = "/v1/click/" + r.ImpID
	}

	if strings.Contains(resp, "balance_usd") {
		writeFile(filepath.Join(dir, "balance"), fmt.Sprintf("%.5f", r.BalanceUSD))
		writeFile(filepath.Join(dir, "balance_display"), fmt.Sprintf("⊕ $%.2f", r.BalanceUSD))
	}
	if r.Text == "" {
		writeFile(filepath.Join(dir, "current_ad.txt"), "")   // no inventory: clear the slot
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
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
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
	body := ""
	if ref != "" {
		body = fmt.Sprintf(`{"ref":%q}`, ref) // attribute this install to the referrer
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
	payload := fmt.Sprintf(`{"publisher_id":%q,"category":%q,"nonce":%q}`, pub, category, nonce)
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

	type sc struct {
		cat string
		n   int
	}
	scores := []sc{}
	for cat, re := range topicPatterns {
		scores = append(scores, sc{cat, len(re.FindAllString(text, -1))})
	}
	sort.Slice(scores, func(i, j int) bool { return scores[i].n > scores[j].n })
	if len(scores) > 0 && scores[0].n >= 3 {
		return scores[0].cat
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
	cmdLine := fmt.Sprintf("%q status", self)
	if root != "" {
		cmdLine = fmt.Sprintf("%q status", filepath.Join(root, "bin", "adtention"))
	}

	settingsPath := filepath.Join(home(), ".claude", "settings.json")
	var settings map[string]any
	if b, err := os.ReadFile(settingsPath); err == nil {
		json.Unmarshal(b, &settings)
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
		os.WriteFile(settingsPath, b, 0o644)
	}
}
