# ADtention

**The Claude Code status line that pays you to code.**

You stare at your terminal all day anyway. ADtention turns the bottom of it into a
genuinely useful status line — model, context usage, weekly limit — and adds one quiet
sponsor line that earns you credit while you work.

One line. No popups. No account. And **nothing about your code ever leaves your machine**. The rest of this README shows you exactly how, in a way you can verify yourself.

```
Context 66% · limit 10%   ⊕ $0.42   🔗 Alchemy: APIs for every chain → alchemy.com
```

That's the whole thing. Your context window, your weekly rate limit, your
running balance, and one sponsor. It never blinks, never interrupts, and never makes you
click anything.

---

## "Wait. An ad plugin reading my code? Hard pass."

Good instinct. Read this part first, then decide.

ADtention is built so that **your code can't leave your machine; even if the server asked
for it.** When you submit a prompt, a hook reads two signals locally: the kinds of files in
your project, and a quick keyword scan of your recent conversation. It sorts that into one
of six broad buckets — all of it on your machine, no network call:

`web3` · `web` · `devops` · `data` · `systems` · `general`

The **only** thing that ever goes to the server is that one word, plus an anonymous random
install ID so it can pick a relevant sponsor and credit your balance.

| Leaves your machine | Never leaves your machine |
|---|---|
| One bucket word (e.g. `web3`) | Your code or file contents |
| A random install ID | Your prompts or Claude's replies |
| | File names, paths, or repo names |
| | Anything identifying you or your work |

No account. No email. No login. The install ID is a random string created locally the first
time the plugin runs. You only attach an email or wallet **later**, if and when you decide
to cash out.

**Don't take our word for it.** The entire plugin is four short shell scripts in
[`bin/`](bin/). The status line script makes *zero* network calls. It only reads a cached
file and prints. The single outbound request lives in one hook, and you can read exactly
what it sends. Twenty minutes with the source and you'll know more about it than you do
about most of your dependencies.

---

## What you actually get

- **A status line worth having on its own**: Model, % context used, and % of your weekly
  rate limit, at a glance. The part you'd keep even if it paid nothing.
- **Passive credit while you work**: The sponsor line earns a small amount each time it's
  served on a real prompt. Money trickles in for doing what you were already doing.
- **Zero friction**: Two commands to install, works instantly, no signup.
- **Privacy by architecture, not by promise**: The design makes leaking your code
  impossible, not just against the rules.
- **A clean exit**: One command to remove, and your old status line comes right back.

---

## Install

```
/plugin marketplace add adtention-ai/claude
/plugin install adtention@adtention
```

On first run it adds its status line to your `~/.claude/settings.json`. **Already have a
custom status line?** It keeps yours and shows the sponsor line beside it or on a second
row when the terminal is narrow. Your original is saved to `~/.claude/adtention/prev_statusline.json` so you can always put it back.

Needs `jq`. Local state lives in `~/.claude/adtention/` (set `$ADTENTION_CACHE` to move it).

On a narrow terminal it drops the least useful parts first and always keeps your balance and
the sponsor line.

---

## How the money works

- You earn a small amount each time the sponsor line is served, **at most once every 15
  seconds**, and **only when you actually send a prompt**.
- An idle terminal earns nothing. Leaving Claude open overnight generates zero impressions —
  no farming, no gaming it.
- Your balance accrues to your install and shows live in the status line.
- Once it passes a threshold (currently **$10**), you attach a payout method and withdraw.

It's not a salary. It's beer money that shows up for work you were doing regardless.

---

## How it works under the hood

Two parts, deliberately kept separate so the terminal is never waiting on a server:

- **The status line script** runs on every repaint. It only reads a cached file and prints It never makes a network call, so the line is always instant.
- **A `UserPromptSubmit` hook** runs once per prompt. It does the local sorting, calls the
  server once to fetch a fresh sponsor, records the impression, and updates the cache.

Display and billing are decoupled: the line is always instant, and an impression only counts
on a real prompt. Sponsor selection happens server-side, so that logic stays off your
machine entirely.

---

## Uninstall

```
/plugin uninstall adtention@adtention
```

If you had a status line before, restore it from
`~/.claude/adtention/prev_statusline.json`. That's it — no residue, no account to close.

---

## FAQ

**Is it going to slow down my terminal?**
No. The status line never makes a network call. It reads a cached file and prints. The one
network request happens in a background hook when you submit a prompt, not on every repaint.

**Will it spam me with flashing ads?**
It's one text line at the bottom of your terminal. No popups, no color flashing, no
interruptions, nothing to click. When space is tight it shrinks before it ever covers your
status info.

**Do I need to sign up or hand over an email?**
No. There's no account and no login. An anonymous install ID is generated locally. You only
provide a payout detail if and when you decide to withdraw.

**How do I know my code isn't being harvested?**
Because the categorization runs locally and only emits one of six bucket words. The plugin
is four readable shell scripts — read [`bin/`](bin/) and confirm it yourself. The status
line script has no network access at all.

**What if I hate it?**
One command to uninstall, and your previous status line is restored from the backup file.
No trace left behind.