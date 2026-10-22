// SPDX-License-Identifier: GPL-3.0-or-later

// dcdump — Dreamcast GD-ROM dump tool for Linux (redumper wrapper).
//
// Command-line conventions: --manual, --drive VALUE, --serial VALUE, --retries=N, and
// everything after a literal -- passed to `redumper disc`. DC specifics: a drive list of
// GD-capable drives, transport-dependent read settings for drives redumper does not know
// (SATA: C2; USB: no C2), skipping the empty area between the LD and HD areas, strict log
// checks, and failure (exit 1) unless every track and the cue match redump.
package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var version = "dev"

func workRoot() string {
	if d := os.Getenv("DCDUMP_WORK"); d != "" {
		return d
	}
	exe, err := os.Executable()
	if err == nil {
		if r, err := filepath.EvalSymlinks(exe); err == nil {
			exe = r
		}
		return filepath.Join(filepath.Dir(filepath.Dir(exe)), "work", "dc")
	}
	return filepath.Join(".", "work", "dc")
}

func resolveOutputDir(auto bool, fullName string) string {
	def := workRoot()
	var dir string
	if auto {
		dir = filepath.Join(def, fullName)
		fmt.Printf("  [auto] Output: %s\n", dir)
	} else {
		dir = filepath.Join(ask("Output directory", def), fullName)
	}
	// redumper refuses to overwrite an existing dump; keep the old one aside instead of deleting it.
	if ents, err := os.ReadDir(dir); err == nil && len(ents) > 0 {
		prev := dir + ".prev-" + time.Now().Format("20060102-150405")
		if err := os.Rename(dir, prev); err != nil {
			fatal(fmt.Sprintf("Output directory is not empty and cannot be moved aside: %v", err))
		}
		fmt.Printf("  [!] Existing output moved to: %s\n", prev)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal(err.Error())
	}
	fmt.Printf("  Output: %s\n", dir)
	fmt.Println()
	return dir
}

// keepFailed renames a failed dump directory to "<dir>.failed-<timestamp>" so nothing is
// deleted and a re-run starts in a clean directory. Returns the path the files are now in.
func keepFailed(dir string) string {
	to := dir + ".failed-" + time.Now().Format("20060102-150405")
	if err := os.Rename(dir, to); err != nil {
		return dir
	}
	return to
}

var syncPattern = []byte{0, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0}

