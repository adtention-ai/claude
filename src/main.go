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
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	defaultAPI = "https://api.adtention.ai"
	minDwellS  = 15
	dailyNote  = "" // server enforces the daily cap
)

var categories = []string{"web3", "web", "devops", "data", "systems", "general"}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func main() {
	if len(os.Args) < 2 {
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

// ---------- status: the render path ----------

type statusInput struct {
	Model struct {
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
	raw, _ := io.ReadAll(os.Stdin)
	var in statusInput
	json.Unmarshal(raw, &in)

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
		piece := fmt.Sprintf("\x1b[36m%s\x1b[0m", ad)
		if slot != "" {
			slot += "  " + piece
			slotW += 2 + len([]rune(ad))
		} else {
			slot = piece
			slotW = len([]rune(ad))
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

// ---------- prompt: the UserPromptSubmit hook (silent, detached) ----------

func cmdPrompt(dir string) {
	raw, _ := io.ReadAll(os.Stdin)
	var in struct {
		Cwd            string `json:"cwd"`
		TranscriptPath string `json:"transcript_path"`
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
	c := exec.Command(self, "refresh", cwd, in.TranscriptPath)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach
	c.Start()
	// do not Wait: fire and forget. Print nothing (stdout is injected into the prompt).
}

// ---------- refresh: classify locally, call the API, write the cache ----------

func cmdRefresh(dir string) {
	cwd, transcript := "", ""
	if len(os.Args) > 2 {
		cwd = os.Args[2]
	}
	if len(os.Args) > 3 {
		transcript = os.Args[3]
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

	category, source := classify(cwd, transcript)
	api := apiURL()

	// identity: register once
	idFile := filepath.Join(dir, "identity.json")
	pub := readPublisher(idFile)
	if pub == "" {
		pub = registerAndSave(api, idFile)
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
		pub = registerAndSave(api, idFile) // self-heal
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
	}
	json.Unmarshal([]byte(resp), &r)

	if strings.Contains(resp, "balance_usd") {
		writeFile(filepath.Join(dir, "balance"), fmt.Sprintf("%.5f", r.BalanceUSD))
		writeFile(filepath.Join(dir, "balance_display"), fmt.Sprintf("⊕ $%.2f", r.BalanceUSD))
	}
	if r.Text == "" {
		writeFile(filepath.Join(dir, "current_ad.txt"), "") // no inventory: clear the slot
		return
	}
	writeFile(filepath.Join(dir, "current_ad.txt"), r.Text)
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

func registerAndSave(api, idFile string) string {
	body := post(api+"/v1/register", "")
	if body == "" {
		return ""
	}
	os.WriteFile(idFile, []byte(body), 0o600)
	var id struct {
		PublisherID string `json:"publisher_id"`
	}
	json.Unmarshal([]byte(body), &id)
	return id.PublisherID
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
