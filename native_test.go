package main

import (
	"bytes"
	"strings"
	"testing"
)

func testUser() []byte {
	u := make([]byte, sectorUser)
	for i := range u {
		u[i] = byte(i*7 + 3)
	}
	return u
}

// Reference hashes come from an independent C implementation of the same sector
// regeneration, validated against whole tracks read from a real disc (ECC included).
func TestMode1Sector(t *testing.T) {
	for fad, want := range map[int]string{
		45150: "989612139eb9deb47e304c4bd96186dbec421234",
		449:   "6f414b3bd5a2eb08d41eb086ea72c3503dec154b",
	} {
		if got := sha1Bytes(Mode1Sector(testUser(), fad)); got != want {
			t.Errorf("Mode1Sector(fad %d) = %s, want %s", fad, got, want)
		}
	}
}

func TestCheckMode1(t *testing.T) {
	good := Mode1Sector(testUser(), 449)
	if st, _ := CheckMode1(good, 449); st != SectorClean {
		t.Errorf("clean sector: state %d", st)
	}
	// the defect seen on a real disc: one sync byte wrong in the last sector of a track
	bent := append([]byte(nil), good...)
	bent[10] = 0
	st, fixed := CheckMode1(bent, 449)
	if st != SectorRepairable || string(fixed) != string(good) {
		t.Errorf("bad sync byte: state %d, repaired=%v", st, string(fixed) == string(good))
	}
	// parity bytes wrong, user data intact
	bent = append([]byte(nil), good...)
	bent[2254] ^= 0x5a
	if st, fixed := CheckMode1(bent, 449); st != SectorRepairable || string(fixed) != string(good) {
		t.Errorf("bad parity byte: state %d", st)
	}
	// user data wrong
	bent = append([]byte(nil), good...)
	bent[100] ^= 1
	if st, _ := CheckMode1(bent, 449); st != SectorBad {
		t.Errorf("bad user data: state %d", st)
	}
	// the sector of another address must not pass as this one: the EDC covers the header
	if st, _ := CheckMode1(good, 450); st != SectorBad {
		t.Errorf("wrong address: state %d", st)
	}
}

func TestScrambledHeaderFAD(t *testing.T) {
	s := Mode1Sector(testUser(), 449)
	for i := 12; i < sectorRaw; i++ {
		s[i] ^= scrambleTable[i-12]
	}
	if got := ScrambledHeaderFAD(s); got != 449 {
		t.Errorf("ScrambledHeaderFAD = %d, want 449", got)
	}
	if got := ScrambledHeaderFAD(make([]byte, 64)); got != -1 {
		t.Errorf("no sync: got %d", got)
	}
}

const tocBittersweet = `ld track  1 ctrl 4 fad    150
ld track  2 ctrl 0 fad    600
ld leadout  ctrl 0 fad   1466
hd track  3 ctrl 4 fad  45150
hd leadout  ctrl 4 fad 549300
`

func TestParseNativeTOC(t *testing.T) {
	toc := parseNativeTOC(tocBittersweet)
	if len(toc.Tracks) != 3 || toc.LDLeadout != 1466 || toc.HDLeadout != 549300 {
		t.Fatalf("parsed %+v", toc)
	}
	if toc.Tracks[0].Audio || !toc.Tracks[1].Audio || toc.Tracks[1].HD || !toc.Tracks[2].HD || toc.Tracks[2].FAD != 45150 {
		t.Errorf("tracks %+v", toc.Tracks)
	}
	if half := parseNativeTOC("ld -\n" + tocBittersweet[strings.Index(tocBittersweet, "hd"):]); half.LDLeadout != 0 {
		t.Errorf("missing LD TOC not detected")
	}
}

func TestNativeLayout(t *testing.T) {
	toc := parseNativeTOC(tocBittersweet)
	entry := &DatEntry{Name: "X (Japan)", Tracks: []DatTrack{
		{Name: "X (Japan).cue", Size: 534},
		{Name: "X (Japan) (Track 1).bin", Size: 300 * sectorRaw},
		{Name: "X (Japan) (Track 2).bin", Size: 1016 * sectorRaw},
		{Name: "X (Japan) (Track 3).bin", Size: 504150 * sectorRaw},
	}}
	want := [][2]int{{150, 300}, {450, 1016}, {45150, 504150}}
	for _, e := range []*DatEntry{entry, nil} { // DAT sizes and pregap rules must agree on this layout
		tr, _, err := nativeLayout(toc, e)
		if err != nil {
			t.Fatal(err)
		}
		for i, w := range want {
			if tr[i].Start != w[0] || tr[i].Sectors != w[1] {
				t.Errorf("entry=%v track %d: start %d sectors %d, want %v", e != nil, i+1, tr[i].Start, tr[i].Sectors, w)
			}
		}
	}
	entry.Tracks[3].Size -= sectorRaw
	if _, _, err := nativeLayout(toc, entry); err == nil {
		t.Errorf("DAT sizes that do not reach the lead-out were accepted")
	}
}

func TestNativeOptions(t *testing.T) {
	if o := ParseOptions([]string{"--native"}); o.Native != "default" || o.Keep {
		t.Errorf("--native: %+v", o)
	}
	if o := ParseOptions([]string{"--native=10.0.0.5", "--keep"}); o.Native != "10.0.0.5" || !o.Keep {
		t.Errorf("--native=host --keep: %+v", o)
	}
	if o := ParseOptions(nil); o.Native != "" {
		t.Errorf("default: %+v", o)
	}
}

func TestDescrambledSector(t *testing.T) {
	user := make([]byte, sectorUser)
	for i := range user {
		user[i] = byte(i * 7)
	}
	const fad = 545489
	want := Mode1Sector(user, fad)
	scr := append([]byte(nil), want...)
	for k := 12; k < sectorRaw; k++ {
		scr[k] ^= scrambleTable[k-12]
	}
	// two audio frames, then the sector 28 bytes late with its tail cut off
	stream := append(make([]byte, 2*sectorRaw+28), scr[:sectorRaw-28]...)
	stream = append(stream, make([]byte, 28)...)
	if got := descrambledSector(stream, fad); !bytes.Equal(got, want) {
		t.Fatal("truncated scrambled sector not recovered")
	}
	if descrambledSector(stream, fad+1) != nil {
		t.Fatal("found a sector that is not in the stream")
	}
	stream[2*sectorRaw+28+100] ^= 1
	if descrambledSector(stream, fad) != nil {
		t.Fatal("accepted a sector whose user data fails EDC")
	}
}

func TestSetLeads(t *testing.T) {
	tr := []nativeTrack{
		{nativeTOCTrack: nativeTOCTrack{Num: 3, HD: true, FAD: 45150}, Start: 45150, Sectors: 488114},
		{nativeTOCTrack: nativeTOCTrack{Num: 4, HD: true, Audio: true, FAD: 533414}, Start: 533264, Sectors: 12149},
		{nativeTOCTrack: nativeTOCTrack{Num: 5, HD: true, FAD: 545638}, Start: 545413, Sectors: 3887},
	}
	setLeads(tr)
	if tr[0].Lead != 0 || tr[1].Lead != 0 || tr[2].Lead != 75 {
		t.Fatalf("leads %d %d %d", tr[0].Lead, tr[1].Lead, tr[2].Lead)
	}
}
