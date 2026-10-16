# dcdump

Dreamcast GD-ROM dump tool for Linux, following redump's GD-ROM dumping guide: the LD area is
dumped as a CD with [redumper](https://github.com/superg/redumper), the HD area with
[dreamdump](https://codeberg.org/MoriGM/dreamdump). Every track and the cue are checked
against redump's datfile, and anything that does not match is a failure.

```
dcdump [--manual] [--drive VALUE] [--serial VALUE] [--retries=N] [--ld-only] [--check]
       [--force-drive ID] [-- other redumper options...]

dcdump --help
```

Options are validated before any drive is touched: an unknown option is an error rather than a
dump, so a typo cannot start one. Options for redumper go after a literal `--`.

```
dcdump --native[=HOST] [--keep] [--manual] [--serial VALUE]
```

- **Drive list** (`drives.json`, built in; `~/.config/dcdump/drives.json` entries take precedence):
  which drives can read the HD area and with which settings. Drives that are not GD-capable can
  only be used with `--ld-only` (LD area, tracks 1-2). `--force-drive ID` applies an entry to a
  drive that does not match it (for testing new drives).
- **LD area (tracks 1-2)**: redumper without `--rings`. SATA reads with C2 using the official
  `redumper`; USB reads without C2 using `redumper-exact` (upstream + PR superg/redumper#459)
  because ASM1153-class bridges corrupt transfers whose length is not a multiple of 4.
  Since b765 redumper knows DH16D2S GD0D itself, so for a drive in its database dcdump passes
  no drive settings at all and only forces the sector order off SATA. Drives it does not know
  still get offset, read method and sector order from the drive list.
- **HD area (track 3+)**: dreamdump with the drive-list parameters (sector order, read offset,
  read-at-once; passed explicitly, so firmware revisions dreamdump does not list work too), run
  in `<dir>/dreamdump/`. Its track files are moved next to the LD ones, its log is kept as
  `<name>.dreamdump.log`, and the two cues are merged into redump's GD-ROM cue. USB links are forced to USB 2.0. On SATA a
  libata fallback to PIO is reported (reboot to restore DMA).
- **Preflight**: TOC, a raw READ CD of the first and last LD sector (catches a drive state where
  READ(10) works but every LD READ CD fails; power-cycle the drive), IP.BIN, and one HD sector.
  `--check` stops after these checks without dumping.
- **Identification**: IP.BIN product number / version / disc number from the LD system area,
  looked up in the redump.info Dreamcast datfile (refreshed with a conditional GET every run).
  Unknown discs are dumped as `Unknown (...)` and named by hash afterwards.
- **Strict result**: exit code 1 unless every LD data track passes redumper's sector check,
  dreamdump produced every HD track, and every track and the cue match the DAT. Produces
  `<name>_logs.zip` for submission, a CRLF cue, and a `.gdi` for archiving.

## `--native`: a Dreamcast as an independent hash source

`--native` reads the disc in a real Dreamcast over the network instead of a PC drive and
compares every track with the DAT. Nothing is shared with the normal path (Sega's own GD-ROM
drive, its native high-density read, no redumper or dreamdump), so matching hashes are an
independent confirmation of a dump, and a second opinion when a PC read looks doubtful.

**It is not a way to make a redump submission.** The Dreamcast drive has no subchannel or C2
output and is not a drive redump accepts; the result is a report (`<name>.native.txt`), and the
track files are deleted after a successful comparison unless `--keep` is given (which also
writes a `.gdi`).

- **Console side**: a Dreamcast running Linux, with a gdrom driver that can read raw
  2352-byte sectors and a `gdserve` service listening on TCP port 9200. The service answers
  one plain-text request per connection: `info`, `toc`, `read FAD COUNT` (raw sectors) and
  `cooked LBA COUNT` (2048-byte sectors); `native.go` documents the exact replies. That side
  is not part of this repository. Give the address as `--native=HOST`, or set
  `DCDUMP_NATIVE_HOST` and use plain `--native`; the mode fails with a clear message when the
  console is not reachable or does not answer like that service (it must report a GD-ROM
  drive identification and 2352-byte raw reads). Any drive model is accepted; the service's
  answer goes into the report.
- **Track boundaries** come from the DAT track sizes (redump track files are contiguous within
  an area) and must add up to the lead-outs in the TOC. A disc that is not in the DAT is cut by
  pregap rules (an audio track starts 150 frames before its TOC address, a data track after
  audio 225 before); discs checked so far follow them, but that path has had no run of its own.
- **Data tracks**: the drive returns whole 2352-byte sectors, descrambled but not
  error-corrected. Every sector is compared with its own regeneration (sync, header, EDC, ECC).
  Sectors whose user data passes EDC but whose framing or parity bytes differ are corrected;
  anything else is read again (after moving the head away) and finally rebuilt from the drive's
  own corrected 2048-byte read. All of this is counted in the report.
- **Data after audio**: the file of such a track begins with 75 frames that are still recorded
  as audio and are read as audio. The first data sectors behind them cannot be read on their
  own; a read started in the audio returns them scrambled, and they are descrambled and
  accepted only if their user data passes EDC.
- **Audio tracks**: the offset between data addressing and the audio stream is measured on the
  disc itself (the last data sector of the track before the first audio track still shows up,
  scrambled, in the audio stream). Audio is read twice and the reads must be identical.
- Exit code 1 unless every track matches the DAT; the output is then kept as
  `<name> [native].failed-<timestamp>`.

Output: `<repo>/work/dc/<name>/` (or `$DCDUMP_WORK/<name>/`).

## Requirements

Linux, Go 1.26+ to build, and the two dumping programs on `PATH`:
[redumper](https://github.com/superg/redumper) b765 or later for the LD area and
[dreamdump](https://codeberg.org/MoriGM/dreamdump) for the HD area. Neither is bundled.
A GD-capable drive is needed for the HD area; see the drive list above.

`testdata/dc-sample.dat` is a seven-entry excerpt of redump's Dreamcast datfile, used as a
test fixture; the full datfile is fetched at run time from redump.info.

Build/test/deploy: `make test`, `make`, `make deploy HOST=the-machine-with-the-drive`.

## License

GPL-3.0-or-later, see [LICENSE](LICENSE).
