# AGENTS.md — jibotool

A non-interactive, JSON-emitting CLI that drives the Jibo RCM eMMC-unlock
exploit. See `README.md` for full background, architecture, and usage.

## Build System

```bash
make build     # host arch, ./bin/jibotool
make arm64     # cross-compile linux/arm64 (e.g. a Coral Dev Board)
make armv7     # cross-compile linux/armv7 (e.g. running directly on Jibo)
make test
make clean
```

## Non-negotiable safety invariants

This tool writes raw sectors to a real, largely irreplaceable piece of
hardware. Any change here needs to preserve these, they were each added
because of a real bug or near-miss, not out of caution for its own sake:

- **Sector numbers are always formatted as hex, never decimal.**
  `shofel2_t124` parses sector arguments with `sscanf("%x", ...)` — passing
  a decimal value silently targets the wrong sector on a live eMMC write.
  `hexArg()` in `utils.go` is the only function allowed to format a sector
  number; route everything through it.
- **Every write is followed by a mandatory SHA256 read-back comparison.**
  `EMMC_WRITE`'s own success status is a single status word, not proof the
  bytes landed correctly. `write`/`restore` must return `"verified": false`
  (not just a non-zero exit code) and must refuse to imply it's safe to
  power-cycle if the comparison fails.
- **`write`/`restore` require an exact confirm phrase as a CLI argument**
  (`I_UNDERSTAND_THIS_WRITES_REAL_EMMC`), not just a prompt a caller could
  script past accidentally. Don't relax this to a `--yes` flag or similar.
- **`mode.json`'s inode must end up `0100644`, not bare `0644`.** A bare
  mode silently produces a "bad type" inode that boots into a broken state.
  Check inode type explicitly after any `debugfs` write, don't just check
  the exit code.
- **`debugfs` operations must `cd` into the parent directory and use a bare
  filename**, not a full path. On this board's `debugfs` version, a full
  path allocates the inode but never links it into the parent directory,
  the file silently vanishes from `ls` and every later lookup fails.
- **A 2-second settle delay is required after every `shofel2_t124`
  invocation before the next one.** The USB device disconnects and
  re-enumerates after each operation; chaining calls with no pause races
  the re-enumeration.

## Real bugs found only by testing against actual hardware

Neither of these was visible from code review:

- Non-interactive SSH's default `PATH` doesn't include `/sbin`, where
  `debugfs`/`e2fsck` live. `widenPATH()` in `main.go` fixes this at
  startup rather than special-casing every call site.
- `shofel2_t124` opens its ARM payload files by relative path. The wrong
  working directory produces a misleading partial failure that looks like
  RCM-handshake progress. Every invocation sets `cmd.Dir` explicitly.

If you find a third one, add it here.
