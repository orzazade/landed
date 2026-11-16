# landed

You tell a coding agent "from now on, settings stay in TOML, never YAML". It says "OK". Then the
rule goes nowhere: it never reaches AGENTS.md, a commit or a note. The agent forgets it, the session
ends, another session picks up the work, and nobody hears of it again.

landed finds the lasting decisions you made that never landed. It reads your Claude Code
transcripts, picks out your decisions, and looks for where each one was written down or carried
out: commits, edited files, notes. The ones with no landing are listed, so you or the agent can
land them.

```
missing   2d  kettle         3f9c2a1b  "never ship without the lid sensor"
        decision: Lid sensor is required before any release
        why: searched 3 later record edits by any session, 14 commits in /home/dev/kettle for lid, sensor, release; none of 6 candidates records it (judge: the commits are about the timer, not the sensor)
```

(Made-up example.)

## Install

1. The binary (Go 1.26+, or a release build for macOS and Linux, amd64 and arm64):

   ```sh
   go install github.com/orzazade/landed/cmd/landed@latest
   ```

   Release builds are on the [releases page](https://github.com/orzazade/landed/releases); put
   `landed` on your `PATH`.

2. The plugin, inside Claude Code:

   ```
   /plugin marketplace add orzazade/landed
   /plugin install landed@landed
   ```

## Use

- **`landed missing`**: every session of the last 48 hours, in every project, including sessions
  that ended long ago. Run it from a terminal whenever you like.
- **`/landed`** inside Claude Code: the same check for the current session only. Useful before you
  end a session or hand its work to another one.
- **Automatic**: the plugin also checks the current session by itself when Claude Code compacts it
  (drops older context to make room), and tells the agent what has not landed:

  ```
  Owner decisions in this session with no landing yet:
  - "From now on, the marmalade timer keeps its settings in TOML files, never YAML."
  Land each before going on.
  ```

  When everything has landed it prints nothing. It never blocks: on any error it prints one line,
  `landed could not check: <why>`, and if the check is not done within 8 s it says so and stops.
  A second hook starts the check in the background just before, so its answers are cached.

landed needs Claude Code transcripts (`~/.claude*/projects`), and the `claude` CLI logged in with
your Claude subscription for its model calls. `--no-model` runs it on rules and keywords only.

## Commands

```sh
landed check --session ID          # one session's lasting decisions with no landing yet
landed missing                     # all sessions, last 48 h, decisions with no landing
landed missing --since 7d --all    # also the ones that landed, with the evidence
landed report                      # one count line, including how many were rescued
landed proof                       # what the warnings led to, and how often you repeat a decision
landed scan                        # how many of your turns were read, per project
```

Every decision gets one of four states, and none is silent: `landed` with the evidence,
`superseded` with the later decision or line that replaced or withdrew it, `missing` with what
was searched, or `?` (could not check) with the reason.

A decision is **rescued** when one run showed it as missing and a later run found it landed.
`landed report` counts them. That number is the reason landed exists.

## Proof from live use

A rescue count says little unless you can check it. So landed keeps a log of its own warnings,
`~/.local/share/landed/proof.jsonl` (mode 600; it quotes your words):

- each time the hook, `/landed`, `landed missing` or `landed report` shows a decision with no
  landing: the time, the session it was shown in (none for a terminal or a report), the decision,
  and the exact line shown
- each time a later run finds a warned decision landed: the landing (commit, file, log line), the
  session that made it, and whether that session had been warned before it acted

A write that fails says so in one line and never blocks the session.

`landed proof` reads that log and prints each warned decision with its chain: when and where it
was flagged, which session saw the warning, the landing with its session id and commit, and a
verdict:

- **caused by landed**: a session that saw the warning landed the decision after it
- **would have landed anyway**: the landing came first, or was made in a session that never saw a
  warning (a report or a terminal run shows rows to no session), or landed cannot tell which
  session made it (a changed note: its time is its last change, which may be unrelated)

Warnings from before the log existed are rebuilt from the transcripts (Claude Code keeps each
hook's output) and from earlier `missing` runs, and are marked `rebuilt`. Warned decisions that
have not landed are listed too.

The second half counts, per week, how often you had to say a decision again: your lasting
decisions that a model call reads as restating an earlier one of yours (an hour or more before,
sharing two keyword words), before and after the install date (`--installed`; default: landed's
first warning). Each week shows three examples with session ids, so you can check the count.
Earlier decisions are looked for up to 30 days back, also before `--since`; a week with less
history behind it is marked `*`, because its count is low. The rate leaves out decisions whose
model call failed, and says how many there were. One message sent to several tabs counts once:
a decision in another session within 10 minutes of an earlier one, close in length and with at
least 80% of the same words, is left out and counted apart. A message pasted again into the same
tab still counts.

## What counts as a decision

Only lasting ones: a rule ("never", "always", "from now on"), a product or design choice, and
an answer you give to a question the agent queued for you. One-off requests ("check it", "give me
the url") are not counted: their result is the work itself.

1. Only your own turns are read. Tool results, slash commands, notifications and text that other
   tools type into the session are dropped. So are messages one agent sends another that arrive as
   your turns: a pasted coordination-log line, or a turn that opens with its sender ("From qa: …",
   "qa → docs, …") when that sender is an agent named in the session's log lines. A turn that
   opens by addressing such an agent ("qa, …") is dropped only when a log kind follows at once
   ("qa: PASS …") or it answers that agent's ask or turn ("qa, on your … ASK: …").
2. Cheap rules settle the clear cases (short answers, acknowledgements, short questions, also
   after a filler word like "okay how …?").
3. The rest goes to `claude -p --model haiku`, in batches, on your own login. landed never uses an
   API key: it removes every `ANTHROPIC_*` and `CLAUDE_CODE_USE_*` variable from the child process,
   and it stops all model calls after the first call whose login reports an API key. Each answer is cached, so a turn is
   classified once.

## How landings are found

Within 7 days after the decision (`--window`), landed collects candidates:

- git commits (all branches) in the session's repo and in every repo the session edited; with the
  judge on, also in every repo under `~/Projects` when it exists (`--repos-under`)
- Markdown edits and `git commit` commands by any session, read from the transcripts
- Markdown notes in the memory folders and in `--notes` folders, when they are created or changed
  after the decision
- the agent's own next commands and edits, and its later ones that match the keywords
  (commands that only read, like `cat` or `git show`, are skipped)
- your own later decisions on the same subject, in any session
- optional: lines in a coordination log (`--bus`, `*.log` files of
  `TIME KIND from -> to — text`). It's off unless the folder exists.

Keywords from the decision rank the candidates. A second `claude -p` call, the judge, reads the
decision and its candidates and picks the one that records or carries out this decision, or
none. A note counts only when it states the choice, not when it only mentions the topic. The
judge also says whether a candidate replaces the decision: a later decision of yours that settles
the same question differently, or a line that says it was withdrawn. That row is `superseded`.

The judge's pick is then checked in a call of its own, with only the decision and that one
record: it must say yes and quote the words of the record that state the choice, and landed
checks that the quote is in the record. A refused pick is ruled out and the judge picks again,
up to three rounds; when nothing passes, the row is `missing`.

## Measured

`landed eval` scores landed against hand labels kept outside the repo (they hold private text).
Last run: 2026-11-16, one developer's transcripts, 315 hand-labelled turns.

- Finding decisions at all: recall 91.8% (134 of 146), precision 84.3% (134 of 159).
- Telling lasting decisions from one-off requests: recall 74.5% (38 of 51), precision 73.1% (38
  of 52). So about 1 in 4 lasting decisions is dropped as a one-off.
- Lasting decisions that never landed, shown as missing: 1 of 3. Before the judge's pick was
  checked (see How landings are found) it was 0 of 3: the judge took an unrelated commit or note
  for each. The check fixes some of that, not all; with so few real misses one row moves the
  number by a third.
- `missing` rows that really did not land: 1 of 5 hand-checked. Of the other 4, one is not a
  decision and 3 did land, in a record landed did not pick.
- `landed` rows the hand check disputes: 2 of 36.
- Lasting decisions that never land are rare, about 3% in this data, so these numbers rest on
  few rows.

Treat its list as a reminder worth a look, not as a complete to-do list.

## Privacy

Everything runs on your machine. There is no telemetry and no server. The only network calls are
the optional `claude -p` calls on your own login:

- **Classifier:** unclear turns, cut to 1,500 characters.
- **Judge and its check:** the decision, with 400-character cuts of its candidates (commit
  messages, note lines, commands).
- **`landed proof` repeat count:** a decision's summary and words, with up to 5 earlier decisions
  (summary and a 300-character cut).

`--no-model` turns them all off. Only Markdown notes are read, so tool output and config files never
reach the model. State lives in one SQLite file, `~/.local/share/landed/landed.db`, and the warning log
`proof.jsonl` next to it (both mode 600).
All test fixtures in this repo are made up.

## Known gaps

- Only Claude Code transcripts are read.
- The first check of a long session reads its history and asks the model, which can take longer
  than the hook's 8 s. Then the hook says `still reading history (not done within 8s)`; the
  background check it started fills the cache for the next one.
- About 1 in 4 lasting decisions is taken for a one-off and never checked (see Measured).
- The judge is a model: a row can flip between runs. A flip from missing to landed with no new
  work counts as rescued.
- A changed note is read as it is now, not as it was in the window.
- A landed result stays cached. A commit rebased away later, or a deleted note, does not undo it.
- On Linux, file creation times are not read, so a note counts only as a changed note.
- Repos outside the session's own repos and `~/Projects` are not searched.
- The judge's answer that a later line replaces a decision is not checked the way its pick is:
  a wrong one hides the decision as `superseded`. (The eval found no such row yet.)
- `superseded` needs the judge: with `--no-model` a replaced decision shows as missing. The hook
  and `/landed` see only the same session's later decisions, so a decision replaced in another
  session is still listed there; `landed missing` sees all sessions.
- A relayed agent message in a shape landed does not know is read as yours.
- `landed proof` finds who landed a commit by its subject in a `git commit` command of a session;
  a commit made outside Claude Code, or with `git commit -F`, has no known session and counts as
  "would have landed anyway". A report shows counts, not rows, so it never causes a landing.
- The repeat count sees only lasting decisions the classifier found (see Measured) and only the
  transcripts Claude Code still keeps (30 days by default).

## Status

This is an experiment with a kill rule. If it rescues fewer than 1 decision a week for its author
after 2 weeks, or has no outside user after 4 weeks, it stops.

## License

MIT, see [LICENSE](LICENSE).
