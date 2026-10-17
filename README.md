# dcdump

<https://github.com/wakasio/dcdump>

Dumps a Dreamcast GD-ROM with one command. The reading is done by two existing tools:
[redumper](https://github.com/superg/redumper) for the low-density area,
[dreamdump](https://codeberg.org/MoriGM/dreamdump) for the high-density area. dcdump is the
glue: it picks the drive, works out what the disc is, runs both with the right settings,
merges the output into redump's GD-ROM layout and compares every track with redump's datfile.
A hash that does not match means exit 1.

## Install

Needs Linux, Go 1.26+, and redumper b767 or later plus dreamdump on `PATH`.

```
go install github.com/wakasio/dcdump@latest
```

From a checkout: `make` builds a static binary, `make deploy HOST=...` copies it to the
machine with the drive.

## Usage

```
dcdump [--manual] [--drive VALUE] [--serial VALUE] [--retries=N] [--ld-only] [--check]
       [--force-drive ID] [-- other redumper options...]
dcdump --native[=HOST] [--keep] [--manual] [--serial VALUE]
dcdump --help
```

Unknown options are rejected before the drive is touched, so a typo cannot start a dump.
Anything after `--` goes to redumper. Output lands in `<repo>/work/dc/<name>/`, or
`$DCDUMP_WORK/<name>/`.

## What it does

- **Drive list**: `drives.json`, built in, overridden by `~/.config/dcdump/drives.json`. It
  says which drives read the HD area and how. Anything else is LD only, with `--ld-only`.
  `--force-drive ID` applies an entry to a drive that does not match it.
- **Preflight**: TOC, a raw READ CD of the first and last LD sector, IP.BIN, one HD sector.
  Some drives answer READ(10) but fail every raw LD read; this catches that before a long
  dump. `--check` stops here.
- **Identification**: product number, version and disc number from IP.BIN, looked up in the
  redump.info datfile, which is refreshed with a conditional GET each run. A disc that is not
  in it still gets dumped and is named by hash afterwards.
- **LD area**: redumper. If redumper knows the drive, its own database decides the settings;
  otherwise they come from the drive list. Over USB the sector order is forced to `DATA_SUB`,
  because ASM1153 bridges mangle transfers whose length is not a multiple of 4; b767 is the
  first build that asks for exactly that length.
- **HD area**: dreamdump with the drive-list parameters, in `<dir>/dreamdump/`. They are
  always passed explicitly: dreamdump's own list stops at DH16D2S GD0C, so a newer firmware
  revision would be refused. Its tracks are moved next to the LD ones, its log is kept, and
  the two cues become one GD-ROM cue. USB links are pinned to USB 2.0. On SATA a libata
  fallback to PIO is reported; reboot to get DMA back.
- **Finish**: `<name>_logs.zip` for submission, a CRLF cue and a `.gdi`. Exit 1 if redumper's
  sector check failed, an HD track is missing, or any hash differs.

## --native

Reads the disc in a real Dreamcast instead and compares the tracks with the datfile. Different
drive, different read path, no redumper or dreamdump, so a match confirms a dump
independently. It cannot produce a submission: that drive has no subchannel or C2.

The console side is not in this repository. It is a Dreamcast running Linux with a gdrom
driver that returns raw 2352-byte sectors, plus a `gdserve` service on TCP 9200 that answers
`info`, `toc`, `read FAD COUNT` and `cooked LBA COUNT`. `native.go` has the replies. Give the
address with `--native=HOST`, or set `DCDUMP_NATIVE_HOST`.

Sectors come back descrambled but not error-corrected, so each one is checked against its own
regeneration (sync, header, EDC, ECC), then re-read or rebuilt from the drive's 2048-byte read
if that fails. Audio is read twice. The audio offset is measured on the disc itself. Track
boundaries come from the datfile, or from pregap rules for a disc that is not in it. All of it
is counted in `<name>.native.txt`. The track files are deleted afterwards unless `--keep`.

## Notes

`testdata/dc-sample.dat` is seven entries from redump's Dreamcast datfile, for the tests.
Run them with `make test`.

## License

GPL-3.0-or-later, see [LICENSE](LICENSE).