// preflight checks the disc before spending an hour on redumper. Returns the LD
// lead-out LBA and the IP.BIN (nil if unreadable).
func preflight(d *Drive, ldOnly bool) (int, *IPBin) {
	fmt.Println("Checking disc...")
	dev, err := OpenSCSI(d.SG)
	if err != nil {
		fatal(fmt.Sprintf("Cannot open %s: %v", d.SG, err))
	}
	defer dev.Close()
	if s := dev.WaitReady(60 * time.Second); !s.OK {
		if s.Key == 2 && s.ASC == 0x3A {
			fatal("No disc in the drive.")
		}
		fatal("Drive not ready: " + s.String())
	}
	toc, s := dev.ReadTOC()
	if !s.OK {
		fatal("READ TOC failed: " + s.String())
	}
	leadout := -1
	var desc []string
	for _, t := range toc {
		if t.Number == 0xAA {
			leadout = t.LBA
			continue
		}
		kind := "audio"
		if t.IsData() {
			kind = "data"
		}
		desc = append(desc, fmt.Sprintf("%d %s@%d", t.Number, kind, t.LBA))
	}
	if leadout < 0 {
		fatal("TOC has no lead-out.")
	}
	fmt.Printf("  TOC: %s, lead-out@%d\n", strings.Join(desc, ", "), leadout)

	// A raw READ CD of the first and the last LD sector. READ(10) and the HD area can work while
	// every LD READ CD fails (seen once on a GD0B HP right after flashing: 05/64/00 in 0.05 s for
	// all LD LBAs; a power-cycle cleared it), so probe the LD with the command redumper uses.
	for _, lba := range []int{0, leadout - 1} {
		if _, s := dev.ReadCDRaw(lba, 1); !s.OK {
			fatal(fmt.Sprintf("LD check: READ CD of LBA %d failed (%s). The drive is in a bad state; power-cycle it and run dcdump again "+
				"(to help find the cause, first run ~/d2s_state/statesnap.py %s ld-fail).", lba, s, d.SG))
		}
	}
	fmt.Printf("  LD area: READ CD of LBA 0 and %d OK\n", leadout-1)
	if hidden, _ := dev.KernelHidesCheckCondition(); hidden {
		box([]string{
			"[!] This kernel reports rejected ATAPI commands with GOOD status (libata bug,",
			"    fixed upstream by commit bc8ce2cea5f7). redumper checks only the status",
			"    byte, so it may take a rejected read as successful data. dcdump removes the",
			"    resulting fake .atip/.pma/.cdtext and still verifies every track against",
			"    redump, but update the kernel before relying on this machine.",
		})
	}

	var ip *IPBin
	if sec, s := dev.Read10(0, 1); s.OK {
		ip, err = ParseIPBin(sec)
		if err != nil {
			fmt.Printf("  [!] LBA 0: %v — not a Dreamcast disc?\n", err)
		} else {
			fmt.Printf("  IP.BIN: %s  %s  V%s  %s  area %s  disc %d/%d\n", ip.Product, ip.Title, ip.Version, ip.Date, ip.Areas, ip.Disc, ip.Of)
		}
	} else {
		fmt.Printf("  [!] Cannot read LBA 0: %s\n", s)
	}

	if !ldOnly {
		if ip == nil {
			fatal("No Dreamcast IP.BIN on this disc; refusing a GD dump (use --ld-only for the LD area of other discs).")
		}
		if leadout >= d.Profile.HDFirstLBA {
			fatal(fmt.Sprintf("LD lead-out %d is not below the HD area (%d): not a GD-ROM layout.", leadout, d.Profile.HDFirstLBA))
		}
		if line, bad := CheckDriveState(dev, d.Profile); line != "" || bad != "" {
			if line != "" {
				fmt.Println(line)
			}
			if bad != "" {
				fatal(bad)
			}
		}
		// Check the HD area at the speed dreamdump will read it at: a drive that needs a lower
		// HD speed can fail this single read at its default (maximum) speed.
		if dd := d.Profile.Dreamdump; dd != nil && dd.Speed > 0 {
			if s := dev.SetCDSpeed(dd.Speed * 176); !s.OK {
				fmt.Printf("  [!] SET CD SPEED %dx failed (%s)\n", dd.Speed, s)
			}
		}
		// On TS-H492C builds before GD1Z the first HD read after a disc load could fail (servo
		// left uncalibrated at spin-up); the dump itself retries, so give the check a few attempts too.
		var buf []byte
		var s Sense
		for try := 1; try <= 3; try++ {
			buf, s = dev.ReadCDRaw(d.Profile.HDFirstLBA, 1)
			if s.OK && bytes.Contains(buf, syncPattern) {
				break
			}
			if try < 3 {
				fmt.Printf("  HD area check: LBA %d attempt %d failed (%s), retrying\n", d.Profile.HDFirstLBA, try, s)
			}
		}
		if !s.OK || !bytes.Contains(buf, syncPattern) {
			if s.OK {
				fatal(fmt.Sprintf("HD area check: LBA %d returned no sector sync.", d.Profile.HDFirstLBA))
			}
			fatal(fmt.Sprintf("HD area check: LBA %d unreadable (%s). Is the GD firmware active?", d.Profile.HDFirstLBA, s))
		}
		fmt.Printf("  HD area: LBA %d readable\n", d.Profile.HDFirstLBA)
	}
	fmt.Println()
	return leadout, ip
}

