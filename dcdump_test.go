package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func readTD(t *testing.T, p string) string {
	b, err := os.ReadFile("testdata/" + p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func failed(cs []Check) []string {
	var f []string
	for _, c := range cs {
		if !c.OK && !c.Info {
			f = append(f, c.Name)
		}
	}
	return f
}

var gdParams = CheckParams{GD: true, AllowFrom: 44925, AllowTo: 44999}

func TestCheckLogGood(t *testing.T) {
	for _, run := range []string{"harusame_redumper_gd07_sata_c2", "harusame_redumper_gd05_exact"} {
		p := gdParams
		p.DataTracks = []string{"Harusame_Youbi_T20106M (Track 1).bin", "Harusame_Youbi_T20106M (Track 3).bin"}
		if f := failed(CheckLog(readTD(t, run+"/Harusame_Youbi_T20106M.log"), p)); len(f) > 0 {
			t.Errorf("%s: unexpected failures %v", run, f)
		}
	}
}

func TestCheckLogDamaged(t *testing.T) {
	p := gdParams
	p.DataTracks = []string{"Sengoku_Turb_T38801M (Track 1).bin"}
	f := strings.Join(failed(CheckLog(readTD(t, "sengoku_redumper_gd07_sata_noc2/Sengoku_Turb_T38801M.log"), p)), "|")
	for _, want := range []string{"GD-ROM dump split", "single write offset", "sector errors: Sengoku_Turb_T38801M (Track 1).bin", "descramble"} {
		if !strings.Contains(f, want) {
			t.Errorf("noc2: expected failure %q, got %s", want, f)
		}
	}
	f = strings.Join(failed(CheckLog(readTD(t, "sengoku_redumper_gd07_sata_c2/Sengoku_Turb_T38801M.log"), gdParams)), "|")
	if !strings.Contains(f, "GD-ROM detected") {
		t.Errorf("c2 (LD-only by accident): expected GD detection failure, got %s", f)
	}
}

func TestCueAndGDI(t *testing.T) {
	tr := ParseCue(readTD(t, "harusame_redumper_gd07_sata_c2/Harusame_Youbi_T20106M.cue"))
	if len(tr) != 3 || !tr[2].HD || tr[1].Audio != true {
		t.Fatalf("cue parse: %+v", tr)
	}
	tr[0].Sectors, tr[1].Sectors, tr[2].Sectors = 450, 9470, 504150
	got := GDIText(tr)
	want := "3\n" +
		"1 0 4 2352 \"Harusame_Youbi_T20106M (Track 1).bin\" 0\n" +
		"2 450 0 2352 \"Harusame_Youbi_T20106M (Track 2).bin\" 0\n" +
		"3 45000 4 2352 \"Harusame_Youbi_T20106M (Track 3).bin\" 0\n"
	if got != want {
		t.Errorf("gdi:\n%s\nwant:\n%s", got, want)
	}
}

func TestDAT(t *testing.T) {
	db := ParseDAT(readTD(t, "dc-sample.dat"))
	cases := map[string]string{
		"T20106M":     "Harusame Youbi (Japan)",
		"T-38801M":    "Sengoku Turb (Japan)",
		"SCLX-2004":   "09 Chairs (Japan)",
		"MK-5101350":  "Dynamite Cop (Europe)",
		"950-0015-50": "Dynamite Cop (Europe)",
	}
	for s, want := range cases {
		e := db.Lookup(s)
		if len(e) == 0 || e[0].Name != want {
			t.Errorf("lookup %s: got %v want %s", s, e, want)
		}
	}
	c := narrow(db.Lookup("T-15117N"), &IPBin{Version: "1.000", Disc: 2, Of: 2})
	if len(c) != 2 || !strings.Contains(c[0].Name, "(Disc 2)") {
		t.Errorf("narrow by disc: %v", c[0].Name)
	}
	h := db.Lookup("T20106M")[0]
	if h.Version != "1.004" || len(h.Tracks) != 4 {
		t.Errorf("harusame entry: %+v", h)
	}
}

func TestIPBin(t *testing.T) {
	s := make([]byte, 2048)
	copy(s, "SEGA SEGAKATANA SEGA ENTERPRISES9903 GD-ROM1/1  J       0799A10 T20106M   V1.00420010220        1ST_READ.BIN    SEGA LC-T-201   HARUSAME YOUBI  ")
	ip, err := ParseIPBin(s)
	if err != nil || ip.Product != "T20106M" || ip.Version != "1.004" || ip.Areas != "J" || ip.Disc != 1 || ip.Of != 1 || ip.Title != "HARUSAME YOUBI" {
		t.Errorf("ipbin: %+v %v", ip, err)
	}
}

func TestProfilesAndArgs(t *testing.T) {
	ps, err := LoadProfiles()
	if err != nil {
		t.Fatal(err)
	}
	match := func(v, m, r string) *Profile {
		d := &Drive{Vendor: v, Model: m, Rev: r}
		for _, p := range ps {
			if p.Matches(d) {
				return p
			}
		}
		return nil
	}
	if p := match("ATAPI", "DVD D  DH16D2S", "GD07"); p == nil || !p.GD || p.HDFirstLBA != 45000 || p.Dreamdump == nil || p.Dreamdump.ReadAtOnce != 16 {
		t.Errorf("GD07 should be GD-capable from 45000: %+v", p)
	}
	if p := match("ATAPI", "DVD D  DH16D2S", "GD08"); p == nil || !p.GD || p.HDFirstLBA != 44853 || p.Dreamdump == nil || p.Dreamdump.SectorOrder != "DATA_C2_SUB" {
		t.Errorf("GD08 should be GD-capable from 44853: %+v", p)
	}
	for _, r := range []string{"GD09", "GD0A", "GD0F", "GD12", "GD1C"} {
		if p := match("ATAPI", "DVD D  DH16D2S", r); p == nil || p.ID != "dh16d2s-gd08" {
			t.Errorf("%s should use the GD08+ entry: %+v", r, p)
		}
	}
	if p := match("ATAPI", "DVD D  DH16D2S", "GD12"); p == nil || p.ID != "dh16d2s-gd08" {
		t.Errorf("later GD revisions should use the newest entry: %+v", p)
	}
	for _, r := range []string{"EH33", "JA12"} {
		if p := match("ATAPI", "DVD D  DH16D2S", r); p == nil || p.GD {
			t.Errorf("%s should be LD only: %v", r, p)
		}
	}
	if p := match("ASUS", "BW-16D1HT", "3.02"); p != nil {
		t.Errorf("BW-16D1HT should not be in the list: %v", p.ID)
	}

	gd := match("ATAPI", "DVD D  DH16D2S", "GD07")
	defer func(f func(string, *Drive) bool) { knownToRedumper = f }(knownToRedumper)
	knownToRedumper = func(string, *Drive) bool { return false }
	sata := &Drive{Transport: "SATA", Profile: gd}
	rp := BuildRun(sata, ParseOptions([]string{"--drive", "2", "--", "--overwrite"}), "/dev/sg2", "/w", "N")
	j := strings.Join(rp.Args, " ")
	if rp.Binary != "redumper" || !strings.Contains(j, "--drive-sector-order=DATA_C2_SUB") || strings.Contains(j, "--skip") ||
		strings.Contains(j, "--rings") || !strings.HasSuffix(j, "--overwrite") || strings.Contains(j, "--drive 2") || rp.ExactSize {
		t.Errorf("sata args: %s %s exact=%v", rp.Binary, j, rp.ExactSize)
	}
	usb := &Drive{Transport: "USB", Profile: gd}
	rp = BuildRun(usb, ParseOptions(nil), "/dev/sg5", "/w", "N")
	j = strings.Join(rp.Args, " ")
	if rp.Binary != "redumper" || !strings.Contains(j, "--drive-sector-order=DATA_SUB") || !rp.ExactSize {
		t.Errorf("usb args: %s %s exact=%v", rp.Binary, j, rp.ExactSize)
	}
	rp = BuildRun(usb, ParseOptions([]string{"--ld-only", "--retries=5"}), "/dev/sg5", "/w", "N")
	j = strings.Join(rp.Args, " ")
	if rp.Binary != "redumper" || strings.Contains(j, "--rings") || !strings.Contains(j, "--retries=5") {
		t.Errorf("ld-only args: %s %s", rp.Binary, j)
	}
	dp := BuildDreamRun(&Drive{SG: "/dev/sg8", Profile: gd}, ParseOptions([]string{"--retries=7"}), "/w", "N")
	j = strings.Join(dp.Args, " ")
	if dp.Dir != "/w/dreamdump" || j != "disc --drive=/dev/sg8 --image-path=/w/dreamdump --image-name=N --sector-order=DATA_C2_SUB --read-offset=6 --read-at-once=16 --retries=7" {
		t.Errorf("dreamdump args: %s", j)
	}

	h := match("TSSTcorp", "CDRWDVD TS-H492C", "GD1S")
	if h == nil || h.ID != "ts-h492c-gd1s" || !h.GD || h.HDFirstLBA != 44854 || h.Dreamdump == nil || h.Dreamdump.Speed != 8 {
		t.Fatalf("TS-H492C GD1S should use the ts-h492c-gd1s entry: %+v", h)
	}
	for _, r := range []string{"GD1T", "GD1U", "GD1V"} {
		if p := match("TSSTcorp", "CDRWDVD TS-H492C", r); p == nil || p.ID != "ts-h492c-gd1s" {
			t.Errorf("%s should use the ts-h492c-gd1s entry: %+v", r, p)
		}
	}
	x := match("TSSTcorp", "CDRWDVD TS-H492C", "GD1X")
	if x == nil || x.ID != "ts-h492c-gd1x" || x.Speed != 4 || x.HDFirstLBA != 44854 || x.Dreamdump == nil || x.Dreamdump.Speed != 8 {
		t.Fatalf("TS-H492C GD1X should use the ts-h492c-gd1x entry: %+v", x)
	}
	for _, r := range []string{"GD1Y", "GD1Z", "GD20"} {
		if p := match("TSSTcorp", "CDRWDVD TS-H492C", r); p == nil || p.ID != "ts-h492c-gd1x" {
			t.Errorf("%s should use the ts-h492c-gd1x entry: %+v", r, p)
		}
	}
	if rp := BuildRun(&Drive{Transport: "SATA", Profile: x}, ParseOptions(nil), "/dev/sg6", "/w", "N"); !strings.Contains(strings.Join(rp.Args, " "), "--speed=4") {
		t.Errorf("TS-H492C GD1X must read the LD at 4x (GD1X cold-start bug above that): %v", rp.Args)
	}
	for _, r := range []string{"DE02", "DE04", "GD09", "GD1N", "GD1R", "GD1W", "DG12"} {
		if p := match("TSSTcorp", "CDRWDVD TS-H492C", r); p != nil {
			t.Errorf("TS-H492C %s should not be listed: %v", r, p.ID)
		}
	}
	dp = BuildDreamRun(&Drive{SG: "/dev/sg6", Profile: h}, ParseOptions(nil), "/w", "N")
	j = strings.Join(dp.Args, " ")
	if j != "disc --drive=/dev/sg6 --image-path=/w/dreamdump --image-name=N --sector-order=DATA_SUB_C2 --read-offset=6 --read-at-once=16 --speed=8 --retries=20" {
		t.Errorf("TS-H492C dreamdump args: %s", j)
	}
	rp = BuildRun(&Drive{Transport: "SATA", Profile: h}, ParseOptions(nil), "/dev/sg6", "/w", "N")
	j = strings.Join(rp.Args, " ")
	if !strings.Contains(j, "--speed=4") || !strings.Contains(j, "--drive-read-offset=6") || !strings.Contains(j, "--drive-sector-order=DATA_SUB_C2") {
		t.Errorf("TS-H492C redumper args: %s", j)
	}
	for _, r := range []string{"GD21", "GD23"} {
		if p := match("TSSTcorp", "CDRWDVD TS-H492C", r); p == nil || p.ID != "ts-h492c-gd1x" || p.StateCheck != "" {
			t.Errorf("%s has no state byte and should use the ts-h492c-gd1x entry: %+v", r, p)
		}
	}
	for _, r := range []string{"GD24", "GD2A", "GD30"} {
		p := match("TSSTcorp", "CDRWDVD TS-H492C", r)
		if p == nil || p.ID != "ts-h492c-gd24" || p.StateCheck != "ts-h492c" || p.Speed != 4 || p.Dreamdump == nil || p.Dreamdump.Speed != 8 {
			t.Errorf("%s should use the ts-h492c-gd24 entry with the state check: %+v", r, p)
		}
	}
}

func TestH492CState(t *testing.T) {
	for _, c := range []struct {
		status byte
		good   bool
	}{
		{0x00, false}, // not written (no spin-up yet, or firmware without the byte)
		{0x80, false}, // stock pressed-CD path: the bad state
		{0x81, true},  // low-level path by the level probe
		{0x83, true},  // low-level path after promotion
		{0x87, false}, // promoted attempt failed
		{0x89, false}, // spin-up failed
		{0x01, false}, // not valid
	} {
		if g := DecodeH492CState(c.status, 0x4C).Good(); g != c.good {
			t.Errorf("status 0x%02X: good=%v, want %v", c.status, g, c.good)
		}
	}
	if s := DecodeH492CState(0x83, 0x4C).String(); s != "status 0x83, spin-up level 0x4C, low-level path (promoted)" {
		t.Errorf("String: %q", s)
	}
	if _, bad := CheckDriveState(nil, &Profile{}); bad != "" {
		t.Errorf("no state_check must not check anything: %s", bad)
	}
	p := &Profile{ID: "x", Vendor: ".", Model: ".", Rev: ".", StateCheck: "other"}
	if err := p.compile(); err == nil {
		t.Errorf("an unknown state_check must be rejected")
	}
}

func TestDatVersion(t *testing.T) {
	if v := datVersion([]byte(readTD(t, "dc-sample.dat"))); v != "2026-10-04 15-25-59" {
		t.Errorf("dat version %q", v)
	}
}

func TestCheckLogPregapDescramble(t *testing.T) {
	p := CheckParams{GD: true, AllowFrom: 44853, AllowTo: 44852,
		DataTracks: []string{"Aero Dancing i (Japan) (Track 1).bin", "Aero Dancing i (Japan) (Track 3).bin", "Aero Dancing i (Japan) (Track 5).bin"}}
	cs := CheckLog(readTD(t, "aero_gd08_sata_c2/Aero Dancing i (Japan).log"), p)
	if f := failed(cs); len(f) > 0 {
		t.Errorf("aero: unexpected failures %v", f)
	}
	info := ""
	for _, c := range cs {
		if c.Info {
			info += c.Name + "|"
		}
	}
	if !strings.Contains(info, "545263..545337") {
		t.Errorf("aero: pregap descramble range not reported: %s", info)
	}
	// a descramble failure outside any pregap must still fail
	log := strings.Replace(readTD(t, "aero_gd08_sata_c2/Aero Dancing i (Japan).log"), "LBA: [545263 .. 545337]", "LBA: [300000 .. 300010]", 1)
	if f := strings.Join(failed(CheckLog(log, p)), "|"); !strings.Contains(f, "descramble") {
		t.Errorf("aero: descramble outside pregap should fail, got %s", f)
	}
}

func TestEarlyAbort(t *testing.T) {
	cases := []struct {
		p    Progress
		stop bool
	}{
		{Progress{LBA: 201, Q: 201}, false},   // too early to judge
		{Progress{LBA: 1005, Q: 1005}, true},  // run4: every sector bad
		{Progress{LBA: 8826, Q: 1}, false},    // normal
		{Progress{LBA: 5000, Q: 2000}, false}, // scratched, but not a lag
		{Progress{LBA: 5000, Q: 2600}, true},
	}
	for _, c := range cases {
		if got := EarlyAbortReason(c.p) != ""; got != c.stop {
			t.Errorf("%+v: stop=%v", c.p, got)
		}
	}
	m := reProgress.FindStringSubmatch("\\ [  1%] LBA:   8826/765000, errors: { SCSIs: 0, C2s: 0, Q: 1 }")
	if m == nil || m[1] != "8826" || m[4] != "1" {
		t.Errorf("progress parse: %v", m)
	}
}

func TestDropStaleTOCFiles(t *testing.T) {
	dir := t.TempDir()
	full := []byte("\x00\x39\x01\x01fulltoc-bytes")
	write := func(ext string, b []byte) {
		if err := os.WriteFile(filepath.Join(dir, "N"+ext), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".fulltoc", full)
	write(".toc", []byte("\x00\x1a\x01\x02toc"))
	write(".atip", full)
	write(".pma", full)
	write(".cdtext", []byte("\x00\x10\x00\x00real cd-text"))
	got := DropStaleTOCFiles(dir, "N")
	if strings.Join(got, ",") != "N.atip,N.pma" {
		t.Fatalf("dropped %v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "N.cdtext")); err != nil {
		t.Fatal("real cd-text removed")
	}
}

func TestCDRLeniencyIsAnErrorOnGD(t *testing.T) {
	log := "*** DUMP (time check: 0s)\ndreamcast: GD-ROM detected\nwarning: CD-R trailing C2 errors detected (sectors: 3)\n"
	for _, c := range CheckLog(log, CheckParams{GD: true}) {
		if c.Name == "no CD-R C2 leniency" {
			if c.OK {
				t.Fatal("leniency accepted")
			}
			return
		}
	}
	t.Fatal("check missing")
}

// MergeCue must reproduce redump's GD-ROM cue from a redumper LD cue and a dreamdump HD cue.
func TestMergeCue(t *testing.T) {
	ld := "FILE \"A (Track 1).bin\" BINARY\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n" +
		"FILE \"A (Track 2).bin\" BINARY\n  TRACK 02 AUDIO\n    INDEX 00 00:00:00\n    INDEX 01 00:02:00\n"
	hd := "REM HIGH-DENSITY AREA\nFILE \"A (Track 3).bin\" BINARY\n  TRACK 03 MODE1/2352\n    INDEX 01 00:00:00\n" +
		"FILE \"A (Track 4).bin\" BINARY\n  TRACK 04 AUDIO\n    INDEX 00 00:00:00\n    INDEX 01 00:02:00\n"
	want := "REM SINGLE-DENSITY AREA\n" + ld + hd
	if got := MergeCue(ld, hd); got != want {
		t.Fatalf("MergeCue:\n%s\nwant:\n%s", got, want)
	}
	if got := MergeCue("CATALOG 0000000000000\n"+ld, hd); got != want {
		t.Fatalf("MergeCue must drop lines before the first FILE:\n%s", got)
	}
	tr := ParseCue(want)
	if len(tr) != 4 || tr[1].HD || !tr[2].HD {
		t.Fatalf("ParseCue of merged cue: %+v", tr)
	}
}

func TestParseOptionsHelp(t *testing.T) {
	for _, flag := range []string{"-h", "--help"} {
		if o := ParseOptions([]string{flag}); !o.Help {
			t.Errorf("%s did not set Help", flag)
		} else if len(o.Passthru) != 0 {
			t.Errorf("%s leaked into Passthru: %v", flag, o.Passthru)
		}
	}
	if o := ParseOptions([]string{"--ld-only"}); o.Help {
		t.Error("--ld-only set Help")
	}
}

func TestParseOptionsPassthru(t *testing.T) {
	o := ParseOptions([]string{"--ld-only", "--", "--speed=4", "--verbose"})
	if !o.LDOnly {
		t.Error("--ld-only before -- was not parsed")
	}
	if got, want := strings.Join(o.Passthru, " "), "--speed=4 --verbose"; got != want {
		t.Errorf("Passthru = %q, want %q", got, want)
	}
	// dcdump's own option names after -- belong to redumper, not dcdump.
	o = ParseOptions([]string{"--", "--ld-only"})
	if o.LDOnly {
		t.Error("--ld-only after -- was consumed by dcdump")
	}
	if len(o.Passthru) != 1 || o.Passthru[0] != "--ld-only" {
		t.Errorf("Passthru = %v, want [--ld-only]", o.Passthru)
	}
}

// A typo must abort before any drive is touched. fatal() exits, so run a child process.
func TestParseOptionsRejectsUnknown(t *testing.T) {
	if os.Getenv("DCDUMP_FATAL_ARG") != "" {
		ParseOptions([]string{os.Getenv("DCDUMP_FATAL_ARG")})
		return
	}
	for _, bad := range []string{"--ld-onlyy", "-x", "stray.bin"} {
		cmd := exec.Command(os.Args[0], "-test.run=TestParseOptionsRejectsUnknown")
		cmd.Env = append(os.Environ(), "DCDUMP_FATAL_ARG="+bad)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("%q was accepted; want a fatal error", bad)
		}
		if !strings.Contains(string(out), "[ERROR]") {
			t.Errorf("%q: output %q lacks [ERROR]", bad, out)
		}
	}
}

// A drive that redumper already knows needs no drive settings from dcdump, except the
// sector order off SATA, where the bridge cannot carry C2.
func TestKnownDriveNeedsNoOverrides(t *testing.T) {
	ps, err := LoadProfiles()
	if err != nil {
		t.Fatal(err)
	}
	probe := &Drive{Vendor: "ATAPI", Model: "DVD D  DH16D2S", Rev: "GD0D"}
	var gd *Profile
	for _, p := range ps {
		if p.Matches(probe) {
			gd = p
			break
		}
	}
	if gd == nil {
		t.Fatal("GD0D should match a profile")
	}
	defer func(f func(string, *Drive) bool) { knownToRedumper = f }(knownToRedumper)
	knownToRedumper = func(string, *Drive) bool { return true }

	sata := &Drive{Vendor: "ATAPI", Model: "DVD D  DH16D2S", Rev: "GD0D", Transport: "SATA", Profile: gd}
	rp := BuildRun(sata, ParseOptions(nil), "/dev/sg2", "/w", "N")
	j := strings.Join(rp.Args, " ")
	if !rp.Known || strings.Contains(j, "--drive-") {
		t.Errorf("known sata drive should get no drive options: %s", j)
	}

	usb := &Drive{Vendor: "ATAPI", Model: "DVD D  DH16D2S", Rev: "GD0D", Transport: "USB", Profile: gd}
	rp = BuildRun(usb, ParseOptions(nil), "/dev/sg5", "/w", "N")
	j = strings.Join(rp.Args, " ")
	if !strings.Contains(j, "--drive-sector-order=DATA_SUB") || strings.Contains(j, "--drive-read-offset") ||
		strings.Contains(j, "--drive-type") || strings.Contains(j, "--drive-read-method") {
		t.Errorf("known usb drive should only get the sector order: %s", j)
	}
}

// redumperKnowsDrive reads the drive list of the redumper it is given.
func TestRedumperKnowsDrive(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"echo 'PLEXTOR - CD-R PREMIUM2 (revision level: 1.03, vendor specific: <empty>)'\n" +
		"echo 'ATAPI - DVD D DH16D2S (revision level: GD0D, vendor specific: 2007/06/04 17:00)'\n"
	if err := os.WriteFile(filepath.Join(dir, "redumper-fake"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	// the drive strings come from sysfs with their original padding
	known := &Drive{Vendor: "ATAPI", Model: "DVD D  DH16D2S", Rev: "GD0D"}
	if !redumperKnowsDrive("redumper-fake", known) {
		t.Error("GD0D should be recognized")
	}
	for _, d := range []*Drive{
		{Vendor: "ATAPI", Model: "DVD D  DH16D2S", Rev: "GD0C"},
		{Vendor: "ATAPI", Model: "DVD D  DH16D5S", Rev: "GD0D"},
		{Vendor: "PLEXTOR", Model: "CD-R PREMIUM2", Rev: "1.02"},
	} {
		if redumperKnowsDrive("redumper-fake", d) {
			t.Errorf("%s %s %s should not be recognized", d.Vendor, d.Model, d.Rev)
		}
	}
	if redumperKnowsDrive("redumper-missing", known) {
		t.Error("a missing binary knows nothing")
	}
}

// A release older than the one that asks for the exact transfer length must be refused on
// the DATA_SUB path; a local build has no release number and is only warned about.
func TestBuildNumber(t *testing.T) {
	for _, c := range []struct {
		build string
		n     int
		ok    bool
	}{
		{"b767", 767, true},
		{"b765", 765, true},
		{"b1024", 1024, true},
		{"b765-pr459", 0, false},
		{"9002", 0, false},
		{"", 0, false},
	} {
		n, ok := buildNumber(c.build)
		if n != c.n || ok != c.ok {
			t.Errorf("buildNumber(%q) = %d, %v; want %d, %v", c.build, n, ok, c.n, c.ok)
		}
		if ok && (n < minExactSizeBuild) != (c.n < minExactSizeBuild) {
			t.Errorf("%q: unexpected comparison against b%d", c.build, minExactSizeBuild)
		}
	}
}
