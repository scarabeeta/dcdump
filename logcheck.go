package main

// Hard checks on redumper's <name>.log. A redumper run can finish with exit code 0
// and still be useless for a GD-ROM: e.g. GD detection silently skipped (LD-only
// dump), the HD session not split (damaged Q in the HD pregap), or raw sectors with
// header/EDC errors that were never retried (no C2). These checks catch that.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type Check struct {
	Name   string
	OK     bool
	Detail string
	Info   bool // informational line, never fails
}

var (
	reRunSep      = regexp.MustCompile(`(?m)^=== \d{4}-\d\d-\d\d .*$`)
	reCDROMBlock  = regexp.MustCompile(`(?m)^CD-ROM \[(.+)\]:\s*$`)
	reRedumpErr   = regexp.MustCompile(`REDUMP\.INFO errors:\s*(\d+)`)
	reWriteOffset = regexp.MustCompile(`disc write offset:\s*([+-]?\d+)`)
	reDescramble  = regexp.MustCompile(`descramble failed \(LBA:\s*(?:\[\s*(-?\d+)\s*\.\.\s*(-?\d+)\s*\]|(-?\d+))\)`)
	reOffsetLine  = regexp.MustCompile(`^\s+LBA:\s+-?\d+ ->`)
	reIndex00     = regexp.MustCompile(`index 00 \{ LBA: \[\s*(-?\d+)\s*\.\.\s*(-?\d+)\]`)
)

// pregaps returns the index-00 ranges of the "final TOC" section. A data track that
// follows an audio track starts its pregap with audio-mode sectors, which redumper
// reports as "descramble failed"; those are expected and covered by the track hash.
func pregaps(run string) [][2]int {
	i := strings.Index(run, "final TOC:")
	if i < 0 {
		return nil
	}
	sec := run[i:]
	if j := strings.Index(sec, "\n\n"); j >= 0 {
		sec = sec[:j]
	}
	var out [][2]int
	for _, m := range reIndex00.FindAllStringSubmatch(sec, -1) {
		a, _ := strconv.Atoi(m[1])
		b, _ := strconv.Atoi(m[2])
		out = append(out, [2]int{a, b})
	}
	return out
}

// LastRun returns the text of the last run in an appended redumper log.
func LastRun(log string) string {
	locs := reRunSep.FindAllStringIndex(log, -1)
	if len(locs) == 0 {
		return log
	}
	return log[locs[len(locs)-1][0]:]
}

type CheckParams struct {
	GD                 bool
	AllowFrom, AllowTo int      // descramble failures tolerated in [AllowFrom, AllowTo] (HD start reject zone)
	DataTracks         []string // data track file names from the cue
}

func CheckLog(log string, cp CheckParams) []Check {
	run := LastRun(log)
	var cs []Check
	has := func(s string) bool { return strings.Contains(run, s) }

	if cp.GD {
		cs = append(cs, Check{Name: "GD-ROM detected (rings)", OK: has("dreamcast: GD-ROM detected"),
			Detail: "redumper did not recognise the disc as a GD-ROM; only the LD area would be dumped"})
		cs = append(cs, Check{Name: "GD-ROM dump split (HD session)", OK: has("dreamcast: GD-ROM dump detected"),
			Detail: "redumper did not split the HD area into its own session (often damaged Q in the HD pregap)"})
	}

	// redumper relaxes the C2 checks for CD-Rs (detected from .atip/.pma). A GD-ROM is not a CD-R;
	// on Linux SATA a rejected READ TOC can come back as success (see DropStaleTOCFiles), so any
	// CD-R leniency applied to a GD is an error.
	if cp.GD && has("warning: CD-R ") {
		cs = append(cs, Check{Name: "no CD-R C2 leniency", OK: false,
			Detail: "redumper treated the disc as a CD-R and accepted leading/trailing C2 errors"})
	}

	// one consistent write offset for all data sectors
	if i := strings.Index(run, "offset configuration:"); i >= 0 {
		n := 0
		for _, l := range strings.Split(run[i:], "\n")[1:] {
			if !reOffsetLine.MatchString(l) {
				break
			}
			n++
		}
		cs = append(cs, Check{Name: "single write offset", OK: n == 1,
			Detail: fmt.Sprintf("%d different offsets in the data track (misplaced or damaged sectors)", n)})
	}
	if m := reWriteOffset.FindAllStringSubmatch(run, -1); m != nil {
		cs = append(cs, Check{Name: "disc write offset " + m[len(m)-1][1], OK: true, Info: true})
	}

	// per data track: redumper's own sector check
	blocks := reCDROMBlock.FindAllStringSubmatchIndex(run, -1)
	seen := map[string]bool{}
	for i, b := range blocks {
		end := len(run)
		if i+1 < len(blocks) {
			end = blocks[i+1][0]
		}
		name := run[b[2]:b[3]]
		seen[name] = true
		if m := reRedumpErr.FindStringSubmatch(run[b[1]:end]); m != nil {
			n, _ := strconv.Atoi(m[1])
			cs = append(cs, Check{Name: "sector errors: " + name, OK: n == 0,
				Detail: fmt.Sprintf("REDUMP.INFO errors: %d", n)})
		} else {
			cs = append(cs, Check{Name: "sector errors: " + name, OK: false, Detail: "no REDUMP.INFO line"})
		}
	}
	for _, t := range cp.DataTracks {
		if !seen[t] {
			cs = append(cs, Check{Name: "sector errors: " + t, OK: false, Detail: "redumper did not check this data track"})
		}
	}

	// descramble failures outside the expected HD-start reject zone and track pregaps
	gaps := pregaps(run)
	inGap := func(lo, hi int) bool {
		for _, g := range gaps {
			if lo >= g[0] && hi <= g[1] {
				return true
			}
		}
		return false
	}
	var bad, gapped []string
	for _, m := range reDescramble.FindAllStringSubmatch(run, -1) {
		a, b := m[1], m[2]
		if m[3] != "" {
			a, b = m[3], m[3]
		}
		lo, _ := strconv.Atoi(a)
		hi, _ := strconv.Atoi(b)
		if cp.GD && lo >= cp.AllowFrom && hi <= cp.AllowTo {
			continue
		}
		r := a + ".." + b
		if lo == hi {
			r = a
		}
		if inGap(lo, hi) {
			gapped = append(gapped, r)
			continue
		}
		bad = append(bad, r)
	}
	if len(gapped) > 0 {
		cs = append(cs, Check{Name: "descramble failures inside track pregaps (expected): " + strings.Join(firstN(gapped, 6), ", "), OK: true, Info: true})
	}
	d := ""
	if len(bad) > 0 {
		d = fmt.Sprintf("%d range(s): %s", len(bad), strings.Join(firstN(bad, 6), ", "))
		if len(bad) > 6 {
			d += ", ..."
		}
	}
	cs = append(cs, Check{Name: "no unexpected descramble failures", OK: len(bad) == 0, Detail: d})
	return cs
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