func main() {
	o := ParseOptions(os.Args[1:])
	if o.Help {
		fmt.Println(Usage)
		return
	}
	auto := !o.Manual

	title := "dcdump — Dreamcast GD-ROM dump tool"
	fmt.Println()
	fmt.Println("  " + title)
	fmt.Println(banner(title))
	if auto {
		fmt.Println("  (auto mode)")
	}
	fmt.Println()

	if o.Native != "" {
		runNative(o)
		return
	}

	profiles, err := LoadProfiles()
	if err != nil {
		fatal(err.Error())
	}
	db := LoadDAT()

	d := SelectDrive(auto, o.DriveArg, o.LDOnly, o.ForceDrive, profiles)
	fmt.Printf("  Transport: %s", d.Transport)
	if d.Profile != nil {
		fmt.Printf("  (drive list: %s)", d.Profile.ID)
	}
	fmt.Println()
	if o.LDOnly {
		fmt.Println("  [--ld-only] LD area only; the HD area is not dumped.")
	}
	fmt.Println()

	d = PrepareDrive(d, profiles)
	_, ip := preflight(d, o.LDOnly)
	if o.Check {
		fmt.Println("Drive and disc checks passed (--check: nothing dumped).")
		return
	}

	info := IdentifyDisc(auto, ip, db, o.SerialArg)
	fullName := info.FullName()
	dir := resolveOutputDir(auto, fullName)

	gd := !o.LDOnly && d.Profile != nil && d.Profile.GD
	if gd && d.Profile.Dreamdump == nil {
		fatal("Drive list entry " + d.Profile.ID + " has no dreamdump parameters.")
	}
	rp := BuildRun(d, o, d.SG, dir, fullName)
	build, abort := rp.Run()
	if abort != "" {
		kept := keepFailed(dir)
		fmt.Println()
		box([]string{
			"[!!!] DUMP STOPPED EARLY — " + abort + ".",
			"This disc/drive state cannot give a usable dump; run dcdump again.",
			"Output kept for analysis: " + kept,
		})
		os.Exit(1)
	}

	var fails []string
	dreamBuild := ""
	if gd {
		ldCue, err := os.ReadFile(filepath.Join(dir, fullName+".cue"))
		if err != nil {
			fatal("redumper wrote no cue for the LD area.")
		}
		dp := BuildDreamRun(d, o, dir, fullName)
		var derr string
		dreamBuild, derr = dp.Run(fullName)
		hd, cerr := CollectHD(dir, fullName)
		if cerr != "" {
			if derr != "" {
				cerr = derr + "; " + cerr
			}
			kept := keepFailed(dir)
			fmt.Println()
			box([]string{"[!!!] HD AREA NOT DUMPED — " + cerr + ".", "Output kept for analysis: " + kept})
			os.Exit(1)
		}
		if derr != "" {
			fails = append(fails, derr)
		}
		os.WriteFile(filepath.Join(dir, fullName+".cue"), []byte(MergeCue(string(ldCue), hd)), 0o644)
		fmt.Printf("  HD tracks from dreamdump (build %s) merged into %s.cue\n\n", dreamBuild, fullName)
	}

	cuePath := filepath.Join(dir, fullName+".cue")
	cueText, err := os.ReadFile(cuePath)
	if err != nil {
		fatal("Expected cue file not found: " + cuePath)
	}
	tracks := ParseCue(string(cueText))
	if len(tracks) == 0 {
		fatal("Could not parse any tracks from " + cuePath)
	}

	fmt.Println("Hashing tracks...")
	for i := range tracks {
		t := &tracks[i]
		sum, n, err := sha1File(filepath.Join(dir, t.File))
		if err != nil {
			fatal(err.Error())
		}
		t.SHA1, t.Sectors = sum, n/sectorRaw
		label := "DATA"
		if t.Audio {
			label = "AUDIO"
		}
		fmt.Printf("  Track %02d (%s) → %s\n", t.Num, label, t.File)
		fmt.Printf("    SHA-1: %s\n", sum)
	}
	fmt.Println()

	if dropped := DropStaleTOCFiles(dir, fullName); len(dropped) > 0 {
		fmt.Printf("  [!] Removed %s: copies of the TOC, not real data (READ TOC error reported as success on Linux).\n\n",
			strings.Join(dropped, ", "))
	}

	fmt.Println("Checking redumper log (LD area)...")
	logText, err := os.ReadFile(filepath.Join(dir, fullName+".log"))
	if err != nil {
		fails = append(fails, "redumper log missing")
		fmt.Println("  ✗  log file missing")
	} else {
		var ldData []string
		for _, t := range tracks {
			if !t.Audio && !t.HD {
				ldData = append(ldData, t.File)
			}
		}
		cp := CheckParams{DataTracks: ldData}
		for _, c := range CheckLog(string(logText), cp) {
			switch {
			case c.Info:
				fmt.Printf("  ·  %s\n", c.Name)
			case c.OK:
				fmt.Printf("  ✓  %s\n", c.Name)
			default:
				fmt.Printf("  ✗  %s — %s\n", c.Name, c.Detail)
				fails = append(fails, c.Name+": "+c.Detail)
			}
		}
	}
	fmt.Println()

	alts := info.Candidates
	if info.IdentifyByHash {
		alts = db.All
	}
	entry := info.DatEntry
	dir, fullName, entry = SettleEntry(tracks, dir, fullName, entry, alts)
	if info.IdentifyByHash && entry == nil {
		fmt.Println("  [!] The dump matches no DAT entry.")
	}

	fails = append(fails, VerifyTracks(tracks, entry, o.LDOnly)...)
	PackSubmissionLogs(dir, fullName)
	if f := FinalizeCue(dir, fullName, entry, o.LDOnly); f != "" {
		fails = append(fails, f)
	}
	if !o.LDOnly {
		gdi := filepath.Join(dir, fullName+".gdi")
		if err := os.WriteFile(gdi, []byte(GDIText(tracks)), 0o644); err == nil {
			fmt.Println("Writing GDI...")
			fmt.Printf("  → %s\n", filepath.Base(gdi))
			fmt.Println()
		}
	}

	var notes []string
	if gd {
		notes = append(notes, fmt.Sprintf("Note: LD area dumped with %s (build %s), HD area with dreamdump (build %s).", rp.Binary, build, dreamBuild))
	}
	if o.LDOnly {
		notes = append(notes, "Note: LD area only — the HD area (track 3+) was not dumped.")
	}
	PrintSummary(dir, tracks, notes)

	if len(fails) > 0 {
		fmt.Println()
		lines := []string{"[!!!] DUMP DOES NOT MATCH REDUMP — do not use or submit it:"}
		for _, f := range fails {
			lines = append(lines, "  - "+f)
		}
		lines = append(lines, "Output kept for analysis: "+keepFailed(dir))
		box(lines)
		os.Exit(1)
	}
	if gd {
		CleanDreamdump(dir)
	}
	fmt.Println("All checks passed: every track and the cue match redump.")
}
