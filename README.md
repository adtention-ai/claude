# adtention

A status line for Claude Code that shows useful session info (model, context usage, weekly
limit) plus one small sponsor line, and pays you a little for the sponsor line.

The status line is the part you actually want. The sponsor line is how it stays free and
how you earn while you work. One line, at the bottom of your terminal. No popups, and no
phoning home with your code.

## What leaves your machine, and what doesn't

This thing sits in your terminal all day, so the data question is the important one. Here
is exactly how it works.

When you submit a prompt, the plugin reads your current project and recent conversation and
sorts it into one of six broad buckets:

`web3` · `web` · `devops` · `data` · `systems` · `general`

That sorting happens entirely on your machine. The only thing sent to the server is the
bucket name (one short word) and an anonymous install ID, so the server can pick a relevant
ad and credit you for showing it.

Never sent:

- your code or file contents
- your prompts or Claude's replies
- file names, paths, or repository names
- anything that identifies you or what you are working on

There is no account, no email, and no login. The install ID is a random string created
locally the first time the plugin runs. You only attach an email or wallet later, if and
when you decide to cash out.

## Install

```
/plugin marketplace add adtention-ai/claude
/plugin install adtention@adtention
```

On first run the plugin adds its status line to your `~/.claude/settings.json`. If you
already have a custom status line, it keeps yours and shows the sponsor line beside it, or
on a second row when the terminal is narrow. Your old status line is saved to
`~/.claude/adtention/prev_statusline.json` so you can put it back if you uninstall.

Needs `jq`. Local state lives in `~/.claude/adtention/` (set `$ADTENTION_CACHE` to change
it).

## What you see

```
Opus 4.8 · context 66% · limit 10%   ⊕ $0.42   🔗 Alchemy: APIs for every chain → alchemy.com
```

Model, context window used, weekly rate limit used, your running balance, and the sponsor
line. On a narrow terminal it drops the least useful parts first and always keeps your
balance and the sponsor line.

## Earning

You earn a small amount each time the sponsor line is served, at most once every 15 seconds
and only when you actually send a prompt. An idle terminal earns nothing, so leaving Claude
open overnight does not generate impressions. The balance accrues to your install and shows
in the status line. Once it passes a threshold (around $10) you can attach a payout method
and withdraw.

## How it works

Two parts, deliberately kept separate:

- The status line script runs on every repaint. It only reads a cached file and prints, and
  never makes a network call, so the terminal never waits on the server.
- A `UserPromptSubmit` hook runs once per prompt. It does the local sorting, calls the
  server once to fetch a fresh ad, records the impression, and updates the cache.

Display and billing are decoupled: the line is always instant, and an impression only
counts on a real prompt. Ad selection happens on the server, so the ad logic stays off your
machine.

## Uninstall

```
/plugin uninstall adtention@adtention
```

If you had a status line before installing, restore it from
`~/.claude/adtention/prev_statusline.json`.

## Status

Early, and the ad inventory is still small.
